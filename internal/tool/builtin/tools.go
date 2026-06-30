package builtin

import (
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
)

// NewCodingTools creates pi's default coding tool set.
func NewCodingTools(env execenv.ExecutionEnv, shellCommandPrefix string, extraBinDirs ...string) []extension.ToolDefinition {
	return []extension.ToolDefinition{
		NewReadTool(env),
		NewBashTool(env, shellCommandPrefix, extraBinDirs...),
		NewEditTool(env),
		NewWriteTool(env),
	}
}

// NewReadOnlyTools creates pi's read-only tool set.
func NewReadOnlyTools(env execenv.ExecutionEnv) []extension.ToolDefinition {
	return []extension.ToolDefinition{
		NewReadTool(env),
		NewGrepTool(env),
		NewFindTool(env),
		NewLsTool(env),
	}
}

// NewAllTools creates the full built-in tool set.
func NewAllTools(env execenv.ExecutionEnv, shellCommandPrefix string, extraBinDirs ...string) []extension.ToolDefinition {
	return []extension.ToolDefinition{
		NewReadTool(env),
		NewBashTool(env, shellCommandPrefix, extraBinDirs...),
		NewEditTool(env),
		NewWriteTool(env),
		NewGrepTool(env),
		NewFindTool(env),
		NewLsTool(env),
	}
}
