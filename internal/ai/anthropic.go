package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	anthropicparam "github.com/anthropics/anthropic-sdk-go/packages/param"
)

const anthropicFineGrainedToolStreamingBeta = "fine-grained-tool-streaming-2025-05-14"
const anthropicInterleavedThinkingBeta = "interleaved-thinking-2025-05-14"

type anthropicProvider struct {
	events iter.Seq2[anthropicStreamEvent, error]
	client *http.Client
}

func (p anthropicProvider) Stream(ctx context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error] {
	model, _ := modelFromOptions(opts)
	if p.events == nil {
		client := p.client
		if client == nil {
			client = http.DefaultClient
		}
		return streamAnthropicSDK(ctx, client, model, opts)
	}
	return streamAnthropicEvents(ctx, model, p.events)
}

type anthropicStreamEvent struct {
	Type         string
	MessageID    string
	Index        int
	Block        anthropicContentBlock
	Delta        anthropicDelta
	StopReason   string
	Usage        *Usage
	ErrorMessage string
}

type anthropicContentBlock struct {
	Type      string
	ID        string
	Name      string
	Input     map[string]any
	Data      string
	Redacted  bool
	Thinking  string
	Signature string
}

type anthropicDelta struct {
	Type        string
	Text        string
	Thinking    string
	PartialJSON string
	Signature   string
}

type anthropicBlock struct {
	ContentBlock
	providerIndex int
	partialJSON   string
}

type anthropicRequestBody struct {
	Model        string               `json:"model"`
	Messages     []anthropicMessage   `json:"messages"`
	MaxTokens    int                  `json:"max_tokens"`
	Stream       bool                 `json:"stream"`
	System       []anthropicTextBlock `json:"system,omitempty"`
	Tools        []anthropicTool      `json:"tools,omitempty"`
	Temperature  float64              `json:"temperature,omitempty"`
	Metadata     map[string]any       `json:"metadata,omitempty"`
	Thinking     map[string]any       `json:"thinking,omitempty"`
	OutputConfig map[string]any       `json:"output_config,omitempty"`
}

type anthropicTextBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type anthropicImageContentBlock struct {
	Type         string                 `json:"type"`
	Source       anthropicImageSource   `json:"source"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type anthropicThinkingContentBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type anthropicRedactedThinkingBlock struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type anthropicToolResultBlock struct {
	Type         string                 `json:"type"`
	ToolUseID    string                 `json:"tool_use_id"`
	Content      []any                  `json:"content"`
	IsError      bool                   `json:"is_error,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicTool struct {
	Name                string                 `json:"name"`
	Description         string                 `json:"description,omitempty"`
	InputSchema         map[string]any         `json:"input_schema"`
	EagerInputStreaming bool                   `json:"eager_input_streaming,omitempty"`
	CacheControl        *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type anthropicCompat struct {
	supportsEagerToolInputStreaming bool
	supportsLongCacheRetention      bool
	sendSessionAffinityHeaders      bool
	supportsCacheControlOnTools     bool
	forceAdaptiveThinking           bool
}

type anthropicUsageFields struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func streamAnthropicSDK(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		payload := any(buildAnthropicRequestBody(model, opts))
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

		params, err := anthropicParamsFromPayload(payload)
		if err != nil {
			yield(nil, err)
			return
		}

		var resp *http.Response
		sdk := anthropic.NewClient(anthropicOptions(client, model, opts, &resp)...)
		stream := sdk.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		if opts.OnResponse != nil && resp != nil {
			if err := opts.OnResponse(providerResponseFromHTTP(resp), model); err != nil {
				yield(nil, err)
				return
			}
		}

		if err := stream.Err(); err != nil {
			if event, ok := anthropicAPIErrorEvent(model, err); ok {
				yield(event, nil)
				return
			}
			yield(nil, err)
			return
		}

		events := streamAnthropicSDKEvents(stream)
		for event, err := range streamAnthropicEvents(ctx, model, events) {
			if !yield(event, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

func buildAnthropicRequestBody(model Model, opts *StreamOptions) anthropicRequestBody {
	compat := getAnthropicCompat(model)
	cacheControl := anthropicCacheControlFor(model, opts.CacheRetention)
	messages := TransformMessages(opts.Messages, model, normalizeAnthropicToolCallID)
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = model.MaxTokens
	}
	reasoning := reasoningEffort(model, opts.Reasoning)
	thinkingEnabled := model.Reasoning && reasoning != ""
	if thinkingEnabled && !compat.forceAdaptiveThinking {
		adjusted := adjustMaxTokensForThinking(
			opts.MaxTokens,
			model.MaxTokens,
			reasoning,
			opts.ThinkingBudgets,
		)
		maxTokens = adjusted.maxTokens
	}

	body := anthropicRequestBody{
		Model:     model.ID,
		Messages:  convertAnthropicMessages(messages, model, cacheControl),
		MaxTokens: maxTokens,
		Stream:    true,
		Tools: convertAnthropicTools(
			opts.Tools,
			compat.supportsEagerToolInputStreaming,
			anthropicToolCacheControl(cacheControl, compat),
		),
		Metadata: anthropicMetadata(opts.Metadata),
	}
	if opts.Temperature != 0 && !thinkingEnabled {
		body.Temperature = opts.Temperature
	}
	if opts.SystemPrompt != "" {
		body.System = []anthropicTextBlock{{
			Type:         "text",
			Text:         opts.SystemPrompt,
			CacheControl: cacheControl,
		}}
	}
	applyAnthropicThinking(model, opts, compat, reasoning, &body)
	return body
}

func anthropicCacheControlFor(model Model, retention CacheRetention) *anthropicCacheControl {
	retention = normalizeCacheRetention(retention)
	if retention == CacheRetentionNone {
		return nil
	}
	cacheControl := &anthropicCacheControl{Type: "ephemeral"}
	if retention == CacheRetentionLong && getAnthropicCompat(model).supportsLongCacheRetention {
		cacheControl.TTL = "1h"
	}
	return cacheControl
}

func anthropicToolCacheControl(
	cacheControl *anthropicCacheControl,
	compat anthropicCompat,
) *anthropicCacheControl {
	if !compat.supportsCacheControlOnTools {
		return nil
	}
	return cacheControl
}

func getAnthropicCompat(model Model) anthropicCompat {
	isFireworks := model.Provider == "fireworks"
	isCloudflareAnthropic := model.Provider == "cloudflare-ai-gateway" &&
		strings.Contains(model.BaseURL, "anthropic")
	compat := anthropicCompat{
		supportsEagerToolInputStreaming: !isFireworks,
		supportsLongCacheRetention:      !isFireworks,
		sendSessionAffinityHeaders:      isFireworks || isCloudflareAnthropic,
		supportsCacheControlOnTools:     !isFireworks,
	}
	if model.Compat == nil {
		return compat
	}
	compat.supportsEagerToolInputStreaming = boolCompat(
		model.Compat.SupportsEagerToolInputStreaming,
		compat.supportsEagerToolInputStreaming,
	)
	compat.supportsLongCacheRetention = boolCompat(
		model.Compat.SupportsLongCacheRetention,
		compat.supportsLongCacheRetention,
	)
	compat.sendSessionAffinityHeaders = boolCompat(
		model.Compat.SendSessionAffinityHeaders,
		compat.sendSessionAffinityHeaders,
	)
	compat.supportsCacheControlOnTools = boolCompat(
		model.Compat.SupportsCacheControlOnTools,
		compat.supportsCacheControlOnTools,
	)
	compat.forceAdaptiveThinking = boolCompat(
		model.Compat.ForceAdaptiveThinking,
		false,
	)
	return compat
}

func applyAnthropicThinking(
	model Model,
	opts *StreamOptions,
	compat anthropicCompat,
	reasoning string,
	body *anthropicRequestBody,
) {
	if !model.Reasoning {
		return
	}
	if reasoning == "" {
		body.Thinking = map[string]any{"type": "disabled"}
		return
	}

	if compat.forceAdaptiveThinking {
		body.Thinking = map[string]any{
			"type":    "adaptive",
			"display": "summarized",
		}
		body.OutputConfig = map[string]any{
			"effort": anthropicEffortFor(model, reasoning),
		}
		return
	}

	adjusted := adjustMaxTokensForThinking(
		opts.MaxTokens,
		model.MaxTokens,
		reasoning,
		opts.ThinkingBudgets,
	)
	body.Thinking = map[string]any{
		"type":          "enabled",
		"budget_tokens": adjusted.thinkingBudget,
		"display":       "summarized",
	}
}

type anthropicThinkingAdjustment struct {
	maxTokens      int
	thinkingBudget int
}

func adjustMaxTokensForThinking(
	baseMaxTokens int,
	modelMaxTokens int,
	reasoning string,
	custom *ThinkingBudgets,
) anthropicThinkingAdjustment {
	budget := anthropicThinkingBudget(reasoning, custom)
	maxTokens := modelMaxTokens
	if maxTokens <= 0 {
		maxTokens = baseMaxTokens
	}
	if baseMaxTokens > 0 && modelMaxTokens > 0 && baseMaxTokens+budget < modelMaxTokens {
		maxTokens = baseMaxTokens + budget
	}
	if maxTokens <= 0 {
		maxTokens = budget + 1024
	}
	if maxTokens <= budget {
		budget = max(0, maxTokens-1024)
	}
	return anthropicThinkingAdjustment{
		maxTokens:      maxTokens,
		thinkingBudget: budget,
	}
}

func anthropicThinkingBudget(reasoning string, custom *ThinkingBudgets) int {
	budgets := ThinkingBudgets{
		Minimal: 1024,
		Low:     2048,
		Medium:  8192,
		High:    16384,
	}
	if custom != nil {
		if custom.Minimal > 0 {
			budgets.Minimal = custom.Minimal
		}
		if custom.Low > 0 {
			budgets.Low = custom.Low
		}
		if custom.Medium > 0 {
			budgets.Medium = custom.Medium
		}
		if custom.High > 0 {
			budgets.High = custom.High
		}
	}
	switch reasoning {
	case "minimal":
		return budgets.Minimal
	case "low":
		return budgets.Low
	case "medium":
		return budgets.Medium
	default:
		return budgets.High
	}
}

func anthropicEffortFor(model Model, reasoning string) string {
	mapped := mappedThinkingLevel(model, reasoning)
	switch mapped {
	case "minimal", "low":
		return "low"
	case "medium", "high", "xhigh", "max":
		return mapped
	default:
		return "high"
	}
}

func anthropicMetadata(metadata map[string]any) map[string]any {
	userID, ok := metadata["user_id"].(string)
	if !ok || userID == "" {
		return nil
	}
	return map[string]any{"user_id": userID}
}

func convertAnthropicTools(
	tools []Tool,
	eagerInputStreaming bool,
	cacheControl *anthropicCacheControl,
) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}

	result := make([]anthropicTool, 0, len(tools))
	for i, tool := range tools {
		converted := anthropicTool{
			Name:                tool.Name,
			Description:         tool.Description,
			InputSchema:         anthropicToolInputSchema(tool.Parameters),
			EagerInputStreaming: eagerInputStreaming,
		}
		if cacheControl != nil && i == len(tools)-1 {
			converted.CacheControl = cacheControl
		}
		result = append(result, converted)
	}
	return result
}

func anthropicToolInputSchema(parameters map[string]any) map[string]any {
	schema := make(map[string]any, len(parameters)+3)
	maps.Copy(schema, parameters)
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	if _, ok := schema["required"]; !ok {
		schema["required"] = []string{}
	}
	return schema
}

func convertAnthropicMessages(
	messages []Message,
	model Model,
	cacheControl *anthropicCacheControl,
) []anthropicMessage {
	result := make([]anthropicMessage, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		message := messages[i]
		switch message.Role {
		case RoleUser:
			content := convertAnthropicUserContent(message.Content, model)
			if len(content) == 0 {
				continue
			}
			result = append(result, anthropicMessage{
				Role:    "user",
				Content: content,
			})
		case RoleAssistant:
			content := convertAnthropicAssistantContent(message.Content)
			if len(content) == 0 {
				continue
			}
			result = append(result, anthropicMessage{
				Role:    "assistant",
				Content: content,
			})
		case RoleToolResult:
			content := []any{convertAnthropicToolResult(message, model)}
			j := i + 1
			for j < len(messages) && messages[j].Role == RoleToolResult {
				content = append(content, convertAnthropicToolResult(messages[j], model))
				j++
			}
			i = j - 1
			result = append(result, anthropicMessage{
				Role:    "user",
				Content: content,
			})
		}
	}
	addAnthropicCacheControl(result, cacheControl)
	return result
}

func convertAnthropicUserContent(content []ContentBlock, model Model) []any {
	result := make([]any, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case ContentText:
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			result = append(result, anthropicTextBlock{
				Type: "text",
				Text: block.Text,
			})
		case ContentImage:
			if ModelSupportsInput(model, InputImage) {
				result = append(result, anthropicImageBlock(block))
			}
		}
	}
	return result
}

func convertAnthropicAssistantContent(content []ContentBlock) []any {
	result := []any{}
	for _, block := range content {
		switch block.Type {
		case ContentText:
			if strings.TrimSpace(block.Text) != "" {
				result = append(result, anthropicTextBlock{
					Type: "text",
					Text: block.Text,
				})
			}
		case ContentThinking:
			result = append(result, convertAnthropicThinkingBlock(block)...)
		case ContentToolCall:
			result = append(result, anthropicToolUseBlock{
				Type:  "tool_use",
				ID:    block.ToolCallID,
				Name:  block.ToolName,
				Input: block.Arguments,
			})
		}
	}
	return result
}

func convertAnthropicThinkingBlock(block ContentBlock) []any {
	if block.Redacted {
		return []any{anthropicRedactedThinkingBlock{
			Type: "redacted_thinking",
			Data: block.ThinkingSignature,
		}}
	}
	if strings.TrimSpace(block.Thinking) == "" {
		return nil
	}
	if strings.TrimSpace(block.ThinkingSignature) == "" {
		return []any{anthropicTextBlock{
			Type: "text",
			Text: block.Thinking,
		}}
	}
	return []any{anthropicThinkingContentBlock{
		Type:      "thinking",
		Thinking:  block.Thinking,
		Signature: block.ThinkingSignature,
	}}
}

func convertAnthropicToolResult(message Message, model Model) anthropicToolResultBlock {
	return anthropicToolResultBlock{
		Type:      "tool_result",
		ToolUseID: message.ToolCallID,
		Content:   convertAnthropicToolResultContent(message.Content, model),
		IsError:   message.IsError,
	}
}

func convertAnthropicToolResultContent(content []ContentBlock, model Model) []any {
	result := make([]any, 0, len(content))
	hasText := false
	for _, block := range content {
		switch block.Type {
		case ContentText:
			result = append(result, anthropicTextBlock{
				Type: "text",
				Text: block.Text,
			})
			hasText = true
		case ContentImage:
			if ModelSupportsInput(model, InputImage) {
				result = append(result, anthropicImageBlock(block))
			}
		}
	}
	if len(result) > 0 && !hasText {
		result = append([]any{anthropicTextBlock{
			Type: "text",
			Text: "(see attached image)",
		}}, result...)
	}
	return result
}

func anthropicImageBlock(block ContentBlock) anthropicImageContentBlock {
	return anthropicImageContentBlock{
		Type: "image",
		Source: anthropicImageSource{
			Type:      "base64",
			MediaType: block.MimeType,
			Data:      block.ImageData,
		},
	}
}

func addAnthropicCacheControl(messages []anthropicMessage, cacheControl *anthropicCacheControl) {
	if cacheControl == nil || len(messages) == 0 {
		return
	}
	last := &messages[len(messages)-1]
	if last.Role != "user" {
		return
	}
	if len(last.Content) == 0 {
		return
	}
	i := len(last.Content) - 1
	switch block := last.Content[i].(type) {
	case anthropicTextBlock:
		block.CacheControl = cacheControl
		last.Content[i] = block
	case anthropicImageContentBlock:
		block.CacheControl = cacheControl
		last.Content[i] = block
	case anthropicToolResultBlock:
		block.CacheControl = cacheControl
		last.Content[i] = block
	}
}

func anthropicParamsFromPayload(payload any) (anthropic.MessageNewParams, error) {
	switch value := payload.(type) {
	case anthropic.MessageNewParams:
		return value, nil
	case *anthropic.MessageNewParams:
		if value == nil {
			return anthropic.MessageNewParams{}, errors.New("anthropic messages payload is nil")
		}
		return *value, nil
	case json.RawMessage:
		return anthropicparam.Override[anthropic.MessageNewParams](value), nil
	case []byte:
		return anthropicparam.Override[anthropic.MessageNewParams](json.RawMessage(value)), nil
	case string:
		return anthropicparam.Override[anthropic.MessageNewParams](json.RawMessage(value)), nil
	default:
		data, err := json.Marshal(payload)
		if err != nil {
			return anthropic.MessageNewParams{}, fmt.Errorf("marshal anthropic request: %w", err)
		}
		return anthropicparam.Override[anthropic.MessageNewParams](json.RawMessage(data)), nil
	}
}

func anthropicOptions(
	client *http.Client,
	model Model,
	opts *StreamOptions,
	resp **http.Response,
) []anthropicoption.RequestOption {
	requestOptions := []anthropicoption.RequestOption{
		anthropicoption.WithHTTPClient(client),
		anthropicoption.WithResponseInto(resp),
	}
	if retries, ok := sdkMaxRetries(opts.MaxRetries); ok {
		requestOptions = append(requestOptions, anthropicoption.WithMaxRetries(retries))
	}
	if model.BaseURL != "" {
		requestOptions = append(requestOptions, anthropicoption.WithBaseURL(model.BaseURL))
	}
	if opts.APIKey != "" && !model.AuthHeader {
		requestOptions = append(requestOptions, anthropicoption.WithAPIKey(opts.APIKey))
	}
	if opts.Timeout > 0 {
		requestOptions = append(requestOptions, anthropicoption.WithRequestTimeout(opts.Timeout))
	}
	for _, beta := range anthropicBetaFeatures(model, opts) {
		requestOptions = append(requestOptions, anthropicoption.WithHeader("anthropic-beta", beta))
	}
	if opts.SessionID != "" && getAnthropicCompat(model).sendSessionAffinityHeaders {
		requestOptions = append(
			requestOptions,
			anthropicoption.WithHeader("x-session-affinity", opts.SessionID),
		)
	}
	for key, value := range model.Headers {
		requestOptions = append(requestOptions, anthropicoption.WithHeader(key, value))
	}
	for key, value := range opts.Headers {
		requestOptions = append(requestOptions, anthropicoption.WithHeader(key, value))
	}
	if opts.APIKey != "" && model.AuthHeader {
		requestOptions = append(requestOptions, anthropicoption.WithAuthToken(opts.APIKey))
	}
	return requestOptions
}

func anthropicBetaFeatures(model Model, opts *StreamOptions) []string {
	features := []string{}
	compat := getAnthropicCompat(model)
	if len(opts.Tools) > 0 && !compat.supportsEagerToolInputStreaming {
		features = append(features, anthropicFineGrainedToolStreamingBeta)
	}
	// pi defaults interleavedThinking to true regardless of whether reasoning
	// was requested; only adaptive-thinking models skip the beta header
	// (anthropic.ts:498,788-795).
	if !compat.forceAdaptiveThinking {
		features = append(features, anthropicInterleavedThinkingBeta)
	}
	if len(features) == 0 {
		return nil
	}
	return []string{strings.Join(features, ",")}
}

func streamAnthropicSDKEvents(
	stream interface {
		Next() bool
		Current() anthropic.MessageStreamEventUnion
		Err() error
	},
) iter.Seq2[anthropicStreamEvent, error] {
	return func(yield func(anthropicStreamEvent, error) bool) {
		for stream.Next() {
			event, ok, err := decodeAnthropicEventData([]byte(stream.Current().RawJSON()))
			if err != nil {
				yield(anthropicStreamEvent{}, err)
				return
			}
			if ok && !yield(event, nil) {
				return
			}
		}
		if err := stream.Err(); err != nil {
			if message, ok := anthropicAPIErrorMessage(err); ok {
				yield(anthropicStreamEvent{Type: "error", ErrorMessage: message}, nil)
				return
			}
			yield(anthropicStreamEvent{}, err)
		}
	}
}

func decodeAnthropicEventData(data []byte) (anthropicStreamEvent, bool, error) {
	var raw struct {
		Type         string          `json:"type"`
		Message      json.RawMessage `json:"message"`
		Index        int             `json:"index"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        json.RawMessage `json:"delta"`
		Usage        json.RawMessage `json:"usage"`
		Error        struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return anthropicStreamEvent{}, false, fmt.Errorf("decode anthropic event: %w", err)
	}
	if raw.Type == "" {
		return anthropicStreamEvent{}, false, nil
	}

	event := anthropicStreamEvent{
		Type:         raw.Type,
		Index:        raw.Index,
		ErrorMessage: raw.Error.Message,
	}
	if raw.Type == "message_start" && len(raw.Message) > 0 {
		messageID, usage, err := decodeAnthropicMessage(raw.Message)
		if err != nil {
			return anthropicStreamEvent{}, false, err
		}
		event.MessageID = messageID
		event.Usage = usage
	}
	if raw.Type == "content_block_start" && len(raw.ContentBlock) > 0 {
		block, err := decodeAnthropicContentBlock(raw.ContentBlock)
		if err != nil {
			return anthropicStreamEvent{}, false, err
		}
		event.Block = block
	}
	if raw.Type == "content_block_delta" && len(raw.Delta) > 0 {
		delta, err := decodeAnthropicDelta(raw.Delta)
		if err != nil {
			return anthropicStreamEvent{}, false, err
		}
		event.Delta = delta
	}
	if raw.Type == "message_delta" {
		stopReason, err := decodeAnthropicStopReason(raw.Delta)
		if err != nil {
			return anthropicStreamEvent{}, false, err
		}
		event.StopReason = stopReason
		if len(raw.Usage) > 0 {
			usage, err := decodeAnthropicUsage(raw.Usage)
			if err != nil {
				return anthropicStreamEvent{}, false, err
			}
			event.Usage = usage
		}
	}
	return event, true, nil
}

func decodeAnthropicMessage(data []byte) (string, *Usage, error) {
	var raw struct {
		ID    string          `json:"id"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", nil, fmt.Errorf("decode anthropic message: %w", err)
	}
	var usage *Usage
	if len(raw.Usage) > 0 {
		decoded, err := decodeAnthropicUsage(raw.Usage)
		if err != nil {
			return "", nil, err
		}
		usage = decoded
	}
	return raw.ID, usage, nil
}

func decodeAnthropicContentBlock(data []byte) (anthropicContentBlock, error) {
	var raw anthropicContentBlock
	if err := json.Unmarshal(data, &raw); err != nil {
		return anthropicContentBlock{}, fmt.Errorf("decode anthropic content block: %w", err)
	}
	return raw, nil
}

func decodeAnthropicDelta(data []byte) (anthropicDelta, error) {
	var raw struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return anthropicDelta{}, fmt.Errorf("decode anthropic delta: %w", err)
	}
	return anthropicDelta{
		Type:        raw.Type,
		Text:        raw.Text,
		Thinking:    raw.Thinking,
		PartialJSON: raw.PartialJSON,
		Signature:   raw.Signature,
	}, nil
}

func decodeAnthropicStopReason(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	var raw struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("decode anthropic message delta: %w", err)
	}
	return raw.StopReason, nil
}

func decodeAnthropicUsage(data []byte) (*Usage, error) {
	var raw anthropicUsageFields
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode anthropic usage: %w", err)
	}
	return &Usage{
		Input:       raw.InputTokens,
		Output:      raw.OutputTokens,
		CacheRead:   raw.CacheReadInputTokens,
		CacheWrite:  raw.CacheCreationInputTokens,
		TotalTokens: raw.InputTokens + raw.OutputTokens + raw.CacheReadInputTokens + raw.CacheCreationInputTokens,
		Cost:        &Cost{},
	}, nil
}

func anthropicAPIErrorEvent(model Model, err error) (*StreamEvent, bool) {
	message, ok := anthropicAPIErrorMessage(err)
	if !ok {
		return nil, false
	}
	return errorMessageEvent(model, &APIError{
		API:     model.API,
		Model:   model.ID,
		Message: message,
	}), true
}

func anthropicAPIErrorMessage(err error) (string, bool) {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return "", false
	}
	message := anthropicErrorMessage(apiErr)
	if message == "" {
		message = err.Error()
	}
	if apiErr.StatusCode >= http.StatusBadRequest {
		message = fmt.Sprintf("Anthropic API error (%d): %s", apiErr.StatusCode, message)
	}
	return message, true
}

func anthropicErrorMessage(apiErr *anthropic.Error) string {
	var raw struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(apiErr.RawJSON()), &raw); err != nil {
		return ""
	}
	return raw.Error.Message
}

func normalizeAnthropicToolCallID(id string, _ Model, _ Message) string {
	var builder strings.Builder
	for _, char := range id {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= 'A' && char <= 'Z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '_' || char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return builder.String()
}

func streamAnthropicEvents(
	ctx context.Context,
	model Model,
	events iter.Seq2[anthropicStreamEvent, error],
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		output := newAssistantMessage(model)
		blocks := []anthropicBlock{}

		for event, err := range events {
			if err != nil {
				yield(nil, err)
				return
			}
			select {
			case <-ctx.Done():
				output.StopReason = StopReasonAborted
				output.ErrorMessage = ctx.Err().Error()
				yield(&StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}, nil)
				return
			default:
			}

			switch event.Type {
			case "message_start":
				output.ResponseID = event.MessageID
				if event.Usage != nil {
					output.Usage = usageWithTotals(event.Usage)
					CalculateCost(model, output.Usage)
				}
				if !yield(&StreamEvent{Type: EventMessageStart, Message: output}, nil) {
					return
				}
			case "content_block_start":
				block := anthropicStartBlock(event)
				blocks = append(blocks, block)
				output.Content = append(output.Content, block.ContentBlock)
				index := len(output.Content) - 1
				startType := contentBlockStartEvent(output.Content[index].Type)
				if !yield(&StreamEvent{Type: startType, Index: index, Delta: &output.Content[index], Message: output}, nil) {
					return
				}
			case "content_block_delta":
				idx := findAnthropicBlock(blocks, event.Index)
				if idx < 0 {
					continue
				}
				changed, delta := applyAnthropicDelta(&blocks[idx], &output.Content[idx], event.Delta)
				if changed {
					deltaType := contentBlockDeltaEvent(output.Content[idx].Type)
					if !yield(&StreamEvent{Type: deltaType, Index: idx, Delta: delta, Message: output}, nil) {
						return
					}
				}
			case "content_block_stop":
				idx := findAnthropicBlock(blocks, event.Index)
				if idx < 0 {
					continue
				}
				finalizeAnthropicBlock(&blocks[idx], &output.Content[idx])
				endType := contentBlockEndEvent(output.Content[idx].Type)
				if !yield(&StreamEvent{Type: endType, Index: idx, Delta: &output.Content[idx], Message: output}, nil) {
					return
				}
			case "message_delta":
				if event.StopReason != "" {
					output.StopReason = mapAnthropicStopReason(event.StopReason)
					if output.StopReason == StopReasonError && output.ErrorMessage == "" {
						output.ErrorMessage = fmt.Sprintf("anthropic stop reason: %s", event.StopReason)
					}
				}
				if event.Usage != nil {
					output.Usage = mergeAnthropicUsage(output.Usage, event.Usage)
					CalculateCost(model, output.Usage)
				}
			case "error":
				output.StopReason = StopReasonError
				output.ErrorMessage = event.ErrorMessage
				if output.ErrorMessage == "" {
					output.ErrorMessage = "anthropic stream error"
				}
				if !yield(&StreamEvent{Type: EventMessageError, Message: output, Usage: output.Usage}, nil) {
					return
				}
				yield(&StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}, nil)
				return
			default:
				yield(nil, fmt.Errorf("anthropic: unknown stream event %q", event.Type))
				return
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

func mergeAnthropicUsage(existing *Usage, update *Usage) *Usage {
	next := usageWithTotals(existing)
	if update == nil {
		return next
	}
	if update.Input != 0 {
		next.Input = update.Input
	}
	if update.Output != 0 {
		next.Output = update.Output
	}
	if update.CacheRead != 0 {
		next.CacheRead = update.CacheRead
	}
	if update.CacheWrite != 0 {
		next.CacheWrite = update.CacheWrite
	}
	next.TotalTokens = next.Input + next.Output + next.CacheRead + next.CacheWrite
	return next
}

func anthropicStartBlock(event anthropicStreamEvent) anthropicBlock {
	block := anthropicBlock{providerIndex: event.Index}
	switch event.Block.Type {
	case "text":
		block.ContentBlock = ContentBlock{Type: ContentText}
	case "thinking":
		block.ContentBlock = ContentBlock{
			Type:              ContentThinking,
			Thinking:          event.Block.Thinking,
			ThinkingSignature: event.Block.Signature,
		}
	case "redacted_thinking":
		block.ContentBlock = ContentBlock{
			Type:              ContentThinking,
			Thinking:          "[Reasoning redacted]",
			ThinkingSignature: event.Block.Data,
			Redacted:          true,
		}
	case "tool_use":
		block.ContentBlock = ContentBlock{
			Type:       ContentToolCall,
			ToolCallID: event.Block.ID,
			ToolName:   event.Block.Name,
			Arguments:  event.Block.Input,
		}
	default:
		block.ContentBlock = ContentBlock{Type: ContentText}
	}
	return block
}

func applyAnthropicDelta(block *anthropicBlock, output *ContentBlock, delta anthropicDelta) (bool, *ContentBlock) {
	switch delta.Type {
	case "text_delta":
		if output.Type != ContentText {
			return false, nil
		}
		output.Text += delta.Text
		return true, &ContentBlock{Type: ContentText, Text: delta.Text}
	case "thinking_delta":
		if output.Type != ContentThinking {
			return false, nil
		}
		output.Thinking += delta.Thinking
		return true, &ContentBlock{Type: ContentThinking, Thinking: delta.Thinking}
	case "signature_delta":
		if output.Type == ContentThinking {
			output.ThinkingSignature += delta.Signature
		}
		return false, nil
	case "input_json_delta":
		if output.Type != ContentToolCall {
			return false, nil
		}
		block.partialJSON += delta.PartialJSON
		output.Arguments = decodeJSONMap(block.partialJSON)
		return true, &ContentBlock{Type: ContentToolCall, Arguments: output.Arguments}
	default:
		return false, nil
	}
}

func finalizeAnthropicBlock(block *anthropicBlock, output *ContentBlock) {
	if output.Type == ContentToolCall {
		output.Arguments = decodeJSONMap(block.partialJSON)
	}
}

func findAnthropicBlock(blocks []anthropicBlock, providerIndex int) int {
	for i, block := range blocks {
		if block.providerIndex == providerIndex {
			return i
		}
	}
	return -1
}

func mapAnthropicStopReason(reason string) StopReason {
	switch reason {
	case "end_turn", "stop_sequence", "stop":
		return StopReasonStop
	case "max_tokens", "length":
		return StopReasonLength
	case "tool_use", "toolUse":
		return StopReasonToolUse
	// pi: "Stop is good enough -> resubmit" (anthropic.ts:1202).
	case "pause_turn":
		return StopReasonStop
	case "error", "refusal", "sensitive":
		return StopReasonError
	default:
		return StopReasonError
	}
}
