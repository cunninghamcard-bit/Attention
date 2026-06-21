package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

const extCommandGrace = 2 * time.Second

var extCommandAPIDeadline = backend.ExtCommandWorkerTimeout + extCommandGrace

func (s *Server) dispatchExtCommandCrossProcess(
	w http.ResponseWriter,
	r *http.Request,
	req protocol.ExtCommandRequest,
) {
	if s.opts.Queue == nil || s.opts.Store == nil || s.opts.Bus == nil {
		writeError(w, http.StatusInternalServerError, "xproc_unavailable", "ext.command cross-process dependencies not configured")
		return
	}

	ctx := r.Context()
	cursor, err := s.currentSessionCursor(ctx, req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "event_log_unavailable", err.Error())
		return
	}
	notify, cancel := s.opts.Bus.Subscribe(req.SessionID)
	defer cancel()

	corrID := protocol.NewCorrID()
	payload, err := json.Marshal(protocol.ExtCommandJobPayload{
		PluginID:  req.PluginID,
		Owner:     req.Owner,
		SessionID: req.SessionID,
		EnvID:     req.EnvID,
		Name:      req.Name,
		Payload:   append(json.RawMessage(nil), req.Payload...),
		CorrID:    corrID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.opts.Queue.Enqueue(ctx, backend.Job{
		SessionID: req.SessionID,
		Kind:      backend.JobExtCommand,
		Payload:   payload,
	}); err != nil {
		writeQueueError(w, err)
		return
	}

	deadline := time.NewTimer(extCommandAPIDeadline)
	defer deadline.Stop()

	for {
		result, failed, ok, err := s.readExtCommandResult(ctx, req.SessionID, corrID, &cursor)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			writeError(w, http.StatusInternalServerError, "event_log_unavailable", err.Error())
			return
		}
		if ok {
			if failed != nil {
				code := failed.Code
				if code == "" {
					code = "ext_command_failed"
				}
				writeError(w, http.StatusUnprocessableEntity, code, failed.Message)
				return
			}
			writeJSON(w, http.StatusOK, protocol.ExtCommandResponse{Result: result.Result})
			return
		}

		select {
		case <-notify:
		case <-deadline.C:
			writeError(w, http.StatusGatewayTimeout, "ext_command_timeout", "ext.command result deadline exceeded")
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) currentSessionCursor(ctx context.Context, sessionID string) (uint64, error) {
	var cursor uint64
	for {
		batch, err := s.opts.Store.ReadAfter(ctx, sessionID, cursor, sseBatchLimit)
		if err != nil {
			return 0, fmt.Errorf("read event cursor: %w", err)
		}
		for _, event := range batch {
			cursor = event.Seq
		}
		if len(batch) < sseBatchLimit {
			return cursor, nil
		}
	}
}

func (s *Server) readExtCommandResult(
	ctx context.Context,
	sessionID string,
	corrID string,
	cursor *uint64,
) (protocol.ExtCommandResultPayload, *protocol.ExtCommandFailedPayload, bool, error) {
	for {
		batch, err := s.opts.Store.ReadAfter(ctx, sessionID, *cursor, sseBatchLimit)
		if err != nil {
			return protocol.ExtCommandResultPayload{}, nil, false, fmt.Errorf("read ext.command result: %w", err)
		}
		for _, event := range batch {
			*cursor = event.Seq
			switch event.Kind {
			case protocol.KindExtCommandResult:
				var payload protocol.ExtCommandResultPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return protocol.ExtCommandResultPayload{}, nil, false, fmt.Errorf("decode ext.command.result: %w", err)
				}
				if payload.CorrID == corrID {
					return payload, nil, true, nil
				}
			case protocol.KindExtCommandFailed:
				var payload protocol.ExtCommandFailedPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return protocol.ExtCommandResultPayload{}, nil, false, fmt.Errorf("decode ext.command.failed: %w", err)
				}
				if payload.CorrID == corrID {
					return protocol.ExtCommandResultPayload{}, &payload, true, nil
				}
			}
		}
		if len(batch) < sseBatchLimit {
			return protocol.ExtCommandResultPayload{}, nil, false, nil
		}
	}
}
