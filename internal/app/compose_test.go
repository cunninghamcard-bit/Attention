package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func helloWorldStream() agentloop.StreamFunc {
	return func(context.Context, ai.Model, ai.Context, ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		partial := &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{{Type: ai.ContentText}}}
		final := &ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "helloworld"}},
			StopReason: ai.StopReasonStop,
		}
		return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
			yield(&ai.StreamEvent{Type: ai.EventMessageStart, Message: partial}, nil)
			yield(&ai.StreamEvent{Type: ai.EventTextDelta, Index: 0, Delta: &ai.ContentBlock{Type: ai.ContentText, Text: "hello"}, Message: partial}, nil)
			yield(&ai.StreamEvent{Type: ai.EventTextDelta, Index: 0, Delta: &ai.ContentBlock{Type: ai.ContentText, Text: "world"}, Message: final}, nil)
			yield(&ai.StreamEvent{Type: ai.EventMessageComplete, Message: final}, nil)
		})
	}
}

func TestComposeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	comp, err := Compose(ctx, ComposeOptions{
		DataDir:       t.TempDir(),
		CWD:           cwd,
		Model:         ai.Model{ID: "test-model", Provider: "test-provider", ContextWindow: 128_000},
		ThinkingLevel: agentloop.ThinkingOff,
		FakeStream:    helloWorldStream(),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer comp.Stop()

	sess, err := comp.Repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	sessionID := sess.GetMetadata().ID
	facade := comp.NewSessionFacade(sessionID)

	eventCh := make(chan compat.Event, 32)
	unsubscribe := facade.Subscribe(func(ev compat.Event) { eventCh <- ev })
	defer unsubscribe()

	result, err := facade.Prompt(ctx, compat.PromptInput{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Message.Role != ai.RoleAssistant {
		t.Fatalf("result role = %q, want assistant", result.Message.Role)
	}
	if got := result.Message.Content[0].Text; got != "helloworld" {
		t.Fatalf("result text = %q, want helloworld", got)
	}

	events := collectFacadeEvents(t, ctx, eventCh)
	if events[len(events)-1].Type != compat.EventAgentEnd {
		t.Fatalf("last facade event = %q, want agent_end", events[len(events)-1].Type)
	}

	envelopes, err := comp.Store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	gotKinds := make([]string, 0, len(envelopes))
	for i, env := range envelopes {
		gotKinds = append(gotKinds, env.Kind)
		if env.Seq != uint64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d", i, env.Seq, i+1)
		}
		if env.RunID == "" {
			t.Fatalf("envelope[%d] missing run id: %+v", i, env)
		}
		if env.SessionID != sessionID {
			t.Fatalf("envelope[%d] session = %q, want %q", i, env.SessionID, sessionID)
		}
	}
	wantKinds := []string{
		protocol.KindRunStarted,
		protocol.KindTurnStarted,
		protocol.KindMessageStarted,
		protocol.KindMessageCompleted,
		protocol.KindMessageStarted,
		protocol.KindMessageDelta,
		protocol.KindMessageDelta,
		protocol.KindMessageCompleted,
		protocol.KindTurnCompleted,
		protocol.KindRunCompleted,
	}
	if !slices.Equal(gotKinds, wantKinds) {
		t.Fatalf("kinds = %v, want %v", gotKinds, wantKinds)
	}
	runID := envelopes[0].RunID
	for i, env := range envelopes[1:] {
		if env.RunID != runID {
			t.Fatalf("runID[%d] = %q, want %q", i+1, env.RunID, runID)
		}
	}
}

func collectFacadeEvents(
	t *testing.T,
	ctx context.Context,
	eventCh <-chan compat.Event,
) []compat.Event {
	t.Helper()

	var events []compat.Event
	for {
		select {
		case ev := <-eventCh:
			events = append(events, ev)
			if ev.Type == compat.EventAgentEnd {
				return events
			}
		case <-ctx.Done():
			t.Fatalf("timeout collecting facade events; got %v", eventTypes(events))
		}
	}
}

func eventTypes(events []compat.Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}
