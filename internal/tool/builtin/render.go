package builtin

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/render"
)

// toolOutputText joins the text content blocks of a tool result.
func toolOutputText(content []ai.ContentBlock) string {
	parts := []string{}
	for _, b := range content {
		if b.Type == ai.ContentText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toolOutputImages renders image content blocks as image render blocks,
// each followed by a text fallback for frontends that cannot display images.
func toolOutputImages(content []ai.ContentBlock) []render.Block {
	blocks := []render.Block{}
	for _, b := range content {
		if b.Type == ai.ContentImage && b.ImageData != "" {
			blocks = append(blocks, render.Image(b.ImageData, b.MimeType))
			blocks = append(blocks, render.ImageFallback(b.MimeType))
		}
	}
	return blocks
}

// outputCodeBlocks renders output text as a code block, collapsing to maxLines
// when not expanded (with a muted "N more lines" badge). Empty text -> nil.
func outputCodeBlocks(text, language string, maxLines int, expanded bool) []render.Block {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if expanded || maxLines <= 0 || strings.Count(text, "\n") < maxLines {
		return []render.Block{render.Code(text, language)}
	}
	lines := strings.Split(text, "\n")
	preview := strings.Join(lines[:maxLines], "\n")
	return []render.Block{
		render.Code(preview, language),
		render.Badge(fmt.Sprintf("… %d more lines", len(lines)-maxLines), "muted"),
	}
}

// argString returns the first present string arg among keys.
func argString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// languageFromPath mirrors template.js getLanguageFromPath.
func languageFromPath(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return ""
	}
	ext := strings.ToLower(path[idx+1:])
	switch ext {
	case "ts", "tsx":
		return "typescript"
	case "js", "jsx":
		return "javascript"
	case "py":
		return "python"
	case "rb":
		return "ruby"
	case "rs":
		return "rust"
	case "go":
		return "go"
	case "java":
		return "java"
	case "c", "h":
		return "c"
	case "cpp", "hpp":
		return "cpp"
	case "cs":
		return "csharp"
	case "php":
		return "php"
	case "sh", "bash", "zsh":
		return "bash"
	case "sql":
		return "sql"
	case "html":
		return "html"
	case "css":
		return "css"
	case "scss":
		return "scss"
	case "json":
		return "json"
	case "yaml", "yml":
		return "yaml"
	case "xml":
		return "xml"
	case "md":
		return "markdown"
	case "dockerfile":
		return "dockerfile"
	}
	return ""
}

// decodeDetails re-decodes a details value (possibly a map after JSON round-trip)
// into the given typed pointer; returns false on failure.
func decodeDetails(details any, out any) bool {
	if details == nil {
		return false
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}
