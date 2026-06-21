package harness

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

// Prompt runs a complete agent turn. It calls agentloop.Run, handles all
// events, writes to session, and fires hooks. Returns the last assistant
// message.
func (h *Harness) Prompt(ctx context.Context, messages []message.AgentMessage, state TurnState) (ai.Message, error) {
	systemPrompt := state.SystemPrompt
	beforeStart, err := h.emitBeforeAgentStart(ctx, messages, state)
	if err != nil {
		return ai.Message{}, err
	}
	if beforeStart.systemPrompt != nil {
		systemPrompt = *beforeStart.systemPrompt
	}
	if len(beforeStart.messages) > 0 {
		messages = append(messages, beforeStart.messages...)
	}

	return h.run(ctx, messages, state, systemPrompt, false)
}

// Continue resumes the agent loop from the current session context without
// appending a new user message.
func (h *Harness) Continue(ctx context.Context, state TurnState) (ai.Message, error) {
	return h.run(ctx, nil, state, state.SystemPrompt, true)
}

func (h *Harness) run(
	ctx context.Context,
	messages []message.AgentMessage,
	state TurnState,
	systemPrompt string,
	continueRun bool,
) (ai.Message, error) {
	sessionCtx, err := h.cfg.Session.BuildContext(ctx)
	if err != nil {
		return ai.Message{}, err
	}

	activeTools := state.ActiveTools
	if len(activeTools) == 0 {
		activeTools = h.cfg.Tools
	}

	loopCtx := agentloop.Context{
		SystemPrompt: systemPrompt,
		Messages:     sessionCtx.Messages,
		Tools:        activeTools,
	}

	sinkState, emit := h.createTrackingEventSink(ctx, state)
	config := h.createLoopConfig(ctx, state)
	stream := h.createStreamFunc(ctx, state)

	var newMessages []message.AgentMessage
	if continueRun {
		newMessages, err = agentloop.Continue(ctx, loopCtx, config, stream, emit)
	} else {
		newMessages, err = agentloop.Run(ctx, messages, loopCtx, config, stream, emit)
	}
	if err != nil {
		failureMsg, reportErr := h.emitRunFailure(ctx, state, err)
		if reportErr != nil {
			return ai.Message{}, fmt.Errorf(
				"agent run failed and failure reporting failed: %w",
				errors.Join(err, reportErr),
			)
		}
		return failureMsg, nil
	}

	if sinkState.hasLastAssistant {
		return sinkState.lastAssistant, nil
	}
	if len(newMessages) == 0 {
		return ai.Message{}, nil
	}
	for i := len(newMessages) - 1; i >= 0; i-- {
		if aiMsg, ok := message.AsAIMessage(newMessages[i]); ok && aiMsg.Role == ai.RoleAssistant {
			return aiMsg, nil
		}
	}
	return ai.Message{}, nil
}

type beforeAgentStart struct {
	messages     []message.AgentMessage
	systemPrompt *string
}

// emitBeforeAgentStart fires the before_agent_start hook and returns any
// injected messages or system prompt replacement.
func (h *Harness) emitBeforeAgentStart(ctx context.Context, messages []message.AgentMessage, state TurnState) (beforeAgentStart, error) {
	if !h.cfg.Hooks.HasHandlers(hook.EventBeforeAgentStart) {
		return beforeAgentStart{}, nil
	}

	var prompt string
	var images []hook.ImageContent
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

	running := state.SystemPrompt
	modified := false
	out := beforeAgentStart{}
	for _, handler := range h.cfg.Hooks.Handlers(hook.EventBeforeAgentStart) {
		result, err := handler(ctx, hook.BeforeAgentStartEvent{
			Type:                hook.EventBeforeAgentStart,
			Prompt:              prompt,
			Images:              images,
			SystemPrompt:        running,
			SystemPromptOptions: &state.SystemPromptOptions,
			Resources:           state.Resources,
		})
		if err != nil {
			// pi runner.ts:965-973 catches before_agent_start errors and continues.
			continue
		}
		r, ok := result.(hook.BeforeAgentStartResult)
		if !ok {
			continue
		}
		for _, m := range r.Messages {
			if msg, ok := m.(message.AgentMessage); ok {
				out.messages = append(out.messages, msg)
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

// createEventSink returns the EventSink closure that bridges agentloop events
// to session writes and hook dispatch.
func (h *Harness) createEventSink(ctx context.Context, state TurnState) agentloop.EventSink {
	_, emit := h.createTrackingEventSink(ctx, state)
	return emit
}

func (h *Harness) createTrackingEventSink(ctx context.Context, _ TurnState) (*eventSinkState, agentloop.EventSink) {
	sinkState := &eventSinkState{}
	emit := func(event agentloop.Event) error {
		switch event.Type {
		case agentloop.MessageStart:
			_, err := h.cfg.Hooks.Emit(ctx, hook.MessageStartEvent{
				Type:    hook.EventMessageStart,
				Message: event.Message,
			})
			return err

		case agentloop.MessageEnd:
			decision := h.emitMessageEndChain(ctx, event.Message)
			sinkState.recordMessageEnd(event.Message, decision.message, decision.changed)
			if _, err := h.cfg.Session.AppendMessage(ctx, decision.message); err != nil {
				return err
			}
			return nil

		case agentloop.MessageUpdate:
			_, err := h.cfg.Hooks.Emit(ctx, hook.MessageUpdateEvent{
				Type:                  hook.EventMessageUpdate,
				Message:               event.Message,
				AssistantMessageEvent: event.AssistantMessageEvent,
			})
			return err

		case agentloop.TurnStart:
			turnIndex := sinkState.startTurn()
			_, err := h.cfg.Hooks.Emit(ctx, hook.TurnStartEvent{
				Type:      hook.EventTurnStart,
				TurnIndex: turnIndex,
				Timestamp: time.Now().UnixMilli(),
			})
			return err

		case agentloop.TurnEnd:
			_, err := h.cfg.Hooks.Emit(ctx, hook.TurnEndEvent{
				Type:        hook.EventTurnEnd,
				TurnIndex:   sinkState.currentTurnIndex,
				Message:     event.Message,
				ToolResults: toAnySlice(event.ToolResults),
			})
			return err

		case agentloop.AgentStart:
			sinkState.resetTurns()
			_, err := h.cfg.Hooks.Emit(ctx, hook.AgentStartEvent{
				Type: hook.EventAgentStart,
			})
			return err

		case agentloop.AgentEnd:
			messages := sinkState.mapAgentEndMessages(event.Messages)
			_, err := h.cfg.Hooks.Emit(ctx, hook.AgentEndEvent{
				Type:     hook.EventAgentEnd,
				Messages: toAnySliceFromAgent(messages),
			})
			return err

		case agentloop.ToolExecutionStart:
			_, err := h.cfg.Hooks.Emit(ctx, hook.ToolExecutionStartEvent{
				Type:       hook.EventToolExecutionStart,
				ToolCallId: event.ToolCallID,
				ToolName:   event.ToolName,
				Args:       event.Args,
			})
			return err

		case agentloop.ToolExecutionUpdate:
			_, err := h.cfg.Hooks.Emit(ctx, hook.ToolExecutionUpdateEvent{
				Type:          hook.EventToolExecutionUpdate,
				ToolCallId:    event.ToolCallID,
				ToolName:      event.ToolName,
				Args:          event.Args,
				PartialResult: event.PartialResult,
			})
			return err

		case agentloop.ToolExecutionEnd:
			_, err := h.cfg.Hooks.Emit(ctx, hook.ToolExecutionEndEvent{
				Type:       hook.EventToolExecutionEnd,
				ToolCallId: event.ToolCallID,
				ToolName:   event.ToolName,
				Result:     event.Result,
				IsError:    event.IsError,
			})
			return err
		}
		return nil
	}
	return sinkState, emit
}

type eventSinkState struct {
	messageIndex     int
	nextTurnIndex    int
	currentTurnIndex int
	replacements     []messageReplacement
	lastAssistant    ai.Message
	hasLastAssistant bool
}

func (s *eventSinkState) startTurn() int {
	index := s.nextTurnIndex
	s.currentTurnIndex = index
	s.nextTurnIndex++
	return index
}

func (s *eventSinkState) resetTurns() {
	s.nextTurnIndex = 0
	s.currentTurnIndex = 0
}

type messageReplacement struct {
	index       int
	identity    string
	pointer     uintptr
	replacement message.AgentMessage
}

func (s *eventSinkState) recordMessageEnd(
	original message.AgentMessage,
	final message.AgentMessage,
	changed bool,
) {
	index := s.messageIndex
	s.messageIndex++

	if aiMsg, ok := message.AsAIMessage(final); ok && aiMsg.Role == ai.RoleAssistant {
		s.lastAssistant = aiMsg
		s.hasLastAssistant = true
	}
	if !changed {
		return
	}

	s.replacements = append(s.replacements, messageReplacement{
		index:       index,
		identity:    agentMessageIdentity(original),
		pointer:     agentMessagePointer(original),
		replacement: final,
	})
}

func (s *eventSinkState) mapAgentEndMessages(messages []message.AgentMessage) []message.AgentMessage {
	if len(s.replacements) == 0 {
		return messages
	}

	mapped := append([]message.AgentMessage(nil), messages...)
	used := make([]bool, len(s.replacements))
	for i, msg := range mapped {
		replacement, ok := s.replacementForMessage(i, msg, used)
		if ok {
			mapped[i] = replacement
		}
	}
	return mapped
}

func (s *eventSinkState) replacementForMessage(
	index int,
	msg message.AgentMessage,
	used []bool,
) (message.AgentMessage, bool) {
	identity := agentMessageIdentity(msg)
	if identity != "" {
		for i, replacement := range s.replacements {
			if used[i] || replacement.identity != identity {
				continue
			}
			used[i] = true
			return replacement.replacement, true
		}
	}

	pointer := agentMessagePointer(msg)
	if pointer != 0 {
		for i, replacement := range s.replacements {
			if used[i] || replacement.pointer != pointer {
				continue
			}
			used[i] = true
			return replacement.replacement, true
		}
	}

	for i, replacement := range s.replacements {
		if used[i] || replacement.index != index {
			continue
		}
		used[i] = true
		return replacement.replacement, true
	}
	return nil, false
}

type messageEndDecision struct {
	message message.AgentMessage
	changed bool
}

func (h *Harness) emitMessageEndChain(
	ctx context.Context,
	msg message.AgentMessage,
) messageEndDecision {
	current := msg
	changed := false
	diagnostics := []ai.AssistantMessageDiagnostic{}

	for i, handler := range h.cfg.Hooks.Handlers(hook.EventMessageEnd) {
		result, err := handler(ctx, hook.MessageEndEvent{
			Type:    hook.EventMessageEnd,
			Message: current,
		})
		if err != nil {
			diagnostics = append(diagnostics, messageEndHandlerErrorDiagnostic(i, err))
			changed = true
			continue
		}

		replacement, ok := messageEndResultMessage(result)
		if !ok {
			continue
		}

		currentRole, replacementRole, ok := sameMessageEndRole(current, replacement)
		if !ok {
			diagnostics = append(
				diagnostics,
				messageEndRoleMismatchDiagnostic(i, currentRole, replacementRole),
			)
			changed = true
			continue
		}

		current = message.Snapshot(replacement)
		changed = true
	}

	if len(diagnostics) > 0 {
		current = appendMessageDiagnostics(current, diagnostics)
	}

	return messageEndDecision{
		message: current,
		changed: changed,
	}
}

func messageEndResultMessage(result any) (message.AgentMessage, bool) {
	switch r := result.(type) {
	case hook.MessageEndResult:
		return resultAgentMessage(r.Message)
	case *hook.MessageEndResult:
		if r == nil {
			return nil, false
		}
		return resultAgentMessage(r.Message)
	default:
		return nil, false
	}
}

func resultAgentMessage(value any) (message.AgentMessage, bool) {
	if value == nil {
		return nil, false
	}
	msg, ok := value.(message.AgentMessage)
	return msg, ok
}

func sameMessageEndRole(
	current message.AgentMessage,
	replacement message.AgentMessage,
) (ai.Role, ai.Role, bool) {
	currentAI, currentOK := message.AsAIMessage(current)
	replacementAI, replacementOK := message.AsAIMessage(replacement)
	if !currentOK || !replacementOK {
		return currentAI.Role, replacementAI.Role, false
	}
	return currentAI.Role, replacementAI.Role, currentAI.Role == replacementAI.Role
}

func messageEndHandlerErrorDiagnostic(index int, err error) ai.AssistantMessageDiagnostic {
	return ai.AssistantMessageDiagnostic{
		Source:  hook.EventMessageEnd,
		Message: "message_end handler error: " + err.Error(),
		Fields: map[string]any{
			"handlerIndex": index,
		},
	}
}

func messageEndRoleMismatchDiagnostic(
	index int,
	currentRole ai.Role,
	replacementRole ai.Role,
) ai.AssistantMessageDiagnostic {
	return ai.AssistantMessageDiagnostic{
		Source:  hook.EventMessageEnd,
		Message: "message_end handlers must return a message with the same role",
		Fields: map[string]any{
			"handlerIndex":    index,
			"currentRole":     string(currentRole),
			"replacementRole": string(replacementRole),
		},
	}
}

func appendMessageDiagnostics(
	msg message.AgentMessage,
	diagnostics []ai.AssistantMessageDiagnostic,
) message.AgentMessage {
	switch m := msg.(type) {
	case ai.Message:
		m.Diagnostics = appendMessageDiagnosticSlice(m.Diagnostics, diagnostics)
		return m
	case *ai.Message:
		if m == nil {
			return msg
		}
		next := *m
		next.Diagnostics = appendMessageDiagnosticSlice(next.Diagnostics, diagnostics)
		return next
	default:
		return msg
	}
}

func appendMessageDiagnosticSlice(
	base []ai.AssistantMessageDiagnostic,
	diagnostics []ai.AssistantMessageDiagnostic,
) []ai.AssistantMessageDiagnostic {
	out := make([]ai.AssistantMessageDiagnostic, 0, len(base)+len(diagnostics))
	out = append(out, base...)
	out = append(out, diagnostics...)
	return out
}

func agentMessageIdentity(msg message.AgentMessage) string {
	aiMsg, ok := message.AsAIMessage(msg)
	if !ok {
		return ""
	}
	if aiMsg.ResponseID != "" {
		return string(aiMsg.Role) + ":response:" + aiMsg.ResponseID
	}
	if aiMsg.ToolCallID != "" {
		return string(aiMsg.Role) + ":tool:" + aiMsg.ToolCallID
	}
	return ""
}

func agentMessagePointer(msg message.AgentMessage) uintptr {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return 0
	}
	return v.Pointer()
}

// createLoopConfig assembles agentloop.Config with callbacks bridged to
// hook.Registry.
func (h *Harness) createLoopConfig(_ context.Context, state TurnState) agentloop.Config {
	return agentloop.Config{
		Model:               state.Model,
		ThinkingLevel:       state.ThinkingLevel,
		SessionID:           state.SessionID,
		ConvertToLLM:        message.DefaultConvertToLLM,
		GetSteeringMessages: state.GetSteeringMessages,
		GetFollowUpMessages: state.GetFollowUpMessages,

		TransformContext: func(ctx context.Context, messages []message.AgentMessage) ([]message.AgentMessage, error) {
			// pi threads the transformed messages through the chain — each
			// handler receives the previous handler's output, so multiple
			// extensions' transforms compose (runner.ts:858-888).
			current := toAnySliceFromAgent(messages)
			changed := false
			for _, handler := range h.cfg.Hooks.Handlers(hook.EventContext) {
				result, err := handler(ctx, hook.ContextEvent{
					Type:     hook.EventContext,
					Messages: current,
				})
				if err != nil {
					h.cfg.Hooks.ReportHandlerError(hook.EventContext, err)
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
		},

		BeforeToolCall: func(ctx context.Context, callCtx agentloop.BeforeToolCallContext) (*agentloop.BeforeToolCallResult, error) {
			currentInput := callCtx.Args
			inputChanged := false
			var last *agentloop.BeforeToolCallResult
			for _, handler := range h.cfg.Hooks.Handlers(hook.EventToolCall) {
				event := hook.ToolCallEvent{
					Type:       hook.EventToolCall,
					ToolCallId: callCtx.ToolCall.ToolCallID,
					ToolName:   callCtx.ToolCall.ToolName,
					Input:      currentInput,
				}
				result, err := handler(ctx, event)
				if err != nil {
					return nil, err
				}
				if result == nil {
					continue
				}
				if r, ok := result.(hook.ToolCallResult); ok {
					last = &agentloop.BeforeToolCallResult{
						Block:  r.Block,
						Reason: r.Reason,
					}
					if r.Block {
						return last, nil
					}
					if r.Input != nil {
						currentInput = r.Input
						inputChanged = true
					}
				}
			}
			if inputChanged {
				if last == nil {
					last = &agentloop.BeforeToolCallResult{}
				}
				last.Args = currentInput
			}
			return last, nil
		},

		AfterToolCall: func(ctx context.Context, callCtx agentloop.AfterToolCallContext) (*agentloop.AfterToolCallResult, error) {
			// pi accumulates field-level patches into a shared event:
			// content/details/isError patches from different extensions
			// compose, and each handler sees the previous patches
			// (runner.ts:756-790).
			event := hook.ToolResultEvent{
				Type:       hook.EventToolResult,
				ToolCallId: callCtx.ToolCall.ToolCallID,
				ToolName:   callCtx.ToolCall.ToolName,
				Input:      callCtx.Args,
				Content:    append([]ai.ContentBlock(nil), callCtx.Result.Content...),
				Details:    callCtx.Result.Details,
				IsError:    callCtx.IsError,
			}
			modified := false
			var terminate *bool
			for _, handler := range h.cfg.Hooks.Handlers(hook.EventToolResult) {
				result, err := handler(ctx, event)
				if err != nil {
					h.cfg.Hooks.ReportHandlerError(hook.EventToolResult, err)
					continue
				}
				r, ok := result.(hook.ToolResultPatch)
				if !ok {
					continue
				}
				if r.Content != nil {
					event.Content = append([]ai.ContentBlock(nil), r.Content...)
					modified = true
				}
				if r.Details != nil {
					event.Details = r.Details
					modified = true
				}
				if r.IsError != nil {
					event.IsError = *r.IsError
					modified = true
				}
				if r.Terminate != nil {
					terminate = r.Terminate
					modified = true
				}
			}
			if !modified {
				return nil, nil
			}
			isError := event.IsError
			return &agentloop.AfterToolCallResult{
				Content:   append([]ai.ContentBlock(nil), event.Content...),
				Details:   event.Details,
				IsError:   &isError,
				Terminate: terminate,
			}, nil
		},

		PrepareNextTurn: func(ctx context.Context, callCtx agentloop.PrepareNextTurnContext) (*agentloop.TurnUpdate, error) {
			sessionCtx, err := h.cfg.Session.BuildContext(ctx)
			if err != nil {
				return nil, err
			}
			newCtx := &agentloop.Context{
				SystemPrompt: callCtx.Context.SystemPrompt,
				Messages:     sessionCtx.Messages,
				Tools:        callCtx.Context.Tools,
			}
			model := state.Model
			thinkingLevel := state.ThinkingLevel
			if state.Refresh != nil {
				model, thinkingLevel = state.Refresh()
			}
			return &agentloop.TurnUpdate{
				Context:       newCtx,
				Model:         &model,
				ThinkingLevel: &thinkingLevel,
			}, nil
		},
	}
}

// createStreamFunc assembles the StreamFunc that bridges provider hooks and
// auth.
func (h *Harness) createStreamFunc(_ context.Context, state TurnState) agentloop.StreamFunc {
	return func(
		ctx context.Context,
		model ai.Model,
		llmCtx ai.Context,
		opts ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		// Auth.
		auth, err := h.providerAuth(ctx, model)
		if err != nil {
			return streamError(err)
		}
		if auth.APIKey != "" {
			opts.APIKey = auth.APIKey
		}
		if len(auth.Headers) > 0 {
			headers := maps.Clone(opts.Headers)
			if headers == nil {
				headers = map[string]string{}
			}
			maps.Copy(headers, auth.Headers)
			opts.Headers = headers
		}

		// before_provider_request — chain patch semantics.
		if h.cfg.Hooks.HasHandlers(hook.EventBeforeProviderRequest) {
			patched, err := h.emitBeforeProviderRequest(ctx, model, state.SessionID, opts)
			if err != nil {
				return streamError(err)
			}
			opts = patched
		}

		onPayload := opts.OnPayload
		opts.OnPayload = func(payload any, model ai.Model) (any, bool, error) {
			current := payload
			changed := false
			if h.cfg.Hooks.HasHandlers(hook.EventBeforeProviderPayload) {
				next, hookChanged, err := h.emitBeforeProviderPayload(ctx, model, current)
				if err != nil {
					return nil, false, err
				}
				current = next
				changed = hookChanged
			}
			if onPayload == nil {
				return current, changed, nil
			}
			next, payloadChanged, err := onPayload(current, model)
			if err != nil {
				return nil, false, err
			}
			if payloadChanged {
				current = next
				changed = true
			}
			return current, changed, nil
		}

		onResponse := opts.OnResponse
		opts.OnResponse = func(response ai.ProviderResponse, model ai.Model) error {
			if onResponse != nil {
				if err := onResponse(response, model); err != nil {
					return err
				}
			}
			if !h.cfg.Hooks.HasHandlers(hook.EventAfterProviderResponse) {
				return nil
			}
			_, err := h.cfg.Hooks.Emit(ctx, hook.AfterProviderResponseEvent{
				Type:    hook.EventAfterProviderResponse,
				Status:  response.Status,
				Headers: maps.Clone(response.Headers),
			})
			return err
		}

		stream := h.cfg.stream
		if stream == nil {
			stream = ai.StreamSimple
		}
		return stream(ctx, model, llmCtx, opts)
	}
}

// emitBeforeProviderRequest iterates through before_provider_request handlers
// with pi's chain patch semantics: each handler sees accumulated options.
func (h *Harness) emitBeforeProviderRequest(
	ctx context.Context,
	model ai.Model,
	sessionID string,
	opts ai.SimpleStreamOptions,
) (ai.SimpleStreamOptions, error) {
	current := cloneStreamOptions(opts)
	for _, handler := range h.cfg.Hooks.Handlers(hook.EventBeforeProviderRequest) {
		result, err := handler(ctx, hook.BeforeProviderRequestEvent{
			Type:          hook.EventBeforeProviderRequest,
			Model:         model,
			SessionID:     sessionID,
			StreamOptions: cloneStreamOptions(current),
		})
		if err != nil {
			return current, err
		}
		if result == nil {
			continue
		}
		r, ok := result.(hook.BeforeProviderRequestResult)
		if !ok {
			continue
		}
		current = applyStreamOptionsPatch(current, r.StreamOptions)
	}
	return current, nil
}

func (h *Harness) emitBeforeProviderPayload(
	ctx context.Context,
	model ai.Model,
	payload any,
) (any, bool, error) {
	current := payload
	changed := false
	for _, handler := range h.cfg.Hooks.Handlers(hook.EventBeforeProviderPayload) {
		result, err := handler(ctx, hook.BeforeProviderPayloadEvent{
			Type:    hook.EventBeforeProviderPayload,
			Model:   model,
			Payload: current,
		})
		if err != nil {
			return nil, false, err
		}
		if result == nil {
			continue
		}
		r, ok := result.(hook.BeforeProviderPayloadResult)
		if !ok {
			continue
		}
		current = r.Payload
		changed = true
	}
	return current, changed, nil
}

func applyStreamOptionsPatch(opts ai.SimpleStreamOptions, result any) ai.SimpleStreamOptions {
	switch patch := result.(type) {
	case nil:
		return opts
	case ai.SimpleStreamOptions:
		return patch
	case *ai.SimpleStreamOptions:
		if patch == nil {
			return opts
		}
		return *patch
	case *hook.StreamOptionsPatch:
		if patch == nil {
			return opts
		}
		return applyStreamOptionsPatch(opts, *patch)
	case hook.StreamOptionsPatch:
		opts = cloneStreamOptions(opts)
		if patch.Temperature != nil {
			opts.Temperature = *patch.Temperature
		}
		if patch.MaxTokens != nil {
			opts.MaxTokens = *patch.MaxTokens
		}
		if patch.APIKey != nil {
			opts.APIKey = *patch.APIKey
		}
		if patch.Transport != nil {
			opts.Transport = ai.Transport(*patch.Transport)
		}
		if patch.CacheRetention != nil {
			opts.CacheRetention = ai.CacheRetention(*patch.CacheRetention)
		}
		if patch.SessionID != nil {
			opts.SessionID = *patch.SessionID
		}
		if patch.ClearHeaders {
			opts.Headers = nil
		}
		if patch.Headers != nil {
			headers := maps.Clone(opts.Headers)
			if headers == nil {
				headers = map[string]string{}
			}
			for key, value := range patch.Headers {
				if value == nil {
					delete(headers, key)
					continue
				}
				headers[key] = *value
			}
			if len(headers) == 0 {
				headers = nil
			}
			opts.Headers = headers
		}
		if patch.Timeout != nil {
			opts.Timeout = *patch.Timeout
		}
		if patch.MaxRetries != nil {
			opts.MaxRetries = *patch.MaxRetries
		}
		if patch.ClearMetadata {
			opts.Metadata = nil
		}
		if patch.Metadata != nil {
			metadata := maps.Clone(opts.Metadata)
			if metadata == nil {
				metadata = map[string]any{}
			}
			for key, value := range patch.Metadata {
				if value == nil {
					delete(metadata, key)
					continue
				}
				metadata[key] = value
			}
			if len(metadata) == 0 {
				metadata = nil
			}
			opts.Metadata = metadata
		}
		if patch.Reasoning != nil {
			opts.Reasoning = *patch.Reasoning
		}
		if patch.ThinkingBudgets != nil {
			opts.ThinkingBudgets = copyAIThinkingBudgets(patch.ThinkingBudgets)
		}
		return opts
	default:
		return opts
	}
}

func cloneStreamOptions(opts ai.SimpleStreamOptions) ai.SimpleStreamOptions {
	opts.Headers = maps.Clone(opts.Headers)
	opts.Metadata = maps.Clone(opts.Metadata)
	opts.ThinkingBudgets = copyAIThinkingBudgets(opts.ThinkingBudgets)
	return opts
}

func copyAIThinkingBudgets(budgets *ai.ThinkingBudgets) *ai.ThinkingBudgets {
	if budgets == nil {
		return nil
	}
	copied := *budgets
	return &copied
}

// emitRunFailure generates a synthetic failure message and drives it through
// the full event lifecycle: message_start → message_end → turn_end → agent_end.
// This ensures the session records the error and the UI receives a complete
// event stream.
func (h *Harness) emitRunFailure(ctx context.Context, state TurnState, runErr error) (ai.Message, error) {
	stopReason := ai.StopReasonError
	if ctx.Err() != nil {
		stopReason = ai.StopReasonAborted
	}

	// pi's createFailureMessage keeps the text content empty — the error
	// lives only in errorMessage so it never becomes assistant prose in the
	// session or future LLM context — and zeroes usage explicitly
	// (agent-harness.ts:49-68).
	failureMsg := ai.Message{
		Role:         ai.RoleAssistant,
		Content:      []ai.ContentBlock{{Type: ai.ContentText, Text: ""}},
		API:          state.Model.API,
		Provider:     state.Model.Provider,
		Model:        state.Model.ID,
		StopReason:   stopReason,
		ErrorMessage: runErr.Error(),
		Timestamp:    time.Now().UnixMilli(),
		Usage:        &ai.Usage{Cost: &ai.Cost{}},
	}

	sinkState, emit := h.createTrackingEventSink(ctx, state)
	if err := emit(agentloop.Event{Type: agentloop.MessageStart, Message: failureMsg}); err != nil {
		return ai.Message{}, err
	}
	if err := emit(agentloop.Event{Type: agentloop.MessageEnd, Message: failureMsg}); err != nil {
		return ai.Message{}, err
	}
	if err := emit(agentloop.Event{
		Type:        agentloop.TurnEnd,
		Message:     failureMsg,
		ToolResults: []ai.Message{},
	}); err != nil {
		return ai.Message{}, err
	}
	if err := emit(agentloop.Event{
		Type:     agentloop.AgentEnd,
		Messages: []message.AgentMessage{failureMsg},
	}); err != nil {
		return ai.Message{}, err
	}
	if sinkState.hasLastAssistant {
		return sinkState.lastAssistant, nil
	}
	return failureMsg, nil
}

// toAnySlice converts []ai.Message to []any.
func toAnySlice(msgs []ai.Message) []any {
	result := make([]any, len(msgs))
	for i, m := range msgs {
		result[i] = m
	}
	return result
}

// toAnySliceFromAgent converts []message.AgentMessage to []any.
func toAnySliceFromAgent(msgs []message.AgentMessage) []any {
	result := make([]any, len(msgs))
	for i, m := range msgs {
		result[i] = m
	}
	return result
}

// fromAnySlice converts []any to []message.AgentMessage, skipping non-AgentMessage values.
func fromAnySlice(items []any) []message.AgentMessage {
	var result []message.AgentMessage
	for _, item := range items {
		if msg, ok := item.(message.AgentMessage); ok {
			result = append(result, msg)
		}
	}
	return result
}

// textFromContent extracts text from content blocks.
func textFromContent(blocks []ai.ContentBlock) string {
	var text strings.Builder
	for _, b := range blocks {
		if b.Type == ai.ContentText {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

func streamError(err error) *ai.AssistantMessageEventStream {
	return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
		yield(nil, err)
	})
}
