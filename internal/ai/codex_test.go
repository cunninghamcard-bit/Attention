package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamCodexSSEPostsRequestAndAccumulatesMessage(t *testing.T) {
	apiKey := fakeCodexAccessToken("acct_1")
	requestSeen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen = true
		if r.URL.Path != "/codex/responses" {
			t.Fatalf("path = %q, want /codex/responses", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "acct_1" {
			t.Fatalf("chatgpt-account-id = %q", got)
		}
		if got := r.Header.Get("originator"); got != codexOriginator {
			t.Fatalf("originator = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("accept = %q", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != "responses=experimental" {
			t.Fatalf("OpenAI-Beta = %q", got)
		}
		if got := r.Header.Get("session_id"); got != "session-1" {
			t.Fatalf("session_id = %q", got)
		}
		if got := r.Header.Get("x-client-request-id"); got != "session-1" {
			t.Fatalf("x-client-request-id = %q", got)
		}
		if got := r.Header.Get("X-Test"); got != "yes" {
			t.Fatalf("X-Test = %q", got)
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		assertCodexRequestPayload(t, payload)

		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"hel"}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"lo"}`,
			"",
			`data: {"type":"response.output_item.done","item":` +
				`{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hello"}]}}`,
			"",
			`data: {"type":"response.done","response":` +
				`{"id":"resp_1","status":"completed","usage":` +
				`{"input_tokens":7,"output_tokens":3,"total_tokens":10,` +
				`"input_tokens_details":{"cached_tokens":2}}}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n"))
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	temp := 0.2
	opts := &StreamOptions{
		Model:        model.ID,
		APIKey:       apiKey,
		SystemPrompt: "system",
		SessionID:    "session-1",
		Temperature:  temp,
		MaxTokens:    123,
		Headers:      map[string]string{"X-Test": "yes"},
		Tools: []Tool{{
			Name:        "run",
			Description: "Run a command",
			Parameters:  map[string]any{"type": "object"},
		}},
		Messages: []Message{
			{
				Role: RoleUser,
				Content: []ContentBlock{
					{Type: ContentText, Text: "hello"},
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
						ToolCallID: "call:1|item:1",
						ToolName:   "run",
						Arguments:  map[string]any{"cmd": "ls"},
					},
				},
			},
			{
				Role:       RoleToolResult,
				ToolCallID: "call:1|item:1",
				Content: []ContentBlock{
					{Type: ContentText, Text: "ok"},
				},
			},
		},
	}

	got := collectProviderEvents(t, streamCodexSSE(context.Background(), server.Client(), model, opts))
	if !requestSeen {
		t.Fatal("server did not receive request")
	}
	final := got[len(got)-1]
	if final.Type != EventMessageComplete {
		t.Fatalf("final event type = %v, want complete", final.Type)
	}
	if final.Message.ResponseID != "resp_1" {
		t.Fatalf("response id = %q", final.Message.ResponseID)
	}
	if final.Message.StopReason != StopReasonStop {
		t.Fatalf("stop reason = %q", final.Message.StopReason)
	}
	if final.Message.Content[0].Text != "hello" {
		t.Fatalf("text = %q", final.Message.Content[0].Text)
	}
	if final.Message.Usage.Input != 5 || final.Message.Usage.CacheRead != 2 || final.Message.Usage.Output != 3 {
		t.Fatalf("usage = %+v", final.Message.Usage)
	}
}

func TestStreamCodexSSEHTTPErrorYieldsAPIErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"usage limit"}}`, http.StatusTooManyRequests)
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamCodexSSE(context.Background(), server.Client(), model, &StreamOptions{
		Model:      model.ID,
		APIKey:     "test-token",
		MaxRetries: -1,
	}))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	// pi surfaces the friendly usage-limit message bare, with no status
	// wrapper (openai-codex-responses.ts:286-291,1269-1294).
	if !strings.Contains(final.Message.ErrorMessage, "You have hit your ChatGPT usage limit") {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
}

func TestParseCodexErrorMessage(t *testing.T) {
	resets := float64(time.Now().Add(30 * time.Minute).Unix())
	got := parseCodexErrorMessage(429, fmt.Sprintf(
		`{"error":{"code":"usage_limit_reached","plan_type":"Plus","resets_at":%d}}`, int64(resets)))
	if !strings.HasPrefix(got, "You have hit your ChatGPT usage limit (plus plan). Try again in ~") {
		t.Fatalf("usage limit message = %q", got)
	}

	if got := parseCodexErrorMessage(400, `{"error":{"message":"bad request body"}}`); got != "bad request body" {
		t.Fatalf("error.message = %q", got)
	}
	if got := parseCodexErrorMessage(500, "plain text failure"); got != "plain text failure" {
		t.Fatalf("raw body = %q", got)
	}
	if got := parseCodexErrorMessage(503, ""); got != "Service Unavailable" {
		t.Fatalf("empty body = %q", got)
	}
}

func TestStreamCodexSSEResponseFailedUsesNestedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.failed","response":` +
				`{"id":"resp_1","status":"failed",` +
				`"error":{"code":"bad_request","message":"bad request"}}}`,
			"",
		}, "\n"))
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamCodexSSE(context.Background(), server.Client(), model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	if final.Message.ErrorMessage != "bad request" {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
}

func TestStreamCodexSSENetworkErrorIsIteratorError(t *testing.T) {
	want := errors.New("dial failed")
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, want
		}),
	}
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")

	for _, err := range streamCodexSSE(context.Background(), client, model, &StreamOptions{
		Model:      model.ID,
		MaxRetries: -1,
	}) {
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		return
	}
	t.Fatal("stream ended without iterator error")
}

func TestStreamCodexSSERetriesTransientStatus(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("retry-after-ms", "0")
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
			"",
		}, "\n"))
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamCodexSSE(context.Background(), server.Client(), model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}))
	final := got[len(got)-1]

	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if final.Message.StopReason != StopReasonStop {
		t.Fatalf("stop reason = %q, want stop", final.Message.StopReason)
	}
}

func TestStreamCodexSSETimeoutYieldsAbortedMessage(t *testing.T) {
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}),
	}

	got := collectProviderEvents(t, streamCodexSSE(context.Background(), client, model, &StreamOptions{
		Model:   model.ID,
		APIKey:  "test-token",
		Timeout: time.Nanosecond,
	}))
	final := got[len(got)-1]

	if final.Message.StopReason != StopReasonAborted {
		t.Fatalf("stop reason = %q, want aborted", final.Message.StopReason)
	}
}

func TestBuildCodexBaseHeadersAccountIDOverridesCustomHeader(t *testing.T) {
	apiKey := fakeCodexAccessToken("acct_1")
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	headers := buildCodexBaseHeaders(model, &StreamOptions{
		Model:  model.ID,
		APIKey: apiKey,
		Headers: map[string]string{
			"chatgpt-account-id": "wrong",
		},
	})

	if got := headers.Get("chatgpt-account-id"); got != "acct_1" {
		t.Fatalf("chatgpt-account-id = %q, want acct_1", got)
	}
}

func TestConvertCodexToolResultImagesUsePlaceholder(t *testing.T) {
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	input := convertCodexMessages([]Message{{
		Role:       RoleToolResult,
		ToolCallID: "call_1|fc_1",
		Content: []ContentBlock{
			{Type: ContentText, Text: "chart"},
			{Type: ContentImage, MimeType: "image/png", ImageData: "abc"},
		},
	}}, model)

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded[0]["output"]; got != "chart\n(see attached image)" {
		t.Fatalf("output = %q, want text plus image placeholder", got)
	}
}

func assertCodexRequestPayload(t *testing.T, payload map[string]any) {
	t.Helper()

	if payload["model"] != "gpt-5.4-mini" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["instructions"] != "system" {
		t.Fatalf("instructions = %v", payload["instructions"])
	}
	if payload["stream"] != true || payload["store"] != false {
		t.Fatalf("stream/store = %v/%v", payload["stream"], payload["store"])
	}
	if payload["tool_choice"] != "auto" || payload["parallel_tool_calls"] != true {
		t.Fatalf("tool options = %v/%v", payload["tool_choice"], payload["parallel_tool_calls"])
	}
	if payload["prompt_cache_key"] != "session-1" {
		t.Fatalf("prompt_cache_key = %v", payload["prompt_cache_key"])
	}

	input := payload["input"].([]any)
	user := input[0].(map[string]any)
	userContent := user["content"].([]any)[0].(map[string]any)
	if user["role"] != "user" || userContent["type"] != "input_text" || userContent["text"] != "hello" {
		t.Fatalf("user input = %+v", user)
	}

	toolCall := input[1].(map[string]any)
	// The fixture's assistant message comes from provider "openai" while the
	// target is "openai-codex" — foreign history, so pi hashes the item id
	// (openai-responses-shared.ts:104-115).
	if toolCall["type"] != "function_call" ||
		toolCall["call_id"] != "call_1" ||
		toolCall["id"] != "fc_"+shortHash("item:1") ||
		toolCall["name"] != "run" {
		t.Fatalf("tool call input = %+v", toolCall)
	}
	toolOutput := input[2].(map[string]any)
	if toolOutput["type"] != "function_call_output" ||
		toolOutput["call_id"] != "call_1" ||
		toolOutput["output"] != "ok" {
		t.Fatalf("tool output input = %+v", toolOutput)
	}

	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "run" || tool["description"] != "Run a command" {
		t.Fatalf("tool = %+v", tool)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func fakeCodexAccessToken(accountID string) string {
	payload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID + `"}}`
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return fmt.Sprintf("header.%s.signature", encoded)
}
