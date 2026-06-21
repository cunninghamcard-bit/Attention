package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

const overflowCompactionReason = "overflow"

var errContextOverflowRecovery = errors.New("context overflow recovery failed")

type assistantEntryResult struct {
	entry   session.SessionEntry
	message ai.Message
	branch  []session.SessionEntry
	ok      bool
}

func (o *Orchestrator) recoverOverflowBeforePrompt(
	ctx context.Context,
	state harness.TurnState,
) (ai.Message, error) {
	last, err := lastAssistantEntry(o.session)
	if err != nil || !last.ok {
		return ai.Message{}, err
	}
	msg, _, err := o.recoverContextOverflow(ctx, state, last)
	return msg, err
}

func (o *Orchestrator) recoverOverflowAfterAssistant(
	ctx context.Context,
	state harness.TurnState,
	msg ai.Message,
) (ai.Message, error) {
	entry, err := assistantEntryForMessage(o.session, msg)
	if err != nil || !entry.ok {
		return msg, err
	}
	recovered, _, err := o.recoverContextOverflow(ctx, state, entry)
	return recovered, err
}

func (o *Orchestrator) recoverContextOverflow(
	ctx context.Context,
	state harness.TurnState,
	assistant assistantEntryResult,
) (ai.Message, bool, error) {
	msg := assistant.message
	if !ai.IsContextOverflow(msg, state.Model.ContextWindow) {
		if msg.StopReason != ai.StopReasonError {
			o.setOverflowRecoveryAttempted(false)
		}
		return msg, false, nil
	}

	if !o.isAutoCompactionEnabled() {
		return msg, false, nil
	}
	if !sameAssistantModel(msg, state.Model) {
		return msg, false, nil
	}
	if !sameRuntimeModel(o.currentModel(), state.Model) {
		return msg, false, nil
	}
	if assistantIsBeforeLatestCompaction(assistant.branch, assistant.entry, msg) {
		return msg, false, nil
	}
	if o.overflowRecoveryWasAttempted() {
		return msg, false, contextOverflowRecoveryError(msg)
	}

	o.setOverflowRecoveryAttempted(true)
	// pi emits overflow compaction_start before auto-compaction work:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1854.
	o.publishCompactionStart(overflowCompactionReason)
	parentID := copyEntryIDPtr(assistant.entry.ParentID)
	if _, err := o.session.MoveTo(ctx, parentID, nil); err != nil {
		wrapped := fmt.Errorf("context overflow recovery move: %w", err)
		o.publishCompactionEnd(overflowCompactionReason, nil, false, false, wrapped)
		return msg, true, wrapped
	}
	compactResult, err := o.harness.Compact(ctx, state, overflowCompactionReason)
	if err != nil {
		o.publishCompactionEnd(overflowCompactionReason, nil, false, false, err)
		return msg, true, fmt.Errorf("context overflow recovery compact: %w", err)
	}
	// pi emits overflow compaction_end before retrying the assistant turn:
	// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:1996.
	o.publishCompactionEnd(overflowCompactionReason, compactResult, false, true, nil)

	continued, err := o.harness.Continue(ctx, state)
	if err != nil {
		return continued, true, fmt.Errorf("context overflow recovery continue: %w", err)
	}
	if ai.IsContextOverflow(continued, state.Model.ContextWindow) {
		return continued, true, contextOverflowRecoveryError(continued)
	}
	if continued.StopReason == ai.StopReasonError {
		return continued, true, continueAssistantError(continued)
	}
	o.setOverflowRecoveryAttempted(false)
	return continued, true, nil
}

// SetAutoCompaction mirrors pi set_auto_compaction -> setAutoCompactionEnabled:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:526-529
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2032-2034
// .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:663-674.
func (o *Orchestrator) SetAutoCompaction(enabled bool) {
	o.mu.Lock()
	o.autoCompactionEnabled = enabled
	o.mu.Unlock()

	o.persistGlobalSetting([]string{"compaction", "enabled"}, enabled)
}

func (o *Orchestrator) isAutoCompactionEnabled() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.autoCompactionEnabled
}

func (o *Orchestrator) setOverflowRecoveryAttempted(attempted bool) {
	o.mu.Lock()
	o.overflowRecoveryAttempted = attempted
	o.mu.Unlock()
}

func (o *Orchestrator) overflowRecoveryWasAttempted() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.overflowRecoveryAttempted
}

func sameAssistantModel(msg ai.Message, model ai.Model) bool {
	return msg.Provider == model.Provider && msg.Model == model.ID
}

func sameRuntimeModel(a ai.Model, b ai.Model) bool {
	return a.Provider == b.Provider && a.ID == b.ID
}

func contextOverflowRecoveryError(msg ai.Message) error {
	if msg.ErrorMessage != "" {
		return fmt.Errorf("%w: %s", errContextOverflowRecovery, msg.ErrorMessage)
	}
	return errContextOverflowRecovery
}

func continueAssistantError(msg ai.Message) error {
	if msg.ErrorMessage != "" {
		return fmt.Errorf("context overflow recovery continue failed: %s", msg.ErrorMessage)
	}
	return errors.New("context overflow recovery continue failed")
}

func assistantEntryForMessage(s harness.Session, target ai.Message) (assistantEntryResult, error) {
	branch, err := s.GetBranch(nil)
	if err != nil {
		return assistantEntryResult{}, err
	}

	var fallback assistantEntryResult
	for i := len(branch) - 1; i >= 0; i-- {
		entry := branch[i]
		if entry.Type != "message" {
			continue
		}
		msg, ok := message.AsAIMessage(entry.Message)
		if !ok || msg.Role != ai.RoleAssistant {
			continue
		}
		result := assistantEntryResult{
			entry:   entry,
			message: msg,
			branch:  branch,
			ok:      true,
		}
		if assistantMessagesMatch(msg, target) {
			return result, nil
		}
		if !fallback.ok {
			fallback = result
		}
	}
	return fallback, nil
}

func lastAssistantEntry(s harness.Session) (assistantEntryResult, error) {
	branch, err := s.GetBranch(nil)
	if err != nil {
		return assistantEntryResult{}, err
	}
	for i := len(branch) - 1; i >= 0; i-- {
		entry := branch[i]
		if entry.Type != "message" {
			continue
		}
		msg, ok := message.AsAIMessage(entry.Message)
		if !ok || msg.Role != ai.RoleAssistant {
			continue
		}
		return assistantEntryResult{
			entry:   entry,
			message: msg,
			branch:  branch,
			ok:      true,
		}, nil
	}
	return assistantEntryResult{branch: branch}, nil
}

func assistantMessagesMatch(a ai.Message, b ai.Message) bool {
	if a.ResponseID != "" || b.ResponseID != "" {
		return a.ResponseID == b.ResponseID && a.ResponseID != ""
	}
	return a.Provider == b.Provider &&
		a.Model == b.Model &&
		a.Timestamp == b.Timestamp &&
		a.StopReason == b.StopReason &&
		a.ErrorMessage == b.ErrorMessage
}

func assistantIsBeforeLatestCompaction(
	branch []session.SessionEntry,
	assistant session.SessionEntry,
	msg ai.Message,
) bool {
	compaction := latestCompactionEntry(branch)
	if compaction == nil {
		return false
	}

	assistantMillis := msg.Timestamp
	if assistantMillis == 0 {
		var ok bool
		assistantMillis, ok = entryTimestampMillis(assistant.Timestamp)
		if !ok {
			return false
		}
	}
	compactionMillis, ok := entryTimestampMillis(compaction.Timestamp)
	if !ok {
		return false
	}
	return assistantMillis <= compactionMillis
}

func latestCompactionEntry(branch []session.SessionEntry) *session.SessionEntry {
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == "compaction" {
			return &branch[i]
		}
	}
	return nil
}

func entryTimestampMillis(timestamp string) (int64, bool) {
	if timestamp == "" {
		return 0, false
	}
	t, err := time.Parse("2006-01-02T15:04:05.000Z", timestamp)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

func copyEntryIDPtr(id *session.EntryID) *session.EntryID {
	if id == nil {
		return nil
	}
	copied := *id
	return &copied
}
