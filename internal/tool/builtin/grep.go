package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

const (
	defaultGrepLimit      = 100
	searchToolExecTimeout = 30 * time.Second
)

type grepToolDetails struct {
	Matches           []grepMatch       `json:"matches,omitempty"`
	Truncation        *truncationResult `json:"truncation,omitempty"`
	MatchLimitReached int               `json:"matchLimitReached,omitempty"`
	LinesTruncated    bool              `json:"linesTruncated,omitempty"`
}

type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type rgMatch struct {
	filePath   string
	lineNumber int
	lineText   *string
}

type rgJSONEvent struct {
	Type string `json:"type"`
	Data struct {
		Path *struct {
			Text string `json:"text"`
		} `json:"path"`
		LineNumber int `json:"line_number"`
		Lines      *struct {
			Text string `json:"text"`
		} `json:"lines"`
	} `json:"data"`
}

type rgOutputCapture struct {
	mu                sync.Mutex
	stdout            strings.Builder
	stderr            strings.Builder
	partial           string
	matchCount        int
	matchLimitReached bool
	limit             int
	cancel            context.CancelFunc
}

type grepToolArgs struct {
	Pattern    string `json:"pattern"              desc:"Search pattern (regex or literal string)"`
	Path       string `json:"path,omitempty"       desc:"Directory or file to search (default: current directory)"`
	Glob       string `json:"glob,omitempty"       desc:"Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"`
	IgnoreCase bool   `json:"ignoreCase,omitempty" desc:"Case-insensitive search (default: false)"`
	Literal    bool   `json:"literal,omitempty"    desc:"Treat pattern as literal string instead of regex (default: false)"`
	Context    int    `json:"context,omitempty"    desc:"Number of lines to show before and after each match (default: 0)"`
	Limit      int    `json:"limit,omitempty"      desc:"Maximum number of matches to return (default: 100)"`
}

// NewGrepTool creates the built-in grep tool.
func NewGrepTool(env execenv.ExecutionEnv) extension.ToolDefinition {
	return extension.ToolDefinition{
		Name: "grep",
		Description: fmt.Sprintf(
			"Search file contents for a pattern. Returns matching lines with file paths and line numbers. "+
				"Output is truncated to %d matches or %dKB (whichever is hit first). "+
				"Long lines are truncated to %d chars.",
			defaultGrepLimit,
			defaultMaxBytes/1024,
			grepMaxLineLength,
		),
		Parameters:    schema[grepToolArgs](),
		Label:         "grep",
		PromptSnippet: "Search file contents for a pattern",
		RenderCall:    grepRenderCall,
		RenderResult:  grepRenderResult,
		Execute: func(ctx context.Context, call extension.ToolCall, _ tool.UpdateCallback, _ extension.ExtensionContext) (tool.Result, error) {
			return executeGrep(ctx, env, call.Args), nil
		},
	}
}

func executeGrep(ctx context.Context, env execenv.ExecutionEnv, args map[string]any) tool.Result {
	a, err := decode[grepToolArgs](args)
	if err != nil {
		return errorResult("%s", err)
	}
	searchPath := a.Path
	if searchPath == "" {
		searchPath = "."
	}
	limit := a.Limit
	if limit == 0 {
		limit = defaultGrepLimit
	}

	absoluteSearchPath, err := env.AbsolutePath(ctx, searchPath)
	if err != nil {
		return errorResult("Path not found: %s. %v", searchPath, err)
	}
	rootInfo, err := env.FileInfo(ctx, absoluteSearchPath)
	if err != nil {
		return errorResult("Path not found: %s. %v", searchPath, err)
	}

	rgDependency := ripgrepSearchToolDependency()
	rgCommand, err := resolveSearchTool(ctx, env, rgDependency)
	if err != nil {
		return errorResult("%s", err)
	}

	// Args mirror pi packages/coding-agent/src/core/tools/grep.ts:214-218.
	// Context formatting follows the match-then-read flow at grep.ts:249-329.
	rgArgs := []string{rgCommand, "--json", "--line-number", "--color=never", "--hidden"}
	if a.IgnoreCase {
		rgArgs = append(rgArgs, "--ignore-case")
	}
	if a.Literal {
		rgArgs = append(rgArgs, "--fixed-strings")
	}
	if a.Glob != "" {
		rgArgs = append(rgArgs, "--glob", a.Glob)
	}
	if a.Context > 0 {
		rgArgs = append(rgArgs, "--context", fmt.Sprint(a.Context))
	}
	rgArgs = append(rgArgs, "--", a.Pattern, absoluteSearchPath)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	capture := newRGOutputCapture(limit, cancel)
	command, err := shellJoin(rgArgs)
	if err != nil {
		return errorResult("%s", err)
	}
	result, execErr := env.Exec(runCtx, command, execenv.ExecOptions{
		Timeout:  searchToolExecTimeout,
		OnStdout: capture.appendStdout,
		OnStderr: capture.appendStderr,
	})

	stdout := capture.stdoutString()
	stderr := capture.stderrString()
	matchLimitReached := capture.matchLimitReachedValue()
	if execErr != nil {
		if !matchLimitReached {
			return errorResult("Failed to run ripgrep: %v", execErr)
		}
	} else {
		stdout = result.Stdout
		stderr = result.Stderr
		if commandNotFoundForAny(result.ExitCode, stderr, "rg", rgCommand) {
			return missingSearchToolResult(rgDependency)
		}
		if result.ExitCode != 0 && result.ExitCode != 1 {
			message := strings.TrimSpace(stderr)
			if message == "" {
				message = fmt.Sprintf("ripgrep exited with code %d", result.ExitCode)
			}
			return errorResult("%s", message)
		}
	}

	matches, parsedLimitReached, err := parseRGMatches(stdout, limit)
	if err != nil {
		return errorResult("Could not parse ripgrep output: %v", err)
	}
	matchLimitReached = matchLimitReached || parsedLimitReached
	if len(matches) == 0 {
		return textResult("No matches found", nil)
	}

	outputLines := []string{}
	renderMatches := []grepMatch{}
	linesTruncated := false
	for _, match := range matches {
		displayPath := grepDisplayPath(rootInfo, match.filePath)
		if a.Context == 0 && match.lineText != nil {
			lineText := sanitizeRGLineText(*match.lineText)
			lineText, truncated := truncateLine(lineText)
			linesTruncated = linesTruncated || truncated
			renderMatches = append(renderMatches, grepMatch{
				Path: displayPath,
				Line: match.lineNumber,
				Text: lineText,
			})
			outputLines = append(outputLines, fmt.Sprintf("%s:%d: %s", displayPath, match.lineNumber, lineText))
			continue
		}

		block, lineText, truncated := formatGrepBlockFromFile(ctx, env, rootInfo, match.filePath, match.lineNumber, a.Context)
		linesTruncated = linesTruncated || truncated
		renderMatches = append(renderMatches, grepMatch{
			Path: displayPath,
			Line: match.lineNumber,
			Text: lineText,
		})
		outputLines = append(outputLines, block...)
	}

	rawOutput := strings.Join(outputLines, "\n")
	truncation := truncateHead(rawOutput, truncationOptions{maxLines: int(^uint(0) >> 1)})
	output := truncation.Content
	details := grepToolDetails{Matches: renderMatches}
	notices := []string{}
	if matchLimitReached {
		notices = append(
			notices,
			fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", limit, limit*2),
		)
		details.MatchLimitReached = limit
	}
	if truncation.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", formatSize(defaultMaxBytes)))
		details.Truncation = &truncation
	}
	if linesTruncated {
		notices = append(
			notices,
			fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", grepMaxLineLength),
		)
		details.LinesTruncated = true
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	return textResult(output, details)
}

func grepRenderCall(input extension.ToolCallRenderInput) []render.Block {
	pattern, ok := input.Args["pattern"].(string)
	if !ok {
		return nil
	}
	text := fmt.Sprintf("grep %q", pattern)
	if path, ok := input.Args["path"].(string); ok && path != "" {
		text += " in " + path
	}
	return []render.Block{render.Text(text)}
}

func grepRenderResult(input extension.ToolResultRenderInput) []render.Block {
	var d grepToolDetails
	if !decodeDetails(input.Result.Details, &d) || len(d.Matches) == 0 {
		return nil
	}
	blocks := []render.Block{}
	i := 0
	for i < len(d.Matches) {
		path := d.Matches[i].Path
		lines := []render.Block{}
		for i < len(d.Matches) && d.Matches[i].Path == path {
			m := d.Matches[i]
			lines = append(lines, render.StyledText(fmt.Sprintf("%d: %s", m.Line, m.Text), "match"))
			i++
		}
		blocks = append(blocks, render.Group(path, lines))
	}
	if d.MatchLimitReached > 0 {
		blocks = append(
			blocks,
			render.Badge(fmt.Sprintf("match limit %d reached", d.MatchLimitReached), "warning"),
		)
	}
	if d.LinesTruncated {
		blocks = append(blocks, render.Badge("some lines truncated", "muted"))
	}
	if d.Truncation != nil && d.Truncation.Truncated {
		blocks = append(blocks, render.Badge("output truncated", "muted"))
	}
	return blocks
}

func newRGOutputCapture(limit int, cancel context.CancelFunc) *rgOutputCapture {
	return &rgOutputCapture{
		limit:  limit,
		cancel: cancel,
	}
}

func (c *rgOutputCapture) appendStdout(chunk string) {
	shouldCancel := false
	c.mu.Lock()
	c.stdout.WriteString(chunk)
	if !c.matchLimitReached {
		shouldCancel = c.observeStdoutLinesLocked(chunk)
	}
	c.mu.Unlock()

	if shouldCancel {
		c.cancel()
	}
}

func (c *rgOutputCapture) appendStderr(chunk string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stderr.WriteString(chunk)
}

func (c *rgOutputCapture) stdoutString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdout.String()
}

func (c *rgOutputCapture) stderrString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderr.String()
}

func (c *rgOutputCapture) matchLimitReachedValue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.matchLimitReached
}

func (c *rgOutputCapture) observeStdoutLinesLocked(chunk string) bool {
	text := c.partial + chunk
	parts := strings.Split(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		c.partial = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	} else {
		c.partial = ""
	}

	for _, line := range parts {
		if !isRGMatchEvent(line) {
			continue
		}
		c.matchCount++
		if c.matchCount >= c.limit {
			c.matchLimitReached = true
			return true
		}
	}
	return false
}

func isRGMatchEvent(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return false
	}
	return event.Type == "match"
}

func parseRGMatches(stdout string, limit int) ([]rgMatch, bool, error) {
	matches := []rgMatch{}
	matchCount := 0
	matchLimitReached := false
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event rgJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Type != "match" {
			continue
		}
		matchCount++
		if event.Data.Path != nil && event.Data.Path.Text != "" && event.Data.LineNumber > 0 {
			var lineText *string
			if event.Data.Lines != nil {
				text := event.Data.Lines.Text
				lineText = &text
			}
			matches = append(matches, rgMatch{
				filePath:   event.Data.Path.Text,
				lineNumber: event.Data.LineNumber,
				lineText:   lineText,
			})
		}
		if matchCount >= limit {
			matchLimitReached = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return matches, matchLimitReached, nil
}

func formatGrepBlockFromFile(
	ctx context.Context,
	env execenv.ExecutionEnv,
	root execenv.FileInfo,
	filePath string,
	lineNumber int,
	contextLines int,
) ([]string, string, bool) {
	displayPath := grepDisplayPath(root, filePath)
	content, err := env.ReadTextFile(ctx, filePath)
	if err != nil {
		return []string{fmt.Sprintf("%s:%d: (unable to read file)", displayPath, lineNumber)}, "(unable to read file)", false
	}
	lines := strings.Split(NormalizeToLF(content), "\n")
	if lineNumber < 1 || lineNumber > len(lines) {
		return []string{fmt.Sprintf("%s:%d: (unable to read file)", displayPath, lineNumber)}, "(unable to read file)", false
	}

	start := lineNumber
	end := lineNumber
	if contextLines > 0 {
		start = max(1, lineNumber-contextLines)
		end = min(len(lines), lineNumber+contextLines)
	}

	block := make([]string, 0, end-start+1)
	matchedText := ""
	linesTruncated := false
	for current := start; current <= end; current++ {
		lineText, truncated := truncateLine(lines[current-1])
		linesTruncated = linesTruncated || truncated
		if current == lineNumber {
			matchedText = lineText
			block = append(block, fmt.Sprintf("%s:%d: %s", displayPath, current, lineText))
		} else {
			block = append(block, fmt.Sprintf("%s-%d- %s", displayPath, current, lineText))
		}
	}
	return block, matchedText, linesTruncated
}

func grepDisplayPath(root execenv.FileInfo, filePath string) string {
	if root.IsDir {
		relative, err := filepath.Rel(root.Path, filePath)
		if err == nil && relative != "." && !strings.HasPrefix(relative, "..") {
			return filepath.ToSlash(relative)
		}
	}
	return filepath.Base(filePath)
}

func sanitizeRGLineText(lineText string) string {
	lineText = strings.ReplaceAll(lineText, "\r\n", "\n")
	lineText = strings.ReplaceAll(lineText, "\r", "")
	return strings.TrimSuffix(lineText, "\n")
}

func isShellCommandNotFound(exitCode int, stderr string, command string) bool {
	if exitCode == 127 {
		return true
	}
	lowerStderr := strings.ToLower(stderr)
	command = strings.ToLower(command)
	return strings.Contains(lowerStderr, command+": command not found") ||
		strings.Contains(lowerStderr, command+": not found") ||
		strings.Contains(lowerStderr, "command not found: "+command)
}
