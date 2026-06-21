package builtin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/render"
)

func TestGrepRenderResultGroupsMatches(t *testing.T) {
	t.Parallel()

	details := grepToolDetails{
		Matches: []grepMatch{
			{Path: "fileA", Line: 1, Text: "alpha"},
			{Path: "fileA", Line: 2, Text: "beta"},
			{Path: "fileB", Line: 5, Text: "gamma"},
		},
	}
	detailsMap := map[string]any{}
	raw, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("Marshal details: %v", err)
	}
	if err := json.Unmarshal(raw, &detailsMap); err != nil {
		t.Fatalf("Unmarshal details map: %v", err)
	}

	want := []render.Block{
		render.Group("fileA", []render.Block{
			render.StyledText("1: alpha", "match"),
			render.StyledText("2: beta", "match"),
		}),
		render.Group("fileB", []render.Block{
			render.StyledText("5: gamma", "match"),
		}),
	}

	tests := []struct {
		name    string
		details any
	}{
		{
			name:    "typed details",
			details: details,
		},
		{
			name:    "map details from json round trip",
			details: detailsMap,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := grepRenderResult(extension.ToolResultRenderInput{
				Result: extension.RenderResult{Details: tt.details},
			})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("grepRenderResult() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestGrepRenderCall(t *testing.T) {
	t.Parallel()

	got := grepRenderCall(extension.ToolCallRenderInput{
		Args: map[string]any{
			"pattern": "x",
			"path":    "p",
		},
	})
	if len(got) != 1 || got[0].Kind != "text" {
		t.Fatalf("grepRenderCall() = %#v, want one text block", got)
	}
	if !strings.Contains(got[0].Text, "grep") || !strings.Contains(got[0].Text, "x") {
		t.Fatalf("grepRenderCall() text = %q, want grep call containing pattern", got[0].Text)
	}
}
