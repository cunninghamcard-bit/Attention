package resource

import (
	"path/filepath"
	"strings"
	"time"
)

type ToolInfo struct {
	Name    string
	Snippet string
}

type SystemPromptOptions struct {
	CustomPrompt       string
	AppendSystemPrompt string
	CWD                string
	Tools              []ToolInfo
	ContextFiles       []ContextFile
	Skills             []Skill
	Guidelines         []string
	HasReadTool        bool
}

// BuildSystemPrompt constructs the system prompt in pi's section order.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/system-prompt.ts:28-175.
func BuildSystemPrompt(opts SystemPromptOptions) string {
	date := time.Now().Format("2006-01-02")
	promptCWD := filepath.ToSlash(opts.CWD)
	appendSection := ""
	if opts.AppendSystemPrompt != "" {
		appendSection = "\n\n" + opts.AppendSystemPrompt
	}

	var prompt string
	if opts.CustomPrompt != "" {
		prompt = opts.CustomPrompt
		if appendSection != "" {
			prompt += appendSection
		}
	} else {
		prompt = defaultBasePrompt(opts, appendSection)
	}

	prompt = appendProjectContext(prompt, opts.ContextFiles)
	if opts.HasReadTool && len(opts.Skills) > 0 {
		prompt += FormatSkillsForPrompt(opts.Skills)
	}
	prompt += "\nCurrent date: " + date
	prompt += "\nCurrent working directory: " + promptCWD
	return prompt
}

func defaultBasePrompt(opts SystemPromptOptions, appendSection string) string {
	toolsList := formatTools(opts.Tools)
	guidelines := formatGuidelines(opts)

	// pi adds a "Pi documentation" section here:
	// .agents/references/pi/packages/coding-agent/src/core/system-prompt.ts:142-149.
	// along intentionally drops it because there are no equivalent config paths,
	// and keeps the default assistant wording brand-neutral.
	prompt := `You are an expert coding assistant operating inside along. You help users by reading files, executing commands, editing code, and writing new files.

Available tools:
` + toolsList + `

In addition to the tools above, you may have access to other custom tools depending on the project.

Guidelines:
` + guidelines

	if appendSection != "" {
		prompt += appendSection
	}
	return prompt
}

func formatTools(tools []ToolInfo) string {
	lines := make([]string, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		snippet := strings.TrimSpace(tool.Snippet)
		if name == "" || snippet == "" {
			continue
		}
		lines = append(lines, "- "+name+": "+snippet)
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, "\n")
}

func formatGuidelines(opts SystemPromptOptions) string {
	guidelines := []string{}
	seen := map[string]struct{}{}
	add := func(guideline string) {
		trimmed := strings.TrimSpace(guideline)
		if trimmed == "" {
			return
		}
		if _, exists := seen[trimmed]; exists {
			return
		}
		seen[trimmed] = struct{}{}
		guidelines = append(guidelines, trimmed)
	}

	var hasBash bool
	var hasGrep bool
	var hasFind bool
	var hasLs bool
	for _, tool := range opts.Tools {
		switch strings.TrimSpace(tool.Name) {
		case "bash":
			hasBash = true
		case "grep":
			hasGrep = true
		case "find":
			hasFind = true
		case "ls":
			hasLs = true
		}
	}

	// pi: .agents/references/pi/packages/coding-agent/src/core/system-prompt.ts:114-115.
	if hasBash && !hasGrep && !hasFind && !hasLs {
		add("Use bash for file operations like ls, rg, find")
	}

	for _, guideline := range opts.Guidelines {
		add(guideline)
	}
	add("Be concise in your responses")
	add("Show file paths clearly when working with files")

	lines := make([]string, 0, len(guidelines))
	for _, guideline := range guidelines {
		lines = append(lines, "- "+guideline)
	}
	return strings.Join(lines, "\n")
}

func appendProjectContext(prompt string, files []ContextFile) string {
	if len(files) == 0 {
		return prompt
	}

	var builder strings.Builder
	builder.WriteString(prompt)
	builder.WriteString("\n\n<project_context>\n\n")
	builder.WriteString("Project-specific instructions and guidelines:\n\n")
	for _, file := range files {
		builder.WriteString(`<project_instructions path="`)
		builder.WriteString(file.Path)
		builder.WriteString(`">`)
		builder.WriteByte('\n')
		builder.WriteString(file.Content)
		builder.WriteString("\n</project_instructions>\n\n")
	}
	builder.WriteString("</project_context>\n")
	return builder.String()
}
