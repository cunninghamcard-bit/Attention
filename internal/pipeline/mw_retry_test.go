package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func TestMWRetryAttemptsAndEmits(t *testing.T) {
	nextErr := errors.New("next failed")

	tests := []struct {
		name          string
		cfg           RetryConfig
		results       []ai.Message
		nextErr       error
		wantCalls     int
		wantErr       error
		wantKinds     []string
		wantDelays    []time.Duration
		wantExhausted bool
	}{
		{
			name: "retryable errors eventually succeed",
			cfg: RetryConfig{
				MaxRetries: intPtr(3),
				BaseDelay:  durationPtr(time.Millisecond),
			},
			results: []ai.Message{
				retryableAssistantMessage("initial-model", "503 service unavailable"),
				retryableAssistantMessage("initial-model", "connection lost"),
				assistantMessage("initial-model", ai.StopReasonStop, "ok"),
			},
			wantCalls:  3,
			wantKinds:  []string{protocol.KindRetryAttempted, protocol.KindRetryAttempted},
			wantDelays: []time.Duration{time.Millisecond, 2 * time.Millisecond},
		},
		{
			name: "retryable errors emit exhausted at max",
			cfg: RetryConfig{
				MaxRetries: intPtr(2),
				BaseDelay:  durationPtr(time.Millisecond),
			},
			results: []ai.Message{
				retryableAssistantMessage("initial-model", "503 service unavailable"),
				retryableAssistantMessage("initial-model", "502 bad gateway"),
				retryableAssistantMessage("initial-model", "503 still unavailable"),
			},
			wantCalls:     3,
			wantKinds:     []string{protocol.KindRetryAttempted, protocol.KindRetryAttempted, protocol.KindRetryExhausted},
			wantDelays:    []time.Duration{time.Millisecond, 2 * time.Millisecond},
			wantExhausted: true,
		},
		{
			name: "non retryable error is returned without events",
			cfg: RetryConfig{
				MaxRetries: intPtr(3),
				BaseDelay:  durationPtr(0),
			},
			results: []ai.Message{
				retryableAssistantMessage("initial-model", "invalid API key"),
			},
			wantCalls: 1,
		},
		{
			name: "disabled retry does not retry",
			cfg: RetryConfig{
				Enabled:    boolPtr(false),
				MaxRetries: intPtr(3),
				BaseDelay:  durationPtr(0),
			},
			results: []ai.Message{
				retryableAssistantMessage("initial-model", "server error"),
			},
			wantCalls: 1,
		},
		{
			name: "next error stops immediately",
			cfg: RetryConfig{
				MaxRetries: intPtr(3),
				BaseDelay:  durationPtr(0),
			},
			nextErr:   nextErr,
			wantCalls: 1,
			wantErr:   nextErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &fakeSessionView{}
			tc := &RunContext{
				Session: session,
				Agent: AgentSnapshot{
					Model: ai.Model{
						ID:            "initial-model",
						Provider:      "test-provider",
						ContextWindow: 100,
					},
				},
			}
			emitter := &capturingEmitter{}
			var delays []time.Duration
			tt.cfg.Wait = func(ctx context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			}
			calls := 0
			next := func(context.Context, *RunContext) error {
				calls++
				if tt.nextErr != nil {
					return tt.nextErr
				}
				index := calls - 1
				if index >= len(tt.results) {
					index = len(tt.results) - 1
				}
				return session.AppendMessage(tt.results[index])
			}

			err := MWRetry(tt.cfg, emitter.emit)(context.Background(), tc, next)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("MWRetry error = %v, want %v", err, tt.wantErr)
			}
			if calls != tt.wantCalls {
				t.Fatalf("next calls = %d, want %d", calls, tt.wantCalls)
			}
			if len(delays) != len(tt.wantDelays) {
				t.Fatalf("delays = %v, want %v", delays, tt.wantDelays)
			}
			for i, want := range tt.wantDelays {
				if delays[i] != want {
					t.Fatalf("delay[%d] = %s, want %s", i, delays[i], want)
				}
			}
			if len(emitter.events) != len(tt.wantKinds) {
				t.Fatalf("events = %#v, want kinds %v", emitter.events, tt.wantKinds)
			}
			for i, want := range tt.wantKinds {
				if emitter.events[i].kind != want || emitter.events[i].actor != protocol.ActorSystem {
					t.Fatalf("event[%d] = %#v, want kind %s actor system", i, emitter.events[i], want)
				}
				payload, ok := emitter.events[i].payload.(RetryPayload)
				if !ok {
					t.Fatalf("event[%d] payload = %T, want RetryPayload", i, emitter.events[i].payload)
				}
				if payload.Attempt <= 0 || payload.MaxAttempts <= 0 {
					t.Fatalf("retry payload missing attempt counts: %#v", payload)
				}
				if want == protocol.KindRetryExhausted && payload.FinalError != "503 still unavailable" {
					t.Fatalf("exhausted final error = %q, want last retry error", payload.FinalError)
				}
			}
		})
	}
}

func durationPtr(v time.Duration) *time.Duration {
	return &v
}
