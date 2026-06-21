package ai

import (
	"strings"
	"time"
)

const (
	nonVisionUserImagePlaceholder = "(image omitted: model does not support images)"
	nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"
)

type NormalizeToolCallIDFunc func(id string, model Model, source Message) string

func TransformMessages(messages []Message, model Model, normalizeToolCallID NormalizeToolCallIDFunc) []Message {
	toolCallIDMap := map[string]string{}
	transformed := make([]Message, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case RoleUser:
			if !ModelSupportsInput(model, InputImage) {
				if content, changed := replaceImagesWithPlaceholder(
					msg.Content,
					nonVisionUserImagePlaceholder,
				); changed {
					msg.Content = content
				}
			}
			transformed = append(transformed, msg)
		case RoleToolResult:
			if normalizedID, ok := toolCallIDMap[msg.ToolCallID]; ok && normalizedID != msg.ToolCallID {
				msg.ToolCallID = normalizedID
			}
			if !ModelSupportsInput(model, InputImage) {
				if content, changed := replaceImagesWithPlaceholder(
					msg.Content,
					nonVisionToolImagePlaceholder,
				); changed {
					msg.Content = content
				}
			}
			transformed = append(transformed, msg)
		case RoleAssistant:
			isSameModel := msg.Provider == model.Provider && msg.API == model.API && msg.Model == model.ID
			if content, changed := transformAssistantContent(
				msg,
				model,
				isSameModel,
				normalizeToolCallID,
				toolCallIDMap,
			); changed {
				msg.Content = content
			}
			transformed = append(transformed, msg)
		default:
			transformed = append(transformed, msg)
		}
	}

	return insertSyntheticToolResults(transformed)
}

func replaceImagesWithPlaceholder(content []ContentBlock, placeholder string) ([]ContentBlock, bool) {
	var result []ContentBlock
	previousWasPlaceholder := false

	for i, block := range content {
		if block.Type == ContentImage {
			if result == nil {
				result = make([]ContentBlock, 0, len(content))
				result = append(result, content[:i]...)
			}
			if !previousWasPlaceholder {
				result = append(result, ContentBlock{Type: ContentText, Text: placeholder})
			}
			previousWasPlaceholder = true
			continue
		}

		if result != nil {
			result = append(result, block)
		}
		previousWasPlaceholder = block.Type == ContentText && block.Text == placeholder
	}

	if result == nil {
		return content, false
	}
	return result, true
}

func transformAssistantContent(
	message Message,
	model Model,
	isSameModel bool,
	normalizeToolCallID NormalizeToolCallIDFunc,
	toolCallIDMap map[string]string,
) ([]ContentBlock, bool) {
	var result []ContentBlock
	appendBlock := func(i int, block ContentBlock) {
		if result == nil {
			result = make([]ContentBlock, 0, len(message.Content))
			result = append(result, message.Content[:i]...)
		}
		result = append(result, block)
	}
	dropBlock := func(i int) {
		if result == nil {
			result = make([]ContentBlock, 0, len(message.Content))
			result = append(result, message.Content[:i]...)
		}
	}

	for i, block := range message.Content {
		switch block.Type {
		case ContentThinking:
			switch {
			case block.Redacted:
				if isSameModel {
					if result != nil {
						result = append(result, block)
					}
				} else {
					dropBlock(i)
				}
			case isSameModel && block.ThinkingSignature != "":
				if result != nil {
					result = append(result, block)
				}
			case strings.TrimSpace(block.Thinking) == "":
				dropBlock(i)
			case isSameModel:
				if result != nil {
					result = append(result, block)
				}
			default:
				appendBlock(i, ContentBlock{Type: ContentText, Text: block.Thinking})
			}
		case ContentText:
			if isSameModel {
				if result != nil {
					result = append(result, block)
				}
			} else {
				appendBlock(i, ContentBlock{Type: ContentText, Text: block.Text})
			}
		case ContentToolCall:
			toolCall := block
			changed := false
			if !isSameModel {
				changed = toolCall.ThoughtSignature != ""
				toolCall.ThoughtSignature = ""
			}
			if !isSameModel && normalizeToolCallID != nil {
				normalizedID := normalizeToolCallID(block.ToolCallID, model, message)
				if normalizedID != block.ToolCallID {
					toolCallIDMap[block.ToolCallID] = normalizedID
					toolCall.ToolCallID = normalizedID
					changed = true
				}
			}
			if changed {
				appendBlock(i, toolCall)
			} else if result != nil {
				result = append(result, block)
			}
		default:
			if result != nil {
				result = append(result, block)
			}
		}
	}

	if result == nil {
		return message.Content, false
	}
	return result, true
}

func insertSyntheticToolResults(messages []Message) []Message {
	result := make([]Message, 0, len(messages))
	pendingToolCalls := []ContentBlock{}
	existingToolResultIDs := map[string]bool{}

	insertSynthetic := func() {
		for _, toolCall := range pendingToolCalls {
			if existingToolResultIDs[toolCall.ToolCallID] {
				continue
			}
			result = append(result, Message{
				Role:       RoleToolResult,
				ToolCallID: toolCall.ToolCallID,
				ToolName:   toolCall.ToolName,
				Content: []ContentBlock{
					{Type: ContentText, Text: "No result provided"},
				},
				IsError:   true,
				Timestamp: time.Now().UnixMilli(),
			})
		}
		pendingToolCalls = nil
		existingToolResultIDs = map[string]bool{}
	}

	for _, msg := range messages {
		switch msg.Role {
		case RoleAssistant:
			insertSynthetic()
			if msg.StopReason == StopReasonError || msg.StopReason == StopReasonAborted {
				continue
			}
			pendingToolCalls = nil
			for _, block := range msg.Content {
				if block.Type == ContentToolCall {
					pendingToolCalls = append(pendingToolCalls, block)
				}
			}
			result = append(result, msg)
		case RoleToolResult:
			existingToolResultIDs[msg.ToolCallID] = true
			result = append(result, msg)
		case RoleUser:
			insertSynthetic()
			result = append(result, msg)
		default:
			result = append(result, msg)
		}
	}
	insertSynthetic()

	return result
}
