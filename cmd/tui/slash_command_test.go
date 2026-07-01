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
	reloadNotice          string
	reloadErr             error
	fetchedCommands       []CommandInfo
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

func (a *slashCommandAgent) Reload() (string, error) {
	if a.reloadNotice != "" || a.reloadErr != nil {
		return a.reloadNotice, a.reloadErr
	}
	return "Reloaded.", nil
}

func (a *slashCommandAgent) FetchCommands() []CommandInfo {
	return append([]CommandInfo(nil), a.fetchedCommands...)
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

func TestTUIReloadRefreshesCommandList(t *testing.T) {
	agent := &slashCommandAgent{
		reloadNotice: "Reloaded.",
		fetchedCommands: []CommandInfo{
			{Name: "reload", Source: "builtin"},
			{Name: "rtk", Source: "extension"},
			{Name: "skill:review", Description: "Review code", Source: "skill"},
		},
	}
	m := &model{
		cfg: Config{
			Agent: agent,
			Commands: []CommandInfo{
				{Name: "reload", Source: "builtin"},
			},
		},
		inputModel: NewInputModel(nil, []CommandInfo{{Name: "reload", Source: "builtin"}}, nil, nil, ""),
	}

	if _, cmd := m.handleSlashCommand(`/reload`); cmd != nil {
		t.Fatal("handleSlashCommand returned async cmd, want synchronous reload")
	}
	if !commandInfoNamed(m.cfg.Commands, "rtk") || !commandInfoNamed(m.inputModel.Commands, "rtk") {
		t.Fatalf("commands not refreshed: cfg=%#v input=%#v", m.cfg.Commands, m.inputModel.Commands)
	}
	if len(m.cfg.Skills) != 1 || m.cfg.Skills[0].Name != "review" {
		t.Fatalf("skills = %#v, want refreshed review skill", m.cfg.Skills)
	}
}

func TestTUIReloadKeepsCommandsWhenRefreshFails(t *testing.T) {
	agent := &slashCommandAgent{reloadNotice: "Reloaded."}
	m := &model{
		cfg: Config{
			Agent: agent,
			Commands: []CommandInfo{
				{Name: "reload", Source: "builtin"},
			},
		},
		inputModel: NewInputModel(nil, []CommandInfo{{Name: "reload", Source: "builtin"}}, nil, nil, ""),
	}

	if _, cmd := m.handleSlashCommand(`/reload`); cmd != nil {
		t.Fatal("handleSlashCommand returned async cmd, want synchronous reload")
	}
	if !commandInfoNamed(m.cfg.Commands, "reload") || !commandInfoNamed(m.inputModel.Commands, "reload") {
		t.Fatalf("commands were cleared: cfg=%#v input=%#v", m.cfg.Commands, m.inputModel.Commands)
	}
	if len(m.chatModel.Messages) == 0 || !m.chatModel.Messages[0].isWarning {
		t.Fatalf("messages = %#v, want command refresh warning", m.chatModel.Messages)
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

func commandInfoNamed(commands []CommandInfo, name string) bool {
	for _, command := range commands {
		if command.Name == name {
			return true
		}
	}
	return false
}
