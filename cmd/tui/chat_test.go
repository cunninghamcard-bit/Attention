package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestAssistantBodyUsesReadableMarkdownStyle(t *testing.T) {
	chat := NewChatModel(newMarkdownRenderer(80))
	chat.Width = 80
	chat.Messages = []message{{
		role:    "assistant",
		content: "plain body",
	}}

	out := chat.RenderMessages(false)
	if !strings.Contains(ansi.Strip(out), "plain body") {
		t.Fatalf("rendered output missing assistant body: %q", out)
	}
	if !strings.Contains(out, "\x1b[38;2;169;177;214mplain") {
		t.Fatalf("assistant body did not use readable tokyo-night text color: %q", out)
	}
}
