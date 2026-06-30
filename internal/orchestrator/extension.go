package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

var (
	extensionShutdownMu       sync.RWMutex
	extensionShutdownCallback = func() {}
)

func (o *Orchestrator) bindExtensionCommands(ext extension.Extension) error {
	for name, def := range ext.Commands {
		if _, exists := o.commands[name]; exists {
			return fmt.Errorf("orchestrator: duplicate command %q", name)
		}
		if def.Source == (resource.SourceInfo{}) {
			def.Source = extensionCommandSource(ext.Path)
		}
		o.commands[name] = def
	}
	return nil
}

func extensionCommandSource(path string) resource.SourceInfo {
	if path == "" {
		return resource.SourceInfo{}
	}
	return resource.SourceInfo{
		Kind: resource.SourceKind("extension"),
		Path: path,
	}
}

func registerExtensionProviders(
	registry *provider.Registry,
	defs map[string]extension.ProviderDefinition,
) error {
	for name, def := range defs {
		cfg, err := adaptProviderDefinition(name, def)
		if err != nil {
			return err
		}
		if err := registry.RegisterProvider(name, cfg); err != nil {
			return fmt.Errorf("orchestrator: register extension provider %q: %w", name, err)
		}
	}
	return nil
}

func adaptProviderDefinition(
	_ string,
	def extension.ProviderDefinition,
) (provider.ProviderConfig, error) {
	return provider.ProviderConfig{
		Name:           copyStringPtr(def.Name),
		BaseURL:        copyStringPtr(def.BaseURL),
		APIKey:         copyStringPtr(def.APIKey),
		API:            copyStringPtr(def.API),
		Headers:        maps.Clone(def.Headers),
		AuthHeader:     copyBoolPtr(def.AuthHeader),
		Compat:         def.Compat,
		Models:         adaptProviderModels(def.Models),
		ModelOverrides: adaptProviderModelOverrides(def.ModelOverrides),
	}, nil
}

func adaptProviderModels(defs []extension.ProviderModel) []provider.ModelDefinition {
	if defs == nil {
		return nil
	}
	models := make([]provider.ModelDefinition, 0, len(defs))
	for _, def := range defs {
		models = append(models, provider.ModelDefinition{
			ID:               def.ID,
			Name:             copyStringPtr(def.Name),
			API:              copyStringPtr(def.API),
			BaseURL:          copyStringPtr(def.BaseURL),
			Reasoning:        copyBoolPtr(def.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(def.ThinkingLevelMap),
			Input:            append([]ai.InputCapability(nil), def.Input...),
			Cost:             adaptProviderModelCost(def.Cost),
			ContextWindow:    copyIntPtr(def.ContextWindow),
			MaxTokens:        copyIntPtr(def.MaxTokens),
			Headers:          maps.Clone(def.Headers),
			Compat:           def.Compat,
		})
	}
	return models
}

func adaptProviderModelOverrides(
	defs map[string]extension.ProviderModelOverride,
) map[string]provider.ModelOverride {
	if defs == nil {
		return nil
	}
	overrides := make(map[string]provider.ModelOverride, len(defs))
	for id, def := range defs {
		overrides[id] = provider.ModelOverride{
			Name:             copyStringPtr(def.Name),
			Reasoning:        copyBoolPtr(def.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(def.ThinkingLevelMap),
			Input:            append([]ai.InputCapability(nil), def.Input...),
			Cost:             adaptProviderModelCost(def.Cost),
			ContextWindow:    copyIntPtr(def.ContextWindow),
			MaxTokens:        copyIntPtr(def.MaxTokens),
			Headers:          maps.Clone(def.Headers),
			Compat:           def.Compat,
		}
	}
	return overrides
}

func adaptProviderModelCost(cost *extension.ProviderModelCost) *provider.ModelCost {
	if cost == nil {
		return nil
	}
	return &provider.ModelCost{
		Input:      copyFloatPtr(cost.Input),
		Output:     copyFloatPtr(cost.Output),
		CacheRead:  copyFloatPtr(cost.CacheRead),
		CacheWrite: copyFloatPtr(cost.CacheWrite),
	}
}

func copyStringPointerMap(values map[string]*string) map[string]*string {
	if values == nil {
		return nil
	}
	copied := make(map[string]*string, len(values))
	for key, value := range values {
		copied[key] = copyStringPtr(value)
	}
	return copied
}

func copyStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyFloatPtr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func (o *Orchestrator) extensionContext(ctx context.Context) extension.ExtensionContext {
	if ctx == nil {
		ctx = context.Background()
	}

	return extension.ExtensionContext{
		Cwd:       o.extensionCwd(),
		SessionID: o.session.GetMetadata().ID,
		Session:   readonlySessionView{session: o.session},
		ModelRegistry: func() []extension.ModelInfo {
			return extensionModelInfos(o.AvailableModels(ctx))
		},
		Model:  o.currentModel,
		IsIdle: o.isIdle,
		IsAborted: func() bool {
			return ctx.Err() != nil
		},
		HasPendingMessages: o.hasPendingMessages,
		GetContextUsage:    o.contextUsage,
		GetSystemPrompt:    o.systemPromptText,
		Notify:             func(string, string) {},
		Abort: func(ctx context.Context) error {
			_, err := o.Abort(ctx)
			return err
		},
		Compact: func(ctx context.Context) error {
			_, err := o.Compact(ctx, CompactOptions{})
			return err
		},
		SetModel: o.SetModel,
		SetThinkingLevel: func(ctx context.Context, level extension.ThinkingLevel) error {
			return o.SetThinkingLevel(ctx, agentloop.ThinkingLevel(level))
		},
		Steer: func(ctx context.Context, input extension.UserInput) error {
			return o.Steer(ctx, UserInput{Text: input.Text})
		},
		FollowUp: func(ctx context.Context, input extension.UserInput) error {
			return o.FollowUp(ctx, UserInput{Text: input.Text})
		},
		WaitForIdle: o.WaitForIdle,
		Shutdown:    runExtensionShutdown,
		NewSession: func(ctx context.Context, parentSession string) error {
			_, err := o.NewSession(ctx, parentSession)
			return err
		},
		Fork: func(ctx context.Context, entryID string) (string, error) {
			text, _, err := o.Fork(ctx, entryID)
			return text, err
		},
		SwitchSession: func(ctx context.Context, sessionPath string) error {
			_, err := o.SwitchSession(ctx, sessionPath)
			return err
		},
		NavigateTree: func(ctx context.Context, entryID string) error {
			_, err := o.NavigateTree(ctx, session.EntryID(entryID), NavOptions{})
			return err
		},
		Reload: func() error {
			// Extension reload has no caller context in pi's API shape; start a
			// top-level reload operation here and keep context propagation inside
			// the orchestrator reload path.
			return o.ReloadSettings(context.Background())
		},
	}
}

func extensionModelInfos(models []ai.Model) []extension.ModelInfo {
	infos := make([]extension.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, extension.ModelInfo{
			ID:            model.ID,
			Provider:      model.Provider,
			DisplayName:   model.Name,
			ContextWindow: model.ContextWindow,
			Reasoning:     model.Reasoning,
		})
	}
	return infos
}

func runExtensionShutdown() {
	extensionShutdownMu.RLock()
	callback := extensionShutdownCallback
	extensionShutdownMu.RUnlock()
	callback()
}

func (o *Orchestrator) extensionCwd() string {
	o.mu.Lock()
	cwd := o.cwd
	execEnv := o.execEnv
	current := o.session
	o.mu.Unlock()

	if execEnv != nil {
		return execEnv.Cwd()
	}
	if cwd != "" {
		return cwd
	}
	if current != nil {
		return current.GetMetadata().CWD
	}
	return ""
}

func (o *Orchestrator) currentModel() ai.Model {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.model
}

func (o *Orchestrator) isIdle() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.phase == phaseIdle
}

func (o *Orchestrator) hasPendingMessages() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.steerQueue) > 0 ||
		len(o.followUpQueue) > 0 ||
		len(o.nextTurnQueue) > 0
}

func (o *Orchestrator) systemPromptText() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.systemPrompt
}

func (o *Orchestrator) contextUsage() *extension.ContextUsage {
	o.mu.Lock()
	model := o.model
	current := o.session
	o.mu.Unlock()

	contextWindow := model.ContextWindow
	if contextWindow <= 0 || current == nil {
		return nil
	}

	// Mirrors pi getContextUsage:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2922-2965.
	branch, err := current.GetBranch(nil)
	if err != nil {
		return nil
	}
	if compaction := latestCompactionEntry(branch); compaction != nil &&
		!hasValidPostCompactionUsage(branch, *compaction) {
		// pi can return { tokens: null, percent: null } here. along's
		// ContextUsage has no nullable fields, so unknown collapses to nil.
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionCtx, err := current.BuildContext(ctx)
	if err != nil {
		return nil
	}
	tokens := estimateContextTokens(sessionCtx.Messages)
	return &extension.ContextUsage{
		Tokens:        tokens,
		ContextWindow: contextWindow,
		Percent:       float64(tokens) / float64(contextWindow) * 100,
	}
}

func hasValidPostCompactionUsage(branch []session.SessionEntry, compaction session.SessionEntry) bool {
	compactionIndex := -1
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == "compaction" && branch[i].ID == compaction.ID {
			compactionIndex = i
			break
		}
	}
	if compactionIndex < 0 {
		return false
	}

	for i := len(branch) - 1; i > compactionIndex; i-- {
		entry := branch[i]
		if entry.Type != "message" {
			continue
		}
		msg, ok := message.AsAIMessage(entry.Message)
		if !ok || msg.Role != ai.RoleAssistant {
			continue
		}
		if msg.StopReason == ai.StopReasonAborted || msg.StopReason == ai.StopReasonError {
			continue
		}
		return calculateContextTokens(msg.Usage) > 0
	}
	return false
}

// calculateContextTokens mirrors pi's usage token calculation:
// .agents/references/pi/packages/coding-agent/src/core/compaction/compaction.ts:135-137.
func calculateContextTokens(usage *ai.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.TotalTokens != 0 {
		return usage.TotalTokens
	}
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

// estimateContextTokens mirrors pi's last-usage-plus-trailing estimate:
// .agents/references/pi/packages/coding-agent/src/core/compaction/compaction.ts:186-214.
func estimateContextTokens(messages []message.AgentMessage) int {
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

func assistantUsage(msg message.AgentMessage) *ai.Usage {
	aiMsg, ok := message.AsAIMessage(msg)
	if !ok || aiMsg.Role != ai.RoleAssistant {
		return nil
	}
	if aiMsg.StopReason == ai.StopReasonAborted || aiMsg.StopReason == ai.StopReasonError {
		return nil
	}
	return aiMsg.Usage
}

// estimateTokens mirrors pi's chars/4 message estimate:
// .agents/references/pi/packages/coding-agent/src/core/compaction/compaction.ts:232-290.
func estimateTokens(msg message.AgentMessage) int {
	switch value := msg.(type) {
	case ai.Message:
		return estimateAITokens(value)
	case *ai.Message:
		if value == nil {
			return 0
		}
		return estimateAITokens(*value)
	case message.CustomMessage:
		return estimateCustomContentTokens(value.Content)
	case *message.CustomMessage:
		if value == nil {
			return 0
		}
		return estimateCustomContentTokens(value.Content)
	case message.BashExecutionMessage:
		return charsToTokens(len(value.Command) + len(value.Output))
	case *message.BashExecutionMessage:
		if value == nil {
			return 0
		}
		return charsToTokens(len(value.Command) + len(value.Output))
	case message.BranchSummaryMessage:
		return charsToTokens(len(value.Summary))
	case *message.BranchSummaryMessage:
		if value == nil {
			return 0
		}
		return charsToTokens(len(value.Summary))
	case message.CompactionSummaryMessage:
		return charsToTokens(len(value.Summary))
	case *message.CompactionSummaryMessage:
		if value == nil {
			return 0
		}
		return charsToTokens(len(value.Summary))
	default:
		return 0
	}
}

func estimateAITokens(msg ai.Message) int {
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
	return charsToTokens(chars)
}

func estimateCustomContentTokens(content any) int {
	if text, ok := content.(string); ok {
		return charsToTokens(len(text))
	}
	blocks, err := message.CustomContentBlocks(content)
	if err != nil {
		return 0
	}
	return charsToTokens(toolResultContentChars(blocks))
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
			chars += 4800
		}
	}
	return chars
}

func jsonStringLength(value any) int {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 0
	}
	return len(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func charsToTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

type readonlySessionView struct {
	session harness.Session
}

func (v readonlySessionView) GetMessages() []any {
	if v.session == nil {
		return []any{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionCtx, err := v.session.BuildContext(ctx)
	if err != nil {
		return []any{}
	}
	out := make([]any, len(sessionCtx.Messages))
	for i, msg := range sessionCtx.Messages {
		out[i] = msg
	}
	return out
}

func (v readonlySessionView) GetEntries() []any {
	if v.session == nil {
		return []any{}
	}
	entries := v.session.GetEntries()
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	return out
}

func (v readonlySessionView) GetMetadata() any {
	if v.session == nil {
		return nil
	}
	return v.session.GetMetadata()
}
