package message

import (
	"encoding/json"
	"fmt"
	"time"
)

type BashExecutionMessage struct {
	Command            string `json:"command"`
	Output             string `json:"output"`
	ExitCode           *int   `json:"exitCode,omitempty"`
	Cancelled          bool   `json:"cancelled"`
	Truncated          bool   `json:"truncated"`
	FullOutputPath     string `json:"fullOutputPath,omitempty"`
	Timestamp          int64  `json:"timestamp"`
	ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`
}

func (BashExecutionMessage) IsAgentMessage()       {}
func (BashExecutionMessage) IsCustomAgentMessage() {}

func (m BashExecutionMessage) MarshalJSON() ([]byte, error) {
	type alias BashExecutionMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{
		Role:  "bashExecution",
		alias: alias(m),
	})
}

type CustomMessage struct {
	CustomType string `json:"customType"`
	Content    any    `json:"content"`
	Display    bool   `json:"display"`
	Details    any    `json:"details,omitempty"`
	Timestamp  int64  `json:"timestamp"`
}

func (CustomMessage) IsAgentMessage()       {}
func (CustomMessage) IsCustomAgentMessage() {}

func (m CustomMessage) MarshalJSON() ([]byte, error) {
	type alias CustomMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{
		Role:  "custom",
		alias: alias(m),
	})
}

type BranchSummaryMessage struct {
	Summary   string `json:"summary"`
	FromID    string `json:"fromId"`
	Timestamp int64  `json:"timestamp"`
}

func (BranchSummaryMessage) IsAgentMessage()       {}
func (BranchSummaryMessage) IsCustomAgentMessage() {}

func (m BranchSummaryMessage) MarshalJSON() ([]byte, error) {
	type alias BranchSummaryMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{
		Role:  "branchSummary",
		alias: alias(m),
	})
}

type CompactionSummaryMessage struct {
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore"`
	Timestamp    int64  `json:"timestamp"`
}

func (CompactionSummaryMessage) IsAgentMessage()       {}
func (CompactionSummaryMessage) IsCustomAgentMessage() {}

func (m CompactionSummaryMessage) MarshalJSON() ([]byte, error) {
	type alias CompactionSummaryMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{
		Role:  "compactionSummary",
		alias: alias(m),
	})
}

type RawMessage struct {
	Raw json.RawMessage
}

func (RawMessage) IsAgentMessage() {}

func (m RawMessage) MarshalJSON() ([]byte, error) {
	if len(m.Raw) == 0 {
		return []byte("null"), nil
	}
	return m.Raw, nil
}

func CreateCustomMessage(
	customType string,
	content any,
	display bool,
	details any,
	timestamp string,
) CustomMessage {
	return CustomMessage{
		CustomType: customType,
		Content:    content,
		Display:    display,
		Details:    details,
		Timestamp:  parseTimestampMillis(timestamp),
	}
}

func CreateBranchSummaryMessage(summary string, fromID string, timestamp string) BranchSummaryMessage {
	return BranchSummaryMessage{
		Summary:   summary,
		FromID:    fromID,
		Timestamp: parseTimestampMillis(timestamp),
	}
}

func CreateCompactionSummaryMessage(
	summary string,
	tokensBefore int,
	timestamp string,
) CompactionSummaryMessage {
	return CompactionSummaryMessage{
		Summary:      summary,
		TokensBefore: tokensBefore,
		Timestamp:    parseTimestampMillis(timestamp),
	}
}

func BashExecutionToText(msg BashExecutionMessage) string {
	text := fmt.Sprintf("Ran `%s`\n", msg.Command)
	if msg.Output != "" {
		text += fmt.Sprintf("```\n%s\n```", msg.Output)
	} else {
		text += "(no output)"
	}
	if msg.Cancelled {
		text += "\n\n(command cancelled)"
	} else if msg.ExitCode != nil && *msg.ExitCode != 0 {
		text += fmt.Sprintf("\n\nCommand exited with code %d", *msg.ExitCode)
	}
	if msg.Truncated && msg.FullOutputPath != "" {
		text += fmt.Sprintf("\n\n[Output truncated. Full output: %s]", msg.FullOutputPath)
	}
	return text
}

func parseTimestampMillis(timestamp string) int64 {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
