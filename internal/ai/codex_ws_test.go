package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"fmt"
	"github.com/coder/websocket"
	"sync"
)

func TestStreamCodexWebSocketSendsCreateAndAccumulatesMessage(t *testing.T) {
	apiKey := fakeCodexAccessToken("acct_1")
	requestSeen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen = true
		if r.URL.Path != "/codex/responses" {
			t.Fatalf("path = %q, want /codex/responses", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Fatalf("method = %q, want GET", r.Method)
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
		if got := r.Header.Get("OpenAI-Beta"); got != codexWebSocketBeta {
			t.Fatalf("OpenAI-Beta = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "" {
			t.Fatalf("accept = %q, want empty", got)
		}
		if got := r.Header.Get("Content-Type"); got != "" {
			t.Fatalf("content-type = %q, want empty", got)
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

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("accept websocket: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		payload := readCodexWebSocketRequest(t, conn)
		if payload["type"] != "response.create" {
			t.Fatalf("type = %v, want response.create", payload["type"])
		}
		assertCodexRequestPayload(t, payload)

		writeCodexWebSocketFrames(t, conn, []string{
			`{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`,
			`{"type":"response.output_text.delta","delta":"hel"}`,
			`{"type":"response.output_text.delta","delta":"lo"}`,
			`{"type":"response.output_item.done","item":` +
				`{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hello"}]}}`,
			`{"type":"response.completed","response":` +
				`{"id":"resp_1","status":"completed","usage":` +
				`{"input_tokens":7,"output_tokens":3,"total_tokens":10,` +
				`"input_tokens_details":{"cached_tokens":2}}}}`,
		})
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
		Headers: map[string]string{
			"Accept":       "text/event-stream",
			"Content-Type": "application/json",
			"OpenAI-Beta":  "responses=experimental",
			"X-Test":       "yes",
		},
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

	got := collectProviderEvents(t, streamCodexWebSocket(context.Background(), server.Client(), model, opts))
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

func TestStreamCodexWebSocketAPIErrorFrameYieldsErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("accept websocket: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_ = readCodexWebSocketRequest(t, conn)

		writeCodexWebSocketFrames(t, conn, []string{
			`{"type":"error","error":{"code":"rate_limit","message":"slow down"}}`,
		})
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamCodexWebSocket(context.Background(), server.Client(), model, &StreamOptions{
		Model:      model.ID,
		APIKey:     "test-token",
		MaxRetries: -1,
	}))
	final := got[len(got)-1]
	if final.Message.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", final.Message.StopReason)
	}
	if final.Message.ErrorMessage != "slow down" {
		t.Fatalf("error message = %q", final.Message.ErrorMessage)
	}
}

func TestStreamCodexWebSocketStopsAfterTerminalFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("accept websocket: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_ = readCodexWebSocketRequest(t, conn)

		if err := conn.Write(
			context.Background(),
			websocket.MessageText,
			[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`),
		); err != nil {
			t.Fatalf("write terminal frame: %v", err)
		}
		_ = conn.Write(
			context.Background(),
			websocket.MessageText,
			[]byte(`{"type":"definitely.unknown"}`),
		)
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamCodexWebSocket(context.Background(), server.Client(), model, &StreamOptions{
		Model:      model.ID,
		APIKey:     "test-token",
		MaxRetries: -1,
	}))
	final := got[len(got)-1]
	if final.Message.ResponseID != "resp_1" || final.Message.StopReason != StopReasonStop {
		t.Fatalf("final message = %+v", final.Message)
	}
}

func TestStreamCodexWebSocketCloseBeforeTerminalIsIteratorError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("accept websocket: %v", err)
		}
		_ = readCodexWebSocketRequest(t, conn)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	for _, err := range streamCodexWebSocket(context.Background(), server.Client(), model, &StreamOptions{
		Model:  model.ID,
		APIKey: "test-token",
	}) {
		if err == nil || !strings.Contains(err.Error(), "closed before response completed") {
			t.Fatalf("err = %v, want close-before-completion", err)
		}
		return
	}
	t.Fatal("stream ended without iterator error")
}

func TestStreamCodexAutoFallsBackToSSEBeforeWebSocketEvents(t *testing.T) {
	var wsRequests atomic.Int32
	var sseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			wsRequests.Add(1)
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				t.Fatalf("accept websocket: %v", err)
			}
			_ = readCodexWebSocketRequest(t, conn)
			_ = conn.Close(websocket.StatusNormalClosure, "")
		case http.MethodPost:
			sseRequests.Add(1)
			w.Header().Set("content-type", "text/event-stream")
			_, _ = w.Write([]byte(strings.Join([]string{
				`data: {"type":"response.completed","response":{"id":"resp_sse","status":"completed"}}`,
				"",
			}, "\n")))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	got := collectProviderEvents(t, streamCodexAuto(context.Background(), server.Client(), model, &StreamOptions{
		Model:     model.ID,
		APIKey:    "test-token",
		Transport: TransportAuto,
	}))
	final := got[len(got)-1]

	if wsRequests.Load() != 1 || sseRequests.Load() != 1 {
		t.Fatalf("ws/sse requests = %d/%d, want 1/1", wsRequests.Load(), sseRequests.Load())
	}
	if final.Message.ResponseID != "resp_sse" || final.Message.StopReason != StopReasonStop {
		t.Fatalf("final message = %+v", final.Message)
	}
}

func TestStreamCodexAutoDoesNotRetryWebSocketBeforeFallback(t *testing.T) {
	var wsRequests atomic.Int32
	var sseRequests atomic.Int32
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Method {
			case http.MethodGet:
				wsRequests.Add(1)
				return nil, errors.New("dial failed")
			case http.MethodPost:
				sseRequests.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(strings.Join([]string{
						`data: {"type":"response.completed","response":{"id":"resp_sse","status":"completed"}}`,
						"",
					}, "\n"))),
					Request: req,
				}, nil
			default:
				t.Fatalf("unexpected method %s", req.Method)
				return nil, nil
			}
		}),
	}
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")

	got := collectProviderEvents(t, streamCodexAuto(context.Background(), client, model, &StreamOptions{
		Model:     model.ID,
		APIKey:    "test-token",
		Transport: TransportAuto,
	}))
	final := got[len(got)-1]
	if wsRequests.Load() != 1 || sseRequests.Load() != 1 {
		t.Fatalf("ws/sse requests = %d/%d, want 1/1", wsRequests.Load(), sseRequests.Load())
	}
	if final.Message.ResponseID != "resp_sse" || final.Message.StopReason != StopReasonStop {
		t.Fatalf("final message = %+v", final.Message)
	}
}

func TestBuildCodexWebSocketHeadersAlwaysSetsRequestID(t *testing.T) {
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	headers := buildCodexWebSocketHeaders(model, &StreamOptions{Model: model.ID})
	sessionID := headers.Get("session_id")
	requestID := headers.Get("x-client-request-id")

	if sessionID == "" || requestID == "" {
		t.Fatalf("session/request ids = %q/%q, want non-empty", sessionID, requestID)
	}
	if sessionID != requestID {
		t.Fatalf("session/request ids = %q/%q, want equal", sessionID, requestID)
	}
	if !strings.HasPrefix(sessionID, "codex_") {
		t.Fatalf("session id = %q, want generated codex id", sessionID)
	}
}

func TestStreamCodexWebSocketHandshakeErrorIsTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "usage limit", http.StatusTooManyRequests)
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	var streamErr error
	for _, err := range streamCodexWebSocket(context.Background(), server.Client(), model, &StreamOptions{
		Model:      model.ID,
		APIKey:     "test-token",
		MaxRetries: -1,
	}) {
		if err != nil {
			streamErr = err
			break
		}
		t.Fatal("handshake rejection must not yield events; pi treats it as a transport failure")
	}
	// pi catches pre-stream websocket failures and falls back to SSE
	// (openai-codex-responses.ts:208-228), so the handshake rejection has to
	// surface as a transport error, not a terminal API error event.
	if streamErr == nil || !strings.Contains(streamErr.Error(), "handshake rejected (429)") {
		t.Fatalf("stream error = %v, want handshake rejection transport error", streamErr)
	}
}

func TestStreamCodexAutoFallsBackToSSEOnHandshakeRejection(t *testing.T) {
	var wsRequests atomic.Int32
	var sseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			wsRequests.Add(1)
			http.Error(w, "websocket not enabled", http.StatusBadRequest)
		case http.MethodPost:
			sseRequests.Add(1)
			w.Header().Set("content-type", "text/event-stream")
			_, _ = w.Write([]byte(strings.Join([]string{
				`data: {"type":"response.completed","response":{"id":"resp_sse","status":"completed"}}`,
				"",
			}, "\n")))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	opts := &StreamOptions{
		Model:     model.ID,
		APIKey:    "test-token",
		Transport: TransportAuto,
		SessionID: "handshake-fallback-session",
	}
	got := collectProviderEvents(t, streamCodexAuto(context.Background(), server.Client(), model, opts))
	final := got[len(got)-1]

	if wsRequests.Load() != 1 || sseRequests.Load() != 1 {
		t.Fatalf("ws/sse requests = %d/%d, want 1/1", wsRequests.Load(), sseRequests.Load())
	}
	if final.Message.ResponseID != "resp_sse" || final.Message.StopReason != StopReasonStop {
		t.Fatalf("final message = %+v", final.Message)
	}

	// pi remembers the failed session and skips the websocket attempt on
	// subsequent requests (openai-codex-responses.ts:177-180).
	got = collectProviderEvents(t, streamCodexAuto(context.Background(), server.Client(), model, opts))
	final = got[len(got)-1]
	if wsRequests.Load() != 1 || sseRequests.Load() != 2 {
		t.Fatalf("ws/sse requests after fallback = %d/%d, want 1/2", wsRequests.Load(), sseRequests.Load())
	}
	if final.Message.ResponseID != "resp_sse" {
		t.Fatalf("second final message = %+v", final.Message)
	}
}

func TestStreamCodexWebSocketHandshakeErrorClosesBody(t *testing.T) {
	var closed atomic.Bool
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{},
				Body:       closeTrackingBody{Reader: strings.NewReader("usage limit"), closed: &closed},
				Request:    req,
			}, nil
		}),
	}
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")

	var streamErr error
	for _, err := range streamCodexWebSocket(context.Background(), client, model, &StreamOptions{
		Model:      model.ID,
		APIKey:     "test-token",
		MaxRetries: -1,
	}) {
		if err != nil {
			streamErr = err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("handshake rejection must surface as a transport error")
	}
	if !closed.Load() {
		t.Fatal("handshake response body was not closed")
	}
}

func TestStreamCodexWebSocketOnResponseErrorClosesBody(t *testing.T) {
	want := errors.New("response rejected")
	var closed atomic.Bool
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{},
				Body:       closeTrackingBody{Reader: strings.NewReader("usage limit"), closed: &closed},
				Request:    req,
			}, nil
		}),
	}
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")

	for _, err := range streamCodexWebSocket(context.Background(), client, model, &StreamOptions{
		Model:      model.ID,
		APIKey:     "test-token",
		MaxRetries: -1,
		OnResponse: func(ProviderResponse, Model) error {
			return want
		},
	}) {
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if !closed.Load() {
			t.Fatal("handshake response body was not closed")
		}
		return
	}
	t.Fatal("stream ended without iterator error")
}

func TestStreamCodexWebSocketDialErrorIsIteratorError(t *testing.T) {
	want := errors.New("dial failed")
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, want
		}),
	}
	model, _ := GetModel("openai-codex", "gpt-5.4-mini")

	for _, err := range streamCodexWebSocket(context.Background(), client, model, &StreamOptions{
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

type closeTrackingBody struct {
	*strings.Reader
	closed *atomic.Bool
}

func (b closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func readCodexWebSocketRequest(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()

	messageType, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatalf("read websocket request: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type = %v, want text", messageType)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode websocket request: %v", err)
	}
	return payload
}

func writeCodexWebSocketFrames(t *testing.T, conn *websocket.Conn, frames []string) {
	t.Helper()

	for _, frame := range frames {
		err := conn.Write(context.Background(), websocket.MessageText, []byte(frame))
		if err != nil {
			t.Fatalf("write websocket frame: %v", err)
		}
	}
}

func TestStreamCodexWebSocketCachedReusesConnectionAndSendsDelta(t *testing.T) {
	var wsConnections atomic.Int32
	var payloads []map[string]any
	var payloadMu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConnections.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		for i := 1; ; i++ {
			messageType, data, err := conn.Read(context.Background())
			if err != nil {
				return // client released / closed
			}
			if messageType != websocket.MessageText {
				t.Errorf("message type = %v", messageType)
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Errorf("decode request: %v", err)
				return
			}
			payloadMu.Lock()
			payloads = append(payloads, payload)
			respID := fmt.Sprintf("resp_%d", len(payloads))
			payloadMu.Unlock()
			writeCodexWebSocketFrames(t, conn, []string{
				`{"type":"response.created","response":{"id":"` + respID + `"}}`,
				`{"type":"response.output_item.added","item":{"type":"message","id":"msg_` + respID + `"}}`,
				`{"type":"response.output_text.delta","delta":"hi"}`,
				`{"type":"response.output_item.done","item":` +
					`{"type":"message","id":"msg_` + respID + `","content":[{"type":"output_text","text":"hi"}]}}`,
				`{"type":"response.completed","response":{"id":"` + respID + `","status":"completed"}}`,
			})
		}
	}))
	defer server.Close()

	model, _ := GetModel("openai-codex", "gpt-5.4-mini")
	model.BaseURL = server.URL
	userMsg := func(text string) Message {
		return Message{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: text}}}
	}
	opts := func(messages []Message) *StreamOptions {
		return &StreamOptions{
			Model:        model.ID,
			APIKey:       "test-token",
			Transport:    TransportWebSocketCached,
			SessionID:    "ws-cached-delta-session",
			SystemPrompt: "system",
			Messages:     messages,
		}
	}

	first := collectProviderEvents(t, streamCodexWebSocket(context.Background(),
		server.Client(), model, opts([]Message{userMsg("hello")})))
	assistant := first[len(first)-1].Message
	if assistant.ResponseID != "resp_1" {
		t.Fatalf("first response id = %q", assistant.ResponseID)
	}

	history := []Message{userMsg("hello"), *assistant, userMsg("again")}
	second := collectProviderEvents(t, streamCodexWebSocket(context.Background(),
		server.Client(), model, opts(history)))
	if second[len(second)-1].Message.ResponseID != "resp_2" {
		t.Fatalf("second response id = %q", second[len(second)-1].Message.ResponseID)
	}

	// pi reuses the session's cached connection (acquireWebSocket,
	// openai-codex-responses.ts:875-960)...
	if got := wsConnections.Load(); got != 1 {
		t.Fatalf("ws connections = %d, want 1 (connection reuse)", got)
	}

	payloadMu.Lock()
	defer payloadMu.Unlock()
	if len(payloads) != 2 {
		t.Fatalf("payloads = %d, want 2", len(payloads))
	}
	if _, ok := payloads[0]["previous_response_id"]; ok {
		t.Fatalf("first request carries previous_response_id: %v", payloads[0])
	}
	// ...and the follow-up sends only the input delta plus
	// previous_response_id from continuation state
	// (openai-codex-responses.ts:1131-1170,1199-1262).
	if got := payloads[1]["previous_response_id"]; got != "resp_1" {
		t.Fatalf("previous_response_id = %v, want resp_1", got)
	}
	deltaInput, _ := payloads[1]["input"].([]any)
	if len(deltaInput) != 1 {
		t.Fatalf("delta input = %v, want only the new user message", payloads[1]["input"])
	}
	item, _ := deltaInput[0].(map[string]any)
	if item["role"] != "user" {
		t.Fatalf("delta item = %v, want user message", item)
	}
}
