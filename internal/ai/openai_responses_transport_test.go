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

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
)

func TestStreamOpenAIResponsesSDKPostsRequestAndAccumulatesMessage(t *testing.T) {
	onResponseCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q, want /responses", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
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
		assertOpenAIResponsesRequestPayload(t, payload)

		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("x-request-id", "req_1")
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
			`data: {"type":"response.completed","response":` +
				`{"id":"resp_1","status":"completed","usage":` +
				`{"input_tokens":8,"output_tokens":4,"total_tokens":12,` +
				`"input_tokens_details":{"cached_tokens":3}}}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n"))
	}))
	defer server.Close()

	model, _ := GetModel("", "gpt-5")
	model.BaseURL = server.URL
	temp := 0.3
	opts := &StreamOptions{
		Model:          model.ID,
		APIKey:         "test-token",
		SystemPrompt:   "system",
		SessionID:      "session-1",
		CacheRetention: CacheRetentionLong,
		Temperature:    temp,
		MaxTokens:      99,
		Headers:        map[string]string{"X-Test": "yes"},
		Metadata:       map[string]any{"trace": "abc"},
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
					{Type: ContentImage, MimeType: "image/png", ImageData: "abc"},
				},
			},
		},
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

	got := collectProviderEvents(t, streamOpenAIResponsesSDK(context.Background(), server.Client(), model, opts))
	if !onResponseCalled {
		t.Fatal("OnResponse was not called")
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
	if final.Message.Usage.Input != 5 || final.Message.Usage.CacheRead != 3 || final.Message.Usage.Output != 4 {
		t.Fatalf("usage = %+v", final.Message.Usage)
	}
}

func TestStreamOpenAIResponsesSDKHonorsSessionHeaderCompat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("session_id"); got != "" {
			t.Fatalf("session_id = %q, want omitted", got)
		}
		if got := r.Header.Get("x-client-request-id"); got != "session-1" {
			t.Fatalf("x-client-request-id = %q, want session-1", got)
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n"))
	}))
	defer server.Close()

	sendSessionID := false
	model, _ := GetModel("", "gpt-5")
	model.BaseURL = server.URL
	model.Compat = &Compat{SendSessionIdHeader: &sendSessionID}

	got := collectProviderEvents(t, streamOpenAIResponsesSDK(context.Background(), server.Client(), model, &StreamOptions{
		Model:          model.ID,
		APIKey:         "test-token",
		SessionID:      "session-1",
		CacheRetention: CacheRetentionLong,
	}))
	final := got[len(got)-1]
	if final.Message.ResponseID != "resp_1" {
		t.Fatalf("response id = %q, want resp_1", final.Message.ResponseID)
	}
}

func TestStreamOpenAIResponsesSDKHTTPErrorYieldsAPIErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"usage limit","code":"rate_limit"}}`)
	}))
	defer server.Close()

	model, _ := GetModel("", "gpt-5")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamOpenAIResponsesSDK(context.Background(), server.Client(), model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	if !strings.Contains(final.Message.ErrorMessage, "OpenAI API error (429)") {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
}

func TestStreamOpenAIResponsesSDKNetworkErrorIsIteratorError(t *testing.T) {
	want := errors.New("dial failed")
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, want
		}),
	}
	model, _ := GetModel("", "gpt-5")

	for _, err := range streamOpenAIResponsesSDK(context.Background(), client, model, &StreamOptions{Model: model.ID}) {
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		return
	}
	t.Fatal("stream ended without iterator error")
}

func TestStreamOpenAIResponsesSDKSSEErrorYieldsAPIErrorMessage(t *testing.T) {
	model, _ := GetModel("", "gpt-5")
	apiErr := &openai.Error{
		StatusCode: http.StatusTooManyRequests,
		Message:    "usage limit",
	}

	events := streamOpenAIResponsesSDKEvents(fakeOpenAIResponsesStream{err: apiErr})
	got := collectProviderEvents(t, streamOpenAIResponsesEvents(context.Background(), model, events))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	if !strings.Contains(final.Message.ErrorMessage, "OpenAI API error (429): usage limit") {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
}

func TestConvertOpenAIResponsesToolResultImages(t *testing.T) {
	model, _ := GetModel("", "gpt-5")
	input := convertOpenAIResponsesMessages("", []Message{{
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
	output := decoded[0]["output"].([]any)
	text := output[0].(map[string]any)
	image := output[1].(map[string]any)
	if text["type"] != "input_text" || text["text"] != "chart" {
		t.Fatalf("text output = %+v", text)
	}
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,abc" {
		t.Fatalf("image output = %+v", image)
	}

	model.Input = []InputCapability{InputText}
	input = convertOpenAIResponsesMessages("", []Message{{
		Role:       RoleToolResult,
		ToolCallID: "call_1|fc_1",
		Content: []ContentBlock{
			{Type: ContentImage, MimeType: "image/png", ImageData: "abc"},
		},
	}}, model)
	data, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded = nil
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[0]["output"] != "(see attached image)" {
		t.Fatalf("text-only output = %+v", decoded[0]["output"])
	}
}

func TestConvertOpenAIResponsesTextSignatureV1(t *testing.T) {
	input := convertOpenAIResponsesAssistantContent(Message{Role: RoleAssistant, Content: []ContentBlock{{
		Type:          ContentText,
		Text:          "answer",
		TextSignature: `{"v":1,"id":"msg_1","phase":"final_answer"}`,
	}}}, Model{})

	message := input[0].(openAIResponsesOutputMessage)
	if message.ID != "msg_1" || message.Phase != "final_answer" {
		t.Fatalf("assistant message = %+v", message)
	}

	var signature struct {
		Version int    `json:"v"`
		ID      string `json:"id"`
		Phase   string `json:"phase"`
	}
	encoded := encodeOpenAITextSignatureV1("msg_1", "commentary")
	if err := json.Unmarshal([]byte(encoded), &signature); err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if signature.Version != 1 || signature.ID != "msg_1" || signature.Phase != "commentary" {
		t.Fatalf("signature = %+v", signature)
	}

	input = convertOpenAIResponsesAssistantContent(Message{Role: RoleAssistant, Content: []ContentBlock{{
		Type: ContentText,
		Text: "answer",
	}}}, Model{})
	message = input[0].(openAIResponsesOutputMessage)
	if message.ID != "msg_0" {
		t.Fatalf("fallback id = %q, want msg_0", message.ID)
	}

	longID := strings.Repeat("x", 65)
	input = convertOpenAIResponsesAssistantContent(Message{Role: RoleAssistant, Content: []ContentBlock{{
		Type:          ContentText,
		Text:          "answer",
		TextSignature: longID,
	}}}, Model{})
	message = input[0].(openAIResponsesOutputMessage)
	id := message.ID
	if !strings.HasPrefix(id, "msg_") || len(id) > 64 {
		t.Fatalf("clamped id = %q", id)
	}
}

type fakeOpenAIResponsesStream struct {
	err error
}

func (s fakeOpenAIResponsesStream) Next() bool {
	return false
}

func (s fakeOpenAIResponsesStream) Current() responses.ResponseStreamEventUnion {
	return responses.ResponseStreamEventUnion{}
}

func (s fakeOpenAIResponsesStream) Err() error {
	return s.err
}

func assertOpenAIResponsesRequestPayload(t *testing.T, payload map[string]any) {
	t.Helper()

	if payload["model"] != "gpt-5" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["stream"] != true || payload["store"] != false {
		t.Fatalf("stream/store = %v/%v", payload["stream"], payload["store"])
	}
	if payload["prompt_cache_key"] != "session-1" || payload["prompt_cache_retention"] != "24h" {
		t.Fatalf(
			"cache fields = %v/%v",
			payload["prompt_cache_key"],
			payload["prompt_cache_retention"],
		)
	}
	if payload["max_output_tokens"] != float64(99) || payload["temperature"] != 0.3 {
		t.Fatalf("sampling fields = %+v", payload)
	}

	input := payload["input"].([]any)
	system := input[0].(map[string]any)
	if system["role"] != "developer" || system["content"] != "system" {
		t.Fatalf("system input = %+v", system)
	}
	user := input[1].(map[string]any)
	userContent := user["content"].([]any)
	text := userContent[0].(map[string]any)
	image := userContent[1].(map[string]any)
	if user["role"] != "user" || text["type"] != "input_text" || text["text"] != "hello" {
		t.Fatalf("user text input = %+v", user)
	}
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,abc" {
		t.Fatalf("user image input = %+v", image)
	}

	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "run" || tool["strict"] != false {
		t.Fatalf("tool = %+v", tool)
	}

	metadata := payload["metadata"].(map[string]any)
	if metadata["trace"] != "abc" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestDecodeOpenAIResponsesEventDataSkipsEmpty(t *testing.T) {
	for _, data := range []string{"", "   ", "\n"} {
		ev, ok, err := decodeOpenAIResponsesEventData([]byte(data))
		if err != nil || ok || ev.Type != "" {
			t.Fatalf("decode(%q) = (%#v,%v,%v), want (zero,false,nil)", data, ev, ok, err)
		}
	}
}

func TestOpenAIResponsesReasoningSignatureRoundTrip(t *testing.T) {
	rawItem := `{"type":"reasoning","id":"rs_1","encrypted_content":"SECRET","summary":[{"type":"summary_text","text":"thought"}]}`
	item, err := decodeOpenAIResponsesItem([]byte(rawItem))
	if err != nil {
		t.Fatalf("decode item: %v", err)
	}

	block := openAIResponsesBlock{ContentBlock: ContentBlock{Type: ContentThinking}}
	var output ContentBlock
	output.Type = ContentThinking
	finalizeOpenAIResponsesBlock(&block, item, &output)

	// pi stores the FULL reasoning item JSON (openai-responses-shared.ts:445);
	// a bare rs_... id can never be replayed.
	if output.ThinkingSignature != rawItem {
		t.Fatalf("thinking signature = %q, want full item JSON", output.ThinkingSignature)
	}

	input := convertOpenAIResponsesAssistantContent(Message{
		Role:    RoleAssistant,
		Content: []ContentBlock{output},
	}, Model{})
	if len(input) != 1 {
		t.Fatalf("replayed input = %+v, want one reasoning item", input)
	}
	reasoning := input[0].(map[string]any)
	if reasoning["id"] != "rs_1" || reasoning["encrypted_content"] != "SECRET" {
		t.Fatalf("reasoning item = %+v", reasoning)
	}
}

func TestNormalizeOpenAIToolCallIDCrossProvider(t *testing.T) {
	target := Model{Provider: "openai", API: APIOpenAIResponses}

	// Cross-provider ids without an item part stay single-part, so the
	// function_call is emitted WITHOUT an item id (openai-responses-shared.ts:111,196-205).
	source := Message{Role: RoleAssistant, Provider: "anthropic", API: APIAnthropicMessages}
	got := normalizeOpenAIToolCallID("toolu_abc123", target, source)
	if got != "toolu_abc123" {
		t.Fatalf("cross-provider id = %q, want single part", got)
	}
	input := convertOpenAIResponsesAssistantContent(Message{
		Role:     RoleAssistant,
		Provider: "anthropic",
		Content: []ContentBlock{{
			Type:       ContentToolCall,
			ToolCallID: got,
			ToolName:   "run",
			Arguments:  map[string]any{},
		}},
	}, target)
	data, err := json.Marshal(input[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"id"`) {
		t.Fatalf("function_call = %s, want no item id", data)
	}

	// Same provider+api keeps the original item id.
	same := Message{Role: RoleAssistant, Provider: "openai", API: APIOpenAIResponses}
	if got := normalizeOpenAIToolCallID("call_1|fc_9", target, same); got != "call_1|fc_9" {
		t.Fatalf("same-provider id = %q", got)
	}

	// Non-responses target providers never grow a pipe.
	other := Model{Provider: "anthropic", API: APIAnthropicMessages}
	if got := normalizeOpenAIToolCallID("toolu_x", other, same); got != "toolu_x" {
		t.Fatalf("non-responses provider id = %q", got)
	}
}
