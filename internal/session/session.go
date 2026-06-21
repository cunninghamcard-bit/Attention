package session

import (
	"context"
	"strings"
	"sync"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

type Session struct {
	mu      sync.Mutex
	storage SessionStorage
}

func NewSession(storage SessionStorage) *Session {
	return &Session{storage: storage}
}

func (s *Session) GetStorage() SessionStorage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage
}

func (s *Session) AppendMessage(ctx context.Context, msg message.AgentMessage) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}
	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:      "message",
		ID:        id,
		ParentID:  leafID,
		Timestamp: newTimestamp(),
		Message:   message.Snapshot(msg),
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) AppendModelChange(
	ctx context.Context,
	provider string,
	modelID string,
) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}
	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:      "model_change",
		ID:        id,
		ParentID:  leafID,
		Timestamp: newTimestamp(),
		Provider:  provider,
		ModelID:   modelID,
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) AppendThinkingLevelChange(ctx context.Context, level string) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}
	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:      "thinking_level_change",
		ID:        id,
		ParentID:  leafID,
		Timestamp: newTimestamp(),
		Level:     level,
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) AppendCompaction(
	ctx context.Context,
	summary string,
	firstKeptEntryID EntryID,
	tokensBefore int,
	details any,
	fromHook bool,
) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}
	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:             "compaction",
		ID:               id,
		ParentID:         leafID,
		Timestamp:        newTimestamp(),
		Summary:          summary,
		FirstKeptEntryID: firstKeptEntryID,
		TokensBefore:     tokensBefore,
		Details:          details,
		FromHook:         fromHook,
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) AppendCustomEntry(ctx context.Context, customType string, data any) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}
	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:       "custom",
		ID:         id,
		ParentID:   leafID,
		Timestamp:  newTimestamp(),
		CustomType: customType,
		Data:       data,
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) AppendCustomMessageEntry(
	ctx context.Context,
	customType string,
	content any,
	display bool,
	details any,
) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}
	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:       "custom_message",
		ID:         id,
		ParentID:   leafID,
		Timestamp:  newTimestamp(),
		CustomType: customType,
		Content:    content,
		Display:    display,
		Details:    details,
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) AppendLabel(ctx context.Context, targetID EntryID, label string) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.storage.GetEntry(targetID); !ok {
		return "", sessionError(ErrorNotFound, "Entry "+string(targetID)+" not found")
	}
	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}

	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:      "label",
		ID:        id,
		ParentID:  leafID,
		Timestamp: newTimestamp(),
		TargetID:  copyEntryIDPtr(&targetID),
		Label:     label,
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) AppendSessionName(ctx context.Context, name string) (EntryID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return "", err
	}
	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:      "session_info",
		ID:        id,
		ParentID:  leafID,
		Timestamp: newTimestamp(),
		Name:      strings.TrimSpace(name),
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) MoveTo(
	ctx context.Context,
	entryID *EntryID,
	summary *BranchSummary,
) (*EntryID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID != nil {
		if _, ok := s.storage.GetEntry(*entryID); !ok {
			return nil, sessionError(ErrorNotFound, "Entry "+string(*entryID)+" not found")
		}
	}
	if err := s.storage.SetLeafID(copyEntryIDPtr(entryID)); err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, nil
	}

	id := s.storage.CreateEntryID()
	entry := SessionEntry{
		Type:      "branch_summary",
		ID:        id,
		ParentID:  copyEntryIDPtr(entryID),
		Timestamp: newTimestamp(),
		FromID:    copyEntryIDPtr(entryID),
		Summary:   summary.Summary,
		Details:   summary.Details,
		FromHook:  summary.FromHook,
	}
	if err := s.storage.AppendEntry(entry); err != nil {
		return nil, err
	}
	return copyEntryIDPtr(&id), nil
}

func (s *Session) BuildContext(ctx context.Context) (Context, error) {
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	leafID, err := s.storage.GetLeafID()
	if err != nil {
		return Context{}, err
	}
	path, err := s.storage.GetPathToRoot(leafID)
	if err != nil {
		return Context{}, err
	}
	return buildContextFromPath(path), nil
}

func (s *Session) GetMetadata() Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.GetMetadata()
}

func (s *Session) GetLeafID() (*EntryID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.GetLeafID()
}

func (s *Session) GetEntry(id EntryID) (SessionEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.GetEntry(id)
}

func (s *Session) GetEntries() []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.GetEntries()
}

func (s *Session) GetBranch(fromID *EntryID) ([]SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	leafID := copyEntryIDPtr(fromID)
	if leafID == nil {
		var err error
		leafID, err = s.storage.GetLeafID()
		if err != nil {
			return nil, err
		}
	}
	return s.storage.GetPathToRoot(leafID)
}

func (s *Session) GetLabel(id EntryID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.GetLabel(id)
}

func (s *Session) GetSessionName() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := s.storage.FindEntries("session_info")
	if len(entries) == 0 {
		return "", false
	}
	name := strings.TrimSpace(entries[len(entries)-1].Name)
	if name == "" {
		return "", false
	}
	return name, true
}

func buildContextFromPath(path []SessionEntry) Context {
	ctx := Context{
		Messages:      []message.AgentMessage{},
		ThinkingLevel: "off",
	}
	var compaction *SessionEntry
	for i := range path {
		entry := path[i]
		switch entry.Type {
		case "thinking_level_change":
			ctx.ThinkingLevel = entry.Level
		case "model_change":
			ctx.Model = &ModelRef{Provider: entry.Provider, ModelID: entry.ModelID}
		case "message":
			if msg, ok := message.AsAIMessage(entry.Message); ok && msg.Role == ai.RoleAssistant {
				ctx.Model = &ModelRef{Provider: msg.Provider, ModelID: msg.Model}
			}
		case "compaction":
			entryCopy := entry
			compaction = &entryCopy
		}
	}

	if compaction == nil {
		for _, entry := range path {
			appendContextMessage(&ctx.Messages, entry)
		}
		return ctx
	}

	ctx.Messages = append(
		ctx.Messages,
		message.CreateCompactionSummaryMessage(
			compaction.Summary,
			compaction.TokensBefore,
			compaction.Timestamp,
		),
	)

	compactionIndex := -1
	for i, entry := range path {
		if entry.Type == "compaction" && entry.ID == compaction.ID {
			compactionIndex = i
			break
		}
	}
	foundFirstKept := false
	for i := 0; i < compactionIndex; i++ {
		entry := path[i]
		if entry.ID == compaction.FirstKeptEntryID {
			foundFirstKept = true
		}
		if foundFirstKept {
			appendContextMessage(&ctx.Messages, entry)
		}
	}
	for i := compactionIndex + 1; i < len(path); i++ {
		appendContextMessage(&ctx.Messages, path[i])
	}
	return ctx
}

func appendContextMessage(messages *[]message.AgentMessage, entry SessionEntry) {
	switch entry.Type {
	case "message":
		*messages = append(*messages, message.Snapshot(entry.Message))
	case "custom_message":
		*messages = append(
			*messages,
			message.CreateCustomMessage(
				entry.CustomType,
				entry.Content,
				entry.Display,
				entry.Details,
				entry.Timestamp,
			),
		)
	case "branch_summary":
		if entry.Summary == "" {
			return
		}
		fromID := "root"
		if entry.FromID != nil {
			fromID = string(*entry.FromID)
		}
		*messages = append(
			*messages,
			message.CreateBranchSummaryMessage(entry.Summary, fromID, entry.Timestamp),
		)
	}
}

func copyEntryIDPtr(id *EntryID) *EntryID {
	if id == nil {
		return nil
	}
	copied := *id
	return &copied
}
