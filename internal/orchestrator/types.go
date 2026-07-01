// Package orchestrator owns runtime state and assembles the session, hooks,
// tools, extensions, and stateless harness into a mode-facing facade.
package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type phase string

const (
	phaseIdle          phase = "idle"
	phaseTurn          phase = "turn"
	phaseCompaction    phase = "compaction"
	phaseBranchSummary phase = "branch_summary"
)

type agentHarness interface {
	Prompt(context.Context, []message.AgentMessage, harness.TurnState) (ai.Message, error)
	Continue(context.Context, harness.TurnState) (ai.Message, error)
	Compact(context.Context, harness.TurnState, string) (harness.CompactionResult, error)
	NavigateTree(
		context.Context,
		session.EntryID,
		harness.TurnState,
		harness.NavigationOptions,
	) (harness.NavigationResult, error)
}

// ErrBusy is matched by errors.Is when the orchestrator already has an active
// run.
var ErrBusy = errors.New("orchestrator busy")

// BusyError reports the phase that rejected a new active operation.
type BusyError struct {
	Phase string
}

func (e *BusyError) Error() string {
	if e == nil || e.Phase == "" {
		return ErrBusy.Error()
	}
	return fmt.Sprintf("%s: phase %s", ErrBusy, e.Phase)
}

func (e *BusyError) Unwrap() error {
	return ErrBusy
}

// ExtensionSource identifies an extension factory loaded during assembly.
type ExtensionSource = extension.Source

type ToolBuilder func(pluginBinDirs []string) ([]extension.ToolDefinition, error)

// NewOptions configures a fresh orchestrator session. If Session is nil, Repo
// and CreateOptions are used to create a JSONL session.
type NewOptions struct {
	Repo          *session.JsonlSessionRepo
	CreateOptions session.JsonlSessionCreateOptions
	Session       harness.Session

	Model   ai.Model
	ModelID string
	// ModelProvider is an optional provider hint (CLI --provider) used to
	// disambiguate a model id that exists under multiple providers.
	ModelProvider string
	Provider      *provider.Registry
	ThinkingLevel agentloop.ThinkingLevel
	SystemPrompt  string
	// AppendSystemPrompt is appended verbatim to the resolved system prompt
	// (CLI --append-system-prompt). Mirrors pi's appendSystemPrompt.
	AppendSystemPrompt string
	GetAPIKey          func(ctx context.Context, provider string) (string, error)
	Settings           config.Settings
	SettingsManager    *config.Manager

	Extensions []ExtensionSource
	// HooksPath points at a declarative shell-hooks file (hooks.json). A missing
	// or empty file is a no-op.
	HooksPath string
	Tools     []extension.ToolDefinition
	// ToolBuilder rebuilds the selected tool set on reload after plugin bin
	// directories have been refreshed.
	ToolBuilder ToolBuilder

	PromptTemplates []resource.PromptTemplate
	Skills          []resource.Skill
	PromptPaths     []string
	SkillPaths      []string
	AgentDir        string
	ContextFiles    []resource.ContextFile
	Diagnostics     []resource.ResourceDiagnostic

	ExecutionEnv execenv.ExecutionEnv
}

// OpenOptions configures an orchestrator around an existing session. If
// Session is nil, Repo and Metadata are used to open a JSONL session.
type OpenOptions struct {
	Repo     *session.JsonlSessionRepo
	Metadata session.Metadata
	Session  harness.Session

	Model   ai.Model
	ModelID string
	// ModelProvider is an optional provider hint (CLI --provider) used to
	// disambiguate a model id that exists under multiple providers.
	ModelProvider string
	Provider      *provider.Registry
	ThinkingLevel agentloop.ThinkingLevel
	SystemPrompt  string
	// AppendSystemPrompt is appended verbatim to the resolved system prompt
	// (CLI --append-system-prompt). Mirrors pi's appendSystemPrompt.
	AppendSystemPrompt string
	GetAPIKey          func(ctx context.Context, provider string) (string, error)
	Settings           config.Settings
	SettingsManager    *config.Manager

	Extensions []ExtensionSource
	// HooksPath points at a declarative shell-hooks file (hooks.json). A missing
	// or empty file is a no-op.
	HooksPath string
	Tools     []extension.ToolDefinition
	// ToolBuilder rebuilds the selected tool set on reload after plugin bin
	// directories have been refreshed.
	ToolBuilder ToolBuilder

	PromptTemplates []resource.PromptTemplate
	Skills          []resource.Skill
	PromptPaths     []string
	SkillPaths      []string
	AgentDir        string
	ContextFiles    []resource.ContextFile
	Diagnostics     []resource.ResourceDiagnostic

	ExecutionEnv execenv.ExecutionEnv
}

type runtimeConfig struct {
	model                  ai.Model
	modelID                string
	modelProvider          string
	provider               *provider.Registry
	repo                   *session.JsonlSessionRepo
	thinkingLevel          agentloop.ThinkingLevel
	systemPrompt           string
	appendSystemPrompt     string
	getAPIKey              func(ctx context.Context, provider string) (string, error)
	settings               config.Settings
	settingsManager        *config.Manager
	extensions             []ExtensionSource
	hooksPath              string
	tools                  []extension.ToolDefinition
	toolBuilder            ToolBuilder
	promptTemplates        []resource.PromptTemplate
	skills                 []resource.Skill
	promptPaths            []string
	skillPaths             []string
	agentDir               string
	contextFiles           []resource.ContextFile
	diagnostics            []resource.ResourceDiagnostic
	executionEnv           execenv.ExecutionEnv
	recoverState           bool
	resourceDiscoverReason string
}

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

// ResourcesSnapshot mirrors pi AgentHarnessResources for resources_update:
// skills and prompt templates only.
type ResourcesSnapshot struct {
	Skills          []ResourceSummary `json:"skills"`
	PromptTemplates []ResourceSummary `json:"promptTemplates"`
}

// Event is the narrow lifecycle event shape exposed to modes.
type Event struct {
	Type    string
	Message *ai.Message
	Delta   *ai.StreamEvent
	// Non-streaming payload names mirror pi AgentEvent exactly:
	// .agents/references/pi/packages/agent/src/types.ts:405-418.
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

// PromptInput is normalized into one user message for Harness.Prompt.
type PromptInput struct {
	Text    string
	Content []ai.ContentBlock
	Message message.AgentMessage
	Source  string
	// StreamingBehavior mirrors pi rpc prompt's busy-routing option:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-types.ts:21.
	StreamingBehavior string
	// PreflightResult, if set, is called once with true as soon as the prompt
	// is accepted (turn slot acquired), before the turn runs. Mirrors pi's
	// session.prompt preflightResult callback (rpc-mode.ts:398): rpc mode uses
	// it to emit the success response before the turn completes. A prompt that
	// fails preflight returns an error without invoking this.
	PreflightResult func(bool)
}

// UserInput is queued as transient steering or follow-up user input.
type UserInput struct {
	Text    string
	Content []ai.ContentBlock
	Message message.AgentMessage
}

// PromptResult is the outcome of a prompt. Handled means an input hook consumed
// the prompt before a user turn or assistant message was produced.
type PromptResult struct {
	Message ai.Message
	Handled bool
}

type CommandNotification struct {
	Message string
	Level   string
}

// AbortResult reports whether an active context was canceled and what
// transient queue state was discarded.
type AbortResult struct {
	Aborted         bool
	ClearedSteer    []message.AgentMessage
	ClearedFollowUp []message.AgentMessage
	ClearedNextTurn []message.AgentMessage
}

// SlashCommand is a prompt-invokable command exposed to rpc get_commands.
type SlashCommand struct {
	Name         string
	Description  string
	ArgumentHint string
	Source       string
	SourceInfo   resource.SourceInfo
}

// CompactOptions controls Harness.Compact.
type CompactOptions struct {
	CustomInstructions string
}

type CompactResult = harness.CompactionResult
type NavOptions = harness.NavigationOptions
type NavResult = harness.NavigationResult

type pendingWrite interface {
	apply(context.Context, harness.Session) error
}

type modelChange struct {
	model ai.Model
}

func (w modelChange) apply(ctx context.Context, s harness.Session) error {
	_, err := s.AppendModelChange(ctx, w.model.Provider, w.model.ID)
	return err
}

type thinkingLevelChange struct {
	level agentloop.ThinkingLevel
}

func (w thinkingLevelChange) apply(ctx context.Context, s harness.Session) error {
	_, err := s.AppendThinkingLevelChange(ctx, string(w.level))
	return err
}
