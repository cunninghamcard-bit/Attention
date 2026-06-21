package orchestrator

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

// Snapshot is the mode-facing view of orchestrator state, backing the rpc
// get_state command. It mirrors pi RpcSessionState (rpc-types.ts:90).
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

// SessionStats is the mode-facing view backing rpc get_session_stats. It
// mirrors pi SessionStats:
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:216-234
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2877-2919.
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

// Snapshot reports current model/thinking/streaming/session state.
func (o *Orchestrator) Snapshot() Snapshot {
	o.mu.Lock()
	var getSessionName func() (string, bool)
	snap := Snapshot{
		Model:                 o.model,
		ThinkingLevel:         o.thinkingLevel,
		IsStreaming:           o.phase == phaseTurn,
		IsCompacting:          o.phase == phaseCompaction,
		SteeringMode:          string(o.steeringMode),
		FollowUpMode:          string(o.followUpMode),
		AutoCompactionEnabled: o.autoCompactionEnabled,
		PendingMessageCount:   len(o.steerQueue) + len(o.followUpQueue) + len(o.nextTurnQueue),
	}
	if o.session != nil {
		md := o.session.GetMetadata()
		snap.SessionID = md.ID
		snap.SessionFile = md.Path
		getSessionName = o.session.GetSessionName
	}
	o.mu.Unlock()

	if getSessionName != nil {
		if name, ok := getSessionName(); ok {
			snap.SessionName = name
		}
	}
	snap.MessageCount = len(o.Messages())
	return snap
}

// SessionStats reports current session message counts and assistant usage.
func (o *Orchestrator) SessionStats() SessionStats {
	o.mu.Lock()
	stats := SessionStats{}
	if o.session != nil {
		md := o.session.GetMetadata()
		stats.SessionID = md.ID
		stats.SessionFile = md.Path
	}
	o.mu.Unlock()

	messages := o.Messages()
	stats.TotalMessages = len(messages)
	for _, msg := range messages {
		switch msg.Role {
		case ai.RoleUser:
			stats.UserMessages++
		case ai.RoleAssistant:
			stats.AssistantMessages++
			stats.addAssistantUsage(msg)
		case ai.RoleToolResult:
			stats.ToolResults++
		}
	}
	stats.Tokens.Total = stats.Tokens.Input +
		stats.Tokens.Output +
		stats.Tokens.CacheRead +
		stats.Tokens.CacheWrite
	stats.ContextUsage = o.contextUsage()
	return stats
}

func (s *SessionStats) addAssistantUsage(msg ai.Message) {
	for _, block := range msg.Content {
		if block.Type == ai.ContentToolCall {
			s.ToolCalls++
		}
	}

	usage := msg.Usage
	if usage == nil {
		return
	}
	s.Tokens.Input += usage.Input
	s.Tokens.Output += usage.Output
	s.Tokens.CacheRead += usage.CacheRead
	s.Tokens.CacheWrite += usage.CacheWrite
	if usage.Cost != nil {
		s.Cost += usage.Cost.Total
	}
}

// Messages returns the current session context messages as ai.Message values,
// backing the rpc get_messages command. ai.Message is along's uniform wire
// message shape (the event stream uses it too).
func (o *Orchestrator) Messages() []ai.Message {
	if o.session == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionCtx, err := o.session.BuildContext(ctx)
	if err != nil {
		return nil
	}
	out := make([]ai.Message, 0, len(sessionCtx.Messages))
	for _, msg := range sessionCtx.Messages {
		if aiMsg, ok := message.AsAIMessage(msg); ok {
			out = append(out, aiMsg)
		}
	}
	return out
}

// ResolveModel looks up a model by provider+id among auth-configured models,
// backing the rpc set_model command. pi matches both fields against the
// auth-filtered getAvailable() set (rpc-mode.ts:463-465), so a model without
// configured auth is not resolvable (pi setModel likewise rejects it,
// agent-session.ts:1417).
func (o *Orchestrator) ResolveModel(ctx context.Context, providerName, id string) (ai.Model, bool) {
	if o.provider == nil {
		return ai.Model{}, false
	}
	for _, m := range o.provider.Available(ctx) {
		if m.Provider == providerName && m.ID == id {
			return m, true
		}
	}
	return ai.Model{}, false
}

// AvailableModels lists auth-configured models, backing the rpc
// get_available_models command (pi uses auth-filtered getAvailable(),
// rpc-mode.ts:481-483).
func (o *Orchestrator) AvailableModels(ctx context.Context) []ai.Model {
	if o.provider == nil {
		return nil
	}
	return o.provider.Available(ctx)
}
