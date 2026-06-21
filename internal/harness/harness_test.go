package harness

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

// --- Stub session ---

type stubSession struct {
	appendedMessages    []message.AgentMessage
	appendedCompactions []compactionRecord
	appendErr           error
	buildContext        session.Context
	buildContextCalls   int
	leafID              *session.EntryID
	entries             map[session.EntryID]session.SessionEntry
	branch              []session.SessionEntry
	branches            map[session.EntryID][]session.SessionEntry
	branchErr           error
	moveToErr           error
}

type compactionRecord struct {
	summary          string
	firstKeptEntryID session.EntryID
	tokensBefore     int
	details          any
	fromHook         bool
}

func newStubSession() *stubSession {
	return &stubSession{
		entries:  make(map[session.EntryID]session.SessionEntry),
		branches: make(map[session.EntryID][]session.SessionEntry),
	}
}

func (s *stubSession) BuildContext(ctx context.Context) (session.Context, error) {
	s.buildContextCalls++
	if s.buildContext.Messages != nil || s.buildContext.Model != nil || s.buildContext.ThinkingLevel != "" {
		return s.buildContext, nil
	}
	return session.Context{Messages: []message.AgentMessage{}, ThinkingLevel: "off"}, nil
}

func (s *stubSession) GetMetadata() session.Metadata {
	return session.Metadata{ID: "test-session"}
}

func (s *stubSession) GetLeafID() (*session.EntryID, error) {
	return s.leafID, nil
}

func (s *stubSession) GetEntry(id session.EntryID) (session.SessionEntry, bool) {
	e, ok := s.entries[id]
	return e, ok
}

func (s *stubSession) GetEntries() []session.SessionEntry {
	result := make([]session.SessionEntry, 0, len(s.entries))
	for _, e := range s.entries {
		result = append(result, e)
	}
	return result
}

func (s *stubSession) GetBranch(fromID *session.EntryID) ([]session.SessionEntry, error) {
	if s.branchErr != nil {
		return nil, s.branchErr
	}
	if fromID != nil {
		if branch, ok := s.branches[*fromID]; ok {
			return branch, nil
		}
	}
	return s.branch, nil
}

func (s *stubSession) GetLabel(id session.EntryID) (string, bool) {
	return "", false
}

func (s *stubSession) GetSessionName() (string, bool) {
	return "", false
}

func (s *stubSession) AppendMessage(ctx context.Context, msg message.AgentMessage) (session.EntryID, error) {
	if s.appendErr != nil {
		return "", s.appendErr
	}
	id := session.EntryID("msg-" + strconv.Itoa(len(s.appendedMessages)))
	s.appendedMessages = append(s.appendedMessages, msg)
	return id, nil
}

func (s *stubSession) AppendModelChange(ctx context.Context, provider, modelID string) (session.EntryID, error) {
	return "model-change", nil
}

func (s *stubSession) AppendThinkingLevelChange(ctx context.Context, level string) (session.EntryID, error) {
	return "thinking-change", nil
}

func (s *stubSession) AppendCompaction(ctx context.Context, summary string, firstKeptEntryID session.EntryID, tokensBefore int, details any, fromHook bool) (session.EntryID, error) {
	if s.appendErr != nil {
		return "", s.appendErr
	}
	s.appendedCompactions = append(s.appendedCompactions, compactionRecord{
		summary:          summary,
		firstKeptEntryID: firstKeptEntryID,
		tokensBefore:     tokensBefore,
		details:          details,
		fromHook:         fromHook,
	})
	return session.EntryID("compaction-" + strconv.Itoa(len(s.appendedCompactions)-1)), nil
}

func (s *stubSession) AppendCustomEntry(ctx context.Context, customType string, data any) (session.EntryID, error) {
	return "custom", nil
}

func (s *stubSession) AppendCustomMessageEntry(ctx context.Context, customType string, content any, display bool, details any) (session.EntryID, error) {
	return "custom-msg", nil
}

func (s *stubSession) AppendLabel(ctx context.Context, targetID session.EntryID, label string) (session.EntryID, error) {
	return "label", nil
}

func (s *stubSession) AppendSessionName(ctx context.Context, name string) (session.EntryID, error) {
	return "session-name", nil
}

func (s *stubSession) MoveTo(ctx context.Context, entryID *session.EntryID, summary *session.BranchSummary) (*session.EntryID, error) {
	if s.moveToErr != nil {
		return nil, s.moveToErr
	}
	if entryID != nil {
		s.leafID = entryID
	} else {
		s.leafID = nil
	}
	if summary != nil {
		id := session.EntryID("summary-entry")
		s.leafID = &id
		s.entries[id] = session.SessionEntry{
			Type:     "branch_summary",
			ID:       id,
			ParentID: entryID,
			Summary:  summary.Summary,
			Details:  summary.Details,
			FromHook: summary.FromHook,
		}
		return &id, nil
	}
	return nil, nil
}

// --- Test helpers ---

func testModel() ai.Model {
	return ai.Model{
		ID:       "test-model",
		Provider: "test-provider",
	}
}

func testState() TurnState {
	return TurnState{
		Model:         testModel(),
		ThinkingLevel: agentloop.ThinkingOff,
		SystemPrompt:  "test system prompt",
		SessionID:     "test-session-id",
	}
}

func summaryTestState(
	t *testing.T,
	responseText string,
	inspect func(*http.Request, string),
) (TurnState, agentloop.StreamFunc) {
	t.Helper()

	model, ok := ai.GetModel("", "gpt-5")
	if !ok {
		t.Fatal("gpt-5 model not registered")
	}
	state := testState()
	state.Model = model

	stream := func(
		ctx context.Context,
		model ai.Model,
		llmCtx ai.Context,
		opts ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodPost,
				"https://openai.test/responses",
				nil,
			)
			if err != nil {
				yield(nil, err)
				return
			}
			if opts.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+opts.APIKey)
			}
			body := summaryStreamBody(llmCtx)
			if inspect != nil {
				inspect(req, body)
			}

			yield(&ai.StreamEvent{
				Type: ai.EventMessageComplete,
				Message: &ai.Message{
					Role:       ai.RoleAssistant,
					Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: responseText}},
					Provider:   model.Provider,
					Model:      model.ID,
					StopReason: ai.StopReasonStop,
				},
			}, nil)
		})
	}

	return state, stream
}

func summaryStreamBody(llmCtx ai.Context) string {
	var body strings.Builder
	body.WriteString(llmCtx.SystemPrompt)
	for _, msg := range llmCtx.Messages {
		for _, block := range msg.Content {
			switch block.Type {
			case ai.ContentText:
				body.WriteString("\n")
				body.WriteString(block.Text)
			case ai.ContentThinking:
				body.WriteString("\n")
				body.WriteString(block.Thinking)
			case ai.ContentToolCall:
				body.WriteString("\n")
				body.WriteString(block.ToolName)
			}
		}
	}
	return body.String()
}

func completeStream(msg *ai.Message) agentloop.StreamFunc {
	return func(
		context.Context,
		ai.Model,
		ai.Context,
		ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
			yield(&ai.StreamEvent{Type: ai.EventMessageComplete, Message: msg}, nil)
		})
	}
}

func errorStream(err error) agentloop.StreamFunc {
	return func(
		context.Context,
		ai.Model,
		ai.Context,
		ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		return ai.NewAssistantMessageEventStream(func(yield func(*ai.StreamEvent, error) bool) {
			yield(nil, err)
		})
	}
}

func textMessage(role ai.Role, text string) ai.Message {
	return ai.Message{
		Role:    role,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
	}
}

func agentMessageText(t *testing.T, msg message.AgentMessage) string {
	t.Helper()

	aiMsg, ok := message.AsAIMessage(msg)
	if !ok {
		t.Fatalf("message type = %T, want ai.Message", msg)
	}
	if len(aiMsg.Content) == 0 {
		t.Fatal("message content is empty")
	}
	return aiMsg.Content[0].Text
}

// --- Tests ---

func TestPromptBasicFlow(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()

	h := New(HarnessConfig{
		Session:   stub,
		Hooks:     reg,
		Tools:     nil,
		GetAPIKey: nil,
	})

	assistantMsg := &ai.Message{
		Role:     ai.RoleAssistant,
		Provider: "test-provider",
		Model:    "test-model",
		Content:  []ai.ContentBlock{{Type: ai.ContentText, Text: "hello"}},
	}

	// Override stream function for testing by calling Prompt with a custom
	// stream. Since Prompt internally creates its own stream, we need to
	// test through the full flow. For now, test that Prompt doesn't panic
	// and returns properly.
	//
	// Note: This test will fail at the stream level because we don't have
	// a real provider. The key thing we're testing is the event sink and
	// hook integration. We'll test with a mock stream via the agentloop
	// directly in other tests.

	_ = assistantMsg
	_ = h

	// Verify the harness was created properly.
	if h.cfg.Session != stub {
		t.Fatal("session not set")
	}
	if h.cfg.Hooks != reg {
		t.Fatal("hooks not set")
	}
}

func TestContinueBuildsContextAndReturnsLastAssistant(t *testing.T) {
	userMsg := textMessage(ai.RoleUser, "existing prompt")
	stub := newStubSession()
	stub.buildContext = session.Context{
		Messages:      []message.AgentMessage{userMsg},
		ThinkingLevel: "off",
	}

	var gotLLMCtx ai.Context
	assistantMsg := &ai.Message{
		Role:       ai.RoleAssistant,
		Provider:   "test-provider",
		Model:      "test-model",
		StopReason: ai.StopReasonStop,
		Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: "continued"}},
	}
	h := New(HarnessConfig{
		Session: stub,
		Hooks:   hook.NewRegistry(),
		stream: func(
			ctx context.Context,
			model ai.Model,
			llmCtx ai.Context,
			opts ai.SimpleStreamOptions,
		) *ai.AssistantMessageEventStream {
			gotLLMCtx = llmCtx
			return completeStream(assistantMsg)(ctx, model, llmCtx, opts)
		},
	})

	got, err := h.Continue(context.Background(), testState())
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if got.Role != ai.RoleAssistant || agentMessageText(t, got) != "continued" {
		t.Fatalf("Continue returned %#v, want assistant continuation", got)
	}
	if stub.buildContextCalls == 0 {
		t.Fatal("BuildContext was not called")
	}
	if len(stub.appendedMessages) != 1 {
		t.Fatalf("appended messages = %d, want only assistant", len(stub.appendedMessages))
	}
	if gotLLMCtx.Messages == nil || len(gotLLMCtx.Messages) != 1 {
		t.Fatalf("llm context messages = %d, want existing user message", len(gotLLMCtx.Messages))
	}
	if gotLLMCtx.Messages[0].Role != ai.RoleUser || textBlocks(gotLLMCtx.Messages[0].Content) != "existing prompt" {
		t.Fatalf("llm context first message = %#v, want existing user", gotLLMCtx.Messages[0])
	}
}

func TestEventSinkWritesMessageToSession(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()

	h := New(HarnessConfig{
		Session: stub,
		Hooks:   reg,
	})

	emit := h.createEventSink(context.Background(), testState())

	// Simulate message_start then message_end.
	msg := ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "test"}},
	}

	if err := emit(agentloop.Event{Type: agentloop.MessageStart, Message: msg}); err != nil {
		t.Fatalf("MessageStart: %v", err)
	}
	if err := emit(agentloop.Event{Type: agentloop.MessageEnd, Message: msg}); err != nil {
		t.Fatalf("MessageEnd: %v", err)
	}

	if len(stub.appendedMessages) != 1 {
		t.Fatalf("appended messages = %d, want 1", len(stub.appendedMessages))
	}
	if got := agentMessageText(t, stub.appendedMessages[0]); got != "test" {
		t.Fatalf("appended text = %q, want test", got)
	}
	appended, ok := message.AsAIMessage(stub.appendedMessages[0])
	if !ok {
		t.Fatalf("appended type = %T, want ai.Message", stub.appendedMessages[0])
	}
	if len(appended.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", appended.Diagnostics)
	}
}

func TestPromptMessageEndReplacementIsPersistedAndReturned(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	reg.On(hook.EventMessageEnd, func(_ context.Context, event any) (any, error) {
		e := event.(hook.MessageEndEvent)
		msg, ok := e.Message.(message.AgentMessage)
		if !ok {
			t.Fatalf("message type = %T, want AgentMessage", e.Message)
		}
		aiMsg, ok := message.AsAIMessage(msg)
		if !ok || aiMsg.Role != ai.RoleAssistant {
			return nil, nil
		}

		replacement := aiMsg
		replacement.Content = []ai.ContentBlock{{Type: ai.ContentText, Text: "replacement"}}
		replacement.StopReason = ai.StopReasonStop
		replacement.ErrorMessage = ""
		return hook.MessageEndResult{Message: replacement}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	got, err := h.Prompt(
		context.Background(),
		[]message.AgentMessage{textMessage(ai.RoleUser, "prompt")},
		testState(),
	)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(got.Content) == 0 || got.Content[0].Text != "replacement" {
		t.Fatalf("Prompt returned text = %#v, want replacement", got.Content)
	}
	if len(stub.appendedMessages) != 2 {
		t.Fatalf("appended messages = %d, want user prompt and assistant", len(stub.appendedMessages))
	}
	if got := agentMessageText(t, stub.appendedMessages[1]); got != "replacement" {
		t.Fatalf("appended assistant text = %q, want replacement", got)
	}
}

func TestMessageEndDropsDifferentRoleReplacementAndRecordsDiagnostic(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	reg.On(hook.EventMessageEnd, func(context.Context, any) (any, error) {
		return hook.MessageEndResult{
			Message: textMessage(ai.RoleUser, "wrong role"),
		}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())

	msg := textMessage(ai.RoleAssistant, "original")
	if err := emit(agentloop.Event{Type: agentloop.MessageEnd, Message: msg}); err != nil {
		t.Fatalf("MessageEnd: %v", err)
	}

	if len(stub.appendedMessages) != 1 {
		t.Fatalf("appended messages = %d, want 1", len(stub.appendedMessages))
	}
	appended, ok := message.AsAIMessage(stub.appendedMessages[0])
	if !ok {
		t.Fatalf("appended type = %T, want ai.Message", stub.appendedMessages[0])
	}
	if appended.Role != ai.RoleAssistant {
		t.Fatalf("role = %q, want assistant", appended.Role)
	}
	if got := agentMessageText(t, stub.appendedMessages[0]); got != "original" {
		t.Fatalf("appended text = %q, want original", got)
	}
	if len(appended.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one role diagnostic", appended.Diagnostics)
	}
	if !strings.Contains(appended.Diagnostics[0].Message, "same role") {
		t.Fatalf("diagnostic = %#v, want same-role message", appended.Diagnostics[0])
	}
}

func TestMessageEndHandlersAreChained(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	reg.On(hook.EventMessageEnd, func(_ context.Context, event any) (any, error) {
		e := event.(hook.MessageEndEvent)
		msg := e.Message.(message.AgentMessage)
		if got := agentMessageText(t, msg); got != "original" {
			t.Fatalf("first handler saw %q, want original", got)
		}
		return hook.MessageEndResult{
			Message: textMessage(ai.RoleAssistant, "first"),
		}, nil
	})
	reg.On(hook.EventMessageEnd, func(_ context.Context, event any) (any, error) {
		e := event.(hook.MessageEndEvent)
		msg := e.Message.(message.AgentMessage)
		if got := agentMessageText(t, msg); got != "first" {
			t.Fatalf("second handler saw %q, want first", got)
		}
		return hook.MessageEndResult{
			Message: textMessage(ai.RoleAssistant, "second"),
		}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())

	if err := emit(agentloop.Event{
		Type:    agentloop.MessageEnd,
		Message: textMessage(ai.RoleAssistant, "original"),
	}); err != nil {
		t.Fatalf("MessageEnd: %v", err)
	}
	if got := agentMessageText(t, stub.appendedMessages[0]); got != "second" {
		t.Fatalf("appended text = %q, want second", got)
	}
}

func TestMessageEndHandlerErrorContinuesChain(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	hookErr := errors.New("handler failed")
	reg.On(hook.EventMessageEnd, func(context.Context, any) (any, error) {
		return nil, hookErr
	})
	var laterCalled bool
	reg.On(hook.EventMessageEnd, func(context.Context, any) (any, error) {
		laterCalled = true
		return hook.MessageEndResult{
			Message: textMessage(ai.RoleAssistant, "later"),
		}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())

	err := emit(agentloop.Event{
		Type:    agentloop.MessageEnd,
		Message: textMessage(ai.RoleAssistant, "original"),
	})
	if err != nil {
		t.Fatalf("MessageEnd error = %v, want nil", err)
	}
	if !laterCalled {
		t.Fatal("later handler was not called")
	}
	appended, ok := message.AsAIMessage(stub.appendedMessages[0])
	if !ok {
		t.Fatalf("appended type = %T, want ai.Message", stub.appendedMessages[0])
	}
	if got := agentMessageText(t, stub.appendedMessages[0]); got != "later" {
		t.Fatalf("appended text = %q, want later", got)
	}
	if len(appended.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one error diagnostic", appended.Diagnostics)
	}
	if !strings.Contains(appended.Diagnostics[0].Message, hookErr.Error()) {
		t.Fatalf("diagnostic = %#v, want handler error", appended.Diagnostics[0])
	}
}

func TestAgentEndSeesMessageEndReplacement(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	original := textMessage(ai.RoleAssistant, "original")
	original.ResponseID = "resp_1"
	replacement := original
	replacement.Content = []ai.ContentBlock{{Type: ai.ContentText, Text: "replacement"}}

	reg.On(hook.EventMessageEnd, func(context.Context, any) (any, error) {
		return hook.MessageEndResult{Message: replacement}, nil
	})

	var agentEndMessages []any
	reg.On(hook.EventAgentEnd, func(_ context.Context, event any) (any, error) {
		e := event.(hook.AgentEndEvent)
		agentEndMessages = append([]any(nil), e.Messages...)
		return nil, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())

	if err := emit(agentloop.Event{Type: agentloop.MessageEnd, Message: original}); err != nil {
		t.Fatalf("MessageEnd: %v", err)
	}
	if err := emit(agentloop.Event{
		Type:     agentloop.AgentEnd,
		Messages: []message.AgentMessage{original},
	}); err != nil {
		t.Fatalf("AgentEnd: %v", err)
	}

	if len(agentEndMessages) != 1 {
		t.Fatalf("agent_end messages = %d, want 1", len(agentEndMessages))
	}
	msg, ok := agentEndMessages[0].(message.AgentMessage)
	if !ok {
		t.Fatalf("agent_end message type = %T, want AgentMessage", agentEndMessages[0])
	}
	if got := agentMessageText(t, msg); got != "replacement" {
		t.Fatalf("agent_end text = %q, want replacement", got)
	}
}

func TestEventSinkForwardsAllEventTypes(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()

	var receivedEvents []string
	reg.On(hook.EventAgentStart, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "agent_start")
		return nil, nil
	})
	reg.On(hook.EventAgentEnd, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "agent_end")
		return nil, nil
	})
	reg.On(hook.EventTurnStart, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "turn_start")
		return nil, nil
	})
	reg.On(hook.EventTurnEnd, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "turn_end")
		return nil, nil
	})
	reg.On(hook.EventMessageStart, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "message_start")
		return nil, nil
	})
	reg.On(hook.EventMessageEnd, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "message_end")
		return nil, nil
	})
	reg.On(hook.EventMessageUpdate, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "message_update")
		return nil, nil
	})
	reg.On(hook.EventToolExecutionStart, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "tool_execution_start")
		return nil, nil
	})
	reg.On(hook.EventToolExecutionUpdate, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "tool_execution_update")
		return nil, nil
	})
	reg.On(hook.EventToolExecutionEnd, func(ctx context.Context, event any) (any, error) {
		receivedEvents = append(receivedEvents, "tool_execution_end")
		return nil, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())

	msg := ai.Message{Role: ai.RoleAssistant}

	events := []agentloop.Event{
		{Type: agentloop.AgentStart},
		{Type: agentloop.TurnStart},
		{Type: agentloop.MessageStart, Message: msg},
		{Type: agentloop.MessageUpdate, Message: msg},
		{Type: agentloop.MessageEnd, Message: msg},
		{Type: agentloop.ToolExecutionStart, ToolCallID: "tc1", ToolName: "test"},
		{Type: agentloop.ToolExecutionUpdate, ToolCallID: "tc1", ToolName: "test"},
		{Type: agentloop.ToolExecutionEnd, ToolCallID: "tc1", ToolName: "test"},
		{Type: agentloop.TurnEnd, Message: msg},
		{Type: agentloop.AgentEnd, Messages: []message.AgentMessage{msg}},
	}

	for _, e := range events {
		if err := emit(e); err != nil {
			t.Fatalf("emit %s: %v", e.Type, err)
		}
	}

	want := []string{
		"agent_start",
		"turn_start",
		"message_start",
		"message_update",
		"message_end",
		"tool_execution_start",
		"tool_execution_update",
		"tool_execution_end",
		"turn_end",
		"agent_end",
	}
	if len(receivedEvents) != len(want) {
		t.Fatalf("received events = %d, want %d: %v", len(receivedEvents), len(want), receivedEvents)
	}
	for i, w := range want {
		if receivedEvents[i] != w {
			t.Fatalf("event[%d] = %q, want %q", i, receivedEvents[i], w)
		}
	}
}

func TestEventSinkTurnEventsIncludeIndexAndTimestamp(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()

	var starts []hook.TurnStartEvent
	var ends []hook.TurnEndEvent
	reg.On(hook.EventTurnStart, func(ctx context.Context, event any) (any, error) {
		start, ok := event.(hook.TurnStartEvent)
		if !ok {
			t.Fatalf("turn_start event = %T, want TurnStartEvent", event)
		}
		starts = append(starts, start)
		return nil, nil
	})
	reg.On(hook.EventTurnEnd, func(ctx context.Context, event any) (any, error) {
		end, ok := event.(hook.TurnEndEvent)
		if !ok {
			t.Fatalf("turn_end event = %T, want TurnEndEvent", event)
		}
		ends = append(ends, end)
		return nil, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())
	msg := ai.Message{Role: ai.RoleAssistant}

	events := []agentloop.Event{
		{Type: agentloop.AgentStart},
		{Type: agentloop.TurnStart},
		{Type: agentloop.TurnEnd, Message: msg},
		{Type: agentloop.TurnStart},
		{Type: agentloop.TurnEnd, Message: msg},
	}
	for _, e := range events {
		if err := emit(e); err != nil {
			t.Fatalf("emit %s: %v", e.Type, err)
		}
	}

	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("turn events starts=%d ends=%d, want 2 each", len(starts), len(ends))
	}
	for i := range starts {
		if starts[i].TurnIndex != i {
			t.Fatalf("turn_start[%d].TurnIndex = %d, want %d", i, starts[i].TurnIndex, i)
		}
		if starts[i].Timestamp == 0 {
			t.Fatalf("turn_start[%d].Timestamp = 0, want epoch millis", i)
		}
		if ends[i].TurnIndex != starts[i].TurnIndex {
			t.Fatalf(
				"turn_end[%d].TurnIndex = %d, want matching turn_start index %d",
				i,
				ends[i].TurnIndex,
				starts[i].TurnIndex,
			)
		}
	}
}

func TestEventSinkSessionWriteError(t *testing.T) {
	stub := newStubSession()
	stub.appendErr = errors.New("write failed")
	reg := hook.NewRegistry()

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())

	msg := ai.Message{Role: ai.RoleAssistant}
	err := emit(agentloop.Event{Type: agentloop.MessageEnd, Message: msg})
	if err == nil {
		t.Fatal("expected error from session write")
	}
	if err.Error() != "write failed" {
		t.Fatalf("error = %q, want %q", err.Error(), "write failed")
	}
}

func TestEventSinkReportsHookErrorWithoutFailing(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	hookErr := errors.New("hook failed")
	var reported []error
	reg.OnHandlerError = func(_ string, err error) {
		reported = append(reported, err)
	}
	reg.On(hook.EventMessageStart, func(context.Context, any) (any, error) {
		return nil, hookErr
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	emit := h.createEventSink(context.Background(), testState())

	// pi reports handler errors as recoverable extension errors; they never
	// abort the agent turn (runner.ts:698-707).
	err := emit(agentloop.Event{Type: agentloop.MessageStart, Message: ai.Message{Role: ai.RoleAssistant}})
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], hookErr) {
		t.Fatalf("reported = %v, want hook error", reported)
	}
}

func TestCompactBasicFlow(t *testing.T) {
	stub := newStubSession()
	entryID := session.EntryID("entry-1")
	olderID := session.EntryID("older")
	stub.branch = []session.SessionEntry{
		{
			Type: "message",
			ID:   olderID,
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "older context"}},
			},
		},
		{
			Type: "message",
			ID:   entryID,
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: strings.Repeat("recent ", 12000)}},
			},
		},
		{
			Type: "message",
			ID:   "entry-2",
			Message: ai.Message{
				Role:     ai.RoleAssistant,
				Content:  []ai.ContentBlock{{Type: ai.ContentText, Text: "world"}},
				Provider: "test",
				Model:    "model",
			},
		},
	}

	reg := hook.NewRegistry()
	state, stream := summaryTestState(t, "real compact summary", func(r *http.Request, body string) {
		if got := r.Header.Get("Authorization"); got != "Bearer summary-key" {
			t.Fatalf("Authorization = %q, want bearer summary-key", got)
		}
		if !strings.Contains(body, "older context") ||
			!strings.Contains(body, "Additional focus: summarize this") {
			t.Fatalf("summary prompt body = %s", body)
		}
	})
	h := New(HarnessConfig{
		Session: stub,
		Hooks:   reg,
		stream:  stream,
		GetProviderAuth: func(context.Context, ai.Model) (ProviderAuth, error) {
			return ProviderAuth{APIKey: "summary-key"}, nil
		},
	})

	result, err := h.Compact(context.Background(), state, "summarize this")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if len(stub.appendedCompactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(stub.appendedCompactions))
	}

	comp := stub.appendedCompactions[0]
	if comp.firstKeptEntryID != entryID {
		t.Fatalf("firstKeptEntryID = %q, want %q", comp.firstKeptEntryID, entryID)
	}
	if comp.fromHook {
		t.Fatal("fromHook = true, want false")
	}
	if result.Summary != "real compact summary" {
		t.Fatalf("summary = %q, want real compact summary", result.Summary)
	}
}

func TestCompactHookCancelReturnsError(t *testing.T) {
	stub := newStubSession()
	stub.branch = []session.SessionEntry{
		{
			Type:    "message",
			ID:      "entry-1",
			Message: ai.Message{Role: ai.RoleUser, Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hi"}}},
		},
	}

	reg := hook.NewRegistry()
	reg.On(hook.EventSessionBeforeCompact, func(ctx context.Context, event any) (any, error) {
		return hook.SessionBeforeCompactResult{Cancel: true}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})

	result, err := h.Compact(context.Background(), testState(), "")
	if err == nil {
		t.Fatal("Compact error = nil, want cancellation error")
	}

	if result.Summary != "" {
		t.Fatalf("summary = %q, want empty (cancelled)", result.Summary)
	}
	if len(stub.appendedCompactions) != 0 {
		t.Fatalf("compactions = %d, want 0 (cancelled)", len(stub.appendedCompactions))
	}
}

func TestCompactHookOverride(t *testing.T) {
	stub := newStubSession()
	entryID := session.EntryID("entry-1")
	stub.branch = []session.SessionEntry{
		{
			Type:    "message",
			ID:      entryID,
			Message: ai.Message{Role: ai.RoleUser, Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hi"}}},
		},
	}

	reg := hook.NewRegistry()
	reg.On(hook.EventSessionBeforeCompact, func(ctx context.Context, event any) (any, error) {
		return hook.SessionBeforeCompactResult{
			Compaction: &hook.CompactionResult{
				Summary:          "hook summary",
				FirstKeptEntryID: string(entryID),
				TokensBefore:     42,
				Details:          map[string]any{"source": "hook"},
			},
		}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})

	result, err := h.Compact(context.Background(), testState(), "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if !result.FromHook {
		t.Fatal("FromHook = false, want true")
	}
	if result.Summary != "hook summary" {
		t.Fatalf("summary = %q, want %q", result.Summary, "hook summary")
	}
	if result.TokensBefore != 42 {
		t.Fatalf("TokensBefore = %d, want 42", result.TokensBefore)
	}
	details, ok := result.Details.(map[string]any)
	if !ok || details["source"] != "hook" {
		t.Fatalf("Details = %#v, want hook details", result.Details)
	}
}

func TestNavigateTreeNoOp(t *testing.T) {
	stub := newStubSession()
	targetID := session.EntryID("target")
	stub.leafID = &targetID

	reg := hook.NewRegistry()
	h := New(HarnessConfig{Session: stub, Hooks: reg})

	result, err := h.NavigateTree(context.Background(), targetID, testState(), NavigationOptions{})
	if err != nil {
		t.Fatalf("NavigateTree: %v", err)
	}
	if result.Cancelled {
		t.Fatal("Cancelled = true, want false (no-op)")
	}
}

func TestNavigateTreeUserMessageParentId(t *testing.T) {
	parentID := session.EntryID("parent")
	targetID := session.EntryID("target")
	stub := newStubSession()
	stub.leafID = ptrEntryID("old-leaf")
	stub.entries[targetID] = session.SessionEntry{
		Type:     "message",
		ID:       targetID,
		ParentID: &parentID,
		Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "user input"}},
		},
	}

	reg := hook.NewRegistry()
	h := New(HarnessConfig{Session: stub, Hooks: reg})

	result, err := h.NavigateTree(context.Background(), targetID, testState(), NavigationOptions{})
	if err != nil {
		t.Fatalf("NavigateTree: %v", err)
	}

	// For user message, new leaf should be parentId.
	if stub.leafID == nil || *stub.leafID != parentID {
		t.Fatalf("leafID = %v, want %v", stub.leafID, parentID)
	}
	if result.EditorText != "user input" {
		t.Fatalf("EditorText = %q, want %q", result.EditorText, "user input")
	}
}

func TestNavigateTreeAssistantMessageUsesTargetId(t *testing.T) {
	targetID := session.EntryID("target")
	stub := newStubSession()
	stub.leafID = ptrEntryID("old-leaf")
	stub.entries[targetID] = session.SessionEntry{
		Type: "message",
		ID:   targetID,
		Message: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "assistant reply"}},
		},
	}

	reg := hook.NewRegistry()
	h := New(HarnessConfig{Session: stub, Hooks: reg})

	result, err := h.NavigateTree(context.Background(), targetID, testState(), NavigationOptions{})
	if err != nil {
		t.Fatalf("NavigateTree: %v", err)
	}

	// For assistant message, new leaf should be targetID itself.
	if stub.leafID == nil || *stub.leafID != targetID {
		t.Fatalf("leafID = %v, want %v", stub.leafID, targetID)
	}
	if result.EditorText != "" {
		t.Fatalf("EditorText = %q, want empty", result.EditorText)
	}
}

func TestNavigateTreeHookCancel(t *testing.T) {
	targetID := session.EntryID("target")
	stub := newStubSession()
	stub.leafID = ptrEntryID("old-leaf")
	stub.entries[targetID] = session.SessionEntry{
		Type: "message",
		ID:   targetID,
		Message: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "reply"}},
		},
	}

	reg := hook.NewRegistry()
	reg.On(hook.EventSessionBeforeTree, func(ctx context.Context, event any) (any, error) {
		return hook.SessionBeforeTreeResult{Cancel: true}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})

	result, err := h.NavigateTree(context.Background(), targetID, testState(), NavigationOptions{})
	if err != nil {
		t.Fatalf("NavigateTree: %v", err)
	}
	if !result.Cancelled {
		t.Fatal("Cancelled = false, want true")
	}
	// Leaf should not have changed.
	if stub.leafID == nil || *stub.leafID != "old-leaf" {
		t.Fatalf("leafID = %v, want old-leaf", stub.leafID)
	}
}

func TestNavigateTreeCustomMessageUsesParentIdAndEditorText(t *testing.T) {
	parentID := session.EntryID("parent")
	targetID := session.EntryID("target")
	stub := newStubSession()
	stub.leafID = ptrEntryID("old-leaf")
	stub.entries[targetID] = session.SessionEntry{
		Type:       "custom_message",
		ID:         targetID,
		ParentID:   &parentID,
		CustomType: "note",
		Content:    "custom content",
	}

	reg := hook.NewRegistry()
	h := New(HarnessConfig{Session: stub, Hooks: reg})

	result, err := h.NavigateTree(context.Background(), targetID, testState(), NavigationOptions{})
	if err != nil {
		t.Fatalf("NavigateTree: %v", err)
	}

	if stub.leafID == nil || *stub.leafID != parentID {
		t.Fatalf("leafID = %v, want %v", stub.leafID, parentID)
	}
	if result.Cancelled {
		t.Fatal("Cancelled = true, want false")
	}
	if result.EditorText != "custom content" {
		t.Fatalf("EditorText = %q, want custom content", result.EditorText)
	}
}

func TestNavigateTreePreparationAndSummaryLeafFollowPi(t *testing.T) {
	rootID := session.EntryID("root")
	oldMidID := session.EntryID("old-mid")
	oldLeafID := session.EntryID("old-leaf")
	targetID := session.EntryID("target")

	root := session.SessionEntry{
		Type: "message",
		ID:   rootID,
		Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "root"}},
		},
	}
	oldMid := session.SessionEntry{
		Type:     "message",
		ID:       oldMidID,
		ParentID: &rootID,
		Message: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "old mid"}},
		},
	}
	oldLeaf := session.SessionEntry{
		Type:     "message",
		ID:       oldLeafID,
		ParentID: &oldMidID,
		Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "old leaf"}},
		},
	}
	target := session.SessionEntry{
		Type:     "message",
		ID:       targetID,
		ParentID: &rootID,
		Message: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "target"}},
		},
	}

	stub := newStubSession()
	stub.leafID = &oldLeafID
	stub.entries[rootID] = root
	stub.entries[oldMidID] = oldMid
	stub.entries[oldLeafID] = oldLeaf
	stub.entries[targetID] = target
	stub.branches[oldLeafID] = []session.SessionEntry{root, oldMid, oldLeaf}
	stub.branches[targetID] = []session.SessionEntry{root, target}

	reg := hook.NewRegistry()
	var prep hook.TreePreparation
	reg.On(hook.EventSessionBeforeTree, func(_ context.Context, event any) (any, error) {
		e := event.(hook.SessionBeforeTreeEvent)
		prep = e.Preparation
		return hook.SessionBeforeTreeResult{
			Summary: &hook.BranchSummaryResult{
				Summary: "hook summary",
				Details: map[string]any{"from": "hook"},
			},
		}, nil
	})

	var tree hook.SessionTreeEvent
	reg.On(hook.EventSessionTree, func(_ context.Context, event any) (any, error) {
		tree = event.(hook.SessionTreeEvent)
		return nil, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	result, err := h.NavigateTree(
		context.Background(),
		targetID,
		testState(),
		NavigationOptions{Summarize: true, CustomInstructions: "focus", Label: "branch"},
	)
	if err != nil {
		t.Fatalf("NavigateTree: %v", err)
	}

	if prep.TargetID != string(targetID) {
		t.Fatalf("TargetID = %q, want %q", prep.TargetID, targetID)
	}
	if prep.OldLeafID == nil || *prep.OldLeafID != string(oldLeafID) {
		t.Fatalf("OldLeafID = %v, want %q", prep.OldLeafID, oldLeafID)
	}
	if prep.CommonAncestorID == nil || *prep.CommonAncestorID != string(rootID) {
		t.Fatalf("CommonAncestorID = %v, want %q", prep.CommonAncestorID, rootID)
	}
	if len(prep.EntriesToSummarize) != 2 {
		t.Fatalf("EntriesToSummarize len = %d, want 2", len(prep.EntriesToSummarize))
	}
	if !prep.UserWantsSummary {
		t.Fatal("UserWantsSummary = false, want true")
	}
	if prep.CustomInstructions == nil || *prep.CustomInstructions != "focus" {
		t.Fatalf("CustomInstructions = %v, want focus", prep.CustomInstructions)
	}
	if prep.Label == nil || *prep.Label != "branch" {
		t.Fatalf("Label = %v, want branch", prep.Label)
	}

	if result.SummaryEntry == nil || result.SummaryEntry.Summary != "hook summary" {
		t.Fatalf("SummaryEntry = %#v, want hook summary", result.SummaryEntry)
	}
	if tree.NewLeafId == nil || *tree.NewLeafId != "summary-entry" {
		t.Fatalf("session_tree NewLeafId = %v, want summary-entry", tree.NewLeafId)
	}
	if tree.OldLeafId == nil || *tree.OldLeafId != string(oldLeafID) {
		t.Fatalf("session_tree OldLeafId = %v, want %q", tree.OldLeafId, oldLeafID)
	}
	if !tree.FromHook {
		t.Fatal("session_tree FromHook = false, want true")
	}
}

func TestNavigateTreeSummarizeCallsLLMAndStoresDetails(t *testing.T) {
	rootID := session.EntryID("root")
	oldMidID := session.EntryID("old-mid")
	oldLeafID := session.EntryID("old-leaf")
	targetID := session.EntryID("target")

	root := session.SessionEntry{
		Type: "message",
		ID:   rootID,
		Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "root"}},
		},
	}
	oldMid := session.SessionEntry{
		Type:     "message",
		ID:       oldMidID,
		ParentID: &rootID,
		Message: ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.ContentBlock{
				{Type: ai.ContentText, Text: "old branch work"},
				{
					Type:      ai.ContentToolCall,
					ToolName:  "read",
					Arguments: map[string]any{"path": "readme.md"},
				},
				{
					Type:      ai.ContentToolCall,
					ToolName:  "edit",
					Arguments: map[string]any{"path": "main.go"},
				},
			},
		},
	}
	oldLeaf := session.SessionEntry{
		Type:     "message",
		ID:       oldLeafID,
		ParentID: &oldMidID,
		Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "old leaf"}},
		},
	}
	target := session.SessionEntry{
		Type:     "message",
		ID:       targetID,
		ParentID: &rootID,
		Message: ai.Message{
			Role:    ai.RoleAssistant,
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "target"}},
		},
	}

	stub := newStubSession()
	stub.leafID = &oldLeafID
	stub.entries[rootID] = root
	stub.entries[oldMidID] = oldMid
	stub.entries[oldLeafID] = oldLeaf
	stub.entries[targetID] = target
	stub.branches[oldLeafID] = []session.SessionEntry{root, oldMid, oldLeaf}
	stub.branches[targetID] = []session.SessionEntry{root, target}

	state, stream := summaryTestState(t, "branch llm summary", func(r *http.Request, body string) {
		if got := r.Header.Get("Authorization"); got != "Bearer branch-key" {
			t.Fatalf("Authorization = %q, want bearer branch-key", got)
		}
		if !strings.Contains(body, "[Assistant]: old branch work") ||
			!strings.Contains(body, "readme.md") ||
			!strings.Contains(body, "Additional focus: branch focus") {
			t.Fatalf("branch summary prompt body = %s", body)
		}
	})

	h := New(HarnessConfig{
		Session: stub,
		Hooks:   hook.NewRegistry(),
		stream:  stream,
		GetProviderAuth: func(context.Context, ai.Model) (ProviderAuth, error) {
			return ProviderAuth{APIKey: "branch-key"}, nil
		},
	})
	result, err := h.NavigateTree(
		context.Background(),
		targetID,
		state,
		NavigationOptions{Summarize: true, CustomInstructions: "branch focus"},
	)
	if err != nil {
		t.Fatalf("NavigateTree: %v", err)
	}
	if result.SummaryEntry == nil {
		t.Fatal("SummaryEntry = nil, want generated summary")
	}
	summary := result.SummaryEntry.Summary
	if !strings.Contains(summary, "Summary of that exploration") ||
		!strings.Contains(summary, "branch llm summary") ||
		!strings.Contains(summary, "<read-files>\nreadme.md\n</read-files>") ||
		!strings.Contains(summary, "<modified-files>\nmain.go\n</modified-files>") {
		t.Fatalf("summary = %q, want generated branch summary with file tags", summary)
	}
	details, ok := result.SummaryEntry.Details.(map[string]any)
	if !ok {
		t.Fatalf("Details = %#v, want map", result.SummaryEntry.Details)
	}
	readFiles, ok := details["readFiles"].([]string)
	if !ok || len(readFiles) != 1 || readFiles[0] != "readme.md" {
		t.Fatalf("readFiles = %#v, want readme.md", details["readFiles"])
	}
	modifiedFiles, ok := details["modifiedFiles"].([]string)
	if !ok || len(modifiedFiles) != 1 || modifiedFiles[0] != "main.go" {
		t.Fatalf("modifiedFiles = %#v, want main.go", details["modifiedFiles"])
	}
}

func TestCreateLoopConfigBridgeCallbacks(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()

	var transformCalled bool
	reg.On(hook.EventContext, func(ctx context.Context, event any) (any, error) {
		transformCalled = true
		return hook.ContextResult{Messages: []any{ai.Message{Role: ai.RoleUser, Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "transformed"}}}}}, nil
	})

	var toolCallCalled bool
	reg.On(hook.EventToolCall, func(ctx context.Context, event any) (any, error) {
		toolCallCalled = true
		return hook.ToolCallResult{Block: true, Reason: "blocked"}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	config := h.createLoopConfig(context.Background(), testState())

	// Test TransformContext.
	if config.TransformContext == nil {
		t.Fatal("TransformContext is nil")
	}
	msgs, err := config.TransformContext(context.Background(), []message.AgentMessage{ai.Message{Role: ai.RoleUser}})
	if err != nil {
		t.Fatalf("TransformContext: %v", err)
	}
	if !transformCalled {
		t.Fatal("TransformContext hook not called")
	}
	if len(msgs) != 1 {
		t.Fatalf("TransformContext returned %d messages, want 1", len(msgs))
	}

	// Test BeforeToolCall.
	if config.BeforeToolCall == nil {
		t.Fatal("BeforeToolCall is nil")
	}
	result, err := config.BeforeToolCall(context.Background(), agentloop.BeforeToolCallContext{
		ToolCall: ai.ContentBlock{ToolCallID: "tc1", ToolName: "test"},
		Args:     map[string]any{},
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if !toolCallCalled {
		t.Fatal("BeforeToolCall hook not called")
	}
	if result == nil || !result.Block {
		t.Fatal("BeforeToolCall should have returned Block=true")
	}
}

func TestBeforeToolCallShortCircuitsOnFirstBlock(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	args := map[string]any{"path": "/tmp/delete"}
	var firstCalled bool
	var secondCalled bool

	reg.On(hook.EventToolCall, func(_ context.Context, event any) (any, error) {
		firstCalled = true
		e, ok := event.(hook.ToolCallEvent)
		if !ok {
			t.Fatalf("event type = %T, want ToolCallEvent", event)
		}
		if e.ToolCallId != "tc1" || e.ToolName != "delete" {
			t.Fatalf("event tool = %s/%s, want tc1/delete", e.ToolCallId, e.ToolName)
		}
		gotArgs, ok := e.Input.(map[string]any)
		if !ok || gotArgs["path"] != "/tmp/delete" {
			t.Fatalf("event input = %#v, want original args", e.Input)
		}
		return hook.ToolCallResult{
			Block:  true,
			Reason: "dangerous",
			Input:  map[string]any{"path": "/tmp/other"},
		}, nil
	})
	reg.On(hook.EventToolCall, func(_ context.Context, _ any) (any, error) {
		secondCalled = true
		return hook.ToolCallResult{Reason: "later"}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	config := h.createLoopConfig(context.Background(), testState())

	result, err := config.BeforeToolCall(context.Background(), agentloop.BeforeToolCallContext{
		ToolCall: ai.ContentBlock{ToolCallID: "tc1", ToolName: "delete"},
		Args:     args,
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if !firstCalled {
		t.Fatal("first handler was not called")
	}
	if secondCalled {
		t.Fatal("second handler should not be called after blocking result")
	}
	if result == nil || !result.Block || result.Reason != "dangerous" {
		t.Fatalf("result = %#v, want blocking dangerous result", result)
	}
	if result.Args != nil {
		t.Fatalf("result.Args = %#v, want nil on block", result.Args)
	}
}

func TestBeforeToolCallChainsInputPatches(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	originalArgs := map[string]any{"path": "original"}
	firstPatch := map[string]any{"path": "first"}
	secondPatch := map[string]any{"path": "second", "mode": "safe"}
	var calls []string

	reg.On(hook.EventToolCall, func(_ context.Context, event any) (any, error) {
		calls = append(calls, "first")
		e, ok := event.(hook.ToolCallEvent)
		if !ok {
			t.Fatalf("event type = %T, want ToolCallEvent", event)
		}
		gotArgs, ok := e.Input.(map[string]any)
		if !ok || gotArgs["path"] != "original" {
			t.Fatalf("first input = %#v, want original args", e.Input)
		}
		return hook.ToolCallResult{Reason: "first", Input: firstPatch}, nil
	})
	reg.On(hook.EventToolCall, func(_ context.Context, event any) (any, error) {
		calls = append(calls, "second")
		e, ok := event.(hook.ToolCallEvent)
		if !ok {
			t.Fatalf("event type = %T, want ToolCallEvent", event)
		}
		gotArgs, ok := e.Input.(map[string]any)
		if !ok || gotArgs["path"] != "first" {
			t.Fatalf("second input = %#v, want first patch", e.Input)
		}
		return hook.ToolCallResult{Reason: "second", Input: secondPatch}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	config := h.createLoopConfig(context.Background(), testState())

	result, err := config.BeforeToolCall(context.Background(), agentloop.BeforeToolCallContext{
		ToolCall: ai.ContentBlock{ToolCallID: "tc1", ToolName: "edit"},
		Args:     originalArgs,
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if result == nil {
		t.Fatal("result = nil, want chained patch result")
	}
	if result.Block {
		t.Fatal("Block = true, want false")
	}
	if result.Reason != "second" {
		t.Fatalf("Reason = %q, want second", result.Reason)
	}
	if result.Args["path"] != "second" || result.Args["mode"] != "safe" {
		t.Fatalf("Args = %#v, want second patch", result.Args)
	}
	if strings.Join(calls, ",") != "first,second" {
		t.Fatalf("calls = %v, want both handlers", calls)
	}
}

func TestBeforeToolCallErrorStopsChain(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	testErr := errors.New("boom")
	var secondCalled bool

	reg.On(hook.EventToolCall, func(_ context.Context, _ any) (any, error) {
		return nil, testErr
	})
	reg.On(hook.EventToolCall, func(_ context.Context, _ any) (any, error) {
		secondCalled = true
		return nil, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	config := h.createLoopConfig(context.Background(), testState())

	result, err := config.BeforeToolCall(context.Background(), agentloop.BeforeToolCallContext{
		ToolCall: ai.ContentBlock{ToolCallID: "tc1", ToolName: "test"},
		Args:     map[string]any{},
	})
	if !errors.Is(err, testErr) {
		t.Fatalf("BeforeToolCall error = %v, want %v", err, testErr)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil on handler error", result)
	}
	if secondCalled {
		t.Fatal("second handler should not be called after error")
	}
}

func TestBeforeToolCallNoBlockReturnsLastNonNilResult(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	var calls []string

	reg.On(hook.EventToolCall, func(_ context.Context, _ any) (any, error) {
		calls = append(calls, "first")
		return hook.ToolCallResult{Reason: "first"}, nil
	})
	reg.On(hook.EventToolCall, func(_ context.Context, _ any) (any, error) {
		calls = append(calls, "second")
		return nil, nil
	})
	reg.On(hook.EventToolCall, func(_ context.Context, _ any) (any, error) {
		calls = append(calls, "third")
		return hook.ToolCallResult{Reason: "third"}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	config := h.createLoopConfig(context.Background(), testState())

	result, err := config.BeforeToolCall(context.Background(), agentloop.BeforeToolCallContext{
		ToolCall: ai.ContentBlock{ToolCallID: "tc1", ToolName: "test"},
		Args:     map[string]any{},
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if result == nil {
		t.Fatal("result = nil, want last non-nil result")
	}
	if result.Block {
		t.Fatal("Block = true, want false")
	}
	if result.Reason != "third" {
		t.Fatalf("Reason = %q, want third", result.Reason)
	}
	if result.Args != nil {
		t.Fatalf("Args = %#v, want nil without patch", result.Args)
	}
	if strings.Join(calls, ",") != "first,second,third" {
		t.Fatalf("calls = %v, want all handlers", calls)
	}
}

func TestCreateLoopConfigAfterToolCallAccumulatesPatches(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	content := []ai.ContentBlock{
		{Type: ai.ContentText, Text: "patched"},
		{Type: ai.ContentImage, MimeType: "image/png", ImageData: "abc"},
	}
	reg.On(hook.EventToolResult, func(_ context.Context, event any) (any, error) {
		e := event.(hook.ToolResultEvent)
		if len(e.Content) != 2 || e.Content[1].Type != ai.ContentImage {
			t.Fatalf("event content = %#v, want original text and image blocks", e.Content)
		}
		return hook.ToolResultPatch{Content: content}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	config := h.createLoopConfig(context.Background(), testState())

	result, err := config.AfterToolCall(context.Background(), agentloop.AfterToolCallContext{
		ToolCall: ai.ContentBlock{ToolCallID: "tc1", ToolName: "test"},
		Result: tool.Result{
			Content: []ai.ContentBlock{
				{Type: ai.ContentText, Text: "original"},
				{Type: ai.ContentImage, MimeType: "image/jpeg", ImageData: "raw"},
			},
			Terminate: true,
		},
		IsError: true,
	})
	if err != nil {
		t.Fatalf("AfterToolCall: %v", err)
	}
	if result == nil {
		t.Fatal("AfterToolCall result = nil, want patch")
	}
	// pi returns the accumulated event's isError when any field was patched —
	// the untouched original value, never a flip (runner.ts:795-803).
	if result.IsError == nil || *result.IsError != true {
		t.Fatalf("IsError = %v, want original true carried through", result.IsError)
	}
	if result.Terminate != nil {
		t.Fatalf("Terminate = %v, want nil when hook omitted field", *result.Terminate)
	}
	if len(result.Content) != 2 || result.Content[0].Text != "patched" || result.Content[1].Type != ai.ContentImage {
		t.Fatalf("Content = %v, want patched text and image", result.Content)
	}
}

func TestBeforeAgentStartIncludesPromptImagesAndResources(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()
	resources := map[string]any{"skills": []string{"s1"}}
	injected := ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "injected"}},
	}

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
		if e.SystemPrompt != "system" {
			t.Fatalf("SystemPrompt = %q, want system", e.SystemPrompt)
		}
		if e.Resources == nil {
			t.Fatal("Resources = nil, want provided resources")
		}
		if e.SystemPromptOptions == nil {
			t.Fatal("SystemPromptOptions = nil, want carried options")
		}
		if e.SystemPromptOptions.CWD != "/repo" {
			t.Fatalf("SystemPromptOptions.CWD = %q, want /repo", e.SystemPromptOptions.CWD)
		}
		if len(e.SystemPromptOptions.SelectedTools) != 1 || e.SystemPromptOptions.SelectedTools[0] != "read" {
			t.Fatalf("SystemPromptOptions.SelectedTools = %#v, want read", e.SystemPromptOptions.SelectedTools)
		}
		systemPrompt := "override"
		return hook.BeforeAgentStartResult{
			Messages:     []any{injected},
			SystemPrompt: &systemPrompt,
		}, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	got, err := h.emitBeforeAgentStart(
		context.Background(),
		[]message.AgentMessage{ai.Message{
			Role: ai.RoleUser,
			Content: []ai.ContentBlock{
				{Type: ai.ContentText, Text: "hello"},
				{Type: ai.ContentImage, MimeType: "image/png", ImageData: "abc"},
			},
		}},
		TurnState{
			SystemPrompt: "system",
			SystemPromptOptions: hook.SystemPromptOptions{
				SelectedTools: []string{"read"},
				CWD:           "/repo",
			},
			Resources: resources,
		},
	)
	if err != nil {
		t.Fatalf("emitBeforeAgentStart: %v", err)
	}
	if got.systemPrompt == nil || *got.systemPrompt != "override" {
		t.Fatalf("systemPrompt = %v, want override", got.systemPrompt)
	}
	if len(got.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(got.messages))
	}
}

func TestBeforeAgentStartChainsSystemPromptAndMessages(t *testing.T) {
	reg := hook.NewRegistry()
	firstMessage := ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "first message"}},
	}
	secondMessage := ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "second message"}},
	}
	var secondReceived string

	reg.On(hook.EventBeforeAgentStart, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeAgentStartEvent)
		if e.SystemPrompt != "base" {
			t.Fatalf("first SystemPrompt = %q, want base", e.SystemPrompt)
		}
		systemPrompt := "first prompt"
		return hook.BeforeAgentStartResult{
			Messages:     []any{firstMessage},
			SystemPrompt: &systemPrompt,
		}, nil
	})
	reg.On(hook.EventBeforeAgentStart, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeAgentStartEvent)
		secondReceived = e.SystemPrompt
		systemPrompt := "second prompt"
		return hook.BeforeAgentStartResult{
			Messages:     []any{secondMessage},
			SystemPrompt: &systemPrompt,
		}, nil
	})

	h := New(HarnessConfig{Session: newStubSession(), Hooks: reg})
	got, err := h.emitBeforeAgentStart(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		TurnState{SystemPrompt: "base"},
	)
	if err != nil {
		t.Fatalf("emitBeforeAgentStart: %v", err)
	}
	if secondReceived != "first prompt" {
		t.Fatalf("second handler SystemPrompt = %q, want first prompt", secondReceived)
	}
	if got.systemPrompt == nil || *got.systemPrompt != "second prompt" {
		t.Fatalf("systemPrompt = %v, want second prompt", got.systemPrompt)
	}
	if len(got.messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(got.messages))
	}
	first, ok := message.AsAIMessage(got.messages[0])
	if !ok || first.Content[0].Text != "first message" {
		t.Fatalf("first message = %#v, want first message", got.messages[0])
	}
	second, ok := message.AsAIMessage(got.messages[1])
	if !ok || second.Content[0].Text != "second message" {
		t.Fatalf("second message = %#v, want second message", got.messages[1])
	}
}

func TestBeforeAgentStartSkipsHandlerError(t *testing.T) {
	reg := hook.NewRegistry()
	hookErr := errors.New("boom")
	injected := ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "injected"}},
	}

	reg.On(hook.EventBeforeAgentStart, func(_ context.Context, _ any) (any, error) {
		return nil, hookErr
	})
	reg.On(hook.EventBeforeAgentStart, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeAgentStartEvent)
		if e.SystemPrompt != "base" {
			t.Fatalf("SystemPrompt after skipped handler = %q, want base", e.SystemPrompt)
		}
		systemPrompt := "recovered"
		return hook.BeforeAgentStartResult{
			Messages:     []any{injected},
			SystemPrompt: &systemPrompt,
		}, nil
	})

	h := New(HarnessConfig{Session: newStubSession(), Hooks: reg})
	got, err := h.emitBeforeAgentStart(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		TurnState{SystemPrompt: "base"},
	)
	if err != nil {
		t.Fatalf("emitBeforeAgentStart: %v", err)
	}
	if got.systemPrompt == nil || *got.systemPrompt != "recovered" {
		t.Fatalf("systemPrompt = %v, want recovered", got.systemPrompt)
	}
	if len(got.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(got.messages))
	}
}

func TestBeforeAgentStartNoHandlersReturnsNoChanges(t *testing.T) {
	h := New(HarnessConfig{Session: newStubSession(), Hooks: hook.NewRegistry()})
	got, err := h.emitBeforeAgentStart(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		TurnState{SystemPrompt: "base"},
	)
	if err != nil {
		t.Fatalf("emitBeforeAgentStart: %v", err)
	}
	if got.systemPrompt != nil {
		t.Fatalf("systemPrompt = %v, want nil", got.systemPrompt)
	}
	if len(got.messages) != 0 {
		t.Fatalf("messages len = %d, want 0", len(got.messages))
	}
}

func TestBeforeAgentStartAllowsEmptySystemPromptOverride(t *testing.T) {
	reg := hook.NewRegistry()
	empty := ""
	reg.On(hook.EventBeforeAgentStart, func(context.Context, any) (any, error) {
		return hook.BeforeAgentStartResult{SystemPrompt: &empty}, nil
	})

	h := New(HarnessConfig{Session: newStubSession(), Hooks: reg})
	got, err := h.emitBeforeAgentStart(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		TurnState{SystemPrompt: "base"},
	)
	if err != nil {
		t.Fatalf("emitBeforeAgentStart: %v", err)
	}
	if got.systemPrompt == nil {
		t.Fatal("systemPrompt = nil, want empty override")
	}
	if *got.systemPrompt != "" {
		t.Fatalf("systemPrompt = %q, want empty", *got.systemPrompt)
	}
}

func TestBeforeProviderRequestHandlersSeeAccumulatedOptions(t *testing.T) {
	reg := hook.NewRegistry()
	one := "1"
	two := "2"

	reg.On(hook.EventBeforeProviderRequest, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeProviderRequestEvent)
		opts := e.StreamOptions.(ai.SimpleStreamOptions)
		if opts.Headers["base"] != "yes" {
			t.Fatalf("base header = %q, want yes", opts.Headers["base"])
		}
		opts.Headers["base"] = "mutated"
		return hook.BeforeProviderRequestResult{
			StreamOptions: hook.StreamOptionsPatch{
				Headers:  map[string]*string{"a": &one, "delete": nil},
				Metadata: map[string]any{"trace": "one"},
			},
		}, nil
	})
	reg.On(hook.EventBeforeProviderRequest, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeProviderRequestEvent)
		opts := e.StreamOptions.(ai.SimpleStreamOptions)
		if opts.Headers["base"] != "yes" {
			t.Fatalf("base header = %q, want unchanged yes", opts.Headers["base"])
		}
		if opts.Headers["a"] != "1" {
			t.Fatalf("a header = %q, want 1", opts.Headers["a"])
		}
		if _, ok := opts.Headers["delete"]; ok {
			t.Fatal("delete header still present")
		}
		if opts.Metadata["trace"] != "one" {
			t.Fatalf("trace metadata = %v, want one", opts.Metadata["trace"])
		}
		return hook.BeforeProviderRequestResult{
			StreamOptions: hook.StreamOptionsPatch{
				ClearHeaders: true,
				Headers:      map[string]*string{"b": &two},
				Metadata:     map[string]any{"trace": nil, "next": "two"},
			},
		}, nil
	})

	h := New(HarnessConfig{Session: newStubSession(), Hooks: reg})
	got, err := h.emitBeforeProviderRequest(
		context.Background(),
		testModel(),
		"session-1",
		ai.SimpleStreamOptions{
			Headers:  map[string]string{"base": "yes", "delete": "old"},
			Metadata: map[string]any{"old": "keep"},
		},
	)
	if err != nil {
		t.Fatalf("emitBeforeProviderRequest: %v", err)
	}
	if len(got.Headers) != 1 || got.Headers["b"] != "2" {
		t.Fatalf("Headers = %#v, want only b=2", got.Headers)
	}
	if got.Metadata["old"] != "keep" || got.Metadata["next"] != "two" {
		t.Fatalf("Metadata = %#v, want old keep and next two", got.Metadata)
	}
	if _, ok := got.Metadata["trace"]; ok {
		t.Fatal("trace metadata still present")
	}
}

func TestProviderAuthHeadersMergeBeforeProviderRequestHook(t *testing.T) {
	reg := hook.NewRegistry()
	hookErr := errors.New("stop before provider")
	reg.On(hook.EventBeforeProviderRequest, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeProviderRequestEvent)
		opts := e.StreamOptions.(ai.SimpleStreamOptions)
		if opts.APIKey != "auth-key" {
			t.Fatalf("APIKey = %q, want auth-key", opts.APIKey)
		}
		if opts.Headers["base"] != "yes" || opts.Headers["auth"] != "header" {
			t.Fatalf("Headers = %#v, want base and auth", opts.Headers)
		}
		return nil, hookErr
	})

	h := New(HarnessConfig{
		Session: newStubSession(),
		Hooks:   reg,
		GetProviderAuth: func(context.Context, ai.Model) (ProviderAuth, error) {
			return ProviderAuth{
				APIKey:  "auth-key",
				Headers: map[string]string{"auth": "header"},
			}, nil
		},
	})

	stream := h.createStreamFunc(context.Background(), testState())(
		context.Background(),
		testModel(),
		ai.Context{},
		ai.SimpleStreamOptions{Headers: map[string]string{"base": "yes"}},
	)
	var gotErr error
	for _, err := range stream.Iter() {
		gotErr = err
		break
	}
	if !errors.Is(gotErr, hookErr) {
		t.Fatalf("stream error = %v, want hook error", gotErr)
	}
}

func TestBeforeProviderPayloadHandlersSeeAccumulatedPayload(t *testing.T) {
	reg := hook.NewRegistry()
	reg.On(hook.EventBeforeProviderPayload, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeProviderPayloadEvent)
		if e.Payload != "a" {
			t.Fatalf("first payload = %v, want a", e.Payload)
		}
		return hook.BeforeProviderPayloadResult{Payload: "ab"}, nil
	})
	reg.On(hook.EventBeforeProviderPayload, func(_ context.Context, event any) (any, error) {
		e := event.(hook.BeforeProviderPayloadEvent)
		if e.Payload != "ab" {
			t.Fatalf("second payload = %v, want ab", e.Payload)
		}
		return hook.BeforeProviderPayloadResult{Payload: "abc"}, nil
	})

	h := New(HarnessConfig{Session: newStubSession(), Hooks: reg})
	got, changed, err := h.emitBeforeProviderPayload(context.Background(), testModel(), "a")
	if err != nil {
		t.Fatalf("emitBeforeProviderPayload: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if got != "abc" {
		t.Fatalf("payload = %v, want abc", got)
	}
}

func TestEmitRunFailure(t *testing.T) {
	stub := newStubSession()
	reg := hook.NewRegistry()

	var messageStartCount, messageEndCount, turnEndCount, agentEndCount int
	reg.On(hook.EventMessageStart, func(ctx context.Context, event any) (any, error) {
		messageStartCount++
		return nil, nil
	})
	reg.On(hook.EventMessageEnd, func(ctx context.Context, event any) (any, error) {
		messageEndCount++
		return nil, nil
	})
	reg.On(hook.EventTurnEnd, func(ctx context.Context, event any) (any, error) {
		turnEndCount++
		return nil, nil
	})
	reg.On(hook.EventAgentEnd, func(ctx context.Context, event any) (any, error) {
		agentEndCount++
		return nil, nil
	})

	h := New(HarnessConfig{Session: stub, Hooks: reg})
	got, err := h.emitRunFailure(context.Background(), testState(), errors.New("test error"))
	if err != nil {
		t.Fatalf("emitRunFailure: %v", err)
	}
	if got.StopReason != ai.StopReasonError || got.ErrorMessage != "test error" {
		t.Fatalf("failure message = %#v, want error stop reason", got)
	}

	if messageStartCount != 1 {
		t.Fatalf("message_start = %d, want 1", messageStartCount)
	}
	if messageEndCount != 1 {
		t.Fatalf("message_end = %d, want 1", messageEndCount)
	}
	if turnEndCount != 1 {
		t.Fatalf("turn_end = %d, want 1", turnEndCount)
	}
	if agentEndCount != 1 {
		t.Fatalf("agent_end = %d, want 1", agentEndCount)
	}

	// Verify failure message was written to session.
	if len(stub.appendedMessages) != 1 {
		t.Fatalf("appended messages = %d, want 1", len(stub.appendedMessages))
	}
	failureMsg, ok := message.AsAIMessage(stub.appendedMessages[0])
	if !ok {
		t.Fatal("failure message is not ai.Message")
	}
	if failureMsg.StopReason != ai.StopReasonError {
		t.Fatalf("stop reason = %q, want %q", failureMsg.StopReason, ai.StopReasonError)
	}
	if failureMsg.ErrorMessage != "test error" {
		t.Fatalf("error message = %q, want %q", failureMsg.ErrorMessage, "test error")
	}
}

func TestPromptReturnsFailureMessageWhenRunFails(t *testing.T) {
	stub := newStubSession()
	h := New(HarnessConfig{
		Session: stub,
		Hooks:   hook.NewRegistry(),
	})

	got, err := h.Prompt(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		testState(),
	)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got.StopReason != ai.StopReasonError {
		t.Fatalf("stop reason = %q, want error", got.StopReason)
	}
	if !strings.Contains(got.ErrorMessage, "unsupported API") {
		t.Fatalf("error message = %q, want unsupported API", got.ErrorMessage)
	}
	if len(stub.appendedMessages) != 2 {
		t.Fatalf("appended messages = %d, want user prompt and failure", len(stub.appendedMessages))
	}
	failureMsg, ok := message.AsAIMessage(stub.appendedMessages[1])
	if !ok {
		t.Fatalf("appended failure type = %T, want ai.Message", stub.appendedMessages[1])
	}
	if failureMsg.StopReason != ai.StopReasonError {
		t.Fatalf("appended stop reason = %q, want error", failureMsg.StopReason)
	}
}

func TestPromptKeepsStreamErrorWhenFailureReportingHookFails(t *testing.T) {
	stub := newStubSession()
	reportErr := errors.New("failure hook failed")
	reg := hook.NewRegistry()
	var messageStarts int
	reg.On(hook.EventMessageStart, func(context.Context, any) (any, error) {
		messageStarts++
		if messageStarts == 2 {
			return nil, reportErr
		}
		return nil, nil
	})

	h := New(HarnessConfig{
		Session: stub,
		Hooks:   reg,
	})

	result, err := h.Prompt(
		context.Background(),
		[]message.AgentMessage{ai.Message{Role: ai.RoleUser}},
		testState(),
	)
	// Hook handler errors are reported, not propagated (pi runner.ts:698-707);
	// the run failure itself surfaces as the failure message's stop reason,
	// exactly like pi's createFailureMessage flow.
	if err != nil {
		t.Fatalf("Prompt error = %v, want nil", err)
	}
	if errors.Is(err, reportErr) {
		t.Fatalf("Prompt error = %v, hook handler error must not propagate", err)
	}
	if result.StopReason != ai.StopReasonError ||
		!strings.Contains(result.ErrorMessage, "unsupported API") {
		t.Fatalf("failure message = %+v, want error stop reason with stream error", result)
	}
}

func TestPrepareCompaction(t *testing.T) {
	entry1 := session.EntryID("e1")
	entry2 := session.EntryID("e2")

	branch := []session.SessionEntry{
		{
			Type: "message",
			ID:   entry1,
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hello"}},
			},
		},
		{
			Type: "message",
			ID:   entry2,
			Message: ai.Message{
				Role:    ai.RoleAssistant,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "world"}},
			},
		},
	}

	prep, ok := prepareCompaction(branch, hook.CompactionSettings{})
	if !ok {
		t.Fatal("prepareCompaction returned false")
	}

	if prep.firstKeptEntryID != entry1 {
		t.Fatalf("firstKeptEntryID = %q, want %q", prep.firstKeptEntryID, entry1)
	}
	if prep.tokensBefore <= 0 {
		t.Fatalf("tokensBefore = %d, want > 0", prep.tokensBefore)
	}
}

func TestPrepareCompactionSelectsPiStyleRecentCutPoint(t *testing.T) {
	entry1 := session.EntryID("e1")
	entry2 := session.EntryID("e2")
	entry3 := session.EntryID("e3")
	large := strings.Repeat("x", 90_000)

	branch := []session.SessionEntry{
		{
			Type: "message",
			ID:   entry1,
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "older request"}},
			},
		},
		{
			Type: "message",
			ID:   entry2,
			Message: ai.Message{
				Role:    ai.RoleAssistant,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: large}},
			},
		},
		{
			Type: "message",
			ID:   entry3,
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "recent"}},
			},
		},
	}

	prep, ok := prepareCompaction(branch, hook.CompactionSettings{})
	if !ok {
		t.Fatal("prepareCompaction returned false")
	}
	if prep.firstKeptEntryID != entry2 {
		t.Fatalf("firstKeptEntryID = %q, want %q", prep.firstKeptEntryID, entry2)
	}
	if !prep.isSplitTurn {
		t.Fatal("isSplitTurn = false, want true")
	}
	if len(prep.turnPrefixMessages) != 1 {
		t.Fatalf("turnPrefixMessages len = %d, want 1", len(prep.turnPrefixMessages))
	}
}

func TestFindValidCutPointsIncludesPiCustomMessageRoles(t *testing.T) {
	entries := []session.SessionEntry{
		{
			Type: "message",
			ID:   "bash",
			Message: message.BashExecutionMessage{
				Command: "go test ./...",
				Output:  "ok",
			},
		},
		{
			Type: "message",
			ID:   "tool-result",
			Message: ai.Message{
				Role: ai.RoleToolResult,
			},
		},
		{
			Type: "message",
			ID:   "custom",
			Message: message.CustomMessage{
				CustomType: "note",
				Content:    "remember this",
			},
		},
	}

	got := findValidCutPoints(entries, 0, len(entries))
	want := []int{0, 2}
	if len(got) != len(want) {
		t.Fatalf("cut points = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cut points = %#v, want %#v", got, want)
		}
	}
}

func TestEstimateContextTokensUsesLastAssistantUsagePlusTrailing(t *testing.T) {
	got := estimateContextTokens([]session.SessionEntry{
		{
			Type: "message",
			ID:   "older",
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: strings.Repeat("x", 1000)}},
			},
		},
		{
			Type: "message",
			ID:   "usage",
			Message: ai.Message{
				Role: ai.RoleAssistant,
				Usage: &ai.Usage{
					TotalTokens: 42,
				},
			},
		},
		{
			Type: "message",
			ID:   "tail",
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "tail"}},
			},
		},
	})

	if got != 43 {
		t.Fatalf("tokens = %d, want 43", got)
	}
}

func ptrEntryID(id string) *session.EntryID {
	eid := session.EntryID(id)
	return &eid
}

func TestEstimateAIMessageTokensIgnoresUsage(t *testing.T) {
	usage := &ai.Usage{TotalTokens: 50000, Input: 49000, Output: 1000}
	msg := ai.Message{
		Role:  ai.RoleAssistant,
		Usage: usage,
		Content: []ai.ContentBlock{
			{Type: ai.ContentText, Text: strings.Repeat("a", 400)},
			{Type: ai.ContentThinking, Thinking: strings.Repeat("b", 40)},
		},
	}

	// pi's estimateTokens is a pure chars/4 heuristic and never reads usage
	// (compaction.ts:202-260); Usage.TotalTokens is the whole context size.
	if got := estimateAIMessageTokens(msg); got != 110 {
		t.Fatalf("estimateAIMessageTokens = %d, want 110 (chars/4, usage ignored)", got)
	}

	user := ai.Message{
		Role: ai.RoleUser,
		Content: []ai.ContentBlock{
			{Type: ai.ContentText, Text: strings.Repeat("c", 100)},
			{Type: ai.ContentImage, ImageData: "zzzz", MimeType: "image/png"},
		},
	}
	if got := estimateAIMessageTokens(user); got != 25 {
		t.Fatalf("user estimate = %d, want 25 (images not counted for user role)", got)
	}

	toolResult := ai.Message{
		Role: ai.RoleToolResult,
		Content: []ai.ContentBlock{
			{Type: ai.ContentText, Text: strings.Repeat("d", 100)},
			{Type: ai.ContentImage, ImageData: "zzzz", MimeType: "image/png"},
		},
	}
	if got := estimateAIMessageTokens(toolResult); got != 1225 {
		t.Fatalf("toolResult estimate = %d, want 1225 (text/4 + 4800/4 image)", got)
	}
}
