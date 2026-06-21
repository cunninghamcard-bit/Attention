package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestPromptRecoversContextOverflowAfterRun(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:        s,
		promptResult:   overflowAssistant("initial-model", millis("2026-01-01T00:00:01.000Z")),
		continueResult: stopAssistant("initial-model", "recovered", millis("2026-01-01T00:00:02.000Z")),
	}
	o := newOverflowOrchestrator(s, h)
	events := subscribeEventsOfType(o, EventCompactionStart, EventCompactionEnd)

	result, err := o.Prompt(ctx, PromptInput{Text: "trigger overflow"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if textOfMessage(t, result.Message) != "recovered" {
		t.Fatalf("result text = %q, want recovered", textOfMessage(t, result.Message))
	}
	if h.compactCalls != 1 || h.continueCalls != 1 {
		t.Fatalf("compact/continue calls = %d/%d, want 1/1", h.compactCalls, h.continueCalls)
	}
	if len(h.compactInstructions) != 1 || h.compactInstructions[0] != overflowCompactionReason {
		t.Fatalf("compact instructions = %v, want overflow reason", h.compactInstructions)
	}
	if len(s.moveToTargets) != 1 {
		t.Fatalf("MoveTo calls = %d, want 1", len(s.moveToTargets))
	}
	target := s.moveToTargets[0]
	if target == nil {
		t.Fatal("MoveTo target = nil, want user prompt parent")
	}
	entry, ok := s.GetEntry(*target)
	if !ok {
		t.Fatalf("MoveTo target %q not found", *target)
	}
	msg, ok := message.AsAIMessage(entry.Message)
	if !ok || msg.Role != ai.RoleUser {
		t.Fatalf("MoveTo target entry = %#v, want user message", entry)
	}
	if o.overflowRecoveryWasAttempted() {
		t.Fatal("overflowRecoveryAttempted = true, want cleared after successful continue")
	}
	if len(*events) != 2 {
		t.Fatalf("overflow compaction events = %d, want 2", len(*events))
	}
	if (*events)[0].Type != EventCompactionStart || (*events)[0].Reason != overflowCompactionReason {
		t.Fatalf("overflow compaction start = %#v", (*events)[0])
	}
	end := (*events)[1]
	if end.Type != EventCompactionEnd ||
		end.Reason != overflowCompactionReason ||
		end.Aborted ||
		!end.WillRetry ||
		end.ErrorMessage != "" {
		t.Fatalf("overflow compaction end = %#v, want successful retrying end", end)
	}
	if _, ok := end.Result.(harness.CompactionResult); !ok {
		t.Fatalf("overflow compaction result type = %T, want harness.CompactionResult", end.Result)
	}
}

func TestPromptPrePromptOverflowRecoveryContinuesThenPrompts(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	userID := s.addEntry(userEntry("previous-user", nil, "previous", "2026-01-01T00:00:00.000Z"))
	assistantID := s.addEntry(assistantEntry(
		"previous-overflow",
		&userID,
		overflowAssistant("initial-model", millis("2026-01-01T00:00:01.000Z")),
		"2026-01-01T00:00:01.000Z",
	))
	s.leafID = &assistantID

	h := &overflowHarness{
		session:        s,
		continueResult: stopAssistant("initial-model", "previous recovered", millis("2026-01-01T00:00:02.000Z")),
		promptResult:   stopAssistant("initial-model", "new answer", millis("2026-01-01T00:00:03.000Z")),
	}
	o := newOverflowOrchestrator(s, h)
	o.setOverflowRecoveryAttempted(true)

	result, err := o.Prompt(ctx, PromptInput{Text: "new prompt"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h.continueCalls != 1 || h.promptCalls != 1 {
		t.Fatalf("continue/prompt calls = %d/%d, want 1/1", h.continueCalls, h.promptCalls)
	}
	if textOfMessage(t, result.Message) != "new answer" {
		t.Fatalf("result text = %q, want new answer", textOfMessage(t, result.Message))
	}
	if len(s.moveToTargets) != 1 || s.moveToTargets[0] == nil || *s.moveToTargets[0] != userID {
		t.Fatalf("MoveTo targets = %v, want previous user parent", s.moveToTargets)
	}
}

func TestPromptDoesNotRecoverNonOverflowError(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session: s,
		promptResult: ai.Message{
			Role:         ai.RoleAssistant,
			Provider:     "test-provider",
			Model:        "initial-model",
			StopReason:   ai.StopReasonError,
			ErrorMessage: "invalid API key",
			Timestamp:    millis("2026-01-01T00:00:01.000Z"),
		},
	}
	o := newOverflowOrchestrator(s, h)

	result, err := o.Prompt(ctx, PromptInput{Text: "non overflow"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Message.ErrorMessage != "invalid API key" {
		t.Fatalf("error message = %q, want invalid API key", result.Message.ErrorMessage)
	}
	if h.compactCalls != 0 || h.continueCalls != 0 || len(s.moveToTargets) != 0 {
		t.Fatalf(
			"recovery calls = compact %d continue %d moveTo %d, want none",
			h.compactCalls,
			h.continueCalls,
			len(s.moveToTargets),
		)
	}
}

func TestPromptSkipsOverflowRecoveryWhenAutoCompactionDisabled(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: overflowAssistant("initial-model", millis("2026-01-01T00:00:01.000Z")),
	}
	o := newOverflowOrchestrator(s, h)

	o.SetAutoCompaction(false)
	result, err := o.Prompt(ctx, PromptInput{Text: "overflow without compaction"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Message.ErrorMessage != "prompt is too long" {
		t.Fatalf("result error = %q, want original overflow message", result.Message.ErrorMessage)
	}
	if h.compactCalls != 0 || h.continueCalls != 0 || len(s.moveToTargets) != 0 {
		t.Fatalf(
			"recovery calls = compact %d continue %d moveTo %d, want none",
			h.compactCalls,
			h.continueCalls,
			len(s.moveToTargets),
		)
	}
	if o.Snapshot().AutoCompactionEnabled {
		t.Fatal("Snapshot AutoCompactionEnabled = true, want false")
	}

	o.SetAutoCompaction(true)
	if !o.Snapshot().AutoCompactionEnabled {
		t.Fatal("Snapshot AutoCompactionEnabled = false after SetAutoCompaction(true)")
	}
}

func TestPromptSkipsOverflowRecoveryWhenModelChanged(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: overflowAssistant("initial-model", millis("2026-01-01T00:00:01.000Z")),
	}
	o := newOverflowOrchestrator(s, h)
	h.onPrompt = func() {
		if err := o.SetModel(ctx, testModel("changed-model")); err != nil {
			t.Fatalf("SetModel: %v", err)
		}
	}

	result, err := o.Prompt(ctx, PromptInput{Text: "model changes during run"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Message.ErrorMessage == "" {
		t.Fatal("result error message is empty, want overflow assistant")
	}
	if h.compactCalls != 0 || h.continueCalls != 0 || len(s.moveToTargets) != 0 {
		t.Fatalf(
			"recovery calls = compact %d continue %d moveTo %d, want none",
			h.compactCalls,
			h.continueCalls,
			len(s.moveToTargets),
		)
	}
}

func TestPromptSkipsOverflowRecoveryWhenAlreadyAttemptedThisTurn(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	h := &overflowHarness{
		session:      s,
		promptResult: overflowAssistant("initial-model", millis("2026-01-01T00:00:01.000Z")),
	}
	o := newOverflowOrchestrator(s, h)
	h.onPrompt = func() {
		o.setOverflowRecoveryAttempted(true)
	}

	result, err := o.Prompt(ctx, PromptInput{Text: "already attempted"})
	if !errors.Is(err, errContextOverflowRecovery) {
		t.Fatalf("Prompt error = %v, want context overflow recovery error", err)
	}
	if result.Message.ErrorMessage == "" {
		t.Fatal("result error message is empty, want overflow assistant")
	}
	if h.compactCalls != 0 || h.continueCalls != 0 || len(s.moveToTargets) != 0 {
		t.Fatalf(
			"recovery calls = compact %d continue %d moveTo %d, want none",
			h.compactCalls,
			h.continueCalls,
			len(s.moveToTargets),
		)
	}
}

func TestPromptSkipsStaleOverflowBeforeLatestCompaction(t *testing.T) {
	ctx := context.Background()
	s := newOverflowSession()
	userID := s.addEntry(userEntry("user", nil, "old prompt", "2026-01-01T00:00:00.000Z"))
	assistantID := s.addEntry(assistantEntry(
		"overflow",
		&userID,
		overflowAssistant("initial-model", millis("2026-01-01T00:00:01.000Z")),
		"2026-01-01T00:00:01.000Z",
	))
	compactionID := s.addEntry(session.SessionEntry{
		Type:             "compaction",
		ID:               "compaction",
		ParentID:         &assistantID,
		Timestamp:        "2026-01-01T00:00:02.000Z",
		Summary:          "already compacted",
		FirstKeptEntryID: userID,
		TokensBefore:     100,
	})
	s.leafID = &compactionID

	h := &overflowHarness{
		session:      s,
		promptResult: stopAssistant("initial-model", "new answer", millis("2026-01-01T00:00:03.000Z")),
	}
	o := newOverflowOrchestrator(s, h)

	result, err := o.Prompt(ctx, PromptInput{Text: "after compaction"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if textOfMessage(t, result.Message) != "new answer" {
		t.Fatalf("result text = %q, want new answer", textOfMessage(t, result.Message))
	}
	if h.compactCalls != 0 || h.continueCalls != 0 || len(s.moveToTargets) != 0 {
		t.Fatalf(
			"recovery calls = compact %d continue %d moveTo %d, want none",
			h.compactCalls,
			h.continueCalls,
			len(s.moveToTargets),
		)
	}
}

type overflowHarness struct {
	session             *overflowSession
	promptResult        ai.Message
	promptErr           error
	continueResult      ai.Message
	continueResults     []ai.Message
	continueErr         error
	continueErrs        []error
	compactErr          error
	onPrompt            func()
	promptCalls         int
	continueCalls       int
	compactCalls        int
	compactInstructions []string
}

func (h *overflowHarness) Prompt(
	ctx context.Context,
	messages []message.AgentMessage,
	state harness.TurnState,
) (ai.Message, error) {
	h.promptCalls++
	for _, msg := range messages {
		if _, err := h.session.AppendMessage(ctx, msg); err != nil {
			return ai.Message{}, err
		}
	}
	if h.promptResult.Role != "" {
		if _, err := h.session.AppendMessage(ctx, h.promptResult); err != nil {
			return ai.Message{}, err
		}
	}
	if h.onPrompt != nil {
		h.onPrompt()
	}
	return h.promptResult, h.promptErr
}

func (h *overflowHarness) Continue(ctx context.Context, state harness.TurnState) (ai.Message, error) {
	call := h.continueCalls
	h.continueCalls++
	result := h.continueResult
	if call < len(h.continueResults) {
		result = h.continueResults[call]
	}
	err := h.continueErr
	if call < len(h.continueErrs) {
		err = h.continueErrs[call]
	}
	if result.Role != "" {
		if _, appendErr := h.session.AppendMessage(ctx, result); appendErr != nil {
			return ai.Message{}, appendErr
		}
	}
	return result, err
}

func (h *overflowHarness) Compact(
	ctx context.Context,
	state harness.TurnState,
	customInstructions string,
) (harness.CompactionResult, error) {
	h.compactCalls++
	h.compactInstructions = append(h.compactInstructions, customInstructions)
	if h.compactErr != nil {
		return harness.CompactionResult{}, h.compactErr
	}
	return harness.CompactionResult{
		Summary:          "compacted",
		FirstKeptEntryID: "first-kept",
		TokensBefore:     100,
	}, nil
}

func (h *overflowHarness) NavigateTree(
	context.Context,
	session.EntryID,
	harness.TurnState,
	harness.NavigationOptions,
) (harness.NavigationResult, error) {
	return harness.NavigationResult{}, nil
}

type overflowSession struct {
	metadata          session.Metadata
	entries           []session.SessionEntry
	leafID            *session.EntryID
	moveToTargets     []*session.EntryID
	modelChangeWrites []string
}

func newOverflowSession() *overflowSession {
	return &overflowSession{
		metadata: session.Metadata{ID: "overflow-session"},
		entries:  []session.SessionEntry{},
	}
}

func (s *overflowSession) BuildContext(ctx context.Context) (session.Context, error) {
	branch, err := s.GetBranch(nil)
	if err != nil {
		return session.Context{}, err
	}
	out := session.Context{Messages: []message.AgentMessage{}, ThinkingLevel: "off"}
	for _, entry := range branch {
		if entry.Type != "message" {
			continue
		}
		out.Messages = append(out.Messages, message.Snapshot(entry.Message))
	}
	return out, nil
}

func (s *overflowSession) GetMetadata() session.Metadata {
	return s.metadata
}

func (s *overflowSession) GetLeafID() (*session.EntryID, error) {
	return copyEntryIDPtr(s.leafID), nil
}

func (s *overflowSession) GetEntry(id session.EntryID) (session.SessionEntry, bool) {
	for _, entry := range s.entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return session.SessionEntry{}, false
}

func (s *overflowSession) GetEntries() []session.SessionEntry {
	return append([]session.SessionEntry(nil), s.entries...)
}

func (s *overflowSession) GetBranch(fromID *session.EntryID) ([]session.SessionEntry, error) {
	leafID := copyEntryIDPtr(fromID)
	if leafID == nil {
		leafID = copyEntryIDPtr(s.leafID)
	}
	if leafID == nil {
		return []session.SessionEntry{}, nil
	}

	byID := map[session.EntryID]session.SessionEntry{}
	for _, entry := range s.entries {
		byID[entry.ID] = entry
	}
	path := []session.SessionEntry{}
	for current := leafID; current != nil; {
		entry, ok := byID[*current]
		if !ok {
			return nil, fmt.Errorf("entry %q not found", *current)
		}
		path = append(path, entry)
		current = entry.ParentID
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

func (s *overflowSession) GetLabel(id session.EntryID) (string, bool) {
	return "", false
}

func (s *overflowSession) GetSessionName() (string, bool) {
	return "", false
}

func (s *overflowSession) AppendMessage(ctx context.Context, msg message.AgentMessage) (session.EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id := session.EntryID(fmt.Sprintf("message-%d", len(s.entries)+1))
	entry := session.SessionEntry{
		Type:      "message",
		ID:        id,
		ParentID:  copyEntryIDPtr(s.leafID),
		Timestamp: entryTimestampForMessage(msg),
		Message:   message.Snapshot(msg),
	}
	s.entries = append(s.entries, entry)
	s.leafID = &id
	return id, nil
}

func (s *overflowSession) AppendModelChange(ctx context.Context, provider, modelID string) (session.EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id := session.EntryID(fmt.Sprintf("model-change-%d", len(s.modelChangeWrites)+1))
	s.modelChangeWrites = append(s.modelChangeWrites, provider+"/"+modelID)
	s.entries = append(s.entries, session.SessionEntry{
		Type:      "model_change",
		ID:        id,
		ParentID:  copyEntryIDPtr(s.leafID),
		Timestamp: "2026-01-01T00:00:10.000Z",
		Provider:  provider,
		ModelID:   modelID,
	})
	s.leafID = &id
	return id, nil
}

func (s *overflowSession) AppendThinkingLevelChange(ctx context.Context, level string) (session.EntryID, error) {
	return "thinking-change", ctx.Err()
}

func (s *overflowSession) AppendCompaction(
	ctx context.Context,
	summary string,
	firstKeptEntryID session.EntryID,
	tokensBefore int,
	details any,
	fromHook bool,
) (session.EntryID, error) {
	return "compaction", ctx.Err()
}

func (s *overflowSession) AppendCustomEntry(ctx context.Context, customType string, data any) (session.EntryID, error) {
	return "custom", ctx.Err()
}

func (s *overflowSession) AppendCustomMessageEntry(
	ctx context.Context,
	customType string,
	content any,
	display bool,
	details any,
) (session.EntryID, error) {
	return "custom-message", ctx.Err()
}

func (s *overflowSession) AppendLabel(ctx context.Context, targetID session.EntryID, label string) (session.EntryID, error) {
	return "label", ctx.Err()
}

func (s *overflowSession) AppendSessionName(ctx context.Context, name string) (session.EntryID, error) {
	return "session-name", ctx.Err()
}

func (s *overflowSession) MoveTo(
	ctx context.Context,
	entryID *session.EntryID,
	summary *session.BranchSummary,
) (*session.EntryID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := copyEntryIDPtr(entryID)
	s.moveToTargets = append(s.moveToTargets, target)
	if target != nil {
		if _, ok := s.GetEntry(*target); !ok {
			return nil, fmt.Errorf("entry %q not found", *target)
		}
	}
	s.leafID = target
	return nil, nil
}

func (s *overflowSession) addEntry(entry session.SessionEntry) session.EntryID {
	s.entries = append(s.entries, entry)
	s.leafID = &entry.ID
	return entry.ID
}

func newOverflowOrchestrator(s *overflowSession, h *overflowHarness) *Orchestrator {
	return &Orchestrator{
		session:               s,
		harness:               h,
		model:                 testModel("initial-model"),
		thinkingLevel:         agentloop.ThinkingOff,
		phase:                 phaseIdle,
		autoCompactionEnabled: true,
	}
}

func userEntry(id string, parent *session.EntryID, text string, timestamp string) session.SessionEntry {
	return session.SessionEntry{
		Type:      "message",
		ID:        session.EntryID(id),
		ParentID:  copyEntryIDPtr(parent),
		Timestamp: timestamp,
		Message: ai.Message{
			Role:      ai.RoleUser,
			Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
			Timestamp: millis(timestamp),
		},
	}
}

func assistantEntry(
	id string,
	parent *session.EntryID,
	msg ai.Message,
	timestamp string,
) session.SessionEntry {
	return session.SessionEntry{
		Type:      "message",
		ID:        session.EntryID(id),
		ParentID:  copyEntryIDPtr(parent),
		Timestamp: timestamp,
		Message:   msg,
	}
}

func overflowAssistant(modelID string, timestamp int64) ai.Message {
	return ai.Message{
		Role:         ai.RoleAssistant,
		Provider:     "test-provider",
		Model:        modelID,
		StopReason:   ai.StopReasonError,
		ErrorMessage: "prompt is too long",
		Content:      []ai.ContentBlock{{Type: ai.ContentText, Text: "Error: prompt is too long"}},
		Timestamp:    timestamp,
	}
}

func stopAssistant(modelID string, text string, timestamp int64) ai.Message {
	return ai.Message{
		Role:       ai.RoleAssistant,
		Provider:   "test-provider",
		Model:      modelID,
		StopReason: ai.StopReasonStop,
		Content:    []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
		Timestamp:  timestamp,
	}
}

func entryTimestampForMessage(msg message.AgentMessage) string {
	aiMsg, ok := message.AsAIMessage(msg)
	if !ok || aiMsg.Timestamp == 0 {
		return "2026-01-01T00:00:00.000Z"
	}
	return time.UnixMilli(aiMsg.Timestamp).UTC().Format("2006-01-02T15:04:05.000Z")
}

func millis(timestamp string) int64 {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", timestamp)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

func textOfMessage(t *testing.T, msg ai.Message) string {
	t.Helper()
	if len(msg.Content) == 0 {
		t.Fatal("message content is empty")
	}
	return msg.Content[0].Text
}
