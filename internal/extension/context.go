// Package extension provides the extension loading framework.
//
// Extensions are the primary producers of hook handlers and tools.
// They register via ExtensionAPI; Load stages the registrations and, only
// after the factory succeeds, commits handlers into hook.Registry and keeps
// tool/command definitions for later consumption.
package extension

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

// ReadonlySessionView provides read-only access to session state.
// The concrete type is defined by the caller; this package only needs
// a narrow read-only surface.
type ReadonlySessionView interface {
	GetMessages() []any
	GetEntries() []any
	GetMetadata() any
}

// ThinkingLevel is the extension-local thinking level descriptor.
type ThinkingLevel string

// UserInput is the extension-local user input passed to action facades.
type UserInput struct {
	Text string
}

// ContextUsage reports current token consumption against the context window.
type ContextUsage struct {
	Tokens        int
	ContextWindow int
	Percent       float64
}

// ModelInfo is the read-only model registry entry exposed to extensions.
// DisplayName maps along's ai.Model.Name into the extension-facing shape.
type ModelInfo struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	DisplayName   string `json:"displayName"`
	ContextWindow int    `json:"contextWindow"`
	Reasoning     bool   `json:"reasoning"`
}

// ExtensionContext carries the runtime context that extension handlers can
// access through the ContextFactory wrapper.
type ExtensionContext struct {
	Cwd                string
	SessionID          string
	PluginBinDirs      []string
	Session            ReadonlySessionView
	ModelRegistry      func() []ModelInfo
	Model              func() ai.Model
	IsIdle             func() bool
	IsAborted          func() bool
	HasPendingMessages func() bool
	GetContextUsage    func() *ContextUsage
	GetSystemPrompt    func() string
	Notify             func(message string, level string)

	Abort            func(context.Context) error
	Compact          func(context.Context) error
	SetModel         func(context.Context, ai.Model) error
	SetThinkingLevel func(context.Context, ThinkingLevel) error
	Steer            func(context.Context, UserInput) error
	FollowUp         func(context.Context, UserInput) error
	WaitForIdle      func(context.Context) error
	Shutdown         func()
	NewSession       func(context.Context, string) error
	Fork             func(context.Context, string) (string, error)
	SwitchSession    func(context.Context, string) error
	NavigateTree     func(context.Context, string) error
	Reload           func() error
}

// ExtensionContext mirrors pi's command-capable extension context where along
// has a direct runtime equivalent:
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:298-327
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:333-363.
// ModelRegistry mirrors pi ctx.modelRegistry at types.ts:307-308 as a read-only
// snapshot. IsAborted is the current pollable equivalent of pi ctx.signal at
// types.ts:313-314.
