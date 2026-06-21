package session

import (
	"encoding/json"
	"fmt"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

func (e SessionEntry) MarshalJSON() ([]byte, error) {
	if e.Type == "" && len(e.Raw) > 0 {
		return append([]byte(nil), e.Raw...), nil
	}

	switch e.Type {
	case "message":
		type messageEntry struct {
			Type      string               `json:"type"`
			ID        EntryID              `json:"id"`
			ParentID  *EntryID             `json:"parentId"`
			Timestamp string               `json:"timestamp"`
			Message   message.AgentMessage `json:"message"`
		}
		return json.Marshal(messageEntry{
			Type:      e.Type,
			ID:        e.ID,
			ParentID:  copyEntryIDPtr(e.ParentID),
			Timestamp: e.Timestamp,
			Message:   e.Message,
		})
	case "thinking_level_change":
		type thinkingLevelEntry struct {
			Type          string   `json:"type"`
			ID            EntryID  `json:"id"`
			ParentID      *EntryID `json:"parentId"`
			Timestamp     string   `json:"timestamp"`
			ThinkingLevel string   `json:"thinkingLevel"`
		}
		return json.Marshal(thinkingLevelEntry{
			Type:          e.Type,
			ID:            e.ID,
			ParentID:      copyEntryIDPtr(e.ParentID),
			Timestamp:     e.Timestamp,
			ThinkingLevel: e.Level,
		})
	case "model_change":
		type modelEntry struct {
			Type      string   `json:"type"`
			ID        EntryID  `json:"id"`
			ParentID  *EntryID `json:"parentId"`
			Timestamp string   `json:"timestamp"`
			Provider  string   `json:"provider"`
			ModelID   string   `json:"modelId"`
		}
		return json.Marshal(modelEntry{
			Type:      e.Type,
			ID:        e.ID,
			ParentID:  copyEntryIDPtr(e.ParentID),
			Timestamp: e.Timestamp,
			Provider:  e.Provider,
			ModelID:   e.ModelID,
		})
	case "compaction":
		type compactionEntry struct {
			Type             string   `json:"type"`
			ID               EntryID  `json:"id"`
			ParentID         *EntryID `json:"parentId"`
			Timestamp        string   `json:"timestamp"`
			Summary          string   `json:"summary"`
			FirstKeptEntryID EntryID  `json:"firstKeptEntryId"`
			TokensBefore     int      `json:"tokensBefore"`
			Details          any      `json:"details,omitempty"`
			FromHook         bool     `json:"fromHook,omitempty"`
		}
		return json.Marshal(compactionEntry{
			Type:             e.Type,
			ID:               e.ID,
			ParentID:         copyEntryIDPtr(e.ParentID),
			Timestamp:        e.Timestamp,
			Summary:          e.Summary,
			FirstKeptEntryID: e.FirstKeptEntryID,
			TokensBefore:     e.TokensBefore,
			Details:          e.Details,
			FromHook:         e.FromHook,
		})
	case "branch_summary":
		type branchSummaryEntry struct {
			Type      string   `json:"type"`
			ID        EntryID  `json:"id"`
			ParentID  *EntryID `json:"parentId"`
			Timestamp string   `json:"timestamp"`
			FromID    string   `json:"fromId"`
			Summary   string   `json:"summary"`
			Details   any      `json:"details,omitempty"`
			FromHook  bool     `json:"fromHook,omitempty"`
		}
		fromID := "root"
		if e.FromID != nil {
			fromID = string(*e.FromID)
		}
		return json.Marshal(branchSummaryEntry{
			Type:      e.Type,
			ID:        e.ID,
			ParentID:  copyEntryIDPtr(e.ParentID),
			Timestamp: e.Timestamp,
			FromID:    fromID,
			Summary:   e.Summary,
			Details:   e.Details,
			FromHook:  e.FromHook,
		})
	case "custom":
		type customEntry struct {
			Type       string   `json:"type"`
			ID         EntryID  `json:"id"`
			ParentID   *EntryID `json:"parentId"`
			Timestamp  string   `json:"timestamp"`
			CustomType string   `json:"customType"`
			Data       any      `json:"data,omitempty"`
		}
		return json.Marshal(customEntry{
			Type:       e.Type,
			ID:         e.ID,
			ParentID:   copyEntryIDPtr(e.ParentID),
			Timestamp:  e.Timestamp,
			CustomType: e.CustomType,
			Data:       e.Data,
		})
	case "custom_message":
		type customMessageEntry struct {
			Type       string   `json:"type"`
			ID         EntryID  `json:"id"`
			ParentID   *EntryID `json:"parentId"`
			Timestamp  string   `json:"timestamp"`
			CustomType string   `json:"customType"`
			Content    any      `json:"content"`
			Display    bool     `json:"display"`
			Details    any      `json:"details,omitempty"`
		}
		return json.Marshal(customMessageEntry{
			Type:       e.Type,
			ID:         e.ID,
			ParentID:   copyEntryIDPtr(e.ParentID),
			Timestamp:  e.Timestamp,
			CustomType: e.CustomType,
			Content:    e.Content,
			Display:    e.Display,
			Details:    e.Details,
		})
	case "label":
		type labelEntry struct {
			Type      string   `json:"type"`
			ID        EntryID  `json:"id"`
			ParentID  *EntryID `json:"parentId"`
			Timestamp string   `json:"timestamp"`
			TargetID  *EntryID `json:"targetId"`
			Label     string   `json:"label"`
		}
		return json.Marshal(labelEntry{
			Type:      e.Type,
			ID:        e.ID,
			ParentID:  copyEntryIDPtr(e.ParentID),
			Timestamp: e.Timestamp,
			TargetID:  copyEntryIDPtr(e.TargetID),
			Label:     e.Label,
		})
	case "session_info":
		type sessionInfoEntry struct {
			Type      string   `json:"type"`
			ID        EntryID  `json:"id"`
			ParentID  *EntryID `json:"parentId"`
			Timestamp string   `json:"timestamp"`
			Name      string   `json:"name,omitempty"`
		}
		return json.Marshal(sessionInfoEntry{
			Type:      e.Type,
			ID:        e.ID,
			ParentID:  copyEntryIDPtr(e.ParentID),
			Timestamp: e.Timestamp,
			Name:      e.Name,
		})
	case "leaf":
		type leafEntry struct {
			Type      string   `json:"type"`
			ID        EntryID  `json:"id"`
			ParentID  *EntryID `json:"parentId"`
			Timestamp string   `json:"timestamp"`
			TargetID  *EntryID `json:"targetId"`
		}
		return json.Marshal(leafEntry{
			Type:      e.Type,
			ID:        e.ID,
			ParentID:  copyEntryIDPtr(e.ParentID),
			Timestamp: e.Timestamp,
			TargetID:  copyEntryIDPtr(e.TargetID),
		})
	default:
		if len(e.Raw) > 0 {
			return append([]byte(nil), e.Raw...), nil
		}
		return nil, fmt.Errorf("unknown session entry type %q", e.Type)
	}
}

func decodeEntry(line []byte, filePath string, lineNumber int) (SessionEntry, error) {
	var discriminator struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		ParentID  json.RawMessage `json:"parentId"`
		Timestamp string          `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &discriminator); err != nil {
		return SessionEntry{}, invalidEntry(filePath, lineNumber, "is not valid JSON", err)
	}
	if discriminator.Type == "" {
		return SessionEntry{}, invalidEntry(filePath, lineNumber, "is missing entry type", nil)
	}
	if discriminator.ID == "" {
		return SessionEntry{}, invalidEntry(filePath, lineNumber, "is missing entry id", nil)
	}
	if len(discriminator.ParentID) == 0 {
		return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid parentId", nil)
	}
	if discriminator.Timestamp == "" {
		return SessionEntry{}, invalidEntry(filePath, lineNumber, "is missing timestamp", nil)
	}

	entry := SessionEntry{
		Type:      discriminator.Type,
		ID:        EntryID(discriminator.ID),
		Timestamp: discriminator.Timestamp,
		Raw:       append(json.RawMessage(nil), line...),
	}
	parentID, err := decodeOptionalEntryID(discriminator.ParentID)
	if err != nil {
		return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid parentId", err)
	}
	entry.ParentID = parentID

	switch discriminator.Type {
	case "message":
		var payload struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid message", err)
		}
		entry.Message = decodeAgentMessage(payload.Message)
	case "thinking_level_change":
		var payload struct {
			Level string `json:"thinkingLevel"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid thinkingLevel", err)
		}
		entry.Level = payload.Level
	case "model_change":
		var payload struct {
			Provider string `json:"provider"`
			ModelID  string `json:"modelId"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid model change", err)
		}
		entry.Provider = payload.Provider
		entry.ModelID = payload.ModelID
	case "compaction":
		var payload struct {
			Summary          string  `json:"summary"`
			FirstKeptEntryID EntryID `json:"firstKeptEntryId"`
			TokensBefore     int     `json:"tokensBefore"`
			Details          any     `json:"details"`
			FromHook         bool    `json:"fromHook"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid compaction", err)
		}
		entry.Summary = payload.Summary
		entry.FirstKeptEntryID = payload.FirstKeptEntryID
		entry.TokensBefore = payload.TokensBefore
		entry.Details = payload.Details
		entry.FromHook = payload.FromHook
	case "branch_summary":
		var payload struct {
			FromID   string `json:"fromId"`
			Summary  string `json:"summary"`
			Details  any    `json:"details"`
			FromHook bool   `json:"fromHook"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid branch summary", err)
		}
		entry.FromID = fromIDFromJSON(payload.FromID)
		entry.Summary = payload.Summary
		entry.Details = payload.Details
		entry.FromHook = payload.FromHook
	case "custom":
		var payload struct {
			CustomType string `json:"customType"`
			Data       any    `json:"data"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid custom entry", err)
		}
		entry.CustomType = payload.CustomType
		entry.Data = payload.Data
	case "custom_message":
		var payload struct {
			CustomType string `json:"customType"`
			Content    any    `json:"content"`
			Display    bool   `json:"display"`
			Details    any    `json:"details"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid custom message", err)
		}
		entry.CustomType = payload.CustomType
		entry.Content = payload.Content
		entry.Display = payload.Display
		entry.Details = payload.Details
	case "label":
		var payload struct {
			TargetID EntryID `json:"targetId"`
			Label    string  `json:"label"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid label", err)
		}
		entry.TargetID = copyEntryIDPtr(&payload.TargetID)
		entry.Label = payload.Label
	case "session_info":
		var payload struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid session info", err)
		}
		entry.Name = payload.Name
	case "leaf":
		var payload struct {
			TargetID json.RawMessage `json:"targetId"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid leaf", err)
		}
		targetID, err := decodeOptionalEntryID(payload.TargetID)
		if err != nil {
			return SessionEntry{}, invalidEntry(filePath, lineNumber, "has invalid targetId", err)
		}
		entry.TargetID = targetID
	default:
		return entry, nil
	}

	return entry, nil
}

func decodeAgentMessage(raw json.RawMessage) message.AgentMessage {
	if len(raw) == 0 {
		return message.RawMessage{Raw: json.RawMessage("null")}
	}

	var discriminator struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return message.RawMessage{Raw: append(json.RawMessage(nil), raw...)}
	}

	switch discriminator.Role {
	case string(ai.RoleUser), string(ai.RoleAssistant), string(ai.RoleToolResult):
		var msg ai.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			return message.RawMessage{Raw: append(json.RawMessage(nil), raw...)}
		}
		return msg
	case "bashExecution":
		var msg message.BashExecutionMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return message.RawMessage{Raw: append(json.RawMessage(nil), raw...)}
		}
		return msg
	case "custom":
		var msg message.CustomMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return message.RawMessage{Raw: append(json.RawMessage(nil), raw...)}
		}
		return msg
	case "branchSummary":
		var msg message.BranchSummaryMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return message.RawMessage{Raw: append(json.RawMessage(nil), raw...)}
		}
		return msg
	case "compactionSummary":
		var msg message.CompactionSummaryMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return message.RawMessage{Raw: append(json.RawMessage(nil), raw...)}
		}
		return msg
	default:
		return message.RawMessage{Raw: append(json.RawMessage(nil), raw...)}
	}
}

func decodeOptionalEntryID(raw json.RawMessage) (*EntryID, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	entryID := EntryID(id)
	return &entryID, nil
}

func fromIDFromJSON(value string) *EntryID {
	if value == "root" {
		return nil
	}
	id := EntryID(value)
	return &id
}
