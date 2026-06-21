package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestRunWritesSessionHeaderAndEventLines(t *testing.T) {
	runner := &fakeRunner{}
	events := []orchestrator.Event{
		messageUpdateEvent("he"),
		messageEndEvent("hello"),
	}
	runner.promptFunc = func(_ context.Context, input orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		if input.Text != "prompt" {
			t.Fatalf("prompt input text = %q, want prompt", input.Text)
		}
		if input.Source != "rpc" {
			t.Fatalf("prompt input source = %q, want rpc", input.Source)
		}
		if runner.subscriber == nil {
			t.Fatal("subscriber was not registered before Prompt")
		}
		for _, event := range events {
			runner.subscriber(event)
		}
		return orchestrator.PromptResult{Message: assistantMessage("hello")}, nil
	}

	var out bytes.Buffer
	if err := run(context.Background(), runner, []string{"prompt"}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("stdout = %q, want newline-terminated JSON lines", out.String())
	}
	if !runner.canceled {
		t.Fatal("subscription was not canceled")
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if got, want := len(lines), len(events)+1; got != want {
		t.Fatalf("line count = %d, want %d: %q", got, want, out.String())
	}

	header := unmarshalLine(t, lines[0])
	if got, want := header["type"], any("session"); got != want {
		t.Fatalf("header type = %v, want %v", got, want)
	}

	gotTypes := make([]string, 0, len(events))
	for _, line := range lines[1:] {
		payload := unmarshalLine(t, line)
		eventType := payload["type"].(string)
		gotTypes = append(gotTypes, eventType)

		switch eventType {
		case orchestrator.EventMessageUpdate:
			if _, ok := payload["message"]; !ok {
				t.Fatalf("message_update missing message: %s", line)
			}
			ame, ok := payload["assistantMessageEvent"].(map[string]any)
			if !ok {
				t.Fatalf("message_update missing assistantMessageEvent: %s", line)
			}
			if ame["type"] != "text_delta" {
				t.Fatalf("assistantMessageEvent type = %v, want text_delta: %s", ame["type"], line)
			}
			if _, ok := payload["delta"]; ok {
				t.Fatalf("message_update unexpectedly has legacy delta: %s", line)
			}
		case orchestrator.EventMessageEnd:
			if _, ok := payload["message"]; !ok {
				t.Fatalf("message_end missing message: %s", line)
			}
			if _, ok := payload["delta"]; ok {
				t.Fatalf("message_end unexpectedly has delta: %s", line)
			}
		}
	}

	wantTypes := []string{
		orchestrator.EventMessageUpdate,
		orchestrator.EventMessageEnd,
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
}

func TestEventJSONFromOrchestratorUsesPiNonStreamingShapes(t *testing.T) {
	msg := assistantMessage("hello")
	toolResult := ai.Message{
		Role:       ai.RoleToolResult,
		ToolCallID: "call-1",
		ToolName:   "shell",
		Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
	}

	agentEnd := eventPayload(t, orchestrator.Event{
		Type:     orchestrator.EventAgentEnd,
		Messages: []ai.Message{msg},
	})
	if agentEnd["type"] != orchestrator.EventAgentEnd {
		t.Fatalf("agent_end type = %v", agentEnd["type"])
	}
	messages := agentEnd["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != string(ai.RoleAssistant) {
		t.Fatalf("agent_end messages = %v", messages)
	}
	if _, ok := agentEnd["message"]; ok {
		t.Fatalf("agent_end unexpectedly has message: %v", agentEnd)
	}

	turnEnd := eventPayload(t, orchestrator.Event{
		Type:        orchestrator.EventTurnEnd,
		Message:     &msg,
		ToolResults: []ai.Message{toolResult},
	})
	if turnEnd["type"] != orchestrator.EventTurnEnd {
		t.Fatalf("turn_end type = %v", turnEnd["type"])
	}
	message := turnEnd["message"].(map[string]any)
	if message["role"] != string(ai.RoleAssistant) {
		t.Fatalf("turn_end message = %v", message)
	}
	toolResults := turnEnd["toolResults"].([]any)
	if len(toolResults) != 1 || toolResults[0].(map[string]any)["toolCallId"] != "call-1" {
		t.Fatalf("turn_end toolResults = %v", toolResults)
	}

	toolStart := eventPayload(t, orchestrator.Event{
		Type:       orchestrator.EventToolExecutionStart,
		ToolCallID: "call-1",
		ToolName:   "shell",
		Args:       map[string]any{"command": "pwd"},
	})
	if toolStart["toolCallId"] != "call-1" || toolStart["toolName"] != "shell" {
		t.Fatalf("tool_execution_start ids = %v", toolStart)
	}
	args := toolStart["args"].(map[string]any)
	if args["command"] != "pwd" {
		t.Fatalf("tool_execution_start args = %v", args)
	}

	toolUpdate := eventPayload(t, orchestrator.Event{
		Type:          orchestrator.EventToolExecutionUpdate,
		ToolCallID:    "call-1",
		ToolName:      "shell",
		Args:          map[string]any{"command": "pwd"},
		PartialResult: map[string]any{"stdout": "/t"},
	})
	if toolUpdate["toolCallId"] != "call-1" || toolUpdate["toolName"] != "shell" {
		t.Fatalf("tool_execution_update ids = %v", toolUpdate)
	}
	partialResult := toolUpdate["partialResult"].(map[string]any)
	if partialResult["stdout"] != "/t" {
		t.Fatalf("tool_execution_update partialResult = %v", partialResult)
	}

	toolEnd := eventPayload(t, orchestrator.Event{
		Type:       orchestrator.EventToolExecutionEnd,
		ToolCallID: "call-1",
		ToolName:   "shell",
		Result:     map[string]any{"stdout": "/tmp"},
		IsError:    false,
	})
	if toolEnd["toolCallId"] != "call-1" || toolEnd["toolName"] != "shell" {
		t.Fatalf("tool_execution_end ids = %v", toolEnd)
	}
	if _, ok := toolEnd["isError"]; !ok {
		t.Fatalf("tool_execution_end missing isError: %v", toolEnd)
	}
	if toolEnd["isError"] != false {
		t.Fatalf("tool_execution_end isError = %v, want false", toolEnd["isError"])
	}
	result := toolEnd["result"].(map[string]any)
	if result["stdout"] != "/tmp" {
		t.Fatalf("tool_execution_end result = %v", result)
	}
}

func TestEventJSONFromOrchestratorUsesPiAutoRetryShapes(t *testing.T) {
	start := eventPayload(t, orchestrator.Event{
		Type:         orchestrator.EventAutoRetryStart,
		Attempt:      2,
		MaxAttempts:  3,
		DelayMs:      200,
		ErrorMessage: "503 unavailable",
	})
	if start["type"] != orchestrator.EventAutoRetryStart ||
		start["attempt"] != float64(2) ||
		start["maxAttempts"] != float64(3) ||
		start["delayMs"] != float64(200) ||
		start["errorMessage"] != "503 unavailable" {
		t.Fatalf("auto_retry_start payload = %v", start)
	}

	successEnd := eventPayload(t, orchestrator.Event{
		Type:    orchestrator.EventAutoRetryEnd,
		Success: true,
		Attempt: 2,
	})
	if successEnd["type"] != orchestrator.EventAutoRetryEnd ||
		successEnd["success"] != true ||
		successEnd["attempt"] != float64(2) {
		t.Fatalf("auto_retry_end success payload = %v", successEnd)
	}
	if _, ok := successEnd["finalError"]; ok {
		t.Fatalf("auto_retry_end success unexpectedly has finalError: %v", successEnd)
	}

	failureEnd := eventPayload(t, orchestrator.Event{
		Type:       orchestrator.EventAutoRetryEnd,
		Success:    false,
		Attempt:    3,
		FinalError: "still unavailable",
	})
	if failureEnd["type"] != orchestrator.EventAutoRetryEnd ||
		failureEnd["success"] != false ||
		failureEnd["attempt"] != float64(3) ||
		failureEnd["finalError"] != "still unavailable" {
		t.Fatalf("auto_retry_end failure payload = %v", failureEnd)
	}
}

func TestEventJSONFromOrchestratorUsesPiModeLifecycleShapes(t *testing.T) {
	thinking := eventPayload(t, orchestrator.Event{
		Type:  orchestrator.EventThinkingLevelChanged,
		Level: "high",
	})
	if thinking["type"] != orchestrator.EventThinkingLevelChanged || thinking["level"] != "high" {
		t.Fatalf("thinking_level_changed payload = %v", thinking)
	}

	start := eventPayload(t, orchestrator.Event{
		Type:   orchestrator.EventCompactionStart,
		Reason: "manual",
	})
	if start["type"] != orchestrator.EventCompactionStart || start["reason"] != "manual" {
		t.Fatalf("compaction_start payload = %v", start)
	}

	end := eventPayload(t, orchestrator.Event{
		Type:      orchestrator.EventCompactionEnd,
		Reason:    "manual",
		Result:    orchestrator.CompactResult{Summary: "summary", FirstKeptEntryID: "first-kept", TokensBefore: 42},
		Aborted:   false,
		WillRetry: false,
	})
	if end["type"] != orchestrator.EventCompactionEnd ||
		end["reason"] != "manual" ||
		end["aborted"] != false ||
		end["willRetry"] != false {
		t.Fatalf("compaction_end success payload = %v", end)
	}
	result := end["result"].(map[string]any)
	if result["summary"] != "summary" ||
		result["firstKeptEntryId"] != "first-kept" ||
		result["tokensBefore"] != float64(42) {
		t.Fatalf("compaction_end result = %v", result)
	}

	failureEnd := eventPayload(t, orchestrator.Event{
		Type:         orchestrator.EventCompactionEnd,
		Reason:       "overflow",
		Aborted:      false,
		WillRetry:    false,
		ErrorMessage: "Context overflow recovery failed: boom",
	})
	if failureEnd["errorMessage"] != "Context overflow recovery failed: boom" {
		t.Fatalf("compaction_end failure payload = %v", failureEnd)
	}
	if _, ok := failureEnd["result"]; ok {
		t.Fatalf("compaction_end failure unexpectedly has result: %v", failureEnd)
	}

	emptyQueue := eventPayload(t, orchestrator.Event{
		Type: orchestrator.EventQueueUpdate,
	})
	steering := emptyQueue["steering"].([]any)
	followUp := emptyQueue["followUp"].([]any)
	if len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("empty queue_update payload = %v", emptyQueue)
	}

	queue := eventPayload(t, orchestrator.Event{
		Type:     orchestrator.EventQueueUpdate,
		Steering: []string{"steer"},
		FollowUp: []string{"follow"},
	})
	if queue["steering"].([]any)[0] != "steer" || queue["followUp"].([]any)[0] != "follow" {
		t.Fatalf("queue_update payload = %v", queue)
	}

	savePoint := eventPayload(t, orchestrator.Event{
		Type:                orchestrator.EventSavePoint,
		HadPendingMutations: true,
	})
	if savePoint["type"] != orchestrator.EventSavePoint ||
		savePoint["hadPendingMutations"] != true {
		t.Fatalf("save_point payload = %v", savePoint)
	}

	sessionInfo := eventPayload(t, orchestrator.Event{
		Type: orchestrator.EventSessionInfoChanged,
		Name: "session",
	})
	if sessionInfo["type"] != orchestrator.EventSessionInfoChanged || sessionInfo["name"] != "session" {
		t.Fatalf("session_info_changed payload = %v", sessionInfo)
	}

	settled := eventPayload(t, orchestrator.Event{
		Type:          orchestrator.EventSettled,
		NextTurnCount: 2,
	})
	if settled["type"] != orchestrator.EventSettled ||
		settled["nextTurnCount"] != float64(2) {
		t.Fatalf("settled payload = %v", settled)
	}
}

func TestMapAssistantMessageEvent(t *testing.T) {
	msg := assistantMessage("partial")
	streamEvent := func(et ai.EventType, block *ai.ContentBlock) *ai.StreamEvent {
		return &ai.StreamEvent{Type: et, Index: 2, Delta: block, Message: &msg}
	}
	block := func(t ai.ContentBlockType, text, thinking string) *ai.ContentBlock {
		return &ai.ContentBlock{Type: t, Text: text, Thinking: thinking}
	}

	cases := []struct {
		name      string
		ev        *ai.StreamEvent
		wantType  string // "" means expect skip (ok=false)
		wantField string
		wantValue string
	}{
		{"text_start", streamEvent(ai.EventTextStart, block(ai.ContentText, "", "")), "text_start", "", ""},
		{"thinking_start", streamEvent(ai.EventThinkingStart, block(ai.ContentThinking, "", "")), "thinking_start", "", ""},
		{"toolcall_start", streamEvent(ai.EventToolCallStart, block(ai.ContentToolCall, "", "")), "toolcall_start", "", ""},
		{"text_delta", streamEvent(ai.EventTextDelta, block(ai.ContentText, "hi", "")), "text_delta", "delta", "hi"},
		{"thinking_delta", streamEvent(ai.EventThinkingDelta, block(ai.ContentThinking, "", "hm")), "thinking_delta", "delta", "hm"},
		{"toolcall_delta", streamEvent(ai.EventToolCallDelta, block(ai.ContentToolCall, "", "")), "toolcall_delta", "delta", ""},
		{"nil_skipped", nil, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, ok := mapAssistantMessageEvent(tc.ev)
			if tc.wantType == "" {
				if ok {
					t.Fatalf("expected skip, got %v", value)
				}
				return
			}
			if !ok {
				t.Fatal("expected mapping, got skip")
			}
			payload := marshalToMap(t, value)
			if payload["type"] != tc.wantType {
				t.Fatalf("type = %v, want %v", payload["type"], tc.wantType)
			}
			if payload["contentIndex"] != float64(2) {
				t.Fatalf("contentIndex = %v, want 2", payload["contentIndex"])
			}
			if _, ok := payload["partial"]; !ok {
				t.Fatalf("missing partial: %v", payload)
			}
			if tc.wantField != "" && payload[tc.wantField] != tc.wantValue {
				t.Fatalf("%s = %v, want %v", tc.wantField, payload[tc.wantField], tc.wantValue)
			}
		})
	}
}

func TestMapAssistantMessageEventCompleteIsSkipped(t *testing.T) {
	// pi routes done/error to message_end, not message_update (agent-loop.ts:341);
	// along emits MessageEnd for EventMessageComplete, so it never reaches the
	// assistantMessageEvent mapper.
	for _, reason := range []ai.StopReason{
		ai.StopReasonStop, ai.StopReasonLength, ai.StopReasonToolUse,
		ai.StopReasonAborted, ai.StopReasonError,
	} {
		m := ai.Message{Role: ai.RoleAssistant, StopReason: reason}
		ev := &ai.StreamEvent{Type: ai.EventMessageComplete, Message: &m}
		if _, ok := mapAssistantMessageEvent(ev); ok {
			t.Fatalf("EventMessageComplete (reason %q) should not map to a message_update event", reason)
		}
	}
}

func TestEventJSONSkipsUnmappableMessageUpdate(t *testing.T) {
	// message_update without a mapped stream-event type has no faithful pi event.
	toolCallMsg := assistantMessage("x")
	_, ok := eventJSONFromOrchestrator(orchestrator.Event{
		Type:    orchestrator.EventMessageUpdate,
		Message: &toolCallMsg,
		Delta: &ai.StreamEvent{
			Type:    ai.EventUnknown,
			Delta:   &ai.ContentBlock{Type: ai.ContentToolCall},
			Message: &toolCallMsg,
		},
	})
	if ok {
		t.Fatal("unknown message_update should be skipped")
	}
}

func TestEventJSONMapsResourcesUpdate(t *testing.T) {
	payload := eventPayload(t, orchestrator.Event{
		Type: orchestrator.EventResourcesUpdate,
		Resources: orchestrator.ResourcesSnapshot{
			Skills: []orchestrator.ResourceSummary{
				{Name: "review", Description: "Review changes"},
			},
			PromptTemplates: []orchestrator.ResourceSummary{
				{Name: "deploy", Description: "Deploy app"},
			},
		},
		PreviousResources: orchestrator.ResourcesSnapshot{
			Skills: []orchestrator.ResourceSummary{
				{Name: "old", Description: "Old skill"},
			},
			PromptTemplates: []orchestrator.ResourceSummary{},
		},
	})

	if payload["type"] != orchestrator.EventResourcesUpdate {
		t.Fatalf("type = %v, want %q", payload["type"], orchestrator.EventResourcesUpdate)
	}
	resources := payload["resources"].(map[string]any)
	skills := resources["skills"].([]any)
	firstSkill := skills[0].(map[string]any)
	if firstSkill["name"] != "review" || firstSkill["description"] != "Review changes" {
		t.Fatalf("resources.skills[0] = %#v, want review skill", firstSkill)
	}
	promptTemplates := resources["promptTemplates"].([]any)
	firstPrompt := promptTemplates[0].(map[string]any)
	if firstPrompt["name"] != "deploy" || firstPrompt["description"] != "Deploy app" {
		t.Fatalf("resources.promptTemplates[0] = %#v, want deploy prompt", firstPrompt)
	}
	previous := payload["previousResources"].(map[string]any)
	previousSkills := previous["skills"].([]any)
	firstPreviousSkill := previousSkills[0].(map[string]any)
	if firstPreviousSkill["name"] != "old" {
		t.Fatalf("previousResources.skills[0] = %#v, want old skill", firstPreviousSkill)
	}
}

func marshalToMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return payload
}

func eventPayload(t *testing.T, ev orchestrator.Event) map[string]any {
	t.Helper()

	value, ok := eventJSONFromOrchestrator(ev)
	if !ok {
		t.Fatalf("event %q unexpectedly skipped", ev.Type)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal event %s: %v", data, err)
	}
	return payload
}

func unmarshalLine(t *testing.T, line string) map[string]any {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("unmarshal line %q: %v", line, err)
	}
	return payload
}

type fakeRunner struct {
	subscriber func(orchestrator.Event)
	canceled   bool
	promptFunc func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error)
	metadata   session.Metadata
}

func (f *fakeRunner) SessionMetadata() session.Metadata {
	return f.metadata
}

func (f *fakeRunner) Subscribe(fn func(orchestrator.Event)) func() {
	f.subscriber = fn
	return func() {
		f.canceled = true
	}
}

func (f *fakeRunner) Prompt(
	ctx context.Context,
	input orchestrator.PromptInput,
) (orchestrator.PromptResult, error) {
	return f.promptFunc(ctx, input)
}

func messageUpdateEvent(text string) orchestrator.Event {
	msg := assistantMessage(text)
	return orchestrator.Event{
		Type:    orchestrator.EventMessageUpdate,
		Message: &msg,
		Delta: &ai.StreamEvent{
			Type:    ai.EventTextDelta,
			Index:   0,
			Delta:   &ai.ContentBlock{Type: ai.ContentText, Text: text},
			Message: &msg,
		},
	}
}

func messageEndEvent(text string) orchestrator.Event {
	msg := assistantMessage(text)
	return orchestrator.Event{
		Type:    orchestrator.EventMessageEnd,
		Message: &msg,
	}
}

func assistantMessage(text string) ai.Message {
	return ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
	}
}
