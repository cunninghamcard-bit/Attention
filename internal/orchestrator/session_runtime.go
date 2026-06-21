package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/harness"
	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// ForkMessage is the user-message selector shape behind rpc get_fork_messages.
type ForkMessage struct {
	EntryID string
	Text    string
}

type rebindOptions struct {
	recoverState      bool
	replacementReason string
}

type sessionBeforeForkOutcome struct {
	cancelled               bool
	skipConversationRestore bool
}

// NewSession creates a fresh JSONL session and rebinds the runtime to it.
// pi calls runtimeHost.newSession and rebinds after success:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:428-435.
func (o *Orchestrator) NewSession(ctx context.Context, parentSession string) (bool, error) {
	repo, cwd, _, metadata, cancelled, err := o.sessionRuntimeSnapshot()
	if cancelled || err != nil {
		return cancelled, err
	}
	if parentSession == "" {
		parentSession = metadata.Path
	}

	cancelled, err = o.emitSessionBeforeSwitch(ctx, "new", "")
	if cancelled || err != nil {
		return cancelled, err
	}

	created, err := repo.Create(ctx, session.JsonlSessionCreateOptions{
		CWD:               cwd,
		ParentSessionPath: parentSession,
	})
	if err != nil {
		return false, err
	}
	return o.rebindSession(ctx, created, rebindOptions{replacementReason: "new"})
}

// SwitchSession opens an existing JSONL session and rebinds the runtime to it.
// pi calls runtimeHost.switchSession and rebinds after success:
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:573-579.
func (o *Orchestrator) SwitchSession(ctx context.Context, sessionPath string) (bool, error) {
	repo, cwd, _, _, cancelled, err := o.sessionRuntimeSnapshot()
	if cancelled || err != nil {
		return cancelled, err
	}

	cancelled, err = o.emitSessionBeforeSwitch(ctx, "resume", sessionPath)
	if cancelled || err != nil {
		return cancelled, err
	}

	opened, err := repo.Open(ctx, session.Metadata{Path: sessionPath, CWD: cwd})
	if err != nil {
		return false, err
	}
	return o.rebindSession(ctx, opened, rebindOptions{
		recoverState:      true,
		replacementReason: "resume",
	})
}

// Fork creates a new session before the selected user message, returns that
// selected text, and rebinds to the fork. pi position "before" targets the
// selected user entry's parent and returns selectedText:
// .agents/references/pi/packages/coding-agent/src/core/agent-session-runtime.ts:263-271.
func (o *Orchestrator) Fork(ctx context.Context, entryID string) (string, bool, error) {
	if entryID == "" {
		return "", false, errors.New("orchestrator: fork entry id is required")
	}
	repo, cwd, current, metadata, cancelled, err := o.sessionRuntimeSnapshot()
	if cancelled || err != nil {
		return "", cancelled, err
	}

	beforeFork, err := o.emitSessionBeforeFork(ctx, entryID, "before")
	if beforeFork.cancelled || err != nil {
		return "", beforeFork.cancelled, err
	}

	id := session.EntryID(entryID)
	branch, err := current.GetBranch(nil)
	if err != nil {
		return "", false, err
	}
	text, err := selectedForkText(branch, id)
	if err != nil {
		return "", false, err
	}

	forked, err := repo.Fork(ctx, metadata, session.JsonlSessionForkOptions{
		EntryID:                 &id,
		Position:                session.ForkBefore,
		CWD:                     cwd,
		SkipConversationRestore: beforeFork.skipConversationRestore,
	})
	if err != nil {
		return "", false, err
	}
	cancelled, err = o.rebindSession(ctx, forked, rebindOptions{
		recoverState:      true,
		replacementReason: "fork",
	})
	return text, cancelled, err
}

// Clone creates a new session at the current leaf entry and rebinds to it.
// pi clone passes the leaf id to runtimeHost.fork with position "at":
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:589-599.
func (o *Orchestrator) Clone(ctx context.Context) (bool, error) {
	repo, cwd, current, metadata, cancelled, err := o.sessionRuntimeSnapshot()
	if cancelled || err != nil {
		return cancelled, err
	}

	branch, err := current.GetBranch(nil)
	if err != nil {
		return false, err
	}
	if len(branch) == 0 {
		return false, errors.New("Cannot clone session: no current entry selected")
	}
	id := branch[len(branch)-1].ID
	beforeFork, err := o.emitSessionBeforeFork(ctx, string(id), "at")
	if beforeFork.cancelled || err != nil {
		return beforeFork.cancelled, err
	}

	cloned, err := repo.Fork(ctx, metadata, session.JsonlSessionForkOptions{
		EntryID:                 &id,
		Position:                session.ForkAt,
		CWD:                     cwd,
		SkipConversationRestore: beforeFork.skipConversationRestore,
	})
	if err != nil {
		return false, err
	}
	return o.rebindSession(ctx, cloned, rebindOptions{
		recoverState:      true,
		replacementReason: "fork",
	})
}

// ForkMessages lists user messages in the current branch as fork candidates.
// pi getUserMessagesForForking emits {entryId,text} for non-empty user text:
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2846-2860.
func (o *Orchestrator) ForkMessages() []ForkMessage {
	o.mu.Lock()
	current := o.session
	o.mu.Unlock()
	if current == nil {
		return []ForkMessage{}
	}

	branch, err := current.GetBranch(nil)
	if err != nil {
		return []ForkMessage{}
	}
	messages := []ForkMessage{}
	for _, entry := range branch {
		text, ok := userEntryText(entry)
		if ok && text != "" {
			messages = append(messages, ForkMessage{EntryID: string(entry.ID), Text: text})
		}
	}
	return messages
}

func (o *Orchestrator) sessionRuntimeSnapshot() (
	*session.JsonlSessionRepo,
	string,
	harness.Session,
	session.Metadata,
	bool,
	error,
) {
	o.mu.Lock()
	if o.phase != phaseIdle {
		o.mu.Unlock()
		return nil, "", nil, session.Metadata{}, true, nil
	}
	repo := o.repo
	cwd := o.cwd
	current := o.session
	o.mu.Unlock()

	if repo == nil {
		return nil, "", nil, session.Metadata{}, false, errors.New("orchestrator: session repo is required")
	}
	if current == nil {
		return nil, "", nil, session.Metadata{}, false, errors.New("orchestrator: session is required")
	}

	metadata := current.GetMetadata()
	if cwd == "" {
		cwd = metadata.CWD
	}
	return repo, cwd, current, metadata, false, nil
}

func (o *Orchestrator) rebindSession(
	ctx context.Context,
	newSession harness.Session,
	opts rebindOptions,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if newSession == nil {
		return false, errors.New("orchestrator: session is required")
	}

	o.mu.Lock()
	if o.phase != phaseIdle {
		o.mu.Unlock()
		return true, nil
	}
	currentModel := o.model
	currentThinkingLevel := o.thinkingLevel
	prov := o.provider
	registry := o.hooks
	tools := o.tools
	settings := cloneSettings(o.settings)
	var previousSessionFile string
	if o.session != nil {
		previousSessionFile = o.session.GetMetadata().Path
	}
	o.mu.Unlock()

	model := currentModel
	thinkingLevel := currentThinkingLevel
	if opts.recoverState {
		var err error
		model, thinkingLevel, err = o.recoverSessionState(
			ctx,
			newSession,
			currentModel,
			currentThinkingLevel,
			prov,
		)
		if err != nil {
			return false, err
		}
	}
	if prov == nil {
		return false, errors.New("orchestrator: provider registry is required")
	}

	newHarness := harness.New(harness.HarnessConfig{
		Session:            newSession,
		Hooks:              registry,
		Tools:              tools,
		CompactionSettings: compactionSettingsFrom(settings),
		GetProviderAuth:    providerAuthResolver(prov),
	})

	if opts.replacementReason != "" {
		newMetadata := newSession.GetMetadata()
		if err := o.emitSessionShutdown(ctx, opts.replacementReason, newMetadata.Path); err != nil {
			return false, err
		}
	}

	o.mu.Lock()
	if o.phase != phaseIdle {
		o.mu.Unlock()
		return true, nil
	}
	o.session = newSession
	o.model = model
	o.thinkingLevel = thinkingLevel
	o.harness = newHarness
	o.overflowRecoveryAttempted = false
	o.retryAttempt = 0
	o.steerQueue = nil
	o.followUpQueue = nil
	o.nextTurnQueue = nil
	o.pendingWrites = nil
	o.mu.Unlock()

	if opts.recoverState {
		o.emitModelSelect(ctx, model, currentModel, modelSelectSourceRestore)
	}

	if opts.replacementReason != "" {
		if err := o.emitSessionStart(ctx, opts.replacementReason, previousSessionFile); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (o *Orchestrator) NotifySessionShutdown(ctx context.Context, reason string) error {
	return o.emitSessionShutdown(ctx, reason, "")
}

func (o *Orchestrator) emitSessionBeforeSwitch(
	ctx context.Context,
	reason string,
	targetSessionFile string,
) (bool, error) {
	registry := o.hookRegistry()
	if registry == nil || !registry.HasHandlers(hook.EventSessionBeforeSwitch) {
		return false, nil
	}

	result, err := registry.Emit(ctx, hook.SessionBeforeSwitchEvent{
		Type:              hook.EventSessionBeforeSwitch,
		Reason:            reason,
		TargetSessionFile: stringPtrOrNil(targetSessionFile),
	})
	if err != nil {
		return false, err
	}
	if r, ok := sessionBeforeSwitchResult(result); ok && r.Cancel {
		return true, nil
	}
	return false, nil
}

func (o *Orchestrator) emitSessionBeforeFork(
	ctx context.Context,
	entryID string,
	position string,
) (sessionBeforeForkOutcome, error) {
	var outcome sessionBeforeForkOutcome
	registry := o.hookRegistry()
	if registry == nil || !registry.HasHandlers(hook.EventSessionBeforeFork) {
		return outcome, nil
	}

	result, err := registry.Emit(ctx, hook.SessionBeforeForkEvent{
		Type:     hook.EventSessionBeforeFork,
		EntryID:  entryID,
		Position: position,
	})
	if err != nil {
		return outcome, err
	}
	r, ok := sessionBeforeForkResult(result)
	if !ok {
		return outcome, nil
	}
	outcome.cancelled = r.Cancel
	outcome.skipConversationRestore = r.SkipConversationRestore
	return outcome, nil
}

func (o *Orchestrator) emitSessionShutdown(
	ctx context.Context,
	reason string,
	targetSessionFile string,
) error {
	registry := o.hookRegistry()
	if registry == nil || !registry.HasHandlers(hook.EventSessionShutdown) {
		return nil
	}

	_, err := registry.Emit(ctx, hook.SessionShutdownEvent{
		Type:              hook.EventSessionShutdown,
		Reason:            reason,
		TargetSessionFile: stringPtrOrNil(targetSessionFile),
	})
	return err
}

func (o *Orchestrator) emitSessionStart(
	ctx context.Context,
	reason string,
	previousSessionFile string,
) error {
	registry := o.hookRegistry()
	if registry == nil || !registry.HasHandlers(hook.EventSessionStart) {
		return nil
	}

	_, err := registry.Emit(ctx, hook.SessionStartEvent{
		Type:                hook.EventSessionStart,
		Reason:              reason,
		PreviousSessionFile: stringPtrOrNil(previousSessionFile),
	})
	return err
}

func (o *Orchestrator) hookRegistry() *hook.Registry {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hooks
}

func sessionBeforeSwitchResult(result any) (hook.SessionBeforeSwitchResult, bool) {
	switch r := result.(type) {
	case hook.SessionBeforeSwitchResult:
		return r, true
	case *hook.SessionBeforeSwitchResult:
		if r == nil {
			return hook.SessionBeforeSwitchResult{}, false
		}
		return *r, true
	default:
		return hook.SessionBeforeSwitchResult{}, false
	}
}

func sessionBeforeForkResult(result any) (hook.SessionBeforeForkResult, bool) {
	switch r := result.(type) {
	case hook.SessionBeforeForkResult:
		return r, true
	case *hook.SessionBeforeForkResult:
		if r == nil {
			return hook.SessionBeforeForkResult{}, false
		}
		return *r, true
	default:
		return hook.SessionBeforeForkResult{}, false
	}
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (o *Orchestrator) recoverSessionState(
	ctx context.Context,
	newSession harness.Session,
	currentModel ai.Model,
	currentThinkingLevel agentloop.ThinkingLevel,
	prov *provider.Registry,
) (ai.Model, agentloop.ThinkingLevel, error) {
	sessionCtx, err := newSession.BuildContext(ctx)
	if err != nil {
		return ai.Model{}, "", err
	}

	model := currentModel
	if sessionCtx.Model != nil && sessionCtx.Model.ModelID != "" && prov != nil {
		var resolved ai.Model
		var ok bool
		if sessionCtx.Model.Provider != "" {
			resolved, ok = prov.ResolveByProvider(sessionCtx.Model.Provider, sessionCtx.Model.ModelID)
		} else {
			resolved, ok = prov.Resolve(sessionCtx.Model.ModelID)
		}
		if ok {
			model = resolved
		}
	}
	thinkingLevel := currentThinkingLevel
	if sessionCtx.ThinkingLevel != "" {
		thinkingLevel = agentloop.ThinkingLevel(sessionCtx.ThinkingLevel)
	}
	return model, thinkingLevel, nil
}

func selectedForkText(branch []session.SessionEntry, id session.EntryID) (string, error) {
	for _, entry := range branch {
		if entry.ID != id {
			continue
		}
		text, ok := userEntryText(entry)
		if !ok {
			return "", fmt.Errorf("orchestrator: entry %s is not a user message", id)
		}
		return text, nil
	}
	return "", fmt.Errorf("orchestrator: fork entry %s not found", id)
}

func userEntryText(entry session.SessionEntry) (string, bool) {
	msg, ok := message.AsAIMessage(entry.Message)
	if entry.Type != "message" || !ok || msg.Role != ai.RoleUser {
		return "", false
	}
	return extractUserMessageText(msg.Content), true
}

func extractUserMessageText(content []ai.ContentBlock) string {
	var text strings.Builder
	for _, block := range content {
		if block.Type == ai.ContentText {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}
