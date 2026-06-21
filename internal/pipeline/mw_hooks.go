package pipeline

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
)

// LoopCallbacks are agentloop callbacks backed by hook.Registry folds.
type LoopCallbacks struct {
	BeforeToolCall func(context.Context, agentloop.BeforeToolCallContext) (*agentloop.BeforeToolCallResult, error)
	AfterToolCall  func(context.Context, agentloop.AfterToolCallContext) (*agentloop.AfterToolCallResult, error)
}

// LoopHookCallbacks adapts hook.Registry decision handlers to agentloop hooks.
func LoopHookCallbacks(registry *hook.Registry) LoopCallbacks {
	if registry == nil {
		return LoopCallbacks{}
	}

	return LoopCallbacks{
		BeforeToolCall: beforeToolCallFold(registry),
		AfterToolCall:  afterToolCallFold(registry),
	}
}

func beforeToolCallFold(
	registry *hook.Registry,
) func(context.Context, agentloop.BeforeToolCallContext) (*agentloop.BeforeToolCallResult, error) {
	return func(ctx context.Context, callCtx agentloop.BeforeToolCallContext) (*agentloop.BeforeToolCallResult, error) {
		if !registry.HasHandlers(hook.EventToolCall) {
			return nil, nil
		}

		currentInput := callCtx.Args
		inputChanged := false
		var last *agentloop.BeforeToolCallResult
		for _, handler := range registry.Handlers(hook.EventToolCall) {
			event := hook.ToolCallEvent{
				Type:       hook.EventToolCall,
				ToolCallId: callCtx.ToolCall.ToolCallID,
				ToolName:   callCtx.ToolCall.ToolName,
				Input:      currentInput,
			}
			result, err := handler(ctx, event)
			if err != nil {
				registry.ReportHandlerError(hook.EventToolCall, err)
				continue
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
	}
}

func afterToolCallFold(
	registry *hook.Registry,
) func(context.Context, agentloop.AfterToolCallContext) (*agentloop.AfterToolCallResult, error) {
	return func(ctx context.Context, callCtx agentloop.AfterToolCallContext) (*agentloop.AfterToolCallResult, error) {
		if !registry.HasHandlers(hook.EventToolResult) {
			return nil, nil
		}

		// pi accumulates field-level patches into a shared event:
		// content/details/isError patches from different extensions compose, and
		// each handler sees the previous patches (runner.ts:756-790).
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
		for _, handler := range registry.Handlers(hook.EventToolResult) {
			result, err := handler(ctx, event)
			if err != nil {
				registry.ReportHandlerError(hook.EventToolResult, err)
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
	}
}
