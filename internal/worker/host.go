// Package worker 是执行面：多会话宿主（D7）。一会话一 runtime 一锁（§7 三层纪律）。
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

type AgentFactory func(sessionID string) (backend.Agent, error)
type UIResolveFunc func(ctx context.Context, sessionID string, req protocol.UIResolveRequest) error
type SessionLeaseFunc func(ctx context.Context, sessionID string) (backend.LeasedJob, bool, error)
type ExtCommandRunner func(ctx context.Context, p protocol.ExtCommandJobPayload) ([]byte, error)

type Options struct {
	Queue            backend.JobQueue
	Factory          AgentFactory
	UIResolve        UIResolveFunc
	Affinity         backend.SessionAffinity
	Signal           backend.SessionSignal
	SessionLease     SessionLeaseFunc
	ExtCommandRunner ExtCommandRunner
	Store            backend.EventStore
	Bus              backend.NotifyBus
	Logger           *slog.Logger

	// LeaseHeartbeatInterval is used for server-form prompt leases. Zero keeps
	// desktop callers simple and defaults to a conservative interval.
	LeaseHeartbeatInterval time.Duration
}

type Host struct {
	opts Options

	mu       sync.Mutex
	runtimes map[string]*SessionRuntime
	owners   map[string]*sessionOwner
}

type sessionOwner struct {
	release func()
	cancel  context.CancelFunc
}

func New(opts Options) *Host {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Host{
		opts:     opts,
		runtimes: map[string]*SessionRuntime{},
		owners:   map[string]*sessionOwner{},
	}
}

// Run 租借收件箱消息并路由到会话 runtime；阻塞至 ctx 结束。
// dispatch 同步执行（steer/cancel 需要在 run 进行中立刻到达 backend）；
// prompt 在 runtime 内异步起 run，不阻塞本循环。
func (h *Host) Run(ctx context.Context) error {
	defer h.releaseAllSessionOwners()

	for {
		leased, err := h.opts.Queue.Lease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		job := leased.Job
		if h.affinityEnabled() {
			if backend.IsSessionTargetedJobKind(job.Kind) {
				h.opts.Logger.Error(
					"worker: targeted job leased globally",
					"session", job.SessionID,
					"kind", job.Kind,
				)
				h.nack(ctx, leased.LeaseToken)
				continue
			}
			acquired, err := h.ensureSessionOwner(ctx, job.SessionID)
			if err != nil {
				h.opts.Logger.Error("worker: session affinity acquire failed", "session", job.SessionID, "err", err)
				h.nack(ctx, leased.LeaseToken)
				continue
			}
			if !acquired {
				h.opts.Logger.Warn("worker: session affinity held elsewhere", "session", job.SessionID)
				h.nack(ctx, leased.LeaseToken)
				continue
			}
		}
		rt, err := h.runtime(ctx, job.SessionID)
		if err != nil {
			h.opts.Logger.Error("worker: runtime create failed", "session", job.SessionID, "err", err)
			h.nack(ctx, leased.LeaseToken)
			continue
		}
		if deferred := rt.dispatch(ctx, job, leased.LeaseToken); deferred {
			continue
		}
		if err := h.opts.Queue.Ack(ctx, leased.LeaseToken); err != nil {
			h.opts.Logger.Error(
				"worker: job ack failed",
				"session", job.SessionID,
				"run", job.RunID,
				"kind", job.Kind,
				"err", err,
			)
		}
	}
}

func (h *Host) affinityEnabled() bool {
	return h.opts.Affinity != nil
}

func (h *Host) ensureSessionOwner(ctx context.Context, sessionID string) (bool, error) {
	h.mu.Lock()
	if _, ok := h.owners[sessionID]; ok {
		h.mu.Unlock()
		return true, nil
	}
	h.mu.Unlock()

	release, acquired, err := h.opts.Affinity.Acquire(ctx, sessionID)
	if err != nil || !acquired {
		return acquired, err
	}

	ownerCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	if _, ok := h.owners[sessionID]; ok {
		h.mu.Unlock()
		cancel()
		release()
		return true, nil
	}
	h.owners[sessionID] = &sessionOwner{release: release, cancel: cancel}
	h.mu.Unlock()

	go h.watchSession(ownerCtx, sessionID)
	return true, nil
}

func (h *Host) watchSession(ctx context.Context, sessionID string) {
	var notify <-chan struct{}
	cancel := func() {}
	if h.opts.Signal != nil {
		notify, cancel = h.opts.Signal.Subscribe(sessionID)
	}
	defer cancel()

	h.drainSessionJobs(ctx, sessionID)
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				return
			}
			h.drainSessionJobs(ctx, sessionID)
		case <-ctx.Done():
			return
		}
	}
}

func (h *Host) drainSessionJobs(ctx context.Context, sessionID string) {
	if h.opts.SessionLease == nil {
		return
	}
	for {
		leased, ok, err := h.opts.SessionLease(ctx, sessionID)
		if err != nil {
			if ctx.Err() == nil {
				h.opts.Logger.Error("worker: session lease failed", "session", sessionID, "err", err)
			}
			return
		}
		if !ok {
			return
		}
		rt, err := h.runtime(ctx, leased.SessionID)
		if err != nil {
			h.opts.Logger.Error("worker: runtime create failed", "session", leased.SessionID, "err", err)
			h.nack(ctx, leased.LeaseToken)
			continue
		}
		if deferred := rt.dispatch(ctx, leased.Job, leased.LeaseToken); deferred {
			continue
		}
		if err := h.opts.Queue.Ack(ctx, leased.LeaseToken); err != nil {
			h.opts.Logger.Error(
				"worker: targeted job ack failed",
				"session", leased.SessionID,
				"run", leased.RunID,
				"kind", leased.Kind,
				"err", err,
			)
		}
	}
}

func (h *Host) releaseSessionOwner(sessionID string) {
	h.mu.Lock()
	owner, ok := h.owners[sessionID]
	if ok {
		delete(h.owners, sessionID)
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	owner.cancel()
	owner.release()
}

func (h *Host) releaseAllSessionOwners() {
	h.mu.Lock()
	owners := h.owners
	h.owners = map[string]*sessionOwner{}
	h.mu.Unlock()

	for _, owner := range owners {
		owner.cancel()
		owner.release()
	}
}

func (h *Host) nack(ctx context.Context, leaseToken string) {
	if leaseToken == "" {
		return
	}
	if err := h.opts.Queue.Nack(ctx, leaseToken, time.Second); err != nil {
		h.opts.Logger.Error("worker: job nack failed", "leaseToken", leaseToken, "err", err)
	}
}

func (h *Host) runtime(ctx context.Context, sessionID string) (*SessionRuntime, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rt, ok := h.runtimes[sessionID]; ok {
		return rt, nil
	}
	agent, err := h.opts.Factory(sessionID)
	if err != nil {
		return nil, err
	}
	if err := agent.Start(ctx); err != nil {
		return nil, err
	}
	rt := newRuntime(
		sessionID,
		agent,
		h.opts.Logger,
		h.opts.UIResolve,
		h.opts.Queue,
		h.leaseHeartbeatInterval(),
		h.opts.ExtCommandRunner,
		h.opts.Store,
		h.opts.Bus,
		h.releaseSessionOwner,
	)
	h.runtimes[sessionID] = rt
	return rt, nil
}

func (h *Host) leaseHeartbeatInterval() time.Duration {
	if h.opts.LeaseHeartbeatInterval > 0 {
		return h.opts.LeaseHeartbeatInterval
	}
	return 5 * time.Second
}
