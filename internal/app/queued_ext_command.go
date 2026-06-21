package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

const (
	queuedExtCommandReadLimit = 128
	extCommandDispatchGrace   = 5 * time.Second
)

var (
	ErrExtCommandNotSupportedInServer = errors.New("ext.command owner is not supported in server dispatcher")
	ErrExtCommandTimeout              = errors.New("ext.command result deadline exceeded")
	ErrExtCommandFailed               = errors.New("ext.command failed")
)

type ExtCommandNotSupportedInServerError struct {
	Owner     string
	SessionID string
	EnvID     string
}

func (e *ExtCommandNotSupportedInServerError) Error() string {
	if e.Owner == "" {
		return ErrExtCommandNotSupportedInServer.Error()
	}
	return fmt.Sprintf("ext.command owner %q is not supported in server dispatcher", e.Owner)
}

func (e *ExtCommandNotSupportedInServerError) Is(target error) bool {
	return target == ErrExtCommandNotSupportedInServer
}

func (e *ExtCommandNotSupportedInServerError) ExtCommandNotSupportedInServer() bool {
	return true
}

type ExtCommandTimeoutError struct {
	CorrID  string
	Timeout time.Duration
}

func (e *ExtCommandTimeoutError) Error() string {
	return ErrExtCommandTimeout.Error()
}

func (e *ExtCommandTimeoutError) Is(target error) bool {
	return target == ErrExtCommandTimeout || target == context.DeadlineExceeded
}

func (e *ExtCommandTimeoutError) ExtCommandTimeout() bool {
	return true
}

type ExtCommandFailedError struct {
	CorrID  string
	Code    string
	Message string
}

func (e *ExtCommandFailedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return "ext.command failed: " + e.Code
	}
	return ErrExtCommandFailed.Error()
}

func (e *ExtCommandFailedError) Is(target error) bool {
	return target == ErrExtCommandFailed
}

func (e *ExtCommandFailedError) ExtCommandFailed() bool {
	return true
}

type QueuedExtCommandDispatcher struct {
	Queue backend.JobQueue
	Store backend.EventStore
	Bus   backend.NotifyBus

	deadline  time.Duration
	readLimit int
}

func NewQueuedExtCommandDispatcher(
	queue backend.JobQueue,
	store backend.EventStore,
	bus backend.NotifyBus,
) *QueuedExtCommandDispatcher {
	return &QueuedExtCommandDispatcher{
		Queue: queue,
		Store: store,
		Bus:   bus,
	}
}

func (d *QueuedExtCommandDispatcher) Route(
	owner string,
	sessionID string,
	envID string,
) (backend.ExtCommandRouteMode, error) {
	return backend.ExtCommandRouteInProcess, nil
}

func (d *QueuedExtCommandDispatcher) DispatchCommand(
	ctx context.Context,
	pluginID string,
	owner string,
	sessionID string,
	envID string,
	name string,
	payload []byte,
) ([]byte, error) {
	if owner != "session" {
		return nil, &ExtCommandNotSupportedInServerError{
			Owner:     owner,
			SessionID: sessionID,
			EnvID:     envID,
		}
	}
	if err := d.validate(); err != nil {
		return nil, err
	}

	corrID := protocol.NewCorrID()
	cursor, err := d.currentCursor(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	jobPayload, err := json.Marshal(protocol.ExtCommandJobPayload{
		PluginID:  pluginID,
		Owner:     owner,
		SessionID: sessionID,
		EnvID:     envID,
		Name:      name,
		Payload:   cloneExtCommandPayload(payload),
		CorrID:    corrID,
	})
	if err != nil {
		return nil, fmt.Errorf("app: marshal ext.command job: %w", err)
	}
	if err := d.Queue.Enqueue(ctx, backend.Job{
		SessionID: sessionID,
		Kind:      backend.JobExtCommand,
		Payload:   jobPayload,
	}); err != nil {
		return nil, fmt.Errorf("app: enqueue ext.command job: %w", err)
	}

	notify, cancel := d.Bus.Subscribe(sessionID)
	defer cancel()

	deadline := time.NewTimer(d.dispatchDeadline())
	defer deadline.Stop()
	for {
		result, failed, ok, err := d.readResult(ctx, sessionID, corrID, &cursor)
		if err != nil {
			return nil, err
		}
		if ok {
			if failed != nil {
				return nil, &ExtCommandFailedError{
					CorrID:  failed.CorrID,
					Code:    failed.Code,
					Message: failed.Message,
				}
			}
			return cloneExtCommandPayload(result.Result), nil
		}

		select {
		case <-notify:
		case <-deadline.C:
			return nil, &ExtCommandTimeoutError{
				CorrID:  corrID,
				Timeout: d.dispatchDeadline(),
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (d *QueuedExtCommandDispatcher) validate() error {
	if d == nil {
		return errors.New("app: queued ext.command dispatcher is required")
	}
	if d.Queue == nil {
		return errors.New("app: queued ext.command queue is required")
	}
	if d.Store == nil {
		return errors.New("app: queued ext.command event store is required")
	}
	if d.Bus == nil {
		return errors.New("app: queued ext.command notify bus is required")
	}
	return nil
}

func (d *QueuedExtCommandDispatcher) currentCursor(
	ctx context.Context,
	sessionID string,
) (uint64, error) {
	var cursor uint64
	limit := d.limit()
	for {
		batch, err := d.Store.ReadAfter(ctx, sessionID, cursor, limit)
		if err != nil {
			return 0, fmt.Errorf("app: read ext.command cursor: %w", err)
		}
		for _, event := range batch {
			cursor = event.Seq
		}
		if len(batch) < limit {
			return cursor, nil
		}
	}
}

func (d *QueuedExtCommandDispatcher) readResult(
	ctx context.Context,
	sessionID string,
	corrID string,
	cursor *uint64,
) (protocol.ExtCommandResultPayload, *protocol.ExtCommandFailedPayload, bool, error) {
	limit := d.limit()
	for {
		batch, err := d.Store.ReadAfter(ctx, sessionID, *cursor, limit)
		if err != nil {
			return protocol.ExtCommandResultPayload{}, nil, false, fmt.Errorf("app: read ext.command result: %w", err)
		}
		for _, event := range batch {
			*cursor = event.Seq
			switch event.Kind {
			case protocol.KindExtCommandResult:
				var payload protocol.ExtCommandResultPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return protocol.ExtCommandResultPayload{}, nil, false, fmt.Errorf("app: decode ext.command result: %w", err)
				}
				if payload.CorrID == corrID {
					return payload, nil, true, nil
				}
			case protocol.KindExtCommandFailed:
				var payload protocol.ExtCommandFailedPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return protocol.ExtCommandResultPayload{}, nil, false, fmt.Errorf("app: decode ext.command failed: %w", err)
				}
				if payload.CorrID == corrID {
					return protocol.ExtCommandResultPayload{}, &payload, true, nil
				}
			}
		}
		if len(batch) < limit {
			return protocol.ExtCommandResultPayload{}, nil, false, nil
		}
	}
}

func (d *QueuedExtCommandDispatcher) dispatchDeadline() time.Duration {
	if d != nil && d.deadline > 0 {
		return d.deadline
	}
	return backend.ExtCommandWorkerTimeout + extCommandDispatchGrace
}

func (d *QueuedExtCommandDispatcher) limit() int {
	if d != nil && d.readLimit > 0 {
		return d.readLimit
	}
	return queuedExtCommandReadLimit
}

func cloneExtCommandPayload(payload []byte) json.RawMessage {
	if payload == nil {
		return nil
	}
	return append(json.RawMessage(nil), payload...)
}
