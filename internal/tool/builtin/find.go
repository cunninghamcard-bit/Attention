package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

const defaultFindLimit = 1000

type findToolDetails struct {
	Truncation         *truncationResult `json:"truncation,omitempty"`
	ResultLimitReached int               `json:"resultLimitReached,omitempty"`
}

type findToolArgs struct {
	Pattern string `json:"pattern"          desc:"Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"`
	Path    string `json:"path,omitempty"   desc:"Directory to search in (default: current directory)"`
	Limit   int    `json:"limit,omitempty"  desc:"Maximum number of results (default: 1000)"`
}

// NewFindTool creates the built-in find tool.
func NewFindTool(env execenv.ExecutionEnv) extension.ToolDefinition {
	return extension.ToolDefinition{
		Name: "find",
		Description: fmt.Sprintf(
			"Search for files by glob pattern. Returns matching paths relative to the search directory. "+
				"Uses fd semantics for hidden files, binary paths, and ignore rules. Output is truncated "+
				"to %d results or %dKB (whichever is hit first).",
			defaultFindLimit,
			defaultMaxBytes/1024,
		),
		Parameters:    schema[findToolArgs](),
		Label:         "find",
		PromptSnippet: "Find files by glob pattern",
		RenderCall:    findRenderCall,
		RenderResult:  findRenderResult,
		Execute: func(ctx context.Context, call extension.ToolCall, _ tool.UpdateCallback, _ extension.ExtensionContext) (tool.Result, error) {
			return executeFind(ctx, env, call.Args), nil
		},
	}
}

func findRenderCall(input extension.ToolCallRenderInput) []render.Block {
	pattern := argString(input.Args, "pattern")
	if pattern == "" {
		return nil
	}
	text := "find " + pattern
	if path := argString(input.Args, "path"); path != "" {
		text += " in " + path
	}
	return []render.Block{render.Text(text)}
}

func findRenderResult(input extension.ToolResultRenderInput) []render.Block {
	out := toolOutputText(input.Result.Content)
	blocks := outputCodeBlocks(out, "", 20, input.Expanded)
	var d findToolDetails
	if decodeDetails(input.Result.Details, &d) {
		if d.ResultLimitReached > 0 {
			blocks = append(blocks, render.Badge(fmt.Sprintf("result limit %d reached", d.ResultLimitReached), "warning"))
		}
		if d.Truncation != nil && d.Truncation.Truncated {
			blocks = append(blocks, render.Badge("output truncated", "muted"))
		}
	}
	return blocks
}

func executeFind(ctx context.Context, env execenv.ExecutionEnv, args map[string]any) tool.Result {
	a, err := decode[findToolArgs](args)
	if err != nil {
		return errorResult("%s", err)
	}
	if a.Pattern == "" {
		return errorResult("missing required argument %q", "pattern")
	}
	pattern := a.Pattern
	searchPath := a.Path
	if searchPath == "" {
		searchPath = "."
	}
	limit := a.Limit
	if limit == 0 {
		limit = defaultFindLimit
	}

	searchRoot, err := env.AbsolutePath(ctx, searchPath)
	if err != nil {
		return errorResult("Path not found: %s. %v", searchPath, err)
	}
	info, err := env.FileInfo(ctx, searchRoot)
	if err != nil {
		return errorResult("Path not found: %s. %v", searchPath, err)
	}
	if !info.IsDir {
		return errorResult("Not a directory: %s", searchRoot)
	}

	fdDependency := fdSearchToolDependency()
	fdCommand, err := resolveSearchTool(ctx, env, fdDependency)
	if err != nil {
		return errorResult("%s", err)
	}

	// Args and path parsing mirror pi packages/coding-agent/src/core/tools/find.ts:226-315.
	fdArgs := []string{
		fdCommand,
		"--glob",
		"--color=never",
		"--hidden",
		"--no-require-git",
		"--max-results",
		strconv.Itoa(limit),
	}

	effectivePattern := pattern
	if strings.Contains(pattern, "/") {
		fdArgs = append(fdArgs, "--full-path")
		if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && pattern != "**" {
			effectivePattern = "**/" + pattern
		}
	}
	fdArgs = append(fdArgs, "--", effectivePattern, searchRoot)

	command, err := shellJoin(fdArgs)
	if err != nil {
		return errorResult("%s", err)
	}
	result, err := env.Exec(ctx, command, execenv.ExecOptions{
		Timeout: searchToolExecTimeout,
	})
	if err != nil {
		return errorResult("Failed to run fd: %v", err)
	}
	if commandNotFoundForAny(result.ExitCode, result.Stderr, "fd", "fdfind", fdCommand) {
		return missingSearchToolResult(fdDependency)
	}

	matches := parseFindOutput(result.Stdout, searchRoot)
	if result.ExitCode != 0 && len(matches) == 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = fmt.Sprintf("fd exited with code %d", result.ExitCode)
		}
		return errorResult("%s", message)
	}
	if len(matches) == 0 {
		return textResult("No files found matching pattern", nil)
	}

	resultLimitReached := len(matches) >= limit
	rawOutput := strings.Join(matches, "\n")
	truncation := truncateHead(rawOutput, truncationOptions{maxLines: int(^uint(0) >> 1)})
	output := truncation.Content
	details := findToolDetails{}
	notices := []string{}
	if resultLimitReached {
		notices = append(
			notices,
			fmt.Sprintf("%d results limit reached. Use limit=%d for more, or refine pattern", limit, limit*2),
		)
		details.ResultLimitReached = limit
	}
	if truncation.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", formatSize(defaultMaxBytes)))
		details.Truncation = &truncation
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	if details.ResultLimitReached == 0 && details.Truncation == nil {
		return textResult(output, nil)
	}
	return textResult(output, details)
}

func parseFindOutput(stdout string, searchRoot string) []string {
	relativized := []string{}
	for rawLine := range strings.SplitSeq(stdout, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" {
			continue
		}
		hadTrailingSlash := strings.HasSuffix(line, "/") || strings.HasSuffix(line, "\\")
		relativePath := line
		if strings.HasPrefix(line, searchRoot) {
			if len(line) > len(searchRoot) {
				relativePath = line[len(searchRoot)+1:]
			} else {
				relativePath = ""
			}
		} else if relative, err := filepath.Rel(searchRoot, line); err == nil {
			relativePath = relative
		}
		if hadTrailingSlash && !strings.HasSuffix(relativePath, "/") {
			relativePath += "/"
		}
		relativized = append(relativized, filepath.ToSlash(relativePath))
	}
	return relativized
}
