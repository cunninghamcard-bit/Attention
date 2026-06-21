package resource

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadReturnsGlobalThenAncestorContextFiles(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	parentDir := filepath.Join(root, "workspace")
	cwd := filepath.Join(parentDir, "service")

	writeFile(t, filepath.Join(agentDir, "AGENTS.md"), "global")
	writeFile(t, filepath.Join(parentDir, "CLAUDE.md"), "parent")
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "cwd")

	got, _ := LoadContextFiles(cwd, agentDir)
	want := []ContextFile{
		{Path: filepath.Join(agentDir, "AGENTS.md"), Content: "global"},
		{Path: filepath.Join(parentDir, "CLAUDE.md"), Content: "parent"},
		{Path: filepath.Join(cwd, "AGENTS.md"), Content: "cwd"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadDeduplicatesGlobalAndAncestorContextFiles(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "shared")

	got, _ := LoadContextFiles(cwd, cwd)
	want := []ContextFile{
		{Path: filepath.Join(cwd, "AGENTS.md"), Content: "shared"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadSkipsMissingContextFiles(t *testing.T) {
	got, _ := LoadContextFiles(t.TempDir(), filepath.Join(t.TempDir(), "missing-agent"))
	if len(got) != 0 {
		t.Fatalf("Load() len = %d, want 0: %#v", len(got), got)
	}
}

func TestLoadSkipsEmptyAgentDir(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "cwd")

	got, _ := LoadContextFiles(cwd, "")
	want := []ContextFile{
		{Path: filepath.Join(cwd, "AGENTS.md"), Content: "cwd"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadUsesFirstCandidatePerDirectory(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "CLAUDE.md"), "claude")
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "agents")

	got, _ := LoadContextFiles(cwd, filepath.Join(t.TempDir(), "agent"))
	want := []ContextFile{
		{Path: filepath.Join(cwd, "AGENTS.md"), Content: "agents"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadReturnsDiagnosticsForUnreadableContextCandidate(t *testing.T) {
	cwd := t.TempDir()
	badPath := filepath.Join(cwd, "AGENTS.md")
	if err := os.MkdirAll(badPath, 0o700); err != nil {
		t.Fatalf("mkdir bad context candidate: %v", err)
	}
	writeFile(t, filepath.Join(cwd, "CLAUDE.md"), "claude")

	got, diagnostics := LoadContextFiles(cwd, "")
	want := []ContextFile{
		{Path: filepath.Join(cwd, "CLAUDE.md"), Content: "claude"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics len = %d, want 1: %#v", len(diagnostics), diagnostics)
	}
	diagnostic := diagnostics[0]
	if diagnostic.Type != DiagnosticWarning {
		t.Fatalf("diagnostic type = %q, want warning", diagnostic.Type)
	}
	if diagnostic.Path != badPath {
		t.Fatalf("diagnostic path = %q, want %q", diagnostic.Path, badPath)
	}
	if !strings.Contains(diagnostic.Message, "read context file") {
		t.Fatalf("diagnostic message = %q, want context read warning", diagnostic.Message)
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
