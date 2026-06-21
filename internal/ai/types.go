package ai

import "time"

type ContentBlockType string

const (
	ContentText     ContentBlockType = "text"
	ContentThinking ContentBlockType = "thinking"
	ContentImage    ContentBlockType = "image"
	ContentToolCall ContentBlockType = "toolCall"
)

type ContentBlock struct {
	Type ContentBlockType `json:"type"`

	Text          string `json:"text,omitempty"`
	TextSignature string `json:"textSignature,omitempty"`

	Thinking          string `json:"thinking,omitempty"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	Redacted          bool   `json:"redacted,omitempty"`

	ImageData string `json:"data,omitempty"`
	MimeType  string `json:"mimeType,omitempty"`

	ToolCallID       string         `json:"id,omitempty"`
	ToolName         string         `json:"name,omitempty"`
	Arguments        map[string]any `json:"arguments,omitempty"`
	ThoughtSignature string         `json:"thoughtSignature,omitempty"`
}

type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "toolResult"
)

func (Message) IsAgentMessage() {}

type Message struct {
	Role      Role           `json:"role"`
	Content   []ContentBlock `json:"content,omitempty"`
	Timestamp int64          `json:"timestamp"`

	API           API                          `json:"api,omitempty"`
	Provider      string                       `json:"provider,omitempty"`
	Model         string                       `json:"model,omitempty"`
	ResponseModel string                       `json:"responseModel,omitempty"`
	ResponseID    string                       `json:"responseId,omitempty"`
	Usage         *Usage                       `json:"usage,omitempty"`
	StopReason    StopReason                   `json:"stopReason,omitempty"`
	ErrorMessage  string                       `json:"errorMessage,omitempty"`
	Diagnostics   []AssistantMessageDiagnostic `json:"diagnostics,omitempty"`

	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	Details    any    `json:"details,omitempty"`
}

type EventType int

const (
	EventUnknown EventType = iota

	// Per-content-block lifecycle events, split by content type. Mirrors pi's
	// AssistantMessageEvent union (ai/types.ts:358-370).
	EventMessageStart  // before first content block
	EventTextStart     // text content block began
	EventTextDelta     // text content block partial update
	EventTextEnd       // text content block finished
	EventThinkingStart // thinking content block began
	EventThinkingDelta // thinking content block partial update
	EventThinkingEnd   // thinking content block finished
	EventToolCallStart // tool call content block began
	EventToolCallDelta // tool call content block partial update
	EventToolCallEnd   // tool call content block finished
	EventMessageDone   // successful completion (stop/length/toolUse)
	EventMessageError  // error or aborted completion

	// EventMessageComplete is the legacy catch-all kept for stream.go result
	// collection. Provider adapters still yield it as the final event.
	EventMessageComplete
)

type StreamEvent struct {
	Type    EventType
	Index   int
	Delta   *ContentBlock
	Message *Message
	Usage   *Usage
}

func contentBlockStartEvent(ct ContentBlockType) EventType {
	switch ct {
	case ContentThinking:
		return EventThinkingStart
	case ContentToolCall:
		return EventToolCallStart
	default:
		return EventTextStart
	}
}

func contentBlockDeltaEvent(ct ContentBlockType) EventType {
	switch ct {
	case ContentThinking:
		return EventThinkingDelta
	case ContentToolCall:
		return EventToolCallDelta
	default:
		return EventTextDelta
	}
}

func contentBlockEndEvent(ct ContentBlockType) EventType {
	switch ct {
	case ContentThinking:
		return EventThinkingEnd
	case ContentToolCall:
		return EventToolCallEnd
	default:
		return EventTextEnd
	}
}

// IsContentBlockStart returns true for any *_start content block event.
func (t EventType) IsContentBlockStart() bool {
	return t == EventTextStart || t == EventThinkingStart || t == EventToolCallStart
}

// IsContentBlockDelta returns true for any *_delta content block event.
func (t EventType) IsContentBlockDelta() bool {
	return t == EventTextDelta || t == EventThinkingDelta || t == EventToolCallDelta
}

// IsContentBlockEnd returns true for any *_end content block event.
func (t EventType) IsContentBlockEnd() bool {
	return t == EventTextEnd || t == EventThinkingEnd || t == EventToolCallEnd
}

type API string

const (
	APIAnthropicMessages    API = "anthropic-messages"
	APIOpenAICompletions    API = "openai-completions"
	APIOpenAIResponses      API = "openai-responses"
	APIOpenAICodexResponses API = "openai-codex-responses"
)

type StopReason string

const (
	StopReasonStop    StopReason = "stop"
	StopReasonLength  StopReason = "length"
	StopReasonToolUse StopReason = "toolUse"
	StopReasonError   StopReason = "error"
	StopReasonAborted StopReason = "aborted"
)

type Usage struct {
	Input       int   `json:"input"`
	Output      int   `json:"output"`
	CacheRead   int   `json:"cacheRead"`
	CacheWrite  int   `json:"cacheWrite"`
	TotalTokens int   `json:"totalTokens"`
	Cost        *Cost `json:"cost,omitempty"`
}

type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

type AssistantMessageDiagnostic struct {
	Source  string         `json:"source,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type Transport string

const (
	TransportSSE             Transport = "sse"
	TransportWebSocket       Transport = "websocket"
	TransportWebSocketCached Transport = "websocket-cached"
	TransportAuto            Transport = "auto"
)

type CacheRetention string

const (
	CacheRetentionNone  CacheRetention = "none"
	CacheRetentionShort CacheRetention = "short"
	CacheRetentionLong  CacheRetention = "long"
)

type ProviderResponse struct {
	Status  int
	Headers map[string]string
}

// ThinkingBudgets maps thinking levels to token budgets for providers that use
// token-based reasoning controls.
type ThinkingBudgets struct {
	Minimal int
	Low     int
	Medium  int
	High    int
}

type Context struct {
	SystemPrompt string
	Messages     []Message
	Tools        []Tool
}

type SimpleStreamOptions struct {
	Temperature    float64
	MaxTokens      int
	APIKey         string
	Transport      Transport
	CacheRetention CacheRetention
	SessionID      string
	Headers        map[string]string
	Timeout        time.Duration
	// MaxRetries uses 0 for the provider default, positive values for explicit
	// retry counts, and negative values to disable retries.
	MaxRetries      int
	Metadata        map[string]any
	Reasoning       string
	ThinkingBudgets *ThinkingBudgets
	OnPayload       func(payload any, model Model) (any, bool, error)
	OnResponse      func(response ProviderResponse, model Model) error
}

type StreamOptions struct {
	Model          string
	ResolvedModel  Model
	Messages       []Message
	SystemPrompt   string
	Tools          []Tool
	Temperature    float64
	MaxTokens      int
	APIKey         string
	Transport      Transport
	CacheRetention CacheRetention
	SessionID      string
	Headers        map[string]string
	Timeout        time.Duration
	// MaxRetries uses 0 for the provider default, positive values for explicit
	// retry counts, and negative values to disable retries.
	MaxRetries      int
	Metadata        map[string]any
	Reasoning       string
	ThinkingBudgets *ThinkingBudgets
	OnPayload       func(payload any, model Model) (any, bool, error)
	OnResponse      func(response ProviderResponse, model Model) error
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}
