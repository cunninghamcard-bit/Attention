package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamAnthropicSDKPostsRequestAndAccumulatesMessage(t *testing.T) {
	onResponseCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("X-Api-Key"); got != "test-token" {
			t.Fatalf("X-Api-Key = %q", got)
		}
		if got := r.Header.Get("X-Test"); got != "yes" {
			t.Fatalf("X-Test = %q", got)
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		assertAnthropicRequestPayload(t, payload)

		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("request-id", "req_1")
		_, _ = io.WriteString(w, strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":` +
				`{"id":"msg_1","usage":{"input_tokens":8,"output_tokens":0,` +
				`"cache_read_input_tokens":3,"cache_creation_input_tokens":1}}}`,
			"",
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`,
			"",
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
			"",
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			"",
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			"",
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			"",
		}, "\n"))
	}))
	defer server.Close()

	model, _ := GetModel("", "claude-sonnet-4-5")
	model.BaseURL = server.URL
	temp := 0.2
	opts := &StreamOptions{
		Model:          model.ID,
		APIKey:         "test-token",
		SystemPrompt:   "system",
		CacheRetention: CacheRetentionLong,
		Temperature:    temp,
		MaxTokens:      99,
		Headers:        map[string]string{"X-Test": "yes"},
		Metadata:       map[string]any{"user_id": "user-1", "trace": "ignored"},
		Tools: []Tool{{
			Name:        "run",
			Description: "Run a command",
			Parameters: map[string]any{
				"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
				"required":   []string{"cmd"},
			},
		}},
		Messages: []Message{
			{
				Role: RoleUser,
				Content: []ContentBlock{
					{Type: ContentText, Text: "hello"},
					{Type: ContentImage, MimeType: "image/png", ImageData: "abc"},
				},
			},
			{
				Role:     RoleAssistant,
				API:      APIOpenAIResponses,
				Provider: "openai",
				Model:    "gpt-5",
				Content: []ContentBlock{
					{
						Type:       ContentToolCall,
						ToolCallID: "call:1",
						ToolName:   "run",
						Arguments:  map[string]any{"cmd": "ls"},
					},
				},
			},
			{
				Role:       RoleToolResult,
				ToolCallID: "call:1",
				Content: []ContentBlock{
					{Type: ContentText, Text: "ok"},
				},
			},
		},
		OnResponse: func(response ProviderResponse, model Model) error {
			onResponseCalled = true
			if response.Status != http.StatusOK {
				t.Fatalf("response status = %d", response.Status)
			}
			if response.Headers["Request-Id"] != "req_1" {
				t.Fatalf("response headers = %+v", response.Headers)
			}
			return nil
		},
	}

	got := collectProviderEvents(t, streamAnthropicSDK(context.Background(), server.Client(), model, opts))
	if !onResponseCalled {
		t.Fatal("OnResponse was not called")
	}
	final := got[len(got)-1]
	if final.Type != EventMessageComplete {
		t.Fatalf("final event type = %v, want complete", final.Type)
	}
	if final.Message.ResponseID != "msg_1" {
		t.Fatalf("response id = %q", final.Message.ResponseID)
	}
	if final.Message.StopReason != StopReasonStop {
		t.Fatalf("stop reason = %q", final.Message.StopReason)
	}
	if final.Message.Content[0].Text != "hello" {
		t.Fatalf("text = %q", final.Message.Content[0].Text)
	}
	if final.Message.Usage.Input != 8 ||
		final.Message.Usage.CacheRead != 3 ||
		final.Message.Usage.CacheWrite != 1 ||
		final.Message.Usage.Output != 4 {
		t.Fatalf("usage = %+v", final.Message.Usage)
	}
}

func TestStreamAnthropicSDKHTTPErrorYieldsAPIErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"usage limit"}}`)
	}))
	defer server.Close()

	model, _ := GetModel("", "claude-sonnet-4-5")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamAnthropicSDK(context.Background(), server.Client(), model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	if !strings.Contains(final.Message.ErrorMessage, "Anthropic API error (429)") {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
}

func TestStreamAnthropicSDKSSEErrorYieldsAPIErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}`,
			"",
			`event: error`,
			`data: {"error":{"type":"overloaded_error","message":"stream failed"}}`,
			"",
			"",
		}, "\n"))
	}))
	defer server.Close()

	model, _ := GetModel("", "claude-sonnet-4-5")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamAnthropicSDK(context.Background(), server.Client(), model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	if final.Message.ErrorMessage != "stream failed" {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
}

func TestStreamAnthropicSDKNetworkErrorIsIteratorError(t *testing.T) {
	want := errors.New("dial failed")
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, want
		}),
	}
	model, _ := GetModel("", "claude-sonnet-4-5")

	for _, err := range streamAnthropicSDK(context.Background(), client, model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}) {
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		return
	}
	t.Fatal("stream ended without iterator error")
}

func TestBuildAnthropicRequestBodyHonorsCompatAndThinking(t *testing.T) {
	model := modelForAnthropicTest(t)
	model.Provider = "fireworks"
	opts := &StreamOptions{
		Model:          model.ID,
		SystemPrompt:   "system",
		CacheRetention: CacheRetentionLong,
		MaxTokens:      2000,
		Reasoning:      "medium",
		ThinkingBudgets: &ThinkingBudgets{
			Medium: 3000,
		},
		Tools: []Tool{{
			Name:        "run",
			Description: "Run a command",
			Parameters:  map[string]any{"type": "object"},
		}},
	}

	body := buildAnthropicRequestBody(model, opts)
	if body.MaxTokens != 5000 {
		t.Fatalf("MaxTokens = %d, want base output plus thinking budget", body.MaxTokens)
	}
	if body.Thinking["type"] != "enabled" ||
		body.Thinking["budget_tokens"] != 3000 ||
		body.Thinking["display"] != "summarized" {
		t.Fatalf("Thinking = %+v, want budget thinking", body.Thinking)
	}
	if body.Temperature != 0 {
		t.Fatalf("Temperature = %v, want omitted while thinking is enabled", body.Temperature)
	}
	if body.System[0].CacheControl.TTL != "" {
		t.Fatalf("system cache ttl = %q, want empty when long retention unsupported", body.System[0].CacheControl.TTL)
	}
	if body.Tools[0].EagerInputStreaming {
		t.Fatal("tool eager_input_streaming = true, want false for fireworks compat")
	}
	if body.Tools[0].CacheControl != nil {
		t.Fatalf("tool cache control = %+v, want nil when unsupported", body.Tools[0].CacheControl)
	}

	betas := anthropicBetaFeatures(model, opts)
	wantBetas := anthropicFineGrainedToolStreamingBeta + "," + anthropicInterleavedThinkingBeta
	if len(betas) != 1 || betas[0] != wantBetas {
		t.Fatalf("betas = %#v, want %q", betas, wantBetas)
	}
}

func TestBuildAnthropicRequestBodyHonorsAdaptiveThinking(t *testing.T) {
	forceAdaptive := true
	xhigh := "xhigh"
	model := modelForAnthropicTest(t)
	model.Compat = &Compat{ForceAdaptiveThinking: &forceAdaptive}
	model.ThinkingLevelMap = map[string]*string{"xhigh": &xhigh}

	body := buildAnthropicRequestBody(model, &StreamOptions{
		Model:     model.ID,
		Reasoning: "xhigh",
	})
	if body.Thinking["type"] != "adaptive" ||
		body.Thinking["display"] != "summarized" {
		t.Fatalf("Thinking = %+v, want adaptive", body.Thinking)
	}
	if body.OutputConfig["effort"] != "xhigh" {
		t.Fatalf("OutputConfig = %+v, want xhigh effort", body.OutputConfig)
	}
	if betas := anthropicBetaFeatures(model, &StreamOptions{Reasoning: "xhigh"}); betas != nil {
		t.Fatalf("betas = %#v, want none for adaptive thinking", betas)
	}
}

func assertAnthropicRequestPayload(t *testing.T, payload map[string]any) {
	t.Helper()

	if payload["model"] != "claude-sonnet-4-5" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["stream"] != true || payload["max_tokens"] != float64(99) || payload["temperature"] != 0.2 {
		t.Fatalf("stream/max_tokens/temperature = %+v", payload)
	}

	system := payload["system"].([]any)[0].(map[string]any)
	if system["type"] != "text" || system["text"] != "system" {
		t.Fatalf("system = %+v", system)
	}
	systemCache := system["cache_control"].(map[string]any)
	if systemCache["type"] != "ephemeral" || systemCache["ttl"] != "1h" {
		t.Fatalf("system cache = %+v", systemCache)
	}

	messages := payload["messages"].([]any)
	user := messages[0].(map[string]any)
	userContent := user["content"].([]any)
	text := userContent[0].(map[string]any)
	image := userContent[1].(map[string]any)
	if user["role"] != "user" || text["type"] != "text" || text["text"] != "hello" {
		t.Fatalf("user text content = %+v", user)
	}
	source := image["source"].(map[string]any)
	if image["type"] != "image" || source["media_type"] != "image/png" || source["data"] != "abc" {
		t.Fatalf("user image content = %+v", image)
	}

	assistant := messages[1].(map[string]any)
	toolUse := assistant["content"].([]any)[0].(map[string]any)
	if assistant["role"] != "assistant" ||
		toolUse["type"] != "tool_use" ||
		toolUse["id"] != "call_1" ||
		toolUse["name"] != "run" {
		t.Fatalf("assistant tool_use = %+v", assistant)
	}

	toolResultMessage := messages[2].(map[string]any)
	toolResult := toolResultMessage["content"].([]any)[0].(map[string]any)
	if toolResultMessage["role"] != "user" ||
		toolResult["type"] != "tool_result" ||
		toolResult["tool_use_id"] != "call_1" {
		t.Fatalf("tool result = %+v", toolResultMessage)
	}
	if _, ok := toolResult["cache_control"]; !ok {
		t.Fatalf("tool result missing cache_control: %+v", toolResult)
	}

	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	inputSchema := tool["input_schema"].(map[string]any)
	if tool["name"] != "run" ||
		tool["description"] != "Run a command" ||
		inputSchema["type"] != "object" {
		t.Fatalf("tool = %+v", tool)
	}
	if tool["eager_input_streaming"] != true {
		t.Fatalf("tool eager_input_streaming = %v, want true", tool["eager_input_streaming"])
	}
	if _, ok := tool["cache_control"]; !ok {
		t.Fatalf("tool missing cache_control: %+v", tool)
	}

	thinking := payload["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %+v, want disabled", thinking)
	}

	metadata := payload["metadata"].(map[string]any)
	if metadata["user_id"] != "user-1" {
		t.Fatalf("metadata = %+v", metadata)
	}
	if _, ok := metadata["trace"]; ok {
		t.Fatalf("metadata should only include user_id: %+v", metadata)
	}
}
