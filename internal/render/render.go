// Package render defines along's neutral, serializable tool render description.
// Frontends (HTML export now, GUI later) interpret a small fixed vocabulary and
// need zero tool-specific knowledge. This is along's TUI-independent replacement
// for pi's renderCall/renderResult -> Component(ANSI) contract.
package render

// Block is one node in a tool's render description.
type Block struct {
	Kind     string  `json:"kind"`               // "group" | "text" | "badge" | "code" | "diff" | "image"
	Label    string  `json:"label,omitempty"`    // group header (e.g. file path)
	Text     string  `json:"text,omitempty"`     // text content
	Style    string  `json:"style,omitempty"`    // semantic style, e.g. "muted", "warning", "match"
	Children []Block `json:"children,omitempty"` // for Kind=="group"
	Language string  `json:"language,omitempty"` // for Kind=="code"
	MimeType string  `json:"mimeType,omitempty"` // for Kind=="image"
	Data     string  `json:"data,omitempty"`     // base64 image data for Kind=="image"
}

func Text(s string) Block { return Block{Kind: "text", Text: s} }

func StyledText(s, style string) Block {
	return Block{Kind: "text", Text: s, Style: style}
}

func Group(label string, children []Block) Block {
	return Block{Kind: "group", Label: label, Children: children}
}

func Badge(text, style string) Block {
	return Block{Kind: "badge", Text: text, Style: style}
}

func Code(text, language string) Block {
	return Block{Kind: "code", Text: text, Language: language}
}

func Diff(text string) Block {
	return Block{Kind: "diff", Text: text}
}

func Image(data, mimeType string) Block {
	return Block{Kind: "image", Data: data, MimeType: mimeType}
}

func ImageFallback(mimeType string) Block {
	if mimeType == "" {
		mimeType = "image/unknown"
	}
	return Block{Kind: "image-fallback", Text: "[" + mimeType + "]", Style: "muted"}
}
