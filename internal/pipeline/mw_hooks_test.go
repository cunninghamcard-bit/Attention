package pipeline

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

func TestLoopHookCallbacksBeforeToolCallFold(t *testing.T) {
	hookErr := errors.New("hook failed")

	tests := []struct {
		name        string
		register    func(t *testing.T, registry *hook.Registry, calls *[]string)
		wantCalls   []string
		wantReports []string
		assert      func(t *testing.T, result *agentloop.BeforeToolCallResult)
	}{
		{
			name: "threads input through handlers",
			register: func(t *testing.T, registry *hook.Registry, calls *[]string) {
				registry.On(hook.EventToolCall, func(_ context.Context, event any) (any, error) {
					*calls = append(*calls, "first")
					e := event.(hook.ToolCallEvent)
					if got := e.Input.(map[string]any)["value"]; got != "initial" {
						t.Fatalf("first input = %v, want initial", got)
					}
					return hook.ToolCallResult{Input: map[string]any{"value": "first"}}, nil
				})
				registry.On(hook.EventToolCall, func(_ context.Context, event any) (any, error) {
					*calls = append(*calls, "second")
					e := event.(hook.ToolCallEvent)
					if got := e.Input.(map[string]any)["value"]; got != "first" {
						t.Fatalf("second input = %v, want first", got)
					}
					return hook.ToolCallResult{Input: map[string]any{"value": "second"}}, nil
				})
				registry.On(hook.EventToolCall, func(context.Context, any) (any, error) {
					*calls = append(*calls, "empty")
					return nil, nil
				})
			},
			wantCalls: []string{"first", "second", "empty"},
			assert: func(t *testing.T, result *agentloop.BeforeToolCallResult) {
				if result == nil {
					t.Fatal("result = nil, want args patch")
				}
				if got := result.Args["value"]; got != "second" {
					t.Fatalf("result.Args[value] = %v, want second", got)
				}
				if result.Block {
					t.Fatal("Block = true, want false")
				}
			},
		},
		{
			name: "first block short circuits",
			register: func(t *testing.T, registry *hook.Registry, calls *[]string) {
				registry.On(hook.EventToolCall, func(context.Context, any) (any, error) {
					*calls = append(*calls, "mutate")
					return hook.ToolCallResult{Input: map[string]any{"value": "mutated"}}, nil
				})
				registry.On(hook.EventToolCall, func(context.Context, any) (any, error) {
					*calls = append(*calls, "block")
					return hook.ToolCallResult{Block: true, Reason: "blocked"}, nil
				})
				registry.On(hook.EventToolCall, func(context.Context, any) (any, error) {
					*calls = append(*calls, "after-block")
					return hook.ToolCallResult{}, nil
				})
			},
			wantCalls: []string{"mutate", "block"},
			assert: func(t *testing.T, result *agentloop.BeforeToolCallResult) {
				if result == nil {
					t.Fatal("result = nil, want block")
				}
				if !result.Block || result.Reason != "blocked" {
					t.Fatalf("result = %+v, want blocked reason", result)
				}
				if result.Args != nil {
					t.Fatalf("blocked result Args = %#v, want nil", result.Args)
				}
			},
		},
		{
			name: "handler error reports and continues",
			register: func(t *testing.T, registry *hook.Registry, calls *[]string) {
				registry.On(hook.EventToolCall, func(context.Context, any) (any, error) {
					*calls = append(*calls, "error")
					return nil, hookErr
				})
				registry.On(hook.EventToolCall, func(context.Context, any) (any, error) {
					*calls = append(*calls, "continue")
					return hook.ToolCallResult{Input: map[string]any{"value": "continued"}}, nil
				})
			},
			wantCalls:   []string{"error", "continue"},
			wantReports: []string{hook.EventToolCall},
			assert: func(t *testing.T, result *agentloop.BeforeToolCallResult) {
				if result == nil {
					t.Fatal("result = nil, want args patch")
				}
				if got := result.Args["value"]; got != "continued" {
					t.Fatalf("result.Args[value] = %v, want continued", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := hook.NewRegistry()
			var calls []string
			var reports []string
			registry.OnHandlerError = func(eventType string, err error) {
				if !errors.Is(err, hookErr) {
					t.Fatalf("reported err = %v, want %v", err, hookErr)
				}
				reports = append(reports, eventType)
			}
			tt.register(t, registry, &calls)

			callbacks := LoopHookCallbacks(registry)
			result, err := callbacks.BeforeToolCall(context.Background(), beforeToolCallContext(map[string]any{
				"value": "initial",
			}))
			if err != nil {
				t.Fatalf("BeforeToolCall error = %v", err)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, tt.wantCalls)
			}
			if !reflect.DeepEqual(reports, tt.wantReports) {
				t.Fatalf("reports = %v, want %v", reports, tt.wantReports)
			}
			tt.assert(t, result)
		})
	}
}

func TestLoopHookCallbacksAfterToolCallFold(t *testing.T) {
	hookErr := errors.New("hook failed")

	tests := []struct {
		name        string
		register    func(t *testing.T, registry *hook.Registry, calls *[]string)
		wantCalls   []string
		wantReports []string
		assert      func(t *testing.T, result *agentloop.AfterToolCallResult)
	}{
		{
			name: "field patches accumulate with last field win",
			register: func(t *testing.T, registry *hook.Registry, calls *[]string) {
				registry.On(hook.EventToolResult, func(_ context.Context, event any) (any, error) {
					*calls = append(*calls, "content")
					e := event.(hook.ToolResultEvent)
					if got := e.Content[0].Text; got != "base" {
						t.Fatalf("first content = %q, want base", got)
					}
					return hook.ToolResultPatch{
						Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "first"}},
					}, nil
				})
				registry.On(hook.EventToolResult, func(_ context.Context, event any) (any, error) {
					*calls = append(*calls, "details")
					e := event.(hook.ToolResultEvent)
					if got := e.Content[0].Text; got != "first" {
						t.Fatalf("second content = %q, want first", got)
					}
					return hook.ToolResultPatch{
						Details: map[string]any{"step": "second"},
						IsError: boolPtr(true),
					}, nil
				})
				registry.On(hook.EventToolResult, func(_ context.Context, event any) (any, error) {
					*calls = append(*calls, "last-content")
					e := event.(hook.ToolResultEvent)
					if got := e.Details.(map[string]any)["step"]; got != "second" {
						t.Fatalf("third details step = %v, want second", got)
					}
					return hook.ToolResultPatch{
						Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: "third"}},
						Terminate: boolPtr(true),
					}, nil
				})
			},
			wantCalls: []string{"content", "details", "last-content"},
			assert: func(t *testing.T, result *agentloop.AfterToolCallResult) {
				if result == nil {
					t.Fatal("result = nil, want accumulated patch")
				}
				if got := result.Content[0].Text; got != "third" {
					t.Fatalf("Content[0].Text = %q, want third", got)
				}
				if got := result.Details.(map[string]any)["step"]; got != "second" {
					t.Fatalf("Details[step] = %v, want second", got)
				}
				if result.IsError == nil || !*result.IsError {
					t.Fatalf("IsError = %v, want true", result.IsError)
				}
				if result.Terminate == nil || !*result.Terminate {
					t.Fatalf("Terminate = %v, want true", result.Terminate)
				}
			},
		},
		{
			name: "handler error reports and continues",
			register: func(t *testing.T, registry *hook.Registry, calls *[]string) {
				registry.On(hook.EventToolResult, func(context.Context, any) (any, error) {
					*calls = append(*calls, "error")
					return nil, hookErr
				})
				registry.On(hook.EventToolResult, func(context.Context, any) (any, error) {
					*calls = append(*calls, "continue")
					return hook.ToolResultPatch{
						Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "continued"}},
					}, nil
				})
			},
			wantCalls:   []string{"error", "continue"},
			wantReports: []string{hook.EventToolResult},
			assert: func(t *testing.T, result *agentloop.AfterToolCallResult) {
				if result == nil {
					t.Fatal("result = nil, want content patch")
				}
				if got := result.Content[0].Text; got != "continued" {
					t.Fatalf("Content[0].Text = %q, want continued", got)
				}
			},
		},
		{
			name: "no patch returns no result",
			register: func(t *testing.T, registry *hook.Registry, calls *[]string) {
				registry.On(hook.EventToolResult, func(context.Context, any) (any, error) {
					*calls = append(*calls, "nil")
					return nil, nil
				})
				registry.On(hook.EventToolResult, func(context.Context, any) (any, error) {
					*calls = append(*calls, "wrong-type")
					return "ignored", nil
				})
			},
			wantCalls: []string{"nil", "wrong-type"},
			assert: func(t *testing.T, result *agentloop.AfterToolCallResult) {
				if result != nil {
					t.Fatalf("result = %+v, want nil", result)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := hook.NewRegistry()
			var calls []string
			var reports []string
			registry.OnHandlerError = func(eventType string, err error) {
				if !errors.Is(err, hookErr) {
					t.Fatalf("reported err = %v, want %v", err, hookErr)
				}
				reports = append(reports, eventType)
			}
			tt.register(t, registry, &calls)

			callbacks := LoopHookCallbacks(registry)
			result, err := callbacks.AfterToolCall(context.Background(), afterToolCallContext())
			if err != nil {
				t.Fatalf("AfterToolCall error = %v", err)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, tt.wantCalls)
			}
			if !reflect.DeepEqual(reports, tt.wantReports) {
				t.Fatalf("reports = %v, want %v", reports, tt.wantReports)
			}
			tt.assert(t, result)
		})
	}
}

func TestFoldTableContextThreadsMessages(t *testing.T) {
	registry := hook.NewRegistry()
	registry.On(hook.EventContext, func(_ context.Context, event any) (any, error) {
		e := event.(hook.ContextEvent)
		assertAnyMessageText(t, e.Messages[0], "initial")
		return hook.ContextResult{Messages: []any{userTextMessage("first")}}, nil
	})
	registry.On(hook.EventContext, func(_ context.Context, event any) (any, error) {
		e := event.(hook.ContextEvent)
		assertAnyMessageText(t, e.Messages[0], "first")
		return hook.ContextResult{Messages: []any{userTextMessage("second")}}, nil
	})

	got, err := transformContextMessages(
		context.Background(),
		registry,
		[]message.AgentMessage{userTextMessage("initial")},
	)
	if err != nil {
		t.Fatalf("transformContextMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("messages len = %d, want 1", len(got))
	}
	assertTextMessage(t, got[0], "second")
}

func TestFoldTableBeforeAgentStartCollectsMessagesAndChainsSystemPrompt(t *testing.T) {
	registry := hook.NewRegistry()
	firstPrompt := "first prompt"
	secondPrompt := "second prompt"
	registry.On(hook.EventBeforeAgentStart, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeAgentStartEvent)
		if e.SystemPrompt != "base prompt" {
			t.Fatalf("first SystemPrompt = %q, want base prompt", e.SystemPrompt)
		}
		return hook.BeforeAgentStartResult{
			Messages:     []any{userTextMessage("first")},
			SystemPrompt: &firstPrompt,
		}, nil
	})
	registry.On(hook.EventBeforeAgentStart, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeAgentStartEvent)
		if e.SystemPrompt != firstPrompt {
			t.Fatalf("second SystemPrompt = %q, want first prompt", e.SystemPrompt)
		}
		return hook.BeforeAgentStartResult{
			Messages:     []any{userTextMessage("second")},
			SystemPrompt: &secondPrompt,
		}, nil
	})

	got, err := emitBeforeAgentStart(
		context.Background(),
		registry,
		[]message.AgentMessage{userTextMessage("prompt")},
		"base prompt",
		hook.SystemPromptOptions{},
		nil,
	)
	if err != nil {
		t.Fatalf("emitBeforeAgentStart: %v", err)
	}
	if got.systemPrompt == nil || *got.systemPrompt != secondPrompt {
		t.Fatalf("systemPrompt = %v, want second prompt", got.systemPrompt)
	}
	if len(got.messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(got.messages))
	}
	assertTextMessage(t, got.messages[0], "first")
	assertTextMessage(t, got.messages[1], "second")
}

func beforeToolCallContext(args map[string]any) agentloop.BeforeToolCallContext {
	return agentloop.BeforeToolCallContext{
		ToolCall: ai.ContentBlock{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "lookup",
		},
		Args: args,
	}
}

func afterToolCallContext() agentloop.AfterToolCallContext {
	return agentloop.AfterToolCallContext{
		ToolCall: ai.ContentBlock{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "lookup",
		},
		Args: map[string]any{"value": "input"},
		Result: tool.Result{
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "base"}},
			Details: map[string]any{
				"step": "base",
			},
		},
	}
}

func assertAnyMessageText(t *testing.T, item any, want string) {
	t.Helper()
	msg, ok := item.(message.AgentMessage)
	if !ok {
		t.Fatalf("message = %T, want AgentMessage", item)
	}
	assertTextMessage(t, msg, want)
}
