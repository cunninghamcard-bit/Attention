package hook

import (
	"context"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

// Event type constants.
const (
	// Decision events (results affect behavior).
	EventBeforeAgentStart      = "before_agent_start"
	EventContext               = "context"
	EventBeforeProviderRequest = "before_provider_request"
	EventBeforeProviderPayload = "before_provider_payload"
	EventToolCall              = "tool_call"
	EventInput                 = "input"
	EventToolResult            = "tool_result"
	EventSessionBeforeSwitch   = "session_before_switch"
	EventSessionBeforeFork     = "session_before_fork"
	EventSessionBeforeCompact  = "session_before_compact"
	EventSessionBeforeTree     = "session_before_tree"
	EventUserBash              = "user_bash"
	EventResourcesDiscover     = "resources_discover"
	// EventMessageEnd is a decision hook emitted by the harness through
	// Registry.Handlers so replacements are chained locally.
	EventMessageEnd = "message_end"

	// Notification events (no return value expected).
	EventAgentStart            = "agent_start"
	EventAgentEnd              = "agent_end"
	EventTurnStart             = "turn_start"
	EventTurnEnd               = "turn_end"
	EventMessageStart          = "message_start"
	EventMessageUpdate         = "message_update"
	EventToolExecutionStart    = "tool_execution_start"
	EventToolExecutionUpdate   = "tool_execution_update"
	EventToolExecutionEnd      = "tool_execution_end"
	EventAfterProviderResponse = "after_provider_response"
	EventSessionStart          = "session_start"
	EventSessionCompact        = "session_compact"
	EventSessionTree           = "session_tree"
	EventSessionShutdown       = "session_shutdown"
	EventModelSelect           = "model_select"
	EventThinkingLevelSelect   = "thinking_level_select"
	EventResourcesUpdate       = "resources_update"
	EventQueueUpdate           = "queue_update"
	EventSavePoint             = "save_point"
	EventAbort                 = "abort"
	EventSettled               = "settled"
)

// Decision events — results influence control flow.

// BeforeAgentStartEvent is emitted before the agent loop starts.
type BeforeAgentStartEvent struct {
	Type                string
	Prompt              string
	Images              []ImageContent
	SystemPrompt        string
	SystemPromptOptions *SystemPromptOptions
	Resources           any
}

// SystemPromptOptions mirrors pi's BuildSystemPromptOptions without importing
// internal/resource:
// .agents/references/pi/packages/coding-agent/src/core/system-prompt.ts:8-26.
type SystemPromptOptions struct {
	CustomPrompt       string
	SelectedTools      []string
	ToolSnippets       map[string]string
	PromptGuidelines   []string
	AppendSystemPrompt string
	CWD                string
	ContextFiles       []ContextFileInfo
	Skills             []SkillInfo
}

type ContextFileInfo struct {
	Path    string
	Content string
}

type SkillInfo struct {
	Name        string
	Description string
}

// ImageContent is the hook package's minimal representation of prompt images.
type ImageContent struct {
	MimeType string
	Data     string
}

// BeforeAgentStartResult allows hook to inject messages or replace the system prompt.
type BeforeAgentStartResult struct {
	Messages     []any
	SystemPrompt *string
}

// ContextEvent is emitted to transform the conversation context.
type ContextEvent struct {
	Type     string
	Messages []any
}

// ContextResult allows hook to replace the context messages.
type ContextResult struct {
	Messages []any
}

// BeforeProviderRequestEvent is emitted before a provider API request.
type BeforeProviderRequestEvent struct {
	Type          string
	Model         any
	SessionID     string
	StreamOptions any
}

// BeforeProviderRequestResult allows hook to patch stream options.
type BeforeProviderRequestResult struct {
	StreamOptions any
}

// StreamOptionsPatch is a Go representation of pi's provider stream option patch.
type StreamOptionsPatch struct {
	Temperature     *float64
	MaxTokens       *int
	APIKey          *string
	Transport       *string
	CacheRetention  *string
	SessionID       *string
	ClearHeaders    bool
	Headers         map[string]*string
	Timeout         *time.Duration
	MaxRetries      *int
	ClearMetadata   bool
	Metadata        map[string]any
	Reasoning       *string
	ThinkingBudgets *ai.ThinkingBudgets
}

// BeforeProviderPayloadEvent is emitted before the provider payload is sent.
type BeforeProviderPayloadEvent struct {
	Type    string
	Model   any
	Payload any
}

// BeforeProviderPayloadResult allows hook to patch the payload.
type BeforeProviderPayloadResult struct {
	Payload any
}

// ToolCallEvent is emitted before a tool is executed.
type ToolCallEvent struct {
	Type       string
	ToolCallId string
	ToolName   string
	Input      any
}

// ToolCallResult allows hook to block tool execution or patch its input.
type ToolCallResult struct {
	Block  bool
	Reason string
	Input  map[string]any
}

// InputEvent is emitted before a prompt input is normalized into a user
// message.
type InputEvent struct {
	Type   string
	Text   string
	Images []ImageContent
	Source string
}

// InputResult allows an input handler to handle the prompt without an agent
// turn or transform the prompt text/images before the user message is built.
type InputResult struct {
	Action string
	Text   string
	Images []ImageContent
}

// ToolResultEvent is emitted after a tool has been executed.
type ToolResultEvent struct {
	Type       string
	ToolCallId string
	ToolName   string
	Input      any
	Content    []ai.ContentBlock
	Details    any
	IsError    bool
}

// ToolResultPatch allows hook to modify tool result or terminate the session.
type ToolResultPatch struct {
	Content   []ai.ContentBlock
	Details   any
	IsError   *bool
	Terminate *bool
}

// CompactionSettings records the settings used to prepare a compaction.
type CompactionSettings struct {
	Enabled          bool
	ReserveTokens    int
	KeepRecentTokens int
}

// CompactionPreparation is the prepared compaction input exposed to hooks.
type CompactionPreparation struct {
	FirstKeptEntryID    string
	MessagesToSummarize []any
	TurnPrefixMessages  []any
	IsSplitTurn         bool
	TokensBefore        int
	PreviousSummary     string
	FileOps             any
	Settings            CompactionSettings
}

// CompactionResult is a hook-provided compaction result.
type CompactionResult struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     int
	Details          any
}

// SessionBeforeCompactEvent is emitted before session compaction.
type SessionBeforeCompactEvent struct {
	Type               string
	Preparation        CompactionPreparation
	BranchEntries      []any
	CustomInstructions string
	Signal             any
}

// SessionBeforeCompactResult allows hook to cancel or provide a compaction.
type SessionBeforeCompactResult struct {
	Cancel     bool
	Compaction *CompactionResult
}

func (r SessionBeforeCompactResult) Cancelled() bool { return r.Cancel }

// TreePreparation is the prepared tree-navigation input exposed to hooks.
type TreePreparation struct {
	TargetID            string
	OldLeafID           *string
	CommonAncestorID    *string
	EntriesToSummarize  []any
	UserWantsSummary    bool
	CustomInstructions  *string
	ReplaceInstructions *bool
	Label               *string
}

// SessionBeforeSwitchEvent is emitted before replacing the current session
// with a new or resumed session.
type SessionBeforeSwitchEvent struct {
	Type              string
	Reason            string
	TargetSessionFile *string
}

// SessionBeforeSwitchResult allows hook to cancel a session switch.
type SessionBeforeSwitchResult struct {
	Cancel bool
}

func (r SessionBeforeSwitchResult) Cancelled() bool { return r.Cancel }

// SessionBeforeForkEvent is emitted before a session fork is created.
type SessionBeforeForkEvent struct {
	Type     string
	EntryID  string
	Position string
}

// SessionBeforeForkResult allows hook to cancel a session fork or skip restoring
// copied conversation entries into the created fork.
type SessionBeforeForkResult struct {
	Cancel                  bool
	SkipConversationRestore bool
}

func (r SessionBeforeForkResult) Cancelled() bool { return r.Cancel }

// BranchSummaryResult is a hook-provided branch summary.
type BranchSummaryResult struct {
	Summary string
	Details any
}

// SessionBeforeTreeEvent is emitted before session tree navigation.
type SessionBeforeTreeEvent struct {
	Type        string
	Preparation TreePreparation
	Signal      any
}

// SessionBeforeTreeResult allows hook to cancel or provide a summary.
type SessionBeforeTreeResult struct {
	Cancel              bool
	Summary             *BranchSummaryResult
	CustomInstructions  *string
	ReplaceInstructions *bool
	Label               *string
}

func (r SessionBeforeTreeResult) Cancelled() bool { return r.Cancel }

// BashExecOptions mirrors pi's BashOperations exec options with ctx carrying
// AbortSignal cancellation:
// .agents/references/pi/packages/coding-agent/src/core/tools/bash.ts:48-57.
type BashExecOptions struct {
	OnData  func(string)
	Timeout time.Duration
	Env     map[string]string
}

// BashOperations lets native user_bash handlers route execution through an
// extension backend instead of along's local shell.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/tools/bash.ts:40-58.
type BashOperations interface {
	Exec(
		ctx context.Context,
		command string,
		cwd string,
		opts BashExecOptions,
	) (BashOpResult, error)
}

// BashOpResult is the result returned by BashOperations.Exec.
type BashOpResult struct {
	ExitCode *int
}

// BashResult is the out-of-band bash result shape exposed to user_bash hooks.
type BashResult struct {
	Output         string
	ExitCode       *int
	Cancelled      bool
	Truncated      bool
	FullOutputPath string
}

// UserBashEvent is emitted before a user bash command runs.
type UserBashEvent struct {
	Type               string
	Command            string
	ExcludeFromContext bool
	CWD                string
}

// UserBashEventResult allows hook to replace or reroute bash execution.
//
// pi's user_bash result supports operations?: BashOperations and result?:
// BashResult:
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:990-995.
//
// Native Go handlers are fully supported here; see pi BashOperations exec:
// .agents/references/pi/packages/coding-agent/src/core/tools/bash.ts:40-58.
type UserBashEventResult struct {
	Result     *BashResult
	Operations BashOperations
}

// ResourcesDiscoverEvent is emitted after session startup so extensions can
// provide extra resource paths.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:495-499.
type ResourcesDiscoverEvent struct {
	Type   string
	CWD    string
	Reason string
}

// ResourcesDiscoverResult carries extra resources to load.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:501-506.
type ResourcesDiscoverResult struct {
	SkillPaths  []string
	PromptPaths []string
	ThemePaths  []string
}

// Notification events — no return value expected.

// SessionStartEvent is emitted when a session starts after replacement.
//
// pi event definitions:
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:513-557
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:733-740
type SessionStartEvent struct {
	Type                string
	Reason              string
	PreviousSessionFile *string
}

// AgentStartEvent is emitted when the agent starts.
type AgentStartEvent struct {
	Type string
}

// AgentEndEvent is emitted when the agent finishes.
type AgentEndEvent struct {
	Type     string
	Messages []any
}

// TurnStartEvent is emitted at the start of a turn.
type TurnStartEvent struct {
	Type      string
	TurnIndex int
	Timestamp int64
}

// TurnEndEvent is emitted at the end of a turn.
type TurnEndEvent struct {
	Type        string
	TurnIndex   int
	Message     any
	ToolResults []any
}

// MessageStartEvent is emitted when a message begins streaming.
type MessageStartEvent struct {
	Type    string
	Message any
}

// MessageUpdateEvent is emitted on each message chunk.
type MessageUpdateEvent struct {
	Type                  string
	Message               any
	AssistantMessageEvent any
}

// MessageEndEvent is emitted when a message finishes streaming.
type MessageEndEvent struct {
	Type    string
	Message any
}

// MessageEndResult allows a message_end handler to replace the finalized
// message. The harness applies this via a local chained emit, not Registry.Emit.
type MessageEndResult struct {
	Message any
}

// ToolExecutionStartEvent is emitted when tool execution begins.
type ToolExecutionStartEvent struct {
	Type       string
	ToolCallId string
	ToolName   string
	Args       any
}

// ToolExecutionUpdateEvent is emitted for partial tool results.
type ToolExecutionUpdateEvent struct {
	Type          string
	ToolCallId    string
	ToolName      string
	Args          any
	PartialResult any
}

// ToolExecutionEndEvent is emitted when tool execution finishes.
type ToolExecutionEndEvent struct {
	Type       string
	ToolCallId string
	ToolName   string
	Result     any
	IsError    bool
}

// AfterProviderResponseEvent is emitted after receiving a provider response.
type AfterProviderResponseEvent struct {
	Type    string
	Status  int
	Headers map[string]string
}

// SessionShutdownEvent is emitted before a session is replaced or the process
// exits.
type SessionShutdownEvent struct {
	Type              string
	Reason            string
	TargetSessionFile *string
}

// SessionCompactEvent is emitted after session compaction completes.
type SessionCompactEvent struct {
	Type            string
	CompactionEntry any
	FromHook        bool
}

// SessionTreeEvent is emitted after session tree navigation completes.
type SessionTreeEvent struct {
	Type         string
	NewLeafId    *string
	OldLeafId    *string
	SummaryEntry any
	FromHook     bool
}

// ModelSelectEvent is emitted when the model changes.
type ModelSelectEvent struct {
	Type          string
	Model         any
	PreviousModel any
	Source        string
}

// ThinkingLevelSelectEvent is emitted when the thinking level changes.
type ThinkingLevelSelectEvent struct {
	Type          string
	Level         string
	PreviousLevel string
}

// ResourcesUpdateEvent is emitted when resources change.
type ResourcesUpdateEvent struct {
	Type              string
	Resources         any
	PreviousResources any
}

// QueueUpdateEvent is emitted when transient run queues change.
type QueueUpdateEvent struct {
	Type     string
	Steer    []any
	FollowUp []any
	NextTurn []any
}

// SavePointEvent is emitted after a turn with pending mutations.
type SavePointEvent struct {
	Type                string
	HadPendingMutations bool
}

// AbortEvent is emitted when an active run is aborted and queues are cleared.
type AbortEvent struct {
	Type            string
	ClearedSteer    []any
	ClearedFollowUp []any
}

// SettledEvent is emitted when the agent is settled.
type SettledEvent struct {
	Type          string
	NextTurnCount int
}
