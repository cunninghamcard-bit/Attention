package ai

import "testing"

func TestTransformMessagesDowngradesImagesForTextOnlyModel(t *testing.T) {
	model := Model{
		ID:       "text-only-test",
		API:      APIOpenAICodexResponses,
		Provider: "openai-codex",
		Input:    []InputCapability{InputText},
	}
	messages := []Message{
		{
			Role: RoleUser,
			Content: []ContentBlock{
				{Type: ContentText, Text: "before"},
				{Type: ContentImage, ImageData: "one", MimeType: "image/png"},
				{Type: ContentImage, ImageData: "two", MimeType: "image/png"},
				{Type: ContentText, Text: "after"},
			},
		},
	}

	got := TransformMessages(messages, model, nil)
	content := got[0].Content

	if len(content) != 3 {
		t.Fatalf("content length = %d, want 3: %+v", len(content), content)
	}
	if content[1].Type != ContentText || content[1].Text != nonVisionUserImagePlaceholder {
		t.Fatalf("image placeholder = %+v", content[1])
	}
	if messages[0].Content[1].Type != ContentImage {
		t.Fatal("TransformMessages mutated input")
	}
}

func TestTransformMessagesCrossModelThinkingAndToolCall(t *testing.T) {
	target, _ := GetModel("", "claude-sonnet-4-5")
	messages := []Message{
		{
			Role:      RoleAssistant,
			API:       APIOpenAIResponses,
			Provider:  "openai",
			Model:     "gpt-5",
			Timestamp: 1,
			Content: []ContentBlock{
				{Type: ContentThinking, Thinking: "reason", ThinkingSignature: "sig"},
				{Type: ContentText, Text: "answer", TextSignature: "text-sig"},
				{Type: ContentToolCall, ToolCallID: "openai|long", ToolName: "run", ThoughtSignature: "thought"},
			},
		},
		{
			Role:       RoleToolResult,
			ToolCallID: "openai|long",
			ToolName:   "run",
			Content:    []ContentBlock{{Type: ContentText, Text: "ok"}},
		},
	}

	got := TransformMessages(messages, target, func(id string, model Model, source Message) string {
		return "normalized"
	})

	assistant := got[0]
	if assistant.Content[0].Type != ContentText || assistant.Content[0].Text != "reason" {
		t.Fatalf("thinking block was not converted to text: %+v", assistant.Content[0])
	}
	if assistant.Content[1].TextSignature != "" {
		t.Fatalf("cross-model text signature preserved: %+v", assistant.Content[1])
	}
	if assistant.Content[2].ToolCallID != "normalized" || assistant.Content[2].ThoughtSignature != "" {
		t.Fatalf("tool call not normalized: %+v", assistant.Content[2])
	}
	if got[1].ToolCallID != "normalized" {
		t.Fatalf("tool result id = %q, want normalized", got[1].ToolCallID)
	}
}

func TestTransformMessagesSyntheticToolResultAndErroredAssistant(t *testing.T) {
	model, _ := GetModel("", "claude-sonnet-4-5")
	messages := []Message{
		{
			Role:       RoleAssistant,
			API:        model.API,
			Provider:   model.Provider,
			Model:      model.ID,
			StopReason: StopReasonError,
			Content:    []ContentBlock{{Type: ContentText, Text: "partial"}},
		},
		{
			Role:       RoleAssistant,
			API:        model.API,
			Provider:   model.Provider,
			Model:      model.ID,
			StopReason: StopReasonToolUse,
			Content:    []ContentBlock{{Type: ContentToolCall, ToolCallID: "call-1", ToolName: "search"}},
		},
		{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "next"}}},
	}

	got := TransformMessages(messages, model, nil)

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	if got[0].Role != RoleAssistant || got[0].Content[0].Type != ContentToolCall {
		t.Fatalf("errored assistant was not skipped or valid assistant missing: %+v", got[0])
	}
	if got[1].Role != RoleToolResult || got[1].ToolCallID != "call-1" || !got[1].IsError {
		t.Fatalf("synthetic tool result missing: %+v", got[1])
	}
	if got[2].Role != RoleUser {
		t.Fatalf("user message position changed: %+v", got[2])
	}
}
