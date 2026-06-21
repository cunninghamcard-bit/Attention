package worker

// SessionRuntime：每会话状态桶 + 消息分发。run 串行（一次一个 prompt），
// steer/cancel 在 run 进行中也要立刻到达 backend（语义对照 orchestrator
// Steer/Abort：internal/orchestrator/orchestrator.go:1013/:1333）。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

type SessionRuntime struct {
	sessionID string
	agent     backend.Agent
	log       *slog.Logger
	uiResolve UIResolveFunc
	queue     backend.JobQueue
	store     backend.EventStore
	bus       backend.NotifyBus

	leaseHeartbeatInterval time.Duration
	extCommandRunner       ExtCommandRunner
	onIdle                 func(sessionID string)

	mu      sync.Mutex
	busy    bool            // run 进行中
	pending []pendingPrompt // follow-up 队列（busy 时收到的 prompt）
	runCtx  context.CancelFunc
}

type pendingPrompt struct {
	runID         string
	text          string
	leaseToken    string
	stopHeartbeat func()
}

func newRuntime(
	id string,
	be backend.Agent,
	log *slog.Logger,
	uiResolve UIResolveFunc,
	queue backend.JobQueue,
	leaseHeartbeatInterval time.Duration,
	extCommandRunner ExtCommandRunner,
	store backend.EventStore,
	bus backend.NotifyBus,
	onIdle func(sessionID string),
) *SessionRuntime {
	return &SessionRuntime{
		sessionID:              id,
		agent:                  be,
		log:                    log,
		uiResolve:              uiResolve,
		queue:                  queue,
		leaseHeartbeatInterval: leaseHeartbeatInterval,
		extCommandRunner:       extCommandRunner,
		store:                  store,
		bus:                    bus,
		onIdle:                 onIdle,
	}
}

func (rt *SessionRuntime) dispatch(ctx context.Context, m backend.Job, leaseToken string) bool {
	switch m.Kind {
	case backend.JobPrompt:
		var p protocol.PromptRequest
		_ = json.Unmarshal(m.Payload, &p)
		rt.prompt(ctx, m.RunID, p.Text, leaseToken)
		return true
	case backend.JobSteer:
		var p protocol.SteerRequest
		_ = json.Unmarshal(m.Payload, &p)
		_ = rt.agent.HandleInput(ctx, backend.Input{Mode: backend.InputSteer, Text: p.Text})
	case backend.JobCancel:
		rt.mu.Lock()
		cancel := rt.runCtx
		rt.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		_ = rt.agent.CancelActiveRun(ctx, "user cancel")
	case backend.JobSetModel:
		var p protocol.SetModelRequest
		_ = json.Unmarshal(m.Payload, &p)
		_ = rt.agent.SetModel(ctx, p.Model)
	case backend.JobSetThinking:
		var p protocol.SetThinkingRequest
		_ = json.Unmarshal(m.Payload, &p)
		_ = rt.agent.SetThinking(ctx, p.Level)
	case backend.JobUIResolve:
		var p protocol.UIResolveRequest
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			rt.log.Error("ui.resolve decode failed", "session", rt.sessionID, "err", err)
			return false
		}
		if rt.uiResolve == nil {
			rt.log.Warn("ui.resolve dropped: handler not configured", "session", rt.sessionID, "requestId", p.RequestID)
			return false
		}
		if err := rt.uiResolve(ctx, m.SessionID, p); err != nil {
			rt.log.Error("ui.resolve failed", "session", rt.sessionID, "requestId", p.RequestID, "err", err)
		}
	case backend.JobStop:
		_ = rt.agent.Stop(ctx, "session stop")
		if rt.onIdle != nil {
			rt.onIdle(rt.sessionID)
		}
	case backend.JobExtCommand:
		rt.dispatchExtCommand(ctx, m)
	}
	return false
}

func (rt *SessionRuntime) dispatchExtCommand(ctx context.Context, m backend.Job) {
	var p protocol.ExtCommandJobPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		rt.log.Error("ext.command decode failed", "session", rt.sessionID, "err", err)
		return
	}
	if p.SessionID == "" {
		p.SessionID = m.SessionID
	}
	if rt.extCommandRunner == nil {
		err := errors.New("worker: ext.command runner not configured")
		rt.log.Error("ext.command runner missing", "session", rt.sessionID, "corrId", p.CorrID)
		rt.emitExtCommandFailed(ctx, m, p, "dispatch_unavailable", err.Error())
		return
	}

	tctx, cancel := context.WithTimeout(ctx, backend.ExtCommandWorkerTimeout)
	defer cancel()
	result, err := rt.extCommandRunner(tctx, p)
	if err != nil {
		rt.emitExtCommandFailed(ctx, m, p, extCommandFailureCode(err), err.Error())
		return
	}
	payload := protocol.ExtCommandResultPayload{
		CorrID: p.CorrID,
		Result: append(json.RawMessage(nil), result...),
	}
	if err := rt.emitExtCommandEnvelope(ctx, m, p.SessionID, protocol.KindExtCommandResult, payload); err != nil {
		rt.log.Error("ext.command result emit failed", "session", p.SessionID, "corrId", p.CorrID, "err", err)
	}
}

func (rt *SessionRuntime) emitExtCommandFailed(
	ctx context.Context,
	m backend.Job,
	p protocol.ExtCommandJobPayload,
	code string,
	message string,
) {
	payload := protocol.ExtCommandFailedPayload{
		CorrID:  p.CorrID,
		Code:    code,
		Message: message,
	}
	if err := rt.emitExtCommandEnvelope(ctx, m, p.SessionID, protocol.KindExtCommandFailed, payload); err != nil {
		rt.log.Error("ext.command failed emit failed", "session", p.SessionID, "corrId", p.CorrID, "err", err)
	}
}

func (rt *SessionRuntime) emitExtCommandEnvelope(
	ctx context.Context,
	m backend.Job,
	sessionID string,
	kind string,
	payload any,
) error {
	if rt.store == nil {
		return errors.New("worker: ext.command event store not configured")
	}
	if rt.bus == nil {
		return errors.New("worker: ext.command notify bus not configured")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     sessionID,
		RunID:         m.RunID,
		Kind:          kind,
		Actor:         protocol.ActorSystem,
		Payload:       raw,
		OccurredAt:    time.Now().UTC(),
		SchemaVersion: protocol.SchemaVersion,
	}
	if err := rt.store.Append(ctx, env); err != nil {
		return err
	}
	rt.bus.Publish(sessionID)
	return nil
}

func extCommandFailureCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "dispatch_failed"
}

func (rt *SessionRuntime) prompt(ctx context.Context, runID, text, leaseToken string) {
	rt.promptWithHeartbeat(ctx, runID, text, leaseToken, rt.startHeartbeat(ctx, leaseToken))
}

func (rt *SessionRuntime) promptWithHeartbeat(
	ctx context.Context,
	runID string,
	text string,
	leaseToken string,
	stopHeartbeat func(),
) {
	rt.mu.Lock()
	if rt.busy {
		rt.pending = append(rt.pending, pendingPrompt{
			runID:         runID,
			text:          text,
			leaseToken:    leaseToken,
			stopHeartbeat: stopHeartbeat,
		}) // follow-up 语义（orchestrator.go:1026）
		rt.mu.Unlock()
		return
	}
	rt.busy = true
	tctx, cancel := context.WithCancel(ctx)
	rt.runCtx = cancel
	rt.mu.Unlock()

	go func() {
		defer cancel()
		if err := rt.agent.HandleInput(tctx, backend.Input{Mode: backend.InputPrompt, Text: text, RunID: runID}); err != nil {
			rt.log.Error("run failed", "session", rt.sessionID, "err", err)
		}
		stopHeartbeat()
		rt.ackLease(ctx, leaseToken, runID)
		rt.mu.Lock()
		rt.busy = false
		rt.runCtx = nil
		var next *pendingPrompt
		if len(rt.pending) > 0 {
			n := rt.pending[0]
			rt.pending = rt.pending[1:]
			next = &n
		}
		rt.mu.Unlock()
		if next != nil {
			rt.promptWithHeartbeat(ctx, next.runID, next.text, next.leaseToken, next.stopHeartbeat)
			return
		}
		if rt.onIdle != nil {
			rt.onIdle(rt.sessionID)
		}
	}()
}

func (rt *SessionRuntime) startHeartbeat(ctx context.Context, leaseToken string) func() {
	if leaseToken == "" || rt.queue == nil || rt.leaseHeartbeatInterval <= 0 {
		return func() {}
	}
	hctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(rt.leaseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := rt.queue.Heartbeat(hctx, leaseToken); err != nil {
					rt.log.Error("worker: lease heartbeat failed", "session", rt.sessionID, "err", err)
					return
				}
			case <-hctx.Done():
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (rt *SessionRuntime) ackLease(ctx context.Context, leaseToken string, runID string) {
	if rt.queue == nil {
		rt.log.Error("worker: queue missing for lease ack", "session", rt.sessionID, "run", runID)
		return
	}
	if err := rt.queue.Ack(ctx, leaseToken); err != nil {
		rt.log.Error("worker: prompt ack failed", "session", rt.sessionID, "run", runID, "err", err)
	}
}
