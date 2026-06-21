package ai

import (
	"encoding/json"
	"testing"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	msg := Message{
		Role:      RoleAssistant,
		Timestamp: 123,
		API:       APIAnthropicMessages,
		Provider:  "anthropic",
		Model:     "claude-sonnet-4-5",
		Usage: &Usage{
			Input:       10,
			Output:      20,
			CacheRead:   3,
			CacheWrite:  4,
			TotalTokens: 37,
			Cost:        &Cost{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Total: 10},
		},
		StopReason: StopReasonToolUse,
		Content: []ContentBlock{
			{Type: ContentText, Text: "hello", TextSignature: "txt-sig"},
			{Type: ContentThinking, Thinking: "plan", ThinkingSignature: "think-sig"},
			{Type: ContentImage, ImageData: "base64", MimeType: "image/png"},
			{
				Type:       ContentToolCall,
				ToolCallID: "tool-1",
				ToolName:   "echo",
				Arguments:  map[string]any{"text": "hi"},
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	if got.Role != RoleAssistant || got.API != APIAnthropicMessages || got.StopReason != StopReasonToolUse {
		t.Fatalf("round-trip lost message metadata: %+v", got)
	}
	if len(got.Content) != 4 {
		t.Fatalf("content length = %d, want 4", len(got.Content))
	}
	if got.Content[3].ToolCallID != "tool-1" || got.Content[3].Arguments["text"] != "hi" {
		t.Fatalf("tool call block did not round-trip: %+v", got.Content[3])
	}
}

func TestEventTypeZeroValueIsUnknown(t *testing.T) {
	var eventType EventType
	if eventType != EventUnknown {
		t.Fatalf("zero value = %d, want EventUnknown", eventType)
	}
	if EventTextStart == EventUnknown {
		t.Fatal("EventTextStart must not be the zero value")
	}
}
