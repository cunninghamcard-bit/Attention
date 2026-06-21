package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/backend/local"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func TestRuntimeDispatchExtCommandEmitsResult(t *testing.T) {
	store := local.NewEventStore(t.TempDir())
	bus := local.NewNotifyBus()
	rt := newRuntime(
		"ses_ext_result",
		&fakeAgent{},
		slog.Default(),
		nil,
		nil,
		0,
		func(ctx context.Context, p protocol.ExtCommandJobPayload) ([]byte, error) {
			return []byte(`{"ok":true}`), nil
		},
		store,
		bus,
		nil,
	)

	payload, _ := json.Marshal(protocol.ExtCommandJobPayload{
		PluginID:  "todo",
		Owner:     "session",
		SessionID: "ses_ext_result",
		Name:      "list",
		CorrID:    "cor_result",
	})
	rt.dispatch(context.Background(), backend.Job{
		SessionID: "ses_ext_result",
		Kind:      backend.JobExtCommand,
		Payload:   payload,
	}, "")

	events, err := store.ReadAfter(context.Background(), "ses_ext_result", 0, 10)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 1 || events[0].Kind != protocol.KindExtCommandResult {
		t.Fatalf("events = %+v, want one ext.command.result", events)
	}
	var result protocol.ExtCommandResultPayload
	if err := json.Unmarshal(events[0].Payload, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.CorrID != "cor_result" || string(result.Result) != `{"ok":true}` {
		t.Fatalf("result payload = %+v result=%s", result, string(result.Result))
	}
}

func TestRuntimeDispatchExtCommandEmitsFailed(t *testing.T) {
	store := local.NewEventStore(t.TempDir())
	bus := local.NewNotifyBus()
	rt := newRuntime(
		"ses_ext_failed",
		&fakeAgent{},
		slog.Default(),
		nil,
		nil,
		0,
		func(ctx context.Context, p protocol.ExtCommandJobPayload) ([]byte, error) {
			return nil, errors.New("boom")
		},
		store,
		bus,
		nil,
	)

	payload, _ := json.Marshal(protocol.ExtCommandJobPayload{
		PluginID:  "todo",
		Owner:     "session",
		SessionID: "ses_ext_failed",
		Name:      "list",
		CorrID:    "cor_failed",
	})
	rt.dispatch(context.Background(), backend.Job{
		SessionID: "ses_ext_failed",
		Kind:      backend.JobExtCommand,
		Payload:   payload,
	}, "")

	events, err := store.ReadAfter(context.Background(), "ses_ext_failed", 0, 10)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 1 || events[0].Kind != protocol.KindExtCommandFailed {
		t.Fatalf("events = %+v, want one ext.command.failed", events)
	}
	var failed protocol.ExtCommandFailedPayload
	if err := json.Unmarshal(events[0].Payload, &failed); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if failed.CorrID != "cor_failed" || failed.Code != "dispatch_failed" || failed.Message != "boom" {
		t.Fatalf("failed payload = %+v", failed)
	}
}
