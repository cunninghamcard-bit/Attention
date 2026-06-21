package enginefacade

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/backend/local"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

func env(sessionID, runID, kind string, payload any) *protocol.Envelope {
	b, _ := json.Marshal(payload)
	return &protocol.Envelope{
		ID: protocol.NewEventID(), SessionID: sessionID, RunID: runID,
		Kind: kind, Actor: protocol.ActorSystem,
		Payload: b, SchemaVersion: protocol.SchemaVersion,
	}
}

func msgPayload(m ai.Message) map[string]any {
	return map[string]any{"message": m}
}

// goldenRun 把黄金路径信封依序写进 store 并广播：
// run.started → user 消息 → assistant 消息(含 delta) → toolResult 消息 →
// turn.completed → run.completed。
func goldenRun(t *testing.T, store *local.EventStore, bus *local.NotifyBus, sessionID, runID string) {
	t.Helper()
	ctx := context.Background()
	user := ai.Message{Role: ai.RoleUser, Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hi"}}}
	assistant := ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "helloworld"}}, StopReason: ai.StopReasonStop}
	toolResult := ai.Message{Role: ai.RoleToolResult, ToolCallID: "tc_1", ToolName: "bash"}

	seq := []*protocol.Envelope{
		env(sessionID, runID, protocol.KindRunStarted, nil),
		env(sessionID, runID, protocol.KindTurnStarted, nil),
		env(sessionID, runID, protocol.KindMessageStarted, map[string]any{"role": "user"}),
		env(sessionID, runID, protocol.KindMessageCompleted, msgPayload(user)),
		env(sessionID, runID, protocol.KindMessageStarted, map[string]any{"role": "assistant"}),
		env(sessionID, runID, protocol.KindMessageDelta, map[string]any{"text": "hello", "contentIndex": 0}),
		env(sessionID, runID, protocol.KindMessageCompleted, msgPayload(assistant)),
		env(sessionID, runID, protocol.KindToolCallStarted, map[string]any{
			"toolCallId": "tc_1", "toolName": "bash", "args": map[string]any{"cmd": "ls"},
		}),
		env(sessionID, runID, protocol.KindToolCompleted, map[string]any{
			"toolCallId": "tc_1", "toolName": "bash", "result": "ok",
		}),
		env(sessionID, runID, protocol.KindMessageCompleted, msgPayload(toolResult)),
		env(sessionID, runID, protocol.KindTurnCompleted, nil),
		env(sessionID, runID, protocol.KindRunCompleted, nil),
	}
	for _, e := range seq {
		if err := store.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	bus.Publish(sessionID)
}

func TestFoldGoldenPath(t *testing.T) {
	store := local.NewEventStore(t.TempDir())
	bus := local.NewNotifyBus()
	goldenRun(t, store, bus, "ses_a", "run_1")

	batch, err := store.ReadAfter(context.Background(), "ses_a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fld := &fold{}
	var got []compat.Event
	for _, e := range batch {
		got = append(got, fld.apply(e)...)
	}

	wantTypes := []string{
		compat.EventAgentStart, compat.EventTurnStart,
		compat.EventMessageStart, compat.EventMessageEnd, // user
		compat.EventMessageStart, compat.EventMessageUpdate, compat.EventMessageEnd, // assistant
		compat.EventToolExecutionStart, compat.EventToolExecutionEnd,
		compat.EventMessageEnd, // toolResult（goldenRun 略去其 message.started；fold 不依赖配对）
		compat.EventTurnEnd, compat.EventAgentEnd,
	}

	if len(got) != len(wantTypes) {
		t.Fatalf("event count: got %d want %d\n%+v", len(got), len(wantTypes), eventTypes(got))
	}
	for i, w := range wantTypes {
		if got[i].Type != w {
			t.Fatalf("event[%d]: got %s want %s (all: %v)", i, got[i].Type, w, eventTypes(got))
		}
	}

	// turn_end 重建：message + toolResults 来自 message.completed 流
	turnEnd := got[len(got)-2]
	if turnEnd.Message == nil || turnEnd.Message.Role != ai.RoleAssistant {
		t.Fatalf("turn_end.message: %+v", turnEnd.Message)
	}
	if len(turnEnd.ToolResults) != 1 || turnEnd.ToolResults[0].ToolCallID != "tc_1" {
		t.Fatalf("turn_end.toolResults: %+v", turnEnd.ToolResults)
	}

	// agent_end.messages = run 内全部 message.completed（user+assistant+toolResult）
	agentEnd := got[len(got)-1]
	if len(agentEnd.Messages) != 3 {
		t.Fatalf("agent_end.messages: %d", len(agentEnd.Messages))
	}

	// tool_execution_end 自包含
	var toolEnd *compat.Event
	for i := range got {
		if got[i].Type == compat.EventToolExecutionEnd {
			toolEnd = &got[i]
		}
	}
	if toolEnd.ToolCallID != "tc_1" || toolEnd.ToolName != "bash" || toolEnd.Result != "ok" {
		t.Fatalf("tool_execution_end: %+v", toolEnd)
	}

	// partial 重建：message_update 的 partial 从 started{role}+delta{text} 累积
	for i := range got {
		if got[i].Type != compat.EventMessageUpdate {
			continue
		}
		m := got[i].Message
		if m == nil || m.Role != ai.RoleAssistant || len(m.Content) == 0 || m.Content[0].Text != "hello" {
			t.Fatalf("partial reconstruction: %+v", m)
		}
		if got[i].Delta == nil || got[i].Delta.Type != ai.EventTextDelta || got[i].Delta.Delta.Text != "hello" {
			t.Fatalf("delta reconstruction: %+v", got[i].Delta)
		}
	}
}

func eventTypes(evs []compat.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// TestPromptBlocksUntilRunTerminal：facade.Prompt 进队后阻塞至终态信封；
// fake worker 租借 job 并回放黄金路径。
func TestPromptBlocksUntilRunTerminal(t *testing.T) {
	store := local.NewEventStore(t.TempDir())
	bus := local.NewNotifyBus()
	queue := local.NewJobQueue(4)
	f := New(Options{SessionID: "ses_a", Store: store, Bus: bus, Queue: queue})

	go func() { // fake worker
		leased, err := queue.Lease(context.Background())
		job := leased.Job
		if err != nil || job.Kind != backend.JobPrompt || job.RunID == "" {
			t.Errorf("lease: %v %+v", err, leased)
			return
		}
		goldenRun(t, store, bus, "ses_a", job.RunID)
	}()

	preflight := false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := f.Prompt(ctx, compat.PromptInput{
		Text:            "hi",
		PreflightResult: func(ok bool) { preflight = ok },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !preflight {
		t.Fatal("preflight not acked")
	}
	if result.Message.Role != ai.RoleAssistant || len(result.Message.Content) == 0 {
		t.Fatalf("result message: %+v", result.Message)
	}
}

func TestSubscribeDeliversLiveEvents(t *testing.T) {
	store := local.NewEventStore(t.TempDir())
	bus := local.NewNotifyBus()
	f := New(Options{SessionID: "ses_a", Store: store, Bus: bus})

	// 订阅前已有的历史不重放
	goldenRun(t, store, bus, "ses_a", "run_0")
	time.Sleep(20 * time.Millisecond)

	events := make(chan compat.Event, 64)
	cancel := f.Subscribe(func(ev compat.Event) { events <- ev })
	defer cancel()

	goldenRun(t, store, bus, "ses_a", "run_1")

	deadline := time.After(3 * time.Second)
	var got []string
	for len(got) == 0 || got[len(got)-1] != compat.EventAgentEnd {
		select {
		case ev := <-events:
			got = append(got, ev.Type)
		case <-deadline:
			t.Fatalf("timeout; got %v", got)
		}
	}
	if got[0] != compat.EventAgentStart {
		t.Fatalf("first live event: %v", got)
	}
	// 只收到 run_1 的一轮（历史 run_0 未重放）
	count := 0
	for _, k := range got {
		if k == compat.EventAgentStart {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("history replayed: %v", got)
	}
}
