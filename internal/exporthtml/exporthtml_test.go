package exporthtml

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestRenderEmbedsTemplateAssetsAndSessionData(t *testing.T) {
	rootID := session.EntryID("root-entry")
	leafID := session.EntryID("leaf-entry")

	html := Render(SessionData{
		Header: SessionHeader{
			Type:      "session",
			Version:   3,
			ID:        "session-1",
			Timestamp: "2026-06-04T01:02:03Z",
			CWD:       "/workspace",
		},
		Entries: []session.SessionEntry{
			{
				Type:      "message",
				ID:        rootID,
				Timestamp: "2026-06-04T01:02:04Z",
				Message: ai.Message{
					Role:    ai.RoleUser,
					Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "hello export"}},
				},
			},
			{
				Type:      "message",
				ID:        leafID,
				ParentID:  &rootID,
				Timestamp: "2026-06-04T01:02:05Z",
				Message: ai.Message{
					Role: ai.RoleAssistant,
					Content: []ai.ContentBlock{{
						Type:       ai.ContentToolCall,
						ToolCallID: "call-1",
						ToolName:   "bash",
						Arguments:  map[string]any{"command": "echo ok"},
					}},
				},
			},
		},
		LeafID:       &leafID,
		SystemPrompt: "system prompt",
		Tools: []ToolDefinition{
			{
				Name:        "bash",
				Description: "Run shell commands",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "Command to run",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		RenderedTools: map[string]RenderedTool{
			"call-1": {
				CallBlocks: []render.Block{
					render.Text(`grep "hello"`),
				},
				ResultBlocks: []render.Block{
					render.Group("a.txt", []render.Block{
						render.Text("2: hello export"),
					}),
				},
			},
		},
	}, Options{})

	assertContains(t, html, `<script id="session-data" type="application/json">`)
	assertContains(t, html, "const base64 = document.getElementById('session-data').textContent;")
	assertContains(t, html, "marked v15.0.4")
	assertContains(t, html, "Highlight.js v11.9.0")

	sessionData := decodeEmbeddedSessionData(t, html)
	if sessionData.Header.ID != "session-1" {
		t.Fatalf("header.id = %q, want session-1", sessionData.Header.ID)
	}
	if len(sessionData.Entries) != 2 {
		t.Fatalf("entries length = %d, want 2", len(sessionData.Entries))
	}
	if sessionData.LeafID == nil || *sessionData.LeafID != "leaf-entry" {
		t.Fatalf("leafId = %v, want leaf-entry", sessionData.LeafID)
	}
	if sessionData.SystemPrompt != "system prompt" {
		t.Fatalf("systemPrompt = %q, want system prompt", sessionData.SystemPrompt)
	}
	if len(sessionData.Tools) != 1 || sessionData.Tools[0].Name != "bash" {
		t.Fatalf("tools = %#v, want bash tool", sessionData.Tools)
	}
	rendered := sessionData.Rendered["call-1"]
	if len(rendered.CallBlocks) != 1 || rendered.CallBlocks[0].Text != `grep "hello"` {
		t.Fatalf("rendered callBlocks = %#v, want grep call", rendered.CallBlocks)
	}
	if len(rendered.ResultBlocks) != 1 {
		t.Fatalf("rendered resultBlocks = %#v, want one group", rendered.ResultBlocks)
	}
	if rendered.ResultBlocks[0].Label != "a.txt" {
		t.Fatalf("rendered group label = %q, want a.txt", rendered.ResultBlocks[0].Label)
	}

	message := sessionData.Entries[0]["message"].(map[string]any)
	content := message["content"].([]any)
	block := content[0].(map[string]any)
	if block["text"] != "hello export" {
		t.Fatalf("decoded first message text = %#v, want hello export", block["text"])
	}
}

func TestRenderKeepsUserContentInsideBase64SessionData(t *testing.T) {
	entryID := session.EntryID("entry-1")
	raw := `user <script>alert("x")</script> & <b>raw</b>`

	html := Render(SessionData{
		Header: SessionHeader{Type: "session", ID: "session-1"},
		Entries: []session.SessionEntry{
			{
				Type:      "message",
				ID:        entryID,
				Timestamp: "2026-06-04T01:02:04Z",
				Message: ai.Message{
					Role:    ai.RoleUser,
					Content: []ai.ContentBlock{{Type: ai.ContentText, Text: raw}},
				},
			},
		},
		LeafID: &entryID,
	}, Options{})

	if strings.Contains(html, raw) {
		t.Fatalf("rendered raw user content outside base64 SESSION_DATA")
	}
	if strings.Contains(html, `<script>alert("x")</script>`) {
		t.Fatalf("rendered executable user script outside base64 SESSION_DATA")
	}
	if strings.Contains(html, `&lt;script&gt;alert`) {
		t.Fatalf("rendered escaped user content outside base64 SESSION_DATA")
	}

	sessionData := decodeEmbeddedSessionData(t, html)
	message := sessionData.Entries[0]["message"].(map[string]any)
	content := message["content"].([]any)
	block := content[0].(map[string]any)
	if block["text"] != raw {
		t.Fatalf("decoded text = %q, want raw user content", block["text"])
	}
}

type decodedSessionData struct {
	Header       SessionHeader           `json:"header"`
	Entries      []map[string]any        `json:"entries"`
	LeafID       *string                 `json:"leafId"`
	SystemPrompt string                  `json:"systemPrompt"`
	Tools        []ToolDefinition        `json:"tools"`
	Rendered     map[string]RenderedTool `json:"renderedTools"`
}

func decodeEmbeddedSessionData(t *testing.T, html string) decodedSessionData {
	t.Helper()

	const startMarker = `<script id="session-data" type="application/json">`
	const endMarker = `</script>`

	start := strings.Index(html, startMarker)
	if start == -1 {
		t.Fatal("missing session-data script")
	}
	start += len(startMarker)

	end := strings.Index(html[start:], endMarker)
	if end == -1 {
		t.Fatal("missing session-data closing script")
	}

	payload := html[start : start+end]
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode base64 SESSION_DATA: %v", err)
	}

	var sessionData decodedSessionData
	if err := json.Unmarshal(data, &sessionData); err != nil {
		t.Fatalf("unmarshal SESSION_DATA: %v", err)
	}
	return sessionData
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("rendered HTML missing %q", want)
	}
}
