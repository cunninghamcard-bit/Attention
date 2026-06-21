package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
)

type capturedEvent struct {
	kind    string
	actor   protocol.Actor
	payload any
}

type capturingEmitter struct {
	events []capturedEvent
}

func (e *capturingEmitter) emit(tc *RunContext, kind string, actor protocol.Actor, payload any) error {
	e.events = append(e.events, capturedEvent{
		kind:    kind,
		actor:   actor,
		payload: payload,
	})
	return nil
}

type fakeSessionView struct {
	messages  []message.AgentMessage
	appendErr error
}

func (s *fakeSessionView) Messages() []message.AgentMessage {
	out := make([]message.AgentMessage, 0, len(s.messages))
	for _, msg := range s.messages {
		out = append(out, message.Snapshot(msg))
	}
	return out
}

func (s *fakeSessionView) AppendMessage(msg message.AgentMessage) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	s.messages = append(s.messages, message.Snapshot(msg))
	return nil
}

func TestMWContextBuildsMessagesAndRunsHooks(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "no handlers builds system prompt and appends input",
			run: func(t *testing.T) {
				session := &fakeSessionView{
					messages: []message.AgentMessage{
						userTextMessage("history"),
					},
				}
				tc := &RunContext{
					Session: session,
					Env:     EnvView{CWD: "/repo"},
					Agent: AgentSnapshot{
						SystemPrompt: "base prompt",
					},
					Input: "hello",
				}

				var nextCalled bool
				next := func(ctx context.Context, got *RunContext) error {
					nextCalled = true
					if len(got.Messages) != 2 {
						t.Fatalf("messages len = %d, want 2", len(got.Messages))
					}
					assertTextMessage(t, got.Messages[0], "history")
					assertTextMessage(t, got.Messages[1], "hello")
					for _, want := range []string{
						"base prompt",
						`<project_instructions path="/repo/AGENTS.md">`,
						"<available_skills>",
						"Current working directory: /repo",
					} {
						if !strings.Contains(got.Agent.SystemPrompt, want) {
							t.Fatalf("system prompt missing %q:\n%s", want, got.Agent.SystemPrompt)
						}
					}
					if strings.Contains(got.Agent.SystemPrompt, "silent") {
						t.Fatalf("system prompt should omit snippetless tool:\n%s", got.Agent.SystemPrompt)
					}
					return nil
				}

				err := MWContext(ContextConfig{
					Hooks: hook.NewRegistry(),
					Tools: []PromptTool{
						{
							Name:              "read",
							PromptSnippet:     "Read files",
							PromptGuidelines:  []string{"Prefer focused reads"},
							AvailableToPrompt: true,
						},
						{
							Name:              "silent",
							AvailableToPrompt: true,
						},
					},
					ContextFiles: []resource.ContextFile{
						{Path: "/repo/AGENTS.md", Content: "project instructions"},
					},
					Skills: []resource.Skill{
						{Name: "review", Description: "Review code", FilePath: "/skills/review/SKILL.md"},
					},
				})(context.Background(), tc, next)
				if err != nil {
					t.Fatalf("MWContext: %v", err)
				}
				if !nextCalled {
					t.Fatal("next was not called")
				}
			},
		},
		{
			name: "before_agent_start and context hooks are pinned before next",
			run: func(t *testing.T) {
				reg := hook.NewRegistry()
				resources := map[string]any{"skills": []string{"s1"}}
				injected := userTextMessage("injected")
				transformed := userTextMessage("transformed")
				var contextSawInjected bool

				reg.On(hook.EventBeforeAgentStart, func(_ context.Context, event any) (any, error) {
					e, ok := event.(hook.BeforeAgentStartEvent)
					if !ok {
						t.Fatalf("event type = %T, want BeforeAgentStartEvent", event)
					}
					if e.Prompt != "hello" {
						t.Fatalf("Prompt = %q, want hello", e.Prompt)
					}
					if len(e.Images) != 1 || e.Images[0].MimeType != "image/png" || e.Images[0].Data != "abc" {
						t.Fatalf("Images = %#v, want image/png abc", e.Images)
					}
					if !strings.Contains(e.SystemPrompt, "base prompt") ||
						!strings.Contains(e.SystemPrompt, "Current working directory: /repo") {
						t.Fatalf("SystemPrompt = %q, want assembled prompt with base and cwd", e.SystemPrompt)
					}
					if e.SystemPromptOptions == nil || e.SystemPromptOptions.CWD != "/repo" {
						t.Fatalf("SystemPromptOptions = %#v, want CWD /repo", e.SystemPromptOptions)
					}
					if e.Resources == nil {
						t.Fatal("Resources = nil, want injected resources")
					}
					override := "hook system"
					return hook.BeforeAgentStartResult{
						Messages:     []any{injected},
						SystemPrompt: &override,
					}, nil
				})
				reg.On(hook.EventContext, func(_ context.Context, event any) (any, error) {
					e := event.(hook.ContextEvent)
					for _, msg := range e.Messages {
						if aiMsg, ok := msg.(ai.Message); ok && textOf(aiMsg) == "injected" {
							contextSawInjected = true
						}
					}
					return hook.ContextResult{Messages: []any{transformed}}, nil
				})

				tc := &RunContext{
					Session: &fakeSessionView{
						messages: []message.AgentMessage{
							ai.Message{
								Role: ai.RoleUser,
								Content: []ai.ContentBlock{
									{Type: ai.ContentText, Text: "hello"},
									{Type: ai.ContentImage, MimeType: "image/png", ImageData: "abc"},
								},
							},
						},
					},
					Env:   EnvView{CWD: "/repo"},
					Agent: AgentSnapshot{SystemPrompt: "base prompt"},
				}

				var nextCalled bool
				next := func(ctx context.Context, got *RunContext) error {
					nextCalled = true
					if got.Agent.SystemPrompt != "hook system" {
						t.Fatalf("SystemPrompt = %q, want hook system", got.Agent.SystemPrompt)
					}
					if len(got.Messages) != 1 {
						t.Fatalf("messages len = %d, want 1", len(got.Messages))
					}
					assertTextMessage(t, got.Messages[0], "transformed")
					return nil
				}

				err := MWContext(ContextConfig{
					Hooks:     reg,
					Resources: resources,
				})(context.Background(), tc, next)
				if err != nil {
					t.Fatalf("MWContext: %v", err)
				}
				if !nextCalled {
					t.Fatal("next was not called")
				}
				if !contextSawInjected {
					t.Fatal("context hook did not receive before_agent_start injected message")
				}
			},
		},
		{
			name: "context hook errors are reported and prior messages continue",
			run: func(t *testing.T) {
				reg := hook.NewRegistry()
				hookErr := errors.New("boom")
				var reported bool
				reg.OnHandlerError = func(eventType string, err error) {
					reported = eventType == hook.EventContext && errors.Is(err, hookErr)
				}
				reg.On(hook.EventContext, func(context.Context, any) (any, error) {
					return nil, hookErr
				})

				tc := &RunContext{
					Session: &fakeSessionView{},
					Agent:   AgentSnapshot{SystemPrompt: "base"},
					Input:   "hello",
				}
				next := func(ctx context.Context, got *RunContext) error {
					assertTextMessage(t, got.Messages[0], "hello")
					return nil
				}

				if err := MWContext(ContextConfig{Hooks: reg})(context.Background(), tc, next); err != nil {
					t.Fatalf("MWContext: %v", err)
				}
				if !reported {
					t.Fatal("context hook error was not reported")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func userTextMessage(text string) ai.Message {
	return ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
	}
}

func assistantMessage(model string, stop ai.StopReason, text string) ai.Message {
	return ai.Message{
		Role:       ai.RoleAssistant,
		Provider:   "test-provider",
		Model:      model,
		StopReason: stop,
		Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
	}
}

func retryableAssistantMessage(model string, errorMessage string) ai.Message {
	msg := assistantMessage(model, ai.StopReasonError, "Error: "+errorMessage)
	msg.ErrorMessage = errorMessage
	return msg
}

func overflowAssistantMessage(model string) ai.Message {
	return retryableAssistantMessage(model, "prompt is too long")
}

func assertTextMessage(t *testing.T, msg message.AgentMessage, want string) {
	t.Helper()
	aiMsg, ok := message.AsAIMessage(msg)
	if !ok {
		t.Fatalf("message = %T, want ai.Message", msg)
	}
	if got := textOf(aiMsg); got != want {
		t.Fatalf("message text = %q, want %q", got, want)
	}
}

func textOf(msg ai.Message) string {
	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == ai.ContentText {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

func boolPtr(v bool) *bool {
	return &v
}

func intPtr(v int) *int {
	return &v
}
