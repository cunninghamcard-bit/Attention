package extension

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/render"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

// Handler processes an extension event with a fresh runtime context.
type Handler func(context.Context, any, ExtensionContext) (any, error)

// Factory registers extension capabilities through the provided API.
type Factory func(api ExtensionAPI) error

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

// ToolCallRenderInput is the input to a tool's CALL (in-progress) renderer,
// shown before the result is available. Mirrors pi's renderCall(args, ...).
type ToolCallRenderInput struct {
	Args             map[string]any
	ToolCallID       string
	CWD              string
	ExecutionStarted bool
	ArgsComplete     bool
	Expanded         bool
	ShowImages       bool
	IsError          bool
	State            any
	LastBlocks       []render.Block
}

// ToolResultRenderInput is the input to a tool's RESULT renderer. Mirrors pi's
// renderResult(result, options, ...); Args is the originating call's arguments.
type ToolResultRenderInput struct {
	Args             map[string]any
	Result           RenderResult
	ToolCallID       string
	CWD              string
	ExecutionStarted bool
	ArgsComplete     bool
	Expanded         bool
	Partial          bool
	ShowImages       bool
	IsError          bool
	State            any
	LastBlocks       []render.Block
}

// RenderResult carries a finished (or partial) tool result for rendering.
// Details may be a map[string]any after a JSON round-trip through session storage.
type RenderResult struct {
	Content []ai.ContentBlock
	Details any
	IsError bool
}

// ToolCallRenderer / ToolResultRenderer turn render inputs into neutral render
// blocks. Split to mirror pi's separate renderCall/renderResult functions.
type ToolCallRenderer func(ToolCallRenderInput) []render.Block
type ToolResultRenderer func(ToolResultRenderInput) []render.Block

type ToolRenderShell string

const (
	ToolRenderShellDefault ToolRenderShell = "default"
	ToolRenderShellSelf    ToolRenderShell = "self"
)

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
	RenderShell      ToolRenderShell
	Execute          func(context.Context, ToolCall, ToolUpdateCallback, ExtensionContext) (ToolResult, error)
	// RenderCall / RenderResult convert tool calls/results into neutral render
	// blocks for frontends (HTML export, GUI). Optional; nil -> frontend uses a
	// generic fallback. Split to mirror pi's renderCall/renderResult.
	RenderCall   ToolCallRenderer
	RenderResult ToolResultRenderer
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

// ProviderOAuth marks an extension provider as OAuth-backed. The callbacks stay
// in the JS host and are invoked through the local jshost protocol.
type ProviderOAuth struct {
	Name string
}
