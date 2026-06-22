package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// toolDef builds a minimal named ToolDefinition for selection tests.
func toolDef(name string) extension.ToolDefinition {
	return extension.ToolDefinition{Name: name}
}

func names(defs []extension.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestSelectTools(t *testing.T) {
	base := []extension.ToolDefinition{toolDef("read"), toolDef("bash"), toolDef("edit"), toolDef("write")}
	all := []extension.ToolDefinition{toolDef("read"), toolDef("bash"), toolDef("edit"), toolDef("write"), toolDef("grep"), toolDef("find"), toolDef("ls")}

	tests := []struct {
		name    string
		sel     toolSelection
		want    []string
		wantErr string
	}{
		{
			name: "default base when no flags",
			sel:  toolSelection{},
			want: []string{"read", "bash", "edit", "write"},
		},
		{
			name: "no-tools empties everything",
			sel:  toolSelection{noTools: true},
			want: []string{},
		},
		{
			name: "no-tools beats tools allowlist",
			sel:  toolSelection{noTools: true, tools: []string{"grep"}},
			want: []string{},
		},
		{
			name: "no-builtin-tools empties built-ins",
			sel:  toolSelection{noBuiltinTool: true},
			want: []string{},
		},
		{
			name: "no-builtin-tools beats tools allowlist",
			sel:  toolSelection{noBuiltinTool: true, tools: []string{"read"}},
			want: []string{},
		},
		{
			name: "tools allowlist selects from full set (read-only reachable)",
			sel:  toolSelection{tools: []string{"grep", "find"}},
			want: []string{"grep", "find"},
		},
		{
			name: "exclude removes from default base",
			sel:  toolSelection{excludeTools: []string{"bash"}},
			want: []string{"read", "edit", "write"},
		},
		{
			name: "exclude applied after allowlist",
			sel:  toolSelection{tools: []string{"read", "grep", "find"}, excludeTools: []string{"grep"}},
			want: []string{"read", "find"},
		},
		{
			name:    "unknown tool in allowlist errors",
			sel:     toolSelection{tools: []string{"grep", "nope"}},
			wantErr: `unknown tool "nope"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectTools(tt.sel, base, all)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(names(got), tt.want) {
				t.Fatalf("tools = %v, want %v", names(got), tt.want)
			}
		})
	}
}

func TestSelectToolsUnknownErrorListsAvailable(t *testing.T) {
	all := []extension.ToolDefinition{toolDef("read"), toolDef("grep")}
	_, err := selectTools(toolSelection{tools: []string{"bogus"}}, all, all)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "grep") || !strings.Contains(err.Error(), "read") {
		t.Fatalf("error %q should list available tool names", err)
	}
}

func TestResolveThinkingLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "low", want: "low"},
		{in: "medium", want: "medium"},
		{in: "high", want: "high"},
		{in: "off", wantErr: true},
		{in: "xhigh", wantErr: true},
		{in: "bogus", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := resolveThinkingLevel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("level = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSessionDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	tests := []struct {
		name      string
		flagValue string
		def       string
		want      string
	}{
		{name: "empty flag falls back to default", flagValue: "", def: "/default/root", want: "/default/root"},
		{name: "explicit flag wins", flagValue: "/custom/sessions", def: "/default/root", want: "/custom/sessions"},
		{name: "tilde expands", flagValue: "~/sessions", def: "/default/root", want: filepath.Join(home, "sessions")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSessionDir(tt.flagValue, tt.def)
			if err != nil {
				t.Fatalf("resolveSessionDir: %v", err)
			}
			if got != tt.want {
				t.Fatalf("dir = %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeVisiter records which flags were "set" so validateName can be exercised
// without a real FlagSet.
type fakeVisiter struct{ set []string }

func (f fakeVisiter) Visit(fn func(*flag.Flag)) {
	for _, name := range f.set {
		fn(&flag.Flag{Name: name})
	}
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		visiter fakeVisiter
		wantErr bool
	}{
		{name: "unset is ok", value: "", visiter: fakeVisiter{}},
		{name: "non-empty value ok", value: "My Session", visiter: fakeVisiter{set: []string{"name"}}},
		{name: "explicit empty long flag errors", value: "", visiter: fakeVisiter{set: []string{"name"}}, wantErr: true},
		{name: "explicit empty short alias errors", value: "", visiter: fakeVisiter{set: []string{"n"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateName(tt.value, tt.visiter)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSplitCommaList(t *testing.T) {
	got := splitCommaList(" grep , , find ,")
	want := []string{"grep", "find"}
	if !slices.Equal(got, want) {
		t.Fatalf("splitCommaList = %v, want %v", got, want)
	}
	if splitCommaList("") != nil {
		t.Fatal("empty input should yield nil")
	}
}

func TestRepeatableFlagAccumulates(t *testing.T) {
	var r repeatableFlag
	_ = r.Set("a")
	_ = r.Set("b")
	if !slices.Equal([]string(r), []string{"a", "b"}) {
		t.Fatalf("repeatableFlag = %v, want [a b]", []string(r))
	}
}

// TestBaseAndAllToolSets confirms the wiring constructors expose the expected
// tool names so the --tools allowlist can reach read-only tools.
func TestBaseAndAllToolSets(t *testing.T) {
	env := local.New(t.TempDir())
	base := names(baseToolSet(env, ""))
	all := names(allToolSet(env, ""))

	if !slices.Equal(base, []string{"read", "bash", "edit", "write"}) {
		t.Fatalf("base tools = %v", base)
	}
	for _, want := range []string{"grep", "find", "ls"} {
		if !slices.Contains(all, want) {
			t.Fatalf("all tools %v missing %q", all, want)
		}
	}
}

// TestResolveSessionPlanForkSetsParent verifies --fork produces a planFork whose
// CreateOptions records the source session path as the parent.
func TestResolveSessionPlanForkSetsParent(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "source.jsonl")
	if err := os.WriteFile(srcPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	lister := stubLister{}

	plan, err := resolveSessionPlan(context.Background(), lister, "/work", sessionFlags{fork: srcPath})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planFork {
		t.Fatalf("kind = %v, want planFork", plan.kind)
	}
	if plan.createOptions.ParentSessionPath != srcPath {
		t.Fatalf("ParentSessionPath = %q, want %q", plan.createOptions.ParentSessionPath, srcPath)
	}
	if plan.cwd != "/work" {
		t.Fatalf("cwd = %q, want /work", plan.cwd)
	}
}

// TestResolveSessionPlanForkByID resolves a fork source given as a partial id.
func TestResolveSessionPlanForkByID(t *testing.T) {
	cwd := "/work"
	lister := stubLister{
		scoped: map[string][]session.Metadata{
			cwd: {meta("abc123", cwd, time.Time{})},
		},
		all: []session.Metadata{meta("abc123", cwd, time.Time{})},
	}
	plan, err := resolveSessionPlan(context.Background(), lister, cwd, sessionFlags{fork: "abc"})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planFork {
		t.Fatalf("kind = %v, want planFork", plan.kind)
	}
	if plan.createOptions.ParentSessionPath != filepath.Join("/sessions", "abc123.jsonl") {
		t.Fatalf("ParentSessionPath = %q", plan.createOptions.ParentSessionPath)
	}
}
