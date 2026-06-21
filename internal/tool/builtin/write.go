package builtin

import (
	"context"
	"fmt"
	"unicode/utf16"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

type writeToolArgs struct {
	Path    string `json:"path"    desc:"Path to the file to write (relative or absolute)"`
	Content string `json:"content" desc:"Content to write to the file"`
}

// NewWriteTool creates the built-in write tool.
func NewWriteTool(env execenv.ExecutionEnv) extension.ToolDefinition {
	return extension.ToolDefinition{
		Name: "write",
		Description: "Write content to a file. Creates the file if it doesn't exist, " +
			"overwrites if it does. Automatically creates parent directories.",
		Parameters:    schema[writeToolArgs](),
		Label:         "write",
		PromptSnippet: "Create or overwrite a file with the given content",
		ExecutionMode: tool.Sequential,
		RenderCall:    writeRenderCall,
		RenderResult:  writeRenderResult,
		Execute: func(ctx context.Context, call extension.ToolCall, _ tool.UpdateCallback, _ extension.ExtensionContext) (tool.Result, error) {
			return executeWrite(ctx, env, call.Args), nil
		},
	}
}

func writeRenderCall(input extension.ToolCallRenderInput) []render.Block {
	path := argString(input.Args, "file_path", "path")
	if path == "" {
		return nil
	}
	blocks := []render.Block{render.Text("write " + path)}
	content := argString(input.Args, "content")
	if content != "" {
		blocks = append(blocks, outputCodeBlocks(
			content,
			languageFromPath(path),
			10,
			input.Expanded,
		)...)
	}
	return blocks
}

func writeRenderResult(input extension.ToolResultRenderInput) []render.Block {
	out := toolOutputText(input.Result.Content)
	if out == "" {
		return nil
	}
	return []render.Block{render.Text(out)}
}

func executeWrite(ctx context.Context, env execenv.ExecutionEnv, args map[string]any) tool.Result {
	a, err := decode[writeToolArgs](args)
	if err != nil {
		return errorResult("%s", err)
	}

	absolutePath, err := env.AbsolutePath(ctx, a.Path)
	if err != nil {
		return errorResult("Could not resolve file path: %s. %v", a.Path, err)
	}

	result, err := withFileMutationQueue(absolutePath, func() tool.Result {
		if err := env.WriteFile(ctx, a.Path, []byte(a.Content)); err != nil {
			return errorResult("Could not write file: %s. %v", a.Path, err)
		}
		contentLength := len(utf16.Encode([]rune(a.Content)))
		return textResult(fmt.Sprintf("Successfully wrote %d bytes to %s", contentLength, a.Path), nil)
	})
	if err != nil {
		return errorResult("Could not resolve file path: %s. %v", a.Path, err)
	}
	return result
}
