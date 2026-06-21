package agentloop

import (
	"context"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

// ThinkingLevel represents the reasoning effort requested from the model.
type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// ThinkingBudgets maps thinking levels to token budgets.
type ThinkingBudgets struct {
	Minimal int
	Low     int
	Medium  int
	High    int
}

// EventType is the discriminator for agent lifecycle events.
type EventType string

const (
	AgentStart          EventType = "agent_start"
	AgentEnd            EventType = "agent_end"
	TurnStart           EventType = "turn_start"
	TurnEnd             EventType = "turn_end"
	MessageStart        EventType = "message_start"
	MessageUpdate       EventType = "message_update"
	MessageEnd          EventType = "message_end"
	ToolExecutionStart  EventType = "tool_execution_start"
	ToolExecutionUpdate EventType = "tool_execution_update"
	ToolExecutionEnd    EventType = "tool_execution_end"
)

// Event is a lifecycle event emitted during an agent loop invocation.
type Event struct {
	Type EventType

	Message               message.AgentMessage // message_start, message_end, message_update, turn_end
	AssistantMessageEvent *ai.StreamEvent      // message_update
	Messages              []message.AgentMessage
	ToolResults           []ai.Message
	ToolCallID            string
	ToolName              string
	Args                  any
	Result                any
	PartialResult         any
	IsError               bool
}

// StreamFunc is the injectable provider stream boundary.
type StreamFunc func(
	ctx context.Context,
	model ai.Model,
	llmCtx ai.Context,
	opts ai.SimpleStreamOptions,
) *ai.AssistantMessageEventStream

// Config is the static configuration for a single agent loop invocation.
type Config struct {
	Model           ai.Model
	ThinkingLevel   ThinkingLevel
	Temperature     float64
	MaxTokens       int
	APIKey          string
	Transport       ai.Transport
	CacheRetention  ai.CacheRetention
	SessionID       string
	OnPayload       func(payload any, model ai.Model) (any, bool, error)
	OnResponse      func(response ai.ProviderResponse, model ai.Model) error
	Headers         map[string]string
	Timeout         time.Duration
	MaxRetries      int
	MaxRetryDelayMs int
	Metadata        map[string]any
	ThinkingBudgets *ThinkingBudgets
	ExecutionMode   tool.ToolExecutionMode

	ConvertToLLM        func(messages []message.AgentMessage) ([]ai.Message, error)
	TransformContext    func(ctx context.Context, messages []message.AgentMessage) ([]message.AgentMessage, error)
	GetAPIKey           func(ctx context.Context, provider string) (string, error)
	BeforeToolCall      func(ctx context.Context, call BeforeToolCallContext) (*BeforeToolCallResult, error)
	AfterToolCall       func(ctx context.Context, call AfterToolCallContext) (*AfterToolCallResult, error)
	ShouldStopAfterTurn func(ctx context.Context, call ShouldStopAfterTurnContext) (bool, error)
	PrepareNextTurn     func(ctx context.Context, call PrepareNextTurnContext) (*TurnUpdate, error)
	GetSteeringMessages func(context.Context) ([]message.AgentMessage, error)
	GetFollowUpMessages func(context.Context) ([]message.AgentMessage, error)
}

// Context is a snapshot of the state visible to one loop invocation.
type Context struct {
	SystemPrompt string
	Messages     []message.AgentMessage
	Tools        []tool.Tool
}

// BeforeToolCallContext is passed to the BeforeToolCall hook.
type BeforeToolCallContext struct {
	AssistantMessage *ai.Message
	ToolCall         ai.ContentBlock
	Args             map[string]any
	Context          Context
}

// BeforeToolCallResult is returned by BeforeToolCall.
type BeforeToolCallResult struct {
	Block  bool
	Reason string
	Args   map[string]any
}

// AfterToolCallContext is passed to the AfterToolCall hook.
type AfterToolCallContext struct {
	AssistantMessage *ai.Message
	ToolCall         ai.ContentBlock
	Args             map[string]any
	Result           tool.Result
	IsError          bool
	Context          Context
}

// AfterToolCallResult is returned by AfterToolCall.
type AfterToolCallResult struct {
	Content   []ai.ContentBlock
	Details   any
	IsError   *bool
	Terminate *bool
}

// ShouldStopAfterTurnContext is passed to ShouldStopAfterTurn.
type ShouldStopAfterTurnContext struct {
	Message     *ai.Message
	ToolResults []ai.Message
	Context     Context
	NewMessages []message.AgentMessage
}

// PrepareNextTurnContext is passed to PrepareNextTurn.
type PrepareNextTurnContext struct {
	Message     *ai.Message
	ToolResults []ai.Message
	Context     Context
	NewMessages []message.AgentMessage
}

// TurnUpdate carries replacement state for the next turn.
type TurnUpdate struct {
	Context       *Context
	Model         *ai.Model
	ThinkingLevel *ThinkingLevel
}

// EventSink is the loop-to-caller event boundary.
type EventSink func(event Event) error
