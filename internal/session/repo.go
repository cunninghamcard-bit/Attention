package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

type JsonlSessionRepo struct {
	sessionsRoot string

	// open 是进程内打开会话注册表：延迟持久化（首条目才落盘）下，刚 Create
	// 的会话磁盘上还不存在——repo 是会话的进程内权威，控制面/执行面/兼容头
	// 都从这里拿同一个实例，不许各建私表。
	mu   sync.Mutex
	open map[string]*Session
}

var _ JsonlSessionRepoAPI = (*JsonlSessionRepo)(nil)

func NewJsonlSessionRepo(sessionsRoot string) *JsonlSessionRepo {
	return &JsonlSessionRepo{sessionsRoot: sessionsRoot, open: map[string]*Session{}}
}

func (r *JsonlSessionRepo) register(s *Session) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := s.GetMetadata().ID
	if existing, ok := r.open[id]; ok {
		return existing // 同会话同实例：一会话一锁的前提
	}
	r.open[id] = s
	return s
}

// Get 按 ID 取会话：注册表命中即返回同一实例；否则从磁盘找到并打开（并登记）。
func (r *JsonlSessionRepo) Get(ctx context.Context, id string) (*Session, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	r.mu.Lock()
	if s, ok := r.open[id]; ok {
		r.mu.Unlock()
		return s, true, nil
	}
	r.mu.Unlock()

	all, err := r.List(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, metadata := range all {
		if metadata.ID != id {
			continue
		}
		s, err := r.Open(ctx, metadata)
		if err != nil {
			return nil, false, err
		}
		return r.register(s), true, nil
	}
	return nil, false, nil
}

func (r *JsonlSessionRepo) Create(ctx context.Context, opts JsonlSessionCreateOptions) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.CWD == "" {
		return nil, sessionError(ErrorInvalidSession, "cwd is required")
	}

	id := opts.ID
	if id == "" {
		id = newRandomID()
	}
	createdAt := newTimestamp()
	sessionDir, err := r.sessionDir(opts.CWD)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, wrapSessionError(ErrorStorage, "Failed to create session directory "+sessionDir, err)
	}
	path := filepath.Join(sessionDir, sessionFileName(createdAt, id))
	storage, err := CreateJSONL(path, CreateOptions{
		ID:                id,
		CWD:               opts.CWD,
		ParentSessionPath: opts.ParentSessionPath,
		ParentRef:         opts.ParentRef,
		SpawnedBy:         opts.SpawnedBy,
	})
	if err != nil {
		return nil, err
	}
	return r.register(NewSession(storage)), nil
}

// Open 的合同是"从磁盘按元信息打开"（未落盘 = not found，pi 同义）。
// 引擎内部按 ID 寻址（含未落盘会话）用 Get。
func (r *JsonlSessionRepo) Open(ctx context.Context, metadata Metadata) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if metadata.Path == "" {
		return nil, sessionError(ErrorNotFound, "Session path is empty")
	}
	if _, err := os.Stat(metadata.Path); err != nil {
		if os.IsNotExist(err) {
			return nil, sessionError(ErrorNotFound, "Session not found: "+metadata.Path)
		}
		return nil, wrapSessionError(ErrorStorage, "Failed to check session "+metadata.Path, err)
	}
	storage, err := OpenJSONL(metadata.Path)
	if err != nil {
		return nil, err
	}
	return NewSession(storage), nil
}

// List 是"给人看的列表"：只含磁盘上的会话——延迟持久化下空会话不可见
// （pi session-manager 同义）。引擎内部按 ID 寻址用 Get（注册表优先）。
func (r *JsonlSessionRepo) List(ctx context.Context, listOpts ...JsonlSessionListOptions) ([]Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(listOpts) > 1 {
		return nil, sessionError(ErrorInvalidSession, "expected at most one list options value")
	}
	opts := JsonlSessionListOptions{}
	if len(listOpts) == 1 {
		opts = listOpts[0]
	}

	dirs := []string{}
	if opts.CWD != "" {
		dir, err := r.sessionDir(opts.CWD)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	} else {
		root, err := r.root()
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			return []Metadata{}, nil
		}
		if err != nil {
			return nil, wrapSessionError(ErrorStorage, "Failed to list sessions root "+root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(root, entry.Name()))
			}
		}
	}

	sessions := []Metadata{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, wrapSessionError(ErrorStorage, "Failed to list sessions in "+dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			metadata, err := LoadJSONLMetadata(path)
			if err != nil {
				if isInvalidSessionError(err) {
					continue
				}
				return nil, err
			}
			sessions = append(sessions, metadata)
		}
	}
	// pi sorts by last-activity time, newest first (session-manager.ts:1410-1414).
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Modified.After(sessions[j].Modified)
	})
	return sessions, nil
}

func (r *JsonlSessionRepo) Delete(ctx context.Context, metadata Metadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.open, metadata.ID)
	r.mu.Unlock()
	if metadata.Path == "" {
		return nil
	}
	if err := os.Remove(metadata.Path); err != nil && !os.IsNotExist(err) {
		return wrapSessionError(ErrorStorage, "Failed to delete session "+metadata.Path, err)
	}
	return nil
}

func (r *JsonlSessionRepo) Fork(ctx context.Context, source Metadata, opts JsonlSessionForkOptions) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sourceSession, err := r.Open(ctx, source)
	if err != nil {
		return nil, err
	}
	entries, err := getEntriesToFork(sourceSession.GetStorage(), opts.EntryID, opts.Position)
	if err != nil {
		return nil, err
	}

	if opts.CWD == "" {
		return nil, sessionError(ErrorInvalidSession, "cwd is required")
	}
	parentSessionPath := opts.ParentSessionPath
	if parentSessionPath == "" {
		parentSessionPath = source.Path
	}
	parentRef := opts.ParentRef
	if parentRef == "" {
		parentRef = source.ID
	}
	forked, err := r.Create(ctx, JsonlSessionCreateOptions{
		ID:                opts.ID,
		CWD:               opts.CWD,
		ParentSessionPath: parentSessionPath,
		ParentRef:         parentRef,
		SpawnedBy:         opts.SpawnedBy,
	})
	if err != nil {
		return nil, err
	}
	if opts.SkipConversationRestore {
		// pi CHANGELOG #286 and extensions/types.ts:1021 define this as
		// creating the fork file without restoring conversation messages.
		return forked, nil
	}
	storage := forked.GetStorage()
	for _, entry := range entries {
		if err := storage.AppendEntry(entry); err != nil {
			return nil, err
		}
	}
	return forked, nil
}

func (r *JsonlSessionRepo) root() (string, error) {
	if r.sessionsRoot == "" {
		return "", sessionError(ErrorInvalidSession, "sessions root is empty")
	}
	return filepath.Abs(r.sessionsRoot)
}

func (r *JsonlSessionRepo) sessionDir(cwd string) (string, error) {
	root, err := r.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, encodeCWD(cwd)), nil
}

func getEntriesToFork(
	storage SessionStorage,
	entryID *EntryID,
	position ForkPosition,
) ([]SessionEntry, error) {
	if entryID == nil {
		return storage.GetEntries(), nil
	}

	target, ok := storage.GetEntry(*entryID)
	if !ok {
		return nil, sessionError(ErrorInvalidForkTarget, fmt.Sprintf("Entry %s not found", *entryID))
	}
	if position == ForkAt {
		return storage.GetPathToRoot(&target.ID)
	}

	msg, ok := message.AsAIMessage(target.Message)
	if target.Type != "message" || !ok || msg.Role != ai.RoleUser {
		return nil, sessionError(
			ErrorInvalidForkTarget,
			fmt.Sprintf("Entry %s is not a user message", *entryID),
		)
	}
	return storage.GetPathToRoot(target.ParentID)
}

func encodeCWD(cwd string) string {
	// pi strips exactly ONE leading separator (replace(/^[/\\]/, "") is
	// non-global, session-manager.ts:431), so "//host/share" and
	// "/host/share" encode to distinct directories.
	trimmed := cwd
	if len(trimmed) > 0 && (trimmed[0] == '/' || trimmed[0] == '\\') {
		trimmed = trimmed[1:]
	}
	replacer := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return "--" + replacer.Replace(trimmed) + "--"
}

func sessionFileName(timestamp string, id string) string {
	safeTimestamp := strings.NewReplacer(":", "-", ".", "-").Replace(timestamp)
	return safeTimestamp + "_" + id + ".jsonl"
}

func isInvalidSessionError(err error) bool {
	sessionErr, ok := err.(*Error)
	return ok && sessionErr.Code == ErrorInvalidSession
}
