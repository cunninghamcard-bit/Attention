package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/pipeline"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func NewEmitter(store backend.EventStore, bus backend.NotifyBus) pipeline.Emitter {
	return func(tc *pipeline.RunContext, kind string, actor protocol.Actor, payload any) error {
		if store == nil {
			err := errors.New("worker: emitter store is nil")
			slog.Error("worker: emit failed", "kind", kind, "err", err)
			return err
		}
		if bus == nil {
			err := errors.New("worker: emitter bus is nil")
			slog.Error("worker: emit failed", "kind", kind, "err", err)
			return err
		}
		if tc == nil {
			err := errors.New("worker: emitter run context is nil")
			slog.Error("worker: emit failed", "kind", kind, "err", err)
			return err
		}

		raw, err := json.Marshal(payload)
		if err != nil {
			slog.Error("worker: marshal event payload failed", "kind", kind, "err", err)
			return err
		}

		env := &protocol.Envelope{
			ID:            protocol.NewEventID(),
			SessionID:     tc.SessionID,
			RunID:         tc.RunID,
			Kind:          kind,
			Actor:         actor,
			Payload:       raw,
			OccurredAt:    time.Now().UTC(),
			SchemaVersion: protocol.SchemaVersion,
		}
		if err := store.Append(context.Background(), env); err != nil {
			slog.Error("worker: append event failed", "session", tc.SessionID, "run", tc.RunID, "kind", kind, "err", err)
			return err
		}
		bus.Publish(tc.SessionID)
		return nil
	}
}
