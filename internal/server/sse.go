package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

const (
	sseBatchLimit     = 128
	sseHeartbeatEvery = 15 * time.Second
	sseWriteTimeout   = 30 * time.Second
)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if !s.sessionExists(r.Context(), w, sessionID) {
		return
	}

	afterSeq, err := parseAfterSeq(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	follow, err := parseFollow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "sse_unsupported", "response writer cannot flush")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var notify <-chan struct{}
	cancel := func() {}
	if follow {
		notify, cancel = s.opts.Bus.Subscribe(sessionID)
		defer cancel()
	}

	controller := http.NewResponseController(w)
	heartbeat := time.NewTicker(sseHeartbeatEvery)
	defer heartbeat.Stop()

	cursor := afterSeq
	for {
		for {
			batch, err := s.opts.Store.ReadAfter(r.Context(), sessionID, cursor, sseBatchLimit)
			if err != nil {
				return
			}
			for _, event := range batch {
				if err := writeSSEFrame(w, controller, event); err != nil {
					return
				}
				flusher.Flush()
				cursor = event.Seq
			}
			if len(batch) < sseBatchLimit {
				break
			}
		}

		if !follow {
			return
		}

		select {
		case <-notify:
		case <-heartbeat.C:
			if err := writeSSEHeartbeat(w, controller); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func parseAfterSeq(r *http.Request) (uint64, error) {
	raw := r.URL.Query().Get("after_seq")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, nil
	}
	afterSeq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid after_seq %q", raw)
	}
	return afterSeq, nil
}

func parseFollow(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("follow")
	if raw == "" {
		return true, nil
	}
	follow, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid follow %q", raw)
	}
	return follow, nil
}

func writeSSEFrame(
	w http.ResponseWriter,
	controller *http.ResponseController,
	event protocol.Envelope,
) error {
	if err := renewSSEWriteDeadline(controller); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Seq, data)
	return err
}

func writeSSEHeartbeat(
	w http.ResponseWriter,
	controller *http.ResponseController,
) error {
	if err := renewSSEWriteDeadline(controller); err != nil {
		return err
	}
	_, err := fmt.Fprint(w, ":\n\n")
	return err
}

func renewSSEWriteDeadline(controller *http.ResponseController) error {
	err := controller.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}
