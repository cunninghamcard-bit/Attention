package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
	"golang.org/x/text/unicode/norm"
)

type readToolDetails struct {
	Truncation *truncationResult `json:"truncation,omitempty"`
}

var readImageMimeTypes = map[string]struct{}{
	"image/gif":  {},
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

var macOSScreenshotAMPMPathPattern = regexp.MustCompile(` (?i:(AM|PM))\.`)

type readToolArgs struct {
	Path   string `json:"path"             desc:"Path to the file to read (relative or absolute)"`
	Offset int    `json:"offset,omitempty" desc:"Line number to start reading from (1-indexed)"`
	Limit  int    `json:"limit,omitempty"  desc:"Maximum number of lines to read"`
}

// NewReadTool creates the built-in read tool.
func NewReadTool(env execenv.ExecutionEnv) extension.ToolDefinition {
	return extension.ToolDefinition{
		Name: "read",
		// Mirrors pi's read tool image description:
		// .agents/references/pi/packages/coding-agent/src/core/tools/read.ts:213-217.
		Description: fmt.Sprintf(
			"Read the contents of a file. Supports text files and images (jpg/jpeg, png, gif, webp). "+
				"Images are sent as attachments. "+
				"For text files, output is truncated to %d lines or %dKB (whichever is hit first). "+
				"Use offset/limit for large files. When you need the full file, continue with offset until complete.",
			defaultMaxLines,
			defaultMaxBytes/1024,
		),
		Parameters:    schema[readToolArgs](),
		Label:         "read",
		PromptSnippet: "Read a file's contents",
		RenderCall:    readRenderCall,
		RenderResult:  readRenderResult,
		Execute: func(ctx context.Context, call extension.ToolCall, _ tool.UpdateCallback, extCtx extension.ExtensionContext) (tool.Result, error) {
			return executeRead(ctx, env, call.Args, extCtx), nil
		},
	}
}

func readRenderCall(input extension.ToolCallRenderInput) []render.Block {
	path := argString(input.Args, "file_path", "path")
	if path == "" {
		return nil
	}
	header := "read " + path
	offset, hasOffset := renderIntArg(input.Args, "offset")
	limit, hasLimit := renderIntArg(input.Args, "limit")
	if hasOffset || hasLimit {
		start := 1
		if hasOffset {
			start = offset
		}
		header += fmt.Sprintf(":%d", start)
		if hasLimit {
			header += fmt.Sprintf("-%d", start+limit-1)
		}
	}
	return []render.Block{render.Text(header)}
}

func readRenderResult(input extension.ToolResultRenderInput) []render.Block {
	blocks := toolOutputImages(input.Result.Content)
	out := toolOutputText(input.Result.Content)
	lang := languageFromPath(argString(input.Args, "file_path", "path"))
	blocks = append(blocks, outputCodeBlocks(out, lang, 10, input.Expanded)...)
	var d readToolDetails
	if decodeDetails(input.Result.Details, &d) && d.Truncation != nil && d.Truncation.Truncated {
		blocks = append(blocks, render.Badge("output truncated", "muted"))
	}
	return blocks
}

func renderIntArg(args map[string]any, key string) (int, bool) {
	switch value := args[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	case json.Number:
		n, err := value.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

func executeRead(
	ctx context.Context,
	env execenv.ExecutionEnv,
	args map[string]any,
	extCtx extension.ExtensionContext,
) tool.Result {
	a, err := decode[readToolArgs](args)
	if err != nil {
		return errorResult("%s", err)
	}
	hasOffset := a.Offset > 0
	hasLimit := a.Limit > 0

	resolvedPath, err := resolveReadPath(ctx, env, a.Path)
	if err != nil {
		return errorResult("Could not read file: %s. %v", a.Path, err)
	}

	content, err := env.ReadBinaryFile(ctx, resolvedPath)
	if err != nil {
		return errorResult("Could not read file: %s. %v", a.Path, err)
	}

	if mimeType, ok := imageMimeTypeForReadContent(content); ok {
		// Mirrors pi's image result shape (text note + image block):
		// .agents/references/pi/packages/coding-agent/src/core/tools/read.ts:250-276.
		resized := resizeReadImage(content, mimeType)
		if resized == nil {
			text := fmt.Sprintf(
				"Read image file [%s]\n[Image omitted: could not be resized below the inline image size limit.]",
				mimeType,
			)
			if note := nonVisionImageNote(extCtx); note != "" {
				text += "\n" + note
			}
			return textResult(text, nil)
		}

		text := fmt.Sprintf("Read image file [%s]", resized.MimeType)
		if note := formatReadImageDimensionNote(resized); note != "" {
			text += "\n" + note
		}
		if note := nonVisionImageNote(extCtx); note != "" {
			text += "\n" + note
		}
		return tool.Result{
			Content: []ai.ContentBlock{
				{Type: ai.ContentText, Text: text},
				{
					Type:      ai.ContentImage,
					ImageData: base64.StdEncoding.EncodeToString(resized.Data),
					MimeType:  resized.MimeType,
				},
			},
		}
	}

	textContent := string(content)
	lines := strings.Split(textContent, "\n")
	start := 0
	if hasOffset {
		start = a.Offset - 1
	}
	if start >= len(lines) {
		return errorResult("Offset %d is beyond end of file (%d lines total)", a.Offset, len(lines))
	}

	selected := lines[start:]
	userLimitedLines := 0
	if hasLimit {
		end := min(start+a.Limit, len(lines))
		selected = lines[start:end]
		userLimitedLines = end - start
	}

	selectedContent := strings.Join(selected, "\n")
	truncation := truncateHead(selectedContent, truncationOptions{})
	startLineDisplay := start + 1
	if truncation.FirstLineExceedsLimit {
		firstLineSize := formatSize(len([]byte(lines[start])))
		text := fmt.Sprintf(
			"[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startLineDisplay,
			firstLineSize,
			formatSize(defaultMaxBytes),
			startLineDisplay,
			a.Path,
			defaultMaxBytes,
		)
		return textResult(text, readToolDetails{Truncation: &truncation})
	}
	if truncation.Truncated {
		endLineDisplay := startLineDisplay + truncation.OutputLines - 1
		nextOffset := endLineDisplay + 1
		text := truncation.Content
		if truncation.TruncatedBy == "lines" {
			text += fmt.Sprintf(
				"\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]",
				startLineDisplay,
				endLineDisplay,
				len(lines),
				nextOffset,
			)
		} else {
			text += fmt.Sprintf(
				"\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]",
				startLineDisplay,
				endLineDisplay,
				len(lines),
				formatSize(defaultMaxBytes),
				nextOffset,
			)
		}
		return textResult(text, readToolDetails{Truncation: &truncation})
	}
	if hasLimit && start+userLimitedLines < len(lines) {
		remaining := len(lines) - (start + userLimitedLines)
		nextOffset := start + userLimitedLines + 1
		text := fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", truncation.Content, remaining, nextOffset)
		return textResult(text, nil)
	}

	return textResult(truncation.Content, nil)
}

func resolveReadPath(ctx context.Context, env execenv.ExecutionEnv, path string) (string, error) {
	resolved, err := env.AbsolutePath(ctx, path)
	if err != nil {
		return "", err
	}

	for _, candidate := range readPathVariants(resolved) {
		exists, err := env.Exists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if exists {
			return candidate, nil
		}
	}
	return resolved, nil
}

func readPathVariants(path string) []string {
	ampm := macOSScreenshotAMPMPathVariant(path)
	nfd := nfdPathVariant(path)
	variants := []string{path}
	for _, candidate := range []string{
		ampm,
		nfd,
		curlyApostrophePathVariant(path),
		curlyApostrophePathVariant(nfd),
	} {
		if !slices.Contains(variants, candidate) {
			variants = append(variants, candidate)
		}
	}
	return variants
}

func macOSScreenshotAMPMPathVariant(path string) string {
	return macOSScreenshotAMPMPathPattern.ReplaceAllString(path, "\u202f$1.")
}

func curlyApostrophePathVariant(path string) string {
	return strings.ReplaceAll(path, "'", "\u2019")
}

func nfdPathVariant(path string) string {
	return norm.NFD.String(path)
}

func imageMimeTypeForReadContent(content []byte) (string, bool) {
	if len(content) >= 12 && string(content[0:4]) == "RIFF" && string(content[8:12]) == "WEBP" {
		return "image/webp", true
	}
	detected := http.DetectContentType(content)
	if _, ok := readImageMimeTypes[detected]; ok {
		return detected, true
	}
	return "", false
}

func nonVisionImageNote(extCtx extension.ExtensionContext) string {
	if extCtx.Model == nil {
		return ""
	}
	model := extCtx.Model()
	if model.ID == "" || ai.ModelSupportsInput(model, ai.InputImage) {
		return ""
	}
	return "[Current model does not support images. The image will be omitted from this request.]"
}
