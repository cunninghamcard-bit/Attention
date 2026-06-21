package builtin

import "testing"

func TestTruncateCountsLinesLikePi(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		truncate   func(string, truncationOptions) truncationResult
		content    string
		wantLines  int
		wantOutput int
	}{
		{
			name:       "head empty content",
			truncate:   truncateHead,
			content:    "",
			wantLines:  0,
			wantOutput: 0,
		},
		{
			name:       "tail empty content",
			truncate:   truncateTail,
			content:    "",
			wantLines:  0,
			wantOutput: 0,
		},
		{
			name:       "head without trailing newline",
			truncate:   truncateHead,
			content:    "a\nb",
			wantLines:  2,
			wantOutput: 2,
		},
		{
			name:       "tail without trailing newline",
			truncate:   truncateTail,
			content:    "a\nb",
			wantLines:  2,
			wantOutput: 2,
		},
		{
			name:       "head with trailing newline",
			truncate:   truncateHead,
			content:    "a\nb\n",
			wantLines:  2,
			wantOutput: 2,
		},
		{
			name:       "tail with trailing newline",
			truncate:   truncateTail,
			content:    "a\nb\n",
			wantLines:  2,
			wantOutput: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.truncate(tt.content, truncationOptions{maxLines: 10, maxBytes: 100})
			if got.Truncated {
				t.Fatalf("Truncated = true, want false: %+v", got)
			}
			if got.TotalLines != tt.wantLines {
				t.Fatalf("TotalLines = %d, want %d", got.TotalLines, tt.wantLines)
			}
			if got.OutputLines != tt.wantOutput {
				t.Fatalf("OutputLines = %d, want %d", got.OutputLines, tt.wantOutput)
			}
			if got.Content != tt.content {
				t.Fatalf("Content = %q, want %q", got.Content, tt.content)
			}
		})
	}
}

func TestTruncateLineLimitBoundariesLikePi(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		truncate    func(string, truncationOptions) truncationResult
		content     string
		wantContent string
		wantLines   int
		wantOutput  int
		wantTrunc   bool
	}{
		{
			name:        "head trailing newline exactly at limit",
			truncate:    truncateHead,
			content:     "a\nb\n",
			wantContent: "a\nb\n",
			wantLines:   2,
			wantOutput:  2,
			wantTrunc:   false,
		},
		{
			name:        "tail trailing newline exactly at limit",
			truncate:    truncateTail,
			content:     "a\nb\n",
			wantContent: "a\nb\n",
			wantLines:   2,
			wantOutput:  2,
			wantTrunc:   false,
		},
		{
			name:        "head trailing newline over limit",
			truncate:    truncateHead,
			content:     "a\nb\nc\n",
			wantContent: "a\nb",
			wantLines:   3,
			wantOutput:  2,
			wantTrunc:   true,
		},
		{
			name:        "tail trailing newline over limit",
			truncate:    truncateTail,
			content:     "a\nb\nc\n",
			wantContent: "b\nc",
			wantLines:   3,
			wantOutput:  2,
			wantTrunc:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.truncate(tt.content, truncationOptions{maxLines: 2, maxBytes: 100})
			if got.Truncated != tt.wantTrunc {
				t.Fatalf("Truncated = %v, want %v: %+v", got.Truncated, tt.wantTrunc, got)
			}
			if got.TotalLines != tt.wantLines {
				t.Fatalf("TotalLines = %d, want %d", got.TotalLines, tt.wantLines)
			}
			if got.OutputLines != tt.wantOutput {
				t.Fatalf("OutputLines = %d, want %d", got.OutputLines, tt.wantOutput)
			}
			if got.Content != tt.wantContent {
				t.Fatalf("Content = %q, want %q", got.Content, tt.wantContent)
			}
		})
	}
}
