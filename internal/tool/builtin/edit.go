package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

type editToolDetails struct {
	Diff             string `json:"diff"`
	Patch            string `json:"patch"`
	FirstChangedLine *int   `json:"firstChangedLine,omitempty"`
}

// NewEditTool creates the built-in edit tool.
func NewEditTool(env execenv.ExecutionEnv) extension.ToolDefinition {
	return extension.ToolDefinition{
		Name: "edit",
		Description: "Edit a single file by replacing one or more targeted edits[].oldText regions. " +
			"Each oldText is matched exactly first, then with fuzzy matching for minor whitespace and " +
			"Unicode punctuation differences. Every oldText must match a unique, non-overlapping region of " +
			"the original file.",
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the file to edit (relative or absolute)"},
				"edits": map[string]any{
					"type":        "array",
					"description": "One or more targeted replacements. Each edit is matched against the original file, not incrementally.",
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []string{"oldText", "newText"},
						"properties": map[string]any{
							"oldText": map[string]any{"type": "string", "description": "Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."},
							"newText": map[string]any{"type": "string", "description": "Replacement text for this targeted edit."},
						},
					},
				},
			},
		},
		Label:         "edit",
		PromptSnippet: "Replace exact text regions in a file (one or more edits)",
		PrepareArgs:   prepareEditArgs,
		ExecutionMode: tool.Sequential,
		RenderShell:   extension.ToolRenderShellSelf,
		RenderCall:    editRenderCall,
		RenderResult:  editRenderResult,
		Execute: func(ctx context.Context, call extension.ToolCall, _ tool.UpdateCallback, _ extension.ExtensionContext) (tool.Result, error) {
			return executeEdit(ctx, env, call.Args), nil
		},
	}
}

func editRenderCall(input extension.ToolCallRenderInput) []render.Block {
	path := argString(input.Args, "file_path", "path")
	if path == "" {
		return nil
	}
	return []render.Block{render.Text("edit " + path)}
}

func editRenderResult(input extension.ToolResultRenderInput) []render.Block {
	var d editToolDetails
	if decodeDetails(input.Result.Details, &d) && d.Diff != "" {
		return []render.Block{render.Diff(d.Diff)}
	}

	out := toolOutputText(input.Result.Content)
	if out == "" {
		return nil
	}
	return []render.Block{render.Text(out)}
}

// prepareEditArgs normalizes tool input before execution: it parses an edits
// array delivered as a JSON string, and folds a legacy top-level oldText/newText
// pair into edits[]. Mirrors pi's prepareEditArguments
// (.agents/references/pi/packages/coding-agent/src/core/tools/edit.ts:93-117).
func prepareEditArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	prepared := make(map[string]any, len(args))
	maps.Copy(prepared, args)

	if text, ok := prepared["edits"].(string); ok {
		var parsed []any
		if err := json.Unmarshal([]byte(text), &parsed); err == nil {
			prepared["edits"] = parsed
		}
	}

	oldText, hasOldText := prepared["oldText"].(string)
	newText, hasNewText := prepared["newText"].(string)
	if !hasOldText || !hasNewText {
		return prepared
	}

	edits := []any{}
	switch value := prepared["edits"].(type) {
	case []any:
		edits = append(edits, value...)
	case []map[string]any:
		for _, edit := range value {
			edits = append(edits, edit)
		}
	case nil:
	default:
		edits = append(edits, value)
	}
	edits = append(edits, map[string]any{"oldText": oldText, "newText": newText})
	prepared["edits"] = edits
	delete(prepared, "oldText")
	delete(prepared, "newText")
	return prepared
}

// executeEdit applies every edit through the single editdiff engine: read,
// strip BOM, normalize to LF, apply (exact then fuzzy), restore line endings,
// write, and report the change. The rendered diff travels in details for a UI to
// display; the model-facing result is a one-line summary, matching pi.
func executeEdit(ctx context.Context, env execenv.ExecutionEnv, args map[string]any) tool.Result {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("missing required argument %q", "path")
	}

	edits, argErr := parseEdits(args["edits"])
	if argErr != nil {
		return argErr.result()
	}

	absolutePath, err := env.AbsolutePath(ctx, path)
	if err != nil {
		return errorResult("Could not resolve file path: %s. %v", path, err)
	}

	result, err := withFileMutationQueue(absolutePath, func() tool.Result {
		rawContent, err := env.ReadTextFile(ctx, path)
		if err != nil {
			return errorResult("Could not edit file: %s. %v", path, err)
		}

		bom, content := StripBOM(rawContent)
		lineEnding := DetectLineEnding(content)
		normalized := NormalizeToLF(content)
		applied, applyErr := ApplyEdits(normalized, edits, path)
		if applyErr != nil {
			return errorResult("%v", applyErr)
		}

		finalContent := bom + RestoreLineEndings(applied.NewContent, lineEnding)
		if err := env.WriteFile(ctx, path, []byte(finalContent)); err != nil {
			return errorResult("Could not edit file: %s. %v", path, err)
		}

		diff, firstChangedLine := DiffString(applied.BaseContent, applied.NewContent, 4)
		details := editToolDetails{
			Diff:             diff,
			Patch:            UnifiedPatch(path, applied.BaseContent, applied.NewContent, 4),
			FirstChangedLine: firstChangedLine,
		}
		return textResult(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), path), details)
	})
	if err != nil {
		return errorResult("Could not resolve file path: %s. %v", path, err)
	}
	return result
}

func parseEdits(value any) ([]Edit, *agentArgError) {
	if value == nil {
		return nil, &agentArgError{message: "Edit tool input is invalid. edits must contain at least one replacement."}
	}

	rawEdits := []any{}
	switch edits := value.(type) {
	case []any:
		rawEdits = edits
	case []map[string]any:
		for _, edit := range edits {
			rawEdits = append(rawEdits, edit)
		}
	case []map[string]string:
		for _, edit := range edits {
			rawEdits = append(rawEdits, edit)
		}
	case []Edit:
		if len(edits) == 0 {
			return nil, &agentArgError{message: "Edit tool input is invalid. edits must contain at least one replacement."}
		}
		return edits, nil
	default:
		return nil, &agentArgError{message: "Invalid argument \"edits\": expected array."}
	}
	if len(rawEdits) == 0 {
		return nil, &agentArgError{message: "Edit tool input is invalid. edits must contain at least one replacement."}
	}

	edits := make([]Edit, 0, len(rawEdits))
	for i, raw := range rawEdits {
		oldText, newText, ok := parseEdit(raw)
		if !ok {
			return nil, &agentArgError{
				message: fmt.Sprintf("Invalid argument \"edits[%d]\": expected oldText and newText strings.", i),
			}
		}
		edits = append(edits, Edit{OldText: oldText, NewText: newText})
	}
	return edits, nil
}

func parseEdit(raw any) (string, string, bool) {
	switch edit := raw.(type) {
	case map[string]any:
		oldText, ok := edit["oldText"].(string)
		if !ok {
			return "", "", false
		}
		newText, ok := edit["newText"].(string)
		return oldText, newText, ok
	case map[string]string:
		oldText, ok := edit["oldText"]
		if !ok {
			return "", "", false
		}
		newText, ok := edit["newText"]
		return oldText, newText, ok
	case Edit:
		return edit.OldText, edit.NewText, true
	default:
		return "", "", false
	}
}
