package builtin

import (
	"strconv"
	"strings"
	"testing"
)

func TestDetectLineEnding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "crlf",
			text: "alpha\r\nbeta\r\n",
			want: "\r\n",
		},
		{
			name: "lf",
			text: "alpha\nbeta\n",
			want: "\n",
		},
		{
			name: "mixed first crlf",
			text: "alpha\r\nbeta\n",
			want: "\r\n",
		},
		{
			name: "mixed first lf",
			text: "alpha\nbeta\r\n",
			want: "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := DetectLineEnding(tt.text); got != tt.want {
				t.Fatalf("DetectLineEnding() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeForFuzzyMatch(t *testing.T) {
	t.Parallel()

	input := "\u201Chello\u201D\u00A0\u2014\u2003x   \n\u2018bye\u2019\t\nkeep\u202Fspace"
	want := "\"hello\" - x\n'bye'\nkeep space"

	if got := NormalizeForFuzzyMatch(input); got != want {
		t.Fatalf("NormalizeForFuzzyMatch() = %q, want %q", got, want)
	}
}

func TestFuzzyFindText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		content       string
		oldText       string
		wantFound     bool
		wantFuzzy     bool
		wantIndex     int
		wantMatchText string
	}{
		{
			name:          "exact match wins first",
			content:       "alpha\n\u201Cbeta\u201D\n",
			oldText:       "\u201Cbeta\u201D",
			wantFound:     true,
			wantFuzzy:     false,
			wantIndex:     len("alpha\n"),
			wantMatchText: "\u201Cbeta\u201D",
		},
		{
			name:          "fuzzy match found",
			content:       "alpha\nname = \u201CACME\u201D  \n",
			oldText:       "name = \"ACME\"\n",
			wantFound:     true,
			wantFuzzy:     true,
			wantIndex:     len("alpha\n"),
			wantMatchText: "name = \"ACME\"\n",
		},
		{
			name:      "not found",
			content:   "alpha\nbeta\n",
			oldText:   "gamma",
			wantFound: false,
			wantFuzzy: false,
			wantIndex: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FuzzyFindText(tt.content, tt.oldText)
			if got.Found != tt.wantFound {
				t.Fatalf("Found = %v, want %v", got.Found, tt.wantFound)
			}
			if got.UsedFuzzy != tt.wantFuzzy {
				t.Fatalf("UsedFuzzy = %v, want %v", got.UsedFuzzy, tt.wantFuzzy)
			}
			if got.Index != tt.wantIndex {
				t.Fatalf("Index = %d, want %d", got.Index, tt.wantIndex)
			}
			if !tt.wantFound {
				return
			}
			gotText := got.ContentForReplacement[got.Index : got.Index+got.MatchLength]
			if gotText != tt.wantMatchText {
				t.Fatalf("matched text = %q, want %q", gotText, tt.wantMatchText)
			}
		})
	}
}

func TestApplyEdits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		content     string
		edits       []Edit
		wantContent string
		wantErr     string
	}{
		{
			name:    "single edit applies",
			content: "alpha\nbeta\n",
			edits: []Edit{
				{OldText: "beta", NewText: "bravo"},
			},
			wantContent: "alpha\nbravo\n",
		},
		{
			name:    "multi edits apply in reverse order",
			content: "aa\nbb\ncc\n",
			edits: []Edit{
				{OldText: "aa", NewText: "longer"},
				{OldText: "cc", NewText: "z"},
			},
			wantContent: "longer\nbb\nz\n",
		},
		{
			name:    "duplicate old text errors",
			content: "dup\ndup\n",
			edits: []Edit{
				{OldText: "dup", NewText: "one"},
			},
			wantErr: "Found 2 occurrences",
		},
		{
			name:    "overlapping edits error",
			content: "abcd",
			edits: []Edit{
				{OldText: "abc", NewText: "x"},
				{OldText: "bcd", NewText: "y"},
			},
			wantErr: "overlap",
		},
		{
			name:    "empty old text errors",
			content: "alpha",
			edits: []Edit{
				{OldText: "", NewText: "beta"},
			},
			wantErr: "oldText must not be empty",
		},
		{
			name:    "no change errors",
			content: "alpha",
			edits: []Edit{
				{OldText: "alpha", NewText: "alpha"},
			},
			wantErr: "No changes made",
		},
		{
			name:    "not found errors",
			content: "alpha",
			edits: []Edit{
				{OldText: "beta", NewText: "bravo"},
			},
			wantErr: "Could not find",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ApplyEdits(tt.content, tt.edits, "fixture.txt")
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ApplyEdits() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ApplyEdits() error = %q, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyEdits() error = %v", err)
			}
			if got.NewContent != tt.wantContent {
				t.Fatalf("NewContent = %q, want %q", got.NewContent, tt.wantContent)
			}
		})
	}
}

func TestDiffString(t *testing.T) {
	t.Parallel()

	oldContent := "l1\nl2\nl3\nold\nl5\nl6\nl7\n"
	newContent := "l1\nl2\nl3\nnew\nadded\nl5\nl6\nl7\n"

	diff, firstChangedLine := DiffString(oldContent, newContent, 1)
	if firstChangedLine == nil || *firstChangedLine != 4 {
		t.Fatalf("firstChangedLine = %v, want 4", firstChangedLine)
	}

	for _, want := range []string{
		"...",
		" 3 l3",
		"-4 old",
		"+4 new",
		"+5 added",
		" 5 l5",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("DiffString() = %q, want containing %q", diff, want)
		}
	}
}

func TestUnifiedPatchMiddleChangeUsesContext(t *testing.T) {
	t.Parallel()

	oldContent := numberedContent(15)
	newContent := strings.Replace(oldContent, "l8\n", "L8\n", 1)

	patch := UnifiedPatch("fixture.txt", oldContent, newContent, 4)

	if !strings.HasPrefix(patch, "--- fixture.txt\n+++ fixture.txt\n") {
		t.Fatalf("UnifiedPatch() header = %q, want file headers", patch)
	}
	if !strings.Contains(patch, "@@ -4,9 +4,9 @@\n") {
		t.Fatalf("UnifiedPatch() = %q, want middle hunk header", patch)
	}
	for _, want := range []string{
		"\n l4\n",
		"\n-l8\n",
		"\n+L8\n",
		"\n l12\n",
	} {
		if !strings.Contains(patch, want) {
			t.Fatalf("UnifiedPatch() = %q, want containing %q", patch, want)
		}
	}

	removed, added := countPatchChangeLines(patch)
	if removed != 1 || added != 1 {
		t.Fatalf("patch change counts = -%d +%d, want -1 +1; patch:\n%s", removed, added, patch)
	}
	if strings.Contains(patch, "\n-l1\n") || strings.Contains(patch, "\n+l1\n") {
		t.Fatalf("UnifiedPatch() = %q, want context hunk instead of whole-file dump", patch)
	}
}

func TestUnifiedPatchDistantChangesProduceTwoHunks(t *testing.T) {
	t.Parallel()

	oldContent := numberedContent(30)
	newContent := strings.Replace(oldContent, "l3\n", "L3\n", 1)
	newContent = strings.Replace(newContent, "l28\n", "L28\n", 1)

	patch := UnifiedPatch("long.txt", oldContent, newContent, 4)

	if got := strings.Count(patch, "@@ -"); got != 2 {
		t.Fatalf("hunk count = %d, want 2; patch:\n%s", got, patch)
	}
	for _, want := range []string{
		"@@ -1,7 +1,7 @@\n",
		"@@ -24,7 +24,7 @@\n",
		"\n-l3\n",
		"\n+L3\n",
		"\n-l28\n",
		"\n+L28\n",
	} {
		if !strings.Contains(patch, want) {
			t.Fatalf("UnifiedPatch() = %q, want containing %q", patch, want)
		}
	}
}

func TestUnifiedPatchChangeLineCountsMatchActualChanges(t *testing.T) {
	t.Parallel()

	oldContent := "a\nold1\nold2\nsame\nz\n"
	newContent := "a\nnew1\nsame\nz\nnew2\n"

	patch := UnifiedPatch("counts.txt", oldContent, newContent, 4)
	removed, added := countPatchChangeLines(patch)
	if removed != 2 || added != 2 {
		t.Fatalf("patch change counts = -%d +%d, want -2 +2; patch:\n%s", removed, added, patch)
	}
}

func numberedContent(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "l" + strconv.Itoa(i+1)
	}
	return strings.Join(lines, "\n") + "\n"
}

func countPatchChangeLines(patch string) (int, int) {
	removed := 0
	added := 0
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
			continue
		case strings.HasPrefix(line, "-"):
			removed++
		case strings.HasPrefix(line, "+"):
			added++
		}
	}
	return removed, added
}
