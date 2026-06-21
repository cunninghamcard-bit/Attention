package builtin

import (
	"context"
	"fmt"
	"maps"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

// Wrap adapts an extension.ToolDefinition into a runtime tool.Tool, binding the
// ExtensionContext through ctxFactory. Both builtin tools and extension-
// registered tools go through Wrap, mirroring pi's wrapToolDefinition
// (.agents/references/pi/packages/coding-agent/src/core/tools/tool-definition-wrapper.ts).
func Wrap(def extension.ToolDefinition, ctxFactory extension.ContextFactory) (tool.Tool, error) {
	if def.Name == "" {
		return tool.Tool{}, fmt.Errorf("tool: definition missing name")
	}
	if def.Execute == nil {
		return tool.Tool{}, fmt.Errorf("tool: definition %q has no execute handler", def.Name)
	}
	return tool.Tool{
		Tool: ai.Tool{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  maps.Clone(def.Parameters),
		},
		Label:         def.Label,
		PrepareArgs:   def.PrepareArgs,
		ExecutionMode: def.ExecutionMode,
		Execute: func(ctx context.Context, toolCallID string, args map[string]any, onUpdate tool.UpdateCallback) (tool.Result, error) {
			update := func(result tool.Result) {
				if onUpdate != nil {
					onUpdate(result)
				}
			}
			return def.Execute(ctx, extension.ToolCall{
				ID:   toolCallID,
				Args: maps.Clone(args),
			}, update, ctxFactory(ctx))
		},
	}, nil
}
