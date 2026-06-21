package resource

import (
	"regexp"
	"strings"
	"testing"
)

const (
	useBashFileOpsGuideline = "Use bash for file operations like ls, rg, find"
	preferToolFileGuideline = "Prefer grep/find/ls tools over bash for file exploration"
)

func TestBuildDefaultPromptIncludesToolsGuidelinesContextSkillsDateAndCWD(t *testing.T) {
	got := BuildSystemPrompt(SystemPromptOptions{
		AppendSystemPrompt: "extra instructions",
		CWD:                `/work/project`,
		Tools: []ToolInfo{
			{Name: "read", Snippet: "Read files"},
			{Name: "bash", Snippet: "Run commands"},
			{Name: "grep", Snippet: "Search files"},
			{Name: "ls", Snippet: "List files"},
			{Name: "empty"},
		},
		ContextFiles: []ContextFile{
			{Path: "/repo/AGENTS.md", Content: "root instructions"},
			{Path: "/repo/app/CLAUDE.md", Content: "app instructions"},
		},
		Skills: []Skill{
			{Name: "review", Description: "Review code", FilePath: "/skills/review/SKILL.md"},
		},
		Guidelines:  []string{"Prefer small focused changes"},
		HasReadTool: true,
	})

	assertContains(t, got, "You are an expert coding assistant operating inside along.")
	assertContains(t, got, "- read: Read files")
	assertContains(t, got, "- bash: Run commands")
	assertContains(t, got, "- grep: Search files")
	assertContains(t, got, "- Prefer small focused changes")
	assertContains(t, got, "- Be concise in your responses")
	assertContains(t, got, "- Show file paths clearly when working with files")
	assertContains(t, got, "extra instructions\n\n<project_context>")
	assertContains(t, got, `<project_instructions path="/repo/AGENTS.md">`+"\nroot instructions\n</project_instructions>")
	assertContains(t, got, `<project_instructions path="/repo/app/CLAUDE.md">`+"\napp instructions\n</project_instructions>")
	assertContains(t, got, "<available_skills>")
	assertContains(t, got, "<name>review</name>")
	assertContains(t, got, "<location>/skills/review/SKILL.md</location>")
	assertNotContains(t, got, "Pi documentation")
	assertDateAndCWDTrailer(t, got, `/work/project`)
}

func TestBuildCustomPromptUsesSameTailSections(t *testing.T) {
	got := BuildSystemPrompt(SystemPromptOptions{
		CustomPrompt:       "custom base",
		AppendSystemPrompt: "extra instructions",
		CWD:                `/work/project`,
		ContextFiles: []ContextFile{
			{Path: "/repo/AGENTS.md", Content: "root instructions"},
		},
		Skills: []Skill{
			{Name: "review", Description: "Review code", FilePath: "/skills/review/SKILL.md"},
		},
		HasReadTool: true,
	})

	if !strings.HasPrefix(got, "custom base\n\nextra instructions\n\n<project_context>") {
		t.Fatalf("prompt prefix = %q, want custom prompt, append, then context", got)
	}
	assertContains(t, got, `<project_instructions path="/repo/AGENTS.md">`+"\nroot instructions\n</project_instructions>")
	assertContains(t, got, "<available_skills>")
	assertContains(t, got, "<name>review</name>")
	assertDateAndCWDTrailer(t, got, `/work/project`)
}

func TestBuildOmitsSkillsWithoutReadTool(t *testing.T) {
	got := BuildSystemPrompt(SystemPromptOptions{
		CWD: `/work/project`,
		Tools: []ToolInfo{
			{Name: "bash", Snippet: "Run commands"},
		},
		Skills: []Skill{
			{Name: "review", Description: "Review code", FilePath: "/skills/review/SKILL.md"},
		},
		HasReadTool: false,
	})

	assertNotContains(t, got, "<available_skills>")
	assertDateAndCWDTrailer(t, got, `/work/project`)
}

func TestFormatGuidelinesFileExplorationMatrix(t *testing.T) {
	promptGuideline := "Extension guideline"
	tests := []struct {
		name     string
		tools    []ToolInfo
		want     string
		notWants []string
	}{
		{
			name: "bash only uses bash guidance",
			tools: []ToolInfo{
				{Name: "bash"},
			},
			want:     useBashFileOpsGuideline,
			notWants: []string{preferToolFileGuideline},
		},
		{
			name: "bash with grep has no matrix guidance",
			tools: []ToolInfo{
				{Name: "bash"},
				{Name: "grep"},
			},
			notWants: []string{useBashFileOpsGuideline, preferToolFileGuideline},
		},
		{
			name: "file tools without bash have no matrix guidance",
			tools: []ToolInfo{
				{Name: "grep"},
				{Name: "ls"},
			},
			notWants: []string{useBashFileOpsGuideline, preferToolFileGuideline},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatGuidelines(SystemPromptOptions{
				Tools:      tt.tools,
				Guidelines: []string{promptGuideline},
			})

			for _, notWant := range tt.notWants {
				assertNotContains(t, got, "- "+notWant)
			}
			if tt.want != "" {
				assertContains(t, got, "- "+tt.want)
				assertBefore(t, got, "- "+tt.want, "- "+promptGuideline)
			}
			assertContains(t, got, "- Be concise in your responses")
			assertContains(t, got, "- Show file paths clearly when working with files")
			assertBefore(t, got, "- "+promptGuideline, "- Be concise in your responses")
			assertBefore(t, got, "- Be concise in your responses", "- Show file paths clearly when working with files")
		})
	}
}

func assertContains(t *testing.T, got string, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("prompt missing %q:\n%s", want, got)
	}
}

func assertNotContains(t *testing.T, got string, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Fatalf("prompt contains %q:\n%s", want, got)
	}
}

func assertBefore(t *testing.T, got string, earlier string, later string) {
	t.Helper()
	earlierIndex := strings.Index(got, earlier)
	if earlierIndex < 0 {
		t.Fatalf("prompt missing earlier text %q:\n%s", earlier, got)
	}
	laterIndex := strings.Index(got, later)
	if laterIndex < 0 {
		t.Fatalf("prompt missing later text %q:\n%s", later, got)
	}
	if earlierIndex > laterIndex {
		t.Fatalf("prompt orders %q after %q:\n%s", earlier, later, got)
	}
}

func assertDateAndCWDTrailer(t *testing.T, got string, cwd string) {
	t.Helper()
	pattern := regexp.MustCompile(`\nCurrent date: \d{4}-\d{2}-\d{2}\nCurrent working directory: ` + regexp.QuoteMeta(cwd) + `$`)
	if !pattern.MatchString(got) {
		t.Fatalf("prompt missing date/cwd trailer for %q:\n%s", cwd, got)
	}
}
