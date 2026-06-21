package message

import (
	"encoding/json"
	"fmt"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

const (
	CompactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	CompactionSummarySuffix = "\n</summary>"
	BranchSummaryPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	BranchSummarySuffix     = "</summary>"
)

// AgentMessage is the transcript-level union of LLM messages and custom
// runtime messages.
type AgentMessage interface {
	IsAgentMessage()
}

// CustomAgentMessage is the extension point for app-specific transcript
// messages.
type CustomAgentMessage interface {
	AgentMessage
	IsCustomAgentMessage()
}

// DefaultConvertToLLM projects transcript messages into provider messages.
// Unknown custom messages are runtime-only and are not sent to the model.
func DefaultConvertToLLM(messages []AgentMessage) ([]ai.Message, error) {
	filtered := make([]ai.Message, 0, len(messages))
	for _, message := range messages {
		switch msg := message.(type) {
		case BashExecutionMessage:
			if msg.ExcludeFromContext {
				continue
			}
			filtered = append(filtered, ai.Message{
				Role:      ai.RoleUser,
				Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: BashExecutionToText(msg)}},
				Timestamp: msg.Timestamp,
			})
		case *BashExecutionMessage:
			if msg == nil || msg.ExcludeFromContext {
				continue
			}
			filtered = append(filtered, ai.Message{
				Role:      ai.RoleUser,
				Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: BashExecutionToText(*msg)}},
				Timestamp: msg.Timestamp,
			})
		case CustomMessage:
			llm, err := customToLLM(msg)
			if err != nil {
				return nil, err
			}
			filtered = append(filtered, llm)
		case *CustomMessage:
			if msg == nil {
				continue
			}
			llm, err := customToLLM(*msg)
			if err != nil {
				return nil, err
			}
			filtered = append(filtered, llm)
		case BranchSummaryMessage:
			filtered = append(filtered, ai.Message{
				Role: ai.RoleUser,
				Content: []ai.ContentBlock{{
					Type: ai.ContentText,
					Text: BranchSummaryPrefix + msg.Summary + BranchSummarySuffix,
				}},
				Timestamp: msg.Timestamp,
			})
		case *BranchSummaryMessage:
			if msg == nil {
				continue
			}
			filtered = append(filtered, ai.Message{
				Role: ai.RoleUser,
				Content: []ai.ContentBlock{{
					Type: ai.ContentText,
					Text: BranchSummaryPrefix + msg.Summary + BranchSummarySuffix,
				}},
				Timestamp: msg.Timestamp,
			})
		case CompactionSummaryMessage:
			filtered = append(filtered, ai.Message{
				Role: ai.RoleUser,
				Content: []ai.ContentBlock{{
					Type: ai.ContentText,
					Text: CompactionSummaryPrefix + msg.Summary + CompactionSummarySuffix,
				}},
				Timestamp: msg.Timestamp,
			})
		case *CompactionSummaryMessage:
			if msg == nil {
				continue
			}
			filtered = append(filtered, ai.Message{
				Role: ai.RoleUser,
				Content: []ai.ContentBlock{{
					Type: ai.ContentText,
					Text: CompactionSummaryPrefix + msg.Summary + CompactionSummarySuffix,
				}},
				Timestamp: msg.Timestamp,
			})
		default:
			aiMsg, ok := AsAIMessage(message)
			if !ok {
				continue
			}
			if aiMsg.Role != ai.RoleUser && aiMsg.Role != ai.RoleAssistant && aiMsg.Role != ai.RoleToolResult {
				continue
			}
			filtered = append(filtered, aiMsg)
		}
	}
	return filtered, nil
}

// AsAIMessage snapshots an AgentMessage when it is backed by ai.Message.
func AsAIMessage(message AgentMessage) (ai.Message, bool) {
	switch msg := message.(type) {
	case ai.Message:
		return msg, true
	case *ai.Message:
		if msg == nil {
			return ai.Message{}, false
		}
		return *msg, true
	default:
		return ai.Message{}, false
	}
}

// Snapshot converts mutable message pointers into value messages before they
// enter a transcript slice owned by another component.
func Snapshot(message AgentMessage) AgentMessage {
	if msg, ok := AsAIMessage(message); ok {
		return msg
	}
	return message
}

func customToLLM(msg CustomMessage) (ai.Message, error) {
	content, err := CustomContentBlocks(msg.Content)
	if err != nil {
		return ai.Message{}, err
	}
	return ai.Message{
		Role:      ai.RoleUser,
		Content:   content,
		Timestamp: msg.Timestamp,
	}, nil
}

func CustomContentBlocks(content any) ([]ai.ContentBlock, error) {
	switch value := content.(type) {
	case string:
		return []ai.ContentBlock{{Type: ai.ContentText, Text: value}}, nil
	case []ai.ContentBlock:
		return append([]ai.ContentBlock(nil), value...), nil
	case []any:
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var blocks []ai.ContentBlock
		if err := json.Unmarshal(data, &blocks); err != nil {
			return nil, err
		}
		return blocks, nil
	case nil:
		return []ai.ContentBlock{}, nil
	default:
		return nil, fmt.Errorf("custom message content has unsupported type %T", content)
	}
}
