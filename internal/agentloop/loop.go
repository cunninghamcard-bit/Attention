package agentloop

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

var (
	errNoMessages          = errors.New("cannot continue: no messages in context")
	errCannotContinue      = errors.New("cannot continue from message role: assistant")
	errMissingConvertToLLM = errors.New("agent loop missing convert to llm")
	errMissingEventSink    = errors.New("agent loop missing event sink")
)

// Run starts a new agent loop with prompt messages.
func Run(
	ctx context.Context,
	prompts []message.AgentMessage,
	agCtx Context,
	config Config,
	stream StreamFunc,
	emit EventSink,
) ([]message.AgentMessage, error) {
	if emit == nil {
		return nil, errMissingEventSink
	}
	return run(ctx, prompts, agCtx, config, stream, emit)
}

func run(
	ctx context.Context,
	prompts []message.AgentMessage,
	agCtx Context,
	config Config,
	stream StreamFunc,
	emit EventSink,
) ([]message.AgentMessage, error) {
	newMessages := make([]message.AgentMessage, 0, len(prompts))

	currentContext := Context{
		SystemPrompt: agCtx.SystemPrompt,
		Messages:     make([]message.AgentMessage, 0, len(agCtx.Messages)+len(prompts)),
		Tools:        agCtx.Tools,
	}
	currentContext.Messages = append(currentContext.Messages, agCtx.Messages...)

	if err := emit(Event{Type: AgentStart}); err != nil {
		return nil, err
	}
	if err := emit(Event{Type: TurnStart}); err != nil {
		return nil, err
	}
	for _, prompt := range prompts {
		msg := message.Snapshot(prompt)
		newMessages = append(newMessages, msg)
		currentContext.Messages = append(currentContext.Messages, msg)
		if err := emit(Event{Type: MessageStart, Message: msg}); err != nil {
			return nil, err
		}
		if err := emit(Event{Type: MessageEnd, Message: msg}); err != nil {
			return nil, err
		}
	}

	err := runLoop(ctx, &currentContext, &newMessages, config, stream, emit)
	if err != nil {
		return nil, err
	}
	return newMessages, nil
}

// Continue continues an agent loop from the current context.
func Continue(
	ctx context.Context,
	agCtx Context,
	config Config,
	stream StreamFunc,
	emit EventSink,
) ([]message.AgentMessage, error) {
	if emit == nil {
		return nil, errMissingEventSink
	}
	return continueRun(ctx, agCtx, config, stream, emit)
}

func continueRun(
	ctx context.Context,
	agCtx Context,
	config Config,
	stream StreamFunc,
	emit EventSink,
) ([]message.AgentMessage, error) {
	if len(agCtx.Messages) == 0 {
		return nil, errNoMessages
	}

	last := agCtx.Messages[len(agCtx.Messages)-1]
	if msg, ok := message.AsAIMessage(last); ok && msg.Role == ai.RoleAssistant {
		return nil, errCannotContinue
	}

	newMessages := []message.AgentMessage{}
	currentContext := Context{
		SystemPrompt: agCtx.SystemPrompt,
		Messages:     make([]message.AgentMessage, 0, len(agCtx.Messages)),
		Tools:        agCtx.Tools,
	}
	for _, msg := range agCtx.Messages {
		currentContext.Messages = append(currentContext.Messages, message.Snapshot(msg))
	}

	if err := emit(Event{Type: AgentStart}); err != nil {
		return nil, err
	}
	if err := emit(Event{Type: TurnStart}); err != nil {
		return nil, err
	}

	err := runLoop(ctx, &currentContext, &newMessages, config, stream, emit)
	if err != nil {
		return nil, err
	}
	return newMessages, nil
}

func runLoop(
	ctx context.Context,
	currentContext *Context,
	newMessages *[]message.AgentMessage,
	config Config,
	stream StreamFunc,
	emit EventSink,
) error {
	firstTurn := true

	var pending []message.AgentMessage
	var err error
	if config.GetSteeringMessages != nil {
		pending, err = config.GetSteeringMessages(ctx)
		if err != nil {
			return err
		}
	}

	for {
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pending) > 0 {
			if !firstTurn {
				if err := emit(Event{Type: TurnStart}); err != nil {
					return err
				}
			} else {
				firstTurn = false
			}

			for _, pendingMessage := range pending {
				msg := message.Snapshot(pendingMessage)
				if err := emit(Event{Type: MessageStart, Message: msg}); err != nil {
					return err
				}
				if err := emit(Event{Type: MessageEnd, Message: msg}); err != nil {
					return err
				}
				currentContext.Messages = append(currentContext.Messages, msg)
				*newMessages = append(*newMessages, msg)
			}
			pending = nil

			msg, err := streamAssistantResponse(ctx, currentContext, config, stream, emit)
			if err != nil {
				return err
			}
			*newMessages = append(*newMessages, *msg)

			if msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted {
				if err := emit(Event{Type: TurnEnd, Message: *msg, ToolResults: []ai.Message{}}); err != nil {
					return err
				}
				return emit(Event{Type: AgentEnd, Messages: *newMessages})
			}

			toolCalls := extractToolCalls(msg)
			toolResults := []ai.Message{}
			hasMoreToolCalls = false

			if len(toolCalls) > 0 {
				batch, err := executeToolCalls(ctx, currentContext, msg, config, emit)
				if err != nil {
					return err
				}
				toolResults = batch.messages
				hasMoreToolCalls = !batch.terminate

				for _, tr := range toolResults {
					currentContext.Messages = append(currentContext.Messages, tr)
					*newMessages = append(*newMessages, tr)
				}
			}

			if err := emit(Event{
				Type:        TurnEnd,
				Message:     *msg,
				ToolResults: toolResults,
			}); err != nil {
				return err
			}

			nextCtx := PrepareNextTurnContext{
				Message:     msg,
				ToolResults: toolResults,
				Context:     *currentContext,
				NewMessages: *newMessages,
			}
			if config.PrepareNextTurn != nil {
				update, err := config.PrepareNextTurn(ctx, nextCtx)
				if err != nil {
					return err
				}
				if update != nil {
					if update.Context != nil {
						*currentContext = *update.Context
					}
					if update.Model != nil {
						config.Model = *update.Model
					}
					if update.ThinkingLevel != nil {
						config.ThinkingLevel = *update.ThinkingLevel
					}
				}
			}

			stopCtx := ShouldStopAfterTurnContext{
				Message:     msg,
				ToolResults: toolResults,
				Context:     *currentContext,
				NewMessages: *newMessages,
			}
			if config.ShouldStopAfterTurn != nil {
				stop, err := config.ShouldStopAfterTurn(ctx, stopCtx)
				if err != nil {
					return err
				}
				if stop {
					return emit(Event{Type: AgentEnd, Messages: *newMessages})
				}
			}

			pending = nil
			if config.GetSteeringMessages != nil {
				pending, err = config.GetSteeringMessages(ctx)
				if err != nil {
					return err
				}
			}
		}

		followUps := []message.AgentMessage{}
		if config.GetFollowUpMessages != nil {
			followUps, err = config.GetFollowUpMessages(ctx)
			if err != nil {
				return err
			}
		}
		if len(followUps) > 0 {
			pending = followUps
			continue
		}
		break
	}

	return emit(Event{Type: AgentEnd, Messages: *newMessages})
}

func streamAssistantResponse(
	ctx context.Context,
	agCtx *Context,
	config Config,
	stream StreamFunc,
	emit EventSink,
) (*ai.Message, error) {
	messages := agCtx.Messages
	if config.TransformContext != nil {
		var err error
		messages, err = config.TransformContext(ctx, messages)
		if err != nil {
			return nil, err
		}
	}

	if config.ConvertToLLM == nil {
		return nil, errMissingConvertToLLM
	}
	llmMessages, err := config.ConvertToLLM(messages)
	if err != nil {
		return nil, err
	}

	apiKey := config.APIKey
	if config.GetAPIKey != nil {
		key, err := config.GetAPIKey(ctx, config.Model.Provider)
		if err != nil {
			return nil, err
		}
		if key != "" {
			apiKey = key
		}
	}

	tools := make([]ai.Tool, 0, len(agCtx.Tools))
	for _, t := range agCtx.Tools {
		tools = append(tools, t.Tool)
	}

	llmCtx := ai.Context{
		SystemPrompt: agCtx.SystemPrompt,
		Messages:     llmMessages,
		Tools:        tools,
	}
	opts := ai.SimpleStreamOptions{
		Temperature:    config.Temperature,
		MaxTokens:      config.MaxTokens,
		APIKey:         apiKey,
		Transport:      config.Transport,
		CacheRetention: config.CacheRetention,
		SessionID:      config.SessionID,
		Headers:        config.Headers,
		Timeout:        config.Timeout,
		MaxRetries:     config.MaxRetries,
		Metadata:       config.Metadata,
		Reasoning:      reasoningFromThinkingLevel(config.ThinkingLevel),
		ThinkingBudgets: aiThinkingBudgets(
			config.ThinkingBudgets,
		),
		OnPayload:  config.OnPayload,
		OnResponse: config.OnResponse,
	}

	streamFunc := stream
	if streamFunc == nil {
		streamFunc = ai.StreamSimple
	}
	response := streamFunc(ctx, config.Model, llmCtx, opts)

	started := false

	for event, err := range response.Iter() {
		if err != nil {
			result, resErr := response.Result()
			if resErr != nil {
				return nil, err
			}
			msg := *result
			if started {
				agCtx.Messages[len(agCtx.Messages)-1] = msg
			} else {
				agCtx.Messages = append(agCtx.Messages, msg)
			}
			if !started {
				if err := emit(Event{Type: MessageStart, Message: msg}); err != nil {
					return nil, err
				}
			}
			if err := emit(Event{Type: MessageEnd, Message: *result}); err != nil {
				return nil, err
			}
			return result, nil
		}
		if event == nil {
			continue
		}

		switch event.Type {
		case ai.EventMessageStart:
			if event.Message == nil {
				continue
			}
			msg := *event.Message
			if started {
				agCtx.Messages[len(agCtx.Messages)-1] = msg
				continue
			}
			agCtx.Messages = append(agCtx.Messages, msg)
			started = true
			if err := emit(Event{Type: MessageStart, Message: msg}); err != nil {
				return nil, err
			}

		case ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd,
			ai.EventThinkingStart, ai.EventThinkingDelta, ai.EventThinkingEnd,
			ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd:
			if event.Message == nil || !started {
				continue
			}
			msg := *event.Message
			agCtx.Messages[len(agCtx.Messages)-1] = msg
			if err := emit(Event{Type: MessageUpdate, Message: msg, AssistantMessageEvent: event}); err != nil {
				return nil, err
			}

		case ai.EventMessageDone, ai.EventMessageError, ai.EventMessageComplete:
			result := event.Message
			if result == nil {
				return nil, ai.ErrStreamMissingResult
			}
			msg := *result
			if started {
				agCtx.Messages[len(agCtx.Messages)-1] = msg
			} else {
				agCtx.Messages = append(agCtx.Messages, msg)
				started = true
				if err := emit(Event{Type: MessageStart, Message: msg}); err != nil {
					return nil, err
				}
			}
			if err := emit(Event{Type: MessageEnd, Message: msg}); err != nil {
				return nil, err
			}
			return result, nil
		}
	}

	result, err := response.Result()
	if err != nil {
		return nil, err
	}
	msg := *result
	if started {
		agCtx.Messages[len(agCtx.Messages)-1] = msg
	} else {
		agCtx.Messages = append(agCtx.Messages, msg)
		if err := emit(Event{Type: MessageStart, Message: msg}); err != nil {
			return nil, err
		}
	}
	if err := emit(Event{Type: MessageEnd, Message: *result}); err != nil {
		return nil, err
	}
	return result, nil
}

type executedToolCallBatch struct {
	messages  []ai.Message
	terminate bool
}

func extractToolCalls(msg *ai.Message) []ai.ContentBlock {
	calls := []ai.ContentBlock{}
	for _, block := range msg.Content {
		if block.Type == ai.ContentToolCall {
			calls = append(calls, block)
		}
	}
	return calls
}

func executeToolCalls(
	ctx context.Context,
	agCtx *Context,
	assistantMessage *ai.Message,
	config Config,
	emit EventSink,
) (executedToolCallBatch, error) {
	toolCalls := extractToolCalls(assistantMessage)

	hasSequential := false
	for _, tc := range toolCalls {
		for _, t := range agCtx.Tools {
			if t.Name == tc.ToolName && t.ExecutionMode == tool.Sequential {
				hasSequential = true
				break
			}
		}
		if hasSequential {
			break
		}
	}

	if config.ExecutionMode == tool.Sequential || hasSequential {
		return executeToolCallsSequential(ctx, agCtx, assistantMessage, toolCalls, config, emit)
	}
	return executeToolCallsParallel(ctx, agCtx, assistantMessage, toolCalls, config, emit)
}

func executeToolCallsSequential(
	ctx context.Context,
	agCtx *Context,
	assistantMessage *ai.Message,
	toolCalls []ai.ContentBlock,
	config Config,
	emit EventSink,
) (executedToolCallBatch, error) {
	finalized := []finalizedToolCallOutcome{}
	messages := []ai.Message{}

	for _, tc := range toolCalls {
		if err := emitToolExecutionStartFromCall(tc, emit); err != nil {
			return executedToolCallBatch{}, err
		}

		prep := prepareToolCall(ctx, agCtx, assistantMessage, tc, config)

		var f finalizedToolCallOutcome
		if prep.kind == kindImmediate {
			f = finalizedToolCallOutcome{
				toolCall: tc,
				result:   prep.result,
				isError:  prep.isError,
			}
		} else {
			executed, err := executePreparedToolCall(ctx, prep, func(event Event) error {
				return emit(event)
			})
			if err != nil {
				return executedToolCallBatch{}, err
			}
			f = finalizeExecutedToolCall(ctx, agCtx, assistantMessage, prep, executed, config)
		}

		if err := emitToolExecutionEnd(f, emit); err != nil {
			return executedToolCallBatch{}, err
		}
		toolResult := createToolResultMessage(f)
		if err := emitToolResultMessage(toolResult, emit); err != nil {
			return executedToolCallBatch{}, err
		}
		finalized = append(finalized, f)
		messages = append(messages, toolResult)

		if ctx.Err() != nil {
			break
		}
	}

	return executedToolCallBatch{
		messages:  messages,
		terminate: shouldTerminateToolBatch(finalized),
	}, nil
}

func executeToolCallsParallel(
	ctx context.Context,
	agCtx *Context,
	assistantMessage *ai.Message,
	toolCalls []ai.ContentBlock,
	config Config,
	emit EventSink,
) (executedToolCallBatch, error) {
	type entry struct {
		finalized  finalizedToolCallOutcome
		prepared   *preparedToolCall
		endEmitted bool
	}

	entries := []entry{}

	for _, tc := range toolCalls {
		if err := emitToolExecutionStartFromCall(tc, emit); err != nil {
			return executedToolCallBatch{}, err
		}

		prep := prepareToolCall(ctx, agCtx, assistantMessage, tc, config)

		if prep.kind == kindImmediate {
			f := finalizedToolCallOutcome{
				toolCall: tc,
				result:   prep.result,
				isError:  prep.isError,
			}
			if err := emitToolExecutionEnd(f, emit); err != nil {
				return executedToolCallBatch{}, err
			}
			entries = append(entries, entry{finalized: f, endEmitted: true})
		} else {
			prepared := prep
			entries = append(entries, entry{prepared: &prepared})
		}

		if ctx.Err() != nil {
			break
		}
	}

	type indexedResult struct {
		idx        int
		finalized  finalizedToolCallOutcome
		updates    []Event
		endEmitted bool
		err        error
	}

	results := make([]indexedResult, len(entries))
	completed := make(chan indexedResult, len(entries))
	var wg sync.WaitGroup

	for i, e := range entries {
		if e.prepared == nil {
			results[i] = indexedResult{
				idx:        i,
				finalized:  e.finalized,
				endEmitted: e.endEmitted,
			}
			continue
		}

		wg.Add(1)
		go func(idx int, prep preparedToolCall) {
			defer wg.Done()
			updates := []Event{}
			executed, err := executePreparedToolCall(ctx, prep, func(event Event) error {
				updates = append(updates, event)
				return nil
			})
			if err != nil {
				completed <- indexedResult{idx: idx, updates: updates, err: err}
				return
			}
			finalized := finalizeExecutedToolCall(ctx, agCtx, assistantMessage, prep, executed, config)
			completed <- indexedResult{
				idx:       idx,
				finalized: finalized,
				updates:   updates,
			}
		}(i, *e.prepared)
	}

	go func() {
		wg.Wait()
		close(completed)
	}()

	var firstErr error
	for result := range completed {
		results[result.idx] = result
		if firstErr != nil {
			continue
		}
		for _, event := range result.updates {
			if err := emit(event); err != nil {
				firstErr = err
				break
			}
		}
		if firstErr != nil {
			continue
		}
		if result.err != nil {
			firstErr = result.err
			continue
		}
		if !result.endEmitted {
			if err := emitToolExecutionEnd(result.finalized, emit); err != nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return executedToolCallBatch{}, firstErr
	}

	finalized := make([]finalizedToolCallOutcome, 0, len(results))
	messages := make([]ai.Message, 0, len(results))
	for _, result := range results {
		finalized = append(finalized, result.finalized)
		toolResult := createToolResultMessage(result.finalized)
		if err := emitToolResultMessage(toolResult, emit); err != nil {
			return executedToolCallBatch{}, err
		}
		messages = append(messages, toolResult)
	}

	return executedToolCallBatch{
		messages:  messages,
		terminate: shouldTerminateToolBatch(finalized),
	}, nil
}

type prepKind string

const (
	kindPrepared  prepKind = "prepared"
	kindImmediate prepKind = "immediate"
)

type preparedToolCall struct {
	kind     prepKind
	toolCall ai.ContentBlock
	tl       tool.Tool
	args     map[string]any
	result   tool.Result
	isError  bool
}

type executedToolCallOutcome struct {
	result  tool.Result
	isError bool
}

type finalizedToolCallOutcome struct {
	toolCall ai.ContentBlock
	result   tool.Result
	isError  bool
}

func shouldTerminateToolBatch(finalized []finalizedToolCallOutcome) bool {
	if len(finalized) == 0 {
		return false
	}
	for _, f := range finalized {
		if !f.result.Terminate {
			return false
		}
	}
	return true
}

func prepareToolCall(
	ctx context.Context,
	agCtx *Context,
	assistantMessage *ai.Message,
	toolCall ai.ContentBlock,
	config Config,
) preparedToolCall {
	var t *tool.Tool
	for _, candidate := range agCtx.Tools {
		if candidate.Name == toolCall.ToolName {
			t = &candidate
			break
		}
	}
	if t == nil {
		return preparedToolCall{
			kind:     kindImmediate,
			toolCall: toolCall,
			result:   createErrorToolResult("Tool " + toolCall.ToolName + " not found"),
			isError:  true,
		}
	}

	args := toolCall.Arguments
	if t.PrepareArgs != nil {
		args = t.PrepareArgs(args)
		if args == nil {
			args = map[string]any{}
		}
	}
	if args == nil {
		args = map[string]any{}
	}

	// Coerce + validate args against the tool's parameter schema after
	// PrepareArgs and before BeforeToolCall, mirroring pi's
	// validateToolArguments position.
	validatedArgs, err := ai.ValidateToolArguments(t.Name, t.Parameters, args)
	if err != nil {
		return preparedToolCall{
			kind:     kindImmediate,
			toolCall: toolCall,
			args:     args,
			result:   createErrorToolResult(err.Error()),
			isError:  true,
		}
	}
	args = validatedArgs

	if config.BeforeToolCall != nil {
		result, err := config.BeforeToolCall(ctx, BeforeToolCallContext{
			AssistantMessage: assistantMessage,
			ToolCall:         toolCall,
			Args:             args,
			Context:          *agCtx,
		})
		if err != nil {
			return preparedToolCall{
				kind:     kindImmediate,
				toolCall: toolCall,
				args:     args,
				result:   createErrorToolResult(err.Error()),
				isError:  true,
			}
		}
		if ctx.Err() != nil {
			return preparedToolCall{
				kind:     kindImmediate,
				toolCall: toolCall,
				args:     args,
				result:   createErrorToolResult("Operation aborted"),
				isError:  true,
			}
		}
		if result != nil && result.Block {
			reason := result.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			return preparedToolCall{
				kind:     kindImmediate,
				toolCall: toolCall,
				args:     args,
				result:   createErrorToolResult(reason),
				isError:  true,
			}
		}
		if result != nil && result.Args != nil {
			args = result.Args
		}
	}

	if ctx.Err() != nil {
		return preparedToolCall{
			kind:     kindImmediate,
			toolCall: toolCall,
			args:     args,
			result:   createErrorToolResult("Operation aborted"),
			isError:  true,
		}
	}

	return preparedToolCall{
		kind:     kindPrepared,
		toolCall: toolCall,
		tl:       *t,
		args:     args,
	}
}

func executePreparedToolCall(
	ctx context.Context,
	prep preparedToolCall,
	onUpdate func(Event) error,
) (executedToolCallOutcome, error) {
	var (
		updateErr   error
		updateErrMu sync.Mutex
	)

	recordUpdateErr := func(err error) {
		updateErrMu.Lock()
		defer updateErrMu.Unlock()
		if updateErr == nil {
			updateErr = err
		}
	}
	currentUpdateErr := func() error {
		updateErrMu.Lock()
		defer updateErrMu.Unlock()
		return updateErr
	}

	callback := func(partial tool.Result) {
		if onUpdate == nil {
			return
		}
		err := onUpdate(Event{
			Type:       ToolExecutionUpdate,
			ToolCallID: prep.toolCall.ToolCallID,
			ToolName:   prep.toolCall.ToolName,
			// pi emits the assistant message's original arguments, not the
			// validated/coerced ones (agent-loop.ts:644-649).
			Args:          prep.toolCall.Arguments,
			PartialResult: partial,
		})
		if err != nil {
			recordUpdateErr(err)
		}
	}

	result, execErr := prep.tl.Execute(ctx, prep.toolCall.ToolCallID, prep.args, callback)
	if err := currentUpdateErr(); err != nil {
		return executedToolCallOutcome{}, err
	}
	if execErr != nil {
		return executedToolCallOutcome{
			result:  createErrorToolResult(execErr.Error()),
			isError: true,
		}, nil
	}
	return executedToolCallOutcome{result: result, isError: result.IsError}, nil
}

func finalizeExecutedToolCall(
	ctx context.Context,
	agCtx *Context,
	assistantMessage *ai.Message,
	prep preparedToolCall,
	executed executedToolCallOutcome,
	config Config,
) finalizedToolCallOutcome {
	result := executed.result
	isError := executed.isError

	if config.AfterToolCall != nil {
		after, err := config.AfterToolCall(ctx, AfterToolCallContext{
			AssistantMessage: assistantMessage,
			ToolCall:         prep.toolCall,
			Args:             prep.args,
			Result:           result,
			IsError:          isError,
			Context:          *agCtx,
		})
		if err != nil {
			result = createErrorToolResult(err.Error())
			isError = true
		} else if after != nil {
			if after.Content != nil {
				result.Content = after.Content
			}
			if after.Details != nil {
				result.Details = after.Details
			}
			if after.IsError != nil {
				isError = *after.IsError
			}
			if after.Terminate != nil {
				result.Terminate = *after.Terminate
			}
		}
	}

	return finalizedToolCallOutcome{
		toolCall: prep.toolCall,
		result:   result,
		isError:  isError,
	}
}

func createErrorToolResult(text string) tool.Result {
	return tool.Result{
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
		Details: map[string]any{},
		IsError: true,
	}
}

func reasoningFromThinkingLevel(level ThinkingLevel) string {
	if level == "" || level == ThinkingOff {
		return ""
	}
	return string(level)
}

func aiThinkingBudgets(budgets *ThinkingBudgets) *ai.ThinkingBudgets {
	if budgets == nil {
		return nil
	}
	return &ai.ThinkingBudgets{
		Minimal: budgets.Minimal,
		Low:     budgets.Low,
		Medium:  budgets.Medium,
		High:    budgets.High,
	}
}

// emitToolExecutionStartFromCall emits tool_execution_start with the raw
// tool-call arguments, before prepareToolCall runs. This mirrors pi, which
// emits the start event before prepare/validate/beforeToolCall.
func emitToolExecutionStartFromCall(tc ai.ContentBlock, emit EventSink) error {
	return emit(Event{
		Type:       ToolExecutionStart,
		ToolCallID: tc.ToolCallID,
		ToolName:   tc.ToolName,
		Args:       tc.Arguments,
	})
}

func emitToolExecutionEnd(f finalizedToolCallOutcome, emit EventSink) error {
	return emit(Event{
		Type:       ToolExecutionEnd,
		ToolCallID: f.toolCall.ToolCallID,
		ToolName:   f.toolCall.ToolName,
		Result:     f.result,
		IsError:    f.isError,
	})
}

func createToolResultMessage(f finalizedToolCallOutcome) ai.Message {
	return ai.Message{
		Role:       ai.RoleToolResult,
		ToolCallID: f.toolCall.ToolCallID,
		ToolName:   f.toolCall.ToolName,
		Content:    f.result.Content,
		IsError:    f.isError,
		Details:    f.result.Details,
		Timestamp:  time.Now().UnixMilli(),
	}
}

func emitToolResultMessage(msg ai.Message, emit EventSink) error {
	if err := emit(Event{Type: MessageStart, Message: msg}); err != nil {
		return err
	}
	return emit(Event{Type: MessageEnd, Message: msg})
}
