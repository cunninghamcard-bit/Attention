package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestComposeTodoPluginToolCommandAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cwd := t.TempDir()
	comp, err := Compose(ctx, ComposeOptions{
		DataDir:       t.TempDir(),
		CWD:           cwd,
		Model:         testModel(),
		ThinkingLevel: agentloop.ThinkingOff,
		FakeStream: scriptedAssistantStream(
			t,
			toolCallMessage("call-add-1", "addTodo", map[string]any{"text": "write tests"}),
			textMessage("todo added"),
		),
		Plugins: todoPluginRegistry(t, ""),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer comp.Stop()

	sess, err := comp.Repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	sessionID := sess.GetMetadata().ID
	facade := comp.NewSessionFacade(sessionID)

	if _, err := facade.Prompt(ctx, compat.PromptInput{Text: "add todo"}); err != nil {
		t.Fatalf("Prompt addTodo: %v", err)
	}

	envelopes, err := comp.Store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter after addTodo: %v", err)
	}
	requireTodoUpdate(t, envelopes, []todoSnapshot{{ID: 1, Text: "write tests", Done: false}})

	ch1, cancel1 := comp.Bus.Subscribe(sessionID)
	defer cancel1()
	ch2, cancel2 := comp.Bus.Subscribe(sessionID)
	defer cancel2()

	raw, err := comp.ExtCommands.DispatchCommand(
		ctx,
		"todo",
		"session",
		sessionID,
		"",
		"setTodos",
		[]byte(`{"todos":[{"id":7,"text":"ship final task","done":true}]}`),
	)
	if err != nil {
		t.Fatalf("DispatchCommand setTodos: %v", err)
	}
	var result struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal setTodos result: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("setTodos result count = %d, want 1; raw=%s", result.Count, string(raw))
	}
	requireSignal(t, ctx, ch1, "first subscriber")
	requireSignal(t, ctx, ch2, "second subscriber")

	firstRead, err := comp.Store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatalf("first replay ReadAfter: %v", err)
	}
	secondRead, err := comp.Store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatalf("second replay ReadAfter: %v", err)
	}
	assertSameEnvelopeSequence(t, firstRead, secondRead)
	requireTodoUpdate(t, firstRead, []todoSnapshot{{ID: 7, Text: "ship final task", Done: true}})
}

func TestComposeTodoPluginHostDownToolCallFastFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	agentDir := t.TempDir()
	writeKillHostPlugin(t, agentDir)

	cwd := t.TempDir()
	comp, err := Compose(ctx, ComposeOptions{
		DataDir:       t.TempDir(),
		CWD:           cwd,
		Model:         testModel(),
		ThinkingLevel: agentloop.ThinkingOff,
		FakeStream: scriptedAssistantStream(
			t,
			textMessage("mounted"),
			toolCallMessage("call-add-host-down", "addTodo", map[string]any{"text": "after host death"}),
			textMessage("host down handled"),
		),
		Plugins: todoPluginRegistry(t, agentDir),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer comp.Stop()

	sess, err := comp.Repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	sessionID := sess.GetMetadata().ID
	facade := comp.NewSessionFacade(sessionID)

	if _, err := facade.Prompt(ctx, compat.PromptInput{Text: "mount plugins"}); err != nil {
		t.Fatalf("Prompt mount plugins: %v", err)
	}

	_, err = comp.ExtCommands.DispatchCommand(
		ctx,
		"kill-host",
		"session",
		sessionID,
		"",
		"killHost",
		[]byte(`{}`),
	)
	var hostDown interface{ ExtHostDown() bool }
	if !errors.As(err, &hostDown) || !hostDown.ExtHostDown() {
		t.Fatalf("DispatchCommand killHost error = %v, want ext host down", err)
	}

	if _, err := facade.Prompt(ctx, compat.PromptInput{Text: "add todo while host is down"}); err != nil {
		t.Fatalf("Prompt after host down: %v", err)
	}

	envelopes, err := comp.Store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter after host down prompt: %v", err)
	}
	requireHostDownToolCompleted(t, envelopes, "call-add-host-down", "addTodo")
}

type todoSnapshot struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func todoPluginRegistry(t *testing.T, agentDir string) *plugin.Registry {
	t.Helper()

	bundledDir, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatalf("todo plugin dir: %v", err)
	}
	if agentDir == "" {
		agentDir = t.TempDir()
	}
	registry, err := plugin.NewRegistry(bundledDir, agentDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.SetEnabled("todo", true); err != nil {
		t.Fatalf("enable todo plugin: %v", err)
	}
	path, ok := registry.ContributionPath("todo", "session")
	if !ok {
		t.Fatal("todo session contribution missing")
	}
	if filepath.Base(path) != "host.mjs" {
		t.Fatalf("todo session contribution = %q, want host.mjs", path)
	}
	return registry
}

func scriptedAssistantStream(t *testing.T, messages ...*ai.Message) agentloop.StreamFunc {
	t.Helper()

	var calls atomic.Int32
	return func(
		_ context.Context,
		_ ai.Model,
		llmCtx ai.Context,
		_ ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		index := int(calls.Add(1)) - 1
		if index >= len(messages) {
			return streamFailure(fmt.Errorf("unexpected stream call %d", index+1))
		}
		msg := messages[index]
		for _, block := range msg.Content {
			if block.Type != ai.ContentToolCall {
				continue
			}
			if !hasLLMTool(llmCtx.Tools, block.ToolName) {
				return streamFailure(fmt.Errorf("missing bare tool %q in LLM context", block.ToolName))
			}
		}
		return streamMessage(msg)
	}
}

func streamMessage(msg *ai.Message) *ai.AssistantMessageEventStream {
	return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
		yield(&ai.StreamEvent{Type: ai.EventMessageComplete, Message: msg}, nil)
	})
}

func streamFailure(err error) *ai.AssistantMessageEventStream {
	return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
		yield(nil, err)
	})
}

func textMessage(text string) *ai.Message {
	return &ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
		StopReason: ai.StopReasonStop,
	}
}

func toolCallMessage(id string, name string, args map[string]any) *ai.Message {
	return &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:       ai.ContentToolCall,
			ToolCallID: id,
			ToolName:   name,
			Arguments:  args,
		}},
		StopReason: ai.StopReasonToolUse,
	}
}

func hasLLMTool(tools []ai.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func requireTodoUpdate(t *testing.T, envelopes []protocol.Envelope, want []todoSnapshot) {
	t.Helper()

	for i := len(envelopes) - 1; i >= 0; i-- {
		if envelopes[i].Kind != "ext.todo.todo.updated" {
			continue
		}
		var payload struct {
			Todos []todoSnapshot `json:"todos"`
		}
		if err := json.Unmarshal(envelopes[i].Payload, &payload); err != nil {
			t.Fatalf("unmarshal ext.todo.todo.updated payload: %v", err)
		}
		if !reflect.DeepEqual(payload.Todos, want) {
			t.Fatalf("ext.todo.todo.updated todos = %+v, want %+v", payload.Todos, want)
		}
		return
	}
	t.Fatalf("missing ext.todo.todo.updated envelope; got kinds %v", envelopeKinds(envelopes))
}

func requireHostDownToolCompleted(
	t *testing.T,
	envelopes []protocol.Envelope,
	toolCallID string,
	toolName string,
) {
	t.Helper()

	for _, env := range envelopes {
		if env.Kind != protocol.KindToolCompleted {
			continue
		}
		var payload struct {
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
			Result     any    `json:"result"`
			IsError    bool   `json:"isError"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("unmarshal tool.completed payload: %v", err)
		}
		if payload.ToolCallID != toolCallID || payload.ToolName != toolName {
			continue
		}
		if !payload.IsError {
			t.Fatalf("tool.completed isError = false, want true; payload=%s", string(env.Payload))
		}
		if !strings.Contains(fmt.Sprint(payload.Result), "ext_host_down") {
			t.Fatalf("tool.completed result = %#v, want ext_host_down; payload=%s", payload.Result, string(env.Payload))
		}
		return
	}
	t.Fatalf("missing host-down tool.completed for %s/%s; got kinds %v", toolName, toolCallID, envelopeKinds(envelopes))
}

func requireSignal(t *testing.T, ctx context.Context, ch <-chan struct{}, label string) {
	t.Helper()

	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("timeout waiting for %s signal: %v", label, ctx.Err())
	}
}

func writeKillHostPlugin(t *testing.T, agentDir string) {
	t.Helper()

	dir := filepath.Join(agentDir, "plugins", "kill-host")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir kill-host plugin: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, "manifest.json"), `{
  "id": "kill-host",
  "name": "Kill Host",
  "version": "0.1.0",
  "minAppVersion": "0.1.0",
  "contributions": {
    "session": "host.mjs"
  }
}
`)
	writeTestFile(t, filepath.Join(dir, "host.mjs"), `export default function activate(along, ctx) {
  along.commands.on("killHost", () => {
    process.exit(1);
  });
}
`)
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertSameEnvelopeSequence(t *testing.T, first []protocol.Envelope, second []protocol.Envelope) {
	t.Helper()

	if len(first) != len(second) {
		t.Fatalf("envelope count mismatch: first=%d second=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].Seq != second[i].Seq ||
			first[i].Kind != second[i].Kind ||
			!bytes.Equal(first[i].Payload, second[i].Payload) {
			t.Fatalf(
				"envelope[%d] mismatch:\nfirst=%+v\nsecond=%+v",
				i,
				first[i],
				second[i],
			)
		}
	}
}
