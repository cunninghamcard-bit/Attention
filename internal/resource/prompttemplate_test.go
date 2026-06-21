package resource

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/config"
)

func TestParseCommandArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "plain",
			in:   "one two",
			want: []string{"one", "two"},
		},
		{
			name: "quoted",
			in:   `"a b"`,
			want: []string{"a b"},
		},
		{
			name: "mixed",
			in:   `one "two three" 'four five' six`,
			want: []string{"one", "two three", "four five", "six"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCommandArgs(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseCommandArgs(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSubstituteArgs(t *testing.T) {
	tests := []struct {
		name    string
		content string
		args    []string
		want    string
	}{
		{
			name:    "positionals",
			content: "first=$1 second=$2",
			args:    []string{"one", "two"},
			want:    "first=one second=two",
		},
		{
			name:    "missing positional",
			content: "missing=$3",
			args:    []string{"one", "two"},
			want:    "missing=",
		},
		{
			name:    "slice from second",
			content: "rest=${@:2}",
			args:    []string{"one", "two", "three"},
			want:    "rest=two three",
		},
		{
			name:    "slice with length",
			content: "one=${@:2:1}",
			args:    []string{"one", "two", "three"},
			want:    "one=two",
		},
		{
			name:    "slice from zero starts at first",
			content: "start=${@:0:2}",
			args:    []string{"one", "two", "three"},
			want:    "start=one two",
		},
		{
			name:    "arguments",
			content: "all=$ARGUMENTS",
			args:    []string{"one", "two"},
			want:    "all=one two",
		},
		{
			name:    "at",
			content: "all=$@",
			args:    []string{"one", "two"},
			want:    "all=one two",
		},
		{
			name:    "positionals are not re-run after wildcards",
			content: "$@ $2",
			args:    []string{"$2", "two"},
			want:    "$2 two two",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SubstituteArgs(tt.content, tt.args)
			if got != tt.want {
				t.Fatalf("SubstituteArgs(%q, %#v) = %q, want %q", tt.content, tt.args, got, tt.want)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	templates := []PromptTemplate{
		{
			Name:    "name",
			Content: "hello $1 $2",
		},
	}

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "non slash",
			text: "hello /name a b",
			want: "hello /name a b",
		},
		{
			name: "known template",
			text: "/name a b",
			want: "hello a b",
		},
		{
			name: "unknown template",
			text: "/missing a b",
			want: "/missing a b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandPromptTemplate(tt.text, templates)
			if got != tt.want {
				t.Fatalf("Expand(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestLoadExplicitTemplate(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "greet.md")
	content := `---
description: Say hello
argument-hint: <name>
---
Hello $1
`
	if err := os.WriteFile(templatePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}

	got, _, err := LoadPromptTemplates(LoadPromptTemplatesOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{dir},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("templates len = %d, want 1: %#v", len(got), got)
	}

	template := got[0]
	if template.Name != "greet" {
		t.Fatalf("Name = %q, want greet", template.Name)
	}
	if template.Description != "Say hello" {
		t.Fatalf("Description = %q, want Say hello", template.Description)
	}
	if template.ArgumentHint != "<name>" {
		t.Fatalf("ArgumentHint = %q, want <name>", template.ArgumentHint)
	}
	if template.Content != "Hello $1" {
		t.Fatalf("Content = %q, want body without frontmatter", template.Content)
	}
	if template.Source.Kind != SourceExplicit || template.Source.Path != templatePath {
		t.Fatalf("Source = %#v, want explicit source at %q", template.Source, templatePath)
	}
}

func TestLoadDefaultsFromGlobalAndProject(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	globalDir := filepath.Join(agentDir, "prompts")
	projectDir := filepath.Join(cwd, config.ConfigDirName, "prompts")
	for _, dir := range []string{globalDir, projectDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(globalDir, "global.md"), []byte("global body"), 0o600); err != nil {
		t.Fatalf("write global template: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "project.md"), []byte("project body"), 0o600); err != nil {
		t.Fatalf("write project template: %v", err)
	}

	got, _, err := LoadPromptTemplates(LoadPromptTemplatesOptions{
		CWD:             cwd,
		AgentDir:        agentDir,
		IncludeDefaults: true,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("templates len = %d, want 2: %#v", len(got), got)
	}
	if got[0].Name != "global" || got[0].Source.Kind != SourceGlobal {
		t.Fatalf("first template = %#v, want global template", got[0])
	}
	if got[1].Name != "project" || got[1].Source.Kind != SourceProject {
		t.Fatalf("second template = %#v, want project template", got[1])
	}
}

func TestLoadPromptTemplatesMissingDirIsEmpty(t *testing.T) {
	got, _, err := LoadPromptTemplates(LoadPromptTemplatesOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{filepath.Join(t.TempDir(), "missing")},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("templates len = %d, want 0: %#v", len(got), got)
	}
}

func TestLoadReturnsDiagnosticsForMalformedTemplates(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "good.md")
	if err := os.WriteFile(goodPath, []byte("good body"), 0o600); err != nil {
		t.Fatalf("write good template: %v", err)
	}
	badPath := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(badPath, []byte(`---
description: [bad
---
bad body
`), 0o600); err != nil {
		t.Fatalf("write bad template: %v", err)
	}

	got, diagnostics, err := LoadPromptTemplates(LoadPromptTemplatesOptions{
		CWD:             t.TempDir(),
		AgentDir:        t.TempDir(),
		Paths:           []string{dir},
		IncludeDefaults: false,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("templates = %#v, want only good template", got)
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
	if !strings.Contains(diagnostic.Message, "load prompt template") {
		t.Fatalf("diagnostic message = %q, want prompt template warning", diagnostic.Message)
	}
}

func TestDedupePromptTemplates(t *testing.T) {
	templates := []PromptTemplate{
		{Name: "review", Source: NewSourceInfo(SourceGlobal, "/g/review.md", "/g")},
		{Name: "plan", Source: NewSourceInfo(SourceGlobal, "/g/plan.md", "/g")},
		{Name: "review", Source: NewSourceInfo(SourceProject, "/p/review.md", "/p")},
	}

	deduped, diagnostics := DedupePromptTemplates(templates)
	if len(deduped) != 2 || deduped[0].Name != "review" || deduped[1].Name != "plan" {
		t.Fatalf("deduped = %#v", deduped)
	}
	if deduped[0].Source.Path != "/g/review.md" {
		t.Errorf("winner = %q, want first occurrence", deduped[0].Source.Path)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want 1 collision", diagnostics)
	}
	d := diagnostics[0]
	if d.Type != DiagnosticCollision || d.Message != `name "/review" collision` || d.Path != "/p/review.md" {
		t.Errorf("diagnostic = %#v", d)
	}
	if d.Collision == nil ||
		d.Collision.ResourceType != ResourceTypePrompt ||
		d.Collision.WinnerPath != "/g/review.md" ||
		d.Collision.LoserPath != "/p/review.md" {
		t.Errorf("collision payload = %#v", d.Collision)
	}
}
