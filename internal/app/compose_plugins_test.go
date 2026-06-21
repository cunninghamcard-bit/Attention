package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/plugin"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestSessionMountEmitsExtEnvelope(t *testing.T) {
	requireNode(t)
	captureDefaultLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cwd := t.TempDir()
	comp, err := Compose(ctx, ComposeOptions{
		DataDir:       t.TempDir(),
		CWD:           cwd,
		Model:         testModel(),
		ThinkingLevel: agentloop.ThinkingOff,
		FakeStream:    helloWorldStream(),
		Plugins:       testPluginRegistry(t),
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

	if _, err := facade.Prompt(ctx, compat.PromptInput{Text: "hi"}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	envelopes, err := comp.Store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(envelopes) == 0 {
		t.Fatal("EventStore empty, want plugin and run envelopes")
	}
	for i, env := range envelopes {
		if env.Seq != uint64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d", i, env.Seq, i+1)
		}
	}

	var ext *protocol.Envelope
	for i := range envelopes {
		if envelopes[i].Kind == "ext.fixture-plugin.hello" {
			ext = &envelopes[i]
			break
		}
	}
	if ext == nil {
		t.Fatalf("missing ext.fixture-plugin.hello envelope; got kinds %v", envelopeKinds(envelopes))
	}
	if ext.SessionID != sessionID {
		t.Fatalf("ext sessionID = %q, want %q", ext.SessionID, sessionID)
	}
	if ext.Actor != protocol.ActorSystem {
		t.Fatalf("ext actor = %q, want %q", ext.Actor, protocol.ActorSystem)
	}

	var payload struct {
		Greeting  string `json:"greeting"`
		SessionID string `json:"sessionId"`
		SeedID    string `json:"seedId"`
		SeedCwd   string `json:"seedCwd"`
	}
	if err := json.Unmarshal(ext.Payload, &payload); err != nil {
		t.Fatalf("unmarshal ext payload: %v", err)
	}
	if payload.Greeting != "hello" || payload.SessionID != sessionID {
		t.Fatalf("payload = %+v, want greeting/session", payload)
	}
	if payload.SeedID != sessionID || payload.SeedCwd != cwd {
		t.Fatalf("payload seed = %+v, want id %q cwd %q", payload, sessionID, cwd)
	}

	if comp.ExtCommands == nil {
		t.Fatal("Composition.ExtCommands is nil, want plugin host bridge")
	}
	rawToolResult, err := comp.ExtCommands.ExecuteTool(
		ctx,
		"fixture-plugin",
		"session",
		sessionID,
		"shout",
		[]byte(`{"text":"hello"}`),
	)
	if err != nil {
		t.Fatalf("ExecuteTool shout: %v", err)
	}
	toolResult, err := decodeHostToolResult(rawToolResult)
	if err != nil {
		t.Fatalf("decode host tool result: %v", err)
	}
	if toolResult.IsError ||
		len(toolResult.Content) != 1 ||
		toolResult.Content[0].Type != ai.ContentText ||
		toolResult.Content[0].Text != "HELLO" {
		t.Fatalf("tool result = %+v, raw=%s", toolResult, string(rawToolResult))
	}

	var capabilities *protocol.Envelope
	for i := range envelopes {
		if envelopes[i].Kind == protocol.KindSessionCapabilities {
			capabilities = &envelopes[i]
			break
		}
	}
	if capabilities == nil {
		t.Fatalf("missing session.capabilities envelope; got kinds %v", envelopeKinds(envelopes))
	}
	var capPayload struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(capabilities.Payload, &capPayload); err != nil {
		t.Fatalf("unmarshal capabilities payload: %v", err)
	}
	if !slices.Contains(capPayload.Tools, "shout") {
		t.Fatalf("capabilities tools = %v, want shout", capPayload.Tools)
	}
}

func TestEngineOwnerEmitRejected(t *testing.T) {
	requireNode(t)
	logs := captureDefaultLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	comp, err := Compose(ctx, ComposeOptions{
		DataDir:       t.TempDir(),
		CWD:           t.TempDir(),
		Model:         testModel(),
		ThinkingLevel: agentloop.ThinkingOff,
		FakeStream:    helloWorldStream(),
		Plugins:       testPluginRegistry(t),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer comp.Stop()

	waitForLog(t, logs, "engine scope has no downlink")
	if got := logs.String(); !strings.Contains(got, "plugin event handler failed") {
		t.Fatalf("logs missing host handler failure: %s", got)
	}
}

func TestComposePluginless(t *testing.T) {
	t.Setenv("PATH", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	comp, err := Compose(ctx, ComposeOptions{
		DataDir:       t.TempDir(),
		CWD:           cwd,
		Model:         testModel(),
		ThinkingLevel: agentloop.ThinkingOff,
		FakeStream:    helloWorldStream(),
	})
	if err != nil {
		t.Fatalf("Compose pluginless: %v", err)
	}
	defer comp.Stop()

	sess, err := comp.Repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	facade := comp.NewSessionFacade(sess.GetMetadata().ID)
	if _, err := facade.Prompt(ctx, compat.PromptInput{Text: "hi"}); err != nil {
		t.Fatalf("Prompt pluginless: %v", err)
	}
}

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
}

func testPluginRegistry(t *testing.T) *plugin.Registry {
	t.Helper()

	bundledDir, err := filepath.Abs(filepath.Join("testdata", "plugins"))
	if err != nil {
		t.Fatalf("fixture plugin dir: %v", err)
	}
	registry, err := plugin.NewRegistry(bundledDir, t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return registry
}

func testModel() ai.Model {
	return ai.Model{
		ID:            "test-model",
		Provider:      "test-provider",
		ContextWindow: 128_000,
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureDefaultLogs(t *testing.T) *lockedBuffer {
	t.Helper()

	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	return &logs
}

func waitForLog(t *testing.T, logs *lockedBuffer, text string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log missing %q: %s", text, logs.String())
}

func envelopeKinds(envelopes []protocol.Envelope) []string {
	kinds := make([]string, 0, len(envelopes))
	for _, env := range envelopes {
		kinds = append(kinds, env.Kind)
	}
	return kinds
}
