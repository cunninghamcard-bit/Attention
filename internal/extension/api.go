package extension

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

// Handler processes an extension event with a fresh runtime context.
type Handler func(context.Context, any, ExtensionContext) (any, error)

// Factory registers extension capabilities through the provided API.
type Factory func(api ExtensionAPI) error

// Source identifies an extension factory loaded during runtime assembly.
type Source struct {
	Path    string
	Factory Factory
}

// ExtensionAPI is the facade passed to an extension factory function.
type ExtensionAPI struct {
	On                 func(eventType string, handler Handler)
	RegisterTool       func(def ToolDefinition)
	RegisterCommand    func(name string, def CommandDefinition)
	RegisterProvider   func(name string, def ProviderDefinition)
	UnregisterProvider func(name string)
}

// Tool primitives are aliases of the agentloop runtime types so the whole
// codebase has a single tool result/callback/mode shape (pi keeps one
// AgentToolResult shared by harness and tool definitions).
type ToolExecutionMode = tool.ToolExecutionMode

const (
	ToolExecutionDefault    ToolExecutionMode = ""
	ToolExecutionSequential                   = tool.Sequential
	ToolExecutionParallel                     = tool.Parallel
)

// ToolCall is the call payload passed to an extension tool.
type ToolCall struct {
	ID   string
	Args map[string]any
}

// ToolResult is the shared tool result descriptor (alias of tool.Result).
type ToolResult = tool.Result

// ToolUpdateCallback publishes incremental tool updates.
type ToolUpdateCallback = tool.UpdateCallback

// ToolDefinition is the single tool definition shape used by both builtin tools
// and extension-registered tools (pi: core/extensions/types.ts ToolDefinition).
// The orchestrator wraps it into an tool.Tool for the harness.
type ToolDefinition struct {
	Name        string
	Label       string
	Description string
	// PromptSnippet is the one-line entry for the system prompt "Available tools"
	// section. PromptGuidelines are appended to the Guidelines section when the
	// tool is active. Mirrors pi's promptSnippet/promptGuidelines.
	PromptSnippet    string
	PromptGuidelines []string
	Parameters       map[string]any
	PrepareArgs      func(map[string]any) map[string]any
	ExecutionMode    ToolExecutionMode
	Execute          func(context.Context, ToolCall, ToolUpdateCallback, ExtensionContext) (ToolResult, error)
}

// CommandDefinition describes a slash-command registered by an extension.
type CommandDefinition struct {
	Description string
	Source      resource.SourceInfo
	Handler     func(context.Context, []string, ExtensionContext) error
}

// ProviderDefinition describes a provider registered by an extension. It is an
// extension-local descriptor; orchestrator adapts it to internal/provider.
type ProviderDefinition struct {
	Name           *string
	BaseURL        *string
	APIKey         *string
	API            *string
	Headers        map[string]string
	AuthHeader     *bool
	Compat         *ai.Compat
	Models         []ProviderModel
	ModelOverrides map[string]ProviderModelOverride
	OAuth          *ProviderOAuth
}

// ProviderModel describes a model supplied by an extension provider.
type ProviderModel struct {
	ID               string
	Name             *string
	API              *string
	BaseURL          *string
	Reasoning        *bool
	ThinkingLevelMap map[string]*string
	Input            []ai.InputCapability
	Cost             *ProviderModelCost
	ContextWindow    *int
	MaxTokens        *int
	Headers          map[string]string
	Compat           *ai.Compat
}

// ProviderModelOverride describes a partial override for an existing model.
type ProviderModelOverride struct {
	Name             *string
	Reasoning        *bool
	ThinkingLevelMap map[string]*string
	Input            []ai.InputCapability
	Cost             *ProviderModelCost
	ContextWindow    *int
	MaxTokens        *int
	Headers          map[string]string
	Compat           *ai.Compat
}

// ProviderModelCost mirrors provider.ModelCost without importing provider.
type ProviderModelCost struct {
	Input      *float64
	Output     *float64
	CacheRead  *float64
	CacheWrite *float64
}

// ProviderOAuth marks an extension provider as OAuth-backed. The extension
// supplies the OAuth callbacks through the Go extension API.
type ProviderOAuth struct {
	Name string
}
