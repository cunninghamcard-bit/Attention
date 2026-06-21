// Package tool defines the runtime tool contracts (the shape the agent loop
// executes) and the tool registry. The rich, extension-facing tool definition
// lives in internal/extension; builtin tools and the extension adapter live in
// internal/tool/builtin. This mirrors pi, where the runtime AgentTool contract
// is the lowest layer and ToolDefinition / the tools sit above it.
package tool

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

// ToolExecutionMode controls how tool calls from a single assistant message run.
type ToolExecutionMode string

const (
	Sequential ToolExecutionMode = "sequential"
	Parallel   ToolExecutionMode = "parallel"
)

// Result is the result produced by a tool execution.
type Result struct {
	Content   []ai.ContentBlock
	Details   any
	IsError   bool
	Terminate bool
}

// UpdateCallback streams partial tool execution updates.
type UpdateCallback func(partial Result)

// Tool is a tool the agent loop can execute. The orchestrator builds these from
// extension.ToolDefinition values via builtin.Wrap.
type Tool struct {
	ai.Tool

	Label         string
	PrepareArgs   func(args map[string]any) map[string]any
	Execute       func(ctx context.Context, toolCallID string, args map[string]any, onUpdate UpdateCallback) (Result, error)
	ExecutionMode ToolExecutionMode
}
