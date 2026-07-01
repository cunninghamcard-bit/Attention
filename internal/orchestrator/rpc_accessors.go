package orchestrator

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// ModelCycleResult is the orchestrator result for rpc cycle_model.
type ModelCycleResult struct {
	Model         ai.Model
	ThinkingLevel agentloop.ThinkingLevel
}

var thinkingLevelsInOrder = []agentloop.ThinkingLevel{
	agentloop.ThinkingOff,
	agentloop.ThinkingMinimal,
	agentloop.ThinkingLow,
	agentloop.ThinkingMedium,
	agentloop.ThinkingHigh,
	agentloop.ThinkingXHigh,
}

// builtinSlashCommands lists the builtin slash commands that map to a real
// kernel operation reachable over RPC. Pure UI/TUI commands from pi's
// BUILTIN_SLASH_COMMANDS (settings, scoped-models, export, import, share, copy,
// changelog, hotkeys, login, logout, quit) are pruned — they have no kernel op.
// .agents/references/pi/packages/coding-agent/src/core/slash-commands.ts:18-40.
var builtinSlashCommands = []SlashCommand{
	{Name: "model", Description: "Select model (opens selector UI)", Source: "builtin"},
	{Name: "name", Description: "Set session display name", Source: "builtin"},
	{Name: "session", Description: "Show session info and stats", Source: "builtin"},
	{Name: "fork", Description: "Create a new fork from a previous user message", Source: "builtin"},
	{Name: "clone", Description: "Duplicate the current session at the current position", Source: "builtin"},
	{Name: "tree", Description: "Navigate session tree (switch branches)", Source: "builtin"},
	{Name: "new", Description: "Start a new session", Source: "builtin"},
	{Name: "compact", Description: "Manually compact the session context", Source: "builtin"},
	{Name: "resume", Description: "Resume a different session", Source: "builtin"},
	{Name: "reload", Description: "Reload keybindings, extensions, skills, prompts, and themes", Source: "builtin"},
}

// CycleModel mirrors pi cycle_model -> _cycleAvailableModel:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:473-479
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1476-1498.
// along has no scoped models or settingsManager.
func (o *Orchestrator) CycleModel(ctx context.Context) (ModelCycleResult, bool, error) {
	availableModels := o.AvailableModels(ctx)
	if len(availableModels) <= 1 {
		return ModelCycleResult{}, false, nil
	}

	o.mu.Lock()
	currentModel := o.model
	currentLevel := o.thinkingLevel
	o.mu.Unlock()

	currentIndex := 0
	for i, model := range availableModels {
		if modelsAreEqual(model, currentModel) {
			currentIndex = i
			break
		}
	}

	nextModel := availableModels[(currentIndex+1)%len(availableModels)]
	nextLevel := clampThinkingLevel(nextModel, currentLevel)
	if err := o.setModel(ctx, nextModel, modelSelectSourceCycle); err != nil {
		return ModelCycleResult{}, false, err
	}
	// pi re-clamps through setThinkingLevel after changing model
	// (.agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1493-1494).
	if err := o.SetThinkingLevel(ctx, nextLevel); err != nil {
		return ModelCycleResult{}, false, err
	}

	return ModelCycleResult{Model: nextModel, ThinkingLevel: nextLevel}, true, nil
}

// CycleThinkingLevel mirrors pi cycleThinkingLevel:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:495-501
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1538-1547.
func (o *Orchestrator) CycleThinkingLevel(ctx context.Context) (agentloop.ThinkingLevel, bool, error) {
	o.mu.Lock()
	currentModel := o.model
	currentLevel := o.thinkingLevel
	o.mu.Unlock()

	if !currentModel.Reasoning {
		return "", false, nil
	}
	levels := supportedThinkingLevels(currentModel)
	if len(levels) == 0 {
		return "", false, nil
	}

	currentIndex := -1
	for i, level := range levels {
		if level == currentLevel {
			currentIndex = i
			break
		}
	}
	nextLevel := levels[(currentIndex+1)%len(levels)]
	if err := o.SetThinkingLevel(ctx, nextLevel); err != nil {
		return "", false, err
	}
	return nextLevel, true, nil
}

// SetSessionName mirrors pi setSessionName:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:611-618
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2637-2639.
func (o *Orchestrator) SetSessionName(ctx context.Context, name string) error {
	if _, err := o.session.AppendSessionName(ctx, name); err != nil {
		return err
	}
	// pi emits session_info_changed after setting the session name:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2639.
	o.publish(Event{Type: EventSessionInfoChanged, Name: name})
	return nil
}

// LastAssistantText mirrors pi getLastAssistantText:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:606-609
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:3038-3059.
// SessionMetadata exposes the active session's header metadata; pi's json
// print mode emits it as the first output line (print-mode.ts:111-116).
func (o *Orchestrator) SessionMetadata() session.Metadata {
	return o.session.GetMetadata()
}

func (o *Orchestrator) LastAssistantText() (string, bool) {
	messages := o.Messages()
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != ai.RoleAssistant {
			continue
		}
		if msg.StopReason == ai.StopReasonAborted && len(msg.Content) == 0 {
			continue
		}

		var text strings.Builder
		for _, block := range msg.Content {
			if block.Type == ai.ContentText {
				text.WriteString(block.Text)
			}
		}
		trimmed := strings.TrimSpace(text.String())
		if trimmed == "" {
			return "", false
		}
		return trimmed, true
	}
	return "", false
}

// SlashCommands mirrors pi get_commands aggregation order:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:632-663.
func (o *Orchestrator) SlashCommands() []SlashCommand {
	o.mu.Lock()
	enableSkillCommands := enableSkillCommandsFrom(o.settings)
	extensionCommands := make(map[string]extension.CommandDefinition, len(o.commands))
	maps.Copy(extensionCommands, o.commands)
	promptTemplates := append([]resource.PromptTemplate(nil), o.promptTemplates...)
	skills := append([]resource.Skill(nil), o.skills...)
	o.mu.Unlock()

	skillCommandCount := 0
	if enableSkillCommands {
		skillCommandCount = len(skills)
	}
	commands := make(
		[]SlashCommand,
		0,
		len(builtinSlashCommands)+len(extensionCommands)+len(promptTemplates)+skillCommandCount,
	)
	commands = append(commands, builtinSlashCommands...)

	extensionNames := make([]string, 0, len(extensionCommands))
	for name := range extensionCommands {
		extensionNames = append(extensionNames, name)
	}
	sort.Strings(extensionNames)
	for _, name := range extensionNames {
		def := extensionCommands[name]
		commands = append(commands, SlashCommand{
			Name:         name,
			Description:  def.Description,
			ArgumentHint: def.ArgumentHint,
			Source:       "extension",
			// pi tags extension commands with sourceInfo:
			// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:635-641.
			SourceInfo: def.Source,
		})
	}

	for _, template := range promptTemplates {
		commands = append(commands, SlashCommand{
			Name:         template.Name,
			Description:  template.Description,
			ArgumentHint: template.ArgumentHint,
			Source:       "prompt",
			SourceInfo:   template.Source,
		})
	}

	if enableSkillCommands {
		for _, skill := range skills {
			commands = append(commands, SlashCommand{
				Name:        "skill:" + skill.Name,
				Description: skill.Description,
				Source:      "skill",
				SourceInfo:  skill.Source,
			})
		}
	}

	return commands
}

// ResourceDiagnostics returns resource diagnostics captured during assembly and
// subsequent resource reloads. Modes may surface these, while cmd/along logs
// startup diagnostics directly.
func (o *Orchestrator) ResourceDiagnostics() []resource.ResourceDiagnostic {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]resource.ResourceDiagnostic(nil), o.diagnostics...)
}

func modelsAreEqual(a, b ai.Model) bool {
	return a.Provider == b.Provider && a.ID == b.ID
}

func supportedThinkingLevels(model ai.Model) []agentloop.ThinkingLevel {
	if !model.Reasoning {
		return []agentloop.ThinkingLevel{agentloop.ThinkingOff}
	}
	if len(model.ThinkingLevelMap) == 0 {
		return append([]agentloop.ThinkingLevel(nil), thinkingLevelsInOrder...)
	}

	levels := []agentloop.ThinkingLevel{}
	for _, level := range thinkingLevelsInOrder {
		mapped, ok := model.ThinkingLevelMap[string(level)]
		if ok && mapped != nil {
			levels = append(levels, level)
		}
	}
	return levels
}

func clampThinkingLevel(model ai.Model, level agentloop.ThinkingLevel) agentloop.ThinkingLevel {
	levels := supportedThinkingLevels(model)
	if containsThinkingLevel(levels, level) {
		return level
	}

	requestedIndex := thinkingLevelIndex(level)
	if requestedIndex == -1 {
		if len(levels) > 0 {
			return levels[0]
		}
		return agentloop.ThinkingOff
	}

	// pi scans forward first, then backward, then falls back to off
	// (.agents/references/pi/packages/ai/src/models.ts:61-79).
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

func containsThinkingLevel(levels []agentloop.ThinkingLevel, target agentloop.ThinkingLevel) bool {
	return slices.Contains(levels, target)
}

func thinkingLevelIndex(target agentloop.ThinkingLevel) int {
	for i, level := range thinkingLevelsInOrder {
		if level == target {
			return i
		}
	}
	return -1
}
