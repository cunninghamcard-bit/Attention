package main

import (
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestInputCharacters(t *testing.T) {
	// Redirect stderr to avoid bubbletea debug output cluttering test output
	stderr := os.Stderr
	os.Stderr, _ = os.OpenFile("/dev/null", os.O_WRONLY, 0)
	defer func() { os.Stderr = stderr }()

	// Create InputModel
	im := NewInputModel(nil, nil, nil, nil, "")

	// Test typing
	msg := tea.KeyPressMsg(tea.Key{Text: "h", Code: 'h'})
	im.HandleKey(msg)
	if im.Text != "h" {
		t.Fatalf("Expected 'h', got %q", im.Text)
	}

	msg = tea.KeyPressMsg(tea.Key{Text: "e", Code: 'e'})
	im.HandleKey(msg)
	msg = tea.KeyPressMsg(tea.Key{Text: "l", Code: 'l'})
	im.HandleKey(msg)
	msg = tea.KeyPressMsg(tea.Key{Text: "l", Code: 'l'})
	im.HandleKey(msg)
	msg = tea.KeyPressMsg(tea.Key{Text: "o", Code: 'o'})
	im.HandleKey(msg)

	if im.Text != "hello" {
		t.Fatalf("Expected 'hello', got %q", im.Text)
	}

	// Test backspace
	backspaceMsg := tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace})
	im.HandleKey(backspaceMsg)
	if im.Text != "hell" {
		t.Fatalf("Expected 'hell' after backspace, got %q", im.Text)
	}

	// Test submit
	enterMsg := tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
	cmd := im.HandleKey(enterMsg)
	if cmd == nil {
		t.Fatal("Expected non-nil cmd for Enter with text")
	}
	submitMsg := cmd().(InputSubmitMsg)
	if submitMsg.Text != "hell" {
		t.Fatalf("Expected submit 'hell', got %q", submitMsg.Text)
	}

	// Test text is cleared
	if im.Text != "" {
		t.Fatalf("Expected empty text after submit, got %q", im.Text)
	}

	t.Log("All basic input tests passed!")
}

func TestInputModelCursor(t *testing.T) {
	im := NewInputModel(nil, nil, nil, nil, "")

	// Initial cursor should be at 0
	if im.CursorPos != 0 {
		t.Fatalf("Expected initial cursor at 0, got %d", im.CursorPos)
	}

	// Type "abc"
	for _, c := range []rune("abc") {
		im.HandleKey(tea.KeyPressMsg(tea.Key{Text: string(c), Code: c}))
	}

	if im.CursorPos != 3 {
		t.Fatalf("Expected cursor at 3, got %d", im.CursorPos)
	}

	// Cursor should be non-nil
	cursor := im.Cursor()
	if cursor == nil {
		t.Fatal("Expected non-nil cursor")
	}

	// Test SetWidth
	im.SetWidth(50)
	if im.Text != "abc" {
		t.Fatalf("Expected 'abc' after SetWidth, got %q", im.Text)
	}

	t.Log("Cursor tests passed!")
}

func TestInputCursorUsesCellWidthForWideCharacters(t *testing.T) {
	im := NewInputModel(nil, nil, nil, nil, "")
	im.SetWidth(20)
	im.SetText("你好")

	cursor := im.Cursor()
	if cursor == nil {
		t.Fatal("expected cursor")
	}
	if cursor.X != 6 { // "> " is 2 cells, "你好" is 4.
		t.Fatalf("cursor X = %d, want 6", cursor.X)
	}
}
