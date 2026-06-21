package pipeline

import (
	"context"
	"strings"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
)

type PromptTool struct {
	Name              string
	PromptSnippet     string
	PromptGuidelines  []string
	AvailableToPrompt bool
}

type ContextConfig struct {
	Hooks              *hook.Registry
	Tools              []PromptTool
	ContextFiles       []resource.ContextFile
	Skills             []resource.Skill
	Resources          any
	AppendSystemPrompt string
}

type beforeAgentStartResult struct {
	messages     []message.AgentMessage
	systemPrompt *string
}

func MWContext(cfg ContextConfig) RunMiddleware {
	return func(ctx context.Context, tc *RunContext, next RunHandler) error {
		promptOptions := systemPromptOptions(cfg, tc)
		tc.Agent.SystemPrompt = resource.BuildSystemPrompt(promptOptions)
		hookOptions := hookSystemPromptOptions(promptOptions)

		messages := []message.AgentMessage{}
		if tc.Session != nil {
			messages = append(messages, tc.Session.Messages()...)
		}
		if tc.Input != "" {
			messages = append(messages, inputMessage(tc.Input))
		}

		beforeStart, err := emitBeforeAgentStart(
			ctx,
			cfg.Hooks,
			messages,
			tc.Agent.SystemPrompt,
			hookOptions,
			cfg.Resources,
		)
		if err != nil {
			return err
		}
		if beforeStart.systemPrompt != nil {
			tc.Agent.SystemPrompt = *beforeStart.systemPrompt
		}
		if len(beforeStart.messages) > 0 {
			messages = append(messages, beforeStart.messages...)
		}

		transformed, err := transformContextMessages(ctx, cfg.Hooks, messages)
		if err != nil {
			return err
		}
		tc.Messages = transformed
		return next(ctx, tc)
	}
}

func inputMessage(input string) ai.Message {
	return ai.Message{
		Role:      ai.RoleUser,
		Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: input}},
		Timestamp: time.Now().UnixMilli(),
	}
}

func systemPromptOptions(cfg ContextConfig, tc *RunContext) resource.SystemPromptOptions {
	tools := make([]resource.ToolInfo, 0, len(cfg.Tools))
	guidelines := []string{}
	hasReadTool := false
	for _, tool := range cfg.Tools {
		tools = append(tools, resource.ToolInfo{
			Name:    tool.Name,
			Snippet: strings.TrimSpace(tool.PromptSnippet),
		})
		guidelines = append(guidelines, tool.PromptGuidelines...)
		if tool.Name == "read" {
			hasReadTool = true
		}
	}
	return resource.SystemPromptOptions{
		CustomPrompt:       tc.Agent.SystemPrompt,
		AppendSystemPrompt: cfg.AppendSystemPrompt,
		CWD:                tc.Env.CWD,
		Tools:              tools,
		ContextFiles:       append([]resource.ContextFile(nil), cfg.ContextFiles...),
		Skills:             append([]resource.Skill(nil), cfg.Skills...),
		Guidelines:         guidelines,
		HasReadTool:        hasReadTool,
	}
}

func hookSystemPromptOptions(opts resource.SystemPromptOptions) hook.SystemPromptOptions {
	toolSnippets := make(map[string]string, len(opts.Tools))
	selectedTools := make([]string, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		selectedTools = append(selectedTools, tool.Name)
		if tool.Snippet != "" {
			toolSnippets[tool.Name] = tool.Snippet
		}
	}

	contextFiles := make([]hook.ContextFileInfo, 0, len(opts.ContextFiles))
	for _, file := range opts.ContextFiles {
		contextFiles = append(contextFiles, hook.ContextFileInfo{
			Path:    file.Path,
			Content: file.Content,
		})
	}

	skills := make([]hook.SkillInfo, 0, len(opts.Skills))
	for _, skill := range opts.Skills {
		skills = append(skills, hook.SkillInfo{
			Name:        skill.Name,
			Description: skill.Description,
		})
	}

	return hook.SystemPromptOptions{
		CustomPrompt:       opts.CustomPrompt,
		SelectedTools:      selectedTools,
		ToolSnippets:       toolSnippets,
		PromptGuidelines:   append([]string{}, opts.Guidelines...),
		AppendSystemPrompt: opts.AppendSystemPrompt,
		CWD:                opts.CWD,
		ContextFiles:       contextFiles,
		Skills:             skills,
	}
}

func emitBeforeAgentStart(
	ctx context.Context,
	registry *hook.Registry,
	messages []message.AgentMessage,
	systemPrompt string,
	systemPromptOptions hook.SystemPromptOptions,
	resources any,
) (beforeAgentStartResult, error) {
	if registry == nil || !registry.HasHandlers(hook.EventBeforeAgentStart) {
		return beforeAgentStartResult{}, nil
	}

	var prompt string
	images := []hook.ImageContent{}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := message.AsAIMessage(messages[i])
		if !ok || msg.Role != ai.RoleUser {
			continue
		}
		prompt = textFromContent(msg.Content)
		for _, block := range msg.Content {
			if block.Type != ai.ContentImage {
				continue
			}
			images = append(images, hook.ImageContent{
				MimeType: block.MimeType,
				Data:     block.ImageData,
			})
		}
		break
	}

	running := systemPrompt
	modified := false
	out := beforeAgentStartResult{}
	for _, handler := range registry.Handlers(hook.EventBeforeAgentStart) {
		result, err := handler(ctx, hook.BeforeAgentStartEvent{
			Type:                hook.EventBeforeAgentStart,
			Prompt:              prompt,
			Images:              images,
			SystemPrompt:        running,
			SystemPromptOptions: &systemPromptOptions,
			Resources:           resources,
		})
		if err != nil {
			registry.ReportHandlerError(hook.EventBeforeAgentStart, err)
			continue
		}
		r, ok := result.(hook.BeforeAgentStartResult)
		if !ok {
			continue
		}
		for _, item := range r.Messages {
			if msg, ok := item.(message.AgentMessage); ok {
				out.messages = append(out.messages, message.Snapshot(msg))
			}
		}
		if r.SystemPrompt != nil {
			running = *r.SystemPrompt
			modified = true
		}
	}
	if modified {
		out.systemPrompt = &running
	}
	return out, nil
}

func transformContextMessages(
	ctx context.Context,
	registry *hook.Registry,
	messages []message.AgentMessage,
) ([]message.AgentMessage, error) {
	if registry == nil || !registry.HasHandlers(hook.EventContext) {
		return messages, nil
	}

	current := toAnySliceFromAgent(messages)
	changed := false
	for _, handler := range registry.Handlers(hook.EventContext) {
		result, err := handler(ctx, hook.ContextEvent{
			Type:     hook.EventContext,
			Messages: current,
		})
		if err != nil {
			registry.ReportHandlerError(hook.EventContext, err)
			continue
		}
		if r, ok := result.(hook.ContextResult); ok && r.Messages != nil {
			current = r.Messages
			changed = true
		}
	}
	if !changed {
		return messages, nil
	}
	return fromAnySlice(current), nil
}

func toAnySliceFromAgent(messages []message.AgentMessage) []any {
	out := make([]any, len(messages))
	for i, msg := range messages {
		out[i] = msg
	}
	return out
}

func fromAnySlice(items []any) []message.AgentMessage {
	out := []message.AgentMessage{}
	for _, item := range items {
		if msg, ok := item.(message.AgentMessage); ok {
			out = append(out, message.Snapshot(msg))
		}
	}
	return out
}

func textFromContent(blocks []ai.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Type == ai.ContentText {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}
