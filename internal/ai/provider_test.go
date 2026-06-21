package ai

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
)

func TestAnthropicProviderLocalAccumulation(t *testing.T) {
	model, _ := GetModel("", "claude-sonnet-4-5")
	events := seq([]anthropicStreamEvent{
		{Type: "message_start", MessageID: "msg_1", Usage: &Usage{Input: 10}},
		{Type: "content_block_start", Index: 2, Block: anthropicContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 2, Delta: anthropicDelta{Type: "text_delta", Text: "hel"}},
		{Type: "content_block_delta", Index: 2, Delta: anthropicDelta{Type: "text_delta", Text: "lo"}},
		{Type: "content_block_start", Index: 3, Block: anthropicContentBlock{Type: "tool_use", ID: "call_1", Name: "search"}},
		{Type: "content_block_delta", Index: 3, Delta: anthropicDelta{Type: "input_json_delta", PartialJSON: `{"q":"go"`}},
		{Type: "content_block_delta", Index: 3, Delta: anthropicDelta{Type: "input_json_delta", PartialJSON: `}`}},
		{Type: "content_block_stop", Index: 3},
		{Type: "message_delta", StopReason: "tool_use", Usage: &Usage{Input: 10, Output: 4}},
	})

	got := collectProviderEvents(t, streamAnthropicEvents(context.Background(), model, events))
	final := got[len(got)-1]

	if final.Type != EventMessageComplete {
		t.Fatalf("final event type = %v, want complete", final.Type)
	}
	if final.Message.ResponseID != "msg_1" || final.Message.StopReason != StopReasonToolUse {
		t.Fatalf("final message metadata = %+v", final.Message)
	}
	if final.Message.Content[0].Text != "hello" {
		t.Fatalf("text = %q", final.Message.Content[0].Text)
	}
	if final.Message.Content[1].Arguments["q"] != "go" {
		t.Fatalf("tool args = %+v", final.Message.Content[1].Arguments)
	}
	if got[2].Message != final.Message {
		t.Fatal("anthropic events did not share provider-local output pointer")
	}
	if got[2].Message.Content[0].Text != "hello" {
		t.Fatal("provider-local output was not mutated through to earlier event snapshot")
	}
}

func TestOpenAIResponsesProviderLocalAccumulation(t *testing.T) {
	model, _ := GetModel("", "gpt-5")
	events := seq([]openAIResponsesStreamEvent{
		{Type: "response.created", ResponseID: "resp_1"},
		{Type: "response.in_progress"},
		{Type: "response.output_item.added", Item: openAIResponsesItem{Type: "reasoning", ID: "rs_1"}},
		{Type: "response.reasoning_summary_part.added"},
		{Type: "response.reasoning_summary_text.delta", Delta: "think"},
		{Type: "response.reasoning_summary_part.done"},
		{
			Type: "response.output_item.done",
			Item: openAIResponsesItem{Type: "reasoning", ID: `{"id":"rs_1"}`, Summary: "think"},
		},
		{Type: "response.output_item.added", Item: openAIResponsesItem{Type: "message", ID: "msg_1"}},
		{Type: "response.content_part.added"},
		{Type: "response.output_text.delta", Delta: "hi"},
		{Type: "response.output_text.done"},
		{Type: "response.content_part.done"},
		{Type: "response.output_item.done", Item: openAIResponsesItem{Type: "message", ID: "msg_1", Text: "hi"}},
		{
			Type:       "response.completed",
			ResponseID: "resp_1",
			Status:     "completed",
			Usage:      &Usage{Input: 2, Output: 3, CacheRead: 1},
		},
	})

	got := collectProviderEvents(t, streamOpenAIResponsesEvents(context.Background(), model, events))
	final := got[len(got)-1]

	if final.Message.StopReason != StopReasonStop {
		t.Fatalf("stop reason = %q, want stop", final.Message.StopReason)
	}
	if final.Message.Content[0].Thinking != "think" || final.Message.Content[1].Text != "hi" {
		t.Fatalf("content = %+v", final.Message.Content)
	}
	if final.Message.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %+v", final.Message.Usage)
	}
	if got[2].Message != final.Message {
		t.Fatal("openai responses events did not share provider-local output pointer")
	}
}

func TestOpenAIResponsesToolCallSetsToolUse(t *testing.T) {
	model, _ := GetModel("", "gpt-5")
	events := seq([]openAIResponsesStreamEvent{
		{
			Type: "response.output_item.added",
			Item: openAIResponsesItem{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "run"},
		},
		{Type: "response.function_call_arguments.delta", Delta: `{"cmd":"l`},
		{Type: "response.function_call_arguments.done", Arguments: `{"cmd":"ls"}`},
		{
			Type: "response.output_item.done",
			Item: openAIResponsesItem{
				Type:      "function_call",
				ID:        "fc_1",
				CallID:    "call_1",
				Name:      "run",
				Arguments: `{"cmd":"ls"}`,
			},
		},
		{Type: "response.completed", Status: "completed"},
	})

	got := collectProviderEvents(t, streamOpenAIResponsesEvents(context.Background(), model, events))
	final := got[len(got)-1]

	if final.Message.StopReason != StopReasonToolUse {
		t.Fatalf("stop reason = %q, want toolUse", final.Message.StopReason)
	}
	if final.Message.Content[0].ToolCallID != "call_1|fc_1" || final.Message.Content[0].Arguments["cmd"] != "ls" {
		t.Fatalf("tool call = %+v", final.Message.Content[0])
	}
	if countToolCallDeltas(got) != 2 {
		t.Fatalf("tool call deltas = %d, want 2", countToolCallDeltas(got))
	}
}

func TestOpenAIResponsesIncompleteMapsToLength(t *testing.T) {
	model, _ := GetModel("", "gpt-5")
	events := seq([]openAIResponsesStreamEvent{
		{Type: "response.incomplete"},
	})
	got := collectProviderEvents(t, streamOpenAIResponsesEvents(context.Background(), model, events))
	final := got[len(got)-1]

	if final.Message.StopReason != StopReasonLength {
		t.Fatalf("stop reason = %q, want length", final.Message.StopReason)
	}
}

func TestCodexProviderLocalAccumulationAndErrors(t *testing.T) {
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	events := seq([]codexStreamEvent{
		{Type: "response.queued"},
		{Type: "response.output_item.added", Item: openAIResponsesItem{Type: "message", ID: "msg_1"}},
		{Type: "response.content_part.added"},
		{Type: "response.output_text.delta", Delta: "codex"},
		{Type: "response.output_text.done"},
		{Type: "response.content_part.done"},
		{Type: "response.output_item.done", Item: openAIResponsesItem{Type: "message", ID: "msg_1", Text: "codex"}},
		{Type: "response.done", Status: "completed", Usage: &Usage{Input: 1, Output: 1}},
	})

	got := collectProviderEvents(t, streamCodexEvents(context.Background(), model, events))
	final := got[len(got)-1]

	if final.Message.API != APIOpenAICodexResponses || final.Message.Provider != "openai-codex" {
		t.Fatalf("codex metadata = %+v", final.Message)
	}
	if final.Message.Content[0].Text != "codex" {
		t.Fatalf("text = %q", final.Message.Content[0].Text)
	}
	if got[1].Message != final.Message {
		t.Fatal("codex events did not share provider-local output pointer")
	}

	errEvents := seq([]codexStreamEvent{{Type: "error", ErrorCode: "rate_limit", ErrorMessage: "slow down"}})
	got = collectProviderEvents(t, streamCodexEvents(context.Background(), model, errEvents))
	final = got[len(got)-1]
	if final.Message.StopReason != StopReasonError || !strings.Contains(final.Message.ErrorMessage, "slow down") {
		t.Fatalf("codex error message = %+v", final.Message)
	}
}

func TestCodexToolCallDoneEmitsFinalSuffixDelta(t *testing.T) {
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	events := seq([]codexStreamEvent{
		{
			Type: "response.output_item.added",
			Item: openAIResponsesItem{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "run"},
		},
		{Type: "response.function_call_arguments.delta", Delta: `{"cmd":"l`},
		{Type: "response.function_call_arguments.done", Arguments: `{"cmd":"ls"}`},
		{Type: "response.completed", Status: "completed"},
	})

	got := collectProviderEvents(t, streamCodexEvents(context.Background(), model, events))
	final := got[len(got)-1]

	if final.Message.Content[0].Arguments["cmd"] != "ls" {
		t.Fatalf("tool call = %+v", final.Message.Content[0])
	}
	if countToolCallDeltas(got) != 2 {
		t.Fatalf("tool call deltas = %d, want 2", countToolCallDeltas(got))
	}
}

func TestAnthropicStopReasonErrors(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{name: "refusal", reason: "refusal"},
		{name: "sensitive", reason: "sensitive"},
		{name: "unknown", reason: "policy_block"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := seq([]anthropicStreamEvent{
				{Type: "message_delta", StopReason: tt.reason},
			})
			got := collectProviderEvents(t, streamAnthropicEvents(context.Background(), modelForAnthropicTest(t), events))
			final := got[len(got)-1]
			if final.Message.StopReason != StopReasonError {
				t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
			}
			if !strings.Contains(final.Message.ErrorMessage, tt.reason) {
				t.Fatalf("error message = %q, want stop reason", final.Message.ErrorMessage)
			}
		})
	}
}

func modelForAnthropicTest(t *testing.T) Model {
	t.Helper()
	model, ok := GetModel("", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("missing anthropic model")
	}
	return model
}

func TestCacheRetentionDefaultsToShort(t *testing.T) {
	model, _ := GetModel("", "gpt-5")
	body := buildOpenAIResponsesRequestBody(model, &StreamOptions{
		Model:     model.ID,
		SessionID: "session-1",
	})
	if body.PromptCacheKey != "session-1" {
		t.Fatalf("prompt_cache_key = %q, want session-1", body.PromptCacheKey)
	}

	body = buildOpenAIResponsesRequestBody(model, &StreamOptions{
		Model:          model.ID,
		SessionID:      "session-1",
		CacheRetention: CacheRetentionNone,
	})
	if body.PromptCacheKey != "" || body.PromptCacheRetention != "" {
		t.Fatalf("cache fields = %q/%q, want empty", body.PromptCacheKey, body.PromptCacheRetention)
	}

	anthropicModel, _ := GetModel("", "claude-sonnet-4-5")
	if anthropicCacheControlFor(anthropicModel, "") == nil {
		t.Fatal("anthropic default cache retention disabled cache control")
	}

	codexModel, _ := GetModel("openai-codex", "gpt-5.4-mini")
	codexBody := buildCodexRequestBody(codexModel, &StreamOptions{
		Model:     codexModel.ID,
		SessionID: "session-1",
	})
	if codexBody.PromptCacheKey != "session-1" {
		t.Fatalf("codex prompt_cache_key = %q, want session-1", codexBody.PromptCacheKey)
	}
}

func TestReasoningOptionsAppearInProviderRequestBodies(t *testing.T) {
	responsesModel, _ := GetModel("", "gpt-5")
	responsesBody := buildOpenAIResponsesRequestBody(responsesModel, &StreamOptions{
		Model:     responsesModel.ID,
		Reasoning: "high",
	})
	if responsesBody.Reasoning == nil ||
		responsesBody.Reasoning.Effort != "high" ||
		responsesBody.Reasoning.Summary != "auto" {
		t.Fatalf("openai responses reasoning = %+v, want high/auto", responsesBody.Reasoning)
	}
	if len(responsesBody.Include) != 1 || string(responsesBody.Include[0]) != "reasoning.encrypted_content" {
		t.Fatalf("openai responses include = %+v, want encrypted reasoning", responsesBody.Include)
	}

	codexModel, _ := GetModel("openai-codex", "gpt-5.4-mini")
	codexBody := buildCodexRequestBody(codexModel, &StreamOptions{
		Model:     codexModel.ID,
		Reasoning: "xhigh",
	})
	if codexBody.Reasoning == nil ||
		codexBody.Reasoning.Effort != "xhigh" ||
		codexBody.Reasoning.Summary != "auto" {
		t.Fatalf("codex reasoning = %+v, want xhigh/auto", codexBody.Reasoning)
	}

	completionsModel := Model{
		ID:        "local-chat",
		API:       APIOpenAICompletions,
		Provider:  "openai",
		BaseURL:   "https://api.openai.com/v1",
		Reasoning: true,
	}
	completionsBody := buildOpenAICompletionsRequestBody(completionsModel, &StreamOptions{
		Model:     completionsModel.ID,
		Reasoning: "medium",
	})
	if completionsBody["reasoning_effort"] != "medium" {
		t.Fatalf("openai completions reasoning_effort = %v, want medium", completionsBody["reasoning_effort"])
	}
}

func TestOpenAIResponsesCompatControlsLongCacheRetention(t *testing.T) {
	supportsLong := false
	model, _ := GetModel("", "gpt-5")
	model.Compat = &Compat{SupportsLongCacheRetention: &supportsLong}

	body := buildOpenAIResponsesRequestBody(model, &StreamOptions{
		Model:          model.ID,
		SessionID:      "session-1",
		CacheRetention: CacheRetentionLong,
	})
	if body.PromptCacheKey != "session-1" {
		t.Fatalf("PromptCacheKey = %q, want session-1", body.PromptCacheKey)
	}
	if body.PromptCacheRetention != "" {
		t.Fatalf("PromptCacheRetention = %q, want empty when unsupported", body.PromptCacheRetention)
	}
}

func TestProviderEventIteratorErrorIsTransportError(t *testing.T) {
	model, _ := GetModel("", "claude-sonnet-4-5")
	want := errors.New("connection reset")
	stream := streamAnthropicEvents(context.Background(), model, func(yield func(anthropicStreamEvent, error) bool) {
		yield(anthropicStreamEvent{}, want)
	})

	for _, err := range stream {
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		return
	}
	t.Fatal("stream ended without transport error")
}

func collectProviderEvents(t *testing.T, stream iter.Seq2[*StreamEvent, error]) []*StreamEvent {
	t.Helper()
	var events []*StreamEvent
	for event, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	return events
}

func countToolCallDeltas(events []*StreamEvent) int {
	var count int
	for _, event := range events {
		if event.Type == EventToolCallDelta && event.Delta != nil && event.Delta.Type == ContentToolCall {
			count++
		}
	}
	return count
}

func seq[T any](items []T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for _, item := range items {
			if !yield(item, nil) {
				return
			}
		}
	}
}
