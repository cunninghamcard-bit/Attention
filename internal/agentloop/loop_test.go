package agentloop

import (
	"context"
	"errors"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
)

func noopEmit(Event) error { return nil }

func completeStream(msg *ai.Message) StreamFunc {
	return func(
		context.Context,
		ai.Model,
		ai.Context,
		ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
			yield(&ai.StreamEvent{Type: ai.EventMessageComplete, Message: msg}, nil)
		})
	}
}

func TestStreamForwardsFirstContentBlockStartAsUpdate(t *testing.T) {
	partial := &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{{Type: ai.ContentText}}}
	final := &ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "hi"}},
		StopReason: ai.StopReasonStop,
	}
	stream := func(context.Context, ai.Model, ai.Context, ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
			yield(&ai.StreamEvent{Type: ai.EventMessageStart, Message: partial}, nil)
			yield(&ai.StreamEvent{Type: ai.EventTextStart, Index: 0, Delta: &ai.ContentBlock{Type: ai.ContentText}, Message: partial}, nil)
			yield(&ai.StreamEvent{Type: ai.EventTextDelta, Index: 0, Delta: &ai.ContentBlock{Type: ai.ContentText, Text: "hi"}, Message: final}, nil)
			yield(&ai.StreamEvent{Type: ai.EventMessageComplete, Message: final}, nil)
		})
	}

	var updates []*ai.StreamEvent
	emit := func(ev Event) error {
		if ev.Type == MessageUpdate {
			updates = append(updates, ev.AssistantMessageEvent)
		}
		return nil
	}

	if _, err := Run(
		context.Background(),
		[]message.AgentMessage{&ai.Message{Role: ai.RoleUser}},
		Context{},
		basicConfig(),
		stream,
		emit,
	); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both the first content-block start and the delta are forwarded as
	// message_update after the stream's message_start event.
	if len(updates) != 2 {
		t.Fatalf("message_update count = %d, want 2 (start + delta)", len(updates))
	}
	if updates[0] == nil || updates[0].Type != ai.EventTextStart {
		t.Fatalf("first update = %v, want EventTextStart", updates[0])
	}
	if updates[1] == nil || updates[1].Type != ai.EventTextDelta {
		t.Fatalf("second update = %v, want EventTextDelta", updates[1])
	}
}

func basicConfig() Config {
	return Config{
		Model: ai.Model{ID: "test-model", Provider: "test-provider"},
		ConvertToLLM: func(messages []message.AgentMessage) ([]ai.Message, error) {
			return message.DefaultConvertToLLM(messages)
		},
	}
}

func TestRunPassesThinkingOptionsToProviderStream(t *testing.T) {
	config := basicConfig()
	config.ThinkingLevel = ThinkingHigh
	config.ThinkingBudgets = &ThinkingBudgets{
		Minimal: 128,
		Low:     256,
		Medium:  512,
		High:    1024,
	}

	var got ai.SimpleStreamOptions
	stream := func(
		ctx context.Context,
		model ai.Model,
		llmCtx ai.Context,
		opts ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		got = opts
		return completeStream(&ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
			StopReason: ai.StopReasonStop,
		})(ctx, model, llmCtx, opts)
	}

	if _, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		config,
		stream,
		noopEmit,
	); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got.Reasoning != "high" {
		t.Fatalf("Reasoning = %q, want high", got.Reasoning)
	}
	if got.ThinkingBudgets == nil || got.ThinkingBudgets.High != 1024 {
		t.Fatalf("ThinkingBudgets = %+v, want high budget", got.ThinkingBudgets)
	}
}

func requireArgsMap(t *testing.T, args any) map[string]any {
	t.Helper()
	got, ok := args.(map[string]any)
	if !ok {
		t.Fatalf("args type = %T, want map[string]any", args)
	}
	return got
}

func TestRunDoesNotReturnPartialMessagesOnQueueCallbackError(t *testing.T) {
	want := errors.New("stop before streaming")

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{&ai.Message{Role: ai.RoleUser}},
		Context{},
		Config{
			GetSteeringMessages: func(context.Context) ([]message.AgentMessage, error) {
				return nil, want
			},
		},
		nil,
		noopEmit,
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestRunPropagatesSteeringError(t *testing.T) {
	want := errors.New("steering failed")

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		Config{
			GetSteeringMessages: func(context.Context) ([]message.AgentMessage, error) {
				return nil, want
			},
		},
		nil,
		noopEmit,
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestRunRequiresConvertToLLM(t *testing.T) {
	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		Config{},
		nil,
		noopEmit,
	)

	if !errors.Is(err, errMissingConvertToLLM) {
		t.Fatalf("Run error = %v, want %v", err, errMissingConvertToLLM)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestRunRequiresEventSink(t *testing.T) {
	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		Config{},
		nil,
		nil,
	)

	if !errors.Is(err, errMissingEventSink) {
		t.Fatalf("Run error = %v, want %v", err, errMissingEventSink)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestRunPropagatesGetAPIKeyError(t *testing.T) {
	want := errors.New("missing api key")
	config := basicConfig()
	config.GetAPIKey = func(context.Context, string) (string, error) {
		return "", want
	}

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		config,
		nil,
		noopEmit,
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestRunPropagatesPromptMessageEndEmitError(t *testing.T) {
	want := errors.New("session append failed")

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{&ai.Message{Role: ai.RoleUser}},
		Context{},
		Config{},
		nil,
		func(event Event) error {
			if event.Type == MessageEnd {
				return want
			}
			return nil
		},
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestRunPropagatesAgentEndEmitError(t *testing.T) {
	want := errors.New("agent end failed")
	streamCalled := false

	stream := func(
		ctx context.Context,
		model ai.Model,
		llmCtx ai.Context,
		opts ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		streamCalled = true
		return completeStream(&ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
			StopReason: ai.StopReasonStop,
		})(ctx, model, llmCtx, opts)
	}

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		basicConfig(),
		stream,
		func(event Event) error {
			if event.Type == AgentEnd {
				return want
			}
			return nil
		},
	)

	if !streamCalled {
		t.Fatal("stream func was not called")
	}
	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestRunPropagatesTurnEndEmitError(t *testing.T) {
	want := errors.New("turn end failed")

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		basicConfig(),
		completeStream(&ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
			StopReason: ai.StopReasonStop,
		}),
		func(event Event) error {
			if event.Type == TurnEnd {
				return want
			}
			return nil
		},
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestQueueCallbacksReceiveRunContext(t *testing.T) {
	ctx := context.Background()
	var gotCtx context.Context
	want := errors.New("stop")

	_, err := Run(
		ctx,
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{},
		Config{
			GetSteeringMessages: func(ctx context.Context) ([]message.AgentMessage, error) {
				gotCtx = ctx
				return nil, want
			},
		},
		nil,
		noopEmit,
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if gotCtx != ctx {
		t.Fatalf("queue callback context = %v, want %v", gotCtx, ctx)
	}
}

func TestToolResultMessageEmitErrorPropagates(t *testing.T) {
	want := errors.New("tool result append failed")
	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "lookup",
		}},
		StopReason: ai.StopReasonToolUse,
	}

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{
			Tools: []tool.Tool{{
				Tool: ai.Tool{Name: "lookup"},
				Execute: func(
					_ context.Context,
					_ string,
					_ map[string]any,
					_ tool.UpdateCallback,
				) (tool.Result, error) {
					return tool.Result{
						Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
						Details: map[string]any{},
					}, nil
				},
			}},
		},
		basicConfig(),
		completeStream(assistant),
		func(event Event) error {
			if event.Type != MessageEnd {
				return nil
			}
			msg, ok := message.AsAIMessage(event.Message)
			if ok && msg.Role == ai.RoleToolResult {
				return want
			}
			return nil
		},
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestBuiltinErrorResultMarksToolResultError(t *testing.T) {
	env := local.New(t.TempDir())
	readTool, err := builtin.Wrap(
		builtin.NewReadTool(env),
		func(context.Context) extension.ExtensionContext {
			return extension.ExtensionContext{}
		},
	)
	if err != nil {
		t.Fatalf("Wrap read tool: %v", err)
	}

	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "read",
			Arguments:  map[string]any{"path": "missing.txt"},
		}},
		StopReason: ai.StopReasonToolUse,
	}

	var endIsError bool
	batch, err := executeToolCallsSequential(
		context.Background(),
		&Context{Tools: []tool.Tool{readTool}},
		assistant,
		extractToolCalls(assistant),
		basicConfig(),
		func(event Event) error {
			if event.Type == ToolExecutionEnd {
				endIsError = event.IsError
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	if !endIsError {
		t.Fatal("tool_execution_end IsError = false, want true")
	}
	if len(batch.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(batch.messages))
	}

	msg := batch.messages[0]
	if !msg.IsError {
		t.Fatalf("tool result IsError = false, want true: %#v", msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != ai.ContentText {
		t.Fatalf("tool result content = %#v, want one text block", msg.Content)
	}
	if msg.Content[0].Text == "" {
		t.Fatal("tool result text is empty, want readable error")
	}
}

func TestSchemaValidationRejectsInvalidArgs(t *testing.T) {
	simpleTool := tool.Tool{
		Tool: ai.Tool{
			Name:        "strict",
			Description: "Tool with required string arg",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []any{"name"},
			},
		},
		Execute: func(ctx context.Context, _ string, _ map[string]any, _ tool.UpdateCallback) (tool.Result, error) {
			return tool.Result{Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}}}, nil
		},
	}

	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "strict",
			Arguments:  map[string]any{},
		}},
		StopReason: ai.StopReasonToolUse,
	}

	batch, err := executeToolCallsSequential(
		context.Background(),
		&Context{Tools: []tool.Tool{simpleTool}},
		assistant,
		extractToolCalls(assistant),
		basicConfig(),
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	if len(batch.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(batch.messages))
	}
	msg := batch.messages[0]
	if !msg.IsError {
		t.Fatal("schema-invalid args should produce IsError = true")
	}
	if len(msg.Content) != 1 || msg.Content[0].Text == "" {
		t.Fatal("schema-invalid args should produce a readable error message")
	}
	if msg.Content[0].Text == "ok" {
		t.Fatal("tool should not have been executed with invalid args")
	}
}

func TestSchemaValidationPassesValidArgs(t *testing.T) {
	simpleTool := tool.Tool{
		Tool: ai.Tool{
			Name:        "strict",
			Description: "Tool with required string arg",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []any{"name"},
			},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, _ tool.UpdateCallback) (tool.Result, error) {
			return tool.Result{Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "executed"}}}, nil
		},
	}

	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "strict",
			Arguments:  map[string]any{"name": "hello"},
		}},
		StopReason: ai.StopReasonToolUse,
	}

	batch, err := executeToolCallsSequential(
		context.Background(),
		&Context{Tools: []tool.Tool{simpleTool}},
		assistant,
		extractToolCalls(assistant),
		basicConfig(),
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	if len(batch.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(batch.messages))
	}
	msg := batch.messages[0]
	if msg.IsError {
		t.Fatalf("valid args should not produce error, got: %s", msg.Content[0].Text)
	}
	if msg.Content[0].Text != "executed" {
		t.Fatalf("result text = %q, want %q", msg.Content[0].Text, "executed")
	}
}

func TestToolUpdateEmitErrorPropagates(t *testing.T) {
	want := errors.New("tool update failed")
	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "lookup",
		}},
		StopReason: ai.StopReasonToolUse,
	}

	got, err := Run(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		Context{
			Tools: []tool.Tool{{
				Tool: ai.Tool{Name: "lookup"},
				Execute: func(
					_ context.Context,
					_ string,
					_ map[string]any,
					onUpdate tool.UpdateCallback,
				) (tool.Result, error) {
					onUpdate(tool.Result{
						Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "partial"}},
						Details: map[string]any{},
					})
					return tool.Result{
						Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
						Details: map[string]any{},
					}, nil
				},
			}},
		},
		Config{
			Model:         ai.Model{ID: "test-model", Provider: "test-provider"},
			ExecutionMode: tool.Sequential,
			ConvertToLLM: func(messages []message.AgentMessage) ([]ai.Message, error) {
				return message.DefaultConvertToLLM(messages)
			},
		},
		completeStream(assistant),
		func(event Event) error {
			if event.Type == ToolExecutionUpdate {
				return want
			}
			return nil
		},
	)

	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestBeforeToolCallArgsPatchExecutesWithPatchedArgs(t *testing.T) {
	originalArgs := map[string]any{"query": "original"}
	patchedArgs := map[string]any{"query": "patched", "limit": 3}
	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "lookup",
			Arguments:  originalArgs,
		}},
		StopReason: ai.StopReasonToolUse,
	}

	var executedArgs map[string]any
	var startArgs map[string]any
	var updateArgs map[string]any
	config := basicConfig()
	config.BeforeToolCall = func(_ context.Context, call BeforeToolCallContext) (*BeforeToolCallResult, error) {
		if call.Args["query"] != "original" {
			t.Fatalf("BeforeToolCall args = %#v, want original", call.Args)
		}
		return &BeforeToolCallResult{Reason: "patched", Args: patchedArgs}, nil
	}

	batch, err := executeToolCallsSequential(
		context.Background(),
		&Context{
			Tools: []tool.Tool{{
				Tool: ai.Tool{Name: "lookup"},
				Execute: func(
					_ context.Context,
					_ string,
					args map[string]any,
					onUpdate tool.UpdateCallback,
				) (tool.Result, error) {
					executedArgs = args
					onUpdate(tool.Result{
						Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "partial"}},
						Details: map[string]any{},
					})
					return tool.Result{
						Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
						Details:   map[string]any{},
						Terminate: true,
					}, nil
				},
			}},
		},
		assistant,
		extractToolCalls(assistant),
		config,
		func(event Event) error {
			switch event.Type {
			case ToolExecutionStart:
				startArgs = requireArgsMap(t, event.Args)
			case ToolExecutionUpdate:
				updateArgs = requireArgsMap(t, event.Args)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	if !batch.terminate {
		t.Fatal("batch.terminate = false, want true")
	}
	if executedArgs["query"] != "patched" || executedArgs["limit"] != 3 {
		t.Fatalf("executed args = %#v, want patched args", executedArgs)
	}
	// tool_execution_start fires before prepare/beforeToolCall, so it carries
	// the original (un-patched) args, matching pi's event ordering.
	if startArgs["query"] != "original" {
		t.Fatalf("start args = %#v, want original (pre-patch) args", startArgs)
	}
	// pi's update events also carry the assistant message's original
	// arguments — prepared.toolCall.arguments is never mutated
	// (agent-loop.ts:644-649).
	if updateArgs["query"] != "original" {
		t.Fatalf("update args = %#v, want original args", updateArgs)
	}
	if originalArgs["query"] != "original" {
		t.Fatalf("original args = %#v, want unchanged", originalArgs)
	}
}

func TestBeforeToolCallNoArgsPatchKeepsOriginalArgs(t *testing.T) {
	originalArgs := map[string]any{"query": "original"}
	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "lookup",
			Arguments:  originalArgs,
		}},
		StopReason: ai.StopReasonToolUse,
	}

	var executedArgs map[string]any
	var startArgs map[string]any
	config := basicConfig()
	config.BeforeToolCall = func(context.Context, BeforeToolCallContext) (*BeforeToolCallResult, error) {
		return &BeforeToolCallResult{Reason: "observed"}, nil
	}

	_, err := executeToolCallsSequential(
		context.Background(),
		&Context{
			Tools: []tool.Tool{{
				Tool: ai.Tool{Name: "lookup"},
				Execute: func(
					_ context.Context,
					_ string,
					args map[string]any,
					_ tool.UpdateCallback,
				) (tool.Result, error) {
					executedArgs = args
					return tool.Result{
						Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
						Details:   map[string]any{},
						Terminate: true,
					}, nil
				},
			}},
		},
		assistant,
		extractToolCalls(assistant),
		config,
		func(event Event) error {
			if event.Type == ToolExecutionStart {
				startArgs = requireArgsMap(t, event.Args)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	if executedArgs["query"] != "original" {
		t.Fatalf("executed args = %#v, want original args", executedArgs)
	}
	if startArgs["query"] != "original" {
		t.Fatalf("start args = %#v, want original args", startArgs)
	}
}

func TestBeforeToolCallBlockIgnoresArgsPatch(t *testing.T) {
	originalArgs := map[string]any{"query": "original"}
	patchedArgs := map[string]any{"query": "patched"}
	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "lookup",
			Arguments:  originalArgs,
		}},
		StopReason: ai.StopReasonToolUse,
	}

	var startArgs map[string]any
	config := basicConfig()
	config.BeforeToolCall = func(context.Context, BeforeToolCallContext) (*BeforeToolCallResult, error) {
		return &BeforeToolCallResult{
			Block:  true,
			Reason: "blocked",
			Args:   patchedArgs,
		}, nil
	}

	batch, err := executeToolCallsSequential(
		context.Background(),
		&Context{
			Tools: []tool.Tool{{
				Tool: ai.Tool{Name: "lookup"},
				Execute: func(
					context.Context,
					string,
					map[string]any,
					tool.UpdateCallback,
				) (tool.Result, error) {
					t.Fatal("blocked tool should not execute")
					return tool.Result{}, nil
				},
			}},
		},
		assistant,
		extractToolCalls(assistant),
		config,
		func(event Event) error {
			if event.Type == ToolExecutionStart {
				startArgs = requireArgsMap(t, event.Args)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	if startArgs["query"] != "original" {
		t.Fatalf("start args = %#v, want original args", startArgs)
	}
	if len(batch.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(batch.messages))
	}
	msg := batch.messages[0]
	if !msg.IsError || len(msg.Content) != 1 || msg.Content[0].Text != "blocked" {
		t.Fatalf("tool result = %#v, want blocked error", msg)
	}
}

func TestContinueRejectsEmptyContext(t *testing.T) {
	_, err := Continue(
		context.Background(),
		Context{},
		Config{},
		nil,
		noopEmit,
	)
	if !errors.Is(err, errNoMessages) {
		t.Fatalf("Continue error = %v, want %v", err, errNoMessages)
	}
}

func TestContinueRejectsAssistantContext(t *testing.T) {
	_, err := Continue(
		context.Background(),
		Context{Messages: []message.AgentMessage{ai.Message{Role: ai.RoleAssistant}}},
		Config{},
		nil,
		noopEmit,
	)
	if !errors.Is(err, errCannotContinue) {
		t.Fatalf("Continue error = %v, want %v", err, errCannotContinue)
	}
}

func TestContinueRequiresEventSink(t *testing.T) {
	got, err := Continue(
		context.Background(),
		Context{Messages: []message.AgentMessage{ai.Message{Role: ai.RoleUser}}},
		Config{},
		nil,
		nil,
	)

	if !errors.Is(err, errMissingEventSink) {
		t.Fatalf("Continue error = %v, want %v", err, errMissingEventSink)
	}
	if got != nil {
		t.Fatalf("new messages = %#v, want nil on error", got)
	}
}

func TestCreateToolResultMessagePreservesDetails(t *testing.T) {
	details := map[string]any{"value": "ok"}

	got := createToolResultMessage(finalizedToolCallOutcome{
		toolCall: ai.ContentBlock{
			ToolCallID: "call-1",
			ToolName:   "lookup",
		},
		result: tool.Result{
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
			Details: details,
		},
	})

	gotDetails, ok := got.Details.(map[string]any)
	if !ok {
		t.Fatalf("details type = %T, want map[string]any", got.Details)
	}
	if gotDetails["value"] != "ok" {
		t.Fatalf("details value = %v, want ok", gotDetails["value"])
	}
}

func TestSchemaValidationCoercesStringToNumber(t *testing.T) {
	executed := false
	var receivedArgs map[string]any
	coerceTool := tool.Tool{
		Tool: ai.Tool{
			Name:        "coerce",
			Description: "Tool with number arg",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{"type": "number"},
				},
				"required": []any{"count"},
			},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, _ tool.UpdateCallback) (tool.Result, error) {
			executed = true
			receivedArgs = args
			return tool.Result{Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}}}, nil
		},
	}

	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "coerce",
			Arguments:  map[string]any{"count": "42"},
		}},
		StopReason: ai.StopReasonToolUse,
	}

	batch, err := executeToolCallsSequential(
		context.Background(),
		&Context{Tools: []tool.Tool{coerceTool}},
		assistant,
		extractToolCalls(assistant),
		basicConfig(),
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	if len(batch.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(batch.messages))
	}
	msg := batch.messages[0]
	if msg.IsError {
		t.Fatalf("coercible args should not produce error, got: %s", msg.Content[0].Text)
	}
	if !executed {
		t.Fatal("tool should have been executed after coercion")
	}
	if receivedArgs["count"] != float64(42) {
		t.Errorf("tool received count = %v (%T), want float64(42)", receivedArgs["count"], receivedArgs["count"])
	}
}

func TestSchemaValidationCoercesStringToBool(t *testing.T) {
	var receivedArgs map[string]any
	boolTool := tool.Tool{
		Tool: ai.Tool{
			Name:        "toggle",
			Description: "Tool with bool arg",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"enabled": map[string]any{"type": "boolean"},
				},
				"required": []any{"enabled"},
			},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, _ tool.UpdateCallback) (tool.Result, error) {
			receivedArgs = args
			return tool.Result{Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}}}, nil
		},
	}

	assistant := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "toggle",
			Arguments:  map[string]any{"enabled": "true"},
		}},
		StopReason: ai.StopReasonToolUse,
	}

	batch, err := executeToolCallsSequential(
		context.Background(),
		&Context{Tools: []tool.Tool{boolTool}},
		assistant,
		extractToolCalls(assistant),
		basicConfig(),
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatalf("executeToolCallsSequential: %v", err)
	}
	msg := batch.messages[0]
	if msg.IsError {
		t.Fatalf("coercible bool args should not error, got: %s", msg.Content[0].Text)
	}
	if receivedArgs["enabled"] != true {
		t.Errorf("tool received enabled = %v (%T), want true (bool)", receivedArgs["enabled"], receivedArgs["enabled"])
	}
}
