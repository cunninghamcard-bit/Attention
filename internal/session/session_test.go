package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

func TestSessionAppendBuildContextAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	s, err := createSession(path, CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AppendMessage(ctx, userMessage("hello")); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if _, err := s.AppendModelChange(ctx, "openai", "gpt-4"); err != nil {
		t.Fatalf("AppendModelChange: %v", err)
	}
	if _, err := s.AppendThinkingLevelChange(ctx, "high"); err != nil {
		t.Fatalf("AppendThinkingLevelChange: %v", err)
	}
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "claude", "hi")); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}

	reopened, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := reopened.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(got.Messages))
	}
	if got.ThinkingLevel != "high" {
		t.Fatalf("thinking level = %q, want high", got.ThinkingLevel)
	}
	if got.Model == nil || got.Model.Provider != "anthropic" || got.Model.ModelID != "claude" {
		t.Fatalf("model = %+v, want anthropic/claude", got.Model)
	}
}

func TestSessionJSONLFormat(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	s, err := createSession(path, CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AppendThinkingLevelChange(ctx, "medium"); err != nil {
		t.Fatalf("AppendThinkingLevelChange: %v", err)
	}
	if _, err := s.AppendCustomMessageEntry(ctx, "note", "hidden", false, nil); err != nil {
		t.Fatalf("AppendCustomMessageEntry: %v", err)
	}
	// Nothing reaches disk before the first assistant message (pi
	// session-manager.ts:843-861); this flushes the buffered entries.
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "claude", "hi")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	lines := readJSONLLines(t, path)
	if len(lines) != 4 {
		t.Fatalf("lines len = %d, want 4", len(lines))
	}

	var header map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header["type"] != "session" || header["version"] != float64(3) {
		t.Fatalf("header = %+v, want session version 3", header)
	}

	var thinking map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &thinking); err != nil {
		t.Fatalf("unmarshal thinking entry: %v", err)
	}
	if thinking["thinkingLevel"] != "medium" {
		t.Fatalf("thinkingLevel = %v, want medium", thinking["thinkingLevel"])
	}

	var custom map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &custom); err != nil {
		t.Fatalf("unmarshal custom entry: %v", err)
	}
	display, ok := custom["display"].(bool)
	if !ok || display {
		t.Fatalf("display = %v, want explicit false", custom["display"])
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d, want custom + assistant", len(got.Messages))
	}
	if _, ok := got.Messages[0].(message.CustomMessage); !ok {
		t.Fatalf("context message type = %T, want CustomMessage", got.Messages[0])
	}

	reopened, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries := reopened.GetEntries()
	if len(entries) != 3 {
		t.Fatalf("reopened entries len = %d, want 3", len(entries))
	}
	customEntry := entries[1]
	if customEntry.Type != "custom_message" ||
		customEntry.CustomType != "note" ||
		customEntry.Content != "hidden" ||
		customEntry.Display {
		t.Fatalf("custom message entry = %+v, want hidden note with display=false", customEntry)
	}
}

func TestSessionCompactionProjection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := createSession(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AppendMessage(ctx, userMessage("drop")); err != nil {
		t.Fatalf("AppendMessage drop: %v", err)
	}
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "old", "drop answer")); err != nil {
		t.Fatalf("AppendMessage drop assistant: %v", err)
	}
	keptID, err := s.AppendMessage(ctx, userMessage("keep"))
	if err != nil {
		t.Fatalf("AppendMessage keep: %v", err)
	}
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "old", "keep answer")); err != nil {
		t.Fatalf("AppendMessage keep assistant: %v", err)
	}
	if _, err := s.AppendCompaction(ctx, "summary", keptID, 5000, nil, false); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	if _, err := s.AppendMessage(ctx, userMessage("after")); err != nil {
		t.Fatalf("AppendMessage after: %v", err)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(got.Messages))
	}
	if _, ok := got.Messages[0].(message.CompactionSummaryMessage); !ok {
		t.Fatalf("first message type = %T, want CompactionSummaryMessage", got.Messages[0])
	}
	if textOfAIMessage(t, got.Messages[1]) != "keep" {
		t.Fatalf("second message text = %q, want keep", textOfAIMessage(t, got.Messages[1]))
	}
}

func TestSessionMoveToCreatesBranch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	s, err := createSession(path, CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	id1, err := s.AppendMessage(ctx, userMessage("root"))
	if err != nil {
		t.Fatalf("AppendMessage root: %v", err)
	}
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "model", "old branch")); err != nil {
		t.Fatalf("AppendMessage old: %v", err)
	}
	if _, err := s.AppendMessage(ctx, userMessage("discarded")); err != nil {
		t.Fatalf("AppendMessage discarded: %v", err)
	}
	if _, err := s.MoveTo(ctx, &id1, nil); err != nil {
		t.Fatalf("MoveTo: %v", err)
	}
	newID, err := s.AppendMessage(ctx, assistantMessage("anthropic", "model", "new branch"))
	if err != nil {
		t.Fatalf("AppendMessage new: %v", err)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(got.Messages))
	}
	if textOfAIMessage(t, got.Messages[1]) != "new branch" {
		t.Fatalf("branched assistant = %q, want new branch", textOfAIMessage(t, got.Messages[1]))
	}

	lines := readJSONLLines(t, path)
	containsLeaf := false
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["type"] == "leaf" && entry["targetId"] == string(id1) {
			containsLeaf = true
		}
	}
	if !containsLeaf {
		t.Fatal("expected JSONL leaf entry for MoveTo")
	}

	reopened, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	leafID, err := reopened.GetLeafID()
	if err != nil {
		t.Fatalf("GetLeafID reopened: %v", err)
	}
	if leafID == nil || *leafID != newID {
		t.Fatalf("reopened leafID = %v, want %s", leafID, newID)
	}
	branch, err := reopened.GetBranch(nil)
	if err != nil {
		t.Fatalf("GetBranch reopened: %v", err)
	}
	if ids := entryIDs(branch); strings.Join(ids, ",") != string(id1)+","+string(newID) {
		t.Fatalf("reopened branch ids = %v, want [%s %s]", ids, id1, newID)
	}
}

func TestSessionGetBranchNilUsesCurrentLeaf(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := createSession(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AppendMessage(ctx, userMessage("first")); err != nil {
		t.Fatalf("AppendMessage first: %v", err)
	}
	if _, err := s.AppendMessage(ctx, userMessage("second")); err != nil {
		t.Fatalf("AppendMessage second: %v", err)
	}

	branch, err := s.GetBranch(nil)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if len(branch) != 2 {
		t.Fatalf("branch len = %d, want 2", len(branch))
	}
}

func TestSessionMoveToWithSummaryMovesLeafToSummary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := createSession(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	id, err := s.AppendMessage(ctx, userMessage("base"))
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	summaryID, err := s.MoveTo(ctx, &id, &BranchSummary{Summary: "branch notes"})
	if err != nil {
		t.Fatalf("MoveTo summary: %v", err)
	}
	if summaryID == nil {
		t.Fatal("summaryID is nil")
	}
	leafID, err := s.GetLeafID()
	if err != nil {
		t.Fatalf("GetLeafID: %v", err)
	}
	if leafID == nil || *leafID != *summaryID {
		t.Fatalf("leafID = %v, want summaryID %v", leafID, summaryID)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(got.Messages))
	}
	if _, ok := got.Messages[1].(message.BranchSummaryMessage); !ok {
		t.Fatalf("second message type = %T, want BranchSummaryMessage", got.Messages[1])
	}
}

func TestSessionLabelsAndName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	s, err := createSession(path, CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	id, err := s.AppendMessage(ctx, userMessage("label me"))
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.AppendLabel(ctx, id, "checkpoint"); err != nil {
		t.Fatalf("AppendLabel: %v", err)
	}
	if label, ok := s.GetLabel(id); !ok || label != "checkpoint" {
		t.Fatalf("label = %q/%v, want checkpoint/true", label, ok)
	}
	if _, err := s.AppendLabel(ctx, id, ""); err != nil {
		t.Fatalf("AppendLabel clear: %v", err)
	}
	if label, ok := s.GetLabel(id); ok || label != "" {
		t.Fatalf("label after clear = %q/%v, want empty/false", label, ok)
	}

	if _, err := s.AppendSessionName(ctx, "  refactor auth  "); err != nil {
		t.Fatalf("AppendSessionName: %v", err)
	}
	if name, ok := s.GetSessionName(); !ok || name != "refactor auth" {
		t.Fatalf("session name = %q/%v, want refactor auth/true", name, ok)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1; labels/session_info must not enter context", len(got.Messages))
	}

	// Flush to disk: nothing persists before the first assistant message
	// (pi session-manager.ts:843-861).
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "claude", "ok")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	reopened, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if name, ok := reopened.GetSessionName(); !ok || name != "refactor auth" {
		t.Fatalf("reopened session name = %q/%v, want refactor auth/true", name, ok)
	}
	if label, ok := reopened.GetLabel(id); ok || label != "" {
		t.Fatalf("reopened label after clear = %q/%v, want empty/false", label, ok)
	}
}

func TestSessionAppendLabelRejectsMissingEntry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := createSession(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = s.AppendLabel(ctx, "missing", "checkpoint")
	if err == nil {
		t.Fatal("AppendLabel missing error = nil, want not_found")
	}
	assertSessionErrorCode(t, err, ErrorNotFound)
}

func TestOpenRejectsCorruptSession(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "non json header",
			content: "not json\n",
		},
		{
			name:    "missing header id",
			content: `{"type":"session","version":3,"timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + "\"}\n",
		},
		{
			name:    "missing header cwd",
			content: `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z"}` + "\n",
		},
		{
			name:    "invalid parent session type",
			content: `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + `","parentSession":null}` + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".jsonl")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := openSession(path); err == nil {
				t.Fatal("Open error = nil, want error")
			}
		})
	}
}

func TestOpenSkipsMalformedEntryLines(t *testing.T) {
	dir := t.TempDir()
	header := `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + "\"}\n"
	good := `{"type":"message","id":"m-good","parentId":null,"timestamp":"2026-05-30T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1}}` + "\n"

	tests := []struct {
		name string
		bad  string
	}{
		{name: "truncated json", bad: `{"type":"message","id":"m-bad","parentId":null,"timesta`},
		{name: "invalid parent type", bad: `{"type":"message","id":"m-bad","parentId":42,"timestamp":"2026-05-30T00:00:02Z","message":{"role":"user"}}`},
		{name: "missing entry type", bad: `{"id":"m-bad","parentId":null,"timestamp":"2026-05-30T00:00:02Z"}`},
		{name: "missing entry id", bad: `{"type":"message","parentId":null,"timestamp":"2026-05-30T00:00:02Z"}`},
		{name: "missing entry timestamp", bad: `{"type":"message","id":"m-bad","parentId":null}`},
		{name: "invalid leaf target type", bad: `{"type":"leaf","id":"leaf","parentId":null,"timestamp":"2026-05-30T00:00:02Z","targetId":42}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".jsonl")
			content := header + good + tt.bad + "\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			// pi skips malformed lines instead of failing the whole session —
			// a truncated trailing line is the exact artifact of a crash
			// during append (session-manager.ts:448-455).
			s, err := openSession(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			entries := s.GetEntries()
			if len(entries) != 1 || entries[0].ID != "m-good" {
				t.Fatalf("entries = %+v, want only m-good", entries)
			}
		})
	}
}

func TestOpenMissingFileReturnsSessionError(t *testing.T) {
	_, err := openSession(filepath.Join(t.TempDir(), "missing.jsonl"))
	if err == nil {
		t.Fatal("Open missing file error = nil, want not_found")
	}
	assertSessionErrorCode(t, err, ErrorNotFound)
}

func TestCreateJSONLRejectsMissingCWD(t *testing.T) {
	_, err := CreateJSONL(filepath.Join(t.TempDir(), "session.jsonl"), CreateOptions{})
	if err == nil {
		t.Fatal("CreateJSONL missing cwd error = nil, want invalid_session")
	}
	assertSessionErrorCode(t, err, ErrorInvalidSession)
}

func TestCreateJSONLStorageError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "session.jsonl")
	// Creation itself touches no disk (pi session-manager.ts:772-794); the
	// storage error surfaces when the first assistant message flushes.
	storage, err := CreateJSONL(path, CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("CreateJSONL: %v", err)
	}
	err = storage.AppendEntry(SessionEntry{
		Type:      "message",
		ID:        storage.CreateEntryID(),
		Timestamp: newTimestamp(),
		Message:   ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hi"}}},
	})
	if err == nil {
		t.Fatal("flush storage error = nil, want storage")
	}
	assertSessionErrorCode(t, err, ErrorStorage)
}

func TestJSONLRejectsDanglingLeafAndParent(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		check   func(t *testing.T, s *Session)
	}{
		{
			name: "dangling leaf target",
			content: `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + "\"}\n" +
				`{"type":"leaf","id":"leaf","parentId":null,"timestamp":"2026-05-30T00:00:01Z","targetId":"missing"}` + "\n",
			check: func(t *testing.T, s *Session) {
				t.Helper()
				_, err := s.GetLeafID()
				if err == nil {
					t.Fatal("GetLeafID error = nil, want invalid_session")
				}
				assertSessionErrorCode(t, err, ErrorInvalidSession)
			},
		},
		{
			name: "dangling parent",
			content: `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + "\"}\n" +
				`{"type":"message","id":"child","parentId":"missing","timestamp":"2026-05-30T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n",
			check: func(t *testing.T, s *Session) {
				t.Helper()
				_, err := s.GetBranch(nil)
				if err == nil {
					t.Fatal("GetBranch error = nil, want invalid_session")
				}
				assertSessionErrorCode(t, err, ErrorInvalidSession)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".jsonl")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			s, err := openSession(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			tt.check(t, s)
		})
	}
}

func TestOpenPreservesUnknownEntryAndSkipsItInContext(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + "\"}\n" +
		`{"type":"unknown","id":"u1","parentId":null,"timestamp":"2026-05-30T00:00:01Z","value":1}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries := s.GetEntries()
	if len(entries) != 1 || entries[0].Type != "unknown" || len(entries[0].Raw) == 0 {
		t.Fatalf("entries = %+v, want preserved unknown raw entry", entries)
	}
	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("messages len = %d, want 0", len(got.Messages))
	}
}

func userMessage(text string) ai.Message {
	return ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
	}
}

func assistantMessage(provider string, model string, text string) ai.Message {
	return ai.Message{
		Role:     ai.RoleAssistant,
		Provider: provider,
		Model:    model,
		Content:  []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
	}
}

func createSession(path string, opts CreateOptions) (*Session, error) {
	storage, err := CreateJSONL(path, opts)
	if err != nil {
		return nil, err
	}
	return NewSession(storage), nil
}

func openSession(path string) (*Session, error) {
	storage, err := OpenJSONL(path)
	if err != nil {
		return nil, err
	}
	return NewSession(storage), nil
}

func textOfAIMessage(t *testing.T, msg message.AgentMessage) string {
	t.Helper()

	aiMsg, ok := message.AsAIMessage(msg)
	if !ok {
		t.Fatalf("message type = %T, want ai.Message", msg)
	}
	if len(aiMsg.Content) == 0 {
		return ""
	}
	return aiMsg.Content[0].Text
}

func TestSessionMoveToNilReturnsToRoot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := createSession(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AppendMessage(ctx, userMessage("first")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.AppendMessage(ctx, userMessage("second")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.MoveTo(ctx, nil, nil); err != nil {
		t.Fatalf("MoveTo nil: %v", err)
	}
	if _, err := s.AppendMessage(ctx, userMessage("from root")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1 (only message from new root)", len(got.Messages))
	}
}

func TestSessionBranchSummaryFromIDRootRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Write a session with a branch_summary from root (fromId: "root")
	content := `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + "\"}\n" +
		`{"type":"message","id":"m1","parentId":null,"timestamp":"2026-05-30T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n" +
		`{"type":"leaf","id":"leaf1","parentId":"m1","timestamp":"2026-05-30T00:00:02Z","targetId":null}` + "\n" +
		`{"type":"branch_summary","id":"bs1","parentId":null,"timestamp":"2026-05-30T00:00:03Z","fromId":"root","summary":"back to root"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	entry, ok := s.GetEntry("bs1")
	if !ok {
		t.Fatal("branch_summary entry not found")
	}
	if entry.FromID != nil {
		t.Fatalf("FromID = %v, want nil (root)", entry.FromID)
	}
	if entry.Summary != "back to root" {
		t.Fatalf("Summary = %q, want back to root", entry.Summary)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1 (branch summary at root)", len(got.Messages))
	}
}

func TestSessionGetLabelNonexistent(t *testing.T) {
	dir := t.TempDir()
	s, err := createSession(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	label, ok := s.GetLabel("nonexistent")
	if ok || label != "" {
		t.Fatalf("GetLabel = %q/%v, want empty/false", label, ok)
	}
}

func TestSessionGetSessionNameUnset(t *testing.T) {
	dir := t.TempDir()
	s, err := createSession(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	name, ok := s.GetSessionName()
	if ok || name != "" {
		t.Fatalf("GetSessionName = %q/%v, want empty/false", name, ok)
	}
}

func TestSessionCompactionFromHookRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	s, err := createSession(path, CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AppendMessage(ctx, userMessage("msg")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	keptID, err := s.AppendMessage(ctx, userMessage("keep"))
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.AppendCompaction(ctx, "hook summary", keptID, 3000, map[string]any{"source": "hook"}, true); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	// Flush to disk (pi defers writes until the first assistant message).
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "claude", "ok")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	reopened, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries := reopened.GetEntries()
	compactions := []SessionEntry{}
	for _, e := range entries {
		if e.Type == "compaction" {
			compactions = append(compactions, e)
		}
	}
	if len(compactions) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(compactions))
	}
	c := compactions[0]
	if !c.FromHook {
		t.Fatal("FromHook = false, want true")
	}
	if c.TokensBefore != 3000 {
		t.Fatalf("TokensBefore = %d, want 3000", c.TokensBefore)
	}
}

func TestOpenSkipsEntryWithMissingTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Entry with missing timestamp
	content := `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + "\"}\n" +
		`{"type":"message","id":"m1","parentId":null}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if entries := s.GetEntries(); len(entries) != 0 {
		t.Fatalf("entries = %+v, want missing-timestamp entry skipped", entries)
	}
}

func TestSessionCustomEntryNotInContext(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	s, err := createSession(path, CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AppendMessage(ctx, userMessage("visible")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.AppendCustomEntry(ctx, "internal_state", map[string]any{"key": "val"}); err != nil {
		t.Fatalf("AppendCustomEntry: %v", err)
	}
	if _, err := s.AppendMessage(ctx, userMessage("also visible")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (custom entry excluded)", len(got.Messages))
	}

	// Flush to disk (pi defers writes until the first assistant message).
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "claude", "ok")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	reopened, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries := reopened.GetEntries()
	var custom *SessionEntry
	for i := range entries {
		if entries[i].Type == "custom" {
			custom = &entries[i]
			break
		}
	}
	if custom == nil {
		t.Fatal("custom entry missing after reopen")
	}
	if custom.CustomType != "internal_state" {
		t.Fatalf("custom type = %q, want internal_state", custom.CustomType)
	}
	data, ok := custom.Data.(map[string]any)
	if !ok || data["key"] != "val" {
		t.Fatalf("custom data = %#v, want key=val", custom.Data)
	}
}

func TestJsonlSessionStorageDirectBehavior(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	storage, err := CreateJSONL(path, CreateOptions{
		ID:  "session-1",
		CWD: dir,
	})
	if err != nil {
		t.Fatalf("CreateJSONL: %v", err)
	}

	entryID := EntryID("entry-1")
	entry := SessionEntry{
		Type:      "message",
		ID:        entryID,
		ParentID:  nil,
		Timestamp: "2026-01-01T00:00:00.000Z",
		// assistant role so the deferred buffer flushes to disk
		// (pi session-manager.ts:843-861).
		Message: assistantMessage("anthropic", "claude", "one"),
	}
	if err := storage.AppendEntry(entry); err != nil {
		t.Fatalf("AppendEntry message: %v", err)
	}

	metadata := storage.GetMetadata()
	if metadata.ID != "session-1" || metadata.CWD != dir || metadata.Path != path {
		t.Fatalf("metadata = %+v, want id/cwd/path", metadata)
	}
	if entries := storage.GetEntries(); len(entries) != 1 || entries[0].ID != entryID {
		t.Fatalf("entries = %+v, want entry-1 only", entries)
	}
	leafID, err := storage.GetLeafID()
	if err != nil {
		t.Fatalf("GetLeafID: %v", err)
	}
	if leafID == nil || *leafID != entryID {
		t.Fatalf("leafID = %v, want entry-1", leafID)
	}
	if err := storage.SetLeafID(nil); err != nil {
		t.Fatalf("SetLeafID nil: %v", err)
	}
	leafID, err = storage.GetLeafID()
	if err != nil {
		t.Fatalf("GetLeafID after nil: %v", err)
	}
	if leafID != nil {
		t.Fatalf("leafID after nil = %v, want nil", leafID)
	}
	if entries := storage.GetEntries(); entries[len(entries)-1].Type != "leaf" || entries[len(entries)-1].TargetID != nil {
		t.Fatalf("last entry = %+v, want leaf to root", entries[len(entries)-1])
	}

	missing := EntryID("missing")
	err = storage.SetLeafID(&missing)
	if err == nil {
		t.Fatal("SetLeafID missing error = nil, want not_found")
	}
	assertSessionErrorCode(t, err, ErrorNotFound)

	if found := storage.FindEntries("message"); len(found) != 1 || found[0].ID != entryID {
		t.Fatalf("FindEntries message = %+v, want entry-1", found)
	}
	if found := storage.FindEntries("session_info"); len(found) != 0 {
		t.Fatalf("FindEntries session_info = %+v, want empty", found)
	}

	labelID := EntryID("label-1")
	if err := storage.AppendEntry(SessionEntry{
		Type:      "label",
		ID:        labelID,
		ParentID:  nil,
		Timestamp: "2026-01-01T00:00:01.000Z",
		TargetID:  &entryID,
		Label:     " checkpoint ",
	}); err != nil {
		t.Fatalf("AppendEntry label: %v", err)
	}
	if label, ok := storage.GetLabel(entryID); !ok || label != "checkpoint" {
		t.Fatalf("label = %q/%v, want checkpoint/true", label, ok)
	}
	if err := storage.AppendEntry(SessionEntry{
		Type:      "label",
		ID:        "label-2",
		ParentID:  &labelID,
		Timestamp: "2026-01-01T00:00:02.000Z",
		TargetID:  &entryID,
		Label:     "",
	}); err != nil {
		t.Fatalf("AppendEntry label clear: %v", err)
	}
	if label, ok := storage.GetLabel(entryID); ok || label != "" {
		t.Fatalf("label after clear = %q/%v, want empty/false", label, ok)
	}
	if path, err := storage.GetPathToRoot(nil); err != nil || len(path) != 0 {
		t.Fatalf("GetPathToRoot nil = %+v/%v, want empty/nil", path, err)
	}

	reopened, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	rootID := EntryID("entry-1")
	child := SessionEntry{
		Type:      "message",
		ID:        "child",
		ParentID:  &rootID,
		Timestamp: "2026-01-01T00:00:03.000Z",
		Message:   assistantMessage("anthropic", "claude", "child"),
	}
	if err := reopened.AppendEntry(child); err != nil {
		t.Fatalf("AppendEntry child: %v", err)
	}
	childID := EntryID("child")
	rootPath, err := reopened.GetPathToRoot(&childID)
	if err != nil {
		t.Fatalf("GetPathToRoot: %v", err)
	}
	if ids := entryIDs(rootPath); strings.Join(ids, ",") != "entry-1,child" {
		t.Fatalf("path ids = %v, want [entry-1 child]", ids)
	}
}

func TestJsonlSessionMetadataSessionTreeFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	storage, err := CreateJSONL(path, CreateOptions{
		ID:                "child",
		CWD:               dir,
		ParentSessionPath: "/tmp/parent.jsonl",
		ParentRef:         "parent",
		SpawnedBy: &SpawnedBy{
			SessionID:  "root",
			RunID:      "run_1",
			ToolCallID: "tool_1",
		},
	})
	if err != nil {
		t.Fatalf("CreateJSONL: %v", err)
	}

	if err := storage.AppendEntry(SessionEntry{
		Type:      "message",
		ID:        EntryID("assistant-1"),
		ParentID:  nil,
		Timestamp: "2026-01-01T00:00:00.000Z",
		Message:   assistantMessage("anthropic", "claude", "one"),
	}); err != nil {
		t.Fatalf("AppendEntry message: %v", err)
	}

	metadata, err := LoadJSONLMetadata(path)
	if err != nil {
		t.Fatalf("LoadJSONLMetadata: %v", err)
	}
	if metadata.ParentRef != "parent" ||
		metadata.SpawnedBy == nil ||
		metadata.SpawnedBy.SessionID != "root" ||
		metadata.SpawnedBy.RunID != "run_1" ||
		metadata.SpawnedBy.ToolCallID != "tool_1" {
		t.Fatalf("metadata = %+v, want session tree fields", metadata)
	}

	reopened, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	reopenedMetadata := reopened.GetMetadata()
	if reopenedMetadata.ParentRef != "parent" ||
		reopenedMetadata.SpawnedBy == nil ||
		reopenedMetadata.SpawnedBy.ToolCallID != "tool_1" {
		t.Fatalf("reopened metadata = %+v, want session tree fields", reopenedMetadata)
	}
}

func TestJsonlSessionStorageCreateEntryIDUsesShortUUIDPrefix(t *testing.T) {
	dir := t.TempDir()
	storage, err := CreateJSONL(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("CreateJSONL: %v", err)
	}

	id := string(storage.CreateEntryID())
	if len(id) != 8 {
		t.Fatalf("entry id len = %d, want 8", len(id))
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("entry id = %q, want lowercase hex UUID prefix", id)
		}
	}
}

func TestJsonlSessionStorageMatchesSessionCore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, err := CreateJSONL(filepath.Join(dir, "session.jsonl"), CreateOptions{CWD: dir})
	if err != nil {
		t.Fatalf("CreateJSONL: %v", err)
	}
	s := NewSession(storage)

	rootID, err := s.AppendMessage(ctx, userMessage("root"))
	if err != nil {
		t.Fatalf("AppendMessage root: %v", err)
	}
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "claude", "old")); err != nil {
		t.Fatalf("AppendMessage old: %v", err)
	}
	if _, err := s.MoveTo(ctx, &rootID, nil); err != nil {
		t.Fatalf("MoveTo: %v", err)
	}
	if _, err := s.AppendMessage(ctx, assistantMessage("anthropic", "claude", "new")); err != nil {
		t.Fatalf("AppendMessage new: %v", err)
	}

	got, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(got.Messages))
	}
	if textOfAIMessage(t, got.Messages[1]) != "new" {
		t.Fatalf("branched message = %q, want new", textOfAIMessage(t, got.Messages[1]))
	}
	entries := storage.GetEntries()
	if entries[len(entries)-2].Type != "leaf" {
		t.Fatalf("penultimate entry type = %q, want leaf", entries[len(entries)-2].Type)
	}
}

func TestJsonlSessionRepoLifecycleAndFork(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := NewJsonlSessionRepo(root)

	source, err := repo.Create(ctx, JsonlSessionCreateOptions{
		ID:  "source",
		CWD: "/tmp/my-project",
	})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	// Unflushed sessions have no file and never appear in List (pi defers
	// writes until the first assistant message).
	seed, err := source.AppendMessage(ctx, assistantMessage("anthropic", "claude", "hi"))
	if err != nil {
		t.Fatalf("AppendMessage source: %v", err)
	}
	sourceMetadata := source.GetMetadata()
	if !strings.Contains(sourceMetadata.Path, "--tmp-my-project--") {
		t.Fatalf("source path = %q, want encoded cwd", sourceMetadata.Path)
	}
	if _, err := repo.Create(ctx, JsonlSessionCreateOptions{
		ID:  "other",
		CWD: "/tmp/other-project",
	}); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	byCWD, err := repo.List(ctx, JsonlSessionListOptions{CWD: "/tmp/my-project"})
	if err != nil {
		t.Fatalf("List by cwd: %v", err)
	}
	if len(byCWD) != 1 || byCWD[0].ID != "source" {
		t.Fatalf("List by cwd = %+v, want source only", byCWD)
	}
	all, err := repo.List(ctx, JsonlSessionListOptions{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	// "other" never got an assistant message, so it has no file and is not
	// listed (pi defers writes until the first assistant message).
	if len(all) != 1 || all[0].ID != "source" {
		t.Fatalf("List all = %+v, want source only", all)
	}

	user1, err := source.AppendMessage(ctx, userMessage("one"))
	if err != nil {
		t.Fatalf("AppendMessage user1: %v", err)
	}
	assistant1, err := source.AppendMessage(ctx, assistantMessage("anthropic", "claude", "two"))
	if err != nil {
		t.Fatalf("AppendMessage assistant1: %v", err)
	}
	user2, err := source.AppendMessage(ctx, userMessage("three"))
	if err != nil {
		t.Fatalf("AppendMessage user2: %v", err)
	}

	reopened, err := repo.Open(ctx, sourceMetadata)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reopened.GetMetadata() != sourceMetadata {
		t.Fatalf("reopened metadata = %+v, want %+v", reopened.GetMetadata(), sourceMetadata)
	}

	forked, err := repo.Fork(ctx, sourceMetadata, JsonlSessionForkOptions{
		ID:      "fork",
		CWD:     "/tmp/target",
		EntryID: &user2,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	forkMetadata := forked.GetMetadata()
	if forkMetadata.CWD != "/tmp/target" {
		t.Fatalf("fork cwd = %q, want /tmp/target", forkMetadata.CWD)
	}
	if forkMetadata.ParentSessionPath != sourceMetadata.Path {
		t.Fatalf("parent path = %q, want %q", forkMetadata.ParentSessionPath, sourceMetadata.Path)
	}
	if forkMetadata.ParentRef != sourceMetadata.ID {
		t.Fatalf("parent ref = %q, want %q", forkMetadata.ParentRef, sourceMetadata.ID)
	}
	if ids := entryIDs(forked.GetEntries()); strings.Join(ids, ",") != string(seed)+","+string(user1)+","+string(assistant1) {
		t.Fatalf("fork ids = %v, want [%s %s %s]", ids, seed, user1, assistant1)
	}

	atFork, err := repo.Fork(ctx, sourceMetadata, JsonlSessionForkOptions{
		ID:        "at-fork",
		CWD:       "/tmp/target-at",
		EntryID:   &assistant1,
		Position:  ForkAt,
		ParentRef: "explicit-parent",
		SpawnedBy: &SpawnedBy{SessionID: "source", RunID: "run_1"},
	})
	if err != nil {
		t.Fatalf("Fork at: %v", err)
	}
	atForkMetadata := atFork.GetMetadata()
	if atForkMetadata.ParentRef != "explicit-parent" ||
		atForkMetadata.SpawnedBy == nil ||
		atForkMetadata.SpawnedBy.RunID != "run_1" {
		t.Fatalf("at fork metadata = %+v, want explicit session tree fields", atForkMetadata)
	}
	if ids := entryIDs(atFork.GetEntries()); strings.Join(ids, ",") != string(seed)+","+string(user1)+","+string(assistant1) {
		t.Fatalf("at fork ids = %v, want [%s %s]", ids, user1, assistant1)
	}

	fullFork, err := repo.Fork(ctx, sourceMetadata, JsonlSessionForkOptions{
		ID:  "full-fork",
		CWD: "/tmp/target-full",
	})
	if err != nil {
		t.Fatalf("Fork full: %v", err)
	}
	if ids := entryIDs(fullFork.GetEntries()); strings.Join(ids, ",") != string(seed)+","+string(user1)+","+string(assistant1)+","+string(user2) {
		t.Fatalf("full fork ids = %v, want full source path", ids)
	}

	_, err = repo.Fork(ctx, sourceMetadata, JsonlSessionForkOptions{
		ID:      "bad-fork",
		CWD:     "/tmp/target-bad",
		EntryID: &assistant1,
	})
	if err == nil {
		t.Fatal("Fork before assistant error = nil, want invalid_fork_target")
	}
	assertSessionErrorCode(t, err, ErrorInvalidForkTarget)

	_, err = repo.Fork(ctx, sourceMetadata, JsonlSessionForkOptions{
		ID: "missing-cwd",
	})
	if err == nil {
		t.Fatal("Fork missing cwd error = nil, want invalid_session")
	}
	assertSessionErrorCode(t, err, ErrorInvalidSession)

	if err := repo.Delete(ctx, sourceMetadata); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(sourceMetadata.Path); !os.IsNotExist(err) {
		t.Fatalf("source path still exists or stat error = %v", err)
	}
	if _, err := repo.Open(ctx, sourceMetadata); err == nil {
		t.Fatal("Open deleted source error = nil, want error")
	}
}

func TestJsonlSessionRepoForkSkipsConversationRestore(t *testing.T) {
	ctx := context.Background()
	repo := NewJsonlSessionRepo(t.TempDir())

	source, err := repo.Create(ctx, JsonlSessionCreateOptions{
		ID:  "source",
		CWD: "/tmp/source-project",
	})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	sourceMetadata := source.GetMetadata()
	user, err := source.AppendMessage(ctx, userMessage("one"))
	if err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	assistant, err := source.AppendMessage(ctx, assistantMessage("anthropic", "claude", "two"))
	if err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}

	defaultFork, err := repo.Fork(ctx, sourceMetadata, JsonlSessionForkOptions{
		ID:  "default-fork",
		CWD: "/tmp/default-target",
	})
	if err != nil {
		t.Fatalf("Fork default: %v", err)
	}
	if ids := entryIDs(defaultFork.GetEntries()); strings.Join(ids, ",") != string(user)+","+string(assistant) {
		t.Fatalf("default fork ids = %v, want copied conversation", ids)
	}

	skippedFork, err := repo.Fork(ctx, sourceMetadata, JsonlSessionForkOptions{
		ID:                      "skip-fork",
		CWD:                     "/tmp/skip-target",
		SkipConversationRestore: true,
	})
	if err != nil {
		t.Fatalf("Fork skipped restore: %v", err)
	}
	if entries := skippedFork.GetEntries(); len(entries) != 0 {
		t.Fatalf("skipped fork entries len = %d, want 0: %+v", len(entries), entries)
	}
	metadata := skippedFork.GetMetadata()
	if metadata.CWD != "/tmp/skip-target" || metadata.ParentSessionPath != sourceMetadata.Path {
		t.Fatalf("skipped fork metadata = %+v, want target cwd and source parent", metadata)
	}
	// pi writes nothing for a fresh fork until its first assistant message
	// (session-manager.ts:772-794,843-861).
	if _, err := os.Stat(metadata.Path); !os.IsNotExist(err) {
		t.Fatalf("skipped fork file stat err = %v, want not-exist", err)
	}

	// Reopening by path requires the file; an unflushed fork only lives in
	// memory, so Open reports not-found until an assistant message lands.
	if _, err := repo.Open(ctx, metadata); err == nil {
		t.Fatal("Open unflushed fork = nil error, want not found")
	}
}

func TestLoadJSONLMetadataReadsOnlyHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + `","parentSession":"/tmp/parent.jsonl"}` + "\n" +
		"{not json\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	metadata, err := LoadJSONLMetadata(path)
	if err != nil {
		t.Fatalf("LoadJSONLMetadata: %v", err)
	}
	if metadata.ID != "s" || metadata.ParentSessionPath != "/tmp/parent.jsonl" {
		t.Fatalf("metadata = %+v, want id and parent path", metadata)
	}
}

func TestLoadJSONLMetadataSkipsBlankFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := "\n" + `{"type":"session","version":3,"id":"s","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + `"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// pi skips blank lines before validating the header (session-manager.ts:449).
	metadata, err := LoadJSONLMetadata(path)
	if err != nil {
		t.Fatalf("LoadJSONLMetadata: %v", err)
	}
	if metadata.ID != "s" {
		t.Fatalf("metadata = %+v, want header parsed", metadata)
	}
}

func TestJsonlSessionRepoListSortsByLastActivity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cwd := "/tmp/activity-project"
	dir := filepath.Join(root, encodeCWD(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Created earlier but active later — pi sorts it first
	// (session-manager.ts:1410-1414, modified = last message activity).
	oldButActive := `{"type":"session","version":3,"id":"old-active","timestamp":"2026-05-01T00:00:00Z","cwd":"` + cwd + `"}` + "\n" +
		`{"type":"message","id":"m1","parentId":null,"timestamp":"2026-05-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1780000000000}}` + "\n"
	newButIdle := `{"type":"session","version":3,"id":"new-idle","timestamp":"2026-05-20T00:00:00Z","cwd":"` + cwd + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(oldButActive), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.jsonl"), []byte(newButIdle), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	repo := NewJsonlSessionRepo(root)
	sessions, err := repo.List(ctx, JsonlSessionListOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 || sessions[0].ID != "old-active" || sessions[1].ID != "new-idle" {
		ids := []string{}
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		t.Fatalf("order = %v, want [old-active new-idle]", ids)
	}
}

func TestJsonlSessionRepoListSortsByParsedCreatedAt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cwd := "/tmp/sort-project"
	dir := filepath.Join(root, encodeCWD(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	files := map[string]string{
		"older.jsonl": `{"type":"session","version":3,"id":"older","timestamp":"2026-01-01T00:00:00Z","cwd":"` + cwd + `"}` + "\n",
		"newer.jsonl": `{"type":"session","version":3,"id":"newer","timestamp":"2026-01-01T00:00:00.500Z","cwd":"` + cwd + `"}` + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	repo := NewJsonlSessionRepo(root)
	list, err := repo.List(ctx, JsonlSessionListOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := metadataIDs(list); strings.Join(ids, ",") != "newer,older" {
		t.Fatalf("list ids = %v, want [newer older]", ids)
	}
}

func TestNewRandomIDUsesUUIDv7LayoutAndOrder(t *testing.T) {
	previous := ""
	for range 100 {
		id := newRandomID()
		if !isUUIDv7(id) {
			t.Fatalf("id = %q, want UUIDv7 layout", id)
		}
		if previous != "" && previous >= id {
			t.Fatalf("ids not monotonic: previous %q current %q", previous, id)
		}
		previous = id
	}
}

func readJSONLLines(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}
	}
	return lines
}

func entryIDs(entries []SessionEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, string(entry.ID))
	}
	return ids
}

func metadataIDs(metadata []Metadata) []string {
	ids := make([]string, 0, len(metadata))
	for _, item := range metadata {
		ids = append(ids, item.ID)
	}
	return ids
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func assertSessionErrorCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	sessionErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error = %T %[1]v, want *Error", err)
	}
	if sessionErr.Code != code {
		t.Fatalf("error code = %s, want %s", sessionErr.Code, code)
	}
}

func isUUIDv7(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
	}
	return id[14] == '7' && strings.ContainsRune("89ab", rune(id[19]))
}

func TestOpenJSONLMigratesV1Session(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v1.jsonl")
	// v1: no version field, no entry ids, compaction uses firstKeptEntryIndex
	// (an index into the file entries INCLUDING the header).
	content := `{"type":"session","id":"s1","timestamp":"2026-05-30T00:00:00Z","cwd":"` + dir + `"}` + "\n" +
		`{"type":"message","timestamp":"2026-05-30T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1}}` + "\n" +
		`{"type":"message","timestamp":"2026-05-30T00:00:02Z","message":{"role":"hookMessage","content":[{"type":"text","text":"hooked"}],"timestamp":2}}` + "\n" +
		`{"type":"compaction","timestamp":"2026-05-30T00:00:03Z","summary":"sum","firstKeptEntryIndex":2,"tokensBefore":10}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := openSession(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries := s.GetEntries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(entries), entries)
	}
	// id/parentId chain assigned linearly (session-manager.ts:215-241).
	if entries[0].ID == "" || entries[0].ParentID != nil {
		t.Fatalf("first entry = %+v, want id set and nil parent", entries[0])
	}
	if entries[1].ParentID == nil || *entries[1].ParentID != entries[0].ID {
		t.Fatalf("second entry parent = %+v, want first entry id", entries[1].ParentID)
	}
	// firstKeptEntryIndex 2 counts the header, so it targets the second
	// message entry.
	if entries[2].Type != "compaction" || entries[2].FirstKeptEntryID != entries[1].ID {
		t.Fatalf("compaction = %+v, want firstKeptEntryId of second message", entries[2])
	}

	// File was rewritten at v3 with the hookMessage role renamed.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `"version":3`) {
		t.Fatalf("rewritten file = %s, want version 3", data)
	}
	if strings.Contains(string(data), "hookMessage") {
		t.Fatalf("rewritten file still contains hookMessage role: %s", data)
	}

	// Re-open: now a plain v3 file.
	if _, err := openSession(path); err != nil {
		t.Fatalf("re-open migrated session: %v", err)
	}
}
