package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestStreamOpenAICompletionsHTTPPostsRequestAndAccumulatesMessage(t *testing.T) {
	onResponseCalled := false
	sendSessionAffinity := true
	model := openAICompletionsTestModel("https://local.example/v1")
	model.Compat = &Compat{SendSessionAffinityHeaders: &sendSessionAffinity}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q, want /v1/chat/completions", req.URL.Path)
		}
		if req.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", req.Method)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := req.Header.Get("session_id"); got != "session-1" {
			t.Fatalf("session_id = %q", got)
		}
		if got := req.Header.Get("x-client-request-id"); got != "session-1" {
			t.Fatalf("x-client-request-id = %q", got)
		}
		if got := req.Header.Get("x-session-affinity"); got != "session-1" {
			t.Fatalf("x-session-affinity = %q", got)
		}
		if got := req.Header.Get("X-Test"); got != "yes" {
			t.Fatalf("X-Test = %q", got)
		}

		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		assertOpenAICompletionsRequestPayload(t, payload)

		body := strings.Join([]string{
			`data: {"id":"chatcmpl_1","model":"local-chat-rev","choices":[{"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
			"",
			`data: {"id":"chatcmpl_1","choices":[{"delta":{"content":"hel"},"finish_reason":null}]}`,
			"",
			`data: {"id":"chatcmpl_1","choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`,
			"",
			`data: {"id":"chatcmpl_1","choices":[{"delta":{"reasoning_content":"more"},"finish_reason":null}]}`,
			"",
			`data: {"id":"chatcmpl_1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"run","arguments":"{\"cmd\":\""}}]},"finish_reason":null}]}`,
			"",
			`data: {"id":"chatcmpl_1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls\"}"}}],"reasoning_details":[{"type":"reasoning.encrypted","id":"call_1","data":"sealed"}]},"finish_reason":null}]}`,
			"",
			`data: {"id":"chatcmpl_1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"",
			`data: {"id":"chatcmpl_1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":1}}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"X-Request-Id": []string{"req_1"},
			},
			Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	opts := &StreamOptions{
		Model:          model.ID,
		APIKey:         "test-token",
		SystemPrompt:   "system",
		SessionID:      "session-1",
		CacheRetention: CacheRetentionLong,
		Temperature:    0.4,
		MaxTokens:      77,
		Headers:        map[string]string{"X-Test": "yes"},
		Tools: []Tool{{
			Name:        "run",
			Description: "Run a command",
			Parameters:  map[string]any{"type": "object"},
		}},
		Messages: openAICompletionsFixtureMessages(),
		OnResponse: func(response ProviderResponse, model Model) error {
			onResponseCalled = true
			if response.Status != http.StatusOK {
				t.Fatalf("response status = %d", response.Status)
			}
			if response.Headers["X-Request-Id"] != "req_1" {
				t.Fatalf("response headers = %+v", response.Headers)
			}
			return nil
		},
	}

	got := collectProviderEvents(t, streamOpenAICompletionsHTTP(context.Background(), client, model, opts))
	if !onResponseCalled {
		t.Fatal("OnResponse was not called")
	}
	final := got[len(got)-1]
	if final.Type != EventMessageComplete {
		t.Fatalf("final event type = %v, want complete", final.Type)
	}
	if final.Message.ResponseID != "chatcmpl_1" || final.Message.ResponseModel != "local-chat-rev" {
		t.Fatalf("response metadata = %+v", final.Message)
	}
	if final.Message.StopReason != StopReasonToolUse {
		t.Fatalf("stop reason = %q, want toolUse", final.Message.StopReason)
	}
	if final.Message.Content[0].Thinking != "think more" {
		t.Fatalf("thinking = %+v", final.Message.Content)
	}
	if final.Message.Content[1].Text != "hello" {
		t.Fatalf("text = %+v", final.Message.Content)
	}
	toolCall := final.Message.Content[2]
	if toolCall.ToolCallID != "call_1" || toolCall.ToolName != "run" || toolCall.Arguments["cmd"] != "ls" {
		t.Fatalf("tool call = %+v", toolCall)
	}
	if !strings.Contains(toolCall.ThoughtSignature, `"reasoning.encrypted"`) {
		t.Fatalf("thought signature = %q", toolCall.ThoughtSignature)
	}
	if final.Message.Usage.Input != 6 ||
		final.Message.Usage.CacheRead != 3 ||
		final.Message.Usage.CacheWrite != 1 ||
		final.Message.Usage.Output != 4 ||
		final.Message.Usage.TotalTokens != 14 {
		t.Fatalf("usage = %+v", final.Message.Usage)
	}
	if got[0].Message != final.Message {
		t.Fatal("openai completions events did not share provider-local output pointer")
	}
}

func TestStreamOpenAICompletionsHTTPErrorYieldsCleanAPIErrorMessage(t *testing.T) {
	model := openAICompletionsTestModel("https://local.example/v1")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid request"}}`)),
		}, nil
	})}
	got := collectProviderEvents(t, streamOpenAICompletionsHTTP(context.Background(), client, model, &StreamOptions{
		Model:        model.ID,
		APIKey:       "secret-token",
		SystemPrompt: "secret prompt",
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: ContentText, Text: "secret user text"}},
		}},
	}))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	if !strings.Contains(final.Message.ErrorMessage, "OpenAI Completions API error (400): invalid request") {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
	if strings.Contains(final.Message.ErrorMessage, "secret-token") ||
		strings.Contains(final.Message.ErrorMessage, "secret prompt") ||
		strings.Contains(final.Message.ErrorMessage, "secret user text") {
		t.Fatalf("error leaked request details: %q", final.Message.ErrorMessage)
	}
}

func TestStreamOpenAICompletionsHTTPNetworkErrorIsIteratorError(t *testing.T) {
	want := errors.New("dial failed")
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, want
		}),
	}
	model := openAICompletionsTestModel("https://local.example/v1")

	for _, err := range streamOpenAICompletionsHTTP(context.Background(), client, model, &StreamOptions{
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

func TestBuildOpenAICompletionsRequestBodyHonorsCompat(t *testing.T) {
	falseValue := false
	trueValue := true
	maxTokensField := "max_tokens"
	thinkingFormat := "zai"
	cacheControlFormat := "anthropic"
	model := openAICompletionsTestModel("https://api.together.ai/v1")
	model.Provider = "together"
	model.Compat = &Compat{
		SupportsStore:            &falseValue,
		SupportsDeveloperRole:    &falseValue,
		SupportsUsageInStreaming: &falseValue,
		SupportsStrictMode:       &falseValue,
		ZaiToolStream:            &trueValue,
		MaxTokensField:           &maxTokensField,
		ThinkingFormat:           &thinkingFormat,
		CacheControlFormat:       &cacheControlFormat,
	}

	body := buildOpenAICompletionsRequestBody(model, &StreamOptions{
		Model:          model.ID,
		SystemPrompt:   "system",
		CacheRetention: CacheRetentionLong,
		SessionID:      "session-1",
		MaxTokens:      22,
		Tools: []Tool{{
			Name:        "run",
			Description: "Run a command",
			Parameters:  map[string]any{"type": "object"},
		}},
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: ContentText, Text: "hello"}},
		}},
	})

	if _, ok := body["stream_options"]; ok {
		t.Fatalf("stream_options present: %+v", body["stream_options"])
	}
	if _, ok := body["store"]; ok {
		t.Fatalf("store present: %+v", body["store"])
	}
	if body["max_tokens"] != 22 {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("max_completion_tokens present: %+v", body["max_completion_tokens"])
	}
	if body["tool_stream"] != true || body["enable_thinking"] != false {
		t.Fatalf("zai compat fields = %+v", body)
	}

	messages := body["messages"].([]openAICompletionsMessage)
	if messages[0]["role"] != "system" {
		t.Fatalf("system role = %v", messages[0]["role"])
	}
	systemContent := messages[0]["content"].([]openAICompletionsContentPart)
	if systemContent[0]["cache_control"] == nil {
		t.Fatalf("system cache control missing: %+v", systemContent)
	}
	userContent := messages[1]["content"].([]openAICompletionsContentPart)
	if userContent[0]["cache_control"] == nil {
		t.Fatalf("user cache control missing: %+v", userContent)
	}
	tools := body["tools"].([]openAICompletionsTool)
	function := tools[0]["function"].(map[string]any)
	if _, ok := function["strict"]; ok {
		t.Fatalf("strict present: %+v", function)
	}
	if tools[0]["cache_control"] == nil {
		t.Fatalf("tool cache control missing: %+v", tools[0])
	}
}

func assertOpenAICompletionsRequestPayload(t *testing.T, payload map[string]any) {
	t.Helper()

	if payload["model"] != "local-chat" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["stream"] != true || payload["store"] != false {
		t.Fatalf("stream/store = %v/%v", payload["stream"], payload["store"])
	}
	streamOptions := payload["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %+v", streamOptions)
	}
	if payload["max_completion_tokens"] != float64(77) || payload["temperature"] != 0.4 {
		t.Fatalf("sampling fields = %+v", payload)
	}
	if payload["prompt_cache_key"] != "session-1" || payload["prompt_cache_retention"] != "24h" {
		t.Fatalf("cache fields = %v/%v", payload["prompt_cache_key"], payload["prompt_cache_retention"])
	}

	messages := payload["messages"].([]any)
	system := messages[0].(map[string]any)
	if system["role"] != "developer" || system["content"] != "system" {
		t.Fatalf("system message = %+v", system)
	}
	user := messages[1].(map[string]any)
	userContent := user["content"].([]any)
	text := userContent[0].(map[string]any)
	image := userContent[1].(map[string]any)
	if user["role"] != "user" || text["type"] != "text" || text["text"] != "hello" {
		t.Fatalf("user text = %+v", user)
	}
	imageURL := image["image_url"].(map[string]any)
	if image["type"] != "image_url" || imageURL["url"] != "data:image/png;base64,abc" {
		t.Fatalf("user image = %+v", image)
	}

	assistant := messages[2].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "prior answer" {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant["reasoning_content"] != "prior thinking" {
		t.Fatalf("assistant reasoning = %+v", assistant)
	}
	toolCall := assistant["tool_calls"].([]any)[0].(map[string]any)
	if toolCall["id"] != "call_1" || toolCall["type"] != "function" {
		t.Fatalf("tool call = %+v", toolCall)
	}
	toolFunction := toolCall["function"].(map[string]any)
	if toolFunction["name"] != "run" || toolFunction["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("tool function = %+v", toolFunction)
	}
	toolResult := messages[3].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_1" || toolResult["content"] != "ok" {
		t.Fatalf("tool result = %+v", toolResult)
	}
	toolImageMessage := messages[4].(map[string]any)
	if toolImageMessage["role"] != "user" {
		t.Fatalf("tool image message = %+v", toolImageMessage)
	}

	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	function := tool["function"].(map[string]any)
	if tool["type"] != "function" ||
		function["name"] != "run" ||
		function["description"] != "Run a command" ||
		function["strict"] != false {
		t.Fatalf("tool = %+v", tool)
	}
}

func openAICompletionsFixtureMessages() []Message {
	return []Message{
		{
			Role: RoleUser,
			Content: []ContentBlock{
				{Type: ContentText, Text: "hello"},
				{Type: ContentImage, MimeType: "image/png", ImageData: "abc"},
			},
		},
		{
			Role:     RoleAssistant,
			API:      APIOpenAICompletions,
			Provider: "openai",
			Model:    "local-chat",
			Content: []ContentBlock{
				{Type: ContentThinking, Thinking: "prior thinking", ThinkingSignature: "reasoning_content"},
				{Type: ContentText, Text: "prior answer"},
				{
					Type:       ContentToolCall,
					ToolCallID: "call_1",
					ToolName:   "run",
					Arguments:  map[string]any{"cmd": "ls"},
				},
			},
		},
		{
			Role:       RoleToolResult,
			ToolCallID: "call_1",
			ToolName:   "run",
			Content: []ContentBlock{
				{Type: ContentText, Text: "ok"},
				{Type: ContentImage, MimeType: "image/png", ImageData: "toolabc"},
			},
		},
	}
}

func openAICompletionsTestModel(baseURL string) Model {
	return Model{
		ID:            "local-chat",
		Name:          "Local Chat",
		API:           APIOpenAICompletions,
		Provider:      "openai",
		BaseURL:       baseURL,
		Reasoning:     true,
		Input:         []InputCapability{InputText, InputImage},
		ContextWindow: 128_000,
		MaxTokens:     4_096,
		Cost: Cost{
			Input:      1,
			Output:     2,
			CacheRead:  0.5,
			CacheWrite: 1.5,
		},
	}
}
