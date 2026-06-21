package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func TestQueuedExtCommandDispatcherResult(t *testing.T) {
	ctx := context.Background()
	store := newQueuedExtCommandStore(t)
	bus := newQueuedExtCommandBus()
	queue := &queuedExtCommandQueue{}
	dispatcher := newTestQueuedExtCommandDispatcher(queue, store, bus)

	queue.onEnqueue = func(job backend.Job) {
		payload := decodeQueuedExtCommandJob(t, job)
		appendQueuedExtCommandEnvelope(t, store, bus, payload.SessionID, protocol.KindExtCommandResult, protocol.ExtCommandResultPayload{
			CorrID: payload.CorrID,
			Result: json.RawMessage(`{"ok":true}`),
		})
	}

	got, err := dispatcher.DispatchCommand(
		ctx,
		"fixture-plugin",
		"session",
		"ses_queued_result",
		"",
		"echo",
		[]byte(`{"message":"hi"}`),
	)
	if err != nil {
		t.Fatalf("DispatchCommand: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("result = %s, want {\"ok\":true}", string(got))
	}

	job := queue.onlyJob(t)
	if job.SessionID != "ses_queued_result" || job.Kind != backend.JobExtCommand {
		t.Fatalf("queued job = %+v", job)
	}
	payload := decodeQueuedExtCommandJob(t, job)
	if payload.PluginID != "fixture-plugin" ||
		payload.Owner != "session" ||
		payload.SessionID != "ses_queued_result" ||
		payload.Name != "echo" ||
		payload.CorrID == "" ||
		string(payload.Payload) != `{"message":"hi"}` {
		t.Fatalf("queued payload = %+v payload=%s", payload, string(payload.Payload))
	}
}

func TestQueuedExtCommandDispatcherFailed(t *testing.T) {
	ctx := context.Background()
	store := newQueuedExtCommandStore(t)
	bus := newQueuedExtCommandBus()
	queue := &queuedExtCommandQueue{}
	dispatcher := newTestQueuedExtCommandDispatcher(queue, store, bus)

	queue.onEnqueue = func(job backend.Job) {
		payload := decodeQueuedExtCommandJob(t, job)
		appendQueuedExtCommandEnvelope(t, store, bus, payload.SessionID, protocol.KindExtCommandFailed, protocol.ExtCommandFailedPayload{
			CorrID:  payload.CorrID,
			Code:    "dispatch_failed",
			Message: "plugin command rejected",
		})
	}

	_, err := dispatcher.DispatchCommand(ctx, "fixture-plugin", "session", "ses_queued_failed", "", "fail", nil)
	if err == nil {
		t.Fatal("DispatchCommand succeeded, want error")
	}
	if !errors.Is(err, ErrExtCommandFailed) {
		t.Fatalf("error = %v, want ErrExtCommandFailed", err)
	}
	if status, code := queuedExtCommandHTTPMapping(err); status != http.StatusUnprocessableEntity || code != "ext_command_failed" {
		t.Fatalf("mapping = %d %s, want 422 ext_command_failed", status, code)
	}
}

func TestQueuedExtCommandDispatcherTimeout(t *testing.T) {
	ctx := context.Background()
	store := newQueuedExtCommandStore(t)
	bus := newQueuedExtCommandBus()
	queue := &queuedExtCommandQueue{}
	dispatcher := newTestQueuedExtCommandDispatcher(queue, store, bus)
	dispatcher.deadline = time.Millisecond

	_, err := dispatcher.DispatchCommand(ctx, "fixture-plugin", "session", "ses_queued_timeout", "", "slow", nil)
	if err == nil {
		t.Fatal("DispatchCommand succeeded, want timeout")
	}
	if !errors.Is(err, ErrExtCommandTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want timeout category", err)
	}
	if status, code := queuedExtCommandHTTPMapping(err); status != http.StatusGatewayTimeout || code != "ext_command_timeout" {
		t.Fatalf("mapping = %d %s, want 504 ext_command_timeout", status, code)
	}
}

func TestQueuedExtCommandDispatcherNonSessionOwner(t *testing.T) {
	dispatcher := newTestQueuedExtCommandDispatcher(
		&queuedExtCommandQueue{},
		newQueuedExtCommandStore(t),
		newQueuedExtCommandBus(),
	)

	_, err := dispatcher.DispatchCommand(context.Background(), "fixture-plugin", "engine", "", "", "echo", nil)
	if err == nil {
		t.Fatal("DispatchCommand succeeded, want unsupported owner error")
	}
	if !errors.Is(err, ErrExtCommandNotSupportedInServer) {
		t.Fatalf("error = %v, want ErrExtCommandNotSupportedInServer", err)
	}
	if status, code := queuedExtCommandHTTPMapping(err); status != http.StatusNotImplemented || code != "not_supported" {
		t.Fatalf("mapping = %d %s, want 501 not_supported", status, code)
	}
}

type queuedExtCommandQueue struct {
	mu        sync.Mutex
	jobs      []backend.Job
	onEnqueue func(backend.Job)
}

func (q *queuedExtCommandQueue) Enqueue(ctx context.Context, job backend.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	q.jobs = append(q.jobs, backend.Job{
		SessionID: job.SessionID,
		RunID:     job.RunID,
		Kind:      job.Kind,
		Payload:   append([]byte(nil), job.Payload...),
	})
	q.mu.Unlock()
	if q.onEnqueue != nil {
		q.onEnqueue(job)
	}
	return nil
}

func (q *queuedExtCommandQueue) Lease(ctx context.Context) (backend.LeasedJob, error) {
	return backend.LeasedJob{}, ctx.Err()
}

func (q *queuedExtCommandQueue) Ack(context.Context, string) error {
	return nil
}

func (q *queuedExtCommandQueue) Nack(context.Context, string, time.Duration) error {
	return nil
}

func (q *queuedExtCommandQueue) Heartbeat(context.Context, string) error {
	return nil
}

func (q *queuedExtCommandQueue) onlyJob(t *testing.T) backend.Job {
	t.Helper()

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.jobs) != 1 {
		t.Fatalf("queued jobs = %d, want 1", len(q.jobs))
	}
	return q.jobs[0]
}

type queuedExtCommandStore struct {
	t      *testing.T
	mu     sync.Mutex
	events map[string][]protocol.Envelope
}

func newQueuedExtCommandStore(t *testing.T) *queuedExtCommandStore {
	t.Helper()

	return &queuedExtCommandStore{
		t:      t,
		events: map[string][]protocol.Envelope{},
	}
}

func (s *queuedExtCommandStore) Append(ctx context.Context, event *protocol.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nextSeq := uint64(len(s.events[event.SessionID]) + 1)
	copied := *event
	copied.Seq = nextSeq
	copied.Payload = append(json.RawMessage(nil), event.Payload...)
	s.events[event.SessionID] = append(s.events[event.SessionID], copied)
	event.Seq = nextSeq
	return nil
}

func (s *queuedExtCommandStore) ReadAfter(
	ctx context.Context,
	sessionID string,
	afterSeq uint64,
	limit int,
) ([]protocol.Envelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		s.t.Fatalf("ReadAfter limit = %d, want positive cursor page limit", limit)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var out []protocol.Envelope
	for _, event := range s.events[sessionID] {
		if event.Seq <= afterSeq {
			continue
		}
		copied := event
		copied.Payload = append(json.RawMessage(nil), event.Payload...)
		out = append(out, copied)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type queuedExtCommandBus struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

func newQueuedExtCommandBus() *queuedExtCommandBus {
	return &queuedExtCommandBus{subs: map[string][]chan struct{}{}}
}

func (b *queuedExtCommandBus) Publish(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[sessionID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (b *queuedExtCommandBus) Subscribe(sessionID string) (<-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{}, 1)
	b.subs[sessionID] = append(b.subs[sessionID], ch)
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.subs[sessionID]
		for i, sub := range subs {
			if sub == ch {
				b.subs[sessionID] = append(subs[:i], subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
}

func newTestQueuedExtCommandDispatcher(
	queue backend.JobQueue,
	store backend.EventStore,
	bus backend.NotifyBus,
) *QueuedExtCommandDispatcher {
	dispatcher := NewQueuedExtCommandDispatcher(queue, store, bus)
	dispatcher.deadline = time.Second
	dispatcher.readLimit = 2
	return dispatcher
}

func appendQueuedExtCommandEnvelope(
	t *testing.T,
	store backend.EventStore,
	bus backend.NotifyBus,
	sessionID string,
	kind string,
	payload any,
) {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", kind, err)
	}
	if err := store.Append(context.Background(), &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     sessionID,
		Kind:          kind,
		Actor:         protocol.ActorSystem,
		Payload:       raw,
		OccurredAt:    time.Now().UTC(),
		SchemaVersion: protocol.SchemaVersion,
	}); err != nil {
		t.Fatalf("append %s envelope: %v", kind, err)
	}
	bus.Publish(sessionID)
}

func decodeQueuedExtCommandJob(t *testing.T, job backend.Job) protocol.ExtCommandJobPayload {
	t.Helper()

	var payload protocol.ExtCommandJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode queued ext.command job: %v", err)
	}
	return payload
}

func queuedExtCommandHTTPMapping(err error) (int, string) {
	var unsupported interface {
		error
		ExtCommandNotSupportedInServer() bool
	}
	var timeout interface {
		error
		ExtCommandTimeout() bool
	}
	var failed interface {
		error
		ExtCommandFailed() bool
	}
	switch {
	case errors.As(err, &unsupported) && unsupported.ExtCommandNotSupportedInServer():
		return http.StatusNotImplemented, "not_supported"
	case errors.As(err, &timeout) && timeout.ExtCommandTimeout():
		return http.StatusGatewayTimeout, "ext_command_timeout"
	case errors.As(err, &failed) && failed.ExtCommandFailed():
		return http.StatusUnprocessableEntity, "ext_command_failed"
	default:
		return http.StatusInternalServerError, "internal"
	}
}
