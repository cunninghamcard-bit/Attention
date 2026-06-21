package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

const defaultLsLimit = 500

type lsToolDetails struct {
	Truncation        *truncationResult `json:"truncation,omitempty"`
	EntryLimitReached int               `json:"entryLimitReached,omitempty"`
}

type lsToolArgs struct {
	Path  string `json:"path,omitempty"  desc:"Directory to list (default: current directory)"`
	Limit int    `json:"limit,omitempty" desc:"Maximum number of entries to return (default: 500)"`
}

// NewLsTool creates the built-in ls tool.
func NewLsTool(env execenv.ExecutionEnv) extension.ToolDefinition {
	return extension.ToolDefinition{
		Name: "ls",
		Description: fmt.Sprintf(
			"List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. "+
				"Includes dotfiles. Output is truncated to %d entries or %dKB (whichever is hit first).",
			defaultLsLimit,
			defaultMaxBytes/1024,
		),
		Parameters:    schema[lsToolArgs](),
		Label:         "ls",
		PromptSnippet: "List directory contents",
		RenderCall:    lsRenderCall,
		RenderResult:  lsRenderResult,
		Execute: func(ctx context.Context, call extension.ToolCall, _ tool.UpdateCallback, _ extension.ExtensionContext) (tool.Result, error) {
			return executeLs(ctx, env, call.Args), nil
		},
	}
}

func lsRenderCall(input extension.ToolCallRenderInput) []render.Block {
	path := argString(input.Args, "path")
	if path == "" {
		path = "."
	}
	return []render.Block{render.Text("ls " + path)}
}

func lsRenderResult(input extension.ToolResultRenderInput) []render.Block {
	out := toolOutputText(input.Result.Content)
	blocks := outputCodeBlocks(out, "", 20, input.Expanded)
	var d lsToolDetails
	if decodeDetails(input.Result.Details, &d) {
		if d.EntryLimitReached > 0 {
			blocks = append(blocks, render.Badge(fmt.Sprintf("entry limit %d reached", d.EntryLimitReached), "warning"))
		}
		if d.Truncation != nil && d.Truncation.Truncated {
			blocks = append(blocks, render.Badge("output truncated", "muted"))
		}
	}
	return blocks
}

func executeLs(ctx context.Context, env execenv.ExecutionEnv, args map[string]any) tool.Result {
	a, err := decode[lsToolArgs](args)
	if err != nil {
		return errorResult("%s", err)
	}
	path := a.Path
	if path == "" {
		path = "."
	}
	limit := a.Limit
	if limit == 0 {
		limit = defaultLsLimit
	}

	info, err := env.FileInfo(ctx, path)
	if err != nil {
		return errorResult("Path not found: %s. %v", path, err)
	}
	if !info.IsDir {
		return errorResult("Not a directory: %s", path)
	}

	entries, err := env.ListDir(ctx, path)
	if err != nil {
		return errorResult("Cannot read directory: %s. %v", path, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	outputEntries := make([]string, 0, min(len(entries), limit))
	entryLimitReached := 0
	for _, entry := range entries {
		if len(outputEntries) >= limit {
			entryLimitReached = limit
			break
		}
		name := entry.Name
		if entry.IsDir {
			name += "/"
		}
		outputEntries = append(outputEntries, name)
	}
	if len(outputEntries) == 0 {
		return textResult("(empty directory)", nil)
	}

	rawOutput := strings.Join(outputEntries, "\n")
	truncation := truncateHead(rawOutput, truncationOptions{maxLines: int(^uint(0) >> 1)})
	output := truncation.Content
	details := lsToolDetails{}
	notices := []string{}
	if entryLimitReached > 0 {
		notices = append(notices, fmt.Sprintf("%d entries limit reached. Use limit=%d for more", limit, limit*2))
		details.EntryLimitReached = limit
	}
	if truncation.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", formatSize(defaultMaxBytes)))
		details.Truncation = &truncation
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	if details.EntryLimitReached == 0 && details.Truncation == nil {
		return textResult(output, nil)
	}
	return textResult(output, details)
}
