package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/ai/sseparse"
)

const defaultOpenAICompletionsBaseURL = "https://api.openai.com/v1"

type openAICompletionsProvider struct {
	events iter.Seq2[openAICompletionsStreamEvent, error]
	client *http.Client
}

func (p openAICompletionsProvider) Stream(ctx context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error] {
	model, _ := modelFromOptions(opts)
	if p.events == nil {
		client := p.client
		if client == nil {
			client = http.DefaultClient
		}
		return streamOpenAICompletionsHTTP(ctx, client, model, opts)
	}
	return streamOpenAICompletionsEvents(ctx, model, p.events)
}

type openAICompletionsRequestBody map[string]any
type openAICompletionsMessage map[string]any
type openAICompletionsTool map[string]any
type openAICompletionsContentPart map[string]any

type openAICompletionsCompat struct {
	supportsStore                               bool
	supportsDeveloperRole                       bool
	supportsReasoningEffort                     bool
	supportsUsageInStreaming                    bool
	requiresToolResultName                      bool
	requiresAssistantAfterToolResult            bool
	requiresThinkingAsText                      bool
	requiresReasoningContentOnAssistantMessages bool
	zaiToolStream                               bool
	supportsStrictMode                          bool
	sendSessionAffinityHeaders                  bool
	supportsLongCacheRetention                  bool
	maxTokensField                              string
	thinkingFormat                              string
	cacheControlFormat                          string
}

type openAICompletionsCompatCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type openAICompletionsStreamEvent struct {
	Type             string
	ResponseID       string
	ResponseModel    string
	ContentDelta     string
	ReasoningDelta   string
	ReasoningField   string
	ToolCalls        []openAICompletionsToolCallDelta
	ReasoningDetails []openAICompletionsReasoningDetail
	FinishReason     string
	HasFinishReason  bool
	Usage            *Usage
	ErrorMessage     string
}

type openAICompletionsToolCallDelta struct {
	Index    int
	HasIndex bool
	ID       string
	Name     string
	Args     string
}

type openAICompletionsReasoningDetail struct {
	ID  string
	Raw string
}

type openAICompletionsBlock struct {
	ContentBlock
	index          int
	streamIndex    int
	hasStreamIndex bool
	partialJSON    string
}

func streamOpenAICompletionsHTTP(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		streamCtx, cancel := openAICompletionsStreamContext(ctx, opts)
		defer cancel()

		payload := any(buildOpenAICompletionsRequestBody(model, opts))
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
			yield(nil, fmt.Errorf("marshal openai completions request: %w", err))
			return
		}
		resp, err := doOpenAICompletionsRequest(streamCtx, client, model, opts, data)
		if err != nil {
			if streamCtx.Err() != nil {
				yield(openAICompletionsAbortedEvent(model, streamCtx.Err()), nil)
				return
			}
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		if opts.OnResponse != nil {
			if err := opts.OnResponse(providerResponseFromHTTP(resp), model); err != nil {
				yield(nil, err)
				return
			}
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			yield(openAICompletionsAPIErrorEvent(model, resp.StatusCode, body), nil)
			return
		}

		events := parseOpenAICompletionsSSE(resp.Body)
		for event, err := range streamOpenAICompletionsEvents(streamCtx, model, events) {
			if !yield(event, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

func doOpenAICompletionsRequest(
	ctx context.Context,
	client *http.Client,
	model Model,
	opts *StreamOptions,
	data []byte,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		resolveOpenAICompletionsURL(model.BaseURL),
		bytes.NewReader(data),
	)
	if err != nil {
		return nil, err
	}
	req.Header = buildOpenAICompletionsHeaders(model, opts)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	return client.Do(req)
}

func openAICompletionsStreamContext(ctx context.Context, opts *StreamOptions) (context.Context, context.CancelFunc) {
	if opts.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, opts.Timeout)
}

func openAICompletionsAbortedEvent(model Model, err error) *StreamEvent {
	output := newAssistantMessage(model)
	output.StopReason = StopReasonAborted
	output.ErrorMessage = err.Error()
	return &StreamEvent{Type: EventMessageComplete, Message: output, Usage: output.Usage}
}

func buildOpenAICompletionsRequestBody(model Model, opts *StreamOptions) openAICompletionsRequestBody {
	compat := getOpenAICompletionsCompat(model)
	messages := TransformMessages(opts.Messages, model, normalizeOpenAICompletionsToolCallID)
	params := convertOpenAICompletionsMessages(model, opts.SystemPrompt, messages, compat)
	tools := openAICompletionsToolsFor(opts.Tools, messages, compat)
	cacheRetention := normalizeCacheRetention(opts.CacheRetention)

	if cacheControl := openAICompletionsCacheControlFor(compat, cacheRetention); cacheControl != nil {
		applyOpenAICompletionsAnthropicCacheControl(params, tools, cacheControl)
	}

	// Mirrors pi openai-completions buildParams request core:
	// .agents/references/pi/packages/ai/src/providers/openai-completions.ts:513
	body := openAICompletionsRequestBody{
		"model":    model.ID,
		"messages": params,
		"stream":   true,
	}
	if tools != nil {
		body["tools"] = tools
	}
	applyOpenAICompletionsCompatParams(body, model, opts, compat, cacheRetention, tools)
	return body
}

func applyOpenAICompletionsCompatParams(
	body openAICompletionsRequestBody,
	model Model,
	opts *StreamOptions,
	compat openAICompletionsCompat,
	cacheRetention CacheRetention,
	tools []openAICompletionsTool,
) {
	// Stream usage/store/max-token fields follow pi:
	// .agents/references/pi/packages/ai/src/providers/openai-completions.ts:525
	if compat.supportsUsageInStreaming {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	if compat.supportsStore {
		body["store"] = false
	}
	if opts.MaxTokens > 0 {
		body[compat.maxTokensField] = opts.MaxTokens
	}
	if opts.Temperature != 0 {
		body["temperature"] = opts.Temperature
	}
	if len(tools) > 0 && compat.zaiToolStream {
		body["tool_stream"] = true
	}
	applyOpenAICompletionsCacheParams(body, model, opts, compat, cacheRetention)
	applyOpenAICompletionsThinkingParams(
		body,
		model,
		compat,
		reasoningEffort(model, opts.Reasoning),
	)
	applyOpenAICompletionsRoutingParams(body, model)
}

func applyOpenAICompletionsCacheParams(
	body openAICompletionsRequestBody,
	model Model,
	opts *StreamOptions,
	compat openAICompletionsCompat,
	cacheRetention CacheRetention,
) {
	if cacheRetention == CacheRetentionNone || opts.SessionID == "" {
		return
	}
	usePromptCacheKey := strings.Contains(model.BaseURL, "api.openai.com") ||
		(cacheRetention == CacheRetentionLong && compat.supportsLongCacheRetention)
	if usePromptCacheKey {
		if key := clampOpenAICompletionsPromptCacheKey(opts.SessionID); key != "" {
			body["prompt_cache_key"] = key
		}
	}
	if cacheRetention == CacheRetentionLong && compat.supportsLongCacheRetention {
		body["prompt_cache_retention"] = "24h"
	}
}

func applyOpenAICompletionsThinkingParams(
	body openAICompletionsRequestBody,
	model Model,
	compat openAICompletionsCompat,
	reasoningEffort string,
) {
	if !model.Reasoning {
		return
	}
	// Thinking-format branches mirror pi's provider-specific compat mapping:
	// .agents/references/pi/packages/ai/src/providers/openai-completions.ts:563
	switch compat.thinkingFormat {
	case "zai", "qwen":
		body["enable_thinking"] = reasoningEffort != ""
	case "qwen-chat-template":
		body["chat_template_kwargs"] = map[string]any{
			"enable_thinking":   reasoningEffort != "",
			"preserve_thinking": true,
		}
	case "deepseek":
		body["thinking"] = map[string]string{"type": "disabled"}
		if reasoningEffort != "" {
			body["thinking"] = map[string]string{"type": "enabled"}
			body["reasoning_effort"] = mappedThinkingLevel(model, reasoningEffort)
		}
	case "openrouter":
		if reasoningEffort != "" {
			body["reasoning"] = map[string]string{"effort": mappedThinkingLevel(model, reasoningEffort)}
		} else if off, ok := model.ThinkingLevelMap["off"]; ok && off != nil {
			body["reasoning"] = map[string]string{"effort": *off}
		}
	case "together":
		body["reasoning"] = map[string]bool{"enabled": reasoningEffort != ""}
		if reasoningEffort != "" && compat.supportsReasoningEffort {
			body["reasoning_effort"] = mappedThinkingLevel(model, reasoningEffort)
		}
	default:
		applyOpenAICompletionsDefaultThinking(body, model, compat, reasoningEffort)
	}
}

func applyOpenAICompletionsDefaultThinking(
	body openAICompletionsRequestBody,
	model Model,
	compat openAICompletionsCompat,
	reasoningEffort string,
) {
	if !compat.supportsReasoningEffort {
		return
	}
	if reasoningEffort != "" {
		body["reasoning_effort"] = mappedThinkingLevel(model, reasoningEffort)
		return
	}
	if off, ok := model.ThinkingLevelMap["off"]; ok && off != nil {
		body["reasoning_effort"] = *off
	}
}

func applyOpenAICompletionsRoutingParams(body openAICompletionsRequestBody, model Model) {
	if model.Compat == nil {
		return
	}
	if strings.Contains(model.BaseURL, "openrouter.ai") && model.Compat.OpenRouterRouting != nil {
		body["provider"] = model.Compat.OpenRouterRouting
	}
	if strings.Contains(model.BaseURL, "ai-gateway.vercel.sh") {
		applyVercelGatewayRouting(body, model.Compat.VercelGatewayRouting)
	}
}

func applyVercelGatewayRouting(body openAICompletionsRequestBody, routing *VercelGatewayRouting) {
	if routing == nil || (len(routing.Only) == 0 && len(routing.Order) == 0) {
		return
	}
	gateway := map[string][]string{}
	if len(routing.Only) > 0 {
		gateway["only"] = routing.Only
	}
	if len(routing.Order) > 0 {
		gateway["order"] = routing.Order
	}
	body["providerOptions"] = map[string]any{"gateway": gateway}
}

func mappedThinkingLevel(model Model, level string) string {
	if mapped, ok := model.ThinkingLevelMap[level]; ok && mapped != nil {
		return *mapped
	}
	return level
}

func openAICompletionsToolsFor(
	tools []Tool,
	messages []Message,
	compat openAICompletionsCompat,
) []openAICompletionsTool {
	if len(tools) > 0 {
		return convertOpenAICompletionsTools(tools, compat)
	}
	if hasOpenAICompletionsToolHistory(messages) {
		return []openAICompletionsTool{}
	}
	return nil
}

func convertOpenAICompletionsTools(tools []Tool, compat openAICompletionsCompat) []openAICompletionsTool {
	result := make([]openAICompletionsTool, 0, len(tools))
	for _, tool := range tools {
		function := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
		}
		if compat.supportsStrictMode {
			function["strict"] = false
		}
		result = append(result, openAICompletionsTool{
			"type":     "function",
			"function": function,
		})
	}
	return result
}

func convertOpenAICompletionsMessages(
	model Model,
	systemPrompt string,
	messages []Message,
	compat openAICompletionsCompat,
) []openAICompletionsMessage {
	params := make([]openAICompletionsMessage, 0, len(messages)+1)
	// System/developer role selection follows pi:
	// .agents/references/pi/packages/ai/src/providers/openai-completions.ts:765
	if systemPrompt != "" {
		role := "system"
		if model.Reasoning && compat.supportsDeveloperRole {
			role = "developer"
		}
		params = append(params, openAICompletionsMessage{"role": role, "content": systemPrompt})
	}

	lastRole := ""
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if compat.requiresAssistantAfterToolResult && lastRole == "toolResult" && msg.Role == RoleUser {
			params = append(params, openAICompletionsProcessedToolResultMessage())
		}
		switch msg.Role {
		case RoleUser:
			if converted, ok := convertOpenAICompletionsUserMessage(msg, model); ok {
				params = append(params, converted)
			}
		case RoleAssistant:
			if converted, ok := convertOpenAICompletionsAssistantMessage(msg, model, compat); ok {
				params = append(params, converted)
			}
		case RoleToolResult:
			next, role := appendOpenAICompletionsToolResults(&params, messages, i, model, compat)
			i = next
			lastRole = role
			continue
		}
		lastRole = string(msg.Role)
	}
	return params
}

func convertOpenAICompletionsUserMessage(message Message, model Model) (openAICompletionsMessage, bool) {
	content := convertOpenAICompletionsContentParts(message.Content, model)
	if len(content) == 0 {
		return nil, false
	}
	return openAICompletionsMessage{"role": "user", "content": content}, true
}

func convertOpenAICompletionsContentParts(content []ContentBlock, model Model) []openAICompletionsContentPart {
	result := make([]openAICompletionsContentPart, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case ContentText:
			result = append(result, openAICompletionsTextPart(block.Text))
		case ContentImage:
			if ModelSupportsInput(model, InputImage) {
				result = append(result, openAICompletionsImagePart(block))
			}
		}
	}
	return result
}

func convertOpenAICompletionsAssistantMessage(
	message Message,
	model Model,
	compat openAICompletionsCompat,
) (openAICompletionsMessage, bool) {
	assistant := openAICompletionsMessage{"role": "assistant", "content": nil}
	if compat.requiresAssistantAfterToolResult {
		assistant["content"] = ""
	}
	applyOpenAICompletionsAssistantTextAndThinking(assistant, message.Content, model, compat)
	if toolCalls := convertOpenAICompletionsToolCalls(message.Content); len(toolCalls) > 0 {
		assistant["tool_calls"] = toolCalls
	}
	if details := openAICompletionsReasoningDetails(message.Content); len(details) > 0 {
		assistant["reasoning_details"] = details
	}
	if compat.requiresReasoningContentOnAssistantMessages && model.Reasoning {
		if _, ok := assistant["reasoning_content"]; !ok {
			assistant["reasoning_content"] = ""
		}
	}
	if !openAICompletionsMessageHasContent(assistant) && assistant["tool_calls"] == nil {
		return nil, false
	}
	return assistant, true
}

func applyOpenAICompletionsAssistantTextAndThinking(
	assistant openAICompletionsMessage,
	content []ContentBlock,
	model Model,
	compat openAICompletionsCompat,
) {
	textParts := openAICompletionsAssistantTextParts(content)
	thinkingBlocks := nonEmptyOpenAICompletionsThinkingBlocks(content)
	assistantText := joinOpenAICompletionsTextParts(textParts)
	if len(thinkingBlocks) > 0 {
		applyOpenAICompletionsThinkingBlocks(assistant, textParts, thinkingBlocks, model, compat)
		return
	}
	if assistantText != "" {
		assistant["content"] = assistantText
	}
}

func applyOpenAICompletionsThinkingBlocks(
	assistant openAICompletionsMessage,
	textParts []openAICompletionsContentPart,
	thinkingBlocks []ContentBlock,
	model Model,
	compat openAICompletionsCompat,
) {
	if compat.requiresThinkingAsText {
		thinkingText := joinOpenAICompletionsThinkingBlocks(thinkingBlocks)
		assistant["content"] = append([]openAICompletionsContentPart{openAICompletionsTextPart(thinkingText)}, textParts...)
		return
	}
	if text := joinOpenAICompletionsTextParts(textParts); text != "" {
		assistant["content"] = text
	}
	signature := thinkingBlocks[0].ThinkingSignature
	if model.Provider == "opencode-go" && signature == "reasoning" {
		signature = "reasoning_content"
	}
	if signature != "" {
		assistant[signature] = joinOpenAICompletionsThinkingBlocks(thinkingBlocks)
	}
}

func openAICompletionsAssistantTextParts(content []ContentBlock) []openAICompletionsContentPart {
	parts := []openAICompletionsContentPart{}
	for _, block := range content {
		if block.Type == ContentText && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, openAICompletionsTextPart(block.Text))
		}
	}
	return parts
}

func nonEmptyOpenAICompletionsThinkingBlocks(content []ContentBlock) []ContentBlock {
	blocks := []ContentBlock{}
	for _, block := range content {
		if block.Type == ContentThinking && strings.TrimSpace(block.Thinking) != "" {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func joinOpenAICompletionsTextParts(parts []openAICompletionsContentPart) string {
	text := make([]string, 0, len(parts))
	for _, part := range parts {
		if value, ok := part["text"].(string); ok {
			text = append(text, value)
		}
	}
	return strings.Join(text, "")
}

func joinOpenAICompletionsThinkingBlocks(blocks []ContentBlock) string {
	text := make([]string, 0, len(blocks))
	for _, block := range blocks {
		text = append(text, block.Thinking)
	}
	return strings.Join(text, "\n")
}

func convertOpenAICompletionsToolCalls(content []ContentBlock) []map[string]any {
	toolCalls := []map[string]any{}
	for _, block := range content {
		if block.Type != ContentToolCall {
			continue
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   block.ToolCallID,
			"type": "function",
			"function": map[string]any{
				"name":      block.ToolName,
				"arguments": mustMarshalJSON(block.Arguments),
			},
		})
	}
	return toolCalls
}

func openAICompletionsReasoningDetails(content []ContentBlock) []any {
	details := []any{}
	for _, block := range content {
		if block.Type != ContentToolCall || block.ThoughtSignature == "" {
			continue
		}
		var detail any
		if err := json.Unmarshal([]byte(block.ThoughtSignature), &detail); err == nil {
			details = append(details, detail)
		}
	}
	return details
}

func appendOpenAICompletionsToolResults(
	params *[]openAICompletionsMessage,
	messages []Message,
	start int,
	model Model,
	compat openAICompletionsCompat,
) (int, string) {
	imageParts := []openAICompletionsContentPart{}
	j := start
	for ; j < len(messages) && messages[j].Role == RoleToolResult; j++ {
		toolMessage := messages[j]
		*params = append(*params, openAICompletionsToolResultMessage(toolMessage, compat))
		if ModelSupportsInput(model, InputImage) {
			imageParts = append(imageParts, openAICompletionsImageParts(toolMessage.Content)...)
		}
	}
	if len(imageParts) == 0 {
		return j - 1, "toolResult"
	}
	if compat.requiresAssistantAfterToolResult {
		*params = append(*params, openAICompletionsProcessedToolResultMessage())
	}
	*params = append(*params, openAICompletionsToolResultImageMessage(imageParts))
	return j - 1, "user"
}

func openAICompletionsToolResultMessage(
	message Message,
	compat openAICompletionsCompat,
) openAICompletionsMessage {
	result := openAICompletionsMessage{
		"role":         "tool",
		"content":      openAICompletionsToolResultText(message.Content),
		"tool_call_id": message.ToolCallID,
	}
	if compat.requiresToolResultName && message.ToolName != "" {
		result["name"] = message.ToolName
	}
	return result
}

func openAICompletionsToolResultText(content []ContentBlock) string {
	text := textBlocksContent(content)
	if text != "" {
		return text
	}
	if hasImageContent(content) {
		return "(see attached image)"
	}
	return ""
}

func openAICompletionsImageParts(content []ContentBlock) []openAICompletionsContentPart {
	parts := []openAICompletionsContentPart{}
	for _, block := range content {
		if block.Type == ContentImage {
			parts = append(parts, openAICompletionsImagePart(block))
		}
	}
	return parts
}

func openAICompletionsProcessedToolResultMessage() openAICompletionsMessage {
	return openAICompletionsMessage{
		"role":    "assistant",
		"content": "I have processed the tool results.",
	}
}

func openAICompletionsToolResultImageMessage(parts []openAICompletionsContentPart) openAICompletionsMessage {
	content := append(
		[]openAICompletionsContentPart{openAICompletionsTextPart("Attached image(s) from tool result:")},
		parts...,
	)
	return openAICompletionsMessage{"role": "user", "content": content}
}

func openAICompletionsTextPart(text string) openAICompletionsContentPart {
	return openAICompletionsContentPart{"type": "text", "text": text}
}

func openAICompletionsImagePart(block ContentBlock) openAICompletionsContentPart {
	return openAICompletionsContentPart{
		"type": "image_url",
		"image_url": map[string]string{
			"url": fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.ImageData),
		},
	}
}

func openAICompletionsMessageHasContent(message openAICompletionsMessage) bool {
	switch content := message["content"].(type) {
	case string:
		return content != ""
	case []openAICompletionsContentPart:
		return len(content) > 0
	default:
		return false
	}
}

func hasOpenAICompletionsToolHistory(messages []Message) bool {
	for _, message := range messages {
		if message.Role == RoleToolResult {
			return true
		}
		for _, block := range message.Content {
			if block.Type == ContentToolCall {
				return true
			}
		}
	}
	return false
}

func openAICompletionsCacheControlFor(
	compat openAICompletionsCompat,
	cacheRetention CacheRetention,
) *openAICompletionsCompatCacheControl {
	if compat.cacheControlFormat != "anthropic" || cacheRetention == CacheRetentionNone {
		return nil
	}
	cacheControl := &openAICompletionsCompatCacheControl{Type: "ephemeral"}
	if cacheRetention == CacheRetentionLong && compat.supportsLongCacheRetention {
		cacheControl.TTL = "1h"
	}
	return cacheControl
}

func applyOpenAICompletionsAnthropicCacheControl(
	messages []openAICompletionsMessage,
	tools []openAICompletionsTool,
	cacheControl *openAICompletionsCompatCacheControl,
) {
	addOpenAICompletionsCacheControlToSystemPrompt(messages, cacheControl)
	addOpenAICompletionsCacheControlToLastTool(tools, cacheControl)
	addOpenAICompletionsCacheControlToLastConversationMessage(messages, cacheControl)
}

func addOpenAICompletionsCacheControlToSystemPrompt(
	messages []openAICompletionsMessage,
	cacheControl *openAICompletionsCompatCacheControl,
) {
	for _, message := range messages {
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			addOpenAICompletionsCacheControlToTextContent(message, cacheControl)
			return
		}
	}
}

func addOpenAICompletionsCacheControlToLastTool(
	tools []openAICompletionsTool,
	cacheControl *openAICompletionsCompatCacheControl,
) {
	if len(tools) == 0 {
		return
	}
	tools[len(tools)-1]["cache_control"] = cacheControl
}

func addOpenAICompletionsCacheControlToLastConversationMessage(
	messages []openAICompletionsMessage,
	cacheControl *openAICompletionsCompatCacheControl,
) {
	for i := len(messages) - 1; i >= 0; i-- {
		role, _ := messages[i]["role"].(string)
		if role == "user" || role == "assistant" {
			if addOpenAICompletionsCacheControlToTextContent(messages[i], cacheControl) {
				return
			}
		}
	}
}

func addOpenAICompletionsCacheControlToTextContent(
	message openAICompletionsMessage,
	cacheControl *openAICompletionsCompatCacheControl,
) bool {
	switch content := message["content"].(type) {
	case string:
		if content == "" {
			return false
		}
		part := openAICompletionsTextPart(content)
		part["cache_control"] = cacheControl
		message["content"] = []openAICompletionsContentPart{part}
		return true
	case []openAICompletionsContentPart:
		return addOpenAICompletionsCacheControlToParts(content, cacheControl)
	default:
		return false
	}
}

func addOpenAICompletionsCacheControlToParts(
	parts []openAICompletionsContentPart,
	cacheControl *openAICompletionsCompatCacheControl,
) bool {
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i]["type"] == "text" {
			parts[i]["cache_control"] = cacheControl
			return true
		}
	}
	return false
}

func buildOpenAICompletionsHeaders(model Model, opts *StreamOptions) http.Header {
	headers := http.Header{}
	for key, value := range model.Headers {
		headers.Set(key, value)
	}
	for key, value := range opts.Headers {
		headers.Set(key, value)
	}
	if opts.APIKey != "" && headers.Get("authorization") == "" {
		headers.Set("authorization", "Bearer "+opts.APIKey)
	}
	compat := getOpenAICompletionsCompat(model)
	if opts.SessionID != "" && cacheRetentionEnabled(opts.CacheRetention) && compat.sendSessionAffinityHeaders {
		headers.Set("session_id", opts.SessionID)
		headers.Set("x-client-request-id", opts.SessionID)
		headers.Set("x-session-affinity", opts.SessionID)
	}
	return headers
}

func parseOpenAICompletionsSSE(reader io.Reader) iter.Seq2[openAICompletionsStreamEvent, error] {
	return func(yield func(openAICompletionsStreamEvent, error) bool) {
		for event, err := range sseparse.Parse(reader) {
			if err != nil {
				yield(openAICompletionsStreamEvent{}, err)
				return
			}
			decoded, ok, err := decodeOpenAICompletionsEventData([]byte(event.Data))
			if err != nil {
				yield(openAICompletionsStreamEvent{}, err)
				return
			}
			if ok && !yield(decoded, nil) {
				return
			}
		}
	}
}

func decodeOpenAICompletionsEventData(data []byte) (openAICompletionsStreamEvent, bool, error) {
	if len(strings.TrimSpace(string(data))) == 0 || strings.TrimSpace(string(data)) == "[DONE]" {
		return openAICompletionsStreamEvent{}, false, nil
	}
	var raw openAICompletionsRawChunk
	if err := json.Unmarshal(data, &raw); err != nil {
		return openAICompletionsStreamEvent{}, false, fmt.Errorf("decode openai completions event: %w", err)
	}
	if raw.Error.Message != "" {
		return openAICompletionsStreamEvent{Type: "error", ErrorMessage: raw.Error.Message}, true, nil
	}
	return raw.toStreamEvent()
}

type openAICompletionsRawChunk struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Choices []json.RawMessage `json:"choices"`
	Usage   json.RawMessage   `json:"usage"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (raw openAICompletionsRawChunk) toStreamEvent() (openAICompletionsStreamEvent, bool, error) {
	event := openAICompletionsStreamEvent{
		Type:          "chunk",
		ResponseID:    raw.ID,
		ResponseModel: raw.Model,
	}
	if len(raw.Usage) > 0 {
		usage, err := decodeOpenAICompletionsUsage(raw.Usage)
		if err != nil {
			return openAICompletionsStreamEvent{}, false, err
		}
		event.Usage = usage
	}
	if len(raw.Choices) == 0 {
		return event, event.ResponseID != "" || event.Usage != nil, nil
	}
	if err := decodeOpenAICompletionsChoice(raw.Choices[0], &event); err != nil {
		return openAICompletionsStreamEvent{}, false, err
	}
	return event, true, nil
}

func decodeOpenAICompletionsChoice(data json.RawMessage, event *openAICompletionsStreamEvent) error {
	var raw struct {
		Delta        openAICompletionsRawDelta `json:"delta"`
		FinishReason *string                   `json:"finish_reason"`
		Usage        json.RawMessage           `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode openai completions choice: %w", err)
	}
	if raw.FinishReason != nil {
		event.FinishReason = *raw.FinishReason
		event.HasFinishReason = true
	}
	if event.Usage == nil && len(raw.Usage) > 0 {
		usage, err := decodeOpenAICompletionsUsage(raw.Usage)
		if err != nil {
			return err
		}
		event.Usage = usage
	}
	raw.Delta.apply(event)
	return nil
}

type openAICompletionsRawDelta struct {
	Content          string                         `json:"content"`
	ReasoningContent string                         `json:"reasoning_content"`
	Reasoning        string                         `json:"reasoning"`
	ReasoningText    string                         `json:"reasoning_text"`
	ToolCalls        []openAICompletionsRawToolCall `json:"tool_calls"`
	ReasoningDetails []json.RawMessage              `json:"reasoning_details"`
}

func (raw openAICompletionsRawDelta) apply(event *openAICompletionsStreamEvent) {
	event.ContentDelta = raw.Content
	event.ReasoningDelta, event.ReasoningField = raw.reasoning()
	event.ToolCalls = raw.toolCallDeltas()
	event.ReasoningDetails = raw.reasoningDetails()
}

func (raw openAICompletionsRawDelta) reasoning() (string, string) {
	// Reasoning field priority mirrors pi:
	// .agents/references/pi/packages/ai/src/providers/openai-completions.ts:312
	switch {
	case raw.ReasoningContent != "":
		return raw.ReasoningContent, "reasoning_content"
	case raw.Reasoning != "":
		return raw.Reasoning, "reasoning"
	case raw.ReasoningText != "":
		return raw.ReasoningText, "reasoning_text"
	default:
		return "", ""
	}
}

func (raw openAICompletionsRawDelta) toolCallDeltas() []openAICompletionsToolCallDelta {
	if len(raw.ToolCalls) == 0 {
		return nil
	}
	result := make([]openAICompletionsToolCallDelta, 0, len(raw.ToolCalls))
	for _, toolCall := range raw.ToolCalls {
		delta := openAICompletionsToolCallDelta{
			ID:   toolCall.ID,
			Name: toolCall.Function.Name,
			Args: toolCall.Function.Arguments,
		}
		if toolCall.Index != nil {
			delta.Index = *toolCall.Index
			delta.HasIndex = true
		}
		result = append(result, delta)
	}
	return result
}

func (raw openAICompletionsRawDelta) reasoningDetails() []openAICompletionsReasoningDetail {
	details := []openAICompletionsReasoningDetail{}
	for _, item := range raw.ReasoningDetails {
		var detail struct {
			Type string          `json:"type"`
			ID   string          `json:"id"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(item, &detail); err == nil &&
			detail.Type == "reasoning.encrypted" &&
			detail.ID != "" &&
			len(detail.Data) > 0 {
			details = append(details, openAICompletionsReasoningDetail{ID: detail.ID, Raw: string(item)})
		}
	}
	return details
}

type openAICompletionsRawToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func decodeOpenAICompletionsUsage(data json.RawMessage) (*Usage, error) {
	var raw struct {
		PromptTokens         int `json:"prompt_tokens"`
		CompletionTokens     int `json:"completion_tokens"`
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails  struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode openai completions usage: %w", err)
	}
	cacheRead := raw.PromptTokensDetails.CachedTokens
	if cacheRead == 0 {
		cacheRead = raw.PromptCacheHitTokens
	}
	cacheWrite := raw.PromptTokensDetails.CacheWriteTokens
	input := max(raw.PromptTokens-cacheRead-cacheWrite, 0)
	output := raw.CompletionTokens
	return &Usage{
		Input:       input,
		Output:      output,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		TotalTokens: input + output + cacheRead + cacheWrite,
		Cost:        &Cost{},
	}, nil
}

func openAICompletionsAPIErrorEvent(model Model, status int, body []byte) *StreamEvent {
	return errorMessageEvent(model, &APIError{
		API:     model.API,
		Model:   model.ID,
		Message: openAICompletionsAPIErrorMessage(status, body),
	})
}

func openAICompletionsAPIErrorMessage(status int, body []byte) string {
	message := openAICompletionsErrorBodyMessage(body)
	if message == "" {
		return fmt.Sprintf("OpenAI Completions API error (%d)", status)
	}
	return fmt.Sprintf("OpenAI Completions API error (%d): %s", status, message)
}

func openAICompletionsErrorBodyMessage(body []byte) string {
	var raw struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	if raw.Error.Message != "" {
		return raw.Error.Message
	}
	return raw.Message
}

func streamOpenAICompletionsEvents(
	ctx context.Context,
	model Model,
	events iter.Seq2[openAICompletionsStreamEvent, error],
) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		state := newOpenAICompletionsStreamState(model)
		if !yield(&StreamEvent{Type: EventMessageStart, Message: state.output}, nil) {
			return
		}
		for event, err := range events {
			if err != nil {
				yield(nil, err)
				return
			}
			if state.cancelled(ctx, yield) {
				return
			}
			if !state.apply(event, yield) {
				return
			}
		}
		state.finish(yield)
	}
}

type openAICompletionsStreamState struct {
	model            Model
	output           *Message
	text             *openAICompletionsBlock
	thinking         *openAICompletionsBlock
	toolCallsByIndex map[int]*openAICompletionsBlock
	toolCallsByID    map[string]*openAICompletionsBlock
	hasFinishReason  bool
}

func newOpenAICompletionsStreamState(model Model) *openAICompletionsStreamState {
	return &openAICompletionsStreamState{
		model:            model,
		output:           newAssistantMessage(model),
		toolCallsByIndex: map[int]*openAICompletionsBlock{},
		toolCallsByID:    map[string]*openAICompletionsBlock{},
	}
}

func (s *openAICompletionsStreamState) cancelled(
	ctx context.Context,
	yield func(*StreamEvent, error) bool,
) bool {
	select {
	case <-ctx.Done():
		s.output.StopReason = StopReasonAborted
		s.output.ErrorMessage = ctx.Err().Error()
		if !yield(&StreamEvent{Type: EventMessageError, Message: s.output, Usage: s.output.Usage}, nil) {
			return true
		}
		yield(&StreamEvent{Type: EventMessageComplete, Message: s.output, Usage: s.output.Usage}, nil)
		return true
	default:
		return false
	}
}

func (s *openAICompletionsStreamState) apply(
	event openAICompletionsStreamEvent,
	yield func(*StreamEvent, error) bool,
) bool {
	if event.Type == "error" {
		s.output.StopReason = StopReasonError
		s.output.ErrorMessage = event.ErrorMessage
		if s.output.ErrorMessage == "" {
			s.output.ErrorMessage = "OpenAI Completions stream error"
		}
		// pi's catch always pushes an error event before ending the stream
		// (openai-completions.ts:406-419).
		if !yield(&StreamEvent{Type: EventMessageError, Message: s.output, Usage: s.output.Usage}, nil) {
			return false
		}
		yield(&StreamEvent{Type: EventMessageComplete, Message: s.output, Usage: s.output.Usage}, nil)
		return false
	}
	s.applyMetadata(event)
	// Delta mapping mirrors pi's OpenAI-compatible chunk loop:
	// .agents/references/pi/packages/ai/src/providers/openai-completions.ts:296
	if event.ContentDelta != "" && !s.applyTextDelta(event.ContentDelta, yield) {
		return false
	}
	if event.ReasoningDelta != "" && !s.applyReasoningDelta(event, yield) {
		return false
	}
	for _, toolCall := range event.ToolCalls {
		if !s.applyToolCallDelta(toolCall, yield) {
			return false
		}
	}
	s.applyReasoningDetails(event.ReasoningDetails)
	return true
}

func (s *openAICompletionsStreamState) applyMetadata(event openAICompletionsStreamEvent) {
	if event.ResponseID != "" && s.output.ResponseID == "" {
		s.output.ResponseID = event.ResponseID
	}
	if event.ResponseModel != "" && event.ResponseModel != s.output.Model {
		s.output.ResponseModel = event.ResponseModel
	}
	if event.Usage != nil {
		s.output.Usage = usageWithTotals(event.Usage)
		CalculateCost(s.model, s.output.Usage)
	}
	if event.HasFinishReason {
		s.hasFinishReason = true
		s.applyFinishReason(event.FinishReason)
	}
}

func (s *openAICompletionsStreamState) applyFinishReason(reason string) {
	stopReason, message := mapOpenAICompletionsStopReason(reason)
	s.output.StopReason = stopReason
	if message != "" {
		s.output.ErrorMessage = message
	}
}

func (s *openAICompletionsStreamState) applyTextDelta(
	delta string,
	yield func(*StreamEvent, error) bool,
) bool {
	block := s.ensureTextBlock(yield)
	if block == nil {
		return false
	}
	s.output.Content[block.index].Text += delta
	block.Text += delta
	return yield(&StreamEvent{
		Type:    EventTextDelta,
		Index:   block.index,
		Delta:   &ContentBlock{Type: ContentText, Text: delta},
		Message: s.output,
	}, nil)
}

func (s *openAICompletionsStreamState) applyReasoningDelta(
	event openAICompletionsStreamEvent,
	yield func(*StreamEvent, error) bool,
) bool {
	block := s.ensureThinkingBlock(event.ReasoningField, yield)
	if block == nil {
		return false
	}
	s.output.Content[block.index].Thinking += event.ReasoningDelta
	block.Thinking += event.ReasoningDelta
	return yield(&StreamEvent{
		Type:    EventThinkingDelta,
		Index:   block.index,
		Delta:   &ContentBlock{Type: ContentThinking, Thinking: event.ReasoningDelta},
		Message: s.output,
	}, nil)
}

func (s *openAICompletionsStreamState) applyToolCallDelta(
	toolCall openAICompletionsToolCallDelta,
	yield func(*StreamEvent, error) bool,
) bool {
	block := s.ensureToolCallBlock(toolCall, yield)
	if block == nil {
		return false
	}
	s.applyToolCallIdentity(block, toolCall)
	if toolCall.Args != "" {
		block.partialJSON += toolCall.Args
		s.output.Content[block.index].Arguments = decodeJSONMap(block.partialJSON)
	}
	return yield(&StreamEvent{
		Type:  EventToolCallDelta,
		Index: block.index,
		Delta: &ContentBlock{
			Type:      ContentToolCall,
			Arguments: s.output.Content[block.index].Arguments,
		},
		Message: s.output,
	}, nil)
}

func (s *openAICompletionsStreamState) applyToolCallIdentity(
	block *openAICompletionsBlock,
	toolCall openAICompletionsToolCallDelta,
) {
	if block.ToolCallID == "" && toolCall.ID != "" {
		block.ToolCallID = toolCall.ID
		s.output.Content[block.index].ToolCallID = toolCall.ID
		s.toolCallsByID[toolCall.ID] = block
	}
	if block.ToolName == "" && toolCall.Name != "" {
		block.ToolName = toolCall.Name
		s.output.Content[block.index].ToolName = toolCall.Name
	}
}

func (s *openAICompletionsStreamState) applyReasoningDetails(details []openAICompletionsReasoningDetail) {
	for _, detail := range details {
		for i := range s.output.Content {
			block := &s.output.Content[i]
			if block.Type == ContentToolCall && block.ToolCallID == detail.ID {
				block.ThoughtSignature = detail.Raw
			}
		}
	}
}

func (s *openAICompletionsStreamState) ensureTextBlock(
	yield func(*StreamEvent, error) bool,
) *openAICompletionsBlock {
	if s.text != nil {
		return s.text
	}
	block := &openAICompletionsBlock{ContentBlock: ContentBlock{Type: ContentText}}
	if !s.startBlock(block, yield) {
		return nil
	}
	s.text = block
	return block
}

func (s *openAICompletionsStreamState) ensureThinkingBlock(
	field string,
	yield func(*StreamEvent, error) bool,
) *openAICompletionsBlock {
	if s.thinking != nil {
		return s.thinking
	}
	signature := field
	if s.output.Provider == "opencode-go" && signature == "reasoning" {
		signature = "reasoning_content"
	}
	block := &openAICompletionsBlock{
		ContentBlock: ContentBlock{Type: ContentThinking, ThinkingSignature: signature},
	}
	if !s.startBlock(block, yield) {
		return nil
	}
	s.thinking = block
	return block
}

func (s *openAICompletionsStreamState) ensureToolCallBlock(
	toolCall openAICompletionsToolCallDelta,
	yield func(*StreamEvent, error) bool,
) *openAICompletionsBlock {
	if toolCall.HasIndex {
		if block := s.toolCallsByIndex[toolCall.Index]; block != nil {
			return block
		}
	}
	if toolCall.ID != "" {
		if block := s.toolCallsByID[toolCall.ID]; block != nil {
			return block
		}
	}
	block := &openAICompletionsBlock{
		ContentBlock: ContentBlock{
			Type:       ContentToolCall,
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Name,
			Arguments:  map[string]any{},
		},
		streamIndex:    toolCall.Index,
		hasStreamIndex: toolCall.HasIndex,
	}
	if !s.startBlock(block, yield) {
		return nil
	}
	s.indexToolCallBlock(block)
	return block
}

func (s *openAICompletionsStreamState) indexToolCallBlock(block *openAICompletionsBlock) {
	if block.hasStreamIndex {
		s.toolCallsByIndex[block.streamIndex] = block
	}
	if block.ToolCallID != "" {
		s.toolCallsByID[block.ToolCallID] = block
	}
}

func (s *openAICompletionsStreamState) startBlock(
	block *openAICompletionsBlock,
	yield func(*StreamEvent, error) bool,
) bool {
	s.output.Content = append(s.output.Content, block.ContentBlock)
	block.index = len(s.output.Content) - 1
	startType := contentBlockStartEvent(block.Type)
	return yield(&StreamEvent{
		Type:    startType,
		Index:   block.index,
		Delta:   &s.output.Content[block.index],
		Message: s.output,
	}, nil)
}

func (s *openAICompletionsStreamState) finish(yield func(*StreamEvent, error) bool) {
	s.finalizeToolCalls()
	// pi runs finishBlock over every accumulated block, pushing
	// text_end/thinking_end/toolcall_end (the latter with finalized parsed
	// arguments) before the done event (openai-completions.ts:174-206,387-389).
	for i := range s.output.Content {
		endType := contentBlockEndEvent(s.output.Content[i].Type)
		if !yield(&StreamEvent{Type: endType, Index: i, Delta: &s.output.Content[i], Message: s.output}, nil) {
			return
		}
	}
	if !s.hasFinishReason {
		s.output.StopReason = StopReasonError
		s.output.ErrorMessage = "Stream ended without finish_reason"
	} else if s.output.StopReason == "" {
		s.output.StopReason = StopReasonStop
	}
	if s.output.StopReason == StopReasonStop && outputHasToolCall(s.output) {
		s.output.StopReason = StopReasonToolUse
	}
	doneOrError := EventMessageDone
	if s.output.StopReason == StopReasonError || s.output.StopReason == StopReasonAborted {
		doneOrError = EventMessageError
	}
	if !yield(&StreamEvent{Type: doneOrError, Message: s.output, Usage: s.output.Usage}, nil) {
		return
	}
	yield(&StreamEvent{Type: EventMessageComplete, Message: s.output, Usage: s.output.Usage}, nil)
}

func (s *openAICompletionsStreamState) finalizeToolCalls() {
	for _, block := range s.toolCallsByIndex {
		s.output.Content[block.index].Arguments = decodeJSONMap(block.partialJSON)
	}
	for _, block := range s.toolCallsByID {
		s.output.Content[block.index].Arguments = decodeJSONMap(block.partialJSON)
	}
}

func mapOpenAICompletionsStopReason(reason string) (StopReason, string) {
	switch reason {
	case "stop", "end":
		return StopReasonStop, ""
	case "length":
		return StopReasonLength, ""
	case "function_call", "tool_calls":
		return StopReasonToolUse, ""
	case "content_filter":
		return StopReasonError, "Provider finish_reason: content_filter"
	case "network_error":
		return StopReasonError, "Provider finish_reason: network_error"
	default:
		return StopReasonError, fmt.Sprintf("Provider finish_reason: %s", reason)
	}
}

func getOpenAICompletionsCompat(model Model) openAICompletionsCompat {
	detected := detectOpenAICompletionsCompat(model)
	if model.Compat == nil {
		return detected
	}
	return openAICompletionsCompat{
		supportsStore:         boolCompat(model.Compat.SupportsStore, detected.supportsStore),
		supportsDeveloperRole: boolCompat(model.Compat.SupportsDeveloperRole, detected.supportsDeveloperRole),
		supportsReasoningEffort: boolCompat(
			model.Compat.SupportsReasoningEffort,
			detected.supportsReasoningEffort,
		),
		supportsUsageInStreaming: boolCompat(
			model.Compat.SupportsUsageInStreaming,
			detected.supportsUsageInStreaming,
		),
		requiresToolResultName: boolCompat(
			model.Compat.RequiresToolResultName,
			detected.requiresToolResultName,
		),
		requiresAssistantAfterToolResult: boolCompat(
			model.Compat.RequiresAssistantAfterToolResult,
			detected.requiresAssistantAfterToolResult,
		),
		requiresThinkingAsText: boolCompat(
			model.Compat.RequiresThinkingAsText,
			detected.requiresThinkingAsText,
		),
		requiresReasoningContentOnAssistantMessages: boolCompat(
			model.Compat.RequiresReasoningContentOnAssistantMessages,
			detected.requiresReasoningContentOnAssistantMessages,
		),
		zaiToolStream:      boolCompat(model.Compat.ZaiToolStream, detected.zaiToolStream),
		supportsStrictMode: boolCompat(model.Compat.SupportsStrictMode, detected.supportsStrictMode),
		sendSessionAffinityHeaders: boolCompat(
			model.Compat.SendSessionAffinityHeaders,
			detected.sendSessionAffinityHeaders,
		),
		supportsLongCacheRetention: boolCompat(
			model.Compat.SupportsLongCacheRetention,
			detected.supportsLongCacheRetention,
		),
		maxTokensField:     stringCompat(model.Compat.MaxTokensField, detected.maxTokensField),
		thinkingFormat:     stringCompat(model.Compat.ThinkingFormat, detected.thinkingFormat),
		cacheControlFormat: stringCompat(model.Compat.CacheControlFormat, detected.cacheControlFormat),
	}
}

func detectOpenAICompletionsCompat(model Model) openAICompletionsCompat {
	provider := model.Provider
	baseURL := model.BaseURL
	isZai := provider == "zai" || strings.Contains(baseURL, "api.z.ai")
	isTogether := provider == "together" ||
		strings.Contains(baseURL, "api.together.ai") ||
		strings.Contains(baseURL, "api.together.xyz")
	isMoonshot := provider == "moonshotai" ||
		provider == "moonshotai-cn" ||
		strings.Contains(baseURL, "api.moonshot.")
	isCloudflareWorkersAI := provider == "cloudflare-workers-ai" || strings.Contains(baseURL, "api.cloudflare.com")
	isCloudflareGateway := provider == "cloudflare-ai-gateway" ||
		strings.Contains(baseURL, "gateway.ai.cloudflare.com")
	isGrok := provider == "xai" || strings.Contains(baseURL, "api.x.ai")
	isDeepSeek := provider == "deepseek" || strings.Contains(baseURL, "deepseek.com")

	isNonStandard := provider == "cerebras" ||
		strings.Contains(baseURL, "cerebras.ai") ||
		isGrok ||
		isTogether ||
		strings.Contains(baseURL, "chutes.ai") ||
		isDeepSeek ||
		isZai ||
		isMoonshot ||
		provider == "opencode" ||
		strings.Contains(baseURL, "opencode.ai") ||
		isCloudflareWorkersAI ||
		isCloudflareGateway
	useMaxTokens := strings.Contains(baseURL, "chutes.ai") || isMoonshot || isCloudflareGateway || isTogether
	return openAICompletionsCompat{
		supportsStore:                               !isNonStandard,
		supportsDeveloperRole:                       !isNonStandard,
		supportsReasoningEffort:                     !isGrok && !isZai && !isMoonshot && !isTogether && !isCloudflareGateway,
		supportsUsageInStreaming:                    true,
		maxTokensField:                              openAICompletionsMaxTokensField(useMaxTokens),
		requiresToolResultName:                      false,
		requiresAssistantAfterToolResult:            false,
		requiresThinkingAsText:                      false,
		requiresReasoningContentOnAssistantMessages: isDeepSeek,
		thinkingFormat:                              openAICompletionsThinkingFormat(model, isDeepSeek, isZai, isTogether),
		zaiToolStream:                               false,
		supportsStrictMode:                          !isMoonshot && !isTogether && !isCloudflareGateway,
		cacheControlFormat:                          openAICompletionsCacheControlFormat(model),
		sendSessionAffinityHeaders:                  false,
		supportsLongCacheRetention:                  !(isTogether || isCloudflareWorkersAI || isCloudflareGateway),
	}
}

func openAICompletionsMaxTokensField(useMaxTokens bool) string {
	if useMaxTokens {
		return "max_tokens"
	}
	return "max_completion_tokens"
}

func openAICompletionsThinkingFormat(
	model Model,
	isDeepSeek bool,
	isZai bool,
	isTogether bool,
) string {
	switch {
	case isDeepSeek:
		return "deepseek"
	case isZai:
		return "zai"
	case isTogether:
		return "together"
	case model.Provider == "openrouter" || strings.Contains(model.BaseURL, "openrouter.ai"):
		return "openrouter"
	default:
		return "openai"
	}
}

func openAICompletionsCacheControlFormat(model Model) string {
	if model.Provider == "openrouter" && strings.HasPrefix(model.ID, "anthropic/") {
		return "anthropic"
	}
	return ""
}

func boolCompat(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func stringCompat(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func normalizeOpenAICompletionsToolCallID(id string, model Model, _ Message) string {
	if strings.Contains(id, "|") {
		callID, _ := splitToolCallID(id)
		return truncateOpenAICompletionsID(sanitizeOpenAICompletionsID(callID), 40)
	}
	if model.Provider == "openai" {
		return truncateOpenAICompletionsID(id, 40)
	}
	return id
}

func sanitizeOpenAICompletionsID(id string) string {
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
	}
	return builder.String()
}

func truncateOpenAICompletionsID(id string, limit int) string {
	var builder strings.Builder
	count := 0
	for _, char := range id {
		if count >= limit {
			break
		}
		builder.WriteRune(char)
		count++
	}
	return builder.String()
}

func clampOpenAICompletionsPromptCacheKey(key string) string {
	return truncateOpenAICompletionsID(key, 64)
}

func resolveOpenAICompletionsURL(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = defaultOpenAICompletionsBaseURL
	}
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}
