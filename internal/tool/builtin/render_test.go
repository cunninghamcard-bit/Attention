package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/render"
)

func TestBuiltinRenderCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		renderer  extension.ToolCallRenderer
		input     extension.ToolCallRenderInput
		wantLen   int
		wantKind  string
		wantText  string
		wantLang  string
		checkRest func(t *testing.T, blocks []render.Block)
	}{
		{
			name:     "bash",
			renderer: bashRenderCall,
			input: extension.ToolCallRenderInput{
				Args: map[string]any{"command": "ls -la"},
			},
			wantLen:  1,
			wantKind: "code",
			wantText: "ls -la",
			wantLang: "shell",
		},
		{
			name:     "read with range",
			renderer: readRenderCall,
			input: extension.ToolCallRenderInput{
				Args: map[string]any{
					"file_path": "x.go",
					"offset":    float64(3),
					"limit":     float64(4),
				},
			},
			wantLen:  1,
			wantKind: "text",
			wantText: "read x.go:3-6",
		},
		{
			name:     "write",
			renderer: writeRenderCall,
			input: extension.ToolCallRenderInput{
				Args: map[string]any{
					"path":    "x.go",
					"content": "package main",
				},
			},
			wantLen:  2,
			wantKind: "text",
			wantText: "write x.go",
			checkRest: func(t *testing.T, blocks []render.Block) {
				t.Helper()
				if blocks[1].Kind != "code" || blocks[1].Language != "go" {
					t.Fatalf("writeRenderCall() second block = %#v, want go code block", blocks[1])
				}
			},
		},
		{
			name:     "edit",
			renderer: editRenderCall,
			input: extension.ToolCallRenderInput{
				Args: map[string]any{"path": "x.go"},
			},
			wantLen:  1,
			wantKind: "text",
			wantText: "edit x.go",
		},
		{
			name:     "ls default path",
			renderer: lsRenderCall,
			input: extension.ToolCallRenderInput{
				Args: map[string]any{},
			},
			wantLen:  1,
			wantKind: "text",
			wantText: "ls .",
		},
		{
			name:     "find",
			renderer: findRenderCall,
			input: extension.ToolCallRenderInput{
				Args: map[string]any{
					"pattern": "*.go",
					"path":    "src",
				},
			},
			wantLen:  1,
			wantKind: "text",
			wantText: "find *.go in src",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.renderer(tt.input)
			if len(got) != tt.wantLen {
				t.Fatalf("render call len = %d, want %d: %#v", len(got), tt.wantLen, got)
			}
			if got[0].Kind != tt.wantKind {
				t.Fatalf("render call first kind = %q, want %q", got[0].Kind, tt.wantKind)
			}
			if !strings.Contains(got[0].Text, tt.wantText) {
				t.Fatalf("render call first text = %q, want containing %q", got[0].Text, tt.wantText)
			}
			if got[0].Language != tt.wantLang {
				t.Fatalf("render call first language = %q, want %q", got[0].Language, tt.wantLang)
			}
			if tt.checkRest != nil {
				tt.checkRest(t, got)
			}
		})
	}
}

func TestBashRenderResultCollapsedAndExpanded(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
		"line 6",
		"line 7",
		"line 8",
	}, "\n")
	result := renderTextResult(output, nil)

	collapsed := bashRenderResult(extension.ToolResultRenderInput{Result: result})
	if len(collapsed) != 2 {
		t.Fatalf("collapsed bashRenderResult() = %#v, want code block plus badge", collapsed)
	}
	if collapsed[0].Kind != "code" || collapsed[0].Language != "console" {
		t.Fatalf("collapsed first block = %#v, want console code", collapsed[0])
	}
	if strings.Contains(collapsed[0].Text, "line 6") {
		t.Fatalf("collapsed code text contains line 6: %q", collapsed[0].Text)
	}
	if collapsed[1].Kind != "badge" ||
		collapsed[1].Style != "muted" ||
		!strings.Contains(collapsed[1].Text, "more lines") {
		t.Fatalf("collapsed second block = %#v, want muted more-lines badge", collapsed[1])
	}

	expanded := bashRenderResult(extension.ToolResultRenderInput{
		Result:   result,
		Expanded: true,
	})
	if len(expanded) != 1 {
		t.Fatalf("expanded bashRenderResult() = %#v, want one code block", expanded)
	}
	if expanded[0].Kind != "code" || !strings.Contains(expanded[0].Text, "line 8") {
		t.Fatalf("expanded first block = %#v, want full code output", expanded[0])
	}
}

func TestReadRenderResultImageAndCode(t *testing.T) {
	t.Parallel()

	got := readRenderResult(extension.ToolResultRenderInput{
		Args: map[string]any{"file_path": "x.go"},
		Result: extension.RenderResult{
			Content: []ai.ContentBlock{
				{Type: ai.ContentImage, ImageData: "abc", MimeType: "image/png"},
				{Type: ai.ContentText, Text: "package main"},
			},
		},
	})

	if len(got) != 3 {
		t.Fatalf("readRenderResult() = %#v, want image + fallback + code", got)
	}
	if got[0].Kind != "image" || got[0].Data != "abc" || got[0].MimeType != "image/png" {
		t.Fatalf("readRenderResult() first block = %#v, want image block", got[0])
	}
	if got[1].Kind != "image-fallback" || got[1].Text != "[image/png]" {
		t.Fatalf("readRenderResult() second block = %#v, want image-fallback", got[1])
	}
	if got[2].Kind != "code" || got[2].Language != "go" || !strings.Contains(got[2].Text, "package main") {
		t.Fatalf("readRenderResult() third block = %#v, want go code block", got[2])
	}
}

func TestEditRenderResultDiffFromMapDetails(t *testing.T) {
	t.Parallel()

	details := detailsAsMap(t, editToolDetails{Diff: "+a\n-b"})
	got := editRenderResult(extension.ToolResultRenderInput{
		Result: extension.RenderResult{Details: details},
	})

	if len(got) != 1 || got[0].Kind != "diff" || got[0].Text != "+a\n-b" {
		t.Fatalf("editRenderResult() = %#v, want one diff block", got)
	}
}

func TestLsFindRenderResultLimitBadges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		renderer  extension.ToolResultRenderer
		details   any
		wantBadge string
	}{
		{
			name:      "ls entry limit",
			renderer:  lsRenderResult,
			details:   lsToolDetails{EntryLimitReached: 2},
			wantBadge: "entry limit 2 reached",
		},
		{
			name:      "find result limit",
			renderer:  findRenderResult,
			details:   findToolDetails{ResultLimitReached: 3},
			wantBadge: "result limit 3 reached",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.renderer(extension.ToolResultRenderInput{
				Result: renderTextResult("a\nb", tt.details),
			})
			if len(got) != 2 {
				t.Fatalf("render result = %#v, want code block plus warning badge", got)
			}
			if got[0].Kind != "code" {
				t.Fatalf("first block = %#v, want code block", got[0])
			}
			if got[1].Kind != "badge" || got[1].Style != "warning" || got[1].Text != tt.wantBadge {
				t.Fatalf("second block = %#v, want warning badge %q", got[1], tt.wantBadge)
			}
		})
	}
}

func TestWriteRenderResultText(t *testing.T) {
	t.Parallel()

	got := writeRenderResult(extension.ToolResultRenderInput{
		Result: renderTextResult("Successfully wrote 3 bytes to a.txt", nil),
	})

	if len(got) != 1 || got[0].Kind != "text" || got[0].Text != "Successfully wrote 3 bytes to a.txt" {
		t.Fatalf("writeRenderResult() = %#v, want one text block", got)
	}
}

func renderTextResult(text string, details any) extension.RenderResult {
	return extension.RenderResult{
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
		Details: details,
	}
}

func detailsAsMap(t *testing.T, details any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("Marshal details: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal details map: %v", err)
	}
	return out
}
