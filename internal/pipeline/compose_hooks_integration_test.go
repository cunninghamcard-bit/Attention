package pipeline_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/app"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestComposeSessionHookDispatchesToNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var toolResultText string
	comp, err := app.Compose(ctx, app.ComposeOptions{
		DataDir:       t.TempDir(),
		CWD:           t.TempDir(),
		Model:         ai.Model{ID: "test-model", Provider: "test-provider", ContextWindow: 128_000},
		ThinkingLevel: agentloop.ThinkingOff,
		FakeStream:    toolCallThenStopStream(&toolResultText),
		Plugins:       hookPluginRegistry(t),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer comp.Stop()

	sess, err := comp.Repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	facade := comp.NewSessionFacade(sess.GetMetadata().ID)

	if _, err := facade.Prompt(ctx, compat.PromptInput{Text: "run tool"}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if toolResultText != "seed-h0-h1" {
		t.Fatalf("tool result text = %q, want seed-h0-h1", toolResultText)
	}
}

func toolCallThenStopStream(toolResultText *string) agentloop.StreamFunc {
	calls := 0
	return func(
		_ context.Context,
		_ ai.Model,
		llmCtx ai.Context,
		_ ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		calls++
		if calls == 1 {
			msg := &ai.Message{
				Role: ai.RoleAssistant,
				Content: []ai.ContentBlock{{
					Type:       ai.ContentToolCall,
					ToolCallID: "call-1",
					ToolName:   "echo_args",
					Arguments:  map[string]any{"text": "seed"},
				}},
				StopReason: ai.StopReasonToolUse,
			}
			return completeMessageStream(msg)
		}

		for _, msg := range llmCtx.Messages {
			if msg.Role != ai.RoleToolResult || msg.ToolCallID != "call-1" || len(msg.Content) == 0 {
				continue
			}
			*toolResultText = msg.Content[0].Text
		}
		return completeMessageStream(&ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
			StopReason: ai.StopReasonStop,
		})
	}
}

func completeMessageStream(msg *ai.Message) *ai.AssistantMessageEventStream {
	return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
		yield(&ai.StreamEvent{Type: ai.EventMessageComplete, Message: msg}, nil)
	})
}

func hookPluginRegistry(t *testing.T) *plugin.Registry {
	t.Helper()

	bundledDir := t.TempDir()
	pluginDir := filepath.Join(bundledDir, "hook-plugin")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	writeTestFile(t, filepath.Join(pluginDir, "manifest.json"), `{
  "id": "hook-plugin",
  "name": "Hook Plugin",
  "version": "1.0.0",
  "minAppVersion": "1.0.0",
  "contributions": {
    "session": "session.mjs"
  }
}
`)
	writeTestFile(t, filepath.Join(pluginDir, "session.mjs"), `export default function activate(along) {
  along.tools.register({ name: "echo_args", inputSchema: { type: "object" } }, (args) => {
    return { content: String(args?.text ?? "") };
  });
  along.hooks.register("tool_call", (event) => {
    return { input: { ...event.input, text: String(event.input?.text ?? "") + "-h0" } };
  });
  along.hooks.register("tool_call", (event) => {
    return { input: { ...event.input, text: String(event.input?.text ?? "") + "-h1" } };
  });
}
`)

	registry, err := plugin.NewRegistry(bundledDir, t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return registry
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
