package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strconv"
	"strings"

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
)

type openAIResponsesProvider struct {
	events iter.Seq2[openAIResponsesStreamEvent, error]
	client *http.Client
}

func (p openAIResponsesProvider) Stream(ctx context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error] {
	model, _ := modelFromOptions(opts)
	if p.events == nil {
		client := p.client
		if client == nil {
			client = http.DefaultClient
		}
		return streamOpenAIResponsesSDK(ctx, client, model, opts)
	}
	return streamOpenAIResponsesEvents(ctx, model, p.events)
}

type openAIResponsesStreamEvent struct {
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

type openAIResponsesItem struct {
	Type      string
	ID        string
	CallID    string
	Name      string
	Arguments string
	Text      string
	Summary   string
	Phase     string
	// Raw is the item's wire JSON. pi persists the FULL reasoning item
	// (including encrypted_content) as the thinking signature
	// (openai-responses-shared.ts:445), so the parsed fields above are not
	// enough.
	Raw json.RawMessage
}

type openAIResponsesBlock struct {
	ContentBlock
	itemType    string
	itemID      string
	partialJSON string
}

type openAIResponsesRequestBody struct {
	Model                string                         `json:"model"`
	Store                bool                           `json:"store"`
	Stream               bool                           `json:"stream"`
	Input                []any                          `json:"input,omitempty"`
	Tools                []openAIResponsesTool          `json:"tools,omitempty"`
	Temperature          float64                        `json:"temperature,omitempty"`
	MaxTokens            int                            `json:"max_output_tokens,omitempty"`
	PromptCacheKey       string                         `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string                         `json:"prompt_cache_retention,omitempty"`
	Metadata             map[string]any                 `json:"metadata,omitempty"`
	Include              []responses.ResponseIncludable `json:"include,omitempty"`
	Reasoning            *openAIResponsesReasoning      `json:"reasoning,omitempty"`
}

type openAIResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type openAIResponsesCompat struct {
	sendSessionIDHeader        bool
	supportsLongCacheRetention bool
}

type openAIResponsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"`
}

type openAIResponsesTextMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponsesUserMessage struct {
	Role    string                        `json:"role"`
	Content []openAIResponsesInputContent `json:"content"`
}

type openAIResponsesInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Detail   string `json:"detail,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type openAIResponsesOutputMessage struct {
	Type    string                         `json:"type"`
	Role    string                         `json:"role"`
	Content []openAIResponsesOutputContent `json:"content"`
	ID      string                         `json:"id"`
	Phase   string                         `json:"phase,omitempty"`
}

type openAIResponsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openAIResponsesFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIResponsesFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}

type openAIResponsesResponse struct {
	id           string
	status       string
	usage        *Usage
	errorCode    string
	errorMessage string
}

func streamOpenAIResponsesSDK(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		payload := any(buildOpenAIResponsesRequestBody(model, opts))
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

		params, err := openAIResponsesParamsFromPayload(payload)
		if err != nil {
			yield(nil, err)
			return
		}

		var resp *http.Response
		sdk := openai.NewClient(openAIResponsesOptions(client, model, opts, &resp)...)
		stream := sdk.Responses.NewStreaming(ctx, params)
		defer stream.Close()

		if opts.OnResponse != nil && resp != nil {
			if err := opts.OnResponse(providerResponseFromHTTP(resp), model); err != nil {
				yield(nil, err)
				return
			}
		}

		if err := stream.Err(); err != nil {
			if event, ok := openAIResponsesAPIErrorEvent(model, err); ok {
				yield(event, nil)
				return
			}
			yield(nil, err)
			return
		}

		events := streamOpenAIResponsesSDKEvents(stream)
		for event, err := range streamOpenAIResponsesEvents(ctx, model, events) {
			if !yield(event, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

func buildOpenAIResponsesRequestBody(model Model, opts *StreamOptions) openAIResponsesRequestBody {
	compat := getOpenAIResponsesCompat(model)
	messages := TransformMessages(opts.Messages, model, normalizeOpenAIToolCallID)
	cacheRetention := normalizeCacheRetention(opts.CacheRetention)
	body := openAIResponsesRequestBody{
		Model:       model.ID,
		Store:       false,
		Stream:      true,
		Input:       convertOpenAIResponsesMessages(opts.SystemPrompt, messages, model),
		Tools:       convertOpenAIResponsesTools(opts.Tools),
		Temperature: opts.Temperature,
		Metadata:    opts.Metadata,
	}
	if opts.MaxTokens > 0 {
		body.MaxTokens = opts.MaxTokens
	}
	if cacheRetention != CacheRetentionNone && opts.SessionID != "" {
		// pi clamps to OpenAI's documented 64-char prompt_cache_key limit
		// (openai-responses.ts:244, openai-prompt-cache.ts:1-8).
		body.PromptCacheKey = clampOpenAICompletionsPromptCacheKey(opts.SessionID)
	}
	if cacheRetention == CacheRetentionLong && compat.supportsLongCacheRetention {
		body.PromptCacheRetention = "24h"
	}
	applyOpenAIResponsesReasoning(model, opts, &body)
	return body
}

func getOpenAIResponsesCompat(model Model) openAIResponsesCompat {
	compat := openAIResponsesCompat{
		sendSessionIDHeader:        true,
		supportsLongCacheRetention: true,
	}
	if model.Compat == nil {
		return compat
	}
	compat.sendSessionIDHeader = boolCompat(
		model.Compat.SendSessionIdHeader,
		compat.sendSessionIDHeader,
	)
	compat.supportsLongCacheRetention = boolCompat(
		model.Compat.SupportsLongCacheRetention,
		compat.supportsLongCacheRetention,
	)
	return compat
}

func applyOpenAIResponsesReasoning(
	model Model,
	opts *StreamOptions,
	body *openAIResponsesRequestBody,
) {
	if !model.Reasoning {
		return
	}
	if effort := reasoningEffort(model, opts.Reasoning); effort != "" {
		body.Reasoning = &openAIResponsesReasoning{
			Effort:  mappedThinkingLevel(model, effort),
			Summary: "auto",
		}
		body.Include = []responses.ResponseIncludable{"reasoning.encrypted_content"}
		return
	}
	if model.Provider == "github-copilot" {
		return
	}
	if effort, ok := openAIResponsesOffEffort(model); ok {
		body.Reasoning = &openAIResponsesReasoning{Effort: effort}
	}
}

func openAIResponsesOffEffort(model Model) (string, bool) {
	if model.ThinkingLevelMap != nil {
		off, ok := model.ThinkingLevelMap["off"]
		if ok && off == nil {
			return "", false
		}
		if ok {
			return *off, true
		}
	}
	return "none", true
}

func convertOpenAIResponsesTools(tools []Tool) []openAIResponsesTool {
	if len(tools) == 0 {
		return nil
	}

	result := make([]openAIResponsesTool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, openAIResponsesTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
			Strict:      false,
		})
	}
	return result
}

func convertOpenAIResponsesMessages(systemPrompt string, messages []Message, model Model) []any {
	input := make([]any, 0, len(messages)+1)
	if systemPrompt != "" {
		role := "system"
		if model.Reasoning {
			role = "developer"
		}
		input = append(input, openAIResponsesTextMessage{
			Role:    role,
			Content: systemPrompt,
		})
	}

	for _, message := range messages {
		switch message.Role {
		case RoleUser:
			content := convertOpenAIResponsesInputContent(message.Content, model)
			if len(content) == 0 {
				continue
			}
			input = append(input, openAIResponsesUserMessage{
				Role:    "user",
				Content: content,
			})
		case RoleAssistant:
			input = append(input, convertOpenAIResponsesAssistantContent(message, model)...)
		case RoleToolResult:
			callID, _ := splitToolCallID(message.ToolCallID)
			input = append(input, openAIResponsesFunctionCallOutput{
				Type:   "function_call_output",
				CallID: callID,
				Output: openAIResponsesToolOutput(message.Content, model),
			})
		}
	}
	return input
}

func convertOpenAIResponsesInputContent(content []ContentBlock, model Model) []openAIResponsesInputContent {
	result := make([]openAIResponsesInputContent, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case ContentText:
			result = append(result, openAIResponsesInputContent{
				Type: "input_text",
				Text: block.Text,
			})
		case ContentImage:
			if ModelSupportsInput(model, InputImage) {
				result = append(result, openAIResponsesInputContent{
					Type:     "input_image",
					Detail:   "auto",
					ImageURL: fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.ImageData),
				})
			}
		}
	}
	return result
}

func convertOpenAIResponsesAssistantContent(message Message, model Model) []any {
	content := message.Content
	// OpenAI validates fc_/rs_ pairing per model; same-provider history from a
	// different model omits item ids to skip it (openai-responses-shared.ts:196-205).
	isDifferentModel := message.Model != model.ID &&
		message.Provider == model.Provider && message.API == model.API
	result := []any{}
	msgIndex := 0
	for _, block := range content {
		switch block.Type {
		case ContentThinking:
			if block.ThinkingSignature == "" {
				continue
			}
			var reasoning map[string]any
			if err := json.Unmarshal([]byte(block.ThinkingSignature), &reasoning); err == nil {
				result = append(result, reasoning)
			}
		case ContentText:
			parsedSignature := parseOpenAITextSignature(block.TextSignature)
			messageID := openAIMessageID(parsedSignature.id, msgIndex)
			msgIndex++
			message := openAIResponsesOutputMessage{
				Type: "message",
				Role: "assistant",
				Content: []openAIResponsesOutputContent{{
					Type: "output_text",
					Text: block.Text,
				}},
				ID: messageID,
			}
			if parsedSignature.phase != "" {
				message.Phase = parsedSignature.phase
			}
			result = append(result, message)
		case ContentToolCall:
			callID, itemID := splitToolCallID(block.ToolCallID)
			if isDifferentModel && strings.HasPrefix(itemID, "fc_") {
				itemID = ""
			}
			result = append(result, openAIResponsesFunctionCall{
				Type:      "function_call",
				ID:        itemID,
				CallID:    callID,
				Name:      block.ToolName,
				Arguments: mustMarshalJSON(block.Arguments),
			})
		}
	}
	return result
}

func openAIResponsesParamsFromPayload(payload any) (responses.ResponseNewParams, error) {
	switch value := payload.(type) {
	case responses.ResponseNewParams:
		return value, nil
	case *responses.ResponseNewParams:
		if value == nil {
			return responses.ResponseNewParams{}, errors.New("openai responses payload is nil")
		}
		return *value, nil
	case json.RawMessage:
		return param.Override[responses.ResponseNewParams](value), nil
	case []byte:
		return param.Override[responses.ResponseNewParams](json.RawMessage(value)), nil
	case string:
		return param.Override[responses.ResponseNewParams](json.RawMessage(value)), nil
	default:
		data, err := json.Marshal(payload)
		if err != nil {
			return responses.ResponseNewParams{}, fmt.Errorf("marshal openai responses request: %w", err)
		}
		return param.Override[responses.ResponseNewParams](json.RawMessage(data)), nil
	}
}

func openAIResponsesOptions(
	client *http.Client,
	model Model,
	opts *StreamOptions,
	resp **http.Response,
) []option.RequestOption {
	requestOptions := []option.RequestOption{
		option.WithHTTPClient(client),
		option.WithResponseInto(resp),
	}
	if retries, ok := sdkMaxRetries(opts.MaxRetries); ok {
		requestOptions = append(requestOptions, option.WithMaxRetries(retries))
	}
	if model.BaseURL != "" {
		requestOptions = append(requestOptions, option.WithBaseURL(model.BaseURL))
	}
	if opts.APIKey != "" && !model.AuthHeader {
		requestOptions = append(requestOptions, option.WithAPIKey(opts.APIKey))
	}
	if opts.Timeout > 0 {
		requestOptions = append(requestOptions, option.WithRequestTimeout(opts.Timeout))
	}
	if opts.SessionID != "" && cacheRetentionEnabled(opts.CacheRetention) {
		if getOpenAIResponsesCompat(model).sendSessionIDHeader {
			requestOptions = append(requestOptions, option.WithHeader("session_id", opts.SessionID))
		}
		requestOptions = append(requestOptions, option.WithHeader("x-client-request-id", opts.SessionID))
	}
	for key, value := range model.Headers {
		requestOptions = append(requestOptions, option.WithHeader(key, value))
	}
	for key, value := range opts.Headers {
		requestOptions = append(requestOptions, option.WithHeader(key, value))
	}
	if opts.APIKey != "" && model.AuthHeader {
		requestOptions = append(requestOptions, option.WithHeader("authorization", "Bearer "+opts.APIKey))
	}
	return requestOptions
}

func streamOpenAIResponsesSDKEvents(
	stream interface {
		Next() bool
		Current() responses.ResponseStreamEventUnion
		Err() error
	},
) iter.Seq2[openAIResponsesStreamEvent, error] {
	return func(yield func(openAIResponsesStreamEvent, error) bool) {
		completed := false
		for stream.Next() {
			event, ok, err := decodeOpenAIResponsesEventData([]byte(stream.Current().RawJSON()))
			if err != nil {
				yield(openAIResponsesStreamEvent{}, err)
				return
			}
			if !ok {
				continue
			}
			if event.Type == "response.completed" {
				completed = true
			}
			if !yield(event, nil) {
				return
			}
		}
		// After a successful terminal event the response is complete; some
		// proxies/SDKs then surface a trailing EOF/JSON decode error from
		// stream.Err() ("unexpected end of JSON input"). Swallow it — pi
		// tolerates this — so it is not recorded as the assistant's error.
		if completed {
			return
		}
		if err := stream.Err(); err != nil {
			if message, ok := openAIResponsesAPIErrorMessage(err); ok {
				yield(openAIResponsesStreamEvent{Type: "error", ErrorMessage: message}, nil)
				return
			}
			yield(openAIResponsesStreamEvent{}, err)
		}
	}
}

func decodeOpenAIResponsesEventData(data []byte) (openAIResponsesStreamEvent, bool, error) {
	// Some SDK/proxy stream events carry empty RawJSON (sentinel/unmapped events,
	// trailing blank SSE lines). Skip them instead of failing json.Unmarshal("")
	// with "unexpected end of JSON input" — the content stream is already complete.
	if len(strings.TrimSpace(string(data))) == 0 {
		return openAIResponsesStreamEvent{}, false, nil
	}
	var raw struct {
		Type      string          `json:"type"`
		Delta     string          `json:"delta"`
		Arguments string          `json:"arguments"`
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		Item      json.RawMessage `json:"item"`
		Response  json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return openAIResponsesStreamEvent{}, false, fmt.Errorf("decode openai responses event: %w", err)
	}
	if raw.Type == "" {
		return openAIResponsesStreamEvent{}, false, nil
	}

	event := openAIResponsesStreamEvent{
		Type:         raw.Type,
		Delta:        raw.Delta,
		Arguments:    raw.Arguments,
		ErrorCode:    raw.Code,
		ErrorMessage: raw.Message,
	}
	if len(raw.Item) > 0 {
		item, err := decodeOpenAIResponsesItem(raw.Item)
		if err != nil {
			return openAIResponsesStreamEvent{}, false, err
		}
		event.Item = item
	}
	if len(raw.Response) > 0 {
		response, err := decodeOpenAIResponsesResponse(raw.Response)
		if err != nil {
			return openAIResponsesStreamEvent{}, false, err
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

func decodeOpenAIResponsesResponse(data []byte) (openAIResponsesResponse, error) {
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
		return openAIResponsesResponse{}, fmt.Errorf("decode openai responses response: %w", err)
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
	return openAIResponsesResponse{
		id:           raw.ID,
		status:       raw.Status,
		usage:        usage,
		errorCode:    raw.Error.Code,
		errorMessage: raw.Error.Message,
	}, nil
}

func openAIResponsesAPIErrorEvent(model Model, err error) (*StreamEvent, bool) {
	message, ok := openAIResponsesAPIErrorMessage(err)
	if !ok {
		return nil, false
	}
	return errorMessageEvent(model, &APIError{
		API:     model.API,
		Model:   model.ID,
		Message: message,
	}), true
}

func openAIResponsesAPIErrorMessage(err error) (string, bool) {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return "", false
	}
	message := apiErr.Message
	if message == "" {
		message = err.Error()
	}
	if apiErr.StatusCode != 0 {
		message = fmt.Sprintf("OpenAI API error (%d): %s", apiErr.StatusCode, message)
	}
	return message, true
}

func streamOpenAIResponsesEvents(
	ctx context.Context,
	model Model,
	events iter.Seq2[openAIResponsesStreamEvent, error],
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		output := newAssistantMessage(model)
		var current *openAIResponsesBlock

		for event, err := range events {
			if err != nil {
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
			case "response.completed", "response.incomplete":
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
			case "error", "response.failed":
				output.StopReason = StopReasonError
				output.ErrorMessage = event.ErrorMessage
				if output.ErrorMessage == "" {
					output.ErrorMessage = fmt.Sprintf("OpenAI API error: %s", event.ErrorCode)
				}
				if !yield(&StreamEvent{Type: EventMessageError, Message: output, Usage: output.Usage}, nil) {
					return
				}
				yield(&StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}, nil)
				return
			default:
				// pi's processResponsesStream is an else-if chain with no
				// else: unrecognized event types (response.refusal.done is
				// reachable today) are skipped, never fatal
				// (openai-responses-shared.ts:295-528).
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

func openAIStartBlock(item openAIResponsesItem) (openAIResponsesBlock, bool) {
	switch item.Type {
	case "reasoning":
		return openAIResponsesBlock{
			ContentBlock: ContentBlock{Type: ContentThinking, Thinking: item.Summary},
			itemType:     item.Type,
			itemID:       item.ID,
		}, true
	case "message":
		return openAIResponsesBlock{
			ContentBlock: ContentBlock{Type: ContentText},
			itemType:     item.Type,
			itemID:       item.ID,
		}, true
	case "function_call":
		return openAIResponsesBlock{
			ContentBlock: ContentBlock{
				Type:       ContentToolCall,
				ToolCallID: item.CallID + "|" + item.ID,
				ToolName:   item.Name,
				Arguments:  decodeJSONMap(item.Arguments),
			},
			itemType:    item.Type,
			itemID:      item.ID,
			partialJSON: item.Arguments,
		}, true
	default:
		return openAIResponsesBlock{}, false
	}
}

func finalizeOpenAIResponsesBlock(block *openAIResponsesBlock, item openAIResponsesItem, output *ContentBlock) {
	switch block.Type {
	case ContentThinking:
		if item.Summary != "" {
			output.Thinking = item.Summary
		}
		// pi stores the whole reasoning item as JSON and replays it verbatim
		// on the next request (openai-responses-shared.ts:172-176,445); a bare
		// rs_... id can never be parsed back into an item.
		output.ThinkingSignature = string(item.Raw)
		if output.ThinkingSignature == "" {
			output.ThinkingSignature = item.ID
		}
	case ContentText:
		if item.Text != "" {
			output.Text = item.Text
		}
		output.TextSignature = encodeOpenAITextSignatureV1(item.ID, item.Phase)
	case ContentToolCall:
		if item.Arguments != "" {
			output.Arguments = decodeJSONMap(item.Arguments)
		} else {
			output.Arguments = decodeJSONMap(block.partialJSON)
		}
	}
}

func openAIResponsesToolOutput(content []ContentBlock, model Model) any {
	text := textBlocksContent(content)
	hasImage := hasImageContent(content)
	if hasImage && ModelSupportsInput(model, InputImage) {
		parts := []openAIResponsesInputContent{}
		if text != "" {
			parts = append(parts, openAIResponsesInputContent{
				Type: "input_text",
				Text: text,
			})
		}
		for _, block := range content {
			if block.Type == ContentImage {
				parts = append(parts, openAIResponsesInputContent{
					Type:     "input_image",
					Detail:   "auto",
					ImageURL: fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.ImageData),
				})
			}
		}
		return parts
	}
	if text != "" {
		return text
	}
	if hasImage {
		return "(see attached image)"
	}
	return ""
}

type openAITextSignature struct {
	id    string
	phase string
}

func encodeOpenAITextSignatureV1(id string, phase string) string {
	if id == "" {
		return ""
	}
	payload := struct {
		Version int    `json:"v"`
		ID      string `json:"id"`
		Phase   string `json:"phase,omitempty"`
	}{
		Version: 1,
		ID:      id,
	}
	if phase == "commentary" || phase == "final_answer" {
		payload.Phase = phase
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return id
	}
	return string(data)
}

func parseOpenAITextSignature(signature string) openAITextSignature {
	if signature == "" {
		return openAITextSignature{}
	}
	if strings.HasPrefix(signature, "{") {
		var parsed struct {
			Version int    `json:"v"`
			ID      string `json:"id"`
			Phase   string `json:"phase"`
		}
		if err := json.Unmarshal([]byte(signature), &parsed); err == nil &&
			parsed.Version == 1 &&
			parsed.ID != "" {
			result := openAITextSignature{id: parsed.ID}
			if parsed.Phase == "commentary" || parsed.Phase == "final_answer" {
				result.phase = parsed.Phase
			}
			return result
		}
	}
	return openAITextSignature{id: signature}
}

func openAIMessageID(id string, fallbackIndex int) string {
	if id == "" {
		return fmt.Sprintf("msg_%d", fallbackIndex)
	}
	if len(id) > 64 {
		return "msg_" + shortHash(id)
	}
	return id
}

func shortHash(value string) string {
	var h1 uint32 = 0xdeadbeef
	var h2 uint32 = 0x41c6ce57
	for _, char := range value {
		ch := uint32(char)
		h1 = (h1 ^ ch) * 2654435761
		h2 = (h2 ^ ch) * 1597334677
	}
	h1 = ((h1 ^ (h1 >> 16)) * 2246822507) ^ ((h2 ^ (h2 >> 13)) * 3266489909)
	h2 = ((h2 ^ (h2 >> 16)) * 2246822507) ^ ((h1 ^ (h1 >> 13)) * 3266489909)
	return strconv.FormatUint(uint64(h2), 36) + strconv.FormatUint(uint64(h1), 36)
}

func applyFunctionCallArgumentsDone(
	current *openAIResponsesBlock,
	output *ContentBlock,
	arguments string,
) bool {
	previousPartialJSON := current.partialJSON
	current.partialJSON = arguments
	output.Arguments = decodeJSONMap(current.partialJSON)
	if arguments == "" || !strings.HasPrefix(arguments, previousPartialJSON) {
		return false
	}
	return len(arguments) > len(previousPartialJSON)
}

func mapOpenAIResponsesStopReason(status string) StopReason {
	switch status {
	case "", "completed", "in_progress", "queued":
		return StopReasonStop
	case "incomplete":
		return StopReasonLength
	case "failed", "cancelled":
		return StopReasonError
	default:
		return StopReasonStop
	}
}

func outputHasToolCall(output *Message) bool {
	for _, block := range output.Content {
		if block.Type == ContentToolCall {
			return true
		}
	}
	return false
}
