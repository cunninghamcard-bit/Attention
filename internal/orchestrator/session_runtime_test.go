package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

func TestForkMessagesListsUserMessages(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	first, err := o.session.AppendMessage(ctx, ai.Message{
		Role: ai.RoleUser,
		Content: []ai.ContentBlock{
			{Type: ai.ContentText, Text: "first"},
			{Type: ai.ContentImage, ImageData: "ignored"},
			{Type: ai.ContentText, Text: " message"},
		},
	})
	if err != nil {
		t.Fatalf("AppendMessage first user: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "assistant"}},
	}); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: ""}},
	}); err != nil {
		t.Fatalf("AppendMessage empty user: %v", err)
	}

	messages := o.ForkMessages()
	if len(messages) != 1 {
		t.Fatalf("ForkMessages len = %d, want 1: %+v", len(messages), messages)
	}
	if messages[0].EntryID != string(first) || messages[0].Text != "first message" {
		t.Fatalf("ForkMessages[0] = %+v, want %s/first message", messages[0], first)
	}
}

func TestNewSessionRebindsFreshSessionAndKeepsRuntimeState(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	if err := o.SetModel(ctx, testModel("recovered-model")); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
		t.Fatalf("SetThinkingLevel: %v", err)
	}
	oldMetadata := o.session.GetMetadata()
	oldHooks := o.hooks
	var sawMessageStart bool
	cancelSubscribe := o.Subscribe(func(ev Event) {
		if ev.Type == EventMessageStart {
			sawMessageStart = true
		}
	})
	defer cancelSubscribe()

	cancelled, err := o.NewSession(ctx, "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if cancelled {
		t.Fatal("NewSession cancelled = true, want false")
	}

	newMetadata := o.session.GetMetadata()
	if newMetadata.ID == oldMetadata.ID || newMetadata.Path == oldMetadata.Path {
		t.Fatalf("new metadata = %+v, want fresh session after %+v", newMetadata, oldMetadata)
	}
	if newMetadata.ParentSessionPath != oldMetadata.Path {
		t.Fatalf("parent path = %q, want %q", newMetadata.ParentSessionPath, oldMetadata.Path)
	}
	if entries := o.session.GetEntries(); len(entries) != 0 {
		t.Fatalf("new session entries len = %d, want 0", len(entries))
	}
	if got := o.currentModel(); got.ID != "recovered-model" {
		t.Fatalf("model after NewSession = %q, want recovered-model", got.ID)
	}
	o.mu.Lock()
	gotThinking := o.thinkingLevel
	hooks := o.hooks
	o.mu.Unlock()
	if gotThinking != agentloop.ThinkingHigh {
		t.Fatalf("thinking level after NewSession = %q, want high", gotThinking)
	}
	if hooks != oldHooks {
		t.Fatal("hook registry was replaced; want existing registry reused")
	}

	if _, err := o.hooks.Emit(ctx, hook.MessageStartEvent{
		Type:    hook.EventMessageStart,
		Message: ai.Message{Role: ai.RoleAssistant},
	}); err != nil {
		t.Fatalf("Emit message_start: %v", err)
	}
	if !sawMessageStart {
		t.Fatal("subscriber did not receive hook-backed event after rebind")
	}
}

func TestRebindSessionEmitsModelSelectHookWithRestoreSource(t *testing.T) {
	ctx := context.Background()
	o, repo := newTestOrchestrator(t, nil)
	events := recordModelSelectEvents(t, o)
	oldMetadata := o.session.GetMetadata()
	next, err := repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: oldMetadata.CWD})
	if err != nil {
		t.Fatalf("Create next session: %v", err)
	}
	if _, err := next.AppendModelChange(ctx, "test-provider", "recovered-model"); err != nil {
		t.Fatalf("AppendModelChange: %v", err)
	}

	cancelled, err := o.rebindSession(ctx, next, rebindOptions{recoverState: true})
	if err != nil {
		t.Fatalf("rebindSession: %v", err)
	}
	if cancelled {
		t.Fatal("rebindSession cancelled = true, want false")
	}
	if len(*events) != 1 {
		t.Fatalf("model_select events = %d, want 1", len(*events))
	}
	assertModelSelectEvent(
		t,
		(*events)[0],
		"recovered-model",
		"initial-model",
		modelSelectSourceRestore,
	)
}

func TestForkBeforeUserEntryAndCloneAtLeaf(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
		t.Fatalf("SetThinkingLevel high: %v", err)
	}
	user1, assistant1, user2 := appendForkFixture(t, ctx, o)
	if err := o.SetModel(ctx, testModel("fallback")); err != nil {
		t.Fatalf("SetModel fallback: %v", err)
	}
	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingLow); err != nil {
		t.Fatalf("SetThinkingLevel low: %v", err)
	}

	text, cancelled, err := o.Fork(ctx, string(user2))
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if cancelled {
		t.Fatal("Fork cancelled = true, want false")
	}
	if text != "second user" {
		t.Fatalf("Fork text = %q, want second user", text)
	}
	forkIDs := sessionEntryIDs(o.session.GetEntries())
	if len(forkIDs) == 0 || forkIDs[len(forkIDs)-1] != string(assistant1) {
		t.Fatalf("fork ids = %v, want branch ending at %s", forkIDs, assistant1)
	}
	if containsString(forkIDs, string(user2)) {
		t.Fatalf("fork ids = %v, want selected user %s excluded", forkIDs, user2)
	}
	if !containsString(forkIDs, string(user1)) {
		t.Fatalf("fork ids = %v, want earlier user %s preserved", forkIDs, user1)
	}
	if got := o.currentModel(); got.ID != "recovered-model" {
		t.Fatalf("fork recovered model = %q, want recovered-model", got.ID)
	}
	o.mu.Lock()
	gotThinking := o.thinkingLevel
	o.mu.Unlock()
	if gotThinking != agentloop.ThinkingHigh {
		t.Fatalf("fork recovered thinking level = %q, want high", gotThinking)
	}

	cloneOrch, _ := newTestOrchestrator(t, nil)
	cloneUser1, cloneAssistant1, cloneUser2 := appendForkFixture(t, ctx, cloneOrch)
	cancelled, err = cloneOrch.Clone(ctx)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if cancelled {
		t.Fatal("Clone cancelled = true, want false")
	}
	wantCloneIDs := strings.Join(
		[]string{string(cloneUser1), string(cloneAssistant1), string(cloneUser2)},
		",",
	)
	if ids := sessionEntryIDs(cloneOrch.session.GetEntries()); strings.Join(ids, ",") != wantCloneIDs {
		t.Fatalf("clone ids = %v, want [%s]", ids, wantCloneIDs)
	}
}

func TestForkCanBeCancelledByExtensionBeforeCreatingSession(t *testing.T) {
	ctx := context.Background()
	var sawEvent bool
	o, repo := newTestOrchestrator(t, []ExtensionSource{
		{
			Path: "cancel-fork",
			Factory: func(api extension.ExtensionAPI) error {
				api.On(hook.EventSessionBeforeFork, func(
					_ context.Context,
					event any,
					_ extension.ExtensionContext,
				) (any, error) {
					e, ok := event.(hook.SessionBeforeForkEvent)
					if !ok {
						t.Fatalf("event type = %T, want SessionBeforeForkEvent", event)
					}
					if e.Position != "before" {
						t.Fatalf("Position = %q, want before", e.Position)
					}
					sawEvent = true
					return hook.SessionBeforeForkResult{Cancel: true}, nil
				})
				return nil
			},
		},
	})
	_, _, user2 := appendForkFixture(t, ctx, o)
	oldMetadata := o.session.GetMetadata()
	beforeList := listSessionsForCWD(t, ctx, repo, oldMetadata.CWD)

	text, cancelled, err := o.Fork(ctx, string(user2))
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if !cancelled {
		t.Fatal("Fork cancelled = false, want true")
	}
	if text != "" {
		t.Fatalf("Fork text = %q, want empty on cancellation", text)
	}
	if !sawEvent {
		t.Fatal("session_before_fork handler was not called")
	}
	if got := o.session.GetMetadata(); got.ID != oldMetadata.ID || got.Path != oldMetadata.Path {
		t.Fatalf("session changed after cancelled fork: got %+v, want %+v", got, oldMetadata)
	}
	afterList := listSessionsForCWD(t, ctx, repo, oldMetadata.CWD)
	if len(afterList) != len(beforeList) {
		t.Fatalf("session count after cancelled fork = %d, want %d", len(afterList), len(beforeList))
	}
}

func TestSessionBeforeForkCanSkipConversationRestore(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		wantPosition string
		run          func(context.Context, *Orchestrator, session.EntryID) (string, bool, error)
	}{
		{
			name:         "fork before user entry",
			wantPosition: "before",
			run: func(ctx context.Context, o *Orchestrator, user2 session.EntryID) (string, bool, error) {
				return o.Fork(ctx, string(user2))
			},
		},
		{
			name:         "clone at leaf",
			wantPosition: "at",
			run: func(ctx context.Context, o *Orchestrator, _ session.EntryID) (string, bool, error) {
				cancelled, err := o.Clone(ctx)
				return "", cancelled, err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sawEvent bool
			o, repo := newTestOrchestrator(t, nil)
			registerSkipConversationRestoreHook(t, o, tt.wantPosition, &sawEvent)
			_, _, user2 := appendForkFixture(t, ctx, o)
			oldMetadata := o.session.GetMetadata()

			text, cancelled, err := tt.run(ctx, o, user2)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if cancelled {
				t.Fatalf("%s cancelled = true, want false", tt.name)
			}
			if tt.wantPosition == "before" && text != "second user" {
				t.Fatalf("Fork text = %q, want second user", text)
			}
			if !sawEvent {
				t.Fatal("session_before_fork handler was not called")
			}

			newMetadata := o.session.GetMetadata()
			if newMetadata.ID == oldMetadata.ID || newMetadata.Path == oldMetadata.Path {
				t.Fatalf("session did not change: got %+v, old %+v", newMetadata, oldMetadata)
			}
			if newMetadata.ParentSessionPath != oldMetadata.Path {
				t.Fatalf("parent path = %q, want %q", newMetadata.ParentSessionPath, oldMetadata.Path)
			}
			if entries := o.session.GetEntries(); len(entries) != 0 {
				t.Fatalf("forked entries len = %d, want 0: %+v", len(entries), entries)
			}
			// An entry-less fork has no file yet — pi defers writes until
			// the first assistant message (session-manager.ts:843-861).
			if _, err := repo.Open(ctx, newMetadata); err == nil {
				t.Fatal("Open unflushed fork = nil error, want not found")
			}
		})
	}
}

func registerSkipConversationRestoreHook(
	t *testing.T,
	o *Orchestrator,
	wantPosition string,
	sawEvent *bool,
) {
	t.Helper()

	o.hooks.On(hook.EventSessionBeforeFork, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.SessionBeforeForkEvent)
		if !ok {
			t.Fatalf("event type = %T, want SessionBeforeForkEvent", event)
		}
		if e.Position != wantPosition {
			t.Fatalf("Position = %q, want %q", e.Position, wantPosition)
		}
		*sawEvent = true
		return hook.SessionBeforeForkResult{SkipConversationRestore: true}, nil
	})
}

func TestForkCanBeCancelledByJSExtensionBeforeCreatingSession(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	extensionPath := piConfirmDestructiveExtensionPath(t)

	tests := []struct {
		name          string
		choice        string
		wantCancelled bool
	}{
		{
			name:          "stays in current session cancels fork",
			choice:        "No, stay in current session",
			wantCancelled: true,
		},
		{
			name:          "creates fork proceeds",
			choice:        "Yes, create fork",
			wantCancelled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			repo := session.NewJsonlSessionRepo(t.TempDir())
			o, err := New(ctx, NewOptions{
				Repo:          repo,
				CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
				ModelID:       "initial-model",
				Provider: testProviderRegistry(
					testModel("initial-model"),
					testModel("recovered-model"),
					testModel("fallback"),
				),
				ThinkingLevel: agentloop.ThinkingOff,
				JSExtensions:  []string{extensionPath},
			})
			if err != nil {
				t.Fatalf("New with JS extension: %v", err)
			}
			t.Cleanup(func() {
				if err := o.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			})

			ui := &scriptedForkUI{choice: tt.choice}
			o.SetUIContext(ui)

			_, _, user2 := appendForkFixture(t, ctx, o)
			oldMetadata := o.session.GetMetadata()
			beforeList := listSessionsForCWD(t, ctx, repo, oldMetadata.CWD)

			text, cancelled, err := o.Fork(ctx, string(user2))
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			if cancelled != tt.wantCancelled {
				t.Fatalf("Fork cancelled = %v, want %v", cancelled, tt.wantCancelled)
			}
			if !ui.sawSelect {
				t.Fatal("scripted UI did not receive select request")
			}

			afterList := listSessionsForCWD(t, ctx, repo, oldMetadata.CWD)
			if tt.wantCancelled {
				if text != "" {
					t.Fatalf("Fork text = %q, want empty on cancellation", text)
				}
				if got := o.session.GetMetadata(); got.ID != oldMetadata.ID || got.Path != oldMetadata.Path {
					t.Fatalf("session changed after cancelled fork: got %+v, want %+v", got, oldMetadata)
				}
				if len(afterList) != len(beforeList) {
					t.Fatalf("session count after cancelled fork = %d, want %d", len(afterList), len(beforeList))
				}
				if !containsString(ui.notifications, "Fork cancelled") {
					t.Fatalf("notifications = %v, want Fork cancelled", ui.notifications)
				}
				return
			}

			if text != "second user" {
				t.Fatalf("Fork text = %q, want second user", text)
			}
			if got := o.session.GetMetadata(); got.ID == oldMetadata.ID || got.Path == oldMetadata.Path {
				t.Fatalf("session did not change after fork: got %+v, old %+v", got, oldMetadata)
			}
			if len(afterList) <= len(beforeList) {
				t.Fatalf("session count after fork = %d, want > %d", len(afterList), len(beforeList))
			}
		})
	}
}

func TestJSExtensionPhase2BCtxToolCallMutationAndProviderAlias(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "claude-sonnet-4-5",
		Provider:      testProviderRegistry(),
		ThinkingLevel: agentloop.ThinkingOff,
		JSExtensions:  []string{phase2BJSExtensionPath(t)},
	})
	if err != nil {
		t.Fatalf("New with JS extension: %v", err)
	}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	if o.hooks.HasHandlers(hook.EventBeforeProviderRequest) {
		t.Fatal("JS before_provider_request should alias to before_provider_payload, not stream options")
	}
	if !o.hooks.HasHandlers(hook.EventBeforeProviderPayload) {
		t.Fatal("before_provider_payload alias handler was not registered")
	}

	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hello"}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	result, err := o.hooks.Emit(ctx, hook.ToolCallEvent{
		Type:       hook.EventToolCall,
		ToolCallId: "tool-1",
		ToolName:   "read",
		Input:      map[string]any{"path": "original"},
	})
	if err != nil {
		t.Fatalf("Emit tool_call: %v", err)
	}
	toolResult, ok := result.(hook.ToolCallResult)
	if !ok {
		t.Fatalf("tool_call result type = %T, want hook.ToolCallResult", result)
	}
	if !toolResult.Block || toolResult.Reason != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("tool_call result = %#v, want blocked claude result", toolResult)
	}
	if got := toolResult.Input["path"]; got != "original:patched:1:1" {
		t.Fatalf("tool_call patched path = %v, want original:patched:1:1", got)
	}

	payloadResult, err := o.hooks.Emit(ctx, hook.BeforeProviderPayloadEvent{
		Type:    hook.EventBeforeProviderPayload,
		Model:   o.currentModel(),
		Payload: map[string]any{"prompt": "original"},
	})
	if err != nil {
		t.Fatalf("Emit before_provider_payload: %v", err)
	}
	payloadPatch, ok := payloadResult.(hook.BeforeProviderPayloadResult)
	if !ok {
		t.Fatalf("payload result type = %T, want hook.BeforeProviderPayloadResult", payloadResult)
	}
	payload, ok := payloadPatch.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", payloadPatch.Payload)
	}
	if payload["aliasPatched"] != true || payload["modelId"] != "claude-sonnet-4-5" {
		t.Fatalf("payload patch = %#v, want aliasPatched with model id", payload)
	}
}

func TestJSExtensionPhase2CRegistersAndInvokesCommandsAndTools(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "claude-sonnet-4-5",
		Provider:      testProviderRegistry(),
		ThinkingLevel: agentloop.ThinkingOff,
		JSExtensions:  []string{phase2CJSExtensionPath(t)},
	})
	if err != nil {
		t.Fatalf("New with JS extension: %v", err)
	}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	commandFound := false
	for _, command := range o.SlashCommands() {
		if command.Name == "phase2c" && command.Description == "Run phase 2c command" {
			commandFound = true
			break
		}
	}
	if !commandFound {
		t.Fatalf("SlashCommands = %#v, want phase2c", o.SlashCommands())
	}

	var jsTool tool.Tool
	for _, candidate := range o.tools {
		if candidate.Name == "phase2c_tool" {
			jsTool = candidate
			break
		}
	}
	if jsTool.Name == "" {
		t.Fatalf("tools = %#v, want phase2c_tool", o.tools)
	}
	if jsTool.Parameters["type"] != "object" {
		t.Fatalf("tool parameters = %#v, want object schema", jsTool.Parameters)
	}

	ui := &scriptedForkUI{}
	o.SetUIContext(ui)
	if err := o.DispatchCommand(ctx, "phase2c", []string{"alpha", "beta"}); err != nil {
		t.Fatalf("DispatchCommand: %v", err)
	}
	if !containsString(ui.notifications, "command:alpha beta") {
		t.Fatalf("notifications = %v, want command notification", ui.notifications)
	}

	var updates []tool.Result
	result, err := jsTool.Execute(
		ctx,
		"call-2",
		map[string]any{"text": "hello"},
		func(partial tool.Result) {
			updates = append(updates, partial)
		},
	)
	if err != nil {
		t.Fatalf("tool Execute: %v", err)
	}
	if len(updates) != 1 || len(updates[0].Content) != 1 || updates[0].Content[0].Text != "partial:hello" {
		t.Fatalf("updates = %#v, want partial text", updates)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "done:hello:claude-sonnet-4-5" {
		t.Fatalf("tool result content = %#v, want done text", result.Content)
	}
	if !result.IsError {
		t.Fatal("tool result IsError = false, want true")
	}
	details, ok := result.Details.(map[string]any)
	if !ok || details["toolCallId"] != "call-2" || details["provider"] != "anthropic" {
		t.Fatalf("tool result details = %#v, want call/provider", result.Details)
	}
	if !containsString(ui.notifications, "tool:hello:call-2") {
		t.Fatalf("notifications = %v, want tool notification", ui.notifications)
	}
}

func TestSessionReplacementEmitsShutdownAndStartEvents(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	events := []string{}

	o.hooks.On(hook.EventSessionShutdown, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.SessionShutdownEvent)
		if !ok {
			t.Fatalf("event type = %T, want SessionShutdownEvent", event)
		}
		targetSessionFile := optionalStringValue(e.TargetSessionFile)
		events = append(events, "shutdown:"+e.Reason+":"+targetSessionFile)
		return nil, nil
	})
	o.hooks.On(hook.EventSessionStart, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.SessionStartEvent)
		if !ok {
			t.Fatalf("event type = %T, want SessionStartEvent", event)
		}
		previousSessionFile := optionalStringValue(e.PreviousSessionFile)
		events = append(events, "start:"+e.Reason+":"+previousSessionFile)
		return nil, nil
	})

	previousSessionFile := o.session.GetMetadata().Path
	cancelled, err := o.NewSession(ctx, "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if cancelled {
		t.Fatal("NewSession cancelled = true, want false")
	}
	newSessionFile := o.session.GetMetadata().Path
	want := []string{
		"shutdown:new:" + newSessionFile,
		"start:new:" + previousSessionFile,
	}
	if !slicesEqual(events, want) {
		t.Fatalf("events after NewSession = %v, want %v", events, want)
	}

	_, _, user2 := appendForkFixture(t, ctx, o)
	previousSessionFile = o.session.GetMetadata().Path
	_, cancelled, err = o.Fork(ctx, string(user2))
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if cancelled {
		t.Fatal("Fork cancelled = true, want false")
	}
	forkedSessionFile := o.session.GetMetadata().Path
	want = append(want,
		"shutdown:fork:"+forkedSessionFile,
		"start:fork:"+previousSessionFile,
	)
	if !slicesEqual(events, want) {
		t.Fatalf("events after Fork = %v, want %v", events, want)
	}
}

func TestRebindSessionRefusesWhileNotIdle(t *testing.T) {
	ctx := context.Background()
	o, repo := newTestOrchestrator(t, nil)
	oldMetadata := o.session.GetMetadata()
	next, err := repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: oldMetadata.CWD})
	if err != nil {
		t.Fatalf("Create next session: %v", err)
	}

	_, cancel, _, err := o.beginRun(ctx, phaseTurn, true)
	if err != nil {
		t.Fatalf("beginRun: %v", err)
	}
	defer func() {
		cancel()
		o.finishRun()
	}()

	cancelled, err := o.rebindSession(ctx, next, rebindOptions{})
	if err != nil {
		t.Fatalf("rebindSession: %v", err)
	}
	if !cancelled {
		t.Fatal("rebindSession cancelled = false, want true while busy")
	}
	if got := o.session.GetMetadata(); got.ID != oldMetadata.ID || got.Path != oldMetadata.Path {
		t.Fatalf("session was rebound while busy: got %+v, want %+v", got, oldMetadata)
	}

	_, err = o.Prompt(ctx, PromptInput{Text: "busy"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Prompt while busy error = %v, want ErrBusy", err)
	}
}

func appendForkFixture(
	t *testing.T,
	ctx context.Context,
	o *Orchestrator,
) (session.EntryID, session.EntryID, session.EntryID) {
	t.Helper()

	user1, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "first user"}},
	})
	if err != nil {
		t.Fatalf("AppendMessage user1: %v", err)
	}
	assistant1, err := o.session.AppendMessage(ctx, ai.Message{
		Role:     ai.RoleAssistant,
		Provider: "test-provider",
		Model:    "recovered-model",
		Content:  []ai.ContentBlock{{Type: ai.ContentText, Text: "assistant"}},
	})
	if err != nil {
		t.Fatalf("AppendMessage assistant1: %v", err)
	}
	user2, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "second user"}},
	})
	if err != nil {
		t.Fatalf("AppendMessage user2: %v", err)
	}
	return user1, assistant1, user2
}

func sessionEntryIDs(entries []session.SessionEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, string(entry.ID))
	}
	return ids
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

func listSessionsForCWD(
	t *testing.T,
	ctx context.Context,
	repo *session.JsonlSessionRepo,
	cwd string,
) []session.Metadata {
	t.Helper()

	metadata, err := repo.List(ctx, session.JsonlSessionListOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("List sessions: %v", err)
	}
	return metadata
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func piConfirmDestructiveExtensionPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return filepath.Join(
		root,
		".agents",
		"references",
		"pi",
		"packages",
		"coding-agent",
		"examples",
		"extensions",
		"confirm-destructive.ts",
	)
}

func phase2BJSExtensionPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return filepath.Join(root, "internal", "extension", "jshost", "testdata", "phase2b-extension.ts")
}

func phase2CJSExtensionPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return filepath.Join(root, "internal", "extension", "jshost", "testdata", "phase2c-extension.ts")
}

type scriptedForkUI struct {
	choice        string
	sawSelect     bool
	notifications []string
}

func (ui *scriptedForkUI) Select(prompt string, options []string) (int, error) {
	ui.sawSelect = true
	wantPromptPrefix := "Fork from entry "
	if !strings.HasPrefix(prompt, wantPromptPrefix) {
		return -1, fmt.Errorf("select prompt = %q, want prefix %q", prompt, wantPromptPrefix)
	}
	for i, option := range options {
		if option == ui.choice {
			return i, nil
		}
	}
	return -1, fmt.Errorf("choice %q not in options %v", ui.choice, options)
}

func (ui *scriptedForkUI) Confirm(string) (bool, error) {
	return true, nil
}

func (ui *scriptedForkUI) Input(string) (string, error) {
	return "", nil
}

func (ui *scriptedForkUI) Editor(string, string) (string, error) {
	return "", nil
}

func (ui *scriptedForkUI) Notify(msg string) {
	ui.notifications = append(ui.notifications, msg)
}

func (ui *scriptedForkUI) SetStatus(string, string) {}

func (ui *scriptedForkUI) SetWidget(string, []string) {}

func (ui *scriptedForkUI) SetTitle(string) {}

func (ui *scriptedForkUI) SetEditorText(string) {}
