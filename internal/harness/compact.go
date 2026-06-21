package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// Compact executes session compaction. It prepares the compaction, checks
// hooks for cancel/override, runs LLM summarization if needed, and writes
// the compaction entry to the session.
func (h *Harness) Compact(ctx context.Context, state TurnState, customInstructions string) (CompactionResult, error) {
	// 1. Get branch entries.
	branch, err := h.cfg.Session.GetBranch(nil)
	if err != nil {
		return CompactionResult{}, err
	}

	// 2. Prepare compaction parameters.
	prep, ok := prepareCompaction(branch, h.cfg.CompactionSettings)
	if !ok {
		return CompactionResult{}, errors.New("nothing to compact")
	}

	// 3. session_before_compact hook — check cancel/override.
	fromHook := false
	providedCompaction := false
	summary := ""

	if h.cfg.Hooks.HasHandlers(hook.EventSessionBeforeCompact) {
		branchEntries := make([]any, len(branch))
		for i, entry := range branch {
			branchEntries[i] = entry
		}
		result, err := h.cfg.Hooks.Emit(ctx, hook.SessionBeforeCompactEvent{
			Type:               hook.EventSessionBeforeCompact,
			Preparation:        prep.hookPreparation(),
			BranchEntries:      branchEntries,
			CustomInstructions: customInstructions,
		})
		if err != nil {
			return CompactionResult{}, err
		}
		if result != nil {
			r, ok := result.(hook.SessionBeforeCompactResult)
			if ok {
				if r.Cancel {
					return CompactionResult{}, errors.New("compaction cancelled")
				}
				if r.Compaction != nil {
					// Hook provided compaction result — use it.
					summary = r.Compaction.Summary
					prep.firstKeptEntryID = session.EntryID(r.Compaction.FirstKeptEntryID)
					prep.tokensBefore = r.Compaction.TokensBefore
					prep.details = r.Compaction.Details
					fromHook = true
					providedCompaction = true
				}
			}
		}
	}

	// 4. LLM summarize if hook didn't provide a result.
	if !providedCompaction {
		result, err := h.compactPrepared(ctx, prep, state, customInstructions)
		if err != nil {
			return CompactionResult{}, err
		}
		summary = result.Summary
		prep.firstKeptEntryID = result.FirstKeptEntryID
		prep.tokensBefore = result.TokensBefore
		prep.details = result.Details
	}

	// 5. Write compaction to session.
	entryID, err := h.cfg.Session.AppendCompaction(
		ctx,
		summary,
		prep.firstKeptEntryID,
		prep.tokensBefore,
		prep.details,
		fromHook,
	)
	if err != nil {
		return CompactionResult{}, err
	}

	// 6. session_compact notification hook.
	if h.cfg.Hooks.HasHandlers(hook.EventSessionCompact) {
		entry, _ := h.cfg.Session.GetEntry(entryID)
		_, err := h.cfg.Hooks.Emit(ctx, hook.SessionCompactEvent{
			Type:            hook.EventSessionCompact,
			CompactionEntry: entry,
			FromHook:        fromHook,
		})
		if err != nil {
			return CompactionResult{}, err
		}
	}

	// 7. Return result.
	return CompactionResult{
		Summary:          summary,
		FirstKeptEntryID: prep.firstKeptEntryID,
		TokensBefore:     prep.tokensBefore,
		Details:          prep.details,
		FromHook:         fromHook,
	}, nil
}

// compactionPrep holds calculated compaction parameters.
type compactionPrep struct {
	firstKeptEntryID    session.EntryID
	messagesToSummarize []message.AgentMessage
	turnPrefixMessages  []message.AgentMessage
	isSplitTurn         bool
	tokensBefore        int
	previousSummary     string
	fileOps             fileOperations
	details             any
	settings            hook.CompactionSettings
}

func (p compactionPrep) hookPreparation() hook.CompactionPreparation {
	return hook.CompactionPreparation{
		FirstKeptEntryID:    string(p.firstKeptEntryID),
		MessagesToSummarize: toAnySliceFromAgent(p.messagesToSummarize),
		TurnPrefixMessages:  toAnySliceFromAgent(p.turnPrefixMessages),
		IsSplitTurn:         p.isSplitTurn,
		TokensBefore:        p.tokensBefore,
		PreviousSummary:     p.previousSummary,
		FileOps:             p.fileOps.details(),
		Settings:            p.settings,
	}
}

// prepareCompaction calculates compaction parameters from branch entries.
// It identifies the first entry to keep after compaction and estimates
// the token count before compaction.
func prepareCompaction(
	branch []session.SessionEntry,
	settings hook.CompactionSettings,
) (compactionPrep, bool) {
	if len(branch) == 0 || branch[len(branch)-1].Type == "compaction" {
		return compactionPrep{}, false
	}

	settings = compactionSettingsWithDefaults(settings)

	prevCompactionIndex := -1
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == "compaction" {
			prevCompactionIndex = i
			break
		}
	}

	previousSummary := ""
	boundaryStart := 0
	if prevCompactionIndex >= 0 {
		prevCompaction := branch[prevCompactionIndex]
		previousSummary = prevCompaction.Summary
		firstKeptEntryIndex := -1
		for i, entry := range branch {
			if entry.ID == prevCompaction.FirstKeptEntryID {
				firstKeptEntryIndex = i
				break
			}
		}
		if firstKeptEntryIndex >= 0 {
			boundaryStart = firstKeptEntryIndex
		} else {
			boundaryStart = prevCompactionIndex + 1
		}
	}

	cutPoint := findCutPoint(branch, boundaryStart, len(branch), settings.KeepRecentTokens)
	if cutPoint.firstKeptEntryIndex < 0 || cutPoint.firstKeptEntryIndex >= len(branch) {
		return compactionPrep{}, false
	}

	firstKeptEntry := branch[cutPoint.firstKeptEntryIndex]
	if firstKeptEntry.ID == "" {
		return compactionPrep{}, false
	}

	historyEnd := cutPoint.firstKeptEntryIndex
	if cutPoint.isSplitTurn {
		historyEnd = cutPoint.turnStartIndex
	}

	prep := compactionPrep{
		firstKeptEntryID: firstKeptEntry.ID,
		isSplitTurn:      cutPoint.isSplitTurn,
		tokensBefore:     estimateContextTokens(branch),
		previousSummary:  previousSummary,
		settings:         settings,
	}
	for i := boundaryStart; i < historyEnd; i++ {
		if msg, ok := messageFromEntryForCompaction(branch[i]); ok {
			prep.messagesToSummarize = append(prep.messagesToSummarize, msg)
		}
	}
	if cutPoint.isSplitTurn {
		for i := cutPoint.turnStartIndex; i < cutPoint.firstKeptEntryIndex; i++ {
			if msg, ok := messageFromEntryForCompaction(branch[i]); ok {
				prep.turnPrefixMessages = append(prep.turnPrefixMessages, msg)
			}
		}
	}
	prep.fileOps = extractFileOperations(prep.messagesToSummarize, branch, prevCompactionIndex)
	if cutPoint.isSplitTurn {
		for _, msg := range prep.turnPrefixMessages {
			extractFileOpsFromMessage(msg, &prep.fileOps)
		}
	}
	return prep, true
}

func compactionSettingsWithDefaults(settings hook.CompactionSettings) hook.CompactionSettings {
	if settings != (hook.CompactionSettings{}) {
		return settings
	}
	return hook.CompactionSettings{
		Enabled:          true,
		ReserveTokens:    16384,
		KeepRecentTokens: 20000,
	}
}

const summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI coding assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

const updateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

const turnPrefixSummarizationPrompt = `This is the PREFIX of a turn that was too large to keep. The SUFFIX (recent work) is retained.

Summarize the prefix to provide context for the retained suffix:

## Original Request
[What did the user ask for in this turn?]

## Early Progress
- [Key decisions and work done in the prefix]

## Context for Suffix
- [Information needed to understand the retained recent work]

Be concise. Focus on what's needed to understand the kept suffix.`

func (h *Harness) compactPrepared(
	ctx context.Context,
	prep compactionPrep,
	state TurnState,
	customInstructions string,
) (CompactionResult, error) {
	if prep.firstKeptEntryID == "" {
		return CompactionResult{}, errors.New("first kept entry has no UUID")
	}

	auth, err := h.providerAuth(ctx, state.Model)
	if err != nil {
		return CompactionResult{}, err
	}

	var summary string
	if prep.isSplitTurn && len(prep.turnPrefixMessages) > 0 {
		historySummary := "No prior history."
		if len(prep.messagesToSummarize) > 0 {
			historySummary, err = h.generateSummary(
				ctx,
				prep.messagesToSummarize,
				state.Model,
				prep.settings.ReserveTokens,
				auth,
				customInstructions,
				prep.previousSummary,
				state.ThinkingLevel,
			)
			if err != nil {
				return CompactionResult{}, err
			}
		}
		prefixSummary, err := h.generateTurnPrefixSummary(
			ctx,
			prep.turnPrefixMessages,
			state.Model,
			prep.settings.ReserveTokens,
			auth,
			state.ThinkingLevel,
		)
		if err != nil {
			return CompactionResult{}, err
		}
		summary = historySummary + "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefixSummary
	} else {
		summary, err = h.generateSummary(
			ctx,
			prep.messagesToSummarize,
			state.Model,
			prep.settings.ReserveTokens,
			auth,
			customInstructions,
			prep.previousSummary,
			state.ThinkingLevel,
		)
		if err != nil {
			return CompactionResult{}, err
		}
	}

	readFiles, modifiedFiles := computeFileLists(prep.fileOps)
	summary += formatFileOperations(readFiles, modifiedFiles)

	return CompactionResult{
		Summary:          summary,
		FirstKeptEntryID: prep.firstKeptEntryID,
		TokensBefore:     prep.tokensBefore,
		Details: map[string]any{
			"readFiles":     readFiles,
			"modifiedFiles": modifiedFiles,
		},
	}, nil
}

func (h *Harness) generateSummary(
	ctx context.Context,
	currentMessages []message.AgentMessage,
	model ai.Model,
	reserveTokens int,
	auth ProviderAuth,
	customInstructions string,
	previousSummary string,
	thinkingLevel agentloop.ThinkingLevel,
) (string, error) {
	basePrompt := summarizationPrompt
	if previousSummary != "" {
		basePrompt = updateSummarizationPrompt
	}
	if customInstructions != "" {
		basePrompt += "\n\nAdditional focus: " + customInstructions
	}

	conversationText, err := serializeAgentConversation(currentMessages)
	if err != nil {
		return "", err
	}
	promptText := "<conversation>\n" + conversationText + "\n</conversation>\n\n"
	if previousSummary != "" {
		promptText += "<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n"
	}
	promptText += basePrompt

	return h.completeSummary(ctx, model, auth, promptText, maxSummaryTokens(reserveTokens, 0.8, model), summaryReasoning(model, thinkingLevel))
}

func (h *Harness) generateTurnPrefixSummary(
	ctx context.Context,
	messages []message.AgentMessage,
	model ai.Model,
	reserveTokens int,
	auth ProviderAuth,
	thinkingLevel agentloop.ThinkingLevel,
) (string, error) {
	conversationText, err := serializeAgentConversation(messages)
	if err != nil {
		return "", err
	}
	promptText := "<conversation>\n" +
		conversationText +
		"\n</conversation>\n\n" +
		turnPrefixSummarizationPrompt

	return h.completeSummary(ctx, model, auth, promptText, maxSummaryTokens(reserveTokens, 0.5, model), summaryReasoning(model, thinkingLevel))
}

// summaryReasoning mirrors pi's compaction option: summarization requests
// carry the session thinking level when the model supports reasoning
// (compaction.ts:490-493,733-736).
func summaryReasoning(model ai.Model, thinkingLevel agentloop.ThinkingLevel) string {
	if model.Reasoning && thinkingLevel != "" && thinkingLevel != agentloop.ThinkingOff {
		return string(thinkingLevel)
	}
	return ""
}

func (h *Harness) completeSummary(
	ctx context.Context,
	model ai.Model,
	auth ProviderAuth,
	promptText string,
	maxTokens int,
	reasoning string,
) (string, error) {
	stream := h.cfg.stream
	if stream == nil {
		stream = ai.StreamSimple
	}
	responseStream := stream(
		ctx,
		model,
		ai.Context{
			SystemPrompt: summarizationSystemPrompt,
			Messages: []ai.Message{{
				Role:      ai.RoleUser,
				Content:   []ai.ContentBlock{{Type: ai.ContentText, Text: promptText}},
				Timestamp: time.Now().UnixMilli(),
			}},
		},
		ai.SimpleStreamOptions{
			MaxTokens: maxTokens,
			APIKey:    auth.APIKey,
			Headers:   auth.Headers,
			Reasoning: reasoning,
		},
	)
	for _, err := range responseStream.Iter() {
		if err != nil {
			break
		}
	}
	response, err := responseStream.Result()
	if err != nil {
		return "", fmt.Errorf("summarization failed: %w", err)
	}
	switch response.StopReason {
	case ai.StopReasonAborted:
		if response.ErrorMessage != "" {
			return "", fmt.Errorf("%w: %s", errSummaryAborted, response.ErrorMessage)
		}
		return "", errSummaryAborted
	case ai.StopReasonError:
		if response.ErrorMessage != "" {
			return "", fmt.Errorf("summarization failed: %s", response.ErrorMessage)
		}
		return "", errors.New("summarization failed")
	}
	return textBlocks(response.Content), nil
}

func maxSummaryTokens(reserveTokens int, ratio float64, model ai.Model) int {
	maxTokens := int(float64(reserveTokens) * ratio)
	if model.MaxTokens > 0 && model.MaxTokens < maxTokens {
		return model.MaxTokens
	}
	return maxTokens
}

func serializeAgentConversation(messages []message.AgentMessage) (string, error) {
	llmMessages, err := message.DefaultConvertToLLM(messages)
	if err != nil {
		return "", err
	}
	return serializeConversation(llmMessages), nil
}

const toolResultMaxChars = 2000

var errSummaryAborted = errors.New("summarization aborted")

func serializeConversation(messages []ai.Message) string {
	parts := []string{}
	for _, msg := range messages {
		switch msg.Role {
		case ai.RoleUser:
			content := textBlocks(msg.Content)
			if content != "" {
				parts = append(parts, "[User]: "+content)
			}
		case ai.RoleAssistant:
			thinkingParts := []string{}
			textParts := []string{}
			toolCalls := []string{}
			for _, block := range msg.Content {
				switch block.Type {
				case ai.ContentThinking:
					thinkingParts = append(thinkingParts, block.Thinking)
				case ai.ContentText:
					textParts = append(textParts, block.Text)
				case ai.ContentToolCall:
					toolCalls = append(toolCalls, formatToolCall(block))
				}
			}
			if len(thinkingParts) > 0 {
				parts = append(parts, "[Assistant thinking]: "+strings.Join(thinkingParts, "\n"))
			}
			if len(textParts) > 0 {
				parts = append(parts, "[Assistant]: "+strings.Join(textParts, "\n"))
			}
			if len(toolCalls) > 0 {
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(toolCalls, "; "))
			}
		case ai.RoleToolResult:
			content := textBlocks(msg.Content)
			if content != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(content, toolResultMaxChars))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func textBlocks(blocks []ai.ContentBlock) string {
	parts := []string{}
	for _, block := range blocks {
		if block.Type == ai.ContentText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "")
}

func formatToolCall(block ai.ContentBlock) string {
	keys := make([]string, 0, len(block.Arguments))
	for key := range block.Arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	args := make([]string, 0, len(keys))
	for _, key := range keys {
		args = append(args, key+"="+safeJSONString(block.Arguments[key]))
	}
	return block.ToolName + "(" + strings.Join(args, ", ") + ")"
}

func safeJSONString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "[unserializable]"
	}
	if data == nil {
		return "undefined"
	}
	return string(data)
}

func truncateForSummary(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	truncatedChars := len(text) - maxChars
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]", text[:maxChars], truncatedChars)
}

type fileOperations struct {
	read    map[string]struct{}
	written map[string]struct{}
	edited  map[string]struct{}
}

func newFileOperations() fileOperations {
	return fileOperations{
		read:    map[string]struct{}{},
		written: map[string]struct{}{},
		edited:  map[string]struct{}{},
	}
}

func extractFileOperations(
	messages []message.AgentMessage,
	entries []session.SessionEntry,
	prevCompactionIndex int,
) fileOperations {
	fileOps := newFileOperations()
	if prevCompactionIndex >= 0 {
		prevCompaction := entries[prevCompactionIndex]
		if !prevCompaction.FromHook && prevCompaction.Details != nil {
			fileOps.addDetails(prevCompaction.Details)
		}
	}
	for _, msg := range messages {
		extractFileOpsFromMessage(msg, &fileOps)
	}
	return fileOps
}

func extractFileOpsFromMessage(msg message.AgentMessage, fileOps *fileOperations) {
	aiMsg, ok := message.AsAIMessage(msg)
	if !ok || aiMsg.Role != ai.RoleAssistant {
		return
	}
	fileOps.ensure()
	for _, block := range aiMsg.Content {
		if block.Type != ai.ContentToolCall {
			continue
		}
		path, ok := block.Arguments["path"].(string)
		if !ok || path == "" {
			continue
		}
		switch block.ToolName {
		case "read":
			fileOps.read[path] = struct{}{}
		case "write":
			fileOps.written[path] = struct{}{}
		case "edit":
			fileOps.edited[path] = struct{}{}
		}
	}
}

func (ops *fileOperations) ensure() {
	if ops.read == nil {
		ops.read = map[string]struct{}{}
	}
	if ops.written == nil {
		ops.written = map[string]struct{}{}
	}
	if ops.edited == nil {
		ops.edited = map[string]struct{}{}
	}
}

func (ops *fileOperations) addDetails(details any) {
	ops.ensure()
	for _, file := range filesFromDetails(details, "readFiles") {
		ops.read[file] = struct{}{}
	}
	for _, file := range filesFromDetails(details, "modifiedFiles") {
		ops.edited[file] = struct{}{}
	}
}

func (ops fileOperations) details() map[string]any {
	readFiles, modifiedFiles := computeFileLists(ops)
	return map[string]any{
		"readFiles":     readFiles,
		"modifiedFiles": modifiedFiles,
	}
}

func filesFromDetails(details any, key string) []string {
	switch value := details.(type) {
	case map[string]any:
		return stringList(value[key])
	case map[string][]string:
		return append([]string(nil), value[key]...)
	default:
		return nil
	}
}

func stringList(value any) []string {
	switch list := value.(type) {
	case []string:
		return append([]string(nil), list...)
	case []any:
		result := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func computeFileLists(fileOps fileOperations) ([]string, []string) {
	modified := map[string]struct{}{}
	for file := range fileOps.edited {
		modified[file] = struct{}{}
	}
	for file := range fileOps.written {
		modified[file] = struct{}{}
	}

	readFiles := []string{}
	for file := range fileOps.read {
		if _, ok := modified[file]; !ok {
			readFiles = append(readFiles, file)
		}
	}
	modifiedFiles := make([]string, 0, len(modified))
	for file := range modified {
		modifiedFiles = append(modifiedFiles, file)
	}
	sort.Strings(readFiles)
	sort.Strings(modifiedFiles)
	return readFiles, modifiedFiles
}

func formatFileOperations(readFiles []string, modifiedFiles []string) string {
	sections := []string{}
	if len(readFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(readFiles, "\n")+"\n</read-files>")
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(modifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}

type cutPointResult struct {
	firstKeptEntryIndex int
	turnStartIndex      int
	isSplitTurn         bool
}

func findCutPoint(
	entries []session.SessionEntry,
	startIndex int,
	endIndex int,
	keepRecentTokens int,
) cutPointResult {
	cutPoints := findValidCutPoints(entries, startIndex, endIndex)
	if len(cutPoints) == 0 {
		return cutPointResult{
			firstKeptEntryIndex: startIndex,
			turnStartIndex:      -1,
			isSplitTurn:         false,
		}
	}

	accumulatedTokens := 0
	cutIndex := cutPoints[0]
	for i := endIndex - 1; i >= startIndex; i-- {
		entry := entries[i]
		if entry.Type != "message" {
			continue
		}
		msg, ok := message.AsAIMessage(entry.Message)
		if !ok {
			continue
		}
		accumulatedTokens += estimateAgentMessageTokens(msg)
		if accumulatedTokens < keepRecentTokens {
			continue
		}
		for _, candidate := range cutPoints {
			if candidate >= i {
				cutIndex = candidate
				break
			}
		}
		break
	}

	for cutIndex > startIndex {
		prevEntry := entries[cutIndex-1]
		if prevEntry.Type == "compaction" || prevEntry.Type == "message" {
			break
		}
		cutIndex--
	}

	cutEntry := entries[cutIndex]
	isUserMessage := false
	if cutEntry.Type == "message" {
		isUserMessage = isUserMessageEntry(cutEntry.Message)
	}
	turnStartIndex := -1
	if !isUserMessage {
		turnStartIndex = findTurnStartIndex(entries, cutIndex, startIndex)
	}

	return cutPointResult{
		firstKeptEntryIndex: cutIndex,
		turnStartIndex:      turnStartIndex,
		isSplitTurn:         !isUserMessage && turnStartIndex != -1,
	}
}

func findValidCutPoints(entries []session.SessionEntry, startIndex int, endIndex int) []int {
	cutPoints := []int{}
	for i := startIndex; i < endIndex; i++ {
		entry := entries[i]
		switch entry.Type {
		case "message":
			if isValidMessageCutPoint(entry.Message) {
				cutPoints = append(cutPoints, i)
			}
		case "branch_summary", "custom_message":
			cutPoints = append(cutPoints, i)
		}
	}
	return cutPoints
}

func findTurnStartIndex(entries []session.SessionEntry, entryIndex int, startIndex int) int {
	for i := entryIndex; i >= startIndex; i-- {
		entry := entries[i]
		if entry.Type == "branch_summary" || entry.Type == "custom_message" {
			return i
		}
		if entry.Type != "message" {
			continue
		}
		if isTurnStartMessage(entry.Message) {
			return i
		}
	}
	return -1
}

func isValidMessageCutPoint(msg message.AgentMessage) bool {
	if aiMsg, ok := message.AsAIMessage(msg); ok {
		return aiMsg.Role != ai.RoleToolResult
	}
	switch msg.(type) {
	case message.BashExecutionMessage,
		*message.BashExecutionMessage,
		message.CustomMessage,
		*message.CustomMessage,
		message.BranchSummaryMessage,
		*message.BranchSummaryMessage,
		message.CompactionSummaryMessage,
		*message.CompactionSummaryMessage:
		return true
	default:
		return false
	}
}

func isUserMessageEntry(msg message.AgentMessage) bool {
	aiMsg, ok := message.AsAIMessage(msg)
	return ok && aiMsg.Role == ai.RoleUser
}

func isTurnStartMessage(msg message.AgentMessage) bool {
	if isUserMessageEntry(msg) {
		return true
	}
	switch msg.(type) {
	case message.BashExecutionMessage, *message.BashExecutionMessage:
		return true
	default:
		return false
	}
}

func estimateContextTokens(entries []session.SessionEntry) int {
	return estimateAgentMessagesContextTokens(contextMessagesForTokenEstimate(entries))
}

func contextMessagesForTokenEstimate(entries []session.SessionEntry) []message.AgentMessage {
	compactionIndex := -1
	for i, entry := range entries {
		if entry.Type == "compaction" {
			compactionIndex = i
		}
	}

	if compactionIndex < 0 {
		messages := make([]message.AgentMessage, 0, len(entries))
		for _, entry := range entries {
			if msg, ok := messageFromEntryForCompaction(entry); ok {
				messages = append(messages, msg)
			}
		}
		return messages
	}

	compaction := entries[compactionIndex]
	messages := []message.AgentMessage{
		message.CreateCompactionSummaryMessage(
			compaction.Summary,
			compaction.TokensBefore,
			compaction.Timestamp,
		),
	}
	foundFirstKept := false
	for i := 0; i < compactionIndex; i++ {
		entry := entries[i]
		if entry.ID == compaction.FirstKeptEntryID {
			foundFirstKept = true
		}
		if !foundFirstKept {
			continue
		}
		if msg, ok := messageFromEntryForCompaction(entry); ok {
			messages = append(messages, msg)
		}
	}
	for i := compactionIndex + 1; i < len(entries); i++ {
		if msg, ok := messageFromEntryForCompaction(entries[i]); ok {
			messages = append(messages, msg)
		}
	}
	return messages
}

func estimateAgentMessagesContextTokens(messages []message.AgentMessage) int {
	lastUsageIndex := -1
	usageTokens := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if tokens, ok := assistantUsageTokens(messages[i]); ok {
			lastUsageIndex = i
			usageTokens = tokens
			break
		}
	}
	if lastUsageIndex < 0 {
		total := 0
		for _, msg := range messages {
			total += estimateAgentMessageTokens(msg)
		}
		return total
	}

	trailingTokens := 0
	for i := lastUsageIndex + 1; i < len(messages); i++ {
		trailingTokens += estimateAgentMessageTokens(messages[i])
	}
	return usageTokens + trailingTokens
}

func assistantUsageTokens(msg message.AgentMessage) (int, bool) {
	aiMsg, ok := message.AsAIMessage(msg)
	if !ok || aiMsg.Role != ai.RoleAssistant || aiMsg.Usage == nil {
		return 0, false
	}
	if aiMsg.StopReason == ai.StopReasonAborted || aiMsg.StopReason == ai.StopReasonError {
		return 0, false
	}
	if aiMsg.Usage.TotalTokens > 0 {
		return aiMsg.Usage.TotalTokens, true
	}
	total := aiMsg.Usage.Input + aiMsg.Usage.Output + aiMsg.Usage.CacheRead + aiMsg.Usage.CacheWrite
	if total > 0 {
		return total, true
	}
	return 0, false
}

func estimateAgentMessageTokens(msg message.AgentMessage) int {
	switch value := msg.(type) {
	case ai.Message:
		return estimateAIMessageTokens(value)
	case *ai.Message:
		if value == nil {
			return 0
		}
		return estimateAIMessageTokens(*value)
	case message.BashExecutionMessage:
		return estimateStringTokens(value.Command + value.Output)
	case *message.BashExecutionMessage:
		if value == nil {
			return 0
		}
		return estimateStringTokens(value.Command + value.Output)
	case message.CustomMessage:
		return estimateContentTokens(value.Content)
	case *message.CustomMessage:
		if value == nil {
			return 0
		}
		return estimateContentTokens(value.Content)
	case message.BranchSummaryMessage:
		return estimateStringTokens(value.Summary)
	case *message.BranchSummaryMessage:
		if value == nil {
			return 0
		}
		return estimateStringTokens(value.Summary)
	case message.CompactionSummaryMessage:
		return estimateStringTokens(value.Summary)
	case *message.CompactionSummaryMessage:
		if value == nil {
			return 0
		}
		return estimateStringTokens(value.Summary)
	default:
		return 0
	}
}

// estimateAIMessageTokens mirrors pi's per-message character heuristic. It
// never reads provider usage — Usage.TotalTokens is the size of the whole
// request context at that point, not the message's own size, and using it in
// findCutPoint guts the kept history (pi compaction.ts:202-260).
func estimateAIMessageTokens(msg ai.Message) int {
	chars := 0
	switch msg.Role {
	case ai.RoleAssistant:
		for _, block := range msg.Content {
			switch block.Type {
			case ai.ContentText:
				chars += len(block.Text)
			case ai.ContentThinking:
				chars += len(block.Thinking)
			case ai.ContentToolCall:
				chars += len(block.ToolName)
				if data, err := json.Marshal(block.Arguments); err == nil {
					chars += len(data)
				}
			}
		}
	case ai.RoleUser:
		// pi counts only text blocks for user messages (compaction.ts:206-218).
		for _, block := range msg.Content {
			if block.Type == ai.ContentText {
				chars += len(block.Text)
			}
		}
	default: // toolResult and custom-message content (compaction.ts:232-248)
		for _, block := range msg.Content {
			switch block.Type {
			case ai.ContentText:
				chars += len(block.Text)
			case ai.ContentImage:
				chars += 4800
			}
		}
	}
	return charsToTokens(chars)
}

func estimateContentTokens(content any) int {
	switch value := content.(type) {
	case string:
		return estimateStringTokens(value)
	case []ai.ContentBlock:
		return estimateAIMessageTokens(ai.Message{Content: value})
	case []any:
		blocks, err := message.CustomContentBlocks(value)
		if err != nil {
			return 0
		}
		return estimateAIMessageTokens(ai.Message{Content: blocks})
	default:
		return 0
	}
}

func messageFromEntryForCompaction(entry session.SessionEntry) (message.AgentMessage, bool) {
	switch entry.Type {
	case "message":
		if entry.Message == nil {
			return nil, false
		}
		return message.Snapshot(entry.Message), true
	case "custom_message":
		return message.CreateCustomMessage(
			entry.CustomType,
			entry.Content,
			entry.Display,
			entry.Details,
			entry.Timestamp,
		), true
	case "branch_summary":
		fromID := "root"
		if entry.FromID != nil {
			fromID = string(*entry.FromID)
		}
		return message.CreateBranchSummaryMessage(entry.Summary, fromID, entry.Timestamp), true
	case "compaction":
		return nil, false
	default:
		return nil, false
	}
}

func estimateStringTokens(text string) int {
	return charsToTokens(len(text))
}

func charsToTokens(chars int) int {
	return (chars + 3) / 4
}
