package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

type bashToolDetails struct {
	Truncation     *truncationResult `json:"truncation,omitempty"`
	FullOutputPath string            `json:"fullOutputPath,omitempty"`
}

type BashRun struct {
	Output         string
	ExitCode       *int
	Cancelled      bool
	Truncated      bool
	FullOutputPath string
}

type shellCapture struct {
	mu                       sync.Mutex
	ctx                      context.Context
	env                      execenv.ExecutionEnv
	prefix                   strings.Builder
	tailText                 string
	tailBytes                int
	tailStartsAtLineBoundary bool
	totalBytes               int
	completedLines           int
	hasOpenLine              bool
	currentLineBytes         int
	fullOutputPath           string
	persistErr               error
}

// Matches pi's BASH_UPDATE_THROTTLE_MS:
// .agents/references/pi/packages/coding-agent/src/core/tools/bash.ts:291-339.
const bashUpdateThrottle = 100 * time.Millisecond

type bashToolArgs struct {
	Command string `json:"command"          desc:"Bash command to execute"`
	Timeout int    `json:"timeout,omitempty" desc:"Timeout in seconds (optional, no default timeout)"`
}

// NewBashTool creates the built-in bash tool.
func NewBashTool(env execenv.ExecutionEnv, commandPrefix string, extraBinDirs ...string) extension.ToolDefinition {
	return extension.ToolDefinition{
		Name: "bash",
		Description: fmt.Sprintf(
			"Execute a bash command in the current working directory. Returns stdout and stderr. "+
				"Output is truncated to last %d lines or %dKB (whichever is hit first). "+
				"Optionally provide a timeout in seconds.",
			defaultMaxLines,
			defaultMaxBytes/1024,
		),
		Parameters:    schema[bashToolArgs](),
		Label:         "bash",
		PromptSnippet: "Run a bash command and return its output",
		Execute: func(ctx context.Context, call extension.ToolCall, onUpdate tool.UpdateCallback, _ extension.ExtensionContext) (tool.Result, error) {
			return executeBash(ctx, env, commandPrefix, extraBinDirs, call.Args, onUpdate), nil
		},
	}
}

// shellEnv mirrors pi's getShellEnv: bash commands run with the managed bin
// dir (downloaded rg/fd) prepended to PATH, case-insensitive key, no
// duplicates (utils/shell.ts:112-124). Returns nil when nothing to override.
func shellEnv(extraBinDirs []string) map[string]string {
	binDir, err := searchToolBinDir()
	dirs := append([]string(nil), extraBinDirs...)
	if err == nil {
		dirs = append(dirs, binDir)
	}
	if len(dirs) == 0 {
		return nil
	}
	pathKey := "PATH"
	current := ""
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "PATH") {
			pathKey = key
			current = value
			break
		}
	}
	prefix := []string{}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		seen := false
		for _, existing := range append(prefix, filepath.SplitList(current)...) {
			if existing == dir {
				seen = true
				break
			}
		}
		if !seen {
			prefix = append(prefix, dir)
		}
	}
	if len(prefix) == 0 {
		return nil
	}
	if current == "" {
		return map[string]string{pathKey: strings.Join(prefix, string(os.PathListSeparator))}
	}
	return map[string]string{pathKey: strings.Join(prefix, string(os.PathListSeparator)) + string(os.PathListSeparator) + current}
}

func executeBash(
	ctx context.Context,
	env execenv.ExecutionEnv,
	commandPrefix string,
	extraBinDirs []string,
	args map[string]any,
	onUpdate tool.UpdateCallback,
) tool.Result {
	a, err := decode[bashToolArgs](args)
	if err != nil {
		return errorResult("%s", err)
	}
	// pi prepends the configured prefix as its own line (tools/bash.ts:289).
	if commandPrefix != "" {
		a.Command = commandPrefix + "\n" + a.Command
	}

	var timeout time.Duration
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
	}

	capture := newShellCapture(ctx, env)
	var updateMu sync.Mutex
	var updateTimerWG sync.WaitGroup
	var updateTimer *time.Timer
	var updateDirty bool
	var updatesDone bool
	var lastUpdateAt time.Time
	emitUpdateLocked := func() {
		if onUpdate == nil || !updateDirty {
			return
		}
		updateDirty = false
		lastUpdateAt = time.Now()
		text, details := capture.formatSnapshot(false)
		onUpdate(textResult(text, details))
	}
	stopUpdateTimer := func() {
		updateMu.Lock()
		updatesDone = true
		if updateTimer != nil {
			timer := updateTimer
			updateTimer = nil
			if timer.Stop() {
				updateTimerWG.Done()
			}
		}
		updateMu.Unlock()
		updateTimerWG.Wait()
	}
	scheduleUpdate := func() {
		if onUpdate == nil {
			return
		}
		updateMu.Lock()
		defer updateMu.Unlock()
		if updatesDone {
			return
		}

		updateDirty = true
		if time.Since(lastUpdateAt) >= bashUpdateThrottle {
			if updateTimer != nil {
				timer := updateTimer
				updateTimer = nil
				if timer.Stop() {
					updateTimerWG.Done()
				}
			}
			emitUpdateLocked()
			return
		}
		if updateTimer != nil {
			return
		}

		delay := bashUpdateThrottle - time.Since(lastUpdateAt)
		updateTimerWG.Add(1)
		updateTimer = time.AfterFunc(delay, func() {
			defer updateTimerWG.Done()
			updateMu.Lock()
			defer updateMu.Unlock()
			updateTimer = nil
			if updatesDone {
				return
			}
			emitUpdateLocked()
		})
	}

	result, err := env.Exec(ctx, a.Command, execenv.ExecOptions{
		Timeout:  timeout,
		Env:      shellEnv(extraBinDirs),
		Stdout:   capture,
		Stderr:   capture,
		OnStdout: func(string) { scheduleUpdate() },
		OnStderr: func(string) { scheduleUpdate() },
	})
	stopUpdateTimer()
	if err != nil {
		text, details := capture.formatSnapshot(true)
		if text != "" {
			text += "\n\n"
		}
		text += executionErrorText(err, a.Timeout)
		return bashErrorResult(text, details)
	}

	text, details := capture.formatSnapshot(true)
	if text == "" {
		text = "(no output)"
	}
	if result.ExitCode != 0 {
		text = appendStatus(text, fmt.Sprintf("Command exited with code %d", result.ExitCode))
		return bashErrorResult(text, details)
	}
	return textResult(text, details)
}

// RunBash returns the out-of-band BashResult shape used by pi RPC bash:
// .agents/references/pi/packages/coding-agent/src/core/bash-executor.ts:29-39.
func RunBash(ctx context.Context, env execenv.ExecutionEnv, command string) BashRun {
	capture := newShellCapture(ctx, env)
	result, err := env.Exec(ctx, command, execenv.ExecOptions{
		Env:    shellEnv(nil),
		Stdout: capture,
		Stderr: capture,
	})
	if err != nil {
		text, details := capture.formatSnapshot(true)
		run := bashRunFromSnapshot(text, details)

		var execErr *execenv.ExecutionError
		if errors.As(err, &execErr) && execErr.Code == execenv.ExecutionErrorAborted {
			run.Cancelled = true
			return run
		}

		if run.Output != "" {
			run.Output += "\n\n"
		}
		run.Output += executionErrorText(err, 0)
		return run
	}

	text, details := capture.formatSnapshot(true)
	run := bashRunFromSnapshot(text, details)
	exitCode := result.ExitCode
	run.ExitCode = &exitCode
	return run
}

func bashRunFromSnapshot(text string, details bashToolDetails) BashRun {
	return BashRun{
		Output:         text,
		Truncated:      details.Truncation != nil,
		FullOutputPath: details.FullOutputPath,
	}
}

func newShellCapture(ctx context.Context, env execenv.ExecutionEnv) *shellCapture {
	return &shellCapture{
		ctx:                      context.WithoutCancel(ctx),
		env:                      env,
		tailStartsAtLineBoundary: true,
	}
}

// Write lets shellCapture serve as the execenv output sink, so env.Exec streams
// decoded output straight into the accumulator instead of buffering it twice.
func (c *shellCapture) Write(p []byte) (int, error) {
	c.append(string(p))
	return len(p), nil
}

func (c *shellCapture) append(chunk string) {
	text := sanitizeBinaryOutput(chunk)
	text = strings.ReplaceAll(text, "\r", "")
	if text == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.appendTailLocked(text)
	c.appendFullOutputLocked(text)
}

func (c *shellCapture) formatSnapshot(
	persistIfTruncated bool,
) (string, bashToolDetails) {
	c.mu.Lock()
	content := c.snapshotTextLocked()
	totalLines := c.totalLinesLocked()
	totalBytes := c.totalBytes
	currentLineBytes := c.currentLineBytes
	truncated := totalLines > defaultMaxLines || totalBytes > defaultMaxBytes
	if truncated && persistIfTruncated {
		c.ensureTempFileLocked()
	}
	fullOutputPath := c.fullOutputPath
	c.mu.Unlock()

	if !truncated {
		return content, bashToolDetails{}
	}

	truncation := truncateTail(content, truncationOptions{})
	truncation.Truncated = true
	if truncation.TruncatedBy == "" {
		if totalBytes > defaultMaxBytes {
			truncation.TruncatedBy = "bytes"
		} else {
			truncation.TruncatedBy = "lines"
		}
	}
	truncation.TotalLines = totalLines
	truncation.TotalBytes = totalBytes
	details := bashToolDetails{Truncation: &truncation}
	if fullOutputPath != "" {
		details.FullOutputPath = fullOutputPath
	}

	text := truncation.Content
	startLine := truncation.TotalLines - truncation.OutputLines + 1
	endLine := truncation.TotalLines
	if truncation.LastLinePartial {
		lastLineSize := formatSize(currentLineBytes)
		text += fmt.Sprintf(
			"\n\n[Showing last %s of line %d (line is %s). Full output: %s]",
			formatSize(truncation.OutputBytes),
			endLine,
			lastLineSize,
			details.FullOutputPath,
		)
	} else if truncation.TruncatedBy == "lines" {
		text += fmt.Sprintf(
			"\n\n[Showing lines %d-%d of %d. Full output: %s]",
			startLine,
			endLine,
			truncation.TotalLines,
			details.FullOutputPath,
		)
	} else {
		text += fmt.Sprintf(
			"\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]",
			startLine,
			endLine,
			truncation.TotalLines,
			formatSize(defaultMaxBytes),
			details.FullOutputPath,
		)
	}
	return text, details
}

func (c *shellCapture) appendTailLocked(text string) {
	textBytes := len(text)
	c.totalBytes += textBytes
	if lastNewline := strings.LastIndex(text, "\n"); lastNewline == -1 {
		c.currentLineBytes += textBytes
		c.hasOpenLine = true
	} else {
		c.completedLines += strings.Count(text, "\n")
		tail := text[lastNewline+1:]
		c.currentLineBytes = len(tail)
		c.hasOpenLine = tail != ""
	}

	c.tailText += text
	c.tailBytes += textBytes
	if c.tailBytes > bashRollingMaxBytes*2 {
		c.trimTailLocked()
	}
}

func (c *shellCapture) appendFullOutputLocked(text string) {
	if c.persistErr != nil {
		return
	}
	if c.fullOutputPath != "" {
		if err := c.env.AppendFile(c.ctx, c.fullOutputPath, []byte(text)); err != nil {
			c.recordPersistErrLocked(err)
		}
		return
	}

	c.prefix.WriteString(text)
	if c.shouldUseTempFileLocked() {
		c.ensureTempFileLocked()
	}
}

func (c *shellCapture) ensureTempFileLocked() {
	if c.fullOutputPath != "" || c.persistErr != nil {
		return
	}
	path, err := c.env.CreateTempFile(c.ctx, "bash-", ".log")
	if err != nil {
		c.recordPersistErrLocked(err)
		return
	}
	if c.prefix.Len() > 0 {
		if err := c.env.WriteFile(c.ctx, path, []byte(c.prefix.String())); err != nil {
			c.recordPersistErrLocked(err)
			return
		}
	}
	c.prefix.Reset()
	c.fullOutputPath = path
}

func (c *shellCapture) recordPersistErrLocked(err error) {
	if c.persistErr == nil {
		c.persistErr = err
	}
	c.prefix.Reset()
	c.fullOutputPath = ""
}

func (c *shellCapture) shouldUseTempFileLocked() bool {
	return c.totalBytes > defaultMaxBytes || c.totalLinesLocked() > defaultMaxLines
}

func (c *shellCapture) totalLinesLocked() int {
	if c.totalBytes == 0 {
		return 0
	}
	totalLines := c.completedLines
	if c.hasOpenLine {
		totalLines++
	}
	return totalLines
}

func (c *shellCapture) trimTailLocked() {
	buffer := []byte(c.tailText)
	if len(buffer) <= bashRollingMaxBytes {
		c.tailBytes = len(buffer)
		return
	}

	start := len(buffer) - bashRollingMaxBytes
	for start < len(buffer) && (buffer[start]&0xc0) == 0x80 {
		start++
	}
	if start > 0 {
		c.tailStartsAtLineBoundary = buffer[start-1] == '\n'
	}
	c.tailText = string(buffer[start:])
	c.tailBytes = len(c.tailText)
}

func (c *shellCapture) snapshotTextLocked() string {
	if c.tailStartsAtLineBoundary {
		return c.tailText
	}
	firstNewline := strings.Index(c.tailText, "\n")
	if firstNewline == -1 {
		return c.tailText
	}
	return c.tailText[firstNewline+1:]
}

func appendStatus(text string, status string) string {
	if text == "" {
		return status
	}
	return text + "\n\n" + status
}

func executionErrorText(err error, timeoutSeconds int) string {
	if execErr, ok := errors.AsType[*execenv.ExecutionError](err); ok {
		switch execErr.Code {
		case execenv.ExecutionErrorAborted:
			return "Command aborted"
		case execenv.ExecutionErrorTimeout:
			if timeoutSeconds > 0 {
				return fmt.Sprintf("Command timed out after %d seconds", timeoutSeconds)
			}
			return "Command timed out"
		}
	}
	return err.Error()
}

func bashErrorResult(text string, details bashToolDetails) tool.Result {
	detailMap := map[string]any{
		"isError": true,
		"error":   text,
	}
	if details.Truncation != nil {
		detailMap["truncation"] = details.Truncation
	}
	if details.FullOutputPath != "" {
		detailMap["fullOutputPath"] = details.FullOutputPath
	}
	return tool.Result{
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
		Details: detailMap,
		IsError: true,
	}
}

func sanitizeBinaryOutput(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	for _, r := range text {
		if r == '\t' || r == '\n' || r == '\r' {
			builder.WriteRune(r)
			continue
		}
		if r <= 0x1f {
			continue
		}
		if r >= 0xfff9 && r <= 0xfffb {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}
