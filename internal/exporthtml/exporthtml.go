package exporthtml

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

//go:embed template/template.html template/template.css template/template.js
//go:embed template/vendor/marked.min.js template/vendor/highlight.min.js
var templateFS embed.FS

// Options controls the transcript renderer.
type Options struct {
	Title string
}

// SessionHeader mirrors pi's JSONL session header:
// .agents/references/pi/packages/coding-agent/src/core/session-manager.ts:30-37.
type SessionHeader struct {
	Type          string `json:"type"`
	Version       int    `json:"version,omitempty"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

// ToolDefinition is the subset pi exports for the viewer's available-tools list:
// .agents/references/pi/packages/coding-agent/src/core/export-html/index.ts:135.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// RenderedTool carries tool call/result views pre-rendered into neutral blocks
// for the viewer to interpret. Along emits blocks (not pre-rendered HTML like
// pi) because it has no TUI/ANSI layer; template.js interprets the same
// vocabulary the GUI will.
type RenderedTool struct {
	CallBlocks           []render.Block `json:"callBlocks,omitempty"`
	CallBlocksExpanded   []render.Block `json:"callBlocksExpanded,omitempty"`
	ResultBlocks         []render.Block `json:"resultBlocks,omitempty"`
	ResultBlocksExpanded []render.Block `json:"resultBlocksExpanded,omitempty"`
}

// SessionData is the JSON shape consumed by pi's template.js:
// .agents/references/pi/packages/coding-agent/src/core/export-html/index.ts:130-138.
type SessionData struct {
	Header        SessionHeader           `json:"header"`
	Entries       []session.SessionEntry  `json:"entries"`
	LeafID        *session.EntryID        `json:"leafId"`
	SystemPrompt  string                  `json:"systemPrompt"`
	Tools         []ToolDefinition        `json:"tools"`
	RenderedTools map[string]RenderedTool `json:"renderedTools,omitempty"`
}

// Render returns pi's self-contained interactive session viewer. It mirrors
// pi generateHtml's base64 SESSION_DATA substitution:
// .agents/references/pi/packages/coding-agent/src/core/export-html/index.ts:143-174.
func Render(data SessionData, opts Options) string {
	_ = opts.Title

	data = normalizeSessionData(data)
	sessionData := encodeSessionData(data)
	css := renderCSS()

	templateHTML := mustReadTemplateAsset("template/template.html")
	templateJS := mustReadTemplateAsset("template/template.js")
	markedJS := mustReadTemplateAsset("template/vendor/marked.min.js")
	highlightJS := mustReadTemplateAsset("template/vendor/highlight.min.js")

	html := strings.Replace(templateHTML, "{{CSS}}", css, 1)
	html = strings.Replace(html, "{{JS}}", templateJS, 1)
	html = strings.Replace(html, "{{SESSION_DATA}}", sessionData, 1)
	html = strings.Replace(html, "{{MARKED_JS}}", markedJS, 1)
	html = strings.Replace(html, "{{HIGHLIGHT_JS}}", highlightJS, 1)
	return html
}

func normalizeSessionData(data SessionData) SessionData {
	if data.Header.Type == "" {
		data.Header.Type = "session"
	}
	if data.Entries == nil {
		data.Entries = []session.SessionEntry{}
	}
	if data.Tools == nil {
		data.Tools = []ToolDefinition{}
	}
	return data
}

func encodeSessionData(data SessionData) string {
	payload, err := json.Marshal(data)
	if err != nil {
		panic("exporthtml: encode session data: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func renderCSS() string {
	css := mustReadTemplateAsset("template/template.css")
	css = strings.Replace(css, "{{THEME_VARS}}", defaultThemeVars(), 1)
	css = strings.Replace(css, "{{BODY_BG}}", "#111318", 1)
	css = strings.Replace(css, "{{CONTAINER_BG}}", "#171a21", 1)
	css = strings.Replace(css, "{{INFO_BG}}", "#24201a", 1)
	return css
}

func mustReadTemplateAsset(path string) string {
	data, err := templateFS.ReadFile(path)
	if err != nil {
		panic("exporthtml: read " + path + ": " + err.Error())
	}
	return string(data)
}

type cssVar struct {
	Name  string
	Value string
}

var defaultTheme = []cssVar{
	{Name: "text", Value: "#e6edf3"},
	{Name: "muted", Value: "#8b949e"},
	{Name: "dim", Value: "#30363d"},
	{Name: "border", Value: "#30363d"},
	{Name: "borderAccent", Value: "#58a6ff"},
	{Name: "accent", Value: "#58a6ff"},
	{Name: "hover", Value: "#1f242c"},
	{Name: "selectedBg", Value: "#263241"},
	{Name: "success", Value: "#3fb950"},
	{Name: "warning", Value: "#d29922"},
	{Name: "error", Value: "#f85149"},
	{Name: "userMessageBg", Value: "#203040"},
	{Name: "userMessageText", Value: "#e6edf3"},
	{Name: "thinkingText", Value: "#a5b4fc"},
	{Name: "toolPendingBg", Value: "#22272e"},
	{Name: "toolSuccessBg", Value: "#1f3326"},
	{Name: "toolErrorBg", Value: "#3a1f22"},
	{Name: "toolOutput", Value: "#c9d1d9"},
	{Name: "toolDiffAdded", Value: "#7ee787"},
	{Name: "toolDiffRemoved", Value: "#ff7b72"},
	{Name: "toolDiffContext", Value: "#8b949e"},
	{Name: "customMessageBg", Value: "#202733"},
	{Name: "customMessageLabel", Value: "#79c0ff"},
	{Name: "customMessageText", Value: "#dbeafe"},
	{Name: "mdHeading", Value: "#f0f6fc"},
	{Name: "mdLink", Value: "#79c0ff"},
	{Name: "mdCode", Value: "#ffa657"},
	{Name: "mdCodeBlockBorder", Value: "#30363d"},
	{Name: "mdQuote", Value: "#8b949e"},
	{Name: "mdQuoteBorder", Value: "#58a6ff"},
	{Name: "mdListBullet", Value: "#58a6ff"},
	{Name: "mdHr", Value: "#30363d"},
	{Name: "syntaxComment", Value: "#8b949e"},
	{Name: "syntaxKeyword", Value: "#ff7b72"},
	{Name: "syntaxNumber", Value: "#79c0ff"},
	{Name: "syntaxString", Value: "#a5d6ff"},
	{Name: "syntaxFunction", Value: "#d2a8ff"},
	{Name: "syntaxType", Value: "#ffa657"},
	{Name: "syntaxVariable", Value: "#ffa657"},
	{Name: "syntaxOperator", Value: "#ff7b72"},
	{Name: "syntaxPunctuation", Value: "#c9d1d9"},
}

func defaultThemeVars() string {
	lines := make([]string, 0, len(defaultTheme))
	for _, cssVar := range defaultTheme {
		lines = append(lines, "--"+cssVar.Name+": "+cssVar.Value+";")
	}
	return strings.Join(lines, "\n      ")
}
