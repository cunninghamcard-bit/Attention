package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func startTestServer(t *testing.T) (
	harness *testServer,
	store *testEventStore,
	bus *testNotifyBus,
	intake *testJobQueue,
) {
	t.Helper()

	dir := t.TempDir()
	store = newTestEventStore()
	bus = newTestNotifyBus()
	intake = newTestJobQueue(16)
	repo := session.NewJsonlSessionRepo(dir)
	srv := &Server{
		opts: Options{
			Addr:  "127.0.0.1:0",
			Token: "tok-test",
			Store: store,
			Bus:   bus,
			Queue: intake,
			Repo:  repo,
		},
		sessions: map[string]*session.Session{},
		metadata: map[string]session.Metadata{},
	}
	mux := http.NewServeMux()
	srv.routes(mux)
	return &testServer{handler: srv.middleware(mux)}, store, bus, intake
}

type testServer struct {
	handler http.Handler
}

func TestAuthRequired(t *testing.T) {
	harness, _, _, _ := startTestServer(t)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	harness.handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.Code)
	}

	var sessions []protocol.SessionInfo
	got := doJSON(t, harness, "GET", "/v1/sessions", nil, &sessions)
	if got.Header().Get("X-Along-Schema") != protocol.SchemaVersion {
		t.Fatalf("schema header = %q, want %q", got.Header().Get("X-Along-Schema"), protocol.SchemaVersion)
	}
}

func TestPromptFlowsToQueueAndEventsFlowBack(t *testing.T) {
	harness, store, bus, intake := startTestServer(t)
	ctx := context.Background()

	var created protocol.CreateSessionResponse
	doJSON(t, harness, "POST", "/v1/sessions", protocol.CreateSessionRequest{CWD: "/tmp"}, &created)
	if created.SessionID == "" {
		t.Fatal("no session id")
	}

	var pr protocol.PromptResponse
	doJSON(
		t,
		harness,
		"POST",
		"/v1/sessions/"+created.SessionID+"/prompt",
		protocol.PromptRequest{Text: "hi"},
		&pr,
	)
	if pr.RunID == "" {
		t.Fatal("no run id")
	}

	leased, err := intake.Lease(ctxWithTimeout(t, time.Second))
	job := leased.Job
	if err != nil ||
		job.Kind != backend.JobPrompt ||
		job.SessionID != created.SessionID ||
		job.RunID != pr.RunID {
		t.Fatalf("intake: %v %+v", err, leased)
	}
	var prompt protocol.PromptRequest
	if err := json.Unmarshal(job.Payload, &prompt); err != nil || prompt.Text != "hi" {
		t.Fatalf("prompt payload: %v %+v", err, prompt)
	}

	events := openSSE(t, harness, created.SessionID, 1, true)
	_ = store.Append(ctx, &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     created.SessionID,
		Kind:          protocol.KindRunStarted,
		Actor:         protocol.ActorSystem,
		SchemaVersion: protocol.SchemaVersion,
	})
	bus.Publish(created.SessionID)

	fr := events.next(t, 2*time.Second)
	if fr.Kind != protocol.KindRunStarted || fr.Seq != 2 {
		t.Fatalf("event frame: %+v", fr)
	}
}

func TestAfterSeqReplayAndFollowFalse(t *testing.T) {
	harness, store, bus, _ := startTestServer(t)
	ctx := context.Background()

	var created protocol.CreateSessionResponse
	doJSON(t, harness, "POST", "/v1/sessions", protocol.CreateSessionRequest{CWD: "/tmp"}, &created)

	for range 3 {
		if err := store.Append(ctx, event(created.SessionID, protocol.KindMessageDelta)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	replay := openSSE(t, harness, created.SessionID, 1, false)
	if fr := replay.next(t, time.Second); fr.Seq != 2 {
		t.Fatalf("first replay seq = %d, want 2", fr.Seq)
	}
	if fr := replay.next(t, time.Second); fr.Seq != 3 {
		t.Fatalf("second replay seq = %d, want 3", fr.Seq)
	}
	if fr := replay.next(t, time.Second); fr.Seq != 4 {
		t.Fatalf("third replay seq = %d, want 4", fr.Seq)
	}
	replay.done(t, 5*time.Second)

	lastEventReplay := openSSEWithLastEventID(t, harness, created.SessionID, "1", false)
	if fr := lastEventReplay.next(t, time.Second); fr.Seq != 2 {
		t.Fatalf("last-event first seq = %d, want 2", fr.Seq)
	}
	if fr := lastEventReplay.next(t, time.Second); fr.Seq != 3 {
		t.Fatalf("last-event second seq = %d, want 3", fr.Seq)
	}
	if fr := lastEventReplay.next(t, time.Second); fr.Seq != 4 {
		t.Fatalf("last-event third seq = %d, want 4", fr.Seq)
	}
	lastEventReplay.done(t, 5*time.Second)

	follow := openSSE(t, harness, created.SessionID, 4, true)
	follow.noFrame(t, 150*time.Millisecond)
	if err := store.Append(ctx, event(created.SessionID, protocol.KindRunCompleted)); err != nil {
		t.Fatalf("append fourth: %v", err)
	}
	bus.Publish(created.SessionID)
	if fr := follow.next(t, time.Second); fr.Seq != 5 {
		t.Fatalf("follow seq = %d, want 5", fr.Seq)
	}
}

func TestSessionCRUDAndFork(t *testing.T) {
	harness, _, _, intake := startTestServer(t)

	var created protocol.CreateSessionResponse
	doJSON(t, harness, "POST", "/v1/sessions", protocol.CreateSessionRequest{CWD: "/tmp/project"}, &created)
	if created.SessionID == "" {
		t.Fatal("no session id")
	}

	var sessions []protocol.SessionInfo
	doJSON(t, harness, "GET", "/v1/sessions", nil, &sessions)
	if !containsSession(sessions, created.SessionID) {
		t.Fatalf("list = %+v, want %s", sessions, created.SessionID)
	}

	var info protocol.SessionInfo
	doJSON(t, harness, "GET", "/v1/sessions/"+created.SessionID, nil, &info)
	if info.ID != created.SessionID {
		t.Fatalf("get info = %+v, want %s", info, created.SessionID)
	}

	var forked protocol.CreateSessionResponse
	doJSON(
		t,
		harness,
		"POST",
		"/v1/sessions/"+created.SessionID+"/fork",
		protocol.ForkSessionRequest{FromSeq: 0},
		&forked,
	)
	if forked.SessionID == "" || forked.SessionID == created.SessionID {
		t.Fatalf("forked session id = %q", forked.SessionID)
	}

	var ok protocol.OKResponse
	doJSON(
		t,
		harness,
		"POST",
		"/v1/sessions/"+forked.SessionID+"/cancel",
		protocol.CancelRequest{LastSeenSeq: 7},
		&ok,
	)
	if !ok.OK {
		t.Fatal("cancel response not ok")
	}
	leased, err := intake.Lease(ctxWithTimeout(t, time.Second))
	job := leased.Job
	if err != nil || job.Kind != backend.JobCancel || job.SessionID != forked.SessionID {
		t.Fatalf("cancel intake: %v %+v", err, leased)
	}
	var cancel protocol.CancelRequest
	if err := json.Unmarshal(job.Payload, &cancel); err != nil || cancel.LastSeenSeq != 7 {
		t.Fatalf("cancel payload: %v %+v", err, cancel)
	}

	doJSON(t, harness, "DELETE", "/v1/sessions/"+created.SessionID, nil, &ok)
	if !ok.OK {
		t.Fatal("delete response not ok")
	}
	doJSON(t, harness, "GET", "/v1/sessions", nil, &sessions)
	if containsSession(sessions, created.SessionID) {
		t.Fatalf("list after delete = %+v, still contains %s", sessions, created.SessionID)
	}
}

func doJSON(t *testing.T, harness *testServer, method string, target string, body any, out any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Authorization", "Bearer tok-test")
	req.Header.Set("X-Along-Schema", protocol.SchemaVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp := httptest.NewRecorder()
	harness.handler.ServeHTTP(resp, req)

	if resp.Code < 200 || resp.Code >= 300 {
		t.Fatalf("%s %s status = %d: %s", method, target, resp.Code, resp.Body.String())
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp
}

func openSSE(
	t *testing.T,
	harness *testServer,
	sessionID string,
	afterSeq uint64,
	follow bool,
) *sseStream {
	t.Helper()

	values := url.Values{}
	values.Set("after_seq", strconv.FormatUint(afterSeq, 10))
	values.Set("follow", strconv.FormatBool(follow))
	return openSSERequest(t, harness, sessionID, values, "")
}

func openSSEWithLastEventID(
	t *testing.T,
	harness *testServer,
	sessionID string,
	lastEventID string,
	follow bool,
) *sseStream {
	t.Helper()

	values := url.Values{}
	values.Set("follow", strconv.FormatBool(follow))
	return openSSERequest(t, harness, sessionID, values, lastEventID)
}

func openSSERequest(
	t *testing.T,
	harness *testServer,
	sessionID string,
	values url.Values,
	lastEventID string,
) *sseStream {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	target := "/v1/sessions/" + sessionID + "/events?" + values.Encode()
	req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tok-test")
	req.Header.Set("X-Along-Schema", protocol.SchemaVersion)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp := newStreamResponse()
	handlerDone := make(chan struct{})
	go func() {
		harness.handler.ServeHTTP(resp, req)
		resp.finish()
		close(handlerDone)
	}()
	resp.waitHeader(t)
	if resp.statusCode() != http.StatusOK {
		cancel()
		_ = resp.reader.Close()
		<-handlerDone
		t.Fatalf("sse status = %d", resp.statusCode())
	}
	if got := resp.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		cancel()
		_ = resp.reader.Close()
		<-handlerDone
		t.Fatalf("sse content type = %q", got)
	}

	stream := &sseStream{
		frames: make(chan protocol.Envelope, 16),
		doneCh: make(chan error, 1),
		close: func() error {
			cancel()
			err := resp.reader.Close()
			<-handlerDone
			return err
		},
	}
	t.Cleanup(func() { _ = stream.close() })
	go stream.read(resp.reader)
	return stream
}

type streamResponse struct {
	header      http.Header
	reader      *io.PipeReader
	writer      *io.PipeWriter
	headerReady chan struct{}
	headerOnce  sync.Once
	mu          sync.Mutex
	status      int
}

func newStreamResponse() *streamResponse {
	reader, writer := io.Pipe()
	return &streamResponse{
		header:      http.Header{},
		reader:      reader,
		writer:      writer,
		headerReady: make(chan struct{}),
	}
}

func (r *streamResponse) Header() http.Header {
	return r.header
}

func (r *streamResponse) WriteHeader(status int) {
	r.mu.Lock()
	if r.status == 0 {
		r.status = status
	}
	r.mu.Unlock()
	r.headerOnce.Do(func() { close(r.headerReady) })
}

func (r *streamResponse) Write(data []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.writer.Write(data)
}

func (r *streamResponse) Flush() {
	r.WriteHeader(http.StatusOK)
}

func (r *streamResponse) waitHeader(t *testing.T) {
	t.Helper()

	select {
	case <-r.headerReady:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response header")
	}
}

func (r *streamResponse) statusCode() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *streamResponse) finish() {
	_ = r.writer.Close()
}

type sseStream struct {
	frames chan protocol.Envelope
	doneCh chan error
	close  func() error
}

func (s *sseStream) next(t *testing.T, timeout time.Duration) protocol.Envelope {
	t.Helper()

	select {
	case frame := <-s.frames:
		return frame
	default:
	}

	select {
	case frame := <-s.frames:
		return frame
	case err := <-s.doneCh:
		select {
		case frame := <-s.frames:
			return frame
		default:
		}
		if err != nil {
			t.Fatalf("sse ended with error: %v", err)
		}
		t.Fatal("sse ended before next frame")
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for sse frame")
	}
	return protocol.Envelope{}
}

func (s *sseStream) noFrame(t *testing.T, duration time.Duration) {
	t.Helper()

	select {
	case frame := <-s.frames:
		t.Fatalf("unexpected frame: %+v", frame)
	case err := <-s.doneCh:
		if err != nil {
			t.Fatalf("sse ended with error: %v", err)
		}
		t.Fatal("sse ended unexpectedly")
	case <-time.After(duration):
	}
}

func (s *sseStream) done(t *testing.T, timeout time.Duration) {
	t.Helper()

	select {
	case frame := <-s.frames:
		t.Fatalf("unexpected frame before close: %+v", frame)
	default:
	}

	select {
	case frame := <-s.frames:
		t.Fatalf("unexpected frame before close: %+v", frame)
	case err := <-s.doneCh:
		if err != nil {
			t.Fatalf("sse ended with error: %v", err)
		}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for sse close")
	}
}

func (s *sseStream) read(body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var id string
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() == 0 {
				continue
			}
			frame, err := decodeSSEFrame(id, data.String())
			if err != nil {
				s.doneCh <- err
				return
			}
			s.frames <- frame
			id = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	s.doneCh <- scanner.Err()
}

func decodeSSEFrame(id string, data string) (protocol.Envelope, error) {
	var frame protocol.Envelope
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return protocol.Envelope{}, err
	}
	if id != strconv.FormatUint(frame.Seq, 10) {
		return protocol.Envelope{}, fmt.Errorf("sse id = %q, frame seq = %d", id, frame.Seq)
	}
	return frame, nil
}

func containsSession(sessions []protocol.SessionInfo, id string) bool {
	for _, info := range sessions {
		if info.ID == id {
			return true
		}
	}
	return false
}

func event(sessionID string, kind string) *protocol.Envelope {
	return &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     sessionID,
		Kind:          kind,
		Actor:         protocol.ActorSystem,
		SchemaVersion: protocol.SchemaVersion,
	}
}

func ctxWithTimeout(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

type testEventStore struct {
	mu     sync.Mutex
	events map[string][]protocol.Envelope
}

var _ backend.EventStore = (*testEventStore)(nil)

func newTestEventStore() *testEventStore {
	return &testEventStore{events: map[string][]protocol.Envelope{}}
}

func (s *testEventStore) Append(ctx context.Context, event *protocol.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	event.Seq = uint64(len(s.events[event.SessionID]) + 1)
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	s.events[event.SessionID] = append(s.events[event.SessionID], *event)
	return nil
}

func (s *testEventStore) ReadAfter(
	ctx context.Context,
	sessionID string,
	afterSeq uint64,
	limit int,
) ([]protocol.Envelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := []protocol.Envelope{}
	for _, event := range s.events[sessionID] {
		if event.Seq <= afterSeq {
			continue
		}
		out = append(out, event)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

type testNotifyBus struct {
	mu   sync.Mutex
	subs map[string]map[int]chan struct{}
	next int
}

var _ backend.NotifyBus = (*testNotifyBus)(nil)

func newTestNotifyBus() *testNotifyBus {
	return &testNotifyBus{subs: map[string]map[int]chan struct{}{}}
}

func (b *testNotifyBus) Publish(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ch := range b.subs[sessionID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (b *testNotifyBus) Subscribe(sessionID string) (<-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[int]chan struct{}{}
	}
	id := b.next
	b.next++
	ch := make(chan struct{}, 1)
	b.subs[sessionID][id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[sessionID], id)
			b.mu.Unlock()
		})
	}
}

type testJobQueue struct {
	ch chan backend.Job
}

var _ backend.JobQueue = (*testJobQueue)(nil)

func newTestJobQueue(size int) *testJobQueue {
	return &testJobQueue{ch: make(chan backend.Job, size)}
}

func (q *testJobQueue) Enqueue(ctx context.Context, job backend.Job) error {
	select {
	case q.ch <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *testJobQueue) Lease(ctx context.Context) (backend.LeasedJob, error) {
	select {
	case job := <-q.ch:
		return backend.LeasedJob{Job: job}, nil
	case <-ctx.Done():
		return backend.LeasedJob{}, ctx.Err()
	}
}

func (q *testJobQueue) Ack(ctx context.Context, leaseToken string) error {
	return nil
}

func (q *testJobQueue) Nack(ctx context.Context, leaseToken string, retryAfter time.Duration) error {
	return nil
}

func (q *testJobQueue) Heartbeat(ctx context.Context, leaseToken string) error {
	return nil
}
