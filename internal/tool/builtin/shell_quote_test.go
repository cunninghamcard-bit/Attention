package builtin

import "testing"

func TestShellJoinQuotesOnlyWhenNeeded(t *testing.T) {
	t.Parallel()

	got, err := shellJoin([]string{
		"command",
		"-v",
		"rg",
		"two words",
		"it's",
		"*.go",
	})
	if err != nil {
		t.Fatalf("shellJoin() error = %v", err)
	}

	want := `command -v rg 'two words' "it's" '*.go'`
	if got != want {
		t.Fatalf("shellJoin() = %q, want %q", got, want)
	}
}

func TestShellJoinReturnsQuoteError(t *testing.T) {
	t.Parallel()

	if _, err := shellJoin([]string{"bad\x00arg"}); err == nil {
		t.Fatal("shellJoin() error = nil, want quote error")
	}
}
