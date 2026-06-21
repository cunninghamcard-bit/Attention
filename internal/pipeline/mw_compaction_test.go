package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func TestMWCompactionEmitsEventsAndAppendsSummary(t *testing.T) {
	compactErr := errors.New("compact failed")

	tests := []struct {
		name            string
		cfg             CompactionConfig
		wantNext        bool
		wantCompact     bool
		wantErr         error
		wantKinds       []string
		wantSummaryText string
	}{
		{
			name: "successful compaction before next",
			cfg: CompactionConfig{
				Reason: "manual",
				Compact: func(ctx context.Context, tc *RunContext, reason string) (CompactionResult, error) {
					if reason != "manual" {
						t.Fatalf("reason = %q, want manual", reason)
					}
					return CompactionResult{
						Summary:          "summary",
						FirstKeptEntryID: "first-kept",
						TokensBefore:     42,
						Details:          map[string]any{"k": "v"},
					}, nil
				},
			},
			wantNext:        true,
			wantCompact:     true,
			wantKinds:       []string{protocol.KindCompactionStarted, protocol.KindCompactionCompleted},
			wantSummaryText: "summary",
		},
		{
			name: "compaction error emits completed and skips next",
			cfg: CompactionConfig{
				Reason: "manual",
				Compact: func(context.Context, *RunContext, string) (CompactionResult, error) {
					return CompactionResult{}, compactErr
				},
			},
			wantCompact: true,
			wantErr:     compactErr,
			wantKinds:   []string{protocol.KindCompactionStarted, protocol.KindCompactionCompleted},
		},
		{
			name: "disabled compaction goes straight to next",
			cfg: CompactionConfig{
				Enabled: boolPtr(false),
				Reason:  "manual",
				Compact: func(context.Context, *RunContext, string) (CompactionResult, error) {
					t.Fatal("compact should not be called")
					return CompactionResult{}, nil
				},
			},
			wantNext: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &fakeSessionView{}
			emitter := &capturingEmitter{}
			tc := &RunContext{Session: session}
			nextCalled := false

			err := MWCompaction(tt.cfg, emitter.emit)(
				context.Background(),
				tc,
				func(context.Context, *RunContext) error {
					nextCalled = true
					return nil
				},
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("MWCompaction error = %v, want %v", err, tt.wantErr)
			}
			if nextCalled != tt.wantNext {
				t.Fatalf("next called = %t, want %t", nextCalled, tt.wantNext)
			}
			if len(emitter.events) != len(tt.wantKinds) {
				t.Fatalf("events = %#v, want kinds %v", emitter.events, tt.wantKinds)
			}
			for i, want := range tt.wantKinds {
				if emitter.events[i].kind != want || emitter.events[i].actor != protocol.ActorSystem {
					t.Fatalf("event[%d] = %#v, want kind %s actor system", i, emitter.events[i], want)
				}
			}
			if tt.wantSummaryText == "" {
				return
			}
			if len(session.messages) != 1 {
				t.Fatalf("session messages = %d, want compaction summary", len(session.messages))
			}
			summary, ok := session.messages[0].(message.CompactionSummaryMessage)
			if !ok {
				t.Fatalf("summary message = %T, want CompactionSummaryMessage", session.messages[0])
			}
			if summary.Summary != tt.wantSummaryText || summary.TokensBefore != 42 {
				t.Fatalf("summary = %#v, want text %q tokens 42", summary, tt.wantSummaryText)
			}
		})
	}
}
