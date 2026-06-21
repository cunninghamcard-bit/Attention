// Package compat defines the pi-facing service vocabulary for mode packages.
// 原为 orchestrator/extension 别名缝（c35a0f9）；orchestrator/harness 处决后
// 这里持有实定义——pi wire 协议的语义词汇从此自给自足（along-api.md §8）。
// extension/hook 词汇仍为别名：它们指向存活器官 src/core/{extension,hook}。
package compat

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type (
	ContextUsage = extension.ContextUsage
	UIContext    = extension.UIContext
	BashResult   = hook.BashResult
)

// pi AgentEvent 的事件类型词汇（types.ts:405-418 同名）。
const (
	EventMessageStart         = "message_start"
	EventMessageUpdate        = "message_update"
	EventMessageEnd           = "message_end"
	EventTurnStart            = "turn_start"
	EventTurnEnd              = "turn_end"
	EventAgentStart           = "agent_start"
	EventAgentEnd             = "agent_end"
	EventToolExecutionStart   = "tool_execution_start"
	EventToolExecutionUpdate  = "tool_execution_update"
	EventToolExecutionEnd     = "tool_execution_end"
	EventAutoRetryStart       = "auto_retry_start"
	EventAutoRetryEnd         = "auto_retry_end"
	EventThinkingLevelChanged = "thinking_level_changed"
	EventCompactionStart      = "compaction_start"
	EventCompactionEnd        = "compaction_end"
	EventQueueUpdate          = "queue_update"
	EventSavePoint            = "save_point"
	EventResourcesUpdate      = "resources_update"
	EventSessionInfoChanged   = "session_info_changed"
	EventSettled              = "settled"
)

// ResourceSummary is the pi resources_update projection for one loaded skill
// or prompt template.
type ResourceSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ResourcesSnapshot mirrors pi AgentHarnessResources for resources_update.
type ResourcesSnapshot struct {
	Skills          []ResourceSummary `json:"skills"`
	PromptTemplates []ResourceSummary `json:"promptTemplates"`
}

// Event is the narrow lifecycle event shape exposed to modes.
// Non-streaming payload names mirror pi AgentEvent exactly (types.ts:405-418).
type Event struct {
	Type    string
	Message *ai.Message
	Delta   *ai.StreamEvent

	Messages            []ai.Message
	ToolResults         []ai.Message
	ToolCallID          string
	ToolName            string
	Args                any
	PartialResult       any
	Result              any
	IsError             bool
	Attempt             int
	MaxAttempts         int
	DelayMs             int
	ErrorMessage        string
	Success             bool
	FinalError          string
	Level               string
	Reason              string
	Aborted             bool
	WillRetry           bool
	Steering            []string
	FollowUp            []string
	Name                string
	HadPendingMutations bool
	NextTurnCount       int
	Resources           ResourcesSnapshot
	PreviousResources   ResourcesSnapshot
}

// PromptInput is normalized into one user message per run.
type PromptInput struct {
	Text    string
	Content []ai.ContentBlock
	Message message.AgentMessage
	Source  string
	// StreamingBehavior mirrors pi rpc prompt's busy-routing option
	// (rpc-types.ts:21).
	StreamingBehavior string
	// PreflightResult, if set, is called once with true as soon as the prompt
	// is accepted, before the run completes (pi rpc-mode.ts:398 ack 语义).
	PreflightResult func(bool)
}

// UserInput is queued as transient steering or follow-up user input.
type UserInput struct {
	Text    string
	Content []ai.ContentBlock
	Message message.AgentMessage
}

// PromptResult is the outcome of a prompt. Handled means an input hook
// consumed the prompt before a user turn or assistant message was produced.
type PromptResult struct {
	Message ai.Message
	Handled bool
}

// AbortResult reports whether an active run was canceled and what transient
// queue state was discarded.
type AbortResult struct {
	Aborted         bool
	ClearedSteer    []message.AgentMessage
	ClearedFollowUp []message.AgentMessage
	ClearedNextTurn []message.AgentMessage
}

// SlashCommand is a prompt-invokable command exposed to rpc get_commands.
type SlashCommand struct {
	Name        string
	Description string
	Source      string
	SourceInfo  resource.SourceInfo
}

// CompactOptions controls compaction.
type CompactOptions struct {
	CustomInstructions string
}

// CompactResult 原为 harness.CompactionResult（harness 已处决，定义归词汇包）。
type CompactResult struct {
	Summary          string
	FirstKeptEntryID session.EntryID
	TokensBefore     int
	Details          any
	FromHook         bool
}

// QueueMode controls how many queued messages are injected at a drain point
// (pi types.ts:38-44).
type QueueMode string

const (
	QueueModeAll        QueueMode = "all"
	QueueModeOneAtATime QueueMode = "one-at-a-time"
)

// Snapshot reports current model/thinking/streaming/session state.
type Snapshot struct {
	Model                 ai.Model
	ThinkingLevel         agentloop.ThinkingLevel
	IsStreaming           bool
	IsCompacting          bool
	SteeringMode          string
	FollowUpMode          string
	SessionFile           string
	SessionID             string
	SessionName           string
	AutoCompactionEnabled bool
	MessageCount          int
	PendingMessageCount   int
}

// SessionStats is the mode-facing view backing rpc get_session_stats
// (pi agent-session.ts:216-234, 2877-2919).
type SessionStats struct {
	SessionFile       string
	SessionID         string
	UserMessages      int
	AssistantMessages int
	ToolCalls         int
	ToolResults       int
	TotalMessages     int
	Tokens            SessionStatsTokens
	Cost              float64
	ContextUsage      *extension.ContextUsage
}

type SessionStatsTokens struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	Total      int
}

type ModelCycleResult struct {
	Model         ai.Model
	ThinkingLevel agentloop.ThinkingLevel
}

type ForkMessage struct {
	EntryID string
	Text    string
}

// Target 是 pi RPC 兼容头的服务合同：28 命令的语义面。enginefacade 是其
// 引擎实现（orchestrator 处决后的唯一实现）。
type Target interface {
	Subscribe(func(Event)) func()
	Prompt(context.Context, PromptInput) (PromptResult, error)
	Steer(context.Context, UserInput) error
	FollowUp(context.Context, UserInput) error
	Abort(context.Context) (AbortResult, error)
	SetModel(context.Context, ai.Model) error
	CycleModel(context.Context) (ModelCycleResult, bool, error)
	SetThinkingLevel(context.Context, agentloop.ThinkingLevel) error
	CycleThinkingLevel(context.Context) (agentloop.ThinkingLevel, bool, error)
	Compact(context.Context, CompactOptions) (CompactResult, error)
	SetSteeringMode(QueueMode)
	SetFollowUpMode(QueueMode)
	SetAutoCompaction(bool)
	SetAutoRetry(bool)
	AbortRetry()
	ExecuteBash(context.Context, string) (BashResult, error)
	AbortBash()
	ExportHTML(context.Context, string) (string, error)
	NewSession(context.Context, string) (bool, error)
	SwitchSession(context.Context, string) (bool, error)
	Fork(context.Context, string) (string, bool, error)
	Clone(context.Context) (bool, error)
	ForkMessages() []ForkMessage
	NotifySessionShutdown(context.Context, string) error
	SetSessionName(context.Context, string) error
	LastAssistantText() (string, bool)
	SlashCommands() []SlashCommand
	WaitForIdle(context.Context) error
	Snapshot() Snapshot
	SessionStats() SessionStats
	Messages() []ai.Message
	ResolveModel(ctx context.Context, provider, modelID string) (ai.Model, bool)
	AvailableModels(context.Context) []ai.Model
	SetUIContext(UIContext)
}
