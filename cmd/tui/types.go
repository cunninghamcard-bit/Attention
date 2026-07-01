// Adapted from github.com/dimetron/pi-go internal/tui
package main

import (
	"context"
	"iter"

	"google.golang.org/adk/session"
)

// AgentBackend is the seam between the TUI render path and the agent
// implementation. pi-go used a concrete *agent.Agent; this interface captures
// exactly the methods the root model calls on it so that Phase 2 can swap in an
// RPC-backed adapter (and Phase 1 can use a stub).
type AgentBackend interface {
	// RunStreaming sends a user message with SSE streaming enabled and returns
	// an iterator over agent events.
	RunStreaming(ctx context.Context, sessionID string, userMessage string) iter.Seq2[*session.Event, error]
	// CreateSession creates a new session and returns its ID.
	CreateSession(ctx context.Context) (string, error)
	FetchCommands() []CommandInfo
	Reload() (string, error)
	DispatchCommand(name, args string) (commandDispatchResult, error)
}

// Skill is a minimal local stand-in for pi-go's extension.Skill. The slash
// command / skill-loading subsystem is cut from this viewer, but the input and
// completion render machinery still type their skill list against this shape.
type Skill struct {
	// Name is the skill's identifier.
	Name string
	// Description is a one-line description from frontmatter.
	Description string
	// Source is where the skill came from: "bundled", "user", or "project".
	Source string
}

// Config holds configuration for the TUI.
//
// This is the minimal viewer Config. The slash-command and in-process service
// fields from pi-go (SessionService, Orchestrator, LLM, GenerateCommitMsg,
// Logger, Skills/SkillDirs, ModelSwitcher, MCPToolsets/MCPServers,
// CompactMetrics, AgentEventCh, DeferredInit, Roles/ActiveRole) are removed.
type Config struct {
	Agent        AgentBackend
	SessionID    string
	AppVersion   string
	ModelName    string
	ProviderName string
	ThemeName    string
	// TokenTracker tracks daily token usage for the status bar. May be nil.
	TokenTracker TokenTracker
	// Commands is the kernel's full command list (get_commands), used VERBATIM
	// as the SINGLE source of truth for slash-command completion and dispatch.
	// There is no hardcoded command list; this is it. May be empty/nil.
	Commands []CommandInfo
	// Skills are the agent skills exposed by the kernel, populated from the
	// kernel via the get_commands rpc (entries with source=="skill"). They feed
	// the sidebar's Skills section (bare-name display only). May be empty/nil.
	Skills []Skill
}

// CompactStatsProvider provides compaction statistics for TUI display.
type CompactStatsProvider interface {
	FormatStats() string
}

// TokenTracker provides read access to daily token usage for the status bar.
type TokenTracker interface {
	Limit() int64
	Remaining() int64     // -1 if unlimited
	PercentUsed() float64 // 0-100+
	TotalUsed() int64     // total tokens consumed today

	// Session context window tracking.
	LastPromptTokens() int64     // most recent prompt tokens from LLM response
	ContextWindowSize() int64    // model's context window size (0 = unknown)
	ContextPercentUsed() float64 // context window usage 0-100+
}

// AgentSubEvent carries a subagent event from the agent tool to the TUI.
type AgentSubEvent struct {
	AgentID    string
	Kind       string // "tool_call", "tool_result", "text_delta", etc.
	Content    string
	PipelineID string // groups agents in same call
	Mode       string // "single", "parallel", "chain"
	Step       int    // 1-based position in pipeline
	Total      int    // total agents in pipeline
}
