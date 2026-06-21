package harness

import (
	"context"
	"maps"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

// Harness is the stateless execution layer. All mutable state lives in
// HarnessConfig (immutable after construction) or TurnState (per-call).
type Harness struct {
	cfg HarnessConfig
}

// HarnessConfig holds dependencies injected by the Orchestrator.
// Immutable after construction.
type HarnessConfig struct {
	Session            Session        // Consumer-side session interface
	Hooks              *hook.Registry // Hook system
	Tools              []tool.Tool
	CompactionSettings hook.CompactionSettings
	GetProviderAuth    func(ctx context.Context, model ai.Model) (ProviderAuth, error)
	GetAPIKey          func(ctx context.Context, provider string) (string, error)
	stream             agentloop.StreamFunc
}

// ProviderAuth is the provider auth bundle used before provider hooks run.
type ProviderAuth struct {
	APIKey  string
	Headers map[string]string
}

// TurnState carries per-call configuration from the Orchestrator.
type TurnState struct {
	Model               ai.Model
	ThinkingLevel       agentloop.ThinkingLevel
	SystemPrompt        string
	SystemPromptOptions hook.SystemPromptOptions
	ActiveTools         []tool.Tool
	GetSteeringMessages func(context.Context) ([]message.AgentMessage, error)
	GetFollowUpMessages func(context.Context) ([]message.AgentMessage, error)
	SessionID           string
	Resources           Resources
	// Refresh returns the current Model and ThinkingLevel, picking up changes
	// made via SetModel/SetThinkingLevel during a run. prepareNextTurn calls
	// this so the next turn uses the latest values, mirroring pi's
	// getTurnState()/setTurnState() pattern. Nil means use the initial values.
	Refresh func() (ai.Model, agentloop.ThinkingLevel)
}

// Resources is an opaque value representing skills and prompt templates.
type Resources = any

// CompactionResult is the outcome of a Compact call.
type CompactionResult struct {
	Summary          string
	FirstKeptEntryID session.EntryID
	TokensBefore     int
	Details          any
	FromHook         bool
}

// NavigationResult is the outcome of a NavigateTree call.
type NavigationResult struct {
	Cancelled    bool
	EditorText   string
	SummaryEntry *session.SessionEntry
}

// NavigationOptions controls NavigateTree behavior.
type NavigationOptions struct {
	Summarize           bool
	CustomInstructions  string
	ReplaceInstructions bool
	Label               string
}

// New creates a Harness. cfg is immutable after this call.
func New(cfg HarnessConfig) *Harness {
	return &Harness{cfg: cfg}
}

func (h *Harness) providerAuth(ctx context.Context, model ai.Model) (ProviderAuth, error) {
	if h.cfg.GetProviderAuth != nil {
		auth, err := h.cfg.GetProviderAuth(ctx, model)
		if err != nil {
			return ProviderAuth{}, err
		}
		auth.Headers = maps.Clone(auth.Headers)
		return auth, nil
	}
	if h.cfg.GetAPIKey == nil {
		return ProviderAuth{}, nil
	}
	key, err := h.cfg.GetAPIKey(ctx, model.Provider)
	if err != nil {
		return ProviderAuth{}, err
	}
	return ProviderAuth{APIKey: key}, nil
}
