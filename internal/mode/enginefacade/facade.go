package enginefacade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/backend"
	localenv "github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/exporthtml"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
	"github.com/cunninghamcard-bit/Attention/internal/provider"
	"github.com/cunninghamcard-bit/Attention/internal/session"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
)

type Options struct {
	SessionID          string
	Metadata           func() session.Metadata // 兼容测试注入；生产路径优先走 Repo。
	Store              backend.EventStore
	Bus                backend.NotifyBus
	Queue              backend.JobQueue
	Repo               *session.JsonlSessionRepo
	CWD                string
	Model              ai.Model
	ThinkingLevel      string
	Provider           *provider.Registry
	ShellPath          string
	ShellCommandPrefix string
	SystemPrompt       string
	ExportTools        []exporthtml.ToolDefinition
	SlashCommands      []compat.SlashCommand
}

// Facade v1：先承重 print/json 模式的 promptRunner 面
// （Subscribe/Prompt/SessionMetadata），逐步长成 compat.Target 全集（Task 13）。
type Facade struct {
	opts Options

	mu                    sync.Mutex
	sessionID             string
	sessionChange         chan struct{}
	query                 queryState
	model                 ai.Model
	thinkingLevel         string
	steeringMode          compat.QueueMode
	followUpMode          compat.QueueMode
	autoCompactionEnabled bool
	autoRetryEnabled      bool
	retryAbortRequested   bool
	ui                    compat.UIContext
	bashCancel            *bashAbortHandle
}

var _ compat.Target = (*Facade)(nil)

type bashAbortHandle struct {
	cancel context.CancelFunc
}

func New(opts Options) *Facade {
	thinkingLevel := opts.ThinkingLevel
	if thinkingLevel == "" {
		thinkingLevel = string(agentloop.ThinkingMedium)
	}
	return &Facade{
		opts:                  opts,
		sessionID:             opts.SessionID,
		sessionChange:         make(chan struct{}),
		model:                 opts.Model,
		thinkingLevel:         thinkingLevel,
		steeringMode:          compat.QueueModeOneAtATime,
		followUpMode:          compat.QueueModeOneAtATime,
		autoCompactionEnabled: true,
		autoRetryEnabled:      true,
	}
}

func (f *Facade) SessionMetadata() session.Metadata {
	sessionID := f.currentSessionID()
	if f.opts.Repo != nil {
		s, ok, err := f.opts.Repo.Get(context.Background(), sessionID)
		if err == nil && ok {
			return s.GetMetadata()
		}
	}
	if f.opts.Metadata != nil {
		return f.opts.Metadata()
	}
	return session.Metadata{ID: sessionID}
}

// Subscribe 语义对照 orchestrator.Subscribe：订阅时刻起的直播（不重放历史）。
// 实现 = notify-then-fetch + fold：信号 → ReadAfter(cursor) → 折叠 → 逐条回调。
func (f *Facade) Subscribe(fn func(compat.Event)) func() {
	if f.opts.Bus == nil || f.opts.Store == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	sessionID, change := f.subscriptionTarget()
	ch, unsub := f.opts.Bus.Subscribe(sessionID)
	cursor := f.lastSeq(ctx, sessionID)

	go func() {
		defer func() { unsub() }()
		fld := &fold{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				batch, err := f.opts.Store.ReadAfter(ctx, sessionID, cursor, 0)
				if err != nil {
					return
				}
				for _, env := range batch {
					cursor = env.Seq
					for _, ev := range fld.apply(env) {
						fn(ev)
					}
				}
			case <-change:
				unsub()
				fld = &fold{}
				sessionID, change = f.subscriptionTarget()
				ch, unsub = f.opts.Bus.Subscribe(sessionID)
				cursor = f.lastSeq(ctx, sessionID)
			}
		}
	}()

	var once sync.Once
	return func() { once.Do(cancel) }
}

// Prompt：进队（g1）→ 进队即受理（PreflightResult(true)，pi rpc ack 语义）
// → 阻塞至本 run 的终态信封（runId 对账）→ 返回最后一条 assistant 消息。
func (f *Facade) Prompt(ctx context.Context, in compat.PromptInput) (compat.PromptResult, error) {
	if f.opts.Queue == nil || f.opts.Bus == nil || f.opts.Store == nil {
		return compat.PromptResult{}, errors.New("enginefacade: prompt is not wired")
	}
	text, err := promptText(in)
	if err != nil {
		return compat.PromptResult{}, err
	}

	sessionID := f.currentSessionID()
	runID := protocol.NewRunID()
	payload, err := json.Marshal(protocol.PromptRequest{Text: text})
	if err != nil {
		return compat.PromptResult{}, err
	}

	// 等待者先挂好再进队：终态信封不会跑在订阅之前。
	ch, unsub := f.opts.Bus.Subscribe(sessionID)
	defer unsub()
	cursor := f.lastSeq(ctx, sessionID)

	if err := f.opts.Queue.Enqueue(ctx, backend.Job{
		SessionID: sessionID,
		RunID:     runID,
		Kind:      backend.JobPrompt,
		Payload:   payload,
	}); err != nil {
		return compat.PromptResult{}, err
	}
	if in.PreflightResult != nil {
		in.PreflightResult(true)
	}

	var lastAssistant *ai.Message
	for {
		batch, err := f.opts.Store.ReadAfter(ctx, sessionID, cursor, 0)
		if err != nil {
			return compat.PromptResult{}, err
		}
		for _, env := range batch {
			cursor = env.Seq
			if env.RunID != runID {
				continue
			}
			switch env.Kind {
			case protocol.KindMessageCompleted:
				var p envelopePayload
				if json.Unmarshal(env.Payload, &p) == nil && p.Message != nil && p.Message.Role == ai.RoleAssistant {
					lastAssistant = p.Message
				}
			case protocol.KindRunCompleted:
				result := compat.PromptResult{}
				if lastAssistant != nil {
					result.Message = *lastAssistant
				}
				return result, nil
			case protocol.KindRunFailed:
				var p envelopePayload
				_ = json.Unmarshal(env.Payload, &p)
				reason := p.ErrorMessage
				if reason == "" {
					reason = p.ErrorClass
				}
				return compat.PromptResult{}, errors.New("run failed: " + reason)
			case protocol.KindRunCancelled:
				result := compat.PromptResult{}
				if lastAssistant != nil {
					result.Message = *lastAssistant
				}
				return result, nil
			}
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return compat.PromptResult{}, ctx.Err()
		}
	}
}

// lastSeq：订阅起点 = 当前末尾。v1 全量扫一遍；热路径优化留给 EventStore
// 增 LastSeq 访问器（接口增量，不动语义）。
func (f *Facade) lastSeq(ctx context.Context, sessionID string) uint64 {
	if f.opts.Store == nil {
		return 0
	}
	var last uint64
	batch, err := f.opts.Store.ReadAfter(ctx, sessionID, 0, 0)
	if err != nil {
		return 0
	}
	for _, env := range batch {
		last = env.Seq
	}
	return last
}

func (f *Facade) Steer(ctx context.Context, in compat.UserInput) error {
	text, err := textInput(in)
	if err != nil {
		return err
	}
	return f.enqueue(ctx, backend.JobSteer, "", protocol.SteerRequest{Text: text})
}

func (f *Facade) FollowUp(ctx context.Context, in compat.UserInput) error {
	text, err := textInput(in)
	if err != nil {
		return err
	}
	return f.enqueue(ctx, backend.JobPrompt, protocol.NewRunID(), protocol.PromptRequest{Text: text})
}

func (f *Facade) Abort(ctx context.Context) (compat.AbortResult, error) {
	wasActive, err := f.activeRun(ctx)
	if err != nil {
		return compat.AbortResult{}, err
	}
	if err := f.enqueue(ctx, backend.JobCancel, "", protocol.CancelRequest{}); err != nil {
		return compat.AbortResult{}, err
	}
	return compat.AbortResult{Aborted: wasActive}, nil
}

func (f *Facade) SetModel(ctx context.Context, m ai.Model) error {
	if m.Provider == "" || m.ID == "" {
		return fmt.Errorf("enginefacade: invalid model %q/%q", m.Provider, m.ID)
	}
	spec := m.Provider + "/" + m.ID
	if err := f.enqueue(ctx, backend.JobSetModel, "", protocol.SetModelRequest{Model: spec}); err != nil {
		return err
	}
	f.mu.Lock()
	f.model = m
	if f.query.sessionID == f.sessionID {
		f.query.model = m
	}
	f.mu.Unlock()
	return nil
}

func (f *Facade) CycleModel(ctx context.Context) (compat.ModelCycleResult, bool, error) {
	models := f.AvailableModels(ctx)
	if len(models) <= 1 {
		return compat.ModelCycleResult{}, false, nil
	}

	current := f.currentModel()
	currentLevel := compatThinkingLevel(f.currentThinkingLevel())
	currentIndex := 0
	for i, model := range models {
		if model.Provider == current.Provider && model.ID == current.ID {
			currentIndex = i
			break
		}
	}
	next := models[(currentIndex+1)%len(models)]
	nextLevel := clampThinkingLevel(next, currentLevel)
	if err := f.SetModel(ctx, next); err != nil {
		return compat.ModelCycleResult{}, false, err
	}
	if err := f.SetThinkingLevel(ctx, nextLevel); err != nil {
		return compat.ModelCycleResult{}, false, err
	}
	return compat.ModelCycleResult{Model: next, ThinkingLevel: nextLevel}, true, nil
}

func (f *Facade) SetThinkingLevel(ctx context.Context, level compatThinkingLevel) error {
	if level == "" {
		level = agentloop.ThinkingOff
	}
	if err := f.enqueue(ctx, backend.JobSetThinking, "", protocol.SetThinkingRequest{Level: string(level)}); err != nil {
		return err
	}
	f.mu.Lock()
	f.thinkingLevel = string(level)
	if f.query.sessionID == f.sessionID {
		f.query.thinkingLevel = string(level)
	}
	f.mu.Unlock()
	return nil
}

func (f *Facade) CycleThinkingLevel(ctx context.Context) (compatThinkingLevel, bool, error) {
	model := f.currentModel()
	if !model.Reasoning {
		return "", false, nil
	}
	levels := supportedThinkingLevels(model)
	if len(levels) == 0 {
		return "", false, nil
	}
	current := compatThinkingLevel(f.currentThinkingLevel())
	next := levels[0]
	for i, level := range levels {
		if level == current {
			next = levels[(i+1)%len(levels)]
			break
		}
	}
	if err := f.SetThinkingLevel(ctx, next); err != nil {
		return "", false, err
	}
	return next, true, nil
}

func (f *Facade) Compact(context.Context, compat.CompactOptions) (compat.CompactResult, error) {
	return compat.CompactResult{}, errors.New("enginefacade: compact not supported yet (P0)")
}

func (f *Facade) SetSteeringMode(mode compat.QueueMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steeringMode = mode
}

func (f *Facade) SetFollowUpMode(mode compat.QueueMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.followUpMode = mode
}

func (f *Facade) SetAutoCompaction(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoCompactionEnabled = enabled
}

func (f *Facade) SetAutoRetry(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoRetryEnabled = enabled
}

func (f *Facade) AbortRetry() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryAbortRequested = true
}

func (f *Facade) ExecuteBash(ctx context.Context, command string) (compat.BashResult, error) {
	env := localenv.New(f.cwd(), localenv.WithShell(f.opts.ShellPath))
	runCtx, cancel := context.WithCancel(ctx)
	handle := &bashAbortHandle{cancel: cancel}
	f.mu.Lock()
	f.bashCancel = handle
	f.mu.Unlock()
	defer func() {
		cancel()
		f.mu.Lock()
		if f.bashCancel == handle {
			f.bashCancel = nil
		}
		f.mu.Unlock()
	}()

	if f.opts.ShellCommandPrefix != "" {
		command = f.opts.ShellCommandPrefix + "\n" + command
	}
	run := builtin.RunBash(runCtx, env, command)
	return compat.BashResult{
		Output:         run.Output,
		ExitCode:       run.ExitCode,
		Cancelled:      run.Cancelled,
		Truncated:      run.Truncated,
		FullOutputPath: run.FullOutputPath,
	}, nil
}

func (f *Facade) AbortBash() {
	f.mu.Lock()
	handle := f.bashCancel
	f.mu.Unlock()
	if handle != nil {
		handle.cancel()
	}
}

func (f *Facade) ExportHTML(ctx context.Context, outputPath string) (string, error) {
	s, ok, err := f.currentSession(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("enginefacade: session %q not found", f.currentSessionID())
	}
	metadata := s.GetMetadata()
	path, err := resolveExportHTMLPath(metadata.CWD, outputPath, time.Now())
	if err != nil {
		return "", err
	}
	leafID, err := s.GetLeafID()
	if err != nil {
		return "", err
	}
	html := exporthtml.Render(exporthtml.SessionData{
		Header: exporthtml.SessionHeader{
			Type:          "session",
			Version:       3,
			ID:            metadata.ID,
			Timestamp:     metadata.CreatedAt,
			CWD:           metadata.CWD,
			ParentSession: metadata.ParentSessionPath,
		},
		Entries:      s.GetEntries(),
		LeafID:       leafID,
		SystemPrompt: f.opts.SystemPrompt,
		Tools:        append([]exporthtml.ToolDefinition(nil), f.opts.ExportTools...),
	}, exporthtml.Options{})
	if err := localenv.New(metadata.CWD).WriteFile(ctx, path, []byte(html)); err != nil {
		return "", fmt.Errorf("export html write: %w", err)
	}
	return path, nil
}

func (f *Facade) NewSession(ctx context.Context, parentSession string) (bool, error) {
	if f.opts.Repo == nil {
		return false, errors.New("enginefacade: session repo is required")
	}
	active, err := f.activeRun(ctx)
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	opts := session.JsonlSessionCreateOptions{CWD: f.cwd()}
	if parentSession == "" {
		parentSession = f.SessionMetadata().Path
	}
	if parentSession != "" {
		opts.ParentSessionPath = parentSession
	}
	s, err := f.opts.Repo.Create(ctx, opts)
	if err != nil {
		return false, err
	}
	f.setCurrentSessionID(s.GetMetadata().ID)
	return false, nil
}

func (f *Facade) SwitchSession(ctx context.Context, sessionPath string) (bool, error) {
	if f.opts.Repo == nil {
		return false, errors.New("enginefacade: session repo is required")
	}
	active, err := f.activeRun(ctx)
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	if sessionPath == "" {
		return false, errors.New("enginefacade: session path is required")
	}
	opened, err := f.opts.Repo.Open(ctx, session.Metadata{Path: sessionPath, CWD: f.cwd()})
	if err != nil {
		return false, err
	}
	f.setCurrentSessionID(opened.GetMetadata().ID)
	return false, nil
}

func (f *Facade) Fork(ctx context.Context, entryID string) (string, bool, error) {
	if f.opts.Repo == nil {
		return "", false, errors.New("enginefacade: session repo is required")
	}
	if entryID == "" {
		return "", false, errors.New("enginefacade: fork entry id is required")
	}
	active, err := f.activeRun(ctx)
	if err != nil {
		return "", false, err
	}
	if active {
		return "", true, nil
	}
	current, ok, err := f.currentSession(ctx)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("enginefacade: session %q not found", f.currentSessionID())
	}
	metadata := f.SessionMetadata()
	id := session.EntryID(entryID)
	branch, err := current.GetBranch(nil)
	if err != nil {
		return "", false, err
	}
	text, err := selectedForkText(branch, id)
	if err != nil {
		return "", false, err
	}
	forked, err := f.opts.Repo.Fork(ctx, metadata, session.JsonlSessionForkOptions{
		EntryID:  &id,
		Position: session.ForkBefore,
		CWD:      f.cwd(),
	})
	if err != nil {
		return "", false, err
	}
	f.setCurrentSessionID(forked.GetMetadata().ID)
	return text, false, nil
}

func (f *Facade) Clone(ctx context.Context) (bool, error) {
	if f.opts.Repo == nil {
		return false, errors.New("enginefacade: session repo is required")
	}
	active, err := f.activeRun(ctx)
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	current, ok, err := f.currentSession(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("enginefacade: session %q not found", f.currentSessionID())
	}
	branch, err := current.GetBranch(nil)
	if err != nil {
		return false, err
	}
	if len(branch) == 0 {
		return false, errors.New("Cannot clone session: no current entry selected")
	}
	id := branch[len(branch)-1].ID
	forked, err := f.opts.Repo.Fork(ctx, f.SessionMetadata(), session.JsonlSessionForkOptions{
		EntryID:  &id,
		Position: session.ForkAt,
		CWD:      f.cwd(),
	})
	if err != nil {
		return false, err
	}
	f.setCurrentSessionID(forked.GetMetadata().ID)
	return false, nil
}

func (f *Facade) ForkMessages() []compat.ForkMessage {
	s, ok, err := f.currentSession(context.Background())
	if err != nil || !ok {
		return []compat.ForkMessage{}
	}
	branch, err := s.GetBranch(nil)
	if err != nil {
		return []compat.ForkMessage{}
	}
	out := []compat.ForkMessage{}
	for _, entry := range branch {
		text, ok := userEntryText(entry)
		if !ok || text == "" {
			continue
		}
		out = append(out, compat.ForkMessage{EntryID: string(entry.ID), Text: text})
	}
	return out
}

func (f *Facade) NotifySessionShutdown(ctx context.Context, _ string) error {
	return f.enqueue(ctx, backend.JobStop, "", nil)
}

func (f *Facade) SetSessionName(ctx context.Context, name string) error {
	s, ok, err := f.currentSession(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("enginefacade: session %q not found", f.currentSessionID())
	}
	if _, err := s.AppendSessionName(ctx, name); err != nil {
		return err
	}
	return f.emitSessionChanged(ctx, map[string]string{"name": name})
}

func (f *Facade) LastAssistantText() (string, bool) {
	messages := f.Messages()
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != ai.RoleAssistant {
			continue
		}
		if msg.StopReason == ai.StopReasonAborted && len(msg.Content) == 0 {
			continue
		}
		trimmed := strings.TrimSpace(textContent(msg.Content))
		if trimmed == "" {
			return "", false
		}
		return trimmed, true
	}
	return "", false
}

func (f *Facade) SlashCommands() []compat.SlashCommand {
	return append([]compat.SlashCommand(nil), f.opts.SlashCommands...)
}

func (f *Facade) WaitForIdle(ctx context.Context) error {
	for {
		active, err := f.activeRun(ctx)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		if f.opts.Bus == nil {
			return errors.New("enginefacade: notify bus is required")
		}
		sessionID := f.currentSessionID()
		ch, unsub := f.opts.Bus.Subscribe(sessionID)
		active, err = f.activeRun(ctx)
		if err != nil {
			unsub()
			return err
		}
		if !active {
			unsub()
			return nil
		}
		select {
		case <-ctx.Done():
			unsub()
			return ctx.Err()
		case <-ch:
			unsub()
		}
	}
}

func (f *Facade) Snapshot() compat.Snapshot {
	f.mu.Lock()
	steeringMode := string(f.steeringMode)
	followUpMode := string(f.followUpMode)
	autoCompactionEnabled := f.autoCompactionEnabled
	f.mu.Unlock()

	snap, err := f.querySnapshot(context.Background())
	if err != nil {
		snap = querySnapshot{sessionID: f.currentSessionID()}
	}
	metadata := f.SessionMetadata()
	name := ""
	if s, ok, err := f.currentSession(context.Background()); err == nil && ok {
		if n, found := s.GetSessionName(); found {
			name = n
		}
	}
	return compat.Snapshot{
		Model:                 snap.model,
		ThinkingLevel:         compatThinkingLevel(snap.thinkingLevel),
		IsStreaming:           snap.activeRun,
		SteeringMode:          steeringMode,
		FollowUpMode:          followUpMode,
		SessionFile:           metadata.Path,
		SessionID:             metadata.ID,
		SessionName:           name,
		AutoCompactionEnabled: autoCompactionEnabled,
		MessageCount:          len(snap.messages),
	}
}

func (f *Facade) SessionStats() compat.SessionStats {
	metadata := f.SessionMetadata()
	stats := compat.SessionStats{SessionFile: metadata.Path, SessionID: metadata.ID}
	snap, err := f.querySnapshot(context.Background())
	if err != nil {
		return stats
	}
	for _, msg := range snap.messages {
		stats.TotalMessages++
		switch msg.Role {
		case ai.RoleUser:
			stats.UserMessages++
		case ai.RoleAssistant:
			stats.AssistantMessages++
			for _, block := range msg.Content {
				if block.Type == ai.ContentToolCall {
					stats.ToolCalls++
				}
			}
			if msg.Usage != nil {
				stats.Tokens.Input += msg.Usage.Input
				stats.Tokens.Output += msg.Usage.Output
				stats.Tokens.CacheRead += msg.Usage.CacheRead
				stats.Tokens.CacheWrite += msg.Usage.CacheWrite
				if msg.Usage.Cost != nil {
					stats.Cost += msg.Usage.Cost.Total
				}
			}
		case ai.RoleToolResult:
			stats.ToolResults++
		}
	}
	stats.Tokens.Total = stats.Tokens.Input + stats.Tokens.Output +
		stats.Tokens.CacheRead + stats.Tokens.CacheWrite
	stats.ContextUsage = f.contextUsage(snap.messages, snap.model)
	return stats
}

func (f *Facade) Messages() []ai.Message {
	snap, err := f.querySnapshot(context.Background())
	if err != nil {
		return []ai.Message{}
	}
	return snap.messages
}

func (f *Facade) ResolveModel(ctx context.Context, providerName, modelID string) (ai.Model, bool) {
	if f.opts.Provider == nil {
		return ai.Model{}, false
	}
	for _, model := range f.opts.Provider.Available(ctx) {
		if model.Provider == providerName && model.ID == modelID {
			return model, true
		}
	}
	return ai.Model{}, false
}

func (f *Facade) AvailableModels(ctx context.Context) []ai.Model {
	if f.opts.Provider == nil {
		return []ai.Model{}
	}
	return f.opts.Provider.Available(ctx)
}

func (f *Facade) SetUIContext(ui compat.UIContext) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ui = ui
}

type compatThinkingLevel = agentloop.ThinkingLevel

func promptText(in compat.PromptInput) (string, error) {
	if in.Message != nil {
		msg, ok := message.AsAIMessage(in.Message)
		if !ok {
			return "", errors.New("enginefacade: non-text prompt not supported in P0")
		}
		return textOnlyContent(msg.Content, "prompt")
	}
	if len(in.Content) > 0 {
		return textOnlyContent(in.Content, "prompt")
	}
	return in.Text, nil
}

func textInput(in compat.UserInput) (string, error) {
	if in.Message != nil {
		msg, ok := message.AsAIMessage(in.Message)
		if !ok {
			return "", errors.New("enginefacade: non-text input not supported in P0")
		}
		return textOnlyContent(msg.Content, "input")
	}
	if len(in.Content) > 0 {
		return textOnlyContent(in.Content, "input")
	}
	return in.Text, nil
}

func textOnlyContent(content []ai.ContentBlock, label string) (string, error) {
	var b strings.Builder
	for _, block := range content {
		if block.Type != ai.ContentText {
			return "", fmt.Errorf("enginefacade: non-text %s not supported in P0", label)
		}
		b.WriteString(block.Text)
	}
	return b.String(), nil
}

func (f *Facade) enqueue(ctx context.Context, kind backend.JobKind, runID string, payload any) error {
	if f.opts.Queue == nil {
		return errors.New("enginefacade: job queue is required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return f.opts.Queue.Enqueue(ctx, backend.Job{
		SessionID: f.currentSessionID(),
		RunID:     runID,
		Kind:      kind,
		Payload:   raw,
	})
}

func (f *Facade) currentSessionID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionID
}

func (f *Facade) subscriptionTarget() (string, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionID, f.sessionChange
}

func (f *Facade) setCurrentSessionID(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID == "" || sessionID == f.sessionID {
		return
	}
	f.sessionID = sessionID
	f.query = queryState{}
	close(f.sessionChange)
	f.sessionChange = make(chan struct{})
}

func (f *Facade) currentModel() ai.Model {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.model
}

func (f *Facade) currentThinkingLevel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.thinkingLevel
}

func (f *Facade) cwd() string {
	if f.opts.CWD != "" {
		return f.opts.CWD
	}
	metadata := f.SessionMetadata()
	if metadata.CWD != "" {
		return metadata.CWD
	}
	return "."
}

func (f *Facade) currentSession(ctx context.Context) (*session.Session, bool, error) {
	if f.opts.Repo == nil {
		return nil, false, nil
	}
	return f.opts.Repo.Get(ctx, f.currentSessionID())
}

func (f *Facade) activeRun(ctx context.Context) (bool, error) {
	snap, err := f.querySnapshot(ctx)
	if err != nil {
		return false, err
	}
	return snap.activeRun, nil
}

func (f *Facade) emitSessionChanged(ctx context.Context, payload any) error {
	if f.opts.Store == nil || f.opts.Bus == nil {
		return errors.New("enginefacade: event store and notify bus are required")
	}
	sessionID := f.currentSessionID()
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := &protocol.Envelope{
		ID:            protocol.NewEventID(),
		SessionID:     sessionID,
		Kind:          protocol.KindSessionChanged,
		Actor:         protocol.ActorSystem,
		Payload:       raw,
		OccurredAt:    time.Now().UTC(),
		SchemaVersion: protocol.SchemaVersion,
	}
	if err := f.opts.Store.Append(ctx, env); err != nil {
		return err
	}
	f.opts.Bus.Publish(sessionID)
	return nil
}

func selectedForkText(branch []session.SessionEntry, id session.EntryID) (string, error) {
	for _, entry := range branch {
		if entry.ID != id {
			continue
		}
		text, ok := userEntryText(entry)
		if !ok {
			return "", fmt.Errorf("enginefacade: entry %s is not a user message", id)
		}
		return text, nil
	}
	return "", fmt.Errorf("enginefacade: fork entry %s not found", id)
}

func userEntryText(entry session.SessionEntry) (string, bool) {
	msg, ok := message.AsAIMessage(entry.Message)
	if entry.Type != "message" || !ok || msg.Role != ai.RoleUser {
		return "", false
	}
	return textContent(msg.Content), true
}

func textContent(content []ai.ContentBlock) string {
	var b strings.Builder
	for _, block := range content {
		if block.Type == ai.ContentText {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

func resolveExportHTMLPath(cwd, outputPath string, now time.Time) (string, error) {
	if cwd == "" {
		cwd = "."
	}
	path := outputPath
	if path == "" {
		path = fmt.Sprintf("session-%s.html", now.Format("2006-01-02T15-04-05"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("export html resolve path: %w", err)
	}
	return filepath.Clean(resolved), nil
}
