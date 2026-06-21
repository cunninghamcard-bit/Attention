package enginefacade

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

type queryState struct {
	sessionID        string
	cursor           uint64
	initialized      bool
	eventReplayIndex int
	messages         []ai.Message
	model            ai.Model
	thinkingLevel    string
	activeRun        bool
}

type querySnapshot struct {
	sessionID     string
	messages      []ai.Message
	model         ai.Model
	thinkingLevel string
	activeRun     bool
}

func (f *Facade) querySnapshot(ctx context.Context) (querySnapshot, error) {
	sessionID := f.currentSessionID()
	if err := f.ensureQueryState(ctx, sessionID); err != nil {
		return querySnapshot{}, err
	}
	if f.opts.Store == nil {
		return f.copyQuerySnapshot(sessionID), nil
	}

	for {
		f.mu.Lock()
		if f.query.sessionID != sessionID {
			f.mu.Unlock()
			continue
		}
		cursor := f.query.cursor
		f.mu.Unlock()

		batch, err := f.opts.Store.ReadAfter(ctx, sessionID, cursor, 0)
		if err != nil {
			return querySnapshot{}, err
		}

		f.mu.Lock()
		if f.query.sessionID != sessionID || f.query.cursor != cursor {
			f.mu.Unlock()
			continue
		}
		if len(batch) == 0 && !f.query.initialized {
			f.query.eventReplayIndex = len(f.query.messages)
			f.query.initialized = true
		}
		for _, env := range batch {
			if env.Seq <= f.query.cursor {
				continue
			}
			f.query.apply(env, f.modelFromSpec)
		}
		snap := f.query.snapshot()
		f.mu.Unlock()
		return snap, nil
	}
}

func (f *Facade) ensureQueryState(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	if f.query.sessionID == sessionID {
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()

	messages, model, thinkingLevel, err := f.sessionBaseline(ctx, sessionID)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.query.sessionID == sessionID {
		return nil
	}
	f.query = queryState{
		sessionID:     sessionID,
		messages:      messages,
		model:         model,
		thinkingLevel: thinkingLevel,
	}
	return nil
}

func (f *Facade) sessionBaseline(
	ctx context.Context,
	sessionID string,
) ([]ai.Message, ai.Model, string, error) {
	f.mu.Lock()
	model := f.model
	thinkingLevel := f.thinkingLevel
	f.mu.Unlock()

	if f.opts.Repo == nil {
		return []ai.Message{}, model, thinkingLevel, nil
	}

	s, ok, err := f.opts.Repo.Get(ctx, sessionID)
	if err != nil {
		return nil, ai.Model{}, "", err
	}
	if !ok {
		return []ai.Message{}, model, thinkingLevel, nil
	}

	sessionCtx, err := s.BuildContext(ctx)
	if err != nil {
		return nil, ai.Model{}, "", err
	}
	messages := make([]ai.Message, 0, len(sessionCtx.Messages))
	for _, msg := range sessionCtx.Messages {
		aiMsg, ok := message.AsAIMessage(msg)
		if ok {
			messages = append(messages, aiMsg)
		}
	}
	if sessionCtx.Model != nil && sessionCtx.Model.ModelID != "" {
		model = f.modelFromRef(*sessionCtx.Model)
	}
	if sessionCtx.ThinkingLevel != "" {
		thinkingLevel = sessionCtx.ThinkingLevel
	}
	return messages, model, thinkingLevel, nil
}

func (f *Facade) copyQuerySnapshot(sessionID string) querySnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.query.sessionID != sessionID {
		return querySnapshot{sessionID: sessionID}
	}
	return f.query.snapshot()
}

func (s *queryState) snapshot() querySnapshot {
	return querySnapshot{
		sessionID:     s.sessionID,
		messages:      append([]ai.Message(nil), s.messages...),
		model:         s.model,
		thinkingLevel: s.thinkingLevel,
		activeRun:     s.activeRun,
	}
}

func (s *queryState) apply(env protocol.Envelope, resolve func(string) ai.Model) {
	s.cursor = env.Seq
	switch env.Kind {
	case protocol.KindRunStarted:
		s.activeRun = true
	case protocol.KindRunCompleted, protocol.KindRunFailed, protocol.KindRunCancelled:
		s.activeRun = false
	case protocol.KindMessageCompleted:
		var p envelopePayload
		if json.Unmarshal(env.Payload, &p) == nil && p.Message != nil {
			s.appendMessage(*p.Message)
		}
	case protocol.KindSessionChanged:
		var p envelopePayload
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		if p.Model != "" {
			s.model = resolve(p.Model)
		}
		if p.ThinkingLevel != "" {
			s.thinkingLevel = p.ThinkingLevel
		}
	}
	s.initialized = true
}

func (s *queryState) appendMessage(msg ai.Message) {
	if s.eventReplayIndex < len(s.messages) &&
		reflect.DeepEqual(s.messages[s.eventReplayIndex], msg) {
		s.eventReplayIndex++
		return
	}
	s.messages = append(s.messages, msg)
	s.eventReplayIndex = len(s.messages)
}

func (f *Facade) modelFromRef(ref session.ModelRef) ai.Model {
	if ref.Provider != "" {
		spec := ref.Provider + "/" + ref.ModelID
		return f.modelFromSpec(spec)
	}
	return f.modelFromSpec(ref.ModelID)
}

func (f *Facade) modelFromSpec(spec string) ai.Model {
	providerName, modelID, ok := strings.Cut(spec, "/")
	if !ok {
		providerName = ""
		modelID = spec
	}
	if f.opts.Provider != nil {
		if providerName != "" {
			if m, ok := f.opts.Provider.ResolveByProvider(providerName, modelID); ok {
				return m
			}
		} else if m, ok := f.opts.Provider.Resolve(modelID); ok {
			return m
		}
	}
	return ai.Model{Provider: providerName, ID: modelID}
}

func (f *Facade) contextUsage(messages []ai.Message, model ai.Model) *compat.ContextUsage {
	contextWindow := model.ContextWindow
	if contextWindow <= 0 {
		return nil
	}
	tokens := estimateContextTokens(messages)
	return &compat.ContextUsage{
		Tokens:        tokens,
		ContextWindow: contextWindow,
		Percent:       float64(tokens) / float64(contextWindow) * 100,
	}
}

func estimateContextTokens(messages []ai.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		usage := assistantUsage(messages[i])
		if usage == nil {
			continue
		}
		tokens := calculateContextTokens(usage)
		for _, msg := range messages[i+1:] {
			tokens += estimateTokens(msg)
		}
		return tokens
	}

	tokens := 0
	for _, msg := range messages {
		tokens += estimateTokens(msg)
	}
	return tokens
}

func assistantUsage(msg ai.Message) *ai.Usage {
	if msg.Role != ai.RoleAssistant {
		return nil
	}
	if msg.StopReason == ai.StopReasonAborted || msg.StopReason == ai.StopReasonError {
		return nil
	}
	return msg.Usage
}

func calculateContextTokens(usage *ai.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.TotalTokens != 0 {
		return usage.TotalTokens
	}
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

func estimateTokens(msg ai.Message) int {
	chars := 0
	switch msg.Role {
	case ai.RoleUser:
		chars = textContentChars(msg.Content)
	case ai.RoleAssistant:
		for _, block := range msg.Content {
			switch block.Type {
			case ai.ContentText:
				chars += len(block.Text)
			case ai.ContentThinking:
				chars += len(block.Thinking)
			case ai.ContentToolCall:
				chars += len(block.ToolName) + jsonStringLength(block.Arguments)
			}
		}
	case ai.RoleToolResult:
		chars = toolResultContentChars(msg.Content)
	}
	return (chars + 3) / 4
}

func textContentChars(blocks []ai.ContentBlock) int {
	chars := 0
	for _, block := range blocks {
		if block.Type == ai.ContentText {
			chars += len(block.Text)
		}
	}
	return chars
}

func toolResultContentChars(blocks []ai.ContentBlock) int {
	chars := 0
	for _, block := range blocks {
		switch block.Type {
		case ai.ContentText:
			chars += len(block.Text)
		case ai.ContentImage:
			chars += len(block.ImageData)
		}
	}
	return chars
}

func jsonStringLength(value any) int {
	if value == nil {
		return 0
	}
	b, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(b)
}

var thinkingLevelsInOrder = []compatThinkingLevel{
	agentloop.ThinkingOff,
	agentloop.ThinkingMinimal,
	agentloop.ThinkingLow,
	agentloop.ThinkingMedium,
	agentloop.ThinkingHigh,
	agentloop.ThinkingXHigh,
}

func supportedThinkingLevels(model ai.Model) []compatThinkingLevel {
	if !model.Reasoning {
		return []compatThinkingLevel{agentloop.ThinkingOff}
	}
	if len(model.ThinkingLevelMap) == 0 {
		return append([]compatThinkingLevel(nil), thinkingLevelsInOrder...)
	}

	levels := []compatThinkingLevel{}
	for _, level := range thinkingLevelsInOrder {
		mapped, ok := model.ThinkingLevelMap[string(level)]
		if ok && mapped != nil {
			levels = append(levels, level)
		}
	}
	return levels
}

func clampThinkingLevel(model ai.Model, level compatThinkingLevel) compatThinkingLevel {
	levels := supportedThinkingLevels(model)
	for _, supported := range levels {
		if supported == level {
			return level
		}
	}

	requestedIndex := thinkingLevelIndex(level)
	if requestedIndex == -1 {
		if len(levels) > 0 {
			return levels[0]
		}
		return agentloop.ThinkingOff
	}
	for i := requestedIndex; i < len(thinkingLevelsInOrder); i++ {
		candidate := thinkingLevelsInOrder[i]
		if containsThinkingLevel(levels, candidate) {
			return candidate
		}
	}
	for i := requestedIndex - 1; i >= 0; i-- {
		candidate := thinkingLevelsInOrder[i]
		if containsThinkingLevel(levels, candidate) {
			return candidate
		}
	}
	return agentloop.ThinkingOff
}

func containsThinkingLevel(levels []compatThinkingLevel, target compatThinkingLevel) bool {
	for _, level := range levels {
		if level == target {
			return true
		}
	}
	return false
}

func thinkingLevelIndex(target compatThinkingLevel) int {
	for i, level := range thinkingLevelsInOrder {
		if level == target {
			return i
		}
	}
	return -1
}
