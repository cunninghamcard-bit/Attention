package jshost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
)

// ExtensionFactory adapts an unmodified pi TypeScript extension loaded by Host
// into along's Go extension registration API.
func ExtensionFactory(host *Host, path string) extension.Factory {
	return func(api extension.ExtensionAPI) error {
		if host == nil {
			return errors.New("jshost: nil host")
		}

		loaded, err := host.LoadExtension(path)
		if err != nil {
			return err
		}
		for _, provider := range loaded.Providers {
			def := providerDefinitionFromInfo(provider)
			if provider.Config.OAuth != nil {
				oauth.RegisterProvider(jsOAuthProvider{
					host: host,
					id:   provider.Name,
					name: provider.Config.OAuth.Name,
				})
			}
			api.RegisterProvider(
				provider.Name,
				def,
			)
		}
		for _, command := range loaded.Commands {
			name := command.Name
			description := command.Description
			api.RegisterCommand(name, extension.CommandDefinition{
				Description: description,
				Source: resource.SourceInfo{
					Kind: resource.SourceKind("extension"),
					Path: path,
				},
				Handler: func(
					ctx context.Context,
					args []string,
					extCtx extension.ExtensionContext,
				) error {
					return host.InvokeCommand(
						ctx,
						name,
						args,
						uiResponder(extCtx.UI),
						ctxResponder(extCtx),
					)
				},
			})
		}
		for _, tool := range loaded.Tools {
			name := tool.Name
			label := tool.Label
			description := tool.Description
			parameters := maps.Clone(tool.Parameters)
			hasRenderCall := tool.HasRenderCall
			hasRenderResult := tool.HasRenderResult
			def := extension.ToolDefinition{
				Name:        name,
				Label:       label,
				Description: description,
				Parameters:  parameters,
				RenderShell: extension.ToolRenderShell(tool.RenderShell),
				Execute: func(
					ctx context.Context,
					toolCall extension.ToolCall,
					onUpdate extension.ToolUpdateCallback,
					extCtx extension.ExtensionContext,
				) (extension.ToolResult, error) {
					return host.InvokeTool(
						ctx,
						name,
						toolCall,
						onUpdate,
						uiResponder(extCtx.UI),
						ctxResponder(extCtx),
					)
				},
			}
			if hasRenderCall {
				def.RenderCall = func(input extension.ToolCallRenderInput) []render.Block {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					blocks, err := host.InvokeToolRenderCall(ctx, name, input)
					if err != nil {
						return nil
					}
					return blocks
				}
			}
			if hasRenderResult {
				def.RenderResult = func(input extension.ToolResultRenderInput) []render.Block {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					blocks, err := host.InvokeToolRenderResult(ctx, name, input)
					if err != nil {
						return nil
					}
					return blocks
				}
			}
			api.RegisterTool(def)
		}
		for _, eventType := range loaded.Events {
			jsEventType := eventType
			alongEventType := alongEventTypeForJSEvent(jsEventType)
			api.On(alongEventType, func(
				ctx context.Context,
				event any,
				extCtx extension.ExtensionContext,
			) (any, error) {
				payload, err := marshalHookPayload(jsEventType, event)
				if err != nil {
					return nil, err
				}
				result, err := host.FireHook(
					ctx,
					jsEventType,
					payload,
					uiResponder(extCtx.UI),
					ctxResponder(extCtx),
				)
				if err != nil {
					return nil, err
				}
				return convertHookResult(jsEventType, event, result)
			})
		}
		return nil
	}
}

func providerDefinitionFromInfo(info ProviderInfo) extension.ProviderDefinition {
	cfg := info.Config
	return extension.ProviderDefinition{
		Name:           copyStringPtr(cfg.Name),
		BaseURL:        copyStringPtr(cfg.BaseURL),
		APIKey:         copyStringPtr(cfg.APIKey),
		API:            copyStringPtr(cfg.API),
		Headers:        maps.Clone(cfg.Headers),
		AuthHeader:     copyBoolPtr(cfg.AuthHeader),
		Compat:         cfg.Compat,
		Models:         providerModelsFromInfo(cfg.Models),
		ModelOverrides: providerModelOverridesFromInfo(cfg.ModelOverrides),
		OAuth:          providerOAuthFromInfo(cfg.OAuth),
	}
}

func providerOAuthFromInfo(in *ProviderOAuthInfo) *extension.ProviderOAuth {
	if in == nil {
		return nil
	}
	return &extension.ProviderOAuth{Name: in.Name}
}

func providerModelsFromInfo(in []ProviderModelInfo) []extension.ProviderModel {
	if in == nil {
		return nil
	}
	out := make([]extension.ProviderModel, 0, len(in))
	for _, model := range in {
		out = append(out, extension.ProviderModel{
			ID:               model.ID,
			Name:             copyStringPtr(model.Name),
			API:              copyStringPtr(model.API),
			BaseURL:          copyStringPtr(model.BaseURL),
			Reasoning:        copyBoolPtr(model.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(model.ThinkingLevelMap),
			Input:            append([]ai.InputCapability(nil), model.Input...),
			Cost:             providerModelCostFromInfo(model.Cost),
			ContextWindow:    copyIntPtr(model.ContextWindow),
			MaxTokens:        copyIntPtr(model.MaxTokens),
			Headers:          maps.Clone(model.Headers),
			Compat:           model.Compat,
		})
	}
	return out
}

func providerModelOverridesFromInfo(
	in map[string]ProviderModelOverrideInfo,
) map[string]extension.ProviderModelOverride {
	if in == nil {
		return nil
	}
	out := make(map[string]extension.ProviderModelOverride, len(in))
	for id, override := range in {
		out[id] = extension.ProviderModelOverride{
			Name:             copyStringPtr(override.Name),
			Reasoning:        copyBoolPtr(override.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(override.ThinkingLevelMap),
			Input:            append([]ai.InputCapability(nil), override.Input...),
			Cost:             providerModelCostFromInfo(override.Cost),
			ContextWindow:    copyIntPtr(override.ContextWindow),
			MaxTokens:        copyIntPtr(override.MaxTokens),
			Headers:          maps.Clone(override.Headers),
			Compat:           override.Compat,
		}
	}
	return out
}

func providerModelCostFromInfo(in *ProviderModelCostInfo) *extension.ProviderModelCost {
	if in == nil {
		return nil
	}
	return &extension.ProviderModelCost{
		Input:      copyFloatPtr(in.Input),
		Output:     copyFloatPtr(in.Output),
		CacheRead:  copyFloatPtr(in.CacheRead),
		CacheWrite: copyFloatPtr(in.CacheWrite),
	}
}

// alongEventTypeForJSEvent aliases pi coding-agent's before_provider_request
// payload hook to along's split before_provider_payload hook. along keeps
// before_provider_request for stream option patches, while pi JS extensions use
// before_provider_request for provider payload inspection/patching:
// .agents/references/pi/packages/coding-agent/src/core/extensions/runner.ts:890-921.
func alongEventTypeForJSEvent(eventType string) string {
	if eventType == hook.EventBeforeProviderRequest {
		return hook.EventBeforeProviderPayload
	}
	return eventType
}

func marshalHookPayload(eventType string, event any) (map[string]any, error) {
	switch eventType {
	case hook.EventBeforeAgentStart:
		e, ok := event.(hook.BeforeAgentStartEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.BeforeAgentStartEvent", eventType, event)
		}
		payload := map[string]any{
			"prompt":              e.Prompt,
			"systemPrompt":        e.SystemPrompt,
			"systemPromptOptions": marshalSystemPromptOptions(e.SystemPromptOptions),
		}
		if len(e.Images) > 0 {
			payload["images"] = marshalHookImages(e.Images)
		}
		return payload, nil
	case hook.EventContext:
		e, ok := event.(hook.ContextEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.ContextEvent", eventType, event)
		}
		return map[string]any{"messages": append([]any(nil), e.Messages...)}, nil
	case hook.EventBeforeProviderRequest, hook.EventBeforeProviderPayload:
		e, ok := event.(hook.BeforeProviderPayloadEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.BeforeProviderPayloadEvent", eventType, event)
		}
		return map[string]any{
			"model":   e.Model,
			"payload": e.Payload,
		}, nil
	case hook.EventToolCall:
		e, ok := event.(hook.ToolCallEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.ToolCallEvent", eventType, event)
		}
		return map[string]any{
			"toolCallId": e.ToolCallId,
			"toolName":   e.ToolName,
			"input":      e.Input,
		}, nil
	case hook.EventInput:
		e, ok := event.(hook.InputEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.InputEvent", eventType, event)
		}
		payload := map[string]any{
			"text":   e.Text,
			"source": e.Source,
		}
		if len(e.Images) > 0 {
			payload["images"] = marshalHookImages(e.Images)
		}
		return payload, nil
	case hook.EventToolResult:
		e, ok := event.(hook.ToolResultEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.ToolResultEvent", eventType, event)
		}
		return map[string]any{
			"toolCallId": e.ToolCallId,
			"toolName":   e.ToolName,
			"input":      e.Input,
			"content":    e.Content,
			"details":    e.Details,
			"isError":    e.IsError,
		}, nil
	case hook.EventSessionBeforeSwitch:
		e, ok := event.(hook.SessionBeforeSwitchEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.SessionBeforeSwitchEvent", eventType, event)
		}
		payload := map[string]any{
			"reason": e.Reason,
		}
		if e.TargetSessionFile != nil {
			payload["targetSessionFile"] = *e.TargetSessionFile
		}
		return payload, nil
	case hook.EventSessionBeforeFork:
		e, ok := event.(hook.SessionBeforeForkEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.SessionBeforeForkEvent", eventType, event)
		}
		return map[string]any{
			"entryId":  e.EntryID,
			"position": e.Position,
		}, nil
	case hook.EventMessageEnd:
		e, ok := event.(hook.MessageEndEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.MessageEndEvent", eventType, event)
		}
		return map[string]any{"message": e.Message}, nil
	case hook.EventAgentStart:
		return map[string]any{}, nil
	case hook.EventAgentEnd:
		e, ok := event.(hook.AgentEndEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.AgentEndEvent", eventType, event)
		}
		return map[string]any{"messages": e.Messages}, nil
	case hook.EventTurnStart:
		return map[string]any{}, nil
	case hook.EventTurnEnd:
		e, ok := event.(hook.TurnEndEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.TurnEndEvent", eventType, event)
		}
		return map[string]any{"message": e.Message, "toolResults": e.ToolResults}, nil
	case hook.EventMessageStart:
		e, ok := event.(hook.MessageStartEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.MessageStartEvent", eventType, event)
		}
		return map[string]any{"message": e.Message}, nil
	case hook.EventMessageUpdate:
		e, ok := event.(hook.MessageUpdateEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.MessageUpdateEvent", eventType, event)
		}
		return map[string]any{
			"message":               e.Message,
			"assistantMessageEvent": e.AssistantMessageEvent,
		}, nil
	case hook.EventToolExecutionStart:
		e, ok := event.(hook.ToolExecutionStartEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.ToolExecutionStartEvent", eventType, event)
		}
		return map[string]any{
			"toolCallId": e.ToolCallId,
			"toolName":   e.ToolName,
			"args":       e.Args,
		}, nil
	case hook.EventToolExecutionUpdate:
		e, ok := event.(hook.ToolExecutionUpdateEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.ToolExecutionUpdateEvent", eventType, event)
		}
		return map[string]any{
			"toolCallId":    e.ToolCallId,
			"toolName":      e.ToolName,
			"args":          e.Args,
			"partialResult": e.PartialResult,
		}, nil
	case hook.EventToolExecutionEnd:
		e, ok := event.(hook.ToolExecutionEndEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.ToolExecutionEndEvent", eventType, event)
		}
		return map[string]any{
			"toolCallId": e.ToolCallId,
			"toolName":   e.ToolName,
			"result":     e.Result,
			"isError":    e.IsError,
		}, nil
	case hook.EventAfterProviderResponse:
		e, ok := event.(hook.AfterProviderResponseEvent)
		if !ok {
			return nil, fmt.Errorf("jshost: %s event = %T, want hook.AfterProviderResponseEvent", eventType, event)
		}
		return map[string]any{"status": e.Status, "headers": maps.Clone(e.Headers)}, nil
	default:
		return map[string]any{}, nil
	}
}

func convertHookResult(eventType string, event any, result HookResult) (any, error) {
	switch eventType {
	case hook.EventBeforeAgentStart:
		beforeStart, ok, err := beforeAgentStartResult(result)
		if err != nil || !ok {
			return nil, err
		}
		return beforeStart, nil
	case hook.EventContext:
		if !result.MessagesSet {
			return nil, nil
		}
		messages, err := decodeMessages(result.Messages)
		if err != nil {
			return nil, err
		}
		return hook.ContextResult{Messages: messages}, nil
	case hook.EventBeforeProviderRequest, hook.EventBeforeProviderPayload:
		payload, ok, err := providerPayloadResult(result)
		if err != nil || !ok {
			return nil, err
		}
		return hook.BeforeProviderPayloadResult{Payload: payload}, nil
	case hook.EventToolCall:
		input := result.Input
		if input == nil {
			e, ok := event.(hook.ToolCallEvent)
			if ok {
				input, _ = mapFromAny(e.Input)
			}
		}
		return hook.ToolCallResult{
			Block:  boolPtrValue(result.Block),
			Reason: result.Reason,
			Input:  input,
		}, nil
	case hook.EventInput:
		if result.Action == "" {
			return nil, nil
		}
		out := hook.InputResult{Action: result.Action}
		if result.Text != nil {
			out.Text = *result.Text
		}
		if result.ImagesSet {
			images, err := decodeHookImages(result.Images)
			if err != nil {
				return nil, err
			}
			out.Images = images
		}
		return out, nil
	case hook.EventToolResult:
		if !result.ContentSet && !result.DetailsSet && result.IsError == nil && result.Terminate == nil {
			return nil, nil
		}
		patch := hook.ToolResultPatch{
			Details:   result.Details,
			IsError:   result.IsError,
			Terminate: result.Terminate,
		}
		if result.ContentSet {
			content, err := decodeContentBlocks(result.Content)
			if err != nil {
				return nil, err
			}
			patch.Content = content
		}
		return patch, nil
	case hook.EventSessionBeforeSwitch:
		if result.Cancel == nil {
			return nil, nil
		}
		return hook.SessionBeforeSwitchResult{Cancel: *result.Cancel}, nil
	case hook.EventSessionBeforeFork:
		if result.Cancel == nil && result.SkipConversationRestore == nil {
			return nil, nil
		}
		return hook.SessionBeforeForkResult{
			Cancel:                  boolPtrValue(result.Cancel),
			SkipConversationRestore: boolPtrValue(result.SkipConversationRestore),
		}, nil
	case hook.EventMessageEnd:
		if !result.MessageSet {
			return nil, nil
		}
		msg, err := decodeMessage(result.Message)
		if err != nil {
			return nil, err
		}
		return hook.MessageEndResult{Message: msg}, nil
	default:
		return nil, nil
	}
}

func beforeAgentStartResult(result HookResult) (hook.BeforeAgentStartResult, bool, error) {
	out := hook.BeforeAgentStartResult{
		SystemPrompt: result.SystemPrompt,
	}
	if result.MessageSet {
		msg, err := decodeAgentMessage(result.Message)
		if err != nil {
			return hook.BeforeAgentStartResult{}, false, err
		}
		if msg != nil {
			out.Messages = append(out.Messages, msg)
		}
	}
	if result.MessagesSet {
		messages, err := decodeAgentMessages(result.Messages)
		if err != nil {
			return hook.BeforeAgentStartResult{}, false, err
		}
		out.Messages = append(out.Messages, messages...)
	}
	if out.SystemPrompt == nil && len(out.Messages) == 0 {
		return hook.BeforeAgentStartResult{}, false, nil
	}
	return out, true, nil
}

func providerPayloadResult(result HookResult) (any, bool, error) {
	if result.PayloadSet {
		return result.Payload, true, nil
	}
	if len(result.Raw) == 0 || string(result.Raw) == "null" {
		return nil, false, nil
	}

	var payload any
	if err := json.Unmarshal(result.Raw, &payload); err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

func boolPtrValue(value *bool) bool {
	return value != nil && *value
}

func mapFromAny(value any) (map[string]any, bool) {
	switch v := value.(type) {
	case map[string]any:
		return maps.Clone(v), true
	case nil:
		return nil, false
	default:
		var out map[string]any
		if err := decodeValue(v, &out); err != nil {
			return nil, false
		}
		return out, true
	}
}

func decodeMessage(value any) (ai.Message, error) {
	var msg ai.Message
	if err := decodeValue(value, &msg); err != nil {
		return ai.Message{}, fmt.Errorf("jshost: decode message: %w", err)
	}
	return msg, nil
}

func decodeMessages(value any) ([]any, error) {
	var messages []ai.Message
	if err := decodeValue(value, &messages); err != nil {
		return nil, fmt.Errorf("jshost: decode messages: %w", err)
	}
	out := make([]any, 0, len(messages))
	for _, msg := range messages {
		out = append(out, msg)
	}
	return out, nil
}

func decodeAgentMessages(values []any) ([]any, error) {
	out := make([]any, 0, len(values))
	for _, value := range values {
		msg, err := decodeAgentMessage(value)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			out = append(out, msg)
		}
	}
	return out, nil
}

func decodeAgentMessage(value any) (message.AgentMessage, error) {
	switch msg := value.(type) {
	case nil:
		return nil, nil
	case ai.Message:
		return msg, nil
	case *ai.Message:
		if msg == nil {
			return nil, nil
		}
		return *msg, nil
	case message.CustomMessage:
		return msg, nil
	case *message.CustomMessage:
		if msg == nil {
			return nil, nil
		}
		return *msg, nil
	}

	if hasField(value, "customType") {
		var msg message.CustomMessage
		if err := decodeValue(value, &msg); err != nil {
			return nil, fmt.Errorf("jshost: decode before_agent_start custom message: %w", err)
		}
		return msg, nil
	}

	var msg ai.Message
	if err := decodeValue(value, &msg); err != nil {
		return nil, fmt.Errorf("jshost: decode before_agent_start message: %w", err)
	}
	return msg, nil
}

func decodeContentBlocks(value any) ([]ai.ContentBlock, error) {
	var content []ai.ContentBlock
	if err := decodeValue(value, &content); err != nil {
		return nil, fmt.Errorf("jshost: decode tool_result content: %w", err)
	}
	return content, nil
}

func convertToolResultPayload(payload toolResultPayload) (extension.ToolResult, error) {
	content, err := decodeToolResultContent(payload.Content)
	if err != nil {
		return extension.ToolResult{}, err
	}
	return extension.ToolResult{
		Content:   content,
		Details:   payload.Details,
		IsError:   boolPtrValue(payload.IsError),
		Terminate: boolPtrValue(payload.Terminate),
	}, nil
}

func decodeToolResultContent(value any) ([]ai.ContentBlock, error) {
	switch v := value.(type) {
	case nil:
		return []ai.ContentBlock{}, nil
	case string:
		return []ai.ContentBlock{{Type: ai.ContentText, Text: v}}, nil
	case []ai.ContentBlock:
		return append([]ai.ContentBlock(nil), v...), nil
	default:
		return decodeContentBlocks(v)
	}
}

func copyArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	return maps.Clone(args)
}

type jsImageContent struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

func marshalHookImages(images []hook.ImageContent) []jsImageContent {
	if len(images) == 0 {
		return nil
	}
	out := make([]jsImageContent, 0, len(images))
	for _, image := range images {
		out = append(out, jsImageContent{
			MimeType: image.MimeType,
			Data:     image.Data,
		})
	}
	return out
}

// marshalSystemPromptOptions emits pi's BuildSystemPromptOptions field names:
// .agents/references/pi/packages/coding-agent/src/core/system-prompt.ts:8-26.
func marshalSystemPromptOptions(opts *hook.SystemPromptOptions) map[string]any {
	if opts == nil {
		return map[string]any{}
	}
	return map[string]any{
		"customPrompt":       opts.CustomPrompt,
		"selectedTools":      append([]string{}, opts.SelectedTools...),
		"toolSnippets":       cloneHookStringMap(opts.ToolSnippets),
		"promptGuidelines":   append([]string{}, opts.PromptGuidelines...),
		"appendSystemPrompt": opts.AppendSystemPrompt,
		"cwd":                opts.CWD,
		"contextFiles":       marshalContextFiles(opts.ContextFiles),
		"skills":             marshalSkills(opts.Skills),
	}
}

func cloneHookStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func marshalContextFiles(files []hook.ContextFileInfo) []map[string]string {
	out := make([]map[string]string, 0, len(files))
	for _, file := range files {
		out = append(out, map[string]string{
			"path":    file.Path,
			"content": file.Content,
		})
	}
	return out
}

func marshalSkills(skills []hook.SkillInfo) []map[string]string {
	out := make([]map[string]string, 0, len(skills))
	for _, skill := range skills {
		out = append(out, map[string]string{
			"name":        skill.Name,
			"description": skill.Description,
		})
	}
	return out
}

func decodeHookImages(value any) ([]hook.ImageContent, error) {
	var images []jsImageContent
	if err := decodeValue(value, &images); err != nil {
		return nil, fmt.Errorf("jshost: decode input images: %w", err)
	}
	out := make([]hook.ImageContent, 0, len(images))
	for _, image := range images {
		out = append(out, hook.ImageContent{
			MimeType: image.MimeType,
			Data:     image.Data,
		})
	}
	return out, nil
}

func decodeValue(value any, out any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func hasField(value any, name string) bool {
	switch v := value.(type) {
	case map[string]any:
		_, ok := v[name]
		return ok
	default:
		var fields map[string]json.RawMessage
		if err := decodeValue(v, &fields); err != nil {
			return false
		}
		_, ok := fields[name]
		return ok
	}
}

func ctxResponder(extCtx extension.ExtensionContext) ContextResponder {
	return func(ctx context.Context, request ContextRequest) (ContextResponse, error) {
		if err := ctx.Err(); err != nil && request.Method != "isAborted" {
			return ContextResponse{Error: err.Error()}, nil
		}

		value, err := handleContextRequest(ctx, extCtx, request)
		if err != nil {
			return ContextResponse{Error: err.Error()}, nil
		}
		return ContextResponse{Value: value}, nil
	}
}

func handleContextRequest(
	ctx context.Context,
	extCtx extension.ExtensionContext,
	request ContextRequest,
) (any, error) {
	switch request.Method {
	case "sessionManager.getMessages":
		if extCtx.Session == nil {
			return []any{}, nil
		}
		return extCtx.Session.GetMessages(), nil
	case "sessionManager.getEntries":
		if extCtx.Session == nil {
			return []any{}, nil
		}
		return extCtx.Session.GetEntries(), nil
	case "model":
		if extCtx.Model == nil {
			return nil, nil
		}
		return extCtx.Model(), nil
	case "modelRegistry":
		if extCtx.ModelRegistry == nil {
			return []extension.ModelInfo{}, nil
		}
		models := extCtx.ModelRegistry()
		if models == nil {
			return []extension.ModelInfo{}, nil
		}
		return models, nil
	case "isIdle":
		if extCtx.IsIdle == nil {
			return false, nil
		}
		return extCtx.IsIdle(), nil
	case "isAborted":
		if extCtx.IsAborted == nil {
			return false, nil
		}
		return extCtx.IsAborted(), nil
	case "cwd":
		return extCtx.Cwd, nil
	case "hasUI":
		return extCtx.HasUI, nil
	case "hasPendingMessages":
		if extCtx.HasPendingMessages == nil {
			return false, nil
		}
		return extCtx.HasPendingMessages(), nil
	case "getSystemPrompt":
		if extCtx.GetSystemPrompt == nil {
			return "", nil
		}
		return extCtx.GetSystemPrompt(), nil
	case "abort":
		return nil, callContextAction(ctx, extCtx.Abort, "abort")
	case "compact":
		return nil, callContextAction(ctx, extCtx.Compact, "compact")
	case "setModel":
		if extCtx.SetModel == nil {
			return nil, errors.New("jshost: ctx.setModel unavailable")
		}
		provider, err := stringArg(request.Args, "provider")
		if err != nil {
			return nil, err
		}
		id, err := stringArg(request.Args, "id")
		if err != nil {
			return nil, err
		}
		model := ai.Model{Provider: provider, ID: id}
		if builtin, ok := ai.GetModel(provider, id); ok {
			model = builtin
		}
		return nil, extCtx.SetModel(ctx, model)
	case "setThinkingLevel":
		if extCtx.SetThinkingLevel == nil {
			return nil, errors.New("jshost: ctx.setThinkingLevel unavailable")
		}
		level, err := stringArg(request.Args, "level")
		if err != nil {
			return nil, err
		}
		return nil, extCtx.SetThinkingLevel(ctx, extension.ThinkingLevel(level))
	case "steer":
		if extCtx.Steer == nil {
			return nil, errors.New("jshost: ctx.steer unavailable")
		}
		text, err := stringArg(request.Args, "text")
		if err != nil {
			return nil, err
		}
		return nil, extCtx.Steer(ctx, extension.UserInput{Text: text})
	case "followUp":
		if extCtx.FollowUp == nil {
			return nil, errors.New("jshost: ctx.followUp unavailable")
		}
		text, err := stringArg(request.Args, "text")
		if err != nil {
			return nil, err
		}
		return nil, extCtx.FollowUp(ctx, extension.UserInput{Text: text})
	// TODO(jshost): Expose shutdown/newSession/fork/switchSession/
	// navigateTree/reload after the bridge can model pi session replacement
	// callbacks:
	// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:319-320
	// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:333-363.
	default:
		return nil, fmt.Errorf("jshost: unsupported ctx method %q", request.Method)
	}
}

func callContextAction(ctx context.Context, action func(context.Context) error, name string) error {
	if action == nil {
		return fmt.Errorf("jshost: ctx.%s unavailable", name)
	}
	return action(ctx)
}

func stringArg(args map[string]any, name string) (string, error) {
	value, ok := args[name]
	if !ok {
		return "", fmt.Errorf("jshost: ctx arg %q missing", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("jshost: ctx arg %q = %T, want string", name, value)
	}
	return text, nil
}

func uiResponder(ui extension.UIContext) UIResponder {
	if ui == nil {
		ui = extension.NoopUIContext{}
	}

	return func(ctx context.Context, request UIRequest) (UIResponse, error) {
		if err := ctx.Err(); err != nil {
			return UIResponse{Cancelled: true}, err
		}

		switch request.Method {
		case "select":
			index, err := ui.Select(request.Title, append([]string(nil), request.Options...))
			if err != nil || index < 0 || index >= len(request.Options) {
				return UIResponse{Cancelled: true}, nil
			}
			value := request.Options[index]
			return UIResponse{Value: &value}, nil
		case "confirm":
			confirmed, err := ui.Confirm(request.Title)
			if err != nil {
				return UIResponse{Cancelled: true}, nil
			}
			return UIResponse{Confirmed: &confirmed}, nil
		case "input":
			value, err := ui.Input(request.Title)
			if err != nil {
				return UIResponse{Cancelled: true}, nil
			}
			return UIResponse{Value: &value}, nil
		case "notify":
			ui.Notify(request.Message)
			return UIResponse{}, nil
		default:
			return UIResponse{Cancelled: true}, fmt.Errorf("jshost: unsupported UI method %q", request.Method)
		}
	}
}
