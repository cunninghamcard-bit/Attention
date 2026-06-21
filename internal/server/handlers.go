package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

const enqueueTimeout = 100 * time.Millisecond

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateSessionRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	if req.CWD == "" {
		req.CWD = s.opts.DefaultCWD // cwd 可选（along-api.md §2）：缺省 = 引擎进程 cwd
	}

	created, err := s.opts.Repo.Create(r.Context(), session.JsonlSessionCreateOptions{
		CWD:       req.CWD,
		ParentRef: req.ParentRef,
		SpawnedBy: sessionSpawnedBy(req.SpawnedBy),
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}

	metadata := s.registerSession(created)
	info := s.infoFromMetadata(metadata)
	if err := s.appendSessionEvent(r.Context(), metadata.ID, protocol.KindSessionCreated, info); err != nil {
		writeMappedError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, protocol.CreateSessionResponse{SessionID: metadata.ID})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	infos, err := s.listSessions(r.Context())
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, infos)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, metadata, ok, err := s.findSession(r.Context(), id)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	writeJSON(w, http.StatusOK, s.infoFromMetadata(metadata))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, metadata, ok, err := s.findSession(r.Context(), id)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	if err := s.opts.Repo.Delete(r.Context(), metadata); err != nil {
		writeMappedError(w, err)
		return
	}
	s.unregisterSession(id)
	writeJSON(w, http.StatusOK, protocol.OKResponse{OK: true})
}

func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	var req protocol.ForkSessionRequest
	if !decodeRequest(w, r, &req) {
		return
	}

	id := r.PathValue("id")
	source, sourceMetadata, ok, err := s.findSession(r.Context(), id)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}

	opts := session.JsonlSessionForkOptions{
		CWD:               sourceMetadata.CWD,
		ParentSessionPath: sourceMetadata.Path,
		ParentRef:         sourceMetadata.ID,
	}
	forked, err := s.forkFrom(r.Context(), source, sourceMetadata, opts)
	if err != nil {
		writeMappedError(w, err)
		return
	}

	forkMetadata := s.registerSession(forked)
	info := s.infoFromMetadata(forkMetadata)
	payload := struct {
		protocol.SessionInfo
		FromSeq uint64 `json:"fromSeq,omitempty"`
	}{
		SessionInfo: info,
		FromSeq:     req.FromSeq,
	}
	if err := s.appendSessionEvent(r.Context(), sourceMetadata.ID, protocol.KindSessionForked, payload); err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.CreateSessionResponse{SessionID: forkMetadata.ID})
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var req protocol.PromptRequest
	if !decodeRequest(w, r, &req) {
		return
	}

	sessionID := r.PathValue("id")
	if !s.sessionExists(r.Context(), w, sessionID) {
		return
	}

	payload, err := json.Marshal(req)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	runID := protocol.NewRunID()
	if err := s.enqueueJob(r.Context(), backend.Job{
		Kind:      backend.JobPrompt,
		SessionID: sessionID,
		RunID:     runID,
		Payload:   payload,
	}); err != nil {
		writeQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.PromptResponse{RunID: runID})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	var req protocol.CancelRequest
	if !decodeRequest(w, r, &req) {
		return
	}

	sessionID := r.PathValue("id")
	if !s.sessionExists(r.Context(), w, sessionID) {
		return
	}

	payload, err := json.Marshal(req)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if err := s.enqueueJob(r.Context(), backend.Job{
		Kind:      backend.JobCancel,
		SessionID: sessionID,
		Payload:   payload,
	}); err != nil {
		writeQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.OKResponse{OK: true})
}

func (s *Server) handleUIResolve(w http.ResponseWriter, r *http.Request) {
	var req protocol.UIResolveRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	if req.RequestID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "requestId is required")
		return
	}

	sessionID := r.PathValue("id")
	if !s.sessionExists(r.Context(), w, sessionID) {
		return
	}

	payload, err := json.Marshal(req)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	// 经 JobQueue 进执行面（plan T9）：与 cancel 同路，first-resolve-wins
	// 由折叠投影裁决（request 减 resolved），引擎无挂起表。
	if err := s.enqueueJob(r.Context(), backend.Job{
		Kind:      backend.JobUIResolve,
		SessionID: sessionID,
		Payload:   payload,
	}); err != nil {
		writeQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.OKResponse{OK: true})
}

func (s *Server) sessionExists(ctx context.Context, w http.ResponseWriter, id string) bool {
	_, _, ok, err := s.findSession(ctx, id)
	if err != nil {
		writeMappedError(w, err)
		return false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return false
	}
	return true
}

func (s *Server) enqueueJob(ctx context.Context, job backend.Job) error {
	ctx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()
	return s.opts.Queue.Enqueue(ctx, job)
}

func (s *Server) appendSessionEvent(
	ctx context.Context,
	sessionID string,
	kind string,
	payload any,
) error {
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = data
	}

	event := &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     sessionID,
		Kind:          kind,
		Actor:         protocol.ActorSystem,
		Payload:       raw,
		SchemaVersion: protocol.SchemaVersion,
	}
	if err := s.opts.Store.Append(ctx, event); err != nil {
		return err
	}
	s.opts.Bus.Publish(sessionID)
	return nil
}

func (s *Server) registerSession(sess *session.Session) session.Metadata {
	metadata := sess.GetMetadata()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[metadata.ID] = sess
	s.metadata[metadata.ID] = metadata
	return metadata
}

func (s *Server) unregisterSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	delete(s.metadata, id)
}

func (s *Server) findSession(
	ctx context.Context,
	id string,
) (*session.Session, session.Metadata, bool, error) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	metadata := s.metadata[id]
	s.mu.Unlock()
	if ok {
		return sess, metadata, true, nil
	}

	all, err := s.opts.Repo.List(ctx)
	if err != nil {
		return nil, session.Metadata{}, false, err
	}
	for _, metadata := range all {
		if metadata.ID != id {
			continue
		}
		opened, err := s.opts.Repo.Open(ctx, metadata)
		if err != nil {
			return nil, session.Metadata{}, false, err
		}
		s.registerSession(opened)
		return opened, metadata, true, nil
	}
	return nil, session.Metadata{}, false, nil
}

func (s *Server) listSessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	metadataByID := map[string]session.Metadata{}
	fromRepo, err := s.opts.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, metadata := range fromRepo {
		metadataByID[metadata.ID] = metadata
	}

	s.mu.Lock()
	for id, metadata := range s.metadata {
		metadataByID[id] = metadata
	}
	s.mu.Unlock()

	infos := make([]protocol.SessionInfo, 0, len(metadataByID))
	for _, metadata := range metadataByID {
		infos = append(infos, s.infoFromMetadata(metadata))
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].ID < infos[j].ID
	})
	return infos, nil
}

func (s *Server) infoFromMetadata(metadata session.Metadata) protocol.SessionInfo {
	modified := ""
	if !metadata.Modified.IsZero() {
		modified = metadata.Modified.Format(time.RFC3339Nano)
	}
	name := ""
	s.mu.Lock()
	sess := s.sessions[metadata.ID]
	s.mu.Unlock()
	if sess != nil {
		if got, ok := sess.GetSessionName(); ok {
			name = got
		}
	}
	return protocol.SessionInfo{
		ID:        metadata.ID,
		Name:      name,
		ParentRef: metadata.ParentRef,
		CreatedAt: metadata.CreatedAt,
		Modified:  modified,
	}
}

// forkFrom 从手上已打开的会话对象 fork——findSession 总是交回打开的对象
// （注册表内未落盘的与刚从磁盘打开的走同一条路），所以这里没有
// Repo.Fork/内存回退的双路径：一条路，失败就失败。
func (s *Server) forkFrom(
	ctx context.Context,
	source *session.Session,
	sourceMetadata session.Metadata,
	opts session.JsonlSessionForkOptions,
) (*session.Session, error) {
	forked, err := s.opts.Repo.Create(ctx, session.JsonlSessionCreateOptions{
		ID:                opts.ID,
		CWD:               opts.CWD,
		ParentSessionPath: sourceMetadata.Path,
		ParentRef:         sourceMetadata.ID,
		SpawnedBy:         opts.SpawnedBy,
	})
	if err != nil {
		return nil, err
	}
	if opts.SkipConversationRestore {
		return forked, nil
	}

	storage := forked.GetStorage()
	for _, entry := range source.GetEntries() {
		if err := storage.AppendEntry(entry); err != nil {
			return nil, err
		}
	}
	return forked, nil
}

func sessionSpawnedBy(in *protocol.SpawnedBy) *session.SpawnedBy {
	if in == nil {
		return nil
	}
	return &session.SpawnedBy{
		SessionID:  in.SessionID,
		RunID:      in.RunID,
		ToolCallID: in.ToolCallID,
	}
}

func decodeRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, protocol.ErrorResponse{
		Error: protocol.ErrorBody{
			Code:    code,
			Message: message,
		},
	})
}

func writeMappedError(w http.ResponseWriter, err error) {
	var sessionErr *session.Error
	if errors.As(err, &sessionErr) {
		switch sessionErr.Code {
		case session.ErrorNotFound:
			writeError(w, http.StatusNotFound, string(sessionErr.Code), sessionErr.Message)
		case session.ErrorInvalidSession, session.ErrorInvalidEntry, session.ErrorInvalidForkTarget:
			writeError(w, http.StatusBadRequest, string(sessionErr.Code), sessionErr.Message)
		default:
			writeError(w, http.StatusInternalServerError, string(sessionErr.Code), sessionErr.Message)
		}
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

func writeQueueError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeError(w, http.StatusServiceUnavailable, "queue_unavailable", err.Error())
		return
	}
	writeMappedError(w, err)
}
