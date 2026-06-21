package builtin

import (
	"fmt"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

func textResult(text string, details any) tool.Result {
	return tool.Result{
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: text}},
		Details: details,
	}
}

func errorResult(format string, args ...any) tool.Result {
	message := fmt.Sprintf(format, args...)
	return tool.Result{
		Content: []ai.ContentBlock{{Type: ai.ContentText, Text: message}},
		Details: map[string]any{
			"isError": true,
			"error":   message,
		},
		IsError: true,
	}
}

func isErrorResult(result tool.Result) bool {
	details, ok := result.Details.(map[string]any)
	if !ok {
		return false
	}
	isError, ok := details["isError"].(bool)
	return ok && isError
}
