package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	aioauth "github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
	"github.com/cunninghamcard-bit/Attention/internal/ai/sseparse"
	"math"
	"regexp"
)

const defaultCodexBaseURL = "https://chatgpt.com/backend-api"
const codexOriginator = "pi"
const defaultCodexMaxRetries = 3

type codexProvider struct {
	events iter.Seq2[codexStreamEvent, error]
	client *http.Client
}

func (p codexProvider) Stream(ctx context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error] {
	model, _ := modelFromOptions(opts)
	if p.events == nil {
		client := p.client
		if client == nil {
			client = http.DefaultClient
		}
		// pi attempts the WebSocket transport for every transport except
		// "sse" and falls back to SSE on pre-stream transport failures, even
		// when websocket was pinned explicitly
		// (openai-codex-responses.ts:182-228).
		if opts.Transport == TransportSSE {
			return streamCodexSSE(ctx, client, model, opts)
		}
		return streamCodexAuto(ctx, client, model, opts)
	}
	return streamCodexEvents(ctx, model, p.events)
}

type codexStreamEvent struct {
	Type         string
	ResponseID   string
	Item         openAIResponsesItem
	Delta        string
	Arguments    string
	Status       string
	Usage        *Usage
	ErrorCode    string
	ErrorMessage string
}

type codexRequestBody struct {
	Model             string          `json:"model"`
	Store             bool            `json:"store"`
	Stream            bool            `json:"stream"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             []any           `json:"input,omitempty"`
	Tools             []codexTool     `json:"tools,omitempty"`
	ToolChoice        string          `json:"tool_choice,omitempty"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
	Temperature       float64         `json:"temperature,omitempty"`
	MaxTokens         int             `json:"max_output_tokens,omitempty"`
	Text              *codexText      `json:"text,omitempty"`
	Include           []string        `json:"include,omitempty"`
	Reasoning         *codexReasoning `json:"reasoning,omitempty"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
}

type codexReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type codexTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type codexText struct {
	Verbosity string `json:"verbosity,omitempty"`
}

type codexInputMessage struct {
	Role    string              `json:"role"`
	Content []codexInputContent `json:"content"`
}

type codexInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Detail   string `json:"detail,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type codexOutputMessage struct {
	Type    string               `json:"type"`
	Role    string               `json:"role"`
	Content []codexOutputContent `json:"content"`
}

type codexOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type codexFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

func streamCodexSSE(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		streamCtx, cancel := codexStreamContext(ctx, opts)
		defer cancel()

		body := buildCodexRequestBody(model, opts)
		payload := any(body)
		if opts.OnPayload != nil {
			nextPayload, changed, err := opts.OnPayload(payload, model)
			if err != nil {
				yield(nil, err)
				return
			}
			if changed {
				payload = nextPayload
			}
		}

		data, err := json.Marshal(payload)
		if err != nil {
			yield(nil, fmt.Errorf("marshal codex request: %w", err))
			return
		}

		var resp *http.Response
		attempts := codexMaxAttempts(opts)
		for attempt := range attempts {
			req, requestErr := http.NewRequestWithContext(
				streamCtx,
				http.MethodPost,
				resolveCodexURL(model.BaseURL),
				bytes.NewReader(data),
			)
			if requestErr != nil {
				yield(nil, requestErr)
				return
			}
			req.Header = buildCodexBaseHeaders(model, opts)
			req.Header.Set("content-type", "application/json")
			req.Header.Set("accept", "text/event-stream")
			req.Header.Set("OpenAI-Beta", "responses=experimental")
			if opts.SessionID != "" {
				req.Header.Set("session_id", opts.SessionID)
				req.Header.Set("x-client-request-id", opts.SessionID)
			}

			resp, err = client.Do(req)
			if err != nil {
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				if streamCtx.Err() != nil {
					yield(codexAbortedEvent(model, streamCtx.Err()), nil)
					return
				}
				if attempt+1 < attempts && isRetryableCodexTransportError(err) {
					if err := waitCodexRetry(streamCtx, codexRetryDelay(nil, attempt)); err != nil {
						yield(codexAbortedEvent(model, err), nil)
						return
					}
					continue
				}
				yield(nil, err)
				return
			}

			if opts.OnResponse != nil {
				if err := opts.OnResponse(providerResponseFromHTTP(resp), model); err != nil {
					resp.Body.Close()
					yield(nil, err)
					return
				}
			}

			if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
				text, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				if attempt+1 < attempts && isRetryableCodexResponse(resp.StatusCode, string(text)) {
					delay := codexRetryDelay(resp.Header, attempt)
					resp.Body.Close()
					if err := waitCodexRetry(streamCtx, delay); err != nil {
						yield(codexAbortedEvent(model, err), nil)
						return
					}
					continue
				}
				resp.Body.Close()
				resp.Body = io.NopCloser(strings.NewReader(string(text)))
			}
			break
		}
		defer resp.Body.Close()

		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			text, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			msg := parseCodexErrorMessage(resp.StatusCode, string(text))
			modelEvent := errorMessageEvent(model, &APIError{API: model.API, Model: model.ID, Message: msg})
			yield(modelEvent, nil)
			return
		}

		events := parseCodexSSE(resp.Body)
		for event, err := range streamCodexEvents(streamCtx, model, events) {
			if !yield(event, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// codexSSEFallbackSessions remembers sessions whose WebSocket transport
// failed; subsequent requests skip the WS attempt entirely, mirroring pi's
// websocketSseFallbackSessions (openai-codex-responses.ts:177-180,711-730).
var codexSSEFallbackSessions sync.Map

func codexSSEFallbackActive(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	_, ok := codexSSEFallbackSessions.Load(sessionID)
	return ok
}

func recordCodexWebSocketFailure(sessionID string) {
	if sessionID != "" {
		codexSSEFallbackSessions.Store(sessionID, struct{}{})
	}
}

func streamCodexAuto(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		if codexSSEFallbackActive(opts.SessionID) {
			streamCodexSSEInto(ctx, client, model, opts, yield)
			return
		}
		wsOpts := *opts
		wsOpts.MaxRetries = -1
		emitted := false
		for event, err := range streamCodexWebSocket(ctx, client, model, &wsOpts) {
			if err != nil {
				if emitted {
					// pi rethrows once the message stream has started
					// (openai-codex-responses.ts:223-225).
					yield(nil, err)
					return
				}
				recordCodexWebSocketFailure(opts.SessionID)
				streamCodexSSEInto(ctx, client, model, opts, yield)
				return
			}
			emitted = true
			if !yield(event, nil) {
				return
			}
		}
	}
}

func streamCodexSSEInto(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
	yield func(*StreamEvent, error) bool,
) {
	for event, err := range streamCodexSSE(ctx, client, model, opts) {
		if !yield(event, err) {
			return
		}
		if err != nil {
			return
		}
	}
}

func codexStreamContext(ctx context.Context, opts *StreamOptions) (context.Context, context.CancelFunc) {
	if opts.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, opts.Timeout)
}

func codexMaxAttempts(opts *StreamOptions) int {
	retries := opts.MaxRetries
	if retries == 0 {
		retries = defaultCodexMaxRetries
	} else if retries < 0 {
		retries = 0
	}
	return retries + 1
}

func isRetryableCodexResponse(status int, text string) bool {
	if isRetryableCodexStatus(status) {
		return true
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "overloaded") ||
		strings.Contains(lower, "service unavailable") ||
		strings.Contains(lower, "service_unavailable") ||
		strings.Contains(lower, "upstream connect") ||
		strings.Contains(lower, "connection refused")
}

func isRetryableCodexStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusInternalServerError ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

func isRetryableCodexTransportError(err error) bool {
	return !strings.Contains(strings.ToLower(err.Error()), "usage limit")
}

func codexRetryDelay(headers http.Header, attempt int) time.Duration {
	if headers != nil {
		if raw := headers.Get("retry-after-ms"); raw != "" {
			if millis, err := strconv.ParseFloat(raw, 64); err == nil {
				if millis <= 0 {
					return 0
				}
				return time.Duration(millis * float64(time.Millisecond))
			}
		}
		if raw := headers.Get("retry-after"); raw != "" {
			if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
				if seconds <= 0 {
					return 0
				}
				return time.Duration(seconds * float64(time.Second))
			}
			if retryTime, err := http.ParseTime(raw); err == nil {
				delay := time.Until(retryTime)
				if delay <= 0 {
					return 0
				}
				return delay
			}
		}
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Second * time.Duration(1<<attempt)
}

func waitCodexRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func codexAbortedEvent(model Model, err error) *StreamEvent {
	output := newAssistantMessage(model)
	output.StopReason = StopReasonAborted
	output.ErrorMessage = err.Error()
	return &StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}
}

func buildCodexRequestBody(model Model, opts *StreamOptions) codexRequestBody {
	messages := TransformMessages(opts.Messages, model, normalizeOpenAIToolCallID)
	instructions := opts.SystemPrompt
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}

	// ChatGPT Codex Responses follows pi's fixed request shape: prompt cache key
	// is session-scoped and encrypted reasoning is always included.
	body := codexRequestBody{
		Model:             model.ID,
		Store:             false,
		Stream:            true,
		Instructions:      instructions,
		Input:             convertCodexMessages(messages, model),
		Tools:             convertCodexTools(opts.Tools),
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Temperature:       opts.Temperature,
		MaxTokens:         opts.MaxTokens,
		Text:              &codexText{Verbosity: "low"},
		Include:           []string{"reasoning.encrypted_content"},
	}
	if cacheRetentionEnabled(opts.CacheRetention) {
		body.PromptCacheKey = opts.SessionID
	}
	if effort := reasoningEffort(model, opts.Reasoning); effort != "" {
		body.Reasoning = &codexReasoning{
			Effort:  mappedThinkingLevel(model, effort),
			Summary: "auto",
		}
	}
	return body
}

func convertCodexTools(tools []Tool) []codexTool {
	if len(tools) == 0 {
		return nil
	}

	result := make([]codexTool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, codexTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		})
	}
	return result
}

func convertCodexMessages(messages []Message, model Model) []any {
	input := make([]any, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case RoleUser:
			input = append(input, codexInputMessage{
				Role:    "user",
				Content: convertCodexInputContent(message.Content, model),
			})
		case RoleAssistant:
			isDifferentModel := message.Model != model.ID &&
				message.Provider == model.Provider && message.API == model.API
			for _, block := range message.Content {
				switch block.Type {
				case ContentThinking:
					// pi replays the stored reasoning item verbatim
					// (openai-responses-shared.ts:172-176); codex requests
					// include reasoning.encrypted_content so dropping it
					// breaks chain-of-thought continuity.
					if block.ThinkingSignature == "" {
						continue
					}
					var reasoning map[string]any
					if err := json.Unmarshal([]byte(block.ThinkingSignature), &reasoning); err == nil {
						input = append(input, reasoning)
					}
				case ContentText:
					input = append(input, codexOutputMessage{
						Type: "message",
						Role: "assistant",
						Content: []codexOutputContent{{
							Type: "output_text",
							Text: block.Text,
						}},
					})
				case ContentToolCall:
					callID, itemID := splitToolCallID(block.ToolCallID)
					if isDifferentModel && strings.HasPrefix(itemID, "fc_") {
						itemID = ""
					}
					input = append(input, codexFunctionCall{
						Type:      "function_call",
						ID:        itemID,
						CallID:    callID,
						Name:      block.ToolName,
						Arguments: mustMarshalJSON(block.Arguments),
					})
				}
			}
		case RoleToolResult:
			callID, _ := splitToolCallID(message.ToolCallID)
			input = append(input, codexFunctionCallOutput{
				Type:   "function_call_output",
				CallID: callID,
				Output: textContent(message.Content),
			})
		}
	}
	return input
}

func convertCodexInputContent(content []ContentBlock, model Model) []codexInputContent {
	result := make([]codexInputContent, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case ContentText:
			result = append(result, codexInputContent{Type: "input_text", Text: block.Text})
		case ContentImage:
			if ModelSupportsInput(model, InputImage) {
				result = append(result, codexInputContent{
					Type:     "input_image",
					Detail:   "auto",
					ImageURL: fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.ImageData),
				})
			}
		}
	}
	return result
}

func parseCodexSSE(reader io.Reader) iter.Seq2[codexStreamEvent, error] {
	return func(yield func(codexStreamEvent, error) bool) {
		for event, err := range sseparse.Parse(reader) {
			if err != nil {
				yield(codexStreamEvent{}, err)
				return
			}
			if event.Data == "" || event.Data == "[DONE]" {
				continue
			}
			streamEvent, ok, err := decodeCodexStreamData([]byte(event.Data))
			if err != nil {
				yield(codexStreamEvent{}, err)
				return
			}
			if ok && !yield(streamEvent, nil) {
				return
			}
		}
	}
}

func decodeCodexStreamData(data []byte) (codexStreamEvent, bool, error) {
	var raw struct {
		Type      string          `json:"type"`
		Delta     string          `json:"delta"`
		Arguments string          `json:"arguments"`
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		Error     json.RawMessage `json:"error"`
		Item      json.RawMessage `json:"item"`
		Response  json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return codexStreamEvent{}, false, fmt.Errorf("decode codex stream data: %w", err)
	}
	if raw.Type == "" {
		return codexStreamEvent{}, false, nil
	}

	event := codexStreamEvent{
		Type:         normalizeCodexEventType(raw.Type),
		Delta:        raw.Delta,
		Arguments:    raw.Arguments,
		ErrorCode:    raw.Code,
		ErrorMessage: raw.Message,
	}
	if len(raw.Error) > 0 {
		code, message, err := decodeCodexError(raw.Error)
		if err != nil {
			return codexStreamEvent{}, false, err
		}
		if event.ErrorCode == "" {
			event.ErrorCode = code
		}
		if event.ErrorMessage == "" {
			event.ErrorMessage = message
		}
	}
	if len(raw.Item) > 0 {
		item, err := decodeOpenAIResponsesItem(raw.Item)
		if err != nil {
			return codexStreamEvent{}, false, err
		}
		event.Item = item
	}
	if len(raw.Response) > 0 {
		response, err := decodeCodexResponse(raw.Response)
		if err != nil {
			return codexStreamEvent{}, false, err
		}
		event.ResponseID = response.id
		event.Status = response.status
		event.Usage = response.usage
		if event.ErrorCode == "" {
			event.ErrorCode = response.errorCode
		}
		if event.ErrorMessage == "" {
			event.ErrorMessage = response.errorMessage
		}
	}
	return event, true, nil
}

func decodeCodexError(data []byte) (string, string, error) {
	var raw struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", fmt.Errorf("decode codex error: %w", err)
	}
	code := raw.Code
	if code == "" {
		code = raw.Type
	}
	return code, raw.Message, nil
}

func normalizeCodexEventType(eventType string) string {
	switch eventType {
	case "response.done":
		return "response.completed"
	default:
		return eventType
	}
}

func decodeOpenAIResponsesItem(data []byte) (openAIResponsesItem, error) {
	var raw struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Summary   []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return openAIResponsesItem{}, fmt.Errorf("decode codex item: %w", err)
	}
	item := openAIResponsesItem{
		Type:      raw.Type,
		ID:        raw.ID,
		CallID:    raw.CallID,
		Name:      raw.Name,
		Arguments: raw.Arguments,
		Phase:     raw.Phase,
		Raw:       append(json.RawMessage(nil), data...),
	}
	for i, summary := range raw.Summary {
		if i > 0 {
			item.Summary += "\n\n"
		}
		item.Summary += summary.Text
	}
	for _, content := range raw.Content {
		switch content.Type {
		case "output_text":
			item.Text += content.Text
		case "refusal":
			item.Text += content.Refusal
		}
	}
	return item, nil
}

type codexResponse struct {
	id           string
	status       string
	usage        *Usage
	errorCode    string
	errorMessage string
}

func decodeCodexResponse(data []byte) (codexResponse, error) {
	var raw struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Usage  struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return codexResponse{}, fmt.Errorf("decode codex response: %w", err)
	}
	cachedTokens := raw.Usage.InputTokensDetails.CachedTokens
	usage := &Usage{
		Input:       raw.Usage.InputTokens - cachedTokens,
		Output:      raw.Usage.OutputTokens,
		CacheRead:   cachedTokens,
		CacheWrite:  0,
		TotalTokens: raw.Usage.TotalTokens,
		Cost:        &Cost{},
	}
	return codexResponse{
		id:           raw.ID,
		status:       raw.Status,
		usage:        usage,
		errorCode:    raw.Error.Code,
		errorMessage: raw.Error.Message,
	}, nil
}

func streamCodexEvents(
	ctx context.Context,
	model Model,
	events iter.Seq2[codexStreamEvent, error],
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		output := newAssistantMessage(model)
		var current *openAIResponsesBlock

	stream:
		for event, err := range events {
			if err != nil {
				if ctx.Err() != nil {
					output.StopReason = StopReasonAborted
					output.ErrorMessage = ctx.Err().Error()
					if !yield(&StreamEvent{Type: EventMessageError, Message: output, Usage: output.Usage}, nil) {
						return
					}
					yield(&StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}, nil)
					return
				}
				yield(nil, err)
				return
			}
			select {
			case <-ctx.Done():
				output.StopReason = StopReasonAborted
				output.ErrorMessage = ctx.Err().Error()
				if !yield(&StreamEvent{Type: EventMessageError, Message: output, Usage: output.Usage}, nil) {
					return
				}
				yield(&StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}, nil)
				return
			default:
			}

			switch event.Type {
			case "response.created":
				output.ResponseID = event.ResponseID
				if !yield(&StreamEvent{Type: EventMessageStart, Message: output}, nil) {
					return
				}
			case "response.output_item.added":
				block, ok := openAIStartBlock(event.Item)
				if !ok {
					continue
				}
				output.Content = append(output.Content, block.ContentBlock)
				current = &block
				index := len(output.Content) - 1
				startType := contentBlockStartEvent(output.Content[index].Type)
				if !yield(&StreamEvent{Type: startType, Index: index, Delta: &output.Content[index], Message: output}, nil) {
					return
				}
			case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
				if current == nil || current.Type != ContentThinking {
					continue
				}
				output.Content[len(output.Content)-1].Thinking += event.Delta
				current.Thinking += event.Delta
				if !yield(&StreamEvent{
					Type:    EventThinkingDelta,
					Index:   len(output.Content) - 1,
					Delta:   &ContentBlock{Type: ContentThinking, Thinking: event.Delta},
					Message: output,
				}, nil) {
					return
				}
			case "response.output_text.delta", "response.refusal.delta":
				if current == nil || current.Type != ContentText {
					continue
				}
				output.Content[len(output.Content)-1].Text += event.Delta
				current.Text += event.Delta
				if !yield(&StreamEvent{
					Type:    EventTextDelta,
					Index:   len(output.Content) - 1,
					Delta:   &ContentBlock{Type: ContentText, Text: event.Delta},
					Message: output,
				}, nil) {
					return
				}
			case "response.function_call_arguments.delta":
				if current == nil || current.Type != ContentToolCall {
					continue
				}
				current.partialJSON += event.Delta
				output.Content[len(output.Content)-1].Arguments = decodeJSONMap(current.partialJSON)
				if !yield(&StreamEvent{
					Type:  EventToolCallDelta,
					Index: len(output.Content) - 1,
					Delta: &ContentBlock{
						Type:      ContentToolCall,
						Arguments: output.Content[len(output.Content)-1].Arguments,
					},
					Message: output,
				}, nil) {
					return
				}
			case "response.function_call_arguments.done":
				if current == nil || current.Type != ContentToolCall {
					continue
				}
				if applyFunctionCallArgumentsDone(current, &output.Content[len(output.Content)-1], event.Arguments) {
					if !yield(&StreamEvent{
						Type:  EventToolCallDelta,
						Index: len(output.Content) - 1,
						Delta: &ContentBlock{
							Type:      ContentToolCall,
							Arguments: output.Content[len(output.Content)-1].Arguments,
						},
						Message: output,
					}, nil) {
						return
					}
				}
			case "response.output_item.done":
				if current == nil {
					continue
				}
				idx := len(output.Content) - 1
				finalizeOpenAIResponsesBlock(current, event.Item, &output.Content[idx])
				endType := contentBlockEndEvent(current.Type)
				current = nil
				if !yield(&StreamEvent{Type: endType, Index: idx, Delta: &output.Content[idx], Message: output}, nil) {
					return
				}
			case "response.done", "response.completed", "response.incomplete":
				if event.ResponseID != "" {
					output.ResponseID = event.ResponseID
				}
				if event.Usage != nil {
					output.Usage = usageWithTotals(event.Usage)
					CalculateCost(model, output.Usage)
				}
				status := event.Status
				if event.Type == "response.incomplete" && status == "" {
					status = "incomplete"
				}
				output.StopReason = mapOpenAIResponsesStopReason(status)
				if output.StopReason == StopReasonStop && outputHasToolCall(output) {
					output.StopReason = StopReasonToolUse
				}
				// pi stops consuming after the terminal event; trailing SSE
				// frames are never processed (openai-codex-responses.ts:536-543).
				break stream
			case "error", "response.failed":
				output.StopReason = StopReasonError
				output.ErrorMessage = event.ErrorMessage
				if output.ErrorMessage == "" {
					output.ErrorMessage = fmt.Sprintf("Codex error: %s", event.ErrorCode)
				}
				if !yield(&StreamEvent{Type: EventMessageError, Message: output, Usage: output.Usage}, nil) {
					return
				}
				yield(&StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}, nil)
				return
			default:
				// pi silently ignores unrecognized event types
				// (openai-codex-responses.ts:545; processResponsesStream has
				// no else branch), so new backend events never kill a stream.
				continue
			}
		}

		if output.StopReason == "" {
			output.StopReason = StopReasonStop
		}
		doneOrError := EventMessageDone
		if output.StopReason == StopReasonError || output.StopReason == StopReasonAborted {
			doneOrError = EventMessageError
		}
		if !yield(&StreamEvent{Type: doneOrError, Message: output, Usage: output.Usage}, nil) {
			return
		}
		yield(&StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}, nil)
	}
}

var codexUsageLimitCodes = regexp.MustCompile(`(?i)usage_limit_reached|usage_not_included|rate_limit_exceeded`)

// parseCodexErrorMessage mirrors pi's parseErrorResponse: usage-limit errors
// become an actionable message built from plan_type/resets_at, otherwise the
// JSON error.message, otherwise the raw body. pi surfaces the message bare,
// with no status wrapper (openai-codex-responses.ts:286-291,1269-1294).
func parseCodexErrorMessage(status int, raw string) string {
	message := strings.TrimSpace(raw)
	if message == "" {
		message = http.StatusText(status)
	}
	if message == "" {
		message = "Request failed"
	}

	var parsed struct {
		Error *struct {
			Code     string  `json:"code"`
			Type     string  `json:"type"`
			Message  string  `json:"message"`
			PlanType string  `json:"plan_type"`
			ResetsAt float64 `json:"resets_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil || parsed.Error == nil {
		return message
	}
	apiErr := parsed.Error

	code := apiErr.Code
	if code == "" {
		code = apiErr.Type
	}
	if codexUsageLimitCodes.MatchString(code) || status == http.StatusTooManyRequests {
		plan := ""
		if apiErr.PlanType != "" {
			plan = " (" + strings.ToLower(apiErr.PlanType) + " plan)"
		}
		when := ""
		if apiErr.ResetsAt != 0 {
			mins := int(math.Round((apiErr.ResetsAt*1000 - float64(time.Now().UnixMilli())) / 60000))
			if mins < 0 {
				mins = 0
			}
			when = fmt.Sprintf(" Try again in ~%d min.", mins)
		}
		return strings.TrimSpace("You have hit your ChatGPT usage limit" + plan + "." + when)
	}
	if apiErr.Message != "" {
		return apiErr.Message
	}
	return message
}

func resolveCodexURL(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		raw = defaultCodexBaseURL
	}
	raw = strings.TrimRight(raw, "/")
	if strings.HasSuffix(raw, "/codex/responses") {
		return raw
	}
	if strings.HasSuffix(raw, "/codex") {
		return raw + "/responses"
	}
	return raw + "/codex/responses"
}

func providerResponseFromHTTP(resp *http.Response) ProviderResponse {
	headers := make(map[string]string, len(resp.Header))
	for key, values := range resp.Header {
		headers[key] = strings.Join(values, ",")
	}
	return ProviderResponse{Status: resp.StatusCode, Headers: headers}
}

func buildCodexBaseHeaders(model Model, opts *StreamOptions) http.Header {
	headers := http.Header{}
	for key, value := range model.Headers {
		headers.Set(key, value)
	}
	for key, value := range opts.Headers {
		headers.Set(key, value)
	}
	if opts.APIKey != "" {
		headers.Set("authorization", "Bearer "+opts.APIKey)
		if accountID, err := aioauth.ExtractOpenAICodexAccountID(opts.APIKey); err == nil {
			headers.Set("chatgpt-account-id", accountID)
		}
	}
	headers.Set("originator", codexOriginator)
	headers.Set("user-agent", codexUserAgent())
	return headers
}

func codexUserAgent() string {
	return fmt.Sprintf("pi (go %s/%s)", runtime.GOOS, runtime.GOARCH)
}

// openAIToolCallProviders mirrors pi's OPENAI/CODEX_TOOL_CALL_PROVIDERS sets
// (openai-responses.ts:25, openai-codex-responses.ts:55).
var openAIToolCallProviders = map[string]bool{
	"openai":       true,
	"openai-codex": true,
	"opencode":     true,
}

// normalizeOpenAIToolCallID mirrors pi's normalizeToolCallId
// (openai-responses-shared.ts:109-122): ids without an item part (e.g.
// anthropic toolu_... from cross-provider history) stay a single part so the
// function_call is emitted WITHOUT an id; foreign piped ids get a hashed
// fc_ item id instead of a fabricated unverifiable one.
func normalizeOpenAIToolCallID(id string, model Model, source Message) string {
	if !openAIToolCallProviders[model.Provider] {
		return normalizeIDPart(id)
	}
	callID, itemID, ok := strings.Cut(id, "|")
	if !ok {
		return normalizeIDPart(id)
	}
	foreign := source.Provider != model.Provider || source.API != model.API
	if foreign {
		itemID = foreignResponsesItemID(itemID)
	} else {
		itemID = normalizeIDPart(itemID)
	}
	if !strings.HasPrefix(itemID, "fc_") {
		itemID = normalizeIDPart("fc_" + itemID)
	}
	return normalizeIDPart(callID) + "|" + itemID
}

// pi: openai-responses-shared.ts:104-107.
func foreignResponsesItemID(itemID string) string {
	normalized := "fc_" + shortHash(itemID)
	if len(normalized) > 64 {
		normalized = normalized[:64]
	}
	return normalized
}

func splitToolCallID(id string) (string, string) {
	callID, itemID, ok := strings.Cut(id, "|")
	if !ok {
		return id, ""
	}
	return callID, itemID
}

func normalizeIDPart(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return strings.TrimRight(b.String(), "_")
}

func mustMarshalJSON(value any) string {
	if value == nil {
		return "{}"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func textContent(content []ContentBlock) string {
	parts := []string{}
	for _, block := range content {
		switch block.Type {
		case ContentText:
			parts = append(parts, block.Text)
		case ContentImage:
			parts = append(parts, "(see attached image)")
		}
	}
	return strings.Join(parts, "\n")
}

func textBlocksContent(content []ContentBlock) string {
	parts := []string{}
	for _, block := range content {
		if block.Type == ContentText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func hasImageContent(content []ContentBlock) bool {
	for _, block := range content {
		if block.Type == ContentImage {
			return true
		}
	}
	return false
}
