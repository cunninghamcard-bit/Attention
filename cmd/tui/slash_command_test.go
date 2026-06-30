package main

import (
	"context"
	"iter"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/adk/session"
)

type slashCommandAgent struct {
	dispatchName          string
	dispatchArgs          string
	dispatchNotifications []commandNotification
	dispatchErr           error
	prompts               chan string
}

func (a *slashCommandAgent) RunStreaming(
	_ context.Context,
	_ string,
	userMessage string,
) iter.Seq2[*session.Event, error] {
	if a.prompts != nil {
		a.prompts <- userMessage
	}
	return func(func(*session.Event, error) bool) {}
}

func (a *slashCommandAgent) CreateSession(context.Context) (string, error) {
	return "", nil
}

func (a *slashCommandAgent) DispatchCommand(name, args string) (commandDispatchResult, error) {
	a.dispatchName = name
	a.dispatchArgs = args
	return commandDispatchResult{
		Notifications: append([]commandNotification(nil), a.dispatchNotifications...),
	}, a.dispatchErr
}

func TestTUIDispatchesExtensionCommandOverRPC(t *testing.T) {
	agent := &slashCommandAgent{
		dispatchNotifications: []commandNotification{
			{Message: "ran extension", Level: "info"},
			{Message: "careful", Level: "warning"},
		},
	}
	m := &model{
		cfg: Config{
			Agent: agent,
			Commands: []CommandInfo{
				{Name: "plugin-run", Source: "extension"},
			},
		},
		inputModel: NewInputModel(nil, nil, nil, nil, ""),
	}

	got, cmd := m.handleSlashCommand(`/plugin-run one "two words"`)
	if cmd != nil {
		t.Fatal("handleSlashCommand returned prompt command, want synchronous extension dispatch")
	}
	if _, ok := got.(tea.Model); !ok {
		t.Fatalf("returned model = %T, want tea.Model", got)
	}
	if agent.dispatchName != "plugin-run" || agent.dispatchArgs != `one "two words"` {
		t.Fatalf("dispatch = %q %q, want plugin-run args", agent.dispatchName, agent.dispatchArgs)
	}
	if len(m.chatModel.Messages) != 2 {
		t.Fatalf("chat messages len = %d, want extension notifications", len(m.chatModel.Messages))
	}
	if msg := m.chatModel.Messages[0]; msg.role != "assistant" || msg.content != "ran extension" || msg.isWarning {
		t.Fatalf("notice message = %#v", msg)
	}
	if msg := m.chatModel.Messages[1]; msg.role != "assistant" || msg.content != "careful" || !msg.isWarning {
		t.Fatalf("warning message = %#v", msg)
	}
}

func TestTUIPromptAndSkillCommandsStillSubmitPrompt(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		source string
		line   string
	}{
		{name: "prompt", cmd: "deploy", source: "prompt", line: `/deploy one "two words"`},
		{name: "skill", cmd: "skill:review", source: "skill", line: `/skill:review src/main.go`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &slashCommandAgent{prompts: make(chan string, 1)}
			m := &model{
				cfg: Config{
					Agent: agent,
					Commands: []CommandInfo{
						{Name: tt.cmd, Source: tt.source},
					},
				},
				ctx:        context.Background(),
				inputModel: NewInputModel(nil, nil, nil, nil, ""),
			}

			if _, cmd := m.handleSlashCommand(tt.line); cmd == nil {
				t.Fatal("handleSlashCommand returned nil cmd, want prompt command")
			}
			if agent.dispatchName != "" {
				t.Fatalf("DispatchCommand called for %s command: %q", tt.source, agent.dispatchName)
			}
			select {
			case got := <-agent.prompts:
				if got != tt.line {
					t.Fatalf("prompt = %q, want full slash line %q", got, tt.line)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for prompt submission")
			}
		})
	}
}
