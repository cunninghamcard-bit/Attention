package resource

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/cunninghamcard-bit/Attention/internal/config"
)

// PromptTemplate represents a markdown prompt template.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/prompt-templates.ts:11-18.
type PromptTemplate struct {
	Name         string
	Content      string
	Description  string
	ArgumentHint string
	Source       SourceInfo
}

// LoadPromptTemplatesOptions configures prompt template discovery.
type LoadPromptTemplatesOptions struct {
	CWD             string
	AgentDir        string
	Paths           []string
	IncludeDefaults bool
}

var (
	positionalArgPattern = regexp.MustCompile(`\$(\d+)`)
	sliceArgPattern      = regexp.MustCompile(`\$\{@:(\d+)(?::(\d+))?\}`)
	slashTemplatePattern = regexp.MustCompile(`^/([^\s]+)(?:\s+([\s\S]*))?$`)
)

// ParseCommandArgs splits command arguments with pi's bash-style quote handling.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/prompt-templates.ts:24-55.
func ParseCommandArgs(s string) []string {
	args := []string{}
	var current strings.Builder
	var inQuote rune

	for _, r := range s {
		switch {
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
				continue
			}
			current.WriteRune(r)
		case r == '"' || r == '\'':
			inQuote = r
		case unicode.IsSpace(r):
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// SubstituteArgs replaces template argument placeholders in pi's replacement order.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/prompt-templates.ts:68-101.
func SubstituteArgs(content string, args []string) string {
	result := positionalArgPattern.ReplaceAllStringFunc(content, func(match string) string {
		groups := positionalArgPattern.FindStringSubmatch(match)
		if len(groups) != 2 {
			return match
		}
		index, err := parsePositiveInt(groups[1])
		if err != nil {
			return ""
		}
		index--
		if index < 0 || index >= len(args) {
			return ""
		}
		return args[index]
	})

	result = sliceArgPattern.ReplaceAllStringFunc(result, func(match string) string {
		groups := sliceArgPattern.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		start, err := parsePositiveInt(groups[1])
		if err != nil {
			return ""
		}
		start--
		if start < 0 {
			start = 0
		}

		if len(groups) >= 3 && groups[2] != "" {
			length, err := parsePositiveInt(groups[2])
			if err != nil {
				return ""
			}
			end := min(start+length, len(args))
			if start > len(args) {
				start = len(args)
			}
			return strings.Join(args[start:end], " ")
		}
		if start > len(args) {
			start = len(args)
		}
		return strings.Join(args[start:], " ")
	})

	allArgs := strings.Join(args, " ")
	result = strings.ReplaceAll(result, "$ARGUMENTS", allArgs)
	result = strings.ReplaceAll(result, "$@", allArgs)
	return result
}

// LoadPromptTemplates reads prompt templates from default directories and
// explicit paths.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/prompt-templates.ts:194-263.
func LoadPromptTemplates(opts LoadPromptTemplatesOptions) ([]PromptTemplate, []ResourceDiagnostic, error) {
	resolvedCWD, err := resolvePath(opts.CWD, "")
	if err != nil {
		return nil, nil, err
	}
	resolvedAgentDir, err := resolvePath(opts.AgentDir, "")
	if err != nil {
		return nil, nil, err
	}

	templates := []PromptTemplate{}
	diagnostics := []ResourceDiagnostic{}
	globalPromptsDir := filepath.Join(resolvedAgentDir, "prompts")
	projectPromptsDir := filepath.Join(resolvedCWD, config.ConfigDirName, "prompts")

	if opts.IncludeDefaults {
		loaded, loadDiagnostics := loadTemplatesFromDir(globalPromptsDir, globalPromptsDir, SourceGlobal)
		templates = append(templates, loaded...)
		diagnostics = append(diagnostics, loadDiagnostics...)

		loaded, loadDiagnostics = loadTemplatesFromDir(projectPromptsDir, projectPromptsDir, SourceProject)
		templates = append(templates, loaded...)
		diagnostics = append(diagnostics, loadDiagnostics...)
	}

	for _, rawPath := range opts.Paths {
		resolvedPath, err := resolvePath(strings.TrimSpace(rawPath), resolvedCWD)
		if err != nil {
			return templates, diagnostics, err
		}
		info, err := os.Stat(resolvedPath)
		if err != nil {
			continue
		}
		if info.IsDir() {
			loaded, loadDiagnostics := loadTemplatesFromDir(resolvedPath, resolvedPath, SourceExplicit)
			templates = append(templates, loaded...)
			diagnostics = append(diagnostics, loadDiagnostics...)
			continue
		}
		if info.Mode().IsRegular() && strings.HasSuffix(resolvedPath, ".md") {
			template, err := loadTemplateFromFile(resolvedPath, templateSourceInfo(SourceExplicit, resolvedPath, filepath.Dir(resolvedPath)))
			if err == nil {
				templates = append(templates, template)
			} else {
				diagnostics = append(diagnostics, templateWarningDiagnostic(err, resolvedPath))
			}
		}
	}

	return templates, diagnostics, nil
}

// DedupePromptTemplates keeps the first template per name and reports each
// loser as a collision diagnostic.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/resource-loader.ts:800-824.
func DedupePromptTemplates(templates []PromptTemplate) ([]PromptTemplate, []ResourceDiagnostic) {
	seen := map[string]PromptTemplate{}
	deduped := make([]PromptTemplate, 0, len(templates))
	diagnostics := []ResourceDiagnostic{}
	for _, t := range templates {
		existing, ok := seen[t.Name]
		if ok {
			diagnostics = append(diagnostics, ResourceDiagnostic{
				Type:    DiagnosticCollision,
				Message: fmt.Sprintf("name %q collision", "/"+t.Name),
				Path:    t.Source.Path,
				Collision: &ResourceCollision{
					ResourceType: ResourceTypePrompt,
					Name:         t.Name,
					WinnerPath:   existing.Source.Path,
					LoserPath:    t.Source.Path,
				},
			})
			continue
		}
		seen[t.Name] = t
		deduped = append(deduped, t)
	}
	return deduped, diagnostics
}

// ExpandPromptTemplate applies a matching slash template, or returns text unchanged.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/prompt-templates.ts:269-285.
func ExpandPromptTemplate(text string, templates []PromptTemplate) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}

	match := slashTemplatePattern.FindStringSubmatch(text)
	if match == nil {
		return text
	}

	templateName := match[1]
	argsString := ""
	if len(match) > 2 {
		argsString = match[2]
	}

	for _, template := range templates {
		if template.Name == templateName {
			return SubstituteArgs(template.Content, ParseCommandArgs(argsString))
		}
	}
	return text
}

func loadTemplatesFromDir(
	dir string,
	baseDir string,
	kind SourceKind,
) ([]PromptTemplate, []ResourceDiagnostic) {
	templates := []PromptTemplate{}
	diagnostics := []ResourceDiagnostic{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return templates, diagnostics
	}

	for _, entry := range entries {
		fullPath := filepath.Join(dir, entry.Name())
		isFile := entry.Type().IsRegular()
		if entry.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(fullPath)
			if err != nil {
				if strings.HasSuffix(entry.Name(), ".md") {
					diagnostics = append(diagnostics, templateWarningDiagnostic(err, fullPath))
				}
				continue
			}
			isFile = info.Mode().IsRegular()
		}
		if !isFile || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		template, err := loadTemplateFromFile(fullPath, templateSourceInfo(kind, fullPath, baseDir))
		if err == nil {
			templates = append(templates, template)
		} else {
			diagnostics = append(diagnostics, templateWarningDiagnostic(err, fullPath))
		}
	}

	return templates, diagnostics
}

func loadTemplateFromFile(filePath string, source SourceInfo) (PromptTemplate, error) {
	rawContent, err := os.ReadFile(filePath)
	if err != nil {
		return PromptTemplate{}, err
	}
	frontmatter, body, err := ParseFrontmatter(string(rawContent))
	if err != nil {
		return PromptTemplate{}, err
	}

	description := frontmatter["description"]
	if description == "" {
		description = firstNonEmptyLine(body)
		if len([]rune(description)) > 60 {
			description = string([]rune(description)[:60]) + "..."
		}
	}

	return PromptTemplate{
		Name:         strings.TrimSuffix(filepath.Base(filePath), ".md"),
		Description:  description,
		ArgumentHint: frontmatter["argument-hint"],
		Content:      body,
		Source:       source,
	}, nil
}

func firstNonEmptyLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

func templateWarningDiagnostic(err error, path string) ResourceDiagnostic {
	return ResourceDiagnostic{
		Type:    DiagnosticWarning,
		Message: "load prompt template: " + err.Error(),
		Path:    path,
	}
}

func templateSourceInfo(kind SourceKind, path string, baseDir string) SourceInfo {
	return NewSourceInfo(kind, path, baseDir)
}

func parsePositiveInt(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid integer %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
