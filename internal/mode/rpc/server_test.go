package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestServeWritesResponsesWithoutHeader(t *testing.T) {
	target := &fakeTarget{
		snapshot: orchestrator.Snapshot{
			Model:         ai.Model{Provider: "anthropic", ID: "claude-sonnet-4-5"},
			ThinkingLevel: agentloop.ThinkingMedium,
			SessionID:     "sess-1",
			SessionName:   "named-session",
			MessageCount:  3,
		},
	}
	stdin := strings.NewReader(`{"id":"a","type":"get_state"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !target.canceled {
		t.Fatal("subscription was not canceled")
	}

	lines := splitLines(t, out.String())
	// pi's bidirectional rpc mode emits no header line — the first output is
	// a response or event (rpc-mode.ts:377+).
	if got := len(lines); got != 1 {
		t.Fatalf("line count = %d, want 1: %q", got, out.String())
	}

	resp := lines[0]
	if resp["type"] != "response" || resp["command"] != "get_state" || resp["success"] != true {
		t.Fatalf("response = %v", resp)
	}
	if resp["id"] != "a" {
		t.Fatalf("response id = %v, want a", resp["id"])
	}
	data := resp["data"].(map[string]any)
	if data["sessionId"] != "sess-1" ||
		data["sessionName"] != "named-session" ||
		data["thinkingLevel"] != "medium" ||
		data["messageCount"] != float64(3) {
		t.Fatalf("state data = %v", data)
	}
	// pi RpcSessionState shape: full field set + full model object (camelCase id).
	for _, key := range []string{"isStreaming", "isCompacting", "steeringMode", "followUpMode", "autoCompactionEnabled", "pendingMessageCount"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("state missing field %q: %v", key, data)
		}
	}
	model := data["model"].(map[string]any)
	if model["provider"] != "anthropic" || model["id"] != "claude-sonnet-4-5" {
		t.Fatalf("model = %v (want full pi Model with camelCase id)", model)
	}
}

func TestServeEmitsSessionShutdownOnExit(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader("")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", target.shutdownCalls)
	}
	if target.shutdownReason != "quit" {
		t.Fatalf("shutdown reason = %q, want quit", target.shutdownReason)
	}
}

func TestServeGetSessionStatsResponseShape(t *testing.T) {
	target := &fakeTarget{
		sessionStats: orchestrator.SessionStats{
			SessionID:         "sess-1",
			UserMessages:      2,
			AssistantMessages: 3,
			ToolCalls:         4,
			ToolResults:       5,
			TotalMessages:     6,
			Tokens: orchestrator.SessionStatsTokens{
				Input:      7,
				Output:     8,
				CacheRead:  9,
				CacheWrite: 10,
				Total:      34,
			},
			Cost: 1.75,
			ContextUsage: &extension.ContextUsage{
				Tokens:        34,
				ContextWindow: 200,
				Percent:       17,
			},
		},
	}
	stdin := strings.NewReader(`{"id":"stats","type":"get_session_stats"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	resp := splitLines(t, out.String())[0]
	if resp["type"] != "response" || resp["command"] != "get_session_stats" || resp["success"] != true {
		t.Fatalf("response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	for _, key := range []string{
		"sessionId",
		"userMessages",
		"assistantMessages",
		"toolCalls",
		"toolResults",
		"totalMessages",
		"tokens",
		"cost",
		"contextUsage",
	} {
		if _, ok := data[key]; !ok {
			t.Fatalf("session stats missing %q: %v", key, data)
		}
	}
	if _, ok := data["sessionFile"]; ok {
		t.Fatalf("sessionFile present for empty path: %v", data)
	}
	if data["sessionId"] != "sess-1" || data["cost"] != 1.75 {
		t.Fatalf("session stats data = %v", data)
	}
	tokens := data["tokens"].(map[string]any)
	if tokens["input"] != float64(7) ||
		tokens["output"] != float64(8) ||
		tokens["cacheRead"] != float64(9) ||
		tokens["cacheWrite"] != float64(10) ||
		tokens["total"] != float64(34) {
		t.Fatalf("tokens = %v", tokens)
	}
	contextUsage := data["contextUsage"].(map[string]any)
	if contextUsage["tokens"] != float64(34) ||
		contextUsage["contextWindow"] != float64(200) ||
		contextUsage["percent"] != float64(17) {
		t.Fatalf("contextUsage = %v", contextUsage)
	}
}

func TestServeGetSessionStatsOmitsNilContextUsage(t *testing.T) {
	target := &fakeTarget{
		sessionStats: orchestrator.SessionStats{
			SessionID: "sess-1",
		},
	}
	stdin := strings.NewReader(`{"id":"stats","type":"get_session_stats"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	resp := splitLines(t, out.String())[0]
	data := resp["data"].(map[string]any)
	if _, ok := data["contextUsage"]; ok {
		t.Fatalf("contextUsage present: %v", data)
	}
}

func TestServeUnknownCommand(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"x","type":"nope"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false || resp["command"] != "nope" {
		t.Fatalf("response = %v", resp)
	}
	if !strings.Contains(resp["error"].(string), "Unknown command") {
		t.Fatalf("error = %v", resp["error"])
	}
}

func TestServeSetModelUnknownIsError(t *testing.T) {
	target := &fakeTarget{} // resolve returns false
	stdin := strings.NewReader(`{"type":"set_model","modelId":"ghost"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false {
		t.Fatalf("expected failure: %v", resp)
	}
	if target.setModelCalls != 0 {
		t.Fatalf("SetModel called %d times, want 0", target.setModelCalls)
	}
}

func TestServePromptIsAsyncAndStreamsEvents(t *testing.T) {
	target := &fakeTarget{}
	target.promptFunc = func(_ context.Context, input orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		if input.Source != "rpc" {
			t.Errorf("prompt source = %q, want rpc", input.Source)
		}
		if input.PreflightResult != nil {
			input.PreflightResult(true) // emits the success response (pi preflight)
		}
		msg := assistantMessage("hi")
		target.subscriber(orchestrator.Event{Type: orchestrator.EventMessageEnd, Message: &msg})
		return orchestrator.PromptResult{Message: msg}, nil
	}
	stdin := strings.NewReader(`{"id":"p","type":"prompt","message":"hello"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.promptCalls != 1 {
		t.Fatalf("Prompt called %d times, want 1", target.promptCalls)
	}

	lines := splitLines(t, out.String())
	// prompt response + message_end event (order is not guaranteed, prompt
	// runs in the background; no header line, matching pi rpc)
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2: %q", len(lines), out.String())
	}
	var sawResponse, sawEvent bool
	for _, l := range lines {
		switch l["type"] {
		case "response":
			if l["command"] == "prompt" && l["success"] == true {
				sawResponse = true
			}
		case orchestrator.EventMessageEnd:
			sawEvent = true
		}
	}
	if !sawResponse || !sawEvent {
		t.Fatalf("sawResponse=%v sawEvent=%v: %q", sawResponse, sawEvent, out.String())
	}
}

func TestServePromptImagesAndStreamingBehavior(t *testing.T) {
	target := &fakeTarget{}
	target.promptFunc = func(_ context.Context, input orchestrator.PromptInput) (orchestrator.PromptResult, error) {
		if input.PreflightResult != nil {
			input.PreflightResult(true)
		}
		return orchestrator.PromptResult{}, nil
	}
	stdin := strings.NewReader(
		`{"id":"p","type":"prompt","message":"hello","streamingBehavior":"followUp","images":[` +
			`{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}]}` + "\n",
	)

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.promptCalls != 1 {
		t.Fatalf("Prompt called %d times, want 1", target.promptCalls)
	}
	if len(target.promptInputs) != 1 {
		t.Fatalf("prompt inputs len = %d, want 1", len(target.promptInputs))
	}
	input := target.promptInputs[0]
	if input.StreamingBehavior != "followUp" {
		t.Fatalf("streamingBehavior = %q, want followUp", input.StreamingBehavior)
	}
	assertTextImageContent(t, input.Content, "hello", "aGVsbG8=", "image/png")
}

func TestServeSteerAndFollowUpImages(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(
		`{"id":"s","type":"steer","message":"redirect","images":[` +
			`{"type":"image","data":"c3RlZXI=","mimeType":"image/jpeg"}]}` + "\n" +
			`{"id":"f","type":"follow_up","message":"next","images":[` +
			`{"type":"image","data":"Zm9sbG93","mimeType":"image/gif"}]}` + "\n",
	)

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.steerCalls != 1 || len(target.steerInputs) != 1 {
		t.Fatalf("steer calls/inputs = %d/%d, want 1/1", target.steerCalls, len(target.steerInputs))
	}
	if target.followUpCalls != 1 || len(target.followUpInputs) != 1 {
		t.Fatalf(
			"follow-up calls/inputs = %d/%d, want 1/1",
			target.followUpCalls,
			len(target.followUpInputs),
		)
	}
	assertTextImageContent(t, target.steerInputs[0].Content, "redirect", "c3RlZXI=", "image/jpeg")
	assertTextImageContent(t, target.followUpInputs[0].Content, "next", "Zm9sbG93", "image/gif")
}

func TestServeThinkingLevelValidation(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"type":"set_thinking_level","level":"bogus"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false {
		t.Fatalf("expected failure for invalid level: %v", resp)
	}
	if target.thinkingCalls != 0 {
		t.Fatalf("SetThinkingLevel called %d times, want 0", target.thinkingCalls)
	}
}

func TestServeThinkingLevelAcceptsPiLevels(t *testing.T) {
	// pi ThinkingLevel union includes minimal and xhigh (agent types.ts:284).
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh"} {
		target := &fakeTarget{}
		stdin := strings.NewReader(`{"type":"set_thinking_level","level":"` + level + `"}` + "\n")
		var out bytes.Buffer
		if err := serve(context.Background(), target, stdin, &out); err != nil {
			t.Fatalf("serve(%s): %v", level, err)
		}
		resp := splitLines(t, out.String())[0]
		if resp["success"] != true {
			t.Fatalf("level %q rejected: %v", level, resp)
		}
		if target.thinkingCalls != 1 {
			t.Fatalf("level %q: SetThinkingLevel called %d times", level, target.thinkingCalls)
		}
	}
}

func TestServeCycleModelResponseShape(t *testing.T) {
	target := &fakeTarget{
		cycleModelResult: orchestrator.ModelCycleResult{
			Model: ai.Model{
				ID:       "model-a",
				Name:     "Model A",
				Provider: "provider-a",
			},
			ThinkingLevel: agentloop.ThinkingLow,
		},
		cycleModelOK: true,
	}
	stdin := strings.NewReader(`{"id":"m","type":"cycle_model"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "cycle_model" {
		t.Fatalf("cycle_model response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	if data["thinkingLevel"] != "low" || data["isScoped"] != false {
		t.Fatalf("cycle_model data = %v", data)
	}
	model := data["model"].(map[string]any)
	if model["id"] != "model-a" || model["provider"] != "provider-a" || model["name"] != "Model A" {
		t.Fatalf("cycle_model model = %v", model)
	}
}

func TestServeCycleModelNullIncludesDataNull(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"m","type":"cycle_model"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "cycle_model" {
		t.Fatalf("cycle_model response = %v", resp)
	}
	if _, ok := resp["data"]; !ok {
		t.Fatalf("cycle_model null response missing data key: %v", resp)
	}
	if resp["data"] != nil {
		t.Fatalf("cycle_model data = %v, want nil", resp["data"])
	}
}

func TestServeCycleThinkingLevelResponseShape(t *testing.T) {
	target := &fakeTarget{
		cycleThinkingLevel: agentloop.ThinkingHigh,
		cycleThinkingOK:    true,
	}
	stdin := strings.NewReader(`{"id":"t","type":"cycle_thinking_level"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "cycle_thinking_level" {
		t.Fatalf("cycle_thinking_level response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	if data["level"] != "high" {
		t.Fatalf("cycle_thinking_level data = %v, want level high", data)
	}
}

func TestServeCycleThinkingLevelNullIncludesDataNull(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"t","type":"cycle_thinking_level"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "cycle_thinking_level" {
		t.Fatalf("cycle_thinking_level response = %v", resp)
	}
	if _, ok := resp["data"]; !ok {
		t.Fatalf("cycle_thinking_level null response missing data key: %v", resp)
	}
	if resp["data"] != nil {
		t.Fatalf("cycle_thinking_level data = %v, want nil", resp["data"])
	}
}

func TestServeSetSessionNameEmptyFails(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"n","type":"set_session_name","name":"   "}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false || resp["command"] != "set_session_name" {
		t.Fatalf("set_session_name response = %v", resp)
	}
	if resp["error"] != "Session name cannot be empty" {
		t.Fatalf("set_session_name error = %v", resp["error"])
	}
	if target.setSessionNameCalls != 0 {
		t.Fatalf("SetSessionName called %d times, want 0", target.setSessionNameCalls)
	}
}

func TestServeSetSessionNameSuccessOmitsData(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"n","type":"set_session_name","name":"  named  "}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "set_session_name" {
		t.Fatalf("set_session_name response = %v", resp)
	}
	if _, ok := resp["data"]; ok {
		t.Fatalf("set_session_name success included data: %v", resp)
	}
	if target.sessionName != "named" {
		t.Fatalf("SetSessionName name = %q, want named", target.sessionName)
	}
}

func TestServeGetLastAssistantTextResponseShape(t *testing.T) {
	target := &fakeTarget{lastAssistantText: "hello", lastAssistantOK: true}
	stdin := strings.NewReader(`{"id":"a","type":"get_last_assistant_text"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "get_last_assistant_text" {
		t.Fatalf("get_last_assistant_text response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	if data["text"] != "hello" {
		t.Fatalf("get_last_assistant_text data = %v, want text hello", data)
	}
}

func TestServeGetLastAssistantTextNull(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"a","type":"get_last_assistant_text"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	data := resp["data"].(map[string]any)
	if _, ok := data["text"]; !ok {
		t.Fatalf("get_last_assistant_text missing text key: %v", data)
	}
	if data["text"] != nil {
		t.Fatalf("get_last_assistant_text text = %v, want nil", data["text"])
	}
}

func TestServeSessionReplacementResponseShapes(t *testing.T) {
	tests := []struct {
		name          string
		stdin         string
		command       string
		assertTarget  func(*testing.T, *fakeTarget)
		configure     func(*fakeTarget)
		wantCancelled bool
	}{
		{
			name:          "new session",
			stdin:         `{"id":"n","type":"new_session","parentSession":"/old.jsonl"}` + "\n",
			command:       "new_session",
			wantCancelled: true,
			configure: func(target *fakeTarget) {
				target.newSessionCancelled = true
			},
			assertTarget: func(t *testing.T, target *fakeTarget) {
				t.Helper()
				if target.newSessionCalls != 1 || target.parentSession != "/old.jsonl" {
					t.Fatalf(
						"NewSession calls/parent = %d/%q, want 1 /old.jsonl",
						target.newSessionCalls,
						target.parentSession,
					)
				}
			},
		},
		{
			name:          "switch session",
			stdin:         `{"id":"s","type":"switch_session","sessionPath":"/next.jsonl"}` + "\n",
			command:       "switch_session",
			wantCancelled: false,
			assertTarget: func(t *testing.T, target *fakeTarget) {
				t.Helper()
				if target.switchSessionCalls != 1 || target.sessionPath != "/next.jsonl" {
					t.Fatalf(
						"SwitchSession calls/path = %d/%q, want 1 /next.jsonl",
						target.switchSessionCalls,
						target.sessionPath,
					)
				}
			},
		},
		{
			name:          "clone",
			stdin:         `{"id":"c","type":"clone"}` + "\n",
			command:       "clone",
			wantCancelled: true,
			configure: func(target *fakeTarget) {
				target.cloneCancelled = true
			},
			assertTarget: func(t *testing.T, target *fakeTarget) {
				t.Helper()
				if target.cloneCalls != 1 {
					t.Fatalf("Clone calls = %d, want 1", target.cloneCalls)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &fakeTarget{}
			if tt.configure != nil {
				tt.configure(target)
			}

			var out bytes.Buffer
			if err := serve(context.Background(), target, strings.NewReader(tt.stdin), &out); err != nil {
				t.Fatalf("serve: %v", err)
			}
			resp := splitLines(t, out.String())[0]
			if resp["success"] != true || resp["command"] != tt.command {
				t.Fatalf("%s response = %v", tt.command, resp)
			}
			data := resp["data"].(map[string]any)
			if data["cancelled"] != tt.wantCancelled {
				t.Fatalf("%s data = %v, want cancelled %v", tt.command, data, tt.wantCancelled)
			}
			tt.assertTarget(t, target)
		})
	}
}

func TestServeForkResponseShape(t *testing.T) {
	target := &fakeTarget{
		forkText:      "selected user",
		forkCancelled: true,
	}
	stdin := strings.NewReader(`{"id":"f","type":"fork","entryId":"entry-1"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "fork" {
		t.Fatalf("fork response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	if data["text"] != "selected user" || data["cancelled"] != true {
		t.Fatalf("fork data = %v, want text/cancelled", data)
	}
	if target.forkCalls != 1 || target.forkEntryID != "entry-1" {
		t.Fatalf("Fork calls/entry = %d/%q, want 1/entry-1", target.forkCalls, target.forkEntryID)
	}
}

func TestServeGetForkMessagesResponseShape(t *testing.T) {
	target := &fakeTarget{
		forkMessages: []orchestrator.ForkMessage{
			{EntryID: "entry-1", Text: "first"},
			{EntryID: "entry-2", Text: "second"},
		},
	}
	stdin := strings.NewReader(`{"id":"fm","type":"get_fork_messages"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "get_fork_messages" {
		t.Fatalf("get_fork_messages response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	messages := data["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	first := messages[0].(map[string]any)
	if first["entryId"] != "entry-1" || first["text"] != "first" {
		t.Fatalf("first message = %v, want entry-1/first", first)
	}
}

func TestServeGetEntriesResponseShape(t *testing.T) {
	target := &fakeTarget{
		entries: []orchestrator.EntrySummary{
			{EntryID: "entry-1", Label: "you: hello"},
			{EntryID: "entry-2", Label: "assistant: hi there"},
		},
	}
	stdin := strings.NewReader(`{"id":"ge","type":"get_entries"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "get_entries" {
		t.Fatalf("get_entries response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	entries := data["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
	}
	first := entries[0].(map[string]any)
	if first["entryId"] != "entry-1" || first["label"] != "you: hello" {
		t.Fatalf("first entry = %v, want entry-1/you: hello", first)
	}
}

func TestServeGetCommandsResponseShape(t *testing.T) {
	target := &fakeTarget{
		slashCommands: []orchestrator.SlashCommand{
			{
				Name:         "run",
				ArgumentHint: "[args]",
				Source:       "extension",
			},
			{
				Name:        "deploy",
				Description: "Deploy app",
				Source:      "prompt",
				SourceInfo: resource.NewSourceInfo(
					resource.SourceProject,
					"/project/prompts/deploy.md",
					"/project/prompts",
				),
			},
		},
	}
	stdin := strings.NewReader(`{"id":"cmds","type":"get_commands"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "get_commands" {
		t.Fatalf("get_commands response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	commands := data["commands"].([]any)
	if len(commands) != 2 {
		t.Fatalf("commands len = %d, want 2: %v", len(commands), commands)
	}

	extensionCommand := commands[0].(map[string]any)
	if extensionCommand["name"] != "run" || extensionCommand["source"] != "extension" {
		t.Fatalf("extension command = %v", extensionCommand)
	}
	if extensionCommand["argumentHint"] != "[args]" {
		t.Fatalf("extension command argumentHint = %v, want [args]", extensionCommand["argumentHint"])
	}
	extensionSourceInfo := extensionCommand["sourceInfo"].(map[string]any)
	if extensionSourceInfo["kind"] != "" || extensionSourceInfo["path"] != "" {
		t.Fatalf("extension sourceInfo = %v, want zero along source info", extensionSourceInfo)
	}

	promptCommand := commands[1].(map[string]any)
	if promptCommand["name"] != "deploy" ||
		promptCommand["description"] != "Deploy app" ||
		promptCommand["source"] != "prompt" {
		t.Fatalf("prompt command = %v", promptCommand)
	}
	promptSourceInfo := promptCommand["sourceInfo"].(map[string]any)
	if promptSourceInfo["kind"] != "project" ||
		promptSourceInfo["path"] != "/project/prompts/deploy.md" ||
		promptSourceInfo["baseDir"] != "/project/prompts" {
		t.Fatalf("prompt sourceInfo = %v, want camelCase along source info", promptSourceInfo)
	}
}

func TestServeCompactAbortsFirst(t *testing.T) {
	// pi manual compact aborts the running agent before compacting.
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"c","type":"compact"}` + "\n")
	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.abortCalls != 1 || target.waitIdleCalls != 1 || target.compactCalls != 1 {
		t.Fatalf("abort=%d waitIdle=%d compact=%d, want 1/1/1",
			target.abortCalls, target.waitIdleCalls, target.compactCalls)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "compact" {
		t.Fatalf("compact response = %v", resp)
	}
}

func TestServeRetryAndCompactionToggleCommandsOmitData(t *testing.T) {
	target := &fakeTarget{autoCompactionEnabled: true}
	stdin := strings.NewReader(
		`{"id":"c","type":"set_auto_compaction","enabled":false}` + "\n" +
			`{"id":"r","type":"set_auto_retry","enabled":true}` + "\n" +
			`{"id":"a","type":"abort_retry"}` + "\n",
	)

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	lines := splitLines(t, out.String())
	if got := len(lines); got != 3 {
		t.Fatalf("line count = %d, want 3: %q", got, out.String())
	}
	for _, resp := range lines {
		if resp["success"] != true {
			t.Fatalf("response = %v, want success", resp)
		}
		if _, ok := resp["data"]; ok {
			t.Fatalf("response included data: %v", resp)
		}
	}
	if target.setAutoCompactionCalls != 1 || target.autoCompactionEnabled {
		t.Fatalf(
			"SetAutoCompaction calls/enabled = %d/%v, want 1/false",
			target.setAutoCompactionCalls,
			target.autoCompactionEnabled,
		)
	}
	if target.setAutoRetryCalls != 1 || !target.autoRetryEnabled {
		t.Fatalf(
			"SetAutoRetry calls/enabled = %d/%v, want 1/true",
			target.setAutoRetryCalls,
			target.autoRetryEnabled,
		)
	}
	if target.abortRetryCalls != 1 {
		t.Fatalf("AbortRetry calls = %d, want 1", target.abortRetryCalls)
	}
}

func TestServeNavigateTreeResponseShape(t *testing.T) {
	target := &fakeTarget{
		navResult: orchestrator.NavResult{
			Cancelled:  true,
			EditorText: "branch summary",
		},
	}
	stdin := strings.NewReader(
		`{"id":"nt","type":"navigate_tree","entryId":"entry-9","summarize":true,` +
			`"customInstructions":"focus","replaceInstructions":true,"label":"main"}` + "\n",
	)

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.navigateTreeCalls != 1 || target.navTarget != "entry-9" {
		t.Fatalf(
			"NavigateTree calls/target = %d/%q, want 1/entry-9",
			target.navigateTreeCalls,
			target.navTarget,
		)
	}
	wantOpts := orchestrator.NavOptions{
		Summarize:           true,
		CustomInstructions:  "focus",
		ReplaceInstructions: true,
		Label:               "main",
	}
	if target.navOpts != wantOpts {
		t.Fatalf("NavigateTree opts = %#v, want %#v", target.navOpts, wantOpts)
	}

	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "navigate_tree" {
		t.Fatalf("navigate_tree response = %v", resp)
	}
	data := resp["data"].(map[string]any)
	if data["cancelled"] != true || data["editorText"] != "branch summary" {
		t.Fatalf("navigate_tree data = %v", data)
	}
}

func TestServeReloadReturnsSuccessNoData(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"r","type":"reload"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.reloadCalls != 1 {
		t.Fatalf("ReloadSettings calls = %d, want 1", target.reloadCalls)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "reload" {
		t.Fatalf("reload response = %v", resp)
	}
	if _, ok := resp["data"]; ok {
		t.Fatalf("reload response included data: %v", resp)
	}
}

func TestServeReloadSettingsErrorIsFailure(t *testing.T) {
	target := &fakeTarget{reloadErr: errors.New("busy")}
	stdin := strings.NewReader(`{"id":"r","type":"reload"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false || resp["command"] != "reload" {
		t.Fatalf("reload response = %v, want failure", resp)
	}
	if resp["error"] != "busy" {
		t.Fatalf("reload error = %v, want busy", resp["error"])
	}
}

func TestServeDispatchCommandParsesQuotedArgsOnceAndAllowsNoNotification(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(
		`{"id":"d","type":"dispatch_command","name":"run","args":"one \"two three\" 'four five'"}` + "\n",
	)

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.dispatchCommandCalls != 1 || target.dispatchCommandName != "run" {
		t.Fatalf(
			"DispatchCommand calls/name = %d/%q, want 1/run",
			target.dispatchCommandCalls,
			target.dispatchCommandName,
		)
	}
	if !slices.Equal(target.dispatchCommandArgs, []string{"one", "two three", "four five"}) {
		t.Fatalf("DispatchCommand args = %#v", target.dispatchCommandArgs)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != true || resp["command"] != "dispatch_command" {
		t.Fatalf("dispatch_command response = %v", resp)
	}
	if _, ok := resp["data"]; ok {
		t.Fatalf("dispatch_command no-result response included data: %v", resp)
	}
}

func TestServeDispatchCommandHandlerErrorReturnsFailure(t *testing.T) {
	target := &fakeTarget{dispatchCommandErr: errors.New("nope")}
	stdin := strings.NewReader(`{"id":"d","type":"dispatch_command","name":"run"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false || resp["command"] != "dispatch_command" {
		t.Fatalf("dispatch_command response = %v, want failure", resp)
	}
	if resp["error"] != "nope" {
		t.Fatalf("dispatch_command error = %v, want nope", resp["error"])
	}
}

func TestServeDispatchCommandUnknownCommandReturnsFailure(t *testing.T) {
	target := &fakeTarget{dispatchCommandErr: errors.New(`orchestrator: command "missing" not found`)}
	stdin := strings.NewReader(`{"id":"d","type":"dispatch_command","name":"missing"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false || resp["command"] != "dispatch_command" {
		t.Fatalf("dispatch_command response = %v, want failure", resp)
	}
	if !strings.Contains(resp["error"].(string), "not found") {
		t.Fatalf("dispatch_command error = %v, want command not found", resp["error"])
	}
}

func TestServeDispatchCommandReturnsNotifications(t *testing.T) {
	tests := []struct {
		name  string
		level string
		want  string
	}{
		{name: "warning", level: "warning", want: "warning"},
		{name: "empty", level: "", want: "info"},
		{name: "invalid", level: "critical", want: "info"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &fakeTarget{
				dispatchCommandNotifications: []orchestrator.CommandNotification{
					{Message: "finished", Level: tt.level},
				},
			}
			stdin := strings.NewReader(`{"id":"d","type":"dispatch_command","name":"run"}` + "\n")

			var out bytes.Buffer
			if err := serve(context.Background(), target, stdin, &out); err != nil {
				t.Fatalf("serve: %v", err)
			}
			resp := splitLines(t, out.String())[0]
			if resp["success"] != true || resp["command"] != "dispatch_command" {
				t.Fatalf("dispatch_command response = %v", resp)
			}
			data := resp["data"].(map[string]any)
			notifications := data["notifications"].([]any)
			if len(notifications) != 1 {
				t.Fatalf("notifications len = %d, want 1: %v", len(notifications), notifications)
			}
			notification := notifications[0].(map[string]any)
			if notification["message"] != "finished" || notification["level"] != tt.want {
				t.Fatalf("notification = %v, want level %q", notification, tt.want)
			}
		})
	}
}

func TestServeDispatchCommandEmptyNameReturnsFailure(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"id":"d","type":"dispatch_command"}` + "\n")

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp := splitLines(t, out.String())[0]
	if resp["success"] != false || resp["command"] != "dispatch_command" {
		t.Fatalf("dispatch_command response = %v, want failure", resp)
	}
	if !strings.Contains(resp["error"].(string), "not found") {
		t.Fatalf("dispatch_command error = %v, want command not found", resp["error"])
	}
}

func TestServeQueueModeCommandsOmitDataAndCallSetters(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(
		`{"id":"s","type":"set_steering_mode","mode":"all"}` + "\n" +
			`{"id":"f","type":"set_follow_up_mode","mode":"one-at-a-time"}` + "\n",
	)

	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	lines := splitLines(t, out.String())
	if got := len(lines); got != 2 {
		t.Fatalf("line count = %d, want 2: %q", got, out.String())
	}
	for _, resp := range lines {
		if resp["success"] != true {
			t.Fatalf("response = %v, want success", resp)
		}
		if _, ok := resp["data"]; ok {
			t.Fatalf("response included data: %v", resp)
		}
	}
	if target.setSteeringModeCalls != 1 || target.steeringMode != orchestrator.QueueModeAll {
		t.Fatalf(
			"SetSteeringMode calls/mode = %d/%q, want 1/all",
			target.setSteeringModeCalls,
			target.steeringMode,
		)
	}
	if target.setFollowUpModeCalls != 1 || target.followUpMode != orchestrator.QueueModeOneAtATime {
		t.Fatalf(
			"SetFollowUpMode calls/mode = %d/%q, want 1/one-at-a-time",
			target.setFollowUpModeCalls,
			target.followUpMode,
		)
	}
}

func TestServeQueueModeInvalidFailsWithoutSetter(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "steering",
			command: `{"id":"s","type":"set_steering_mode","mode":"bogus"}`,
		},
		{
			name:    "follow-up",
			command: `{"id":"f","type":"set_follow_up_mode","mode":"bogus"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &fakeTarget{}
			stdin := strings.NewReader(tt.command + "\n")

			var out bytes.Buffer
			if err := serve(context.Background(), target, stdin, &out); err != nil {
				t.Fatalf("serve: %v", err)
			}

			resp := splitLines(t, out.String())[0]
			if resp["success"] != false {
				t.Fatalf("response = %v, want failure", resp)
			}
			if resp["error"] != `invalid queue mode "bogus"` {
				t.Fatalf("error = %v, want invalid queue mode", resp["error"])
			}
			if target.setSteeringModeCalls != 0 || target.setFollowUpModeCalls != 0 {
				t.Fatalf(
					"setter calls = steering %d follow-up %d, want 0/0",
					target.setSteeringModeCalls,
					target.setFollowUpModeCalls,
				)
			}
		})
	}
}

func TestServeAbortWaitsForIdle(t *testing.T) {
	target := &fakeTarget{}
	stdin := strings.NewReader(`{"type":"abort"}` + "\n")
	var out bytes.Buffer
	if err := serve(context.Background(), target, stdin, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if target.abortCalls != 1 || target.waitIdleCalls != 1 {
		t.Fatalf("abort=%d waitIdle=%d, want 1/1", target.abortCalls, target.waitIdleCalls)
	}
}

func splitLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	raw := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	lines := make([]map[string]any, 0, len(raw))
	for _, line := range raw {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		lines = append(lines, payload)
	}
	return lines
}

func assertTextImageContent(t *testing.T, content []ai.ContentBlock, text, data, mimeType string) {
	t.Helper()

	if len(content) != 2 {
		t.Fatalf("content len = %d, want text plus image: %#v", len(content), content)
	}
	if content[0].Type != ai.ContentText || content[0].Text != text {
		t.Fatalf("text content = %#v, want %q", content[0], text)
	}
	if content[1].Type != ai.ContentImage || content[1].ImageData != data || content[1].MimeType != mimeType {
		t.Fatalf("image content = %#v, want data %q mime %q", content[1], data, mimeType)
	}
}

type fakeTarget struct {
	mu                           sync.Mutex
	subscriber                   func(orchestrator.Event)
	canceled                     bool
	snapshot                     orchestrator.Snapshot
	promptFunc                   func(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error)
	promptCalls                  int
	promptInputs                 []orchestrator.PromptInput
	steerCalls                   int
	steerInputs                  []orchestrator.UserInput
	followUpCalls                int
	followUpInputs               []orchestrator.UserInput
	setModelCalls                int
	thinkingCalls                int
	cycleModelResult             orchestrator.ModelCycleResult
	cycleModelOK                 bool
	cycleModelCalls              int
	cycleThinkingLevel           agentloop.ThinkingLevel
	cycleThinkingOK              bool
	cycleThinkingCalls           int
	sessionName                  string
	setSessionNameCalls          int
	lastAssistantText            string
	lastAssistantOK              bool
	abortCalls                   int
	abortRetryCalls              int
	navigateTreeCalls            int
	navTarget                    string
	navOpts                      orchestrator.NavOptions
	navResult                    orchestrator.NavResult
	navErr                       error
	reloadCalls                  int
	reloadErr                    error
	newSessionCalls              int
	parentSession                string
	newSessionCancelled          bool
	newSessionErr                error
	switchSessionCalls           int
	sessionPath                  string
	switchCancelled              bool
	switchSessionErr             error
	forkCalls                    int
	forkEntryID                  string
	forkText                     string
	forkCancelled                bool
	forkErr                      error
	cloneCalls                   int
	cloneCancelled               bool
	cloneErr                     error
	forkMessages                 []orchestrator.ForkMessage
	entries                      []orchestrator.EntrySummary
	shutdownCalls                int
	shutdownReason               string
	waitIdleCalls                int
	compactCalls                 int
	sessionStats                 orchestrator.SessionStats
	slashCommands                []orchestrator.SlashCommand
	dispatchCommandCalls         int
	dispatchCommandName          string
	dispatchCommandArgs          []string
	dispatchCommandNotifications []orchestrator.CommandNotification
	dispatchCommandErr           error
	setSteeringModeCalls         int
	steeringMode                 orchestrator.QueueMode
	setFollowUpModeCalls         int
	followUpMode                 orchestrator.QueueMode
	setAutoRetryCalls            int
	autoRetryEnabled             bool
	setAutoCompactionCalls       int
	autoCompactionEnabled        bool
}

func (f *fakeTarget) Subscribe(fn func(orchestrator.Event)) func() {
	f.subscriber = fn
	return func() { f.canceled = true }
}

func (f *fakeTarget) Prompt(ctx context.Context, input orchestrator.PromptInput) (orchestrator.PromptResult, error) {
	f.mu.Lock()
	f.promptCalls++
	f.promptInputs = append(f.promptInputs, input)
	f.mu.Unlock()
	if f.promptFunc != nil {
		return f.promptFunc(ctx, input)
	}
	return orchestrator.PromptResult{}, nil
}

func (f *fakeTarget) Steer(_ context.Context, input orchestrator.UserInput) error {
	f.steerCalls++
	f.steerInputs = append(f.steerInputs, input)
	return nil
}

func (f *fakeTarget) FollowUp(_ context.Context, input orchestrator.UserInput) error {
	f.followUpCalls++
	f.followUpInputs = append(f.followUpInputs, input)
	return nil
}

func (f *fakeTarget) Abort(context.Context) (orchestrator.AbortResult, error) {
	f.abortCalls++
	return orchestrator.AbortResult{}, nil
}

func (f *fakeTarget) AbortRetry() {
	f.abortRetryCalls++
}

func (f *fakeTarget) NavigateTree(
	_ context.Context,
	target session.EntryID,
	opts orchestrator.NavOptions,
) (orchestrator.NavResult, error) {
	f.navigateTreeCalls++
	f.navTarget = string(target)
	f.navOpts = opts
	return f.navResult, f.navErr
}

func (f *fakeTarget) ReloadSettings(context.Context) error {
	f.reloadCalls++
	return f.reloadErr
}

func (f *fakeTarget) NewSession(_ context.Context, parentSession string) (bool, error) {
	f.newSessionCalls++
	f.parentSession = parentSession
	return f.newSessionCancelled, f.newSessionErr
}

func (f *fakeTarget) SwitchSession(_ context.Context, sessionPath string) (bool, error) {
	f.switchSessionCalls++
	f.sessionPath = sessionPath
	return f.switchCancelled, f.switchSessionErr
}

func (f *fakeTarget) Fork(_ context.Context, entryID string) (string, bool, error) {
	f.forkCalls++
	f.forkEntryID = entryID
	return f.forkText, f.forkCancelled, f.forkErr
}

func (f *fakeTarget) Clone(context.Context) (bool, error) {
	f.cloneCalls++
	return f.cloneCancelled, f.cloneErr
}

func (f *fakeTarget) ForkMessages() []orchestrator.ForkMessage {
	return append([]orchestrator.ForkMessage(nil), f.forkMessages...)
}

func (f *fakeTarget) Entries() []orchestrator.EntrySummary {
	return append([]orchestrator.EntrySummary(nil), f.entries...)
}

func (f *fakeTarget) NotifySessionShutdown(_ context.Context, reason string) error {
	f.shutdownCalls++
	f.shutdownReason = reason
	return nil
}

func (f *fakeTarget) SetModel(context.Context, ai.Model) error {
	f.setModelCalls++
	return nil
}

func (f *fakeTarget) CycleModel(context.Context) (orchestrator.ModelCycleResult, bool, error) {
	f.cycleModelCalls++
	return f.cycleModelResult, f.cycleModelOK, nil
}

func (f *fakeTarget) SetThinkingLevel(context.Context, agentloop.ThinkingLevel) error {
	f.thinkingCalls++
	return nil
}

func (f *fakeTarget) CycleThinkingLevel(context.Context) (agentloop.ThinkingLevel, bool, error) {
	f.cycleThinkingCalls++
	return f.cycleThinkingLevel, f.cycleThinkingOK, nil
}

func (f *fakeTarget) Compact(context.Context, orchestrator.CompactOptions) (orchestrator.CompactResult, error) {
	f.compactCalls++
	return orchestrator.CompactResult{}, nil
}

func (f *fakeTarget) SetSteeringMode(mode orchestrator.QueueMode) {
	f.setSteeringModeCalls++
	f.steeringMode = mode
}

func (f *fakeTarget) SetFollowUpMode(mode orchestrator.QueueMode) {
	f.setFollowUpModeCalls++
	f.followUpMode = mode
}

func (f *fakeTarget) SetAutoCompaction(enabled bool) {
	f.setAutoCompactionCalls++
	f.autoCompactionEnabled = enabled
}

func (f *fakeTarget) SetAutoRetry(enabled bool) {
	f.setAutoRetryCalls++
	f.autoRetryEnabled = enabled
}

func (f *fakeTarget) SetSessionName(_ context.Context, name string) error {
	f.setSessionNameCalls++
	f.sessionName = name
	return nil
}

func (f *fakeTarget) LastAssistantText() (string, bool) {
	return f.lastAssistantText, f.lastAssistantOK
}

func (f *fakeTarget) SlashCommands() []orchestrator.SlashCommand {
	return append([]orchestrator.SlashCommand(nil), f.slashCommands...)
}

func (f *fakeTarget) DispatchCommand(
	_ context.Context,
	name string,
	args []string,
) ([]orchestrator.CommandNotification, error) {
	f.dispatchCommandCalls++
	f.dispatchCommandName = name
	f.dispatchCommandArgs = append([]string(nil), args...)
	return append([]orchestrator.CommandNotification(nil), f.dispatchCommandNotifications...), f.dispatchCommandErr
}

func (f *fakeTarget) WaitForIdle(context.Context) error {
	f.waitIdleCalls++
	return nil
}

func (f *fakeTarget) Snapshot() orchestrator.Snapshot { return f.snapshot }
func (f *fakeTarget) SessionStats() orchestrator.SessionStats {
	return f.sessionStats
}
func (f *fakeTarget) Messages() []ai.Message { return nil }

func (f *fakeTarget) ResolveModel(context.Context, string, string) (ai.Model, bool) {
	return ai.Model{}, false
}
func (f *fakeTarget) AvailableModels(context.Context) []ai.Model { return nil }
