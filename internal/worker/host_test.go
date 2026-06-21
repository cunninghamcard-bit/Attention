package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/backend/local"
)

// fakeAgent records calls and simulates a run that stays busy briefly.
type fakeAgent struct {
	prompts, steers, cancels atomic.Int32
	blockPrompt              chan struct{}
}

func (f *fakeAgent) Start(context.Context) error { return nil }

func (f *fakeAgent) HandleInput(ctx context.Context, in backend.Input) error {
	if in.Mode == backend.InputSteer {
		f.steers.Add(1)
		return nil
	}
	f.prompts.Add(1)
	if f.blockPrompt != nil {
		select {
		case <-f.blockPrompt:
		case <-ctx.Done():
		}
		return nil
	}
	time.Sleep(50 * time.Millisecond)
	return nil
}

func (f *fakeAgent) CancelActiveRun(context.Context, string) error {
	f.cancels.Add(1)
	return nil
}
func (f *fakeAgent) SetModel(context.Context, string) error    { return nil }
func (f *fakeAgent) SetThinking(context.Context, string) error { return nil }
func (f *fakeAgent) Stop(context.Context, string) error        { return nil }

func TestHostRoutesPerSession(t *testing.T) {
	intake := local.NewJobQueue(16)
	fb := &fakeAgent{}
	h := New(Options{
		Queue:   intake,
		Factory: func(sessionID string) (backend.Agent, error) { return fb, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = h.Run(ctx)
	}()

	_ = intake.Enqueue(ctx, backend.Job{
		SessionID: "a",
		Kind:      backend.JobPrompt,
		Payload:   []byte(`{"text":"x"}`),
	})
	_ = intake.Enqueue(ctx, backend.Job{
		SessionID: "b",
		Kind:      backend.JobPrompt,
		Payload:   []byte(`{"text":"y"}`),
	})

	deadline := time.After(2 * time.Second)
	for fb.prompts.Load() != 2 {
		select {
		case <-deadline:
			t.Fatalf("prompts=%d want 2", fb.prompts.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHostSessionAffinityRoutesTargetedJobsToOwner(t *testing.T) {
	queue := newAffinityTestQueue()
	signal := newAffinityTestSignal()
	agent := &fakeAgent{blockPrompt: make(chan struct{})}
	h := New(Options{
		Queue:    queue,
		Affinity: &singleAcquireAffinity{},
		Signal:   signal,
		SessionLease: func(ctx context.Context, sessionID string) (backend.LeasedJob, bool, error) {
			return queue.LeaseSession(ctx, sessionID)
		},
		Factory: func(sessionID string) (backend.Agent, error) { return agent, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = h.Run(ctx)
	}()

	queue.global <- backend.LeasedJob{
		Job: backend.Job{
			SessionID: "affinity-session",
			RunID:     "run_affinity",
			Kind:      backend.JobPrompt,
			Payload:   []byte(`{"text":"x"}`),
		},
		LeaseToken: "prompt-token",
	}

	waitForCount(t, &agent.prompts, 1)
	select {
	case <-signal.subscribed:
	case <-time.After(time.Second):
		t.Fatal("session signal was not subscribed")
	}

	queue.targeted <- backend.LeasedJob{
		Job: backend.Job{
			SessionID: "affinity-session",
			Kind:      backend.JobCancel,
			Payload:   []byte(`{}`),
		},
		LeaseToken: "cancel-token",
	}
	signal.Publish("affinity-session")
	waitForCount(t, &agent.cancels, 1)
	close(agent.blockPrompt)
}

func waitForCount(t *testing.T, counter *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for counter.Load() != want {
		select {
		case <-deadline:
			t.Fatalf("counter=%d want %d", counter.Load(), want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type affinityTestQueue struct {
	global   chan backend.LeasedJob
	targeted chan backend.LeasedJob
}

func newAffinityTestQueue() *affinityTestQueue {
	return &affinityTestQueue{
		global:   make(chan backend.LeasedJob, 4),
		targeted: make(chan backend.LeasedJob, 4),
	}
}

func (q *affinityTestQueue) Enqueue(ctx context.Context, job backend.Job) error {
	return nil
}

func (q *affinityTestQueue) Lease(ctx context.Context) (backend.LeasedJob, error) {
	select {
	case job := <-q.global:
		return job, nil
	case <-ctx.Done():
		return backend.LeasedJob{}, ctx.Err()
	}
}

func (q *affinityTestQueue) LeaseSession(ctx context.Context, sessionID string) (backend.LeasedJob, bool, error) {
	select {
	case job := <-q.targeted:
		return job, true, nil
	case <-ctx.Done():
		return backend.LeasedJob{}, false, ctx.Err()
	default:
		return backend.LeasedJob{}, false, nil
	}
}

func (q *affinityTestQueue) Ack(ctx context.Context, leaseToken string) error {
	return nil
}

func (q *affinityTestQueue) Nack(ctx context.Context, leaseToken string, retryAfter time.Duration) error {
	return nil
}

func (q *affinityTestQueue) Heartbeat(ctx context.Context, leaseToken string) error {
	return nil
}

type affinityTestSignal struct {
	mu         sync.Mutex
	subs       map[string]chan struct{}
	subscribed chan string
}

func newAffinityTestSignal() *affinityTestSignal {
	return &affinityTestSignal{
		subs:       map[string]chan struct{}{},
		subscribed: make(chan string, 1),
	}
}

func (s *affinityTestSignal) Publish(sessionID string) {
	s.mu.Lock()
	ch := s.subs[sessionID]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *affinityTestSignal) Subscribe(sessionID string) (<-chan struct{}, func()) {
	s.mu.Lock()
	ch := make(chan struct{}, 1)
	s.subs[sessionID] = ch
	s.mu.Unlock()
	select {
	case s.subscribed <- sessionID:
	default:
	}
	return ch, func() {}
}

type singleAcquireAffinity struct{}

func (a *singleAcquireAffinity) Acquire(
	ctx context.Context,
	sessionID string,
) (func(), bool, error) {
	return func() {}, true, nil
}
