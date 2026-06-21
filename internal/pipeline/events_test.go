package pipeline

import (
	"context"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func TestTranslateTable(t *testing.T) {
	cases := []struct {
		in        agentloop.EventType
		want      string
		wantActor protocol.Actor
	}{
		{agentloop.AgentStart, protocol.KindRunStarted, protocol.ActorSystem},
		{agentloop.AgentEnd, protocol.KindRunCompleted, protocol.ActorSystem},
		{agentloop.TurnStart, protocol.KindTurnStarted, protocol.ActorSystem},
		{agentloop.TurnEnd, protocol.KindTurnCompleted, protocol.ActorSystem},
		{agentloop.MessageStart, protocol.KindMessageStarted, protocol.ActorAgent},
		{agentloop.MessageEnd, protocol.KindMessageCompleted, protocol.ActorAgent},
		{agentloop.ToolExecutionStart, protocol.KindToolCallStarted, protocol.ActorAgent},
		{agentloop.ToolExecutionUpdate, protocol.KindToolUpdate, protocol.ActorTool},
		{agentloop.ToolExecutionEnd, protocol.KindToolCompleted, protocol.ActorTool},
	}
	for _, c := range cases {
		kind, actor := translate(agentloop.Event{Type: c.in})
		if kind != c.want || actor != c.wantActor {
			t.Fatalf("%s → got (%s,%s) want (%s,%s)", c.in, kind, actor, c.want, c.wantActor)
		}
	}
	if kind, _ := translate(agentloop.Event{Type: agentloop.EventType("bogus")}); kind != "" {
		t.Fatalf("unknown type should not emit, got %s", kind)
	}
	// 块开合等流装饰不进日志（瘦 payload 裁决）：裸 MessageUpdate 与
	// 非增量 StreamEvent 一律不发。
	if kind, _ := translate(agentloop.Event{Type: agentloop.MessageUpdate}); kind != "" {
		t.Fatalf("bare MessageUpdate should not emit, got %s", kind)
	}
	if kind, _ := translate(agentloop.Event{
		Type:                  agentloop.MessageUpdate,
		AssistantMessageEvent: &ai.StreamEvent{Type: ai.EventTextStart},
	}); kind != "" {
		t.Fatalf("text_start decoration should not emit, got %s", kind)
	}
}

func TestTranslateThinkingDelta(t *testing.T) {
	cases := []struct {
		in   ai.EventType
		want string
	}{
		{ai.EventThinkingStart, protocol.KindThoughtStarted},
		{ai.EventThinkingDelta, protocol.KindThoughtDelta},
		{ai.EventThinkingEnd, protocol.KindThoughtCompleted},
		{ai.EventTextDelta, protocol.KindMessageDelta},
	}
	for _, c := range cases {
		kind, actor := translate(agentloop.Event{
			Type:                  agentloop.MessageUpdate,
			AssistantMessageEvent: &ai.StreamEvent{Type: c.in},
		})
		if kind != c.want || actor != protocol.ActorAgent {
			t.Fatalf("%v → got %s want %s", c.in, kind, c.want)
		}
	}
}

func TestSinkEmitsToolFactsSelfContained(t *testing.T) {
	var gotKind string
	var gotPayload toolPayload
	emit := func(tc *RunContext, kind string, actor protocol.Actor, payload any) error {
		gotKind = kind
		gotPayload = payload.(toolPayload)
		return nil
	}
	sink := NewAgentloopSink(emit)
	sink(context.Background(), &RunContext{SessionID: "ses_a"}, agentloop.Event{
		Type:       agentloop.ToolExecutionEnd,
		ToolCallID: "tc_1",
		ToolName:   "bash",
		Args:       map[string]any{"cmd": "ls"},
		Result:     "ok",
		IsError:    false,
	})
	if gotKind != protocol.KindToolCompleted {
		t.Fatalf("kind: %s", gotKind)
	}
	// e9 自包含：凭载荷即可渲染，零回问。
	if gotPayload.ToolCallID != "tc_1" || gotPayload.ToolName != "bash" ||
		gotPayload.Args == nil || gotPayload.Result != "ok" {
		t.Fatalf("payload not self-contained: %+v", gotPayload)
	}
}

func TestSinkSkipsUnknown(t *testing.T) {
	called := false
	emit := func(tc *RunContext, kind string, actor protocol.Actor, payload any) error {
		called = true
		return nil
	}
	NewAgentloopSink(emit)(context.Background(), &RunContext{}, agentloop.Event{Type: agentloop.EventType("bogus")})
	if called {
		t.Fatal("unknown event must not emit")
	}
}
