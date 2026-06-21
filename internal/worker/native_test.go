package worker

import (
	"context"
	"slices"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/pipeline"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

// fakeSession 记录 AppendMessage。
type fakeSession struct{ appended []message.AgentMessage }

func (f *fakeSession) Messages() []message.AgentMessage { return nil }
func (f *fakeSession) AppendMessage(m message.AgentMessage) error {
	f.appended = append(f.appended, m)
	return nil
}

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

func TestNativePromptEmitsEnvelopeFlow(t *testing.T) {
	var kinds []string
	emit := func(tc *pipeline.RunContext, kind string, actor protocol.Actor, payload any) error {
		if tc.RunID != "run_1" {
			t.Fatalf("runID not threaded: %q", tc.RunID)
		}
		kinds = append(kinds, kind)
		return nil
	}
	sess := &fakeSession{}
	n := NewNative(NativeOptions{
		SessionID: "ses_a",
		Snapshot:  func() pipeline.AgentSnapshot { return pipeline.AgentSnapshot{} },
		Session:   sess,
		Emit:      emit,
		Stream:    helloWorldStream(),
	})

	if err := n.HandleInput(context.Background(), backend.Input{
		Mode: backend.InputPrompt, Text: "hi", RunID: "run_1",
	}); err != nil {
		t.Fatalf("HandleInput: %v", err)
	}

	if len(kinds) == 0 || kinds[0] != protocol.KindRunStarted {
		t.Fatalf("first kind: %v", kinds)
	}
	if kinds[len(kinds)-1] != protocol.KindRunCompleted {
		t.Fatalf("last kind: %v", kinds)
	}
	deltas := 0
	for _, k := range kinds {
		if k == protocol.KindMessageDelta {
			deltas++
		}
	}
	if deltas < 2 {
		t.Fatalf("want ≥2 message.delta, got %d in %v", deltas, kinds)
	}
	for _, want := range []string{protocol.KindTurnStarted, protocol.KindTurnCompleted, protocol.KindMessageCompleted} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("missing %s in %v", want, kinds)
		}
	}
	// run 产物写回会话存储（user prompt + assistant 回复）。
	if len(sess.appended) < 2 {
		t.Fatalf("session append: %d messages", len(sess.appended))
	}
}

func TestNativeRunFailedEnvelope(t *testing.T) {
	failStream := func(context.Context, ai.Model, ai.Context, ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
			yield(nil, context.DeadlineExceeded)
		})
	}
	var kinds []string
	emit := func(tc *pipeline.RunContext, kind string, actor protocol.Actor, payload any) error {
		kinds = append(kinds, kind)
		return nil
	}
	n := NewNative(NativeOptions{
		SessionID: "ses_a",
		Snapshot:  func() pipeline.AgentSnapshot { return pipeline.AgentSnapshot{} },
		Emit:      emit,
		Stream:    failStream,
	})
	err := n.HandleInput(context.Background(), backend.Input{Mode: backend.InputPrompt, Text: "hi", RunID: "run_1"})
	if err == nil {
		t.Fatal("want error")
	}
	if !slices.Contains(kinds, protocol.KindRunFailed) {
		t.Fatalf("missing run.failed in %v", kinds)
	}
}

func TestNativeSteerQueuesIntoLoop(t *testing.T) {
	n := NewNative(NativeOptions{SessionID: "ses_a"})
	if err := n.HandleInput(context.Background(), backend.Input{Mode: backend.InputSteer, Text: "turn left"}); err != nil {
		t.Fatal(err)
	}
	msgs, err := n.drainSteer(context.Background())
	if err != nil || len(msgs) != 1 {
		t.Fatalf("drain: %v %d", err, len(msgs))
	}
}
