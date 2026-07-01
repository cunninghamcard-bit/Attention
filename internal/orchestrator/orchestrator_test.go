package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/auth"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

func TestPromptRejectsBusy(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	_, cancel, _, err := o.beginRun(ctx, phaseTurn, true)
	if err != nil {
		t.Fatalf("beginRun: %v", err)
	}
	defer func() {
		cancel()
		o.finishRun()
	}()

	_, err = o.Prompt(ctx, PromptInput{Text: "second prompt"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Prompt error = %v, want ErrBusy", err)
	}
}

func TestPromptBusyStreamingBehaviorQueues(t *testing.T) {
	tests := []struct {
		name         string
		behavior     string
		wantSteering []string
		wantFollowUp []string
	}{
		{
			name:         "steer",
			behavior:     "steer",
			wantSteering: []string{"queued"},
			wantFollowUp: []string{},
		},
		{
			name:         "follow-up",
			behavior:     "followUp",
			wantSteering: []string{},
			wantFollowUp: []string{"queued"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			o, _ := newTestOrchestrator(t, nil)
			_, cancel, _, err := o.beginRun(ctx, phaseTurn, true)
			if err != nil {
				t.Fatalf("beginRun: %v", err)
			}
			defer func() {
				cancel()
				o.finishRun()
			}()

			preflightSucceeded := false
			result, err := o.Prompt(ctx, PromptInput{
				Text:              "queued",
				StreamingBehavior: tt.behavior,
				PreflightResult: func(ok bool) {
					preflightSucceeded = ok
				},
			})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if result.Handled || result.Message.Role != "" {
				t.Fatalf("Prompt result = %#v, want zero result", result)
			}
			if !preflightSucceeded {
				t.Fatal("preflight result was not fired")
			}

			o.mu.Lock()
			steering := cloneMessages(o.steerQueue)
			followUp := cloneMessages(o.followUpQueue)
			o.mu.Unlock()
			if got := messageTexts(t, steering); !reflect.DeepEqual(got, tt.wantSteering) {
				t.Fatalf("steering queue = %v, want %v", got, tt.wantSteering)
			}
			if got := messageTexts(t, followUp); !reflect.DeepEqual(got, tt.wantFollowUp) {
				t.Fatalf("follow-up queue = %v, want %v", got, tt.wantFollowUp)
			}
		})
	}
}

func TestPromptAppendsNextTurnAfterUserMessage(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	rec := &recordingHarness{
		promptResult: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}},
		},
	}
	o.harness = rec

	o.mu.Lock()
	o.nextTurnQueue = append(o.nextTurnQueue, userInputMessage(UserInput{Text: "next"}))
	o.mu.Unlock()

	if _, err := o.Prompt(ctx, PromptInput{Text: "current"}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(rec.promptMessages) != 1 {
		t.Fatalf("prompt calls = %d, want 1", len(rec.promptMessages))
	}
	if got, want := messageTexts(t, rec.promptMessages[0]), []string{"current", "next"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prompt messages = %v, want %v", got, want)
	}

	o.mu.Lock()
	nextTurnLen := len(o.nextTurnQueue)
	o.mu.Unlock()
	if nextTurnLen != 0 {
		t.Fatalf("nextTurn queue len = %d, want 0", nextTurnLen)
	}
}

func TestPromptPublishesSavePointAndSettledOwnEvents(t *testing.T) {
	// recordingHarness emits no turn_end hook events, so the per-turn settle
	// handler (events.go) never fires here — this exercises the end-of-run
	// residual path only. Post-divergence the end-of-run block emits a
	// save_point ONLY when the final flush had residual pending writes (a
	// config change queued after the last turn_end); settled always fires once.
	tests := []struct {
		name            string
		mutateDuringRun bool
		wantSavePoint   bool
	}{
		{
			// No pending mutations: no residual flush, so no end-of-run
			// save_point — only settled is emitted.
			name:            "no pending mutations",
			mutateDuringRun: false,
			wantSavePoint:   false,
		},
		{
			// A mid-run config change is queued after the (absent) last
			// turn_end, so the residual flush emits a final save_point.
			name:            "pending mutations",
			mutateDuringRun: true,
			wantSavePoint:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			o, _ := newTestOrchestrator(t, nil)
			savePointHooks := recordSavePointEvents(t, o)
			settledHooks := recordSettledEvents(t, o)
			events := subscribeEventsOfType(o, EventSavePoint, EventSettled)
			rec := &recordingHarness{
				promptResult: ai.Message{
					Role:    ai.RoleAssistant,
					Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}},
				},
				onPrompt: func(ctx context.Context) {
					o.mu.Lock()
					o.nextTurnQueue = append(
						o.nextTurnQueue,
						userInputMessage(UserInput{Text: "queued next turn"}),
					)
					o.mu.Unlock()
					if !tt.mutateDuringRun {
						return
					}
					if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
						t.Fatalf("SetThinkingLevel during run: %v", err)
					}
				},
			}
			o.harness = rec

			if _, err := o.Prompt(ctx, PromptInput{Text: "current"}); err != nil {
				t.Fatalf("Prompt: %v", err)
			}

			wantSavePointCount := 0
			if tt.wantSavePoint {
				wantSavePointCount = 1
			}
			wantOwnEvents := wantSavePointCount + 1 // + settled
			if len(*events) != wantOwnEvents {
				t.Fatalf("own events = %d, want %d: %#v", len(*events), wantOwnEvents, *events)
			}
			settled := (*events)[len(*events)-1]
			if tt.wantSavePoint {
				savePoint := (*events)[0]
				if savePoint.Type != EventSavePoint || !savePoint.HadPendingMutations {
					t.Fatalf("save_point = %#v, want hadPending true", savePoint)
				}
			}
			if settled.Type != EventSettled || settled.NextTurnCount != 1 {
				t.Fatalf("settled = %#v, want nextTurnCount 1", settled)
			}

			if len(*savePointHooks) != wantSavePointCount {
				t.Fatalf("save_point hooks = %d, want %d", len(*savePointHooks), wantSavePointCount)
			}
			if tt.wantSavePoint && !(*savePointHooks)[0].HadPendingMutations {
				t.Fatalf("save_point hook hadPending = false, want true")
			}
			if len(*settledHooks) != 1 {
				t.Fatalf("settled hooks = %d, want 1", len(*settledHooks))
			}
			if (*settledHooks)[0].NextTurnCount != 1 {
				t.Fatalf(
					"settled hook nextTurnCount = %d, want 1",
					(*settledHooks)[0].NextTurnCount,
				)
			}
		})
	}
}

// TestPerTurnSettleFlushesConfigChangePerTurnEnd is the FIX C proof: the
// turn_end hook handler (events.go) must run a settle at EVERY turn_end —
// flushing pending config-change writes per turn and emitting save_point per
// turn — rather than deferring the flush and collapsing save_point to a single
// end-of-run boundary (pi agent-harness.ts:484-535). A mid-run thinking-level
// change queued before the second turn must be persisted by the time that
// second turn ends, and settled must still fire exactly once at end-of-run.
func TestPerTurnSettleFlushesConfigChangePerTurnEnd(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)

	savePointHooks := recordSavePointEvents(t, o)
	settledHooks := recordSettledEvents(t, o)
	events := subscribeEventsOfType(o, EventSavePoint, EventSettled)

	// persistedAfterSecondTurn records whether the queued thinking-level change
	// was already flushed to the session by the moment the SECOND turn_end
	// settle completes — proving per-turn persistence, not end-of-run deferral.
	var persistedAfterSecondTurn int

	rec := &recordingHarness{
		promptResult: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}},
		},
		onPrompt: func(ctx context.Context) {
			// The real harness emits a TurnEndEvent per turn
			// (harness/prompt.go:212-219); drive two turns here so the
			// registered handler runs its settle twice.

			// Turn 1: no pending config changes queued yet.
			if _, err := o.hooks.Emit(ctx, hook.TurnEndEvent{
				Type:      hook.EventTurnEnd,
				TurnIndex: 0,
			}); err != nil {
				t.Fatalf("emit turn_end 1: %v", err)
			}

			// Mid-run: queue a thinking-level change. phase != idle during the
			// run, so SetThinkingLevel appends to o.pendingWrites instead of
			// persisting immediately.
			if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
				t.Fatalf("SetThinkingLevel during run: %v", err)
			}
			if got := len(entriesOfType(o, "thinking_level_change")); got != 0 {
				t.Fatalf("thinking_level_change persisted before turn_end 2 = %d, want 0", got)
			}

			// Turn 2: the settle must flush the queued change to the session.
			if _, err := o.hooks.Emit(ctx, hook.TurnEndEvent{
				Type:      hook.EventTurnEnd,
				TurnIndex: 1,
			}); err != nil {
				t.Fatalf("emit turn_end 2: %v", err)
			}
			persistedAfterSecondTurn = len(entriesOfType(o, "thinking_level_change"))
		},
	}
	o.harness = rec

	if _, err := o.Prompt(ctx, PromptInput{Text: "go"}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// (ii) The config change was persisted by the end of the SECOND turn, not
	// deferred to end-of-run.
	if persistedAfterSecondTurn != 1 {
		t.Fatalf(
			"thinking_level_change persisted by 2nd turn_end = %d, want 1 (per-turn flush)",
			persistedAfterSecondTurn,
		)
	}

	// (i) save_point fired per turn — at least twice (turn 1 + turn 2), not once.
	// The end-of-run residual flush adds no extra save_point here because the
	// second turn already drained pendingWrites.
	if len(*savePointHooks) < 2 {
		t.Fatalf("save_point hooks = %d, want >= 2 (per-turn)", len(*savePointHooks))
	}
	if len(*savePointHooks) != 2 {
		t.Fatalf("save_point hooks = %d, want exactly 2 (no double-emit for a turn)", len(*savePointHooks))
	}
	// Turn 1 had nothing pending; turn 2 had the queued thinking-level change.
	if (*savePointHooks)[0].HadPendingMutations {
		t.Fatalf("save_point[0] hadPending = true, want false (turn 1 had no pending writes)")
	}
	if !(*savePointHooks)[1].HadPendingMutations {
		t.Fatalf("save_point[1] hadPending = false, want true (turn 2 flushed the queued change)")
	}

	var savePointEvents, settledEvents int
	for _, ev := range *events {
		switch ev.Type {
		case EventSavePoint:
			savePointEvents++
		case EventSettled:
			settledEvents++
		}
	}
	if savePointEvents != 2 {
		t.Fatalf("published save_point events = %d, want 2", savePointEvents)
	}

	// (iii) settled fires exactly once, at end-of-run.
	if settledEvents != 1 {
		t.Fatalf("published settled events = %d, want 1", settledEvents)
	}
	if len(*settledHooks) != 1 {
		t.Fatalf("settled hooks = %d, want 1", len(*settledHooks))
	}
}

func TestPromptInputHandledShortCircuits(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	rec := &recordingHarness{}
	o.harness = rec
	var secondCalled bool

	o.hooks.On(hook.EventInput, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.InputEvent)
		if !ok {
			t.Fatalf("event type = %T, want InputEvent", event)
		}
		if e.Text != "ping" {
			t.Fatalf("Text = %q, want ping", e.Text)
		}
		if e.Source != "interactive" {
			t.Fatalf("Source = %q, want interactive", e.Source)
		}
		return hook.InputResult{Action: "handled"}, nil
	})
	o.hooks.On(hook.EventInput, func(context.Context, any) (any, error) {
		secondCalled = true
		return nil, nil
	})

	result, err := o.Prompt(ctx, PromptInput{Text: "ping"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !result.Handled {
		t.Fatal("Handled = false, want true")
	}
	if result.Message.Role != "" {
		t.Fatalf("Message = %#v, want zero assistant message", result.Message)
	}
	if rec.promptCalls != 0 {
		t.Fatalf("prompt calls = %d, want 0", rec.promptCalls)
	}
	if secondCalled {
		t.Fatal("second input handler should not be called after handled result")
	}
	if entries := o.session.GetEntries(); len(entries) != 0 {
		t.Fatalf("session entries len = %d, want 0", len(entries))
	}
}

func TestPromptInputTransformRewritesUserMessageAndImages(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	rec := &recordingHarness{
		promptResult: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}},
		},
	}
	o.harness = rec

	o.hooks.On(hook.EventInput, func(_ context.Context, event any) (any, error) {
		e := event.(hook.InputEvent)
		if e.Text != "original" {
			t.Fatalf("first Text = %q, want original", e.Text)
		}
		if e.Source != "rpc" {
			t.Fatalf("first Source = %q, want rpc", e.Source)
		}
		if len(e.Images) != 1 || e.Images[0].Data != "orig" {
			t.Fatalf("first Images = %#v, want original image", e.Images)
		}
		images := append([]hook.ImageContent(nil), e.Images...)
		images = append(images, hook.ImageContent{MimeType: "image/png", Data: "one"})
		return hook.InputResult{
			Action: "transform",
			Text:   "first",
			Images: images,
		}, nil
	})
	o.hooks.On(hook.EventInput, func(_ context.Context, event any) (any, error) {
		e := event.(hook.InputEvent)
		if e.Text != "first" {
			t.Fatalf("second Text = %q, want first", e.Text)
		}
		if len(e.Images) != 2 || e.Images[1].Data != "one" {
			t.Fatalf("second Images = %#v, want accumulated first transform", e.Images)
		}
		images := append([]hook.ImageContent(nil), e.Images...)
		images = append(images, hook.ImageContent{MimeType: "image/gif", Data: "two"})
		return hook.InputResult{
			Action: "transform",
			Text:   e.Text + " second",
			Images: images,
		}, nil
	})

	result, err := o.Prompt(ctx, PromptInput{
		Text: "original",
		Content: []ai.ContentBlock{
			{Type: ai.ContentImage, MimeType: "image/jpeg", ImageData: "orig"},
		},
		Source: "rpc",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Handled {
		t.Fatal("Handled = true, want false")
	}
	if rec.promptCalls != 1 {
		t.Fatalf("prompt calls = %d, want 1", rec.promptCalls)
	}
	if len(rec.promptMessages) != 1 || len(rec.promptMessages[0]) != 1 {
		t.Fatalf("prompt messages = %#v, want one user message", rec.promptMessages)
	}
	msg, ok := message.AsAIMessage(rec.promptMessages[0][0])
	if !ok {
		t.Fatalf("prompt message type = %T, want ai.Message", rec.promptMessages[0][0])
	}
	if msg.Role != ai.RoleUser {
		t.Fatalf("prompt role = %q, want user", msg.Role)
	}
	if len(msg.Content) != 4 {
		t.Fatalf("content len = %d, want text plus three images: %#v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != ai.ContentText || msg.Content[0].Text != "first second" {
		t.Fatalf("text content = %#v, want transformed text", msg.Content[0])
	}
	gotImages := []string{msg.Content[1].ImageData, msg.Content[2].ImageData, msg.Content[3].ImageData}
	if !reflect.DeepEqual(gotImages, []string{"orig", "one", "two"}) {
		t.Fatalf("image data = %v, want [orig one two]", gotImages)
	}
}

func TestPromptInputHandlerErrorContinues(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	rec := &recordingHarness{}
	o.harness = rec
	testErr := errors.New("boom")
	var sawSource string

	o.hooks.On(hook.EventInput, func(_ context.Context, event any) (any, error) {
		sawSource = event.(hook.InputEvent).Source
		return nil, testErr
	})
	o.hooks.On(hook.EventInput, func(_ context.Context, event any) (any, error) {
		e := event.(hook.InputEvent)
		return hook.InputResult{Action: "transform", Text: e.Text + " after"}, nil
	})

	result, err := o.Prompt(ctx, PromptInput{Text: "error"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Handled {
		t.Fatal("Handled = true, want false")
	}
	if sawSource != "interactive" {
		t.Fatalf("Source = %q, want interactive", sawSource)
	}
	if rec.promptCalls != 1 {
		t.Fatalf("prompt calls = %d, want 1", rec.promptCalls)
	}
	msg, ok := message.AsAIMessage(rec.promptMessages[0][0])
	if !ok {
		t.Fatalf("prompt message type = %T, want ai.Message", rec.promptMessages[0][0])
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "error after" {
		t.Fatalf("prompt content = %#v, want transformed text after error", msg.Content)
	}
}

func TestPromptWithoutInputHandlersUnchanged(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	rec := &recordingHarness{}
	o.harness = rec

	result, err := o.Prompt(ctx, PromptInput{
		Text: "plain",
		Content: []ai.ContentBlock{
			{Type: ai.ContentImage, MimeType: "image/png", ImageData: "raw"},
		},
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Handled {
		t.Fatal("Handled = true, want false")
	}
	if rec.promptCalls != 1 {
		t.Fatalf("prompt calls = %d, want 1", rec.promptCalls)
	}
	msg, ok := message.AsAIMessage(rec.promptMessages[0][0])
	if !ok {
		t.Fatalf("prompt message type = %T, want ai.Message", rec.promptMessages[0][0])
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content len = %d, want text plus image: %#v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Text != "plain" {
		t.Fatalf("text = %q, want plain", msg.Content[0].Text)
	}
	if msg.Content[1].Type != ai.ContentImage || msg.Content[1].ImageData != "raw" {
		t.Fatalf("image content = %#v, want original image", msg.Content[1])
	}
}

func TestPromptTemplateExpandsBeforeUserMessage(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		PromptTemplates: []resource.PromptTemplate{
			{
				Name:    "greet",
				Content: "hello $1",
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &recordingHarness{}
	o.harness = rec

	result, err := o.Prompt(ctx, PromptInput{Text: "/greet world"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Handled {
		t.Fatal("Handled = true, want false")
	}
	if rec.promptCalls != 1 {
		t.Fatalf("prompt calls = %d, want 1", rec.promptCalls)
	}
	if len(rec.promptMessages) != 1 || len(rec.promptMessages[0]) != 1 {
		t.Fatalf("prompt messages = %#v, want one user message", rec.promptMessages)
	}
	msg, ok := message.AsAIMessage(rec.promptMessages[0][0])
	if !ok {
		t.Fatalf("prompt message type = %T, want ai.Message", rec.promptMessages[0][0])
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello world" {
		t.Fatalf("content = %#v, want expanded template text", msg.Content)
	}
}

func TestAssembleBuildsSystemPromptWithContextFilesAndSkills(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	cwd := t.TempDir()
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		SystemPrompt:  "base prompt",
		Tools: []extension.ToolDefinition{
			{
				Name:          "read",
				Description:   "Read files\nwith details",
				PromptSnippet: "Read files",
				Execute: func(context.Context, extension.ToolCall, tool.UpdateCallback, extension.ExtensionContext) (tool.Result, error) {
					return tool.Result{}, nil
				},
			},
		},
		ContextFiles: []resource.ContextFile{
			{Path: "/repo/AGENTS.md", Content: "project instructions"},
		},
		Skills: []resource.Skill{
			{
				Name:        "foo",
				Description: "Use foo",
				FilePath:    "/tmp/foo/SKILL.md",
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	state := o.turnState()
	got := state.SystemPrompt
	wantPrefix := "base prompt\n\n<project_context>"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("SystemPrompt prefix = %q, want %q", got, wantPrefix)
	}
	if !strings.Contains(got, `<project_instructions path="/repo/AGENTS.md">`+"\nproject instructions\n</project_instructions>") {
		t.Fatalf("SystemPrompt missing context file block:\n%s", got)
	}
	if !strings.Contains(got, "<name>foo</name>") ||
		!strings.Contains(got, "<description>Use foo</description>") ||
		!strings.Contains(got, "<location>/tmp/foo/SKILL.md</location>") {
		t.Fatalf("SystemPrompt missing skill block:\n%s", got)
	}
	if !strings.Contains(got, "\nCurrent working directory: "+cwd) {
		t.Fatalf("SystemPrompt missing cwd trailer:\n%s", got)
	}

	options := state.SystemPromptOptions
	if options.CWD != cwd {
		t.Fatalf("SystemPromptOptions.CWD = %q, want %q", options.CWD, cwd)
	}
	if options.CustomPrompt != "base prompt" {
		t.Fatalf("SystemPromptOptions.CustomPrompt = %q, want base prompt", options.CustomPrompt)
	}
	if len(options.SelectedTools) != 1 || options.SelectedTools[0] != "read" {
		t.Fatalf("SystemPromptOptions.SelectedTools = %#v, want read", options.SelectedTools)
	}
	if options.ToolSnippets["read"] != "Read files" {
		t.Fatalf("SystemPromptOptions.ToolSnippets = %#v, want read snippet", options.ToolSnippets)
	}
	if len(options.ContextFiles) != 1 || options.ContextFiles[0].Path != "/repo/AGENTS.md" ||
		options.ContextFiles[0].Content != "project instructions" {
		t.Fatalf("SystemPromptOptions.ContextFiles = %#v, want AGENTS.md instructions", options.ContextFiles)
	}
	if len(options.Skills) != 1 || options.Skills[0].Name != "foo" ||
		options.Skills[0].Description != "Use foo" {
		t.Fatalf("SystemPromptOptions.Skills = %#v, want foo skill", options.Skills)
	}
}

// TestSystemPromptOmitsToolsWithoutSnippet mirrors pi: a tool appears in the
// "Available tools" section (and the ToolSnippets map) only when it provides a
// PromptSnippet. There is no description fallback. Such a tool is still active,
// so it remains in SelectedTools.
func TestSystemPromptOmitsToolsWithoutSnippet(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	cwd := t.TempDir()
	noop := func(context.Context, extension.ToolCall, tool.UpdateCallback, extension.ExtensionContext) (tool.Result, error) {
		return tool.Result{}, nil
	}
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Tools: []extension.ToolDefinition{
			{Name: "read", Description: "Read files", PromptSnippet: "Read files", Execute: noop},
			{Name: "silent", Description: "No snippet here\nsecond line", Execute: noop},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	state := o.turnState()
	if !strings.Contains(state.SystemPrompt, "- read: Read files") {
		t.Fatalf("SystemPrompt missing read tool line:\n%s", state.SystemPrompt)
	}
	if strings.Contains(state.SystemPrompt, "silent") {
		t.Fatalf("SystemPrompt should omit snippetless tool, got:\n%s", state.SystemPrompt)
	}

	options := state.SystemPromptOptions
	if _, ok := options.ToolSnippets["silent"]; ok {
		t.Fatalf("ToolSnippets should omit snippetless tool, got %#v", options.ToolSnippets)
	}
	if options.ToolSnippets["read"] != "Read files" {
		t.Fatalf("ToolSnippets[read] = %q, want %q", options.ToolSnippets["read"], "Read files")
	}
	if !slices.Contains(options.SelectedTools, "silent") {
		t.Fatalf("SelectedTools should still list snippetless tool, got %#v", options.SelectedTools)
	}
}

func TestExtensionToolOverridesBuiltinDefinition(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	cwd := t.TempDir()
	var baseCalled bool
	var overrideCalled bool
	baseRead := extension.ToolDefinition{
		Name:          "read",
		Description:   "Base read",
		PromptSnippet: "Base read",
		Execute: func(
			context.Context,
			extension.ToolCall,
			tool.UpdateCallback,
			extension.ExtensionContext,
		) (tool.Result, error) {
			baseCalled = true
			return tool.Result{
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "base"}},
			}, nil
		},
	}
	overrideRead := extension.ToolDefinition{
		Name:          "read",
		Description:   "Override read",
		PromptSnippet: "Override read",
		Execute: func(
			context.Context,
			extension.ToolCall,
			tool.UpdateCallback,
			extension.ExtensionContext,
		) (tool.Result, error) {
			overrideCalled = true
			return tool.Result{
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "override"}},
			}, nil
		},
	}

	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Tools:         []extension.ToolDefinition{baseRead},
		Plugins: []PluginSource{{
			Path: "override",
			Factory: func(api extension.ExtensionAPI) error {
				api.RegisterTool(overrideRead)
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if len(o.toolDefs) != 1 || o.toolDefs[0].PromptSnippet != "Override read" {
		t.Fatalf("toolDefs = %#v, want extension override", o.toolDefs)
	}
	state := o.turnState()
	if !strings.Contains(state.SystemPrompt, "- read: Override read") ||
		strings.Contains(state.SystemPrompt, "Base read") {
		t.Fatalf("SystemPrompt did not use override snippet:\n%s", state.SystemPrompt)
	}

	result, err := o.tools[0].Execute(ctx, "call-1", map[string]any{}, nil)
	if err != nil {
		t.Fatalf("Execute override: %v", err)
	}
	if result.Content[0].Text != "override" || !overrideCalled || baseCalled {
		t.Fatalf(
			"tool execution result=%#v overrideCalled=%v baseCalled=%v, want override only",
			result,
			overrideCalled,
			baseCalled,
		)
	}
}

func TestSlashCommandsPrependsBuiltinsAndAggregatesExtensionTemplatesAndSkills(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	promptSource := resource.NewSourceInfo(resource.SourceProject, "/project/prompts/deploy.md", "/project/prompts")
	skillSource := resource.NewSourceInfo(resource.SourceUser, "/agent/skills/review/SKILL.md", "/agent/skills/review")
	extensionSource := resource.SourceInfo{
		Kind: resource.SourceKind("plugin"),
		Path: "commands",
	}

	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Plugins: []PluginSource{
			{
				Path: "commands",
				Factory: func(api extension.ExtensionAPI) error {
					api.RegisterCommand("zeta", extension.CommandDefinition{
						Description: "Run zeta",
					})
					api.RegisterCommand("alpha", extension.CommandDefinition{
						Description: "Run alpha",
					})
					return nil
				},
			},
		},
		PromptTemplates: []resource.PromptTemplate{
			{
				Name:        "deploy",
				Description: "Deploy app",
				Source:      promptSource,
			},
		},
		Skills: []resource.Skill{
			{
				Name:        "review",
				Description: "Review changes",
				Source:      skillSource,
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := o.SlashCommands()
	wantBuiltinNames := []string{
		"model",
		"name",
		"session",
		"fork",
		"clone",
		"tree",
		"new",
		"compact",
		"resume",
		"reload",
	}
	wantBuiltinDescriptions := []string{
		"Select model (opens selector UI)",
		"Set session display name",
		"Show session info and stats",
		"Create a new fork from a previous user message",
		"Duplicate the current session at the current position",
		"Navigate session tree (switch branches)",
		"Start a new session",
		"Manually compact the session context",
		"Resume a different session",
		"Reload keybindings, plugins, skills, prompts, and themes",
	}
	if len(wantBuiltinNames) != len(wantBuiltinDescriptions) {
		t.Fatalf("test setup mismatch: names %d descriptions %d", len(wantBuiltinNames), len(wantBuiltinDescriptions))
	}
	if len(got) != len(wantBuiltinNames)+4 {
		t.Fatalf("SlashCommands len = %d, want %d: %#v", len(got), len(wantBuiltinNames)+4, got)
	}
	for i := range wantBuiltinNames {
		if got[i].Name != wantBuiltinNames[i] ||
			got[i].Source != "builtin" ||
			got[i].Description != wantBuiltinDescriptions[i] {
			t.Fatalf(
				"SlashCommands builtin[%d] = %#v, want name/source/description %q/%q/%q",
				i,
				got[i],
				wantBuiltinNames[i],
				"builtin",
				wantBuiltinDescriptions[i],
			)
		}
		if got[i].SourceInfo != (resource.SourceInfo{}) {
			t.Fatalf("builtin SourceInfo[%d] = %#v, want zero value", i, got[i].SourceInfo)
		}
	}

	extensionPromptSkill := got[len(wantBuiltinNames):]
	wantNames := []string{"alpha", "zeta", "deploy", "skill:review"}
	wantSources := []string{"plugin", "plugin", "prompt", "skill"}
	wantDescriptions := []string{"Run alpha", "Run zeta", "Deploy app", "Review changes"}
	for i := range extensionPromptSkill {
		if extensionPromptSkill[i].Name != wantNames[i] ||
			extensionPromptSkill[i].Source != wantSources[i] ||
			extensionPromptSkill[i].Description != wantDescriptions[i] {
			t.Fatalf(
				"SlashCommands[%d] = %#v, want name/source/description %q/%q/%q",
				i,
				extensionPromptSkill[i],
				wantNames[i],
				wantSources[i],
				wantDescriptions[i],
			)
		}
	}
	if extensionPromptSkill[0].SourceInfo != extensionSource ||
		extensionPromptSkill[1].SourceInfo != extensionSource {
		t.Fatalf(
			"extension SourceInfo = %#v/%#v, want %#v",
			extensionPromptSkill[0].SourceInfo,
			extensionPromptSkill[1].SourceInfo,
			extensionSource,
		)
	}
	if extensionPromptSkill[2].SourceInfo != promptSource {
		t.Fatalf("prompt SourceInfo = %#v, want %#v", extensionPromptSkill[2].SourceInfo, promptSource)
	}
	if extensionPromptSkill[3].SourceInfo != skillSource {
		t.Fatalf("skill SourceInfo = %#v, want %#v", extensionPromptSkill[3].SourceInfo, skillSource)
	}
}

func TestFilePluginHookCommandDoesNotBreakStartup(t *testing.T) {
	runner, err := hook.LoadShellHooksData([]byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"missing-plugin-command"}]}]}}`), hook.ShellHooksOptions{
		InputFormat: hook.ShellHookInputPlugin,
	})
	if err != nil {
		t.Fatalf("LoadShellHooksData: %v", err)
	}
	o, _ := newTestOrchestrator(t, []PluginSource{{
		Path: "plugin:lazy-hook",
		Factory: func(api extension.ExtensionAPI) error {
			for _, handler := range runner.Handlers() {
				handler := handler
				api.On(handler.EventType, func(ctx context.Context, event any, extCtx extension.ExtensionContext) (any, error) {
					return handler.Handle(ctx, event, extCtx.SessionID)
				})
			}
			return nil
		},
	}})
	defer o.Close()
}

func TestSlashCommandsHonorsEnableSkillCommandsSetting(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name                  string
		settings              config.Settings
		wantSkillSlashCommand bool
	}{
		{
			name:                  "default true",
			wantSkillSlashCommand: true,
		},
		{
			name: "top-level false",
			settings: config.Settings{
				"enableSkillCommands": false,
			},
		},
		{
			name: "legacy nested false",
			settings: config.Settings{
				"skills": map[string]any{
					"enableSkillCommands": false,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := session.NewJsonlSessionRepo(t.TempDir())
			o, err := New(ctx, NewOptions{
				Repo:          repo,
				CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
				ModelID:       "initial-model",
				Provider:      testProviderRegistry(testModel("initial-model")),
				ThinkingLevel: agentloop.ThinkingOff,
				Settings:      tt.settings,
				PromptTemplates: []resource.PromptTemplate{
					{
						Name:        "deploy",
						Description: "Deploy app",
					},
				},
				Skills: []resource.Skill{
					{
						Name:        "review",
						Description: "Review changes",
					},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			got := o.SlashCommands()
			if !hasSlashCommandNamed(got, "model") {
				t.Fatalf("SlashCommands missing builtin model: %#v", got)
			}
			if !hasSlashCommandNamed(got, "deploy") {
				t.Fatalf("SlashCommands missing prompt template deploy: %#v", got)
			}
			if !hasSkillNamed(o.skills, "review") {
				t.Fatalf("skills missing review: %#v", o.skills)
			}
			hasSkillCommand := hasSlashCommandNamed(got, "skill:review")
			if hasSkillCommand != tt.wantSkillSlashCommand {
				t.Fatalf(
					"SlashCommands has skill:review = %t, want %t: %#v",
					hasSkillCommand,
					tt.wantSkillSlashCommand,
					got,
				)
			}
		})
	}
}

func TestResourcesDiscoverLoadsSkillsAndPromptsFromAllHandlers(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	firstSkillDir := writeDiscoveredSkill(t, t.TempDir(), "discover-one", "Use discover one")
	secondSkillDir := writeDiscoveredSkill(t, t.TempDir(), "discover-two", "Use discover two")
	firstPromptDir := writeDiscoveredPrompt(t, t.TempDir(), "prompt-one", "Use prompt one")
	secondPromptDir := writeDiscoveredPrompt(t, t.TempDir(), "prompt-two", "Use prompt two")
	themeDir := filepath.Join(t.TempDir(), "themes")
	if err := os.MkdirAll(themeDir, 0o700); err != nil {
		t.Fatalf("mkdir theme dir: %v", err)
	}

	var calls int
	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Plugins: []PluginSource{
			{
				Path: "resources",
				Factory: func(api extension.ExtensionAPI) error {
					api.On(hook.EventResourcesDiscover, func(
						_ context.Context,
						event any,
						_ extension.ExtensionContext,
					) (any, error) {
						calls++
						assertResourcesDiscoverEvent(t, event, cwd, "startup")
						return hook.ResourcesDiscoverResult{
							SkillPaths:  []string{firstSkillDir},
							PromptPaths: []string{firstPromptDir},
							ThemePaths:  []string{themeDir},
						}, nil
					})
					api.On(hook.EventResourcesDiscover, func(
						_ context.Context,
						event any,
						_ extension.ExtensionContext,
					) (any, error) {
						calls++
						assertResourcesDiscoverEvent(t, event, cwd, "startup")
						return &hook.ResourcesDiscoverResult{
							SkillPaths:  []string{secondSkillDir},
							PromptPaths: []string{secondPromptDir},
						}, nil
					})
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if calls != 2 {
		t.Fatalf("resources_discover calls = %d, want 2", calls)
	}

	for _, name := range []string{"discover-one", "discover-two"} {
		if !hasSkillNamed(o.skills, name) {
			t.Fatalf("skills missing %q: %#v", name, o.skills)
		}
		if !hasSlashCommandNamed(o.SlashCommands(), "skill:"+name) {
			t.Fatalf("SlashCommands missing skill:%s: %#v", name, o.SlashCommands())
		}
	}
	for _, name := range []string{"prompt-one", "prompt-two"} {
		if !hasPromptTemplateNamed(o.promptTemplates, name) {
			t.Fatalf("prompt templates missing %q: %#v", name, o.promptTemplates)
		}
		if !hasSlashCommandNamed(o.SlashCommands(), name) {
			t.Fatalf("SlashCommands missing %q: %#v", name, o.SlashCommands())
		}
	}
}

func TestReloadSettingsReloadsResourcesAndPublishesUpdate(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	agentDir := t.TempDir()
	skillRoot := t.TempDir()
	promptRoot := t.TempDir()
	writeDiscoveredSkill(t, skillRoot, "before", "Use before")
	writeDiscoveredPrompt(t, promptRoot, "before-prompt", "Use before prompt")

	skills, skillDiagnostics, err := resource.LoadSkills(resource.LoadSkillsOptions{
		CWD:             cwd,
		AgentDir:        agentDir,
		Paths:           []string{skillRoot},
		IncludeDefaults: true,
	})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	promptTemplates, promptDiagnostics, err := resource.LoadPromptTemplates(resource.LoadPromptTemplatesOptions{
		CWD:             cwd,
		AgentDir:        agentDir,
		Paths:           []string{promptRoot},
		IncludeDefaults: true,
	})
	if err != nil {
		t.Fatalf("LoadPromptTemplates: %v", err)
	}

	settingsPath := filepath.Join(agentDir, "settings.json")
	writeSettingsFile(t, settingsPath, `{
  "skills": [`+strconv.Quote(skillRoot)+`],
  "prompts": [`+strconv.Quote(promptRoot)+`]
}`)
	manager, err := config.NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:            repo,
		CreateOptions:   session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:         "initial-model",
		Provider:        testProviderRegistry(testModel("initial-model")),
		ThinkingLevel:   agentloop.ThinkingOff,
		Settings:        manager.Settings(),
		SettingsManager: manager,
		PromptTemplates: promptTemplates,
		Skills:          skills,
		PromptPaths:     []string{promptRoot},
		SkillPaths:      []string{skillRoot},
		AgentDir:        agentDir,
		Diagnostics:     append(promptDiagnostics, skillDiagnostics...),
		Tools: []extension.ToolDefinition{
			{
				Name:        "read",
				Description: "Read files",
				Execute: func(
					context.Context,
					extension.ToolCall,
					tool.UpdateCallback,
					extension.ExtensionContext,
				) (tool.Result, error) {
					return tool.Result{}, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	hookEvents := recordResourcesUpdateEvents(t, o)
	modeEvents := subscribeEventsOfType(o, EventResourcesUpdate)
	writeDiscoveredSkill(t, skillRoot, "after", "Use after")
	writeDiscoveredPrompt(t, promptRoot, "after-prompt", "Use after prompt")

	if err := o.ReloadSettings(ctx); err != nil {
		t.Fatalf("ReloadSettings: %v", err)
	}

	if len(*hookEvents) != 1 {
		t.Fatalf("resources_update hooks = %d, want 1", len(*hookEvents))
	}
	previous := resourcesSnapshotFromAny(t, (*hookEvents)[0].PreviousResources)
	current := resourcesSnapshotFromAny(t, (*hookEvents)[0].Resources)
	assertResourceSnapshotHasSkill(t, previous, "before")
	assertResourceSnapshotMissingSkill(t, previous, "after")
	assertResourceSnapshotHasSkill(t, current, "after")
	assertResourceSnapshotHasPrompt(t, current, "after-prompt")

	if len(*modeEvents) != 1 {
		t.Fatalf("resources_update mode events = %d, want 1", len(*modeEvents))
	}
	assertResourceSnapshotHasSkill(t, (*modeEvents)[0].PreviousResources, "before")
	assertResourceSnapshotHasSkill(t, (*modeEvents)[0].Resources, "after")

	state := o.turnState()
	if !strings.Contains(state.SystemPrompt, "<name>after</name>") {
		t.Fatalf("SystemPrompt missing reloaded skill:\n%s", state.SystemPrompt)
	}
}

func TestReloadSettingsLoadsNewlyEnabledFilePlugin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	agentDir := filepath.Join(root, config.AgentDirName)
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(agentDir, "settings.json")
	writeSettingsFile(t, settingsPath, `{}`)
	manager, err := config.NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:            repo,
		CreateOptions:   session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:         "initial-model",
		Provider:        testProviderRegistry(testModel("initial-model")),
		ThinkingLevel:   agentloop.ThinkingOff,
		Settings:        manager.Settings(),
		SettingsManager: manager,
		AgentDir:        agentDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if hasSlashCommandNamed(o.SlashCommands(), "demo-command") {
		t.Fatal("demo-command exists before plugin is enabled")
	}

	pluginRoot := filepath.Join(root, "plugins", "demo-plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".attention-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(pluginRoot, ".attention-plugin", "plugin.json"),
		[]byte(`{"name":"demo-plugin"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	writeDiscoveredPrompt(t, filepath.Join(pluginRoot, "commands"), "demo-command", "Use demo command")
	writePluginHandlerCommand(t, pluginRoot, "handler-command", "Run handler")
	writeSettingsFile(t, settingsPath, `{"plugins":["demo-plugin"]}`)

	if err := o.ReloadSettings(ctx); err != nil {
		t.Fatalf("ReloadSettings: %v", err)
	}
	if !hasSlashCommandNamed(o.SlashCommands(), "demo-command") {
		t.Fatalf("SlashCommands missing demo-command after reload: %#v", o.SlashCommands())
	}
	handlerCommand := slashCommandNamed(o.SlashCommands(), "handler-command")
	if handlerCommand == nil ||
		handlerCommand.Description != "Run handler" ||
		handlerCommand.ArgumentHint != "[args]" {
		t.Fatalf("SlashCommands handler-command = %#v, want handler command with metadata", handlerCommand)
	}
	notifications, err := o.DispatchCommand(ctx, "handler-command", []string{"one", "two three"})
	if err != nil {
		t.Fatalf("DispatchCommand handler-command: %v", err)
	}
	if !reflect.DeepEqual(notifications, []CommandNotification{{Message: "handler ok", Level: "warning"}}) {
		t.Fatalf("DispatchCommand notifications = %#v, want handler notification", notifications)
	}
	writePluginHandlerCommand(t, pluginRoot, "bad-command", "Bad handler")
	if err := o.ReloadSettings(ctx); err != nil {
		t.Fatalf("ReloadSettings bad command: %v", err)
	}
	if _, err := o.DispatchCommand(ctx, "bad-command", nil); err == nil || !strings.Contains(err.Error(), "non-JSON") {
		t.Fatalf("DispatchCommand bad-command err = %v, want non-JSON error", err)
	}
}

func TestNewLoadsEnabledFilePlugin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	agentDir := filepath.Join(root, config.AgentDirName)
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	pluginRoot := filepath.Join(root, "plugins", "startup-plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".attention-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(pluginRoot, ".attention-plugin", "plugin.json"),
		[]byte(`{"name":"startup-plugin"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	writePluginHandlerCommand(t, pluginRoot, "startup-command", "Run startup handler")

	settingsPath := filepath.Join(agentDir, "settings.json")
	writeSettingsFile(t, settingsPath, `{"plugins":["startup-plugin"]}`)
	manager, err := config.NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:            repo,
		CreateOptions:   session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:         "initial-model",
		Provider:        testProviderRegistry(testModel("initial-model")),
		ThinkingLevel:   agentloop.ThinkingOff,
		Settings:        manager.Settings(),
		SettingsManager: manager,
		AgentDir:        agentDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !hasSlashCommandNamed(o.SlashCommands(), "startup-command") {
		t.Fatalf("SlashCommands missing startup-command: %#v", o.SlashCommands())
	}
	wantBin := filepath.Join(pluginRoot, "bin")
	if !slices.Equal(o.extensionContext(ctx).PluginBinDirs, []string{wantBin}) {
		t.Fatalf("extension context plugin bins = %#v, want [%s]", o.extensionContext(ctx).PluginBinDirs, wantBin)
	}
}

func TestReloadSettingsUpdatesPluginBinDirs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	agentDir := filepath.Join(root, config.AgentDirName)
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(agentDir, "settings.json")
	writeSettingsFile(t, settingsPath, `{}`)
	manager, err := config.NewManager(agentDir, cwd)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:            repo,
		CreateOptions:   session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:         "initial-model",
		Provider:        testProviderRegistry(testModel("initial-model")),
		ThinkingLevel:   agentloop.ThinkingOff,
		Settings:        manager.Settings(),
		SettingsManager: manager,
		AgentDir:        agentDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pluginRoot := filepath.Join(root, "plugins", "bin-plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".attention-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(pluginRoot, ".attention-plugin", "plugin.json"),
		[]byte(`{"name":"bin-plugin"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	writeSettingsFile(t, settingsPath, `{"plugins":["bin-plugin"]}`)

	if err := o.ReloadSettings(ctx); err != nil {
		t.Fatalf("ReloadSettings: %v", err)
	}
	wantBin := filepath.Join(pluginRoot, "bin")
	if !slices.Equal(o.extensionContext(ctx).PluginBinDirs, []string{wantBin}) {
		t.Fatalf("extension context plugin bins = %#v, want [%s]", o.extensionContext(ctx).PluginBinDirs, wantBin)
	}
}

func TestReloadSettingsEmitsResourcesDiscoverReasonReload(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	agentDir := t.TempDir()
	reasons := []string{}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		AgentDir:      agentDir,
		Plugins: []PluginSource{
			{
				Path: "resources",
				Factory: func(api extension.ExtensionAPI) error {
					api.On(hook.EventResourcesDiscover, func(
						_ context.Context,
						event any,
						_ extension.ExtensionContext,
					) (any, error) {
						got, ok := event.(hook.ResourcesDiscoverEvent)
						if !ok {
							t.Fatalf("event type = %T, want ResourcesDiscoverEvent", event)
						}
						reasons = append(reasons, got.Reason)
						return nil, nil
					})
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !reflect.DeepEqual(reasons, []string{"startup"}) {
		t.Fatalf("startup resources_discover reasons = %v, want startup", reasons)
	}

	if err := o.ReloadSettings(ctx); err != nil {
		t.Fatalf("ReloadSettings: %v", err)
	}
	if !reflect.DeepEqual(reasons, []string{"startup", "reload"}) {
		t.Fatalf("resources_discover reasons = %v, want startup/reload", reasons)
	}
}

func TestReloadSettingsRebuildsExtensionToolRuntime(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	var loads int
	baseRead := extension.ToolDefinition{
		Name:          "read",
		Description:   "Base read",
		PromptSnippet: "Base read",
		Execute: func(
			context.Context,
			extension.ToolCall,
			tool.UpdateCallback,
			extension.ExtensionContext,
		) (tool.Result, error) {
			return tool.Result{
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "base"}},
			}, nil
		},
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Tools:         []extension.ToolDefinition{baseRead},
		Plugins: []PluginSource{{
			Path: "tool-reload",
			Factory: func(api extension.ExtensionAPI) error {
				loads++
				text := "override-" + strconv.Itoa(loads)
				api.RegisterTool(extension.ToolDefinition{
					Name:          "read",
					Description:   text,
					PromptSnippet: text,
					Execute: func(
						context.Context,
						extension.ToolCall,
						tool.UpdateCallback,
						extension.ExtensionContext,
					) (tool.Result, error) {
						return tool.Result{
							Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
						}, nil
					},
				})
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if loads != 1 {
		t.Fatalf("extension loads = %d, want 1", loads)
	}

	if err := o.ReloadSettings(ctx); err != nil {
		t.Fatalf("ReloadSettings: %v", err)
	}
	if loads != 2 {
		t.Fatalf("extension loads = %d, want 2 after reload", loads)
	}
	if len(o.toolDefs) != 1 || o.toolDefs[0].PromptSnippet != "override-2" {
		t.Fatalf("toolDefs = %#v, want reloaded override", o.toolDefs)
	}
	if !strings.Contains(o.turnState().SystemPrompt, "- read: override-2") {
		t.Fatalf("SystemPrompt missing reloaded tool snippet:\n%s", o.turnState().SystemPrompt)
	}
}

func TestReloadSettingsResetsExtensionProviders(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	apiName := string(ai.APIOpenAIResponses)
	baseURL := "http://localhost:8320/v1"
	var loads int

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Plugins: []PluginSource{{
			Path: "provider-reload",
			Factory: func(api extension.ExtensionAPI) error {
				loads++
				if loads > 1 {
					return nil
				}
				api.RegisterProvider("temp-provider", extension.ProviderDefinition{
					BaseURL: &baseURL,
					API:     &apiName,
					Models: []extension.ProviderModel{
						{ID: "temp-model"},
					},
				})
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := o.provider.Resolve("temp-model"); !ok {
		t.Fatal("temp-model missing before reload")
	}

	if err := o.ReloadSettings(ctx); err != nil {
		t.Fatalf("ReloadSettings: %v", err)
	}
	if _, ok := o.provider.Resolve("temp-model"); ok {
		t.Fatal("temp-model persisted after reload without provider registration")
	}
}

func TestResourcesDiscoverThemeOnlyDoesNotChangeResources(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	themeDir := filepath.Join(t.TempDir(), "themes")
	if err := os.MkdirAll(themeDir, 0o700); err != nil {
		t.Fatalf("mkdir theme dir: %v", err)
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		Plugins: []PluginSource{
			{
				Path: "theme-only",
				Factory: func(api extension.ExtensionAPI) error {
					api.On(hook.EventResourcesDiscover, func(
						context.Context,
						any,
						extension.ExtensionContext,
					) (any, error) {
						return hook.ResourcesDiscoverResult{
							ThemePaths: []string{themeDir},
						}, nil
					})
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(o.skills) != 0 || len(o.promptTemplates) != 0 {
		t.Fatalf("resources = skills %#v prompts %#v, want unchanged empty", o.skills, o.promptTemplates)
	}
}

func TestResourcesDiscoverNoHandlerLeavesConfiguredResources(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	promptSource := resource.NewSourceInfo(resource.SourceProject, "/project/prompts/deploy.md", "/project/prompts")
	skillSource := resource.NewSourceInfo(resource.SourceUser, "/agent/skills/review/SKILL.md", "/agent/skills/review")

	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		PromptTemplates: []resource.PromptTemplate{
			{
				Name:        "deploy",
				Description: "Deploy app",
				Source:      promptSource,
			},
		},
		Skills: []resource.Skill{
			{
				Name:        "review",
				Description: "Review changes",
				Source:      skillSource,
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(o.promptTemplates) != 1 || o.promptTemplates[0].Name != "deploy" {
		t.Fatalf("promptTemplates = %#v, want configured deploy only", o.promptTemplates)
	}
	if len(o.skills) != 1 || o.skills[0].Name != "review" {
		t.Fatalf("skills = %#v, want configured review only", o.skills)
	}

	got := o.SlashCommands()
	got = got[len(builtinSlashCommands):]
	if len(got) != 2 {
		t.Fatalf("extension resource commands len = %d, want 2: %#v", len(got), got)
	}
	if got[0].Name != "deploy" || got[1].Name != "skill:review" {
		t.Fatalf("extension resource commands = %#v, want deploy and skill:review", got)
	}
}

func TestSkillCommandExpandsBeforePromptTemplate(t *testing.T) {
	ctx := context.Background()
	skillDir := filepath.Join(t.TempDir(), "foo")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(`---
name: foo
description: Use foo
---
Foo body
`), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		ThinkingLevel: agentloop.ThinkingOff,
		PromptTemplates: []resource.PromptTemplate{
			{
				Name:    "skill:foo",
				Content: "template should not win",
			},
		},
		Skills: []resource.Skill{
			{
				Name:     "foo",
				FilePath: skillPath,
				BaseDir:  skillDir,
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &recordingHarness{}
	o.harness = rec

	result, err := o.Prompt(ctx, PromptInput{Text: "/skill:foo extra args"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Handled {
		t.Fatal("Handled = true, want false")
	}
	if rec.promptCalls != 1 {
		t.Fatalf("prompt calls = %d, want 1", rec.promptCalls)
	}
	msg, ok := message.AsAIMessage(rec.promptMessages[0][0])
	if !ok {
		t.Fatalf("prompt message type = %T, want ai.Message", rec.promptMessages[0][0])
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content = %#v, want one text block", msg.Content)
	}
	got := msg.Content[0].Text
	if got == "template should not win" {
		t.Fatal("prompt template expanded before skill command")
	}
	for _, want := range []string{
		"<skill name=\"foo\" location=\"" + skillPath + "\">",
		"References are relative to " + skillDir + ".",
		"Foo body\n</skill>",
		"extra args",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expanded prompt missing %q:\n%s", want, got)
		}
	}
}

func writeDiscoveredSkill(t *testing.T, root string, name string, description string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	path := filepath.Join(dir, "SKILL.md")
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\nSkill body\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	return dir
}

func writeDiscoveredPrompt(t *testing.T, root string, name string, description string) string {
	t.Helper()

	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir prompt dir: %v", err)
	}
	path := filepath.Join(root, name+".md")
	content := "---\ndescription: " + description + "\n---\nPrompt body\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	return root
}

func writePluginHandlerCommand(t *testing.T, pluginRoot string, name string, description string) {
	t.Helper()

	binDir := filepath.Join(pluginRoot, "bin")
	commandsDir := filepath.Join(pluginRoot, "commands")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir plugin bin: %v", err)
	}
	if err := os.MkdirAll(commandsDir, 0o700); err != nil {
		t.Fatalf("mkdir plugin commands: %v", err)
	}
	script := `#!/bin/sh
payload=$(cat)
case "$payload" in *'"command_name":"` + name + `"'*) ;; *) echo "bad command"; exit 2;; esac
test "$1" = "from-json" || exit 3
test "$ATTENTION_PLUGIN_ROOT" = "$(cd "$(dirname "$0")/.." && pwd)" || exit 4
test "$(cd "$ATTENTION_PROJECT_DIR" && pwd -P)" = "$(pwd -P)" || { echo "project=$ATTENTION_PROJECT_DIR pwd=$(pwd -P)" >&2; exit 5; }
case ":$PATH:" in *":$ATTENTION_PLUGIN_ROOT/bin:"*) ;; *) exit 6;; esac
test -n "$ATTENTION_AGENT_DIR" || exit 7
test -n "$ALONG_CODING_AGENT_DIR" || exit 8
`
	if name == "bad-command" {
		script += `printf 'not json'
`
	} else {
		script += `printf '{"notifications":[{"level":"warning","message":"handler ok"}]}'
`
	}
	scriptPath := filepath.Join(binDir, name)
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write plugin handler: %v", err)
	}
	config := `{
  "commands": [
    {
      "name": "` + name + `",
      "description": "` + description + `",
      "argumentHint": "[args]",
      "handler": {
        "type": "command",
        "command": "` + name + `",
        "args": ["from-json"],
        "timeout": 8
      }
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(commandsDir, "commands.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("write commands.json: %v", err)
	}
}

func assertResourcesDiscoverEvent(t *testing.T, event any, cwd string, reason string) {
	t.Helper()

	got, ok := event.(hook.ResourcesDiscoverEvent)
	if !ok {
		t.Fatalf("event type = %T, want ResourcesDiscoverEvent", event)
	}
	if got.Type != hook.EventResourcesDiscover || got.CWD != cwd || got.Reason != reason {
		t.Fatalf("event = %#v, want resources_discover %s cwd %q", got, reason, cwd)
	}
}

func hasSkillNamed(skills []resource.Skill, name string) bool {
	for _, skill := range skills {
		if skill.Name == name {
			return true
		}
	}
	return false
}

func hasPromptTemplateNamed(templates []resource.PromptTemplate, name string) bool {
	for _, template := range templates {
		if template.Name == name {
			return true
		}
	}
	return false
}

func hasSlashCommandNamed(commands []SlashCommand, name string) bool {
	return slashCommandNamed(commands, name) != nil
}

func slashCommandNamed(commands []SlashCommand, name string) *SlashCommand {
	for _, command := range commands {
		if command.Name == name {
			return &command
		}
	}
	return nil
}

func TestSubscribeTranslatesLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	msg := ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hello"}},
	}
	delta := &ai.StreamEvent{
		Type:    ai.EventTextDelta,
		Delta:   &ai.ContentBlock{Type: ai.ContentText, Text: "hello"},
		Message: &msg,
	}

	tests := []struct {
		name        string
		event       any
		wantType    string
		wantMessage bool
		wantDelta   bool
	}{
		{
			name:        "message start",
			event:       hook.MessageStartEvent{Type: hook.EventMessageStart, Message: msg},
			wantType:    EventMessageStart,
			wantMessage: true,
		},
		{
			name: "message update",
			event: hook.MessageUpdateEvent{
				Type:                  hook.EventMessageUpdate,
				Message:               msg,
				AssistantMessageEvent: delta,
			},
			wantType:    EventMessageUpdate,
			wantMessage: true,
			wantDelta:   true,
		},
		{
			name:        "message end",
			event:       hook.MessageEndEvent{Type: hook.EventMessageEnd, Message: msg},
			wantType:    EventMessageEnd,
			wantMessage: true,
		},
		{
			name:     "turn start",
			event:    hook.TurnStartEvent{Type: hook.EventTurnStart},
			wantType: EventTurnStart,
		},
		{
			name:        "turn end",
			event:       hook.TurnEndEvent{Type: hook.EventTurnEnd, Message: msg},
			wantType:    EventTurnEnd,
			wantMessage: true,
		},
		{
			name:     "agent start",
			event:    hook.AgentStartEvent{Type: hook.EventAgentStart},
			wantType: EventAgentStart,
		},
		{
			name:     "agent end",
			event:    hook.AgentEndEvent{Type: hook.EventAgentEnd},
			wantType: EventAgentEnd,
		},
		{
			name:     "tool execution start",
			event:    hook.ToolExecutionStartEvent{Type: hook.EventToolExecutionStart},
			wantType: EventToolExecutionStart,
		},
		{
			name:     "tool execution update",
			event:    hook.ToolExecutionUpdateEvent{Type: hook.EventToolExecutionUpdate},
			wantType: EventToolExecutionUpdate,
		},
		{
			name:     "tool execution end",
			event:    hook.ToolExecutionEndEvent{Type: hook.EventToolExecutionEnd},
			wantType: EventToolExecutionEnd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := []Event{}
			cancel := o.Subscribe(func(ev Event) {
				events = append(events, ev)
			})
			defer cancel()

			if _, err := o.hooks.Emit(ctx, tt.event); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			// turn_end now also drives the per-turn settle handler, which
			// publishes a save_point event as a side effect; find the
			// translated lifecycle event among what was published rather than
			// requiring exactly one.
			var got Event
			found := false
			for _, ev := range events {
				if ev.Type == tt.wantType {
					got = ev
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no %q event published, got %#v", tt.wantType, events)
			}
			if (got.Message != nil) != tt.wantMessage {
				t.Fatalf("event message present = %t, want %t", got.Message != nil, tt.wantMessage)
			}
			if tt.wantMessage && got.Message.Role != msg.Role {
				t.Fatalf("event message role = %q, want %q", got.Message.Role, msg.Role)
			}
			if (got.Delta != nil) != tt.wantDelta {
				t.Fatalf("event delta present = %t, want %t", got.Delta != nil, tt.wantDelta)
			}
			if tt.wantDelta && got.Delta != delta {
				t.Fatal("event delta did not preserve the hook stream event")
			}
		})
	}
}

func TestCompletionCriteriaToolExecutionUpdateMutationPublishesFinalEvent(t *testing.T) {
	ctx := context.Background()
	patched := map[string]any{"stdout": "patched"}
	o, _ := newTestOrchestrator(t, []PluginSource{{
		Path: "mutate-update",
		Factory: func(api extension.ExtensionAPI) error {
			api.On(hook.EventToolExecutionUpdate, func(_ context.Context, event any, _ extension.ExtensionContext) (any, error) {
				event.(*hook.ToolExecutionUpdateEvent).PartialResult = patched
				return nil, nil
			})
			return nil
		},
	}})

	var got []Event
	cancel := o.Subscribe(func(ev Event) {
		if ev.Type == EventToolExecutionUpdate {
			got = append(got, ev)
		}
	})
	defer cancel()

	ev := hook.ToolExecutionUpdateEvent{
		Type:          hook.EventToolExecutionUpdate,
		ToolCallId:    "call-1",
		ToolName:      "bash",
		Args:          map[string]any{"command": "pwd"},
		PartialResult: map[string]any{"stdout": "original"},
	}
	_, err := o.hooks.Emit(ctx, &ev)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("published updates = %d, want 1: %#v", len(got), got)
	}
	if !reflect.DeepEqual(got[0].PartialResult, patched) {
		t.Fatalf("PartialResult = %#v, want %#v", got[0].PartialResult, patched)
	}
}

func TestCompletionCriteriaToolExecutionEndMutationPublishesFinalEvent(t *testing.T) {
	ctx := context.Background()
	patchedResult := map[string]any{"stdout": "scrubbed"}
	o, _ := newTestOrchestrator(t, []PluginSource{{
		Path: "mutate-end",
		Factory: func(api extension.ExtensionAPI) error {
			api.On(hook.EventToolExecutionEnd, func(_ context.Context, event any, _ extension.ExtensionContext) (any, error) {
				ev := event.(*hook.ToolExecutionEndEvent)
				ev.Result = patchedResult
				ev.IsError = true
				return nil, nil
			})
			return nil
		},
	}})

	var got []Event
	cancel := o.Subscribe(func(ev Event) {
		if ev.Type == EventToolExecutionEnd {
			got = append(got, ev)
		}
	})
	defer cancel()

	ev := hook.ToolExecutionEndEvent{
		Type:       hook.EventToolExecutionEnd,
		ToolCallId: "call-1",
		ToolName:   "bash",
		Result:     map[string]any{"stdout": "original"},
		IsError:    false,
	}
	_, err := o.hooks.Emit(ctx, &ev)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("published ends = %d, want 1: %#v", len(got), got)
	}
	if !reflect.DeepEqual(got[0].Result, patchedResult) || !got[0].IsError {
		t.Fatalf("published end = %#v, want patched result and isError", got[0])
	}
}

func TestCompletionCriteriaToolExecutionMutationHandlersComposeAndErrorsDoNotBlockPublish(t *testing.T) {
	ctx := context.Background()
	hookErr := errors.New("patch failed")
	var secondSawFirst bool
	o, _ := newTestOrchestrator(t, []PluginSource{{
		Path: "compose-update",
		Factory: func(api extension.ExtensionAPI) error {
			api.On(hook.EventToolExecutionUpdate, func(_ context.Context, event any, _ extension.ExtensionContext) (any, error) {
				event.(*hook.ToolExecutionUpdateEvent).PartialResult = map[string]any{"stage": "first"}
				return nil, nil
			})
			api.On(hook.EventToolExecutionUpdate, func(_ context.Context, event any, _ extension.ExtensionContext) (any, error) {
				e := event.(*hook.ToolExecutionUpdateEvent)
				partial, _ := e.PartialResult.(map[string]any)
				secondSawFirst = partial["stage"] == "first"
				e.PartialResult = map[string]any{"stage": "second"}
				return nil, nil
			})
			api.On(hook.EventToolExecutionUpdate, func(context.Context, any, extension.ExtensionContext) (any, error) {
				return nil, hookErr
			})
			return nil
		},
	}})
	var reported []error
	o.hooks.OnHandlerError = func(_ string, err error) {
		reported = append(reported, err)
	}

	var got []Event
	cancel := o.Subscribe(func(ev Event) {
		if ev.Type == EventToolExecutionUpdate {
			got = append(got, ev)
		}
	})
	defer cancel()

	ev := hook.ToolExecutionUpdateEvent{
		Type:          hook.EventToolExecutionUpdate,
		ToolCallId:    "call-1",
		ToolName:      "bash",
		PartialResult: map[string]any{"stage": "original"},
	}
	_, err := o.hooks.Emit(ctx, &ev)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !secondSawFirst {
		t.Fatal("second handler did not see first handler patch")
	}
	if len(reported) != 1 || !errors.Is(reported[0], hookErr) {
		t.Fatalf("reported errors = %v, want hookErr", reported)
	}
	if len(got) != 1 {
		t.Fatalf("published updates = %d, want 1: %#v", len(got), got)
	}
	partial, _ := got[0].PartialResult.(map[string]any)
	if partial["stage"] != "second" {
		t.Fatalf("published partial = %#v, want second", got[0].PartialResult)
	}
}

func TestCompletionCriteriaToolExecutionPublisherRunsAfterNativeHandlers(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, []PluginSource{{
		Path: "publisher-last",
		Factory: func(api extension.ExtensionAPI) error {
			api.On(hook.EventToolExecutionStart, func(context.Context, any, extension.ExtensionContext) (any, error) {
				return nil, nil
			})
			api.On(hook.EventToolExecutionUpdate, func(_ context.Context, event any, _ extension.ExtensionContext) (any, error) {
				event.(*hook.ToolExecutionUpdateEvent).PartialResult = map[string]any{"stdout": "mutated partial"}
				return nil, nil
			})
			api.On(hook.EventToolExecutionEnd, func(_ context.Context, event any, _ extension.ExtensionContext) (any, error) {
				ev := event.(*hook.ToolExecutionEndEvent)
				ev.Result = map[string]any{"stdout": "mutated final"}
				ev.IsError = true
				return nil, nil
			})
			return nil
		},
	}})

	var got []Event
	cancel := o.Subscribe(func(ev Event) {
		switch ev.Type {
		case EventToolExecutionStart, EventToolExecutionUpdate, EventToolExecutionEnd:
			got = append(got, ev)
		}
	})
	defer cancel()

	startArgs := map[string]any{"command": "pwd"}
	start := hook.ToolExecutionStartEvent{
		Type:       hook.EventToolExecutionStart,
		ToolCallId: "call-1",
		ToolName:   "bash",
		Args:       startArgs,
	}
	update := hook.ToolExecutionUpdateEvent{
		Type:          hook.EventToolExecutionUpdate,
		ToolCallId:    "call-1",
		ToolName:      "bash",
		Args:          startArgs,
		PartialResult: map[string]any{"stdout": "original partial"},
	}
	end := hook.ToolExecutionEndEvent{
		Type:       hook.EventToolExecutionEnd,
		ToolCallId: "call-1",
		ToolName:   "bash",
		Result:     map[string]any{"stdout": "original final"},
		IsError:    false,
	}
	for _, event := range []any{start, &update, &end} {
		if _, err := o.hooks.Emit(ctx, event); err != nil {
			t.Fatalf("Emit %T: %v", event, err)
		}
	}

	if len(got) != 3 {
		t.Fatalf("published tool execution events = %d, want 3: %#v", len(got), got)
	}
	if got[0].Type != EventToolExecutionStart || !reflect.DeepEqual(got[0].Args, startArgs) {
		t.Fatalf("start event = %#v, want original args", got[0])
	}
	partial := got[1].PartialResult.(map[string]any)
	if got[1].Type != EventToolExecutionUpdate || partial["stdout"] != "mutated partial" {
		t.Fatalf("update event = %#v, want mutated partial", got[1])
	}
	result := got[2].Result.(map[string]any)
	if got[2].Type != EventToolExecutionEnd || result["stdout"] != "mutated final" || !got[2].IsError {
		t.Fatalf("end event = %#v, want mutated result and isError true", got[2])
	}
}

func TestModeEventFromHookPopulatesPayloadFields(t *testing.T) {
	msg := ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hello"}},
	}
	toolResult := ai.Message{
		Role:       ai.RoleToolResult,
		ToolCallID: "call-1",
		ToolName:   "shell",
		Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
	}
	args := map[string]any{"command": "pwd"}
	partial := map[string]any{"stdout": "/tmp"}
	result := map[string]any{"exitCode": 0}

	tests := []struct {
		name  string
		event any
		check func(t *testing.T, ev Event)
	}{
		{
			name: "agent end value",
			event: hook.AgentEndEvent{
				Type:     hook.EventAgentEnd,
				Messages: []any{msg, &toolResult, "skip"},
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventAgentEnd {
					t.Fatalf("type = %q, want %q", ev.Type, EventAgentEnd)
				}
				if got := len(ev.Messages); got != 2 {
					t.Fatalf("messages len = %d, want 2", got)
				}
				if ev.Messages[0].Role != ai.RoleAssistant || ev.Messages[1].Role != ai.RoleToolResult {
					t.Fatalf("messages = %#v", ev.Messages)
				}
			},
		},
		{
			name: "agent end pointer",
			event: &hook.AgentEndEvent{
				Type:     hook.EventAgentEnd,
				Messages: []any{&msg},
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventAgentEnd || len(ev.Messages) != 1 || ev.Messages[0].Role != ai.RoleAssistant {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
		{
			name: "turn end value",
			event: hook.TurnEndEvent{
				Type:        hook.EventTurnEnd,
				Message:     msg,
				ToolResults: []any{toolResult},
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventTurnEnd {
					t.Fatalf("type = %q, want %q", ev.Type, EventTurnEnd)
				}
				if ev.Message == nil || ev.Message.Role != ai.RoleAssistant {
					t.Fatalf("message = %#v", ev.Message)
				}
				if len(ev.ToolResults) != 1 || ev.ToolResults[0].ToolCallID != "call-1" {
					t.Fatalf("toolResults = %#v", ev.ToolResults)
				}
			},
		},
		{
			name: "turn end pointer",
			event: &hook.TurnEndEvent{
				Type:        hook.EventTurnEnd,
				Message:     &msg,
				ToolResults: []any{&toolResult, nil},
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventTurnEnd || ev.Message == nil || len(ev.ToolResults) != 1 {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
		{
			name: "tool execution start value",
			event: hook.ToolExecutionStartEvent{
				Type:       hook.EventToolExecutionStart,
				ToolCallId: "call-1",
				ToolName:   "shell",
				Args:       args,
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventToolExecutionStart ||
					ev.ToolCallID != "call-1" ||
					ev.ToolName != "shell" ||
					!reflect.DeepEqual(ev.Args, args) {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
		{
			name: "tool execution start pointer",
			event: &hook.ToolExecutionStartEvent{
				Type:       hook.EventToolExecutionStart,
				ToolCallId: "call-2",
				ToolName:   "read",
				Args:       args,
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventToolExecutionStart || ev.ToolCallID != "call-2" || ev.ToolName != "read" {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
		{
			name: "tool execution update value",
			event: hook.ToolExecutionUpdateEvent{
				Type:          hook.EventToolExecutionUpdate,
				ToolCallId:    "call-1",
				ToolName:      "shell",
				Args:          args,
				PartialResult: partial,
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventToolExecutionUpdate ||
					ev.ToolCallID != "call-1" ||
					ev.ToolName != "shell" ||
					!reflect.DeepEqual(ev.Args, args) ||
					!reflect.DeepEqual(ev.PartialResult, partial) {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
		{
			name: "tool execution update pointer",
			event: &hook.ToolExecutionUpdateEvent{
				Type:          hook.EventToolExecutionUpdate,
				ToolCallId:    "call-2",
				ToolName:      "read",
				Args:          args,
				PartialResult: partial,
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventToolExecutionUpdate || ev.ToolCallID != "call-2" || ev.ToolName != "read" {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
		{
			name: "tool execution end value",
			event: hook.ToolExecutionEndEvent{
				Type:       hook.EventToolExecutionEnd,
				ToolCallId: "call-1",
				ToolName:   "shell",
				Result:     result,
				IsError:    true,
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventToolExecutionEnd ||
					ev.ToolCallID != "call-1" ||
					ev.ToolName != "shell" ||
					!reflect.DeepEqual(ev.Result, result) ||
					!ev.IsError {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
		{
			name: "tool execution end pointer",
			event: &hook.ToolExecutionEndEvent{
				Type:       hook.EventToolExecutionEnd,
				ToolCallId: "call-2",
				ToolName:   "read",
				Result:     result,
				IsError:    false,
			},
			check: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != EventToolExecutionEnd || ev.ToolCallID != "call-2" || ev.ToolName != "read" || ev.IsError {
					t.Fatalf("event = %#v", ev)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := modeEventFromHook(tt.event)
			if !ok {
				t.Fatal("modeEventFromHook did not translate event")
			}
			tt.check(t, got)
		})
	}
}

func TestSubscribeCancelStopsDelivery(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	var count int
	cancel := o.Subscribe(func(Event) {
		count++
	})
	cancel()

	_, err := o.hooks.Emit(ctx, hook.TurnStartEvent{Type: hook.EventTurnStart})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if count != 0 {
		t.Fatalf("subscriber delivered %d events after cancel, want 0", count)
	}
}

func TestSubscribeConcurrentSubscribeAndEmit(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	msg := ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hello"}},
	}
	delta := &ai.StreamEvent{
		Type:    ai.EventTextDelta,
		Delta:   &ai.ContentBlock{Type: ai.ContentText, Text: "hello"},
		Message: &msg,
	}
	event := hook.MessageUpdateEvent{
		Type:                  hook.EventMessageUpdate,
		Message:               msg,
		AssistantMessageEvent: delta,
	}

	var delivered atomic.Int64
	cancel := o.Subscribe(func(Event) {
		delivered.Add(1)
	})
	defer cancel()

	var hadErr atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				cancel := o.Subscribe(func(Event) {})
				cancel()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 100 {
				if _, err := o.hooks.Emit(ctx, event); err != nil {
					hadErr.Store(true)
				}
			}
		})
	}
	wg.Wait()

	if hadErr.Load() {
		t.Fatal("Emit returned an error")
	}
	if delivered.Load() == 0 {
		t.Fatal("base subscriber received no events")
	}
}

func TestAbortClearsTransientQueues(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	if err := o.Steer(ctx, UserInput{Text: "steer"}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := o.FollowUp(ctx, UserInput{Text: "follow up"}); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	o.mu.Lock()
	o.nextTurnQueue = append(o.nextTurnQueue, userInputMessage(UserInput{Text: "next"}))
	o.mu.Unlock()

	runCtx, cancel, _, err := o.beginRun(ctx, phaseTurn, true)
	if err != nil {
		t.Fatalf("beginRun: %v", err)
	}
	defer o.finishRun()

	result, err := o.Abort(ctx)
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !result.Aborted {
		t.Fatal("AbortResult.Aborted = false, want true")
	}
	if !errors.Is(runCtx.Err(), context.Canceled) {
		t.Fatalf("run context error = %v, want context.Canceled", runCtx.Err())
	}
	cancel()

	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.steerQueue) != 0 || len(o.followUpQueue) != 0 || len(o.nextTurnQueue) != 0 {
		t.Fatalf(
			"queues len = steer %d followUp %d nextTurn %d, want all 0",
			len(o.steerQueue),
			len(o.followUpQueue),
			len(o.nextTurnQueue),
		)
	}
	if len(result.ClearedSteer) != 1 || len(result.ClearedFollowUp) != 1 || len(result.ClearedNextTurn) != 1 {
		t.Fatalf("cleared queue counts = %#v, want one item from each queue", result)
	}
}

func TestUnconsumedQueuesDoNotPersist(t *testing.T) {
	ctx := context.Background()
	o, repo := newTestOrchestrator(t, nil)
	if err := o.Steer(ctx, UserInput{Text: "steer"}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := o.FollowUp(ctx, UserInput{Text: "follow up"}); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	if _, err := o.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	if entries := o.session.GetEntries(); len(entries) != 0 {
		t.Fatalf("entries len = %d, want 0", len(entries))
	}
	// An entry-less session has no file yet — pi defers writes until the
	// first assistant message (session-manager.ts:843-861).
	if _, err := repo.Open(ctx, o.session.GetMetadata()); err == nil {
		t.Fatal("repo.Open unflushed session = nil error, want not found")
	}
}

func TestQueueModeDefaultsAndSnapshotReportsRealModes(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)

	snap := o.Snapshot()
	if snap.SteeringMode != string(QueueModeOneAtATime) || snap.FollowUpMode != string(QueueModeOneAtATime) {
		t.Fatalf("default queue modes = %q/%q, want one-at-a-time/one-at-a-time",
			snap.SteeringMode, snap.FollowUpMode)
	}

	o.SetSteeringMode(QueueModeAll)
	o.SetFollowUpMode(QueueModeAll)

	snap = o.Snapshot()
	if snap.SteeringMode != string(QueueModeAll) || snap.FollowUpMode != string(QueueModeAll) {
		t.Fatalf("snapshot queue modes = %q/%q, want all/all", snap.SteeringMode, snap.FollowUpMode)
	}
}

func TestSteeringQueueModeDraining(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)

	if err := o.Steer(ctx, UserInput{Text: "oldest"}); err != nil {
		t.Fatalf("Steer oldest: %v", err)
	}
	if err := o.Steer(ctx, UserInput{Text: "middle"}); err != nil {
		t.Fatalf("Steer middle: %v", err)
	}

	drained, err := o.drainSteeringMessages(ctx)
	if err != nil {
		t.Fatalf("drainSteeringMessages one-at-a-time: %v", err)
	}
	if got := messageTexts(t, drained); !reflect.DeepEqual(got, []string{"oldest"}) {
		t.Fatalf("one-at-a-time drained = %v, want [oldest]", got)
	}
	if got := steerQueueLen(o); got != 1 {
		t.Fatalf("steer queue len after one-at-a-time drain = %d, want 1", got)
	}

	if err := o.Steer(ctx, UserInput{Text: "newest"}); err != nil {
		t.Fatalf("Steer newest: %v", err)
	}
	o.SetSteeringMode(QueueModeAll)

	drained, err = o.drainSteeringMessages(ctx)
	if err != nil {
		t.Fatalf("drainSteeringMessages all: %v", err)
	}
	if got := messageTexts(t, drained); !reflect.DeepEqual(got, []string{"middle", "newest"}) {
		t.Fatalf("all drained = %v, want [middle newest]", got)
	}
	if got := steerQueueLen(o); got != 0 {
		t.Fatalf("steer queue len after all drain = %d, want 0", got)
	}
}

func TestFollowUpQueueModeDraining(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)

	if err := o.FollowUp(ctx, UserInput{Text: "oldest"}); err != nil {
		t.Fatalf("FollowUp oldest: %v", err)
	}
	if err := o.FollowUp(ctx, UserInput{Text: "middle"}); err != nil {
		t.Fatalf("FollowUp middle: %v", err)
	}

	drained, err := o.drainFollowUpMessages(ctx)
	if err != nil {
		t.Fatalf("drainFollowUpMessages one-at-a-time: %v", err)
	}
	if got := messageTexts(t, drained); !reflect.DeepEqual(got, []string{"oldest"}) {
		t.Fatalf("one-at-a-time drained = %v, want [oldest]", got)
	}
	if got := followUpQueueLen(o); got != 1 {
		t.Fatalf("follow-up queue len after one-at-a-time drain = %d, want 1", got)
	}

	if err := o.FollowUp(ctx, UserInput{Text: "newest"}); err != nil {
		t.Fatalf("FollowUp newest: %v", err)
	}
	o.SetFollowUpMode(QueueModeAll)

	drained, err = o.drainFollowUpMessages(ctx)
	if err != nil {
		t.Fatalf("drainFollowUpMessages all: %v", err)
	}
	if got := messageTexts(t, drained); !reflect.DeepEqual(got, []string{"middle", "newest"}) {
		t.Fatalf("all drained = %v, want [middle newest]", got)
	}
	if got := followUpQueueLen(o); got != 0 {
		t.Fatalf("follow-up queue len after all drain = %d, want 0", got)
	}
}

func TestSetThinkingLevelPublishesChangedEvent(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	events := subscribeEventsOfType(o, EventThinkingLevelChanged)

	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
		t.Fatalf("SetThinkingLevel high: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("thinking_level_changed events = %d, want 1", len(*events))
	}
	if (*events)[0].Level != string(agentloop.ThinkingHigh) {
		t.Fatalf("thinking level event = %#v, want high", (*events)[0])
	}

	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
		t.Fatalf("SetThinkingLevel unchanged: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("unchanged thinking level events = %d, want still 1", len(*events))
	}
}

func TestSetModelEmitsModelSelectHookOnChange(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	events := recordModelSelectEvents(t, o)

	nextModel := testModel("selected")
	previousModel := o.currentModel()
	if err := o.SetModel(ctx, nextModel); err != nil {
		t.Fatalf("SetModel selected: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("model_select events = %d, want 1", len(*events))
	}
	assertModelSelectEvent(
		t,
		(*events)[0],
		nextModel.ID,
		previousModel.ID,
		modelSelectSourceSet,
	)

	if err := o.SetModel(ctx, nextModel); err != nil {
		t.Fatalf("SetModel unchanged: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("unchanged model_select events = %d, want still 1", len(*events))
	}
}

func TestSetModelIgnoresModelSelectHookErrors(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	o.hooks.On(hook.EventModelSelect, func(context.Context, any) (any, error) {
		return nil, errors.New("handler failed")
	})

	if err := o.SetModel(ctx, testModel("selected")); err != nil {
		t.Fatalf("SetModel with failing model_select hook: %v", err)
	}
}

func TestCycleModelEmitsModelSelectHookWithCycleSource(t *testing.T) {
	ctx := context.Background()
	first := testModel("first")
	second := testModel("second")
	o := newTestOrchestratorWithModels(
		t,
		first.ID,
		agentloop.ThinkingOff,
		first,
		second,
	)
	events := recordModelSelectEvents(t, o)

	_, cycled, err := o.CycleModel(ctx)
	if err != nil {
		t.Fatalf("CycleModel: %v", err)
	}
	if !cycled {
		t.Fatal("CycleModel cycled = false, want true")
	}
	if len(*events) != 1 {
		t.Fatalf("model_select events = %d, want 1", len(*events))
	}
	assertModelSelectEvent(t, (*events)[0], second.ID, first.ID, modelSelectSourceCycle)
}

func TestSetThinkingLevelEmitsThinkingLevelSelectHookOnChange(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	events := recordThinkingLevelSelectEvents(t, o)

	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
		t.Fatalf("SetThinkingLevel high: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("thinking_level_select events = %d, want 1", len(*events))
	}
	assertThinkingLevelSelectEvent(
		t,
		(*events)[0],
		agentloop.ThinkingHigh,
		agentloop.ThinkingOff,
	)

	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
		t.Fatalf("SetThinkingLevel unchanged: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("unchanged thinking_level_select events = %d, want still 1", len(*events))
	}
}

func TestCycleThinkingLevelEmitsThinkingLevelSelectHook(t *testing.T) {
	ctx := context.Background()
	model := testModel("reasoning")
	model.Reasoning = true
	o := newTestOrchestratorWithModels(t, model.ID, agentloop.ThinkingMedium, model)
	events := recordThinkingLevelSelectEvents(t, o)

	level, cycled, err := o.CycleThinkingLevel(ctx)
	if err != nil {
		t.Fatalf("CycleThinkingLevel: %v", err)
	}
	if !cycled {
		t.Fatal("CycleThinkingLevel cycled = false, want true")
	}
	if level != agentloop.ThinkingHigh {
		t.Fatalf("CycleThinkingLevel level = %q, want high", level)
	}
	if len(*events) != 1 {
		t.Fatalf("thinking_level_select events = %d, want 1", len(*events))
	}
	assertThinkingLevelSelectEvent(
		t,
		(*events)[0],
		agentloop.ThinkingHigh,
		agentloop.ThinkingMedium,
	)
}

func TestQueueUpdatePublishesSteerFollowUpAndDrainTexts(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	events := subscribeEventsOfType(o, EventQueueUpdate)

	if err := o.Steer(ctx, UserInput{Text: "steer"}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	assertQueueUpdate(t, (*events)[0], []string{"steer"}, []string{})

	if err := o.FollowUp(ctx, UserInput{Text: "follow up"}); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	assertQueueUpdate(t, (*events)[1], []string{"steer"}, []string{"follow up"})

	if _, err := o.drainSteeringMessages(ctx); err != nil {
		t.Fatalf("drainSteeringMessages: %v", err)
	}
	assertQueueUpdate(t, (*events)[2], []string{}, []string{"follow up"})

	if _, err := o.drainFollowUpMessages(ctx); err != nil {
		t.Fatalf("drainFollowUpMessages: %v", err)
	}
	assertQueueUpdate(t, (*events)[3], []string{}, []string{})
}

func TestAbortPublishesQueueUpdateAfterClearingQueues(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	if err := o.Steer(ctx, UserInput{Text: "steer"}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := o.FollowUp(ctx, UserInput{Text: "follow up"}); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	events := subscribeEventsOfType(o, EventQueueUpdate)

	if _, err := o.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("queue_update events after abort = %d, want 1", len(*events))
	}
	assertQueueUpdate(t, (*events)[0], []string{}, []string{})
}

func TestSetSessionNamePublishesSessionInfoChanged(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	events := subscribeEventsOfType(o, EventSessionInfoChanged)

	if err := o.SetSessionName(ctx, "session name"); err != nil {
		t.Fatalf("SetSessionName: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("session_info_changed events = %d, want 1", len(*events))
	}
	if (*events)[0].Name != "session name" {
		t.Fatalf("session_info_changed = %#v, want name", (*events)[0])
	}
}

func TestSnapshotIncludesSessionName(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	if _, err := o.session.AppendSessionName(ctx, "  session name  "); err != nil {
		t.Fatalf("AppendSessionName: %v", err)
	}

	snap := o.Snapshot()
	if snap.SessionName != "session name" {
		t.Fatalf("Snapshot SessionName = %q, want session name", snap.SessionName)
	}
}

func TestCompactPublishesManualCompactionEvents(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	wantResult := harness.CompactionResult{
		Summary:          "summary",
		FirstKeptEntryID: "first-kept",
		TokensBefore:     42,
	}
	rec := &recordingHarness{compactResult: wantResult}
	o.harness = rec
	events := subscribeEventsOfType(o, EventCompactionStart, EventCompactionEnd)

	result, err := o.Compact(ctx, CompactOptions{CustomInstructions: "custom"})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if rec.compactCalls != 1 {
		t.Fatalf("compact calls = %d, want 1", rec.compactCalls)
	}
	if len(rec.compactInstructions) != 1 || rec.compactInstructions[0] != "custom" {
		t.Fatalf("compact instructions = %v, want [custom]", rec.compactInstructions)
	}
	if !reflect.DeepEqual(result, wantResult) {
		t.Fatalf("Compact result = %#v, want %#v", result, wantResult)
	}
	if len(*events) != 2 {
		t.Fatalf("compaction events = %d, want 2", len(*events))
	}
	if (*events)[0].Type != EventCompactionStart || (*events)[0].Reason != manualCompactionReason {
		t.Fatalf("compaction start = %#v", (*events)[0])
	}
	end := (*events)[1]
	if end.Type != EventCompactionEnd ||
		end.Reason != manualCompactionReason ||
		end.Aborted ||
		end.WillRetry ||
		end.ErrorMessage != "" {
		t.Fatalf("compaction end = %#v, want successful manual end", end)
	}
	gotResult, ok := end.Result.(harness.CompactionResult)
	if !ok {
		t.Fatalf("compaction end result type = %T, want harness.CompactionResult", end.Result)
	}
	if !reflect.DeepEqual(gotResult, wantResult) {
		t.Fatalf("compaction end result = %#v, want %#v", gotResult, wantResult)
	}
}

func TestSetModelIdlePersistsModelChange(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)

	model := testModel("model-idle")
	if err := o.SetModel(ctx, model); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	entries := entriesOfType(o, "model_change")
	if len(entries) != 1 {
		t.Fatalf("model_change entries len = %d, want 1", len(entries))
	}
	if entries[0].Provider != model.Provider || entries[0].ModelID != model.ID {
		t.Fatalf("model_change entry = %+v, want %s/%s", entries[0], model.Provider, model.ID)
	}
}

func TestSetModelBusyQueuesPendingAndFlushesAfterRun(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	runCtx, cancel, state, err := o.beginRun(ctx, phaseTurn, true)
	if err != nil {
		t.Fatalf("beginRun: %v", err)
	}
	defer func() {
		cancel()
		o.finishRun()
	}()
	_ = runCtx

	newModel := testModel("model-pending")
	if err := o.SetModel(ctx, newModel); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if state.Model.ID == newModel.ID {
		t.Fatalf("in-flight state model = %q, want old snapshot", state.Model.ID)
	}
	if entries := entriesOfType(o, "model_change"); len(entries) != 0 {
		t.Fatalf("model_change entries before flush = %d, want 0", len(entries))
	}

	if err := o.flushPendingWrites(ctx); err != nil {
		t.Fatalf("flushPendingWrites: %v", err)
	}
	entries := entriesOfType(o, "model_change")
	if len(entries) != 1 {
		t.Fatalf("model_change entries after flush = %d, want 1", len(entries))
	}
	if entries[0].ModelID != newModel.ID {
		t.Fatalf("flushed model = %q, want %q", entries[0].ModelID, newModel.ID)
	}
}

func TestCycleModelCyclesForwardAndWraps(t *testing.T) {
	ctx := context.Background()
	levelLow := "low"
	first := testModel("first")
	first.Reasoning = true
	second := testModel("second")
	second.Reasoning = true
	second.ThinkingLevelMap = map[string]*string{
		string(agentloop.ThinkingLow): &levelLow,
	}
	o := newTestOrchestratorWithModels(
		t,
		"first",
		agentloop.ThinkingHigh,
		first,
		second,
	)

	result, cycled, err := o.CycleModel(ctx)
	if err != nil {
		t.Fatalf("CycleModel first: %v", err)
	}
	if !cycled {
		t.Fatal("CycleModel first cycled = false, want true")
	}
	if result.Model.ID != "second" || result.ThinkingLevel != agentloop.ThinkingLow {
		t.Fatalf("first cycle result = %+v, want second/low", result)
	}
	snap := o.Snapshot()
	if snap.Model.ID != "second" || snap.ThinkingLevel != agentloop.ThinkingLow {
		t.Fatalf("snapshot after first cycle = %+v, want second/low", snap)
	}

	result, cycled, err = o.CycleModel(ctx)
	if err != nil {
		t.Fatalf("CycleModel second: %v", err)
	}
	if !cycled {
		t.Fatal("CycleModel second cycled = false, want true")
	}
	if result.Model.ID != "first" || result.ThinkingLevel != agentloop.ThinkingLow {
		t.Fatalf("second cycle result = %+v, want first/low", result)
	}
}

func TestCycleModelNoCycleWithOneAvailableModel(t *testing.T) {
	ctx := context.Background()
	model := testModel("only")
	o := newTestOrchestratorWithModels(t, "only", agentloop.ThinkingHigh, model)

	_, cycled, err := o.CycleModel(ctx)
	if err != nil {
		t.Fatalf("CycleModel: %v", err)
	}
	if cycled {
		t.Fatal("CycleModel cycled = true, want false")
	}
}

func TestCycleThinkingLevelReasoningSupport(t *testing.T) {
	ctx := context.Background()
	plain := testModel("plain")
	plain.Reasoning = false
	o := newTestOrchestratorWithModels(t, "plain", agentloop.ThinkingMedium, plain)

	_, cycled, err := o.CycleThinkingLevel(ctx)
	if err != nil {
		t.Fatalf("CycleThinkingLevel non-reasoning: %v", err)
	}
	if cycled {
		t.Fatal("CycleThinkingLevel non-reasoning cycled = true, want false")
	}

	off := "none"
	medium := "medium"
	xhigh := "xhigh"
	reasoning := testModel("reasoning")
	reasoning.Reasoning = true
	reasoning.ThinkingLevelMap = map[string]*string{
		string(agentloop.ThinkingOff):    &off,
		string(agentloop.ThinkingMedium): &medium,
		string(agentloop.ThinkingHigh):   nil,
		string(agentloop.ThinkingXHigh):  &xhigh,
	}
	o = newTestOrchestratorWithModels(t, "reasoning", agentloop.ThinkingMedium, reasoning)

	level, cycled, err := o.CycleThinkingLevel(ctx)
	if err != nil {
		t.Fatalf("CycleThinkingLevel reasoning: %v", err)
	}
	if !cycled {
		t.Fatal("CycleThinkingLevel reasoning cycled = false, want true")
	}
	if level != agentloop.ThinkingXHigh {
		t.Fatalf("CycleThinkingLevel level = %q, want xhigh", level)
	}
	if snap := o.Snapshot(); snap.ThinkingLevel != agentloop.ThinkingXHigh {
		t.Fatalf("snapshot thinkingLevel = %q, want xhigh", snap.ThinkingLevel)
	}
}

func TestLastAssistantTextSkipsAbortedEmptyAndConcatenatesText(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "older"}},
	}); err != nil {
		t.Fatalf("AppendMessage older: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			{Type: ai.ContentText, Text: " first"},
			{Type: ai.ContentThinking, Thinking: "ignored"},
			{Type: ai.ContentText, Text: " second "},
		},
	}); err != nil {
		t.Fatalf("AppendMessage text: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:       ai.RoleAssistant,
		StopReason: ai.StopReasonAborted,
	}); err != nil {
		t.Fatalf("AppendMessage aborted: %v", err)
	}

	text, ok := o.LastAssistantText()
	if !ok {
		t.Fatal("LastAssistantText ok = false, want true")
	}
	if text != "first second" {
		t.Fatalf("LastAssistantText = %q, want first second", text)
	}
}

func TestSessionStatsCountsMessagesToolCallsUsageAndCost(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	model := testModel("initial-model")
	model.ContextWindow = 200
	if err := o.SetModel(ctx, model); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hello"}},
	}); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentToolCall, ToolName: "nil-usage"}},
	}); err != nil {
		t.Fatalf("AppendMessage nil usage assistant: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			{Type: ai.ContentText, Text: "using tool"},
			{Type: ai.ContentToolCall, ToolName: "nil-cost"},
		},
		Usage: &ai.Usage{
			Input:       3,
			Output:      5,
			CacheRead:   7,
			CacheWrite:  11,
			TotalTokens: 999,
		},
	}); err != nil {
		t.Fatalf("AppendMessage nil cost assistant: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:       ai.RoleToolResult,
		ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("AppendMessage tool result: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			{Type: ai.ContentToolCall, ToolName: "one"},
			{Type: ai.ContentToolCall, ToolName: "two"},
		},
		Usage: &ai.Usage{
			Input:       13,
			Output:      17,
			CacheRead:   19,
			CacheWrite:  23,
			TotalTokens: 999,
			Cost:        &ai.Cost{Total: 1.25},
		},
	}); err != nil {
		t.Fatalf("AppendMessage cost assistant: %v", err)
	}

	stats := o.SessionStats()
	metadata := o.session.GetMetadata()
	if stats.SessionID != metadata.ID || stats.SessionFile != metadata.Path {
		t.Fatalf(
			"metadata = %q/%q, want %q/%q",
			stats.SessionID,
			stats.SessionFile,
			metadata.ID,
			metadata.Path,
		)
	}
	if stats.UserMessages != 1 || stats.AssistantMessages != 3 || stats.ToolResults != 1 {
		t.Fatalf(
			"role counts = user:%d assistant:%d toolResult:%d, want 1/3/1",
			stats.UserMessages,
			stats.AssistantMessages,
			stats.ToolResults,
		)
	}
	if stats.ToolCalls != 4 || stats.TotalMessages != 5 {
		t.Fatalf("toolCalls/totalMessages = %d/%d, want 4/5", stats.ToolCalls, stats.TotalMessages)
	}
	wantTokens := SessionStatsTokens{
		Input:      16,
		Output:     22,
		CacheRead:  26,
		CacheWrite: 34,
		Total:      98,
	}
	if stats.Tokens != wantTokens {
		t.Fatalf("tokens = %+v, want %+v", stats.Tokens, wantTokens)
	}
	if stats.Cost != 1.25 {
		t.Fatalf("cost = %v, want 1.25", stats.Cost)
	}
	if stats.ContextUsage == nil {
		t.Fatal("contextUsage is nil")
	}
	if stats.ContextUsage.Tokens != 999 ||
		stats.ContextUsage.ContextWindow != 200 ||
		stats.ContextUsage.Percent != 499.5 {
		t.Fatalf("contextUsage = %+v, want 999/200/499.5", stats.ContextUsage)
	}
}

func TestSessionStatsContextUsageNilWhenModelHasNoWindow(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role: ai.RoleAssistant,
		Usage: &ai.Usage{
			Input:  10,
			Output: 5,
		},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	stats := o.SessionStats()
	if stats.ContextUsage != nil {
		t.Fatalf("contextUsage = %+v, want nil", stats.ContextUsage)
	}
}

func TestSessionStatsContextUsageNilAfterCompactionWithoutPostUsage(t *testing.T) {
	ctx := context.Background()
	o, _ := newTestOrchestrator(t, nil)
	model := testModel("initial-model")
	model.ContextWindow = 200
	if err := o.SetModel(ctx, model); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	keptID, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "keep me"}},
	})
	if err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role: ai.RoleAssistant,
		Usage: &ai.Usage{
			TotalTokens: 100,
		},
	}); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}
	if _, err := o.session.AppendCompaction(ctx, "summary", keptID, 100, nil, false); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}

	stats := o.SessionStats()
	if stats.ContextUsage != nil {
		t.Fatalf("contextUsage = %+v, want nil", stats.ContextUsage)
	}
}

func TestOpenRecoversModelAndThinkingLevel(t *testing.T) {
	ctx := context.Background()
	o, repo := newTestOrchestrator(t, nil)
	// Flush to disk so Open can recover state: pi defers writes until the
	// first assistant message (session-manager.ts:843-861).
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}},
	}); err != nil {
		t.Fatalf("AppendMessage flush: %v", err)
	}

	model := testModel("recovered-model")
	if err := o.SetModel(ctx, model); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := o.SetThinkingLevel(ctx, agentloop.ThinkingHigh); err != nil {
		t.Fatalf("SetThinkingLevel: %v", err)
	}

	recovered, err := Open(ctx, OpenOptions{
		Repo:          repo,
		Metadata:      o.session.GetMetadata(),
		ModelID:       "fallback",
		Provider:      testProviderRegistry(testModel("fallback"), model),
		ThinkingLevel: agentloop.ThinkingOff,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if got := recovered.currentModel(); got.Provider != model.Provider || got.ID != model.ID {
		t.Fatalf("recovered model = %+v, want %s/%s", got, model.Provider, model.ID)
	}
	recovered.mu.Lock()
	gotLevel := recovered.thinkingLevel
	recovered.mu.Unlock()
	if gotLevel != agentloop.ThinkingHigh {
		t.Fatalf("recovered thinkingLevel = %q, want %q", gotLevel, agentloop.ThinkingHigh)
	}
}

func TestOpenRecoversDuplicateModelIDByProvider(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	initial := ai.Model{ID: "initial-model", Name: "initial-model", Provider: "provider-a"}
	modelA := ai.Model{ID: "shared-model", Name: "A", Provider: "provider-a"}
	modelB := ai.Model{ID: "shared-model", Name: "B", Provider: "provider-b"}
	prov := provider.New([]ai.Model{initial, modelA, modelB}, staticAuthResolver{})
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       initial.ID,
		Provider:      prov,
		ThinkingLevel: agentloop.ThinkingOff,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Flush to disk so Open can recover state: pi defers writes until the
	// first assistant message (session-manager.ts:843-861).
	if _, err := o.session.AppendMessage(ctx, ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "ok"}},
	}); err != nil {
		t.Fatalf("AppendMessage flush: %v", err)
	}

	if err := o.SetModel(ctx, modelB); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	recovered, err := Open(ctx, OpenOptions{
		Repo:          repo,
		Metadata:      o.session.GetMetadata(),
		ModelID:       initial.ID,
		Provider:      provider.New([]ai.Model{initial, modelA, modelB}, staticAuthResolver{}),
		ThinkingLevel: agentloop.ThinkingOff,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if got := recovered.currentModel(); got.Provider != "provider-b" || got.ID != "shared-model" {
		t.Fatalf("recovered model = %+v, want provider-b/shared-model", got)
	}
}

func TestNewRejectsAmbiguousModelID(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	models := []ai.Model{
		{ID: "shared-model", Name: "A", Provider: "provider-a"},
		{ID: "shared-model", Name: "B", Provider: "provider-b"},
	}
	_, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "shared-model",
		Provider:      provider.New(models, staticAuthResolver{}),
		ThinkingLevel: agentloop.ThinkingOff,
	})
	if err == nil {
		t.Fatal("New error = nil, want ambiguous model error")
	}
	if !strings.Contains(err.Error(), "ambiguous model") ||
		!strings.Contains(err.Error(), "provider-a/shared-model") ||
		!strings.Contains(err.Error(), "provider-b/shared-model") {
		t.Fatalf("New error = %v, want ambiguous provider-qualified matches", err)
	}
}

func TestDispatchCommandRoutesToRegisteredHandler(t *testing.T) {
	ctx := context.Background()
	var gotArgs []string
	var gotModel ai.Model
	var gotIdle bool
	o, _ := newTestOrchestrator(t, []PluginSource{
		{
			Path: "commands",
			Factory: func(api extension.ExtensionAPI) error {
				api.RegisterCommand("run", extension.CommandDefinition{
					Handler: func(
						_ context.Context,
						args []string,
						extCtx extension.ExtensionContext,
					) error {
						gotArgs = append([]string(nil), args...)
						gotModel = extCtx.Model()
						gotIdle = extCtx.IsIdle()
						extCtx.Notify("ran", "info")
						extCtx.Notify("warned", "warning")
						return nil
					},
				})
				return nil
			},
		},
	})

	notifications, err := o.DispatchCommand(ctx, "run", []string{"a", "b"})
	if err != nil {
		t.Fatalf("DispatchCommand: %v", err)
	}
	if !reflect.DeepEqual(notifications, []CommandNotification{
		{Message: "ran", Level: "info"},
		{Message: "warned", Level: "warning"},
	}) {
		t.Fatalf("DispatchCommand notifications = %#v", notifications)
	}
	if !reflect.DeepEqual(gotArgs, []string{"a", "b"}) {
		t.Fatalf("args = %v, want [a b]", gotArgs)
	}
	if gotModel.ID != "initial-model" {
		t.Fatalf("model ID = %q, want initial-model", gotModel.ID)
	}
	if !gotIdle {
		t.Fatal("IsIdle = false, want true")
	}
}

func TestExtensionContextModelRegistryAndIsAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	models := []ai.Model{
		{
			ID:            "registry-a",
			Name:          "Registry A",
			Provider:      "provider-a",
			ContextWindow: 111,
		},
		{
			ID:            "registry-b",
			Name:          "Registry B",
			Provider:      "provider-b",
			ContextWindow: 222,
			Reasoning:     true,
		},
	}
	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "registry-a",
		Provider:      provider.New(models, staticAuthResolver{}),
		ThinkingLevel: agentloop.ThinkingOff,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	extCtx := o.extensionContext(ctx)
	got := extCtx.ModelRegistry()
	if len(got) != len(models) {
		t.Fatalf("ModelRegistry len = %d, want %d: %#v", len(got), len(models), got)
	}
	byID := map[string]extension.ModelInfo{}
	for _, info := range got {
		byID[info.ID] = info
	}
	if byID["registry-a"].Provider != "provider-a" ||
		byID["registry-a"].DisplayName != "Registry A" ||
		byID["registry-a"].ContextWindow != 111 ||
		byID["registry-a"].Reasoning {
		t.Fatalf("registry-a info = %#v", byID["registry-a"])
	}
	if byID["registry-b"].Provider != "provider-b" ||
		byID["registry-b"].DisplayName != "Registry B" ||
		byID["registry-b"].ContextWindow != 222 ||
		!byID["registry-b"].Reasoning {
		t.Fatalf("registry-b info = %#v", byID["registry-b"])
	}
	if extCtx.IsAborted() {
		t.Fatal("IsAborted before cancel = true, want false")
	}
	cancel()
	if !extCtx.IsAborted() {
		t.Fatal("IsAborted after cancel = false, want true")
	}
}

func TestDuplicateCommandNameFailsAssembly(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	cwd := t.TempDir()

	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "initial-model",
		Provider:      testProviderRegistry(testModel("initial-model")),
		Plugins: []PluginSource{
			{
				Path: "first",
				Factory: func(api extension.ExtensionAPI) error {
					api.RegisterCommand("same", extension.CommandDefinition{
						Handler: func(
							context.Context,
							[]string,
							extension.ExtensionContext,
						) error {
							return nil
						},
					})
					return nil
				},
			},
			{
				Path: "second",
				Factory: func(api extension.ExtensionAPI) error {
					api.RegisterCommand("same", extension.CommandDefinition{
						Handler: func(
							context.Context,
							[]string,
							extension.ExtensionContext,
						) error {
							return nil
						},
					})
					return nil
				},
			},
		},
	})
	// pi keeps loading the remaining extensions and records the failure as a
	// per-extension error instead of aborting startup (loader.ts:380-438).
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	found := false
	for _, d := range o.ResourceDiagnostics() {
		if d.Type == resource.DiagnosticError && d.Path == "second" &&
			strings.Contains(d.Message, "duplicate command") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want duplicate command load error for second", o.ResourceDiagnostics())
	}
}

func TestExtensionProviderRegistersBeforeModelResolution(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	cwd := t.TempDir()
	name := "Local OpenAI"
	baseURL := "http://localhost:8317/v1"
	apiName := string(ai.APIOpenAIResponses)
	apiKey := "local-key"
	authHeader := true

	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "local-gpt-5.5",
		Provider:      testProviderRegistry(),
		Plugins: []PluginSource{
			{
				Path: "provider",
				Factory: func(api extension.ExtensionAPI) error {
					api.RegisterProvider("local-openai", extension.ProviderDefinition{
						Name:       &name,
						BaseURL:    &baseURL,
						API:        &apiName,
						APIKey:     &apiKey,
						AuthHeader: &authHeader,
						Models: []extension.ProviderModel{
							{ID: "local-gpt-5.5"},
						},
					})
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	model := o.currentModel()
	if model.ID != "local-gpt-5.5" || model.Provider != "local-openai" {
		t.Fatalf("current model = %+v, want local-openai/local-gpt-5.5", model)
	}
	if model.BaseURL != baseURL || model.API != ai.APIOpenAIResponses {
		t.Fatalf("current model endpoint = %s/%s, want %s/%s", model.BaseURL, model.API, baseURL, ai.APIOpenAIResponses)
	}
	if !model.AuthHeader {
		t.Fatal("current model AuthHeader = false, want true")
	}
}

func TestExtensionOAuthProviderRegistersBeforeModelResolution(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	cwd := t.TempDir()
	name := "Local OAuth"
	baseURL := "http://localhost:8318/v1"
	apiName := string(ai.APIOpenAIResponses)
	authHeader := true

	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: cwd},
		ModelID:       "local-oauth-gpt",
		Provider:      testProviderRegistry(),
		Plugins: []PluginSource{
			{
				Path: "oauth-provider",
				Factory: func(api extension.ExtensionAPI) error {
					api.RegisterProvider("local-oauth", extension.ProviderDefinition{
						Name:       &name,
						BaseURL:    &baseURL,
						API:        &apiName,
						AuthHeader: &authHeader,
						OAuth:      &extension.ProviderOAuth{Name: "Local OAuth"},
						Models: []extension.ProviderModel{
							{ID: "local-oauth-gpt"},
						},
					})
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	model := o.currentModel()
	if model.ID != "local-oauth-gpt" || model.Provider != "local-oauth" {
		t.Fatalf("current model = %+v, want local-oauth/local-oauth-gpt", model)
	}
	if model.BaseURL != baseURL || model.API != ai.APIOpenAIResponses {
		t.Fatalf("current model endpoint = %s/%s, want %s/%s", model.BaseURL, model.API, baseURL, ai.APIOpenAIResponses)
	}
	if !model.AuthHeader {
		t.Fatal("current model AuthHeader = false, want true")
	}
}

func TestProviderAuthResolverMapsRegistryAuth(t *testing.T) {
	ctx := context.Background()
	prov := testProviderRegistry()
	baseURL := "http://localhost:8317/v1"
	apiName := string(ai.APIOpenAIResponses)
	apiKey := "local-key"
	authHeader := true
	if err := prov.RegisterProvider("local-openai", provider.ProviderConfig{
		BaseURL:    &baseURL,
		API:        &apiName,
		APIKey:     &apiKey,
		AuthHeader: &authHeader,
		Headers:    map[string]string{"X-Provider": "provider-header"},
		Models: []provider.ModelDefinition{
			{
				ID:      "local-gpt-5.5",
				Headers: map[string]string{"X-Model": "model-header"},
			},
		},
	}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	model, ok := prov.Resolve("local-gpt-5.5")
	if !ok {
		t.Fatal("Resolve local-gpt-5.5 = false")
	}

	got, err := providerAuthResolver(prov)(ctx, model)
	if err != nil {
		t.Fatalf("providerAuthResolver: %v", err)
	}
	if got.APIKey != apiKey {
		t.Fatalf("APIKey = %q, want %q", got.APIKey, apiKey)
	}
	if got.Headers["Authorization"] != "Bearer "+apiKey {
		t.Fatalf("Authorization header = %q, want bearer key", got.Headers["Authorization"])
	}
	if got.Headers["X-Provider"] != "provider-header" || got.Headers["X-Model"] != "model-header" {
		t.Fatalf("headers = %#v, want provider and model headers", got.Headers)
	}
}

func TestUnknownModelListsRegistryModels(t *testing.T) {
	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	_, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       "missing-model",
		Provider:      testProviderRegistry(testModel("available-model")),
	})
	if err == nil {
		t.Fatal("New error = nil, want unknown model error")
	}
	if !strings.Contains(err.Error(), "missing-model") || !strings.Contains(err.Error(), "available-model") {
		t.Fatalf("New error = %v, want missing and available model IDs", err)
	}
}

func messageTexts(t *testing.T, messages []message.AgentMessage) []string {
	t.Helper()
	out := make([]string, 0, len(messages))
	for _, msg := range messages {
		aiMsg, ok := message.AsAIMessage(msg)
		if !ok {
			t.Fatalf("message %T is not an ai message", msg)
		}
		if len(aiMsg.Content) != 1 || aiMsg.Content[0].Type != ai.ContentText {
			t.Fatalf("message content = %#v, want one text block", aiMsg.Content)
		}
		out = append(out, aiMsg.Content[0].Text)
	}
	return out
}

func steerQueueLen(o *Orchestrator) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.steerQueue)
}

func followUpQueueLen(o *Orchestrator) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.followUpQueue)
}

func subscribeEventsOfType(o *Orchestrator, eventTypes ...string) *[]Event {
	eventTypeSet := make(map[string]bool, len(eventTypes))
	for _, eventType := range eventTypes {
		eventTypeSet[eventType] = true
	}
	events := []Event{}
	o.Subscribe(func(ev Event) {
		if eventTypeSet[ev.Type] {
			events = append(events, ev)
		}
	})
	return &events
}

func recordModelSelectEvents(t *testing.T, o *Orchestrator) *[]hook.ModelSelectEvent {
	t.Helper()

	events := []hook.ModelSelectEvent{}
	o.hooks.On(hook.EventModelSelect, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.ModelSelectEvent)
		if !ok {
			t.Fatalf("event type = %T, want ModelSelectEvent", event)
		}
		events = append(events, e)
		return nil, nil
	})
	return &events
}

func assertModelSelectEvent(
	t *testing.T,
	event hook.ModelSelectEvent,
	wantModelID string,
	wantPreviousModelID string,
	wantSource string,
) {
	t.Helper()

	if event.Type != hook.EventModelSelect {
		t.Fatalf("model_select Type = %q, want %q", event.Type, hook.EventModelSelect)
	}
	model, ok := event.Model.(ai.Model)
	if !ok {
		t.Fatalf("model_select Model type = %T, want ai.Model", event.Model)
	}
	previousModel, ok := event.PreviousModel.(ai.Model)
	if !ok {
		t.Fatalf("model_select PreviousModel type = %T, want ai.Model", event.PreviousModel)
	}
	if model.ID != wantModelID ||
		previousModel.ID != wantPreviousModelID ||
		event.Source != wantSource {
		t.Fatalf(
			"model_select = model %q previous %q source %q, want %q/%q/%q",
			model.ID,
			previousModel.ID,
			event.Source,
			wantModelID,
			wantPreviousModelID,
			wantSource,
		)
	}
}

func recordThinkingLevelSelectEvents(
	t *testing.T,
	o *Orchestrator,
) *[]hook.ThinkingLevelSelectEvent {
	t.Helper()

	events := []hook.ThinkingLevelSelectEvent{}
	o.hooks.On(hook.EventThinkingLevelSelect, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.ThinkingLevelSelectEvent)
		if !ok {
			t.Fatalf("event type = %T, want ThinkingLevelSelectEvent", event)
		}
		events = append(events, e)
		return nil, nil
	})
	return &events
}

func recordSavePointEvents(t *testing.T, o *Orchestrator) *[]hook.SavePointEvent {
	t.Helper()

	events := []hook.SavePointEvent{}
	o.hooks.On(hook.EventSavePoint, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.SavePointEvent)
		if !ok {
			t.Fatalf("event type = %T, want SavePointEvent", event)
		}
		events = append(events, e)
		return nil, nil
	})
	return &events
}

func recordSettledEvents(t *testing.T, o *Orchestrator) *[]hook.SettledEvent {
	t.Helper()

	events := []hook.SettledEvent{}
	o.hooks.On(hook.EventSettled, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.SettledEvent)
		if !ok {
			t.Fatalf("event type = %T, want SettledEvent", event)
		}
		events = append(events, e)
		return nil, nil
	})
	return &events
}

func recordResourcesUpdateEvents(t *testing.T, o *Orchestrator) *[]hook.ResourcesUpdateEvent {
	t.Helper()

	events := []hook.ResourcesUpdateEvent{}
	o.hooks.On(hook.EventResourcesUpdate, func(_ context.Context, event any) (any, error) {
		e, ok := event.(hook.ResourcesUpdateEvent)
		if !ok {
			t.Fatalf("event type = %T, want ResourcesUpdateEvent", event)
		}
		events = append(events, e)
		return nil, nil
	})
	return &events
}

func resourcesSnapshotFromAny(t *testing.T, value any) ResourcesSnapshot {
	t.Helper()

	snap, ok := value.(ResourcesSnapshot)
	if !ok {
		t.Fatalf("resource snapshot type = %T, want ResourcesSnapshot", value)
	}
	return snap
}

func assertResourceSnapshotHasSkill(t *testing.T, snap ResourcesSnapshot, name string) {
	t.Helper()

	for _, skill := range snap.Skills {
		if skill.Name == name {
			return
		}
	}
	t.Fatalf("resources skills missing %q: %#v", name, snap.Skills)
}

func assertResourceSnapshotMissingSkill(t *testing.T, snap ResourcesSnapshot, name string) {
	t.Helper()

	for _, skill := range snap.Skills {
		if skill.Name == name {
			t.Fatalf("resources skills unexpectedly include %q: %#v", name, snap.Skills)
		}
	}
}

func assertResourceSnapshotHasPrompt(t *testing.T, snap ResourcesSnapshot, name string) {
	t.Helper()

	for _, template := range snap.PromptTemplates {
		if template.Name == name {
			return
		}
	}
	t.Fatalf("resources promptTemplates missing %q: %#v", name, snap.PromptTemplates)
}

func assertThinkingLevelSelectEvent(
	t *testing.T,
	event hook.ThinkingLevelSelectEvent,
	wantLevel agentloop.ThinkingLevel,
	wantPreviousLevel agentloop.ThinkingLevel,
) {
	t.Helper()

	if event.Type != hook.EventThinkingLevelSelect {
		t.Fatalf(
			"thinking_level_select Type = %q, want %q",
			event.Type,
			hook.EventThinkingLevelSelect,
		)
	}
	if event.Level != string(wantLevel) || event.PreviousLevel != string(wantPreviousLevel) {
		t.Fatalf(
			"thinking_level_select = level %q previous %q, want %q/%q",
			event.Level,
			event.PreviousLevel,
			wantLevel,
			wantPreviousLevel,
		)
	}
}

func assertQueueUpdate(t *testing.T, ev Event, steering []string, followUp []string) {
	t.Helper()
	if ev.Type != EventQueueUpdate {
		t.Fatalf("event type = %q, want %q", ev.Type, EventQueueUpdate)
	}
	if !reflect.DeepEqual(ev.Steering, steering) || !reflect.DeepEqual(ev.FollowUp, followUp) {
		t.Fatalf(
			"queue update = steering %v followUp %v, want %v/%v",
			ev.Steering,
			ev.FollowUp,
			steering,
			followUp,
		)
	}
}

func newTestOrchestrator(t *testing.T, extensions []PluginSource) (*Orchestrator, *session.JsonlSessionRepo) {
	t.Helper()

	ctx := context.Background()
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
		Plugins:       extensions,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, repo
}

func newTestOrchestratorWithModels(
	t *testing.T,
	modelID string,
	thinkingLevel agentloop.ThinkingLevel,
	models ...ai.Model,
) *Orchestrator {
	t.Helper()

	ctx := context.Background()
	repo := session.NewJsonlSessionRepo(t.TempDir())
	o, err := New(ctx, NewOptions{
		Repo:          repo,
		CreateOptions: session.JsonlSessionCreateOptions{CWD: t.TempDir()},
		ModelID:       modelID,
		Provider:      provider.New(models, staticAuthResolver{}),
		ThinkingLevel: thinkingLevel,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

type recordingHarness struct {
	promptCalls         int
	promptMessages      [][]message.AgentMessage
	promptResult        ai.Message
	promptErr           error
	onPrompt            func(context.Context)
	compactCalls        int
	compactInstructions []string
	compactResult       harness.CompactionResult
	compactErr          error
}

func (h *recordingHarness) Prompt(
	ctx context.Context,
	messages []message.AgentMessage,
	state harness.TurnState,
) (ai.Message, error) {
	h.promptCalls++
	h.promptMessages = append(h.promptMessages, cloneMessages(messages))
	if h.onPrompt != nil {
		h.onPrompt(ctx)
	}
	return h.promptResult, h.promptErr
}

func (h *recordingHarness) Continue(ctx context.Context, state harness.TurnState) (ai.Message, error) {
	return ai.Message{}, nil
}

func (h *recordingHarness) Compact(
	ctx context.Context,
	state harness.TurnState,
	customInstructions string,
) (harness.CompactionResult, error) {
	h.compactCalls++
	h.compactInstructions = append(h.compactInstructions, customInstructions)
	return h.compactResult, h.compactErr
}

func (h *recordingHarness) NavigateTree(
	ctx context.Context,
	target session.EntryID,
	state harness.TurnState,
	opts harness.NavigationOptions,
) (harness.NavigationResult, error) {
	return harness.NavigationResult{}, nil
}

func testModel(id string) ai.Model {
	return ai.Model{
		ID:       id,
		Name:     id,
		Provider: "test-provider",
	}
}

func testProviderRegistry(extra ...ai.Model) *provider.Registry {
	models := ai.BuiltinModels()
	models = append(models, extra...)
	return provider.New(models, staticAuthResolver{})
}

type staticAuthResolver struct{}

func (staticAuthResolver) Resolve(ctx context.Context, providerName string) (auth.Credential, error) {
	if err := ctx.Err(); err != nil {
		return auth.Credential{}, err
	}
	return auth.Credential{
		Type: auth.TypeAPIKey,
		Key:  providerName + "-key",
	}, nil
}

func entriesOfType(o *Orchestrator, entryType string) []session.SessionEntry {
	entries := o.session.GetEntries()
	matches := make([]session.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == entryType {
			matches = append(matches, entry)
		}
	}
	return matches
}
