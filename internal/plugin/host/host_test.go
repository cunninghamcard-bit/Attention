package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/pipeline"
)

type recordedEvent struct {
	pluginID  string
	owner     string
	sessionID string
	name      string
	payload   []byte
}

type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *eventRecorder) onEvent(pluginID, owner, sessionID, name string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{
		pluginID:  pluginID,
		owner:     owner,
		sessionID: sessionID,
		name:      name,
		payload:   append([]byte(nil), payload...),
	})
	return nil
}

func (r *eventRecorder) waitCount(t *testing.T, name string, count int) []recordedEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events := r.snapshot()
		if countEvents(events, name) >= count {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	events := r.snapshot()
	t.Fatalf("event %q count = %d, want at least %d; events=%+v", name, countEvents(events, name), count, events)
	return nil
}

func (r *eventRecorder) snapshot() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

func countEvents(events []recordedEvent, name string) int {
	count := 0
	for _, event := range events {
		if event.name == name {
			count++
		}
	}
	return count
}

func countEventsForOwner(events []recordedEvent, name, owner string) int {
	count := 0
	for _, event := range events {
		if event.name == name && event.owner == owner {
			count++
		}
	}
	return count
}

type registeredNames []string

func (names registeredNames) contains(name string) bool {
	return slices.Contains(names, name)
}

func registeredToolNames(tools []ToolReg) registeredNames {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func registeredCommandNames(commands []CommandReg) registeredNames {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name)
	}
	return names
}

func requireNode(t *testing.T) string {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	return nodePath
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture path: %v", err)
	}
	return path
}

func newTestManager(t *testing.T, recorder *eventRecorder) *Manager {
	t.Helper()
	manager := New(Options{
		NodePath: requireNode(t),
		WorkDir:  t.TempDir(),
		OnEvent:  recorder.onEvent,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
	return manager
}

func TestActivateAtomicRegistered(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	registered, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "session",
		SessionID:  "s1",
		ModulePath: fixturePath(t, "fixture.mjs"),
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !registeredToolNames(registered.Tools).contains("greet") ||
		!registeredToolNames(registered.Tools).contains("shout") {
		t.Fatalf("tools = %+v, want greet and shout", registered.Tools)
	}
	if !registeredCommandNames(registered.Commands).contains("hello") ||
		!registeredCommandNames(registered.Commands).contains("echo") {
		t.Fatalf("commands = %+v, want hello and echo", registered.Commands)
	}

	events := recorder.waitCount(t, "plugin:activated", 1)
	if !bytes.Contains(events[0].payload, []byte(`"msg":"hello"`)) {
		t.Fatalf("payload = %s, want hello msg", string(events[0].payload))
	}
}

func TestDisposeRunsCleanup(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	_, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "session",
		SessionID:  "s1",
		ModulePath: fixturePath(t, "fixture.mjs"),
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if err := manager.Dispose(context.Background(), MountKey{
		PluginID:  "fixture",
		Owner:     "session",
		SessionID: "s1",
	}); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	events := recorder.waitCount(t, "plugin:disposed", 1)
	if !bytes.Contains(events[len(events)-1].payload, []byte(`"msg":"bye"`)) {
		t.Fatalf("payload = %s, want bye msg", string(events[len(events)-1].payload))
	}
}

func TestPingPong(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame, err := manager.call(ctx, Frame{T: FramePing})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if frame.T != FramePong {
		t.Fatalf("frame type = %q, want %q", frame.T, FramePong)
	}
}

func TestActivateErrorIsolated(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	registered, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "bad",
		Owner:      "engine",
		ModulePath: fixturePath(t, "fixture_error.mjs"),
	})
	if err == nil {
		t.Fatal("Activate error fixture succeeded, want error")
	}
	if !registered.IsError || registered.Error == "" {
		t.Fatalf("registered = %+v, want isError with error text", registered)
	}

	registered, err = manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "engine",
		ModulePath: fixturePath(t, "fixture.mjs"),
	})
	if err != nil {
		t.Fatalf("Activate good fixture after error: %v", err)
	}
	if !registeredToolNames(registered.Tools).contains("greet") {
		t.Fatalf("tools = %+v, want greet", registered.Tools)
	}
}

func TestCommandDispatchRoundTrip(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	_, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "session",
		SessionID:  "s1",
		ModulePath: fixturePath(t, "fixture.mjs"),
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := manager.DispatchCommand(
		ctx,
		"fixture",
		"session",
		"s1",
		"",
		"echo",
		[]byte(`{"msg":"hi"}`),
	)
	if err != nil {
		t.Fatalf("DispatchCommand echo: %v", err)
	}
	var echoed struct {
		OK  bool            `json:"ok"`
		Got json.RawMessage `json:"got"`
	}
	if err := json.Unmarshal(got, &echoed); err != nil {
		t.Fatalf("unmarshal echo: %v", err)
	}
	if !echoed.OK || string(echoed.Got) != `{"msg":"hi"}` {
		t.Fatalf("echo result = %s", string(got))
	}

	_, err = manager.DispatchCommand(ctx, "fixture", "session", "s1", "", "explode", nil)
	if err == nil || !strings.Contains(err.Error(), "command exploded") {
		t.Fatalf("DispatchCommand explode error = %v, want command exploded", err)
	}

	_, err = manager.DispatchCommand(ctx, "fixture", "session", "s1", "", "missing", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown command "missing"`) {
		t.Fatalf("DispatchCommand missing error = %v, want unknown command", err)
	}

	_, err = manager.DispatchCommand(ctx, "fixture", "session", "no-session", "", "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "plugin instance not active") {
		t.Fatalf("DispatchCommand no instance error = %v, want inactive instance", err)
	}
}

func TestToolExecuteRoundTrip(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	_, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "session",
		SessionID:  "s1",
		ModulePath: fixturePath(t, "fixture.mjs"),
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := manager.ExecuteTool(ctx, "fixture", "session", "s1", "shout", []byte(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("ExecuteTool shout: %v", err)
	}
	var result struct {
		Content string `json:"content"`
		Details struct {
			Owner string `json:"owner"`
		} `json:"details"`
	}
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if result.Content != "HELLO" || result.Details.Owner != "session" {
		t.Fatalf("tool result = %+v, raw=%s", result, string(got))
	}

	_, err = manager.ExecuteTool(ctx, "fixture", "session", "s1", "fail", nil)
	if err == nil || !strings.Contains(err.Error(), "tool exploded") {
		t.Fatalf("ExecuteTool fail error = %v, want tool exploded", err)
	}

	_, err = manager.ExecuteTool(ctx, "fixture", "session", "s1", "missing", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown tool "missing"`) {
		t.Fatalf("ExecuteTool missing error = %v, want unknown tool", err)
	}
}

func TestHookDispatchRoundTripThreadsEngineFold(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	registered, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "session",
		SessionID:  "s1",
		ModulePath: fixturePath(t, "hook_threading.mjs"),
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(registered.Hooks) != 2 {
		t.Fatalf("hooks = %+v, want two tool_call hooks", registered.Hooks)
	}
	for i, reg := range registered.Hooks {
		if reg.Point != hook.EventToolCall || reg.Index != i {
			t.Fatalf("hooks[%d] = %+v, want point %q index %d", i, reg, hook.EventToolCall, i)
		}
	}

	registry := hook.NewRegistry()
	var dispatches []int
	for _, registeredHook := range registered.Hooks {
		registeredHook := registeredHook
		registry.On(registeredHook.Point, func(ctx context.Context, event any) (any, error) {
			eventJSON, err := json.Marshal(event)
			if err != nil {
				return nil, err
			}
			raw, err := manager.DispatchHook(
				ctx,
				"fixture",
				"session",
				"s1",
				registeredHook.Point,
				registeredHook.Index,
				eventJSON,
			)
			if err != nil {
				return nil, err
			}
			dispatches = append(dispatches, registeredHook.Index)
			if strings.TrimSpace(string(raw)) == "null" {
				return nil, nil
			}
			var result hook.ToolCallResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
			return result, nil
		})
	}

	callbacks := pipeline.LoopHookCallbacks(registry)
	result, err := callbacks.BeforeToolCall(context.Background(), agentloop.BeforeToolCallContext{
		ToolCall: ai.ContentBlock{
			Type:       ai.ContentToolCall,
			ToolCallID: "call-1",
			ToolName:   "demo",
		},
		Args: map[string]any{"trace": ""},
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if !slices.Equal(dispatches, []int{0, 1}) {
		t.Fatalf("dispatches = %v, want [0 1]", dispatches)
	}
	if result == nil {
		t.Fatal("result = nil, want threaded args")
	}
	if got := result.Args["trace"]; got != "01" {
		t.Fatalf("result.Args[trace] = %v, want 01", got)
	}
	if got := result.Args["seenBySecond"]; got != "0" {
		t.Fatalf("result.Args[seenBySecond] = %v, want 0", got)
	}
}

func TestUIConfirmRoundTrip(t *testing.T) {
	var recorder eventRecorder
	requests := make(chan UIRequest, 1)
	manager := New(Options{
		NodePath: requireNode(t),
		WorkDir:  t.TempDir(),
		OnEvent:  recorder.onEvent,
		OnUIRequest: func(req UIRequest) (string, error) {
			requests <- req
			return "uir_test_1", nil // engine mints the requestId; host keys its pending promise by it
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	startCtx, startCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer startCancel()
	if err := manager.Start(startCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Stop(ctx)
	})

	if _, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "session",
		SessionID:  "s1",
		ModulePath: fixturePath(t, "ui_confirm.mjs"),
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// askConfirm blocks on ui.confirm; run it off the test goroutine.
	type toolOutcome struct {
		raw []byte
		err error
	}
	done := make(chan toolOutcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		raw, err := manager.ExecuteTool(ctx, "fixture", "session", "s1", "askConfirm", nil)
		done <- toolOutcome{raw, err}
	}()

	var req UIRequest
	select {
	case req = <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ui.request")
	}
	if req.Kind != "confirm" || req.SessionID != "s1" {
		t.Fatalf("ui.request = %+v, want confirm on s1", req)
	}
	if !bytes.Contains(req.Payload, []byte(`"title":"Proceed?"`)) {
		t.Fatalf("ui.request payload = %s, want confirm title", string(req.Payload))
	}

	resolved, err := json.Marshal(map[string]any{
		"requestId":  "uir_test_1",
		"value":      true,
		"resolvedBy": "client",
	})
	if err != nil {
		t.Fatalf("marshal resolved: %v", err)
	}
	if err := manager.ResolveUI(context.Background(), "s1", resolved); err != nil {
		t.Fatalf("ResolveUI: %v", err)
	}

	var outcome toolOutcome
	select {
	case outcome = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for askConfirm to return after resolve")
	}
	if outcome.err != nil {
		t.Fatalf("ExecuteTool askConfirm: %v", outcome.err)
	}
	var result struct {
		Details struct {
			Answer bool `json:"answer"`
		} `json:"details"`
	}
	if err := json.Unmarshal(outcome.raw, &result); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if !result.Details.Answer {
		t.Fatalf("answer = %v, want true (resolved value reached the awaiting promise); raw=%s", result.Details.Answer, string(outcome.raw))
	}
}

func TestCrashRestartReplays(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	for _, spec := range []ActivateSpec{
		{
			PluginID:   "fixture",
			Owner:      "engine",
			ModulePath: fixturePath(t, "fixture.mjs"),
		},
		{
			PluginID:   "fixture",
			Owner:      "session:s1",
			SessionID:  "s1",
			ModulePath: fixturePath(t, "fixture.mjs"),
		},
	} {
		if _, err := manager.Activate(context.Background(), spec); err != nil {
			t.Fatalf("Activate %+v: %v", spec, err)
		}
	}
	recorder.waitCount(t, "plugin:activated", 2)

	if err := manager.processKillForTest(); err != nil {
		t.Fatalf("processKillForTest: %v", err)
	}

	events := recorder.waitCount(t, "plugin:activated", 4)
	if countEventsForOwner(events, "plugin:activated", "engine") != 2 {
		t.Fatalf("engine activation count = %d, want 2; events=%+v", countEventsForOwner(events, "plugin:activated", "engine"), events)
	}
	if countEventsForOwner(events, "plugin:activated", "session:s1") != 2 {
		t.Fatalf("session activation count = %d, want 2; events=%+v", countEventsForOwner(events, "plugin:activated", "session:s1"), events)
	}

	activated := make([]recordedEvent, 0, len(events))
	for _, event := range events {
		if event.name == "plugin:activated" {
			activated = append(activated, event)
		}
	}
	replayed := activated[len(activated)-2:]
	if replayed[0].owner != "engine" || replayed[1].owner != "session:s1" {
		t.Fatalf("replay order = %s, %s; want engine before session:s1", replayed[0].owner, replayed[1].owner)
	}
}

func TestHostDownFastFail(t *testing.T) {
	var recorder eventRecorder
	manager := newTestManager(t, &recorder)

	if err := manager.processKillForTest(); err != nil {
		t.Fatalf("processKillForTest: %v", err)
	}

	start := time.Now()
	_, err := manager.Activate(context.Background(), ActivateSpec{
		PluginID:   "fixture",
		Owner:      "engine",
		ModulePath: fixturePath(t, "fixture.mjs"),
	})
	if !errors.Is(err, ErrHostDown) {
		t.Fatalf("Activate error = %v, want ErrHostDown", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Activate elapsed = %s, want fast fail before restart", elapsed)
	}
}
