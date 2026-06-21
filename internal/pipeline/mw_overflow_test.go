package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func TestMWOverflowRecovery(t *testing.T) {
	tests := []struct {
		name          string
		cfg           OverflowConfig
		results       []ai.Message
		wantCalls     int
		wantCompact   int
		wantErr       error
		wantKinds     []string
		wantWillRetry bool
	}{
		{
			name: "overflow compacts and reruns next",
			cfg: OverflowConfig{
				Compact: successfulTestCompactor("compacted"),
			},
			results: []ai.Message{
				overflowAssistantMessage("initial-model"),
				assistantMessage("initial-model", ai.StopReasonStop, "recovered"),
			},
			wantCalls:     2,
			wantCompact:   1,
			wantKinds:     []string{protocol.KindCompactionStarted, protocol.KindCompactionCompleted},
			wantWillRetry: true,
		},
		{
			name: "non overflow error is ignored",
			cfg: OverflowConfig{
				Compact: successfulTestCompactor("compacted"),
			},
			results: []ai.Message{
				retryableAssistantMessage("initial-model", "invalid API key"),
			},
			wantCalls: 1,
		},
		{
			name: "disabled overflow recovery is ignored",
			cfg: OverflowConfig{
				Enabled: boolPtr(false),
				Compact: successfulTestCompactor("compacted"),
			},
			results: []ai.Message{
				overflowAssistantMessage("initial-model"),
			},
			wantCalls: 1,
		},
		{
			name: "model mismatch is ignored",
			cfg: OverflowConfig{
				Compact: successfulTestCompactor("compacted"),
			},
			results: []ai.Message{
				overflowAssistantMessage("other-model"),
			},
			wantCalls: 1,
		},
		{
			name: "second overflow is exhausted",
			cfg: OverflowConfig{
				Compact: successfulTestCompactor("compacted"),
			},
			results: []ai.Message{
				overflowAssistantMessage("initial-model"),
				overflowAssistantMessage("initial-model"),
			},
			wantCalls:     2,
			wantCompact:   1,
			wantErr:       errContextOverflowRecovery,
			wantKinds:     []string{protocol.KindCompactionStarted, protocol.KindCompactionCompleted},
			wantWillRetry: true,
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
			compactCalls := 0
			if tt.cfg.Compact != nil {
				base := tt.cfg.Compact
				tt.cfg.Compact = func(ctx context.Context, tc *RunContext, reason string) (CompactionResult, error) {
					compactCalls++
					return base(ctx, tc, reason)
				}
			}
			calls := 0
			next := func(context.Context, *RunContext) error {
				calls++
				index := calls - 1
				if index >= len(tt.results) {
					index = len(tt.results) - 1
				}
				return session.AppendMessage(tt.results[index])
			}

			err := MWOverflow(tt.cfg, emitter.emit)(context.Background(), tc, next)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("MWOverflow error = %v, want %v", err, tt.wantErr)
			}
			if calls != tt.wantCalls {
				t.Fatalf("next calls = %d, want %d", calls, tt.wantCalls)
			}
			if compactCalls != tt.wantCompact {
				t.Fatalf("compact calls = %d, want %d", compactCalls, tt.wantCompact)
			}
			if len(emitter.events) != len(tt.wantKinds) {
				t.Fatalf("events = %#v, want kinds %v", emitter.events, tt.wantKinds)
			}
			for i, want := range tt.wantKinds {
				if emitter.events[i].kind != want || emitter.events[i].actor != protocol.ActorSystem {
					t.Fatalf("event[%d] = %#v, want kind %s actor system", i, emitter.events[i], want)
				}
			}
			if len(emitter.events) == 0 {
				return
			}
			payload, ok := emitter.events[len(emitter.events)-1].payload.(CompactionPayload)
			if !ok {
				t.Fatalf("compaction completed payload = %T, want CompactionPayload", emitter.events[len(emitter.events)-1].payload)
			}
			if payload.Reason != overflowCompactionReason || payload.WillRetry != tt.wantWillRetry {
				t.Fatalf("compaction payload = %#v, want overflow reason willRetry=%t", payload, tt.wantWillRetry)
			}
		})
	}
}

func successfulTestCompactor(summary string) CompactionFunc {
	return func(context.Context, *RunContext, string) (CompactionResult, error) {
		return CompactionResult{
			Summary:          summary,
			FirstKeptEntryID: "first-kept",
			TokensBefore:     100,
		}, nil
	}
}
