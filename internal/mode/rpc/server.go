package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// commandTarget is the orchestrator surface the bidirectional rpc server drives.
type commandTarget interface {
	Subscribe(func(orchestrator.Event)) func()
	Prompt(context.Context, orchestrator.PromptInput) (orchestrator.PromptResult, error)
	Steer(context.Context, orchestrator.UserInput) error
	FollowUp(context.Context, orchestrator.UserInput) error
	Abort(context.Context) (orchestrator.AbortResult, error)
	SetModel(context.Context, ai.Model) error
	CycleModel(context.Context) (orchestrator.ModelCycleResult, bool, error)
	SetThinkingLevel(context.Context, agentloop.ThinkingLevel) error
	CycleThinkingLevel(context.Context) (agentloop.ThinkingLevel, bool, error)
	Compact(context.Context, orchestrator.CompactOptions) (orchestrator.CompactResult, error)
	SetSteeringMode(orchestrator.QueueMode)
	SetFollowUpMode(orchestrator.QueueMode)
	SetAutoCompaction(bool)
	SetAutoRetry(bool)
	AbortRetry()
	NewSession(context.Context, string) (bool, error)
	SwitchSession(context.Context, string) (bool, error)
	Fork(context.Context, string) (string, bool, error)
	Clone(context.Context) (bool, error)
	NavigateTree(context.Context, session.EntryID, orchestrator.NavOptions) (orchestrator.NavResult, error)
	ReloadSettings(context.Context) error
	ForkMessages() []orchestrator.ForkMessage
	Entries() []orchestrator.EntrySummary
	NotifySessionShutdown(context.Context, string) error
	SetSessionName(context.Context, string) error
	LastAssistantText() (string, bool)
	SlashCommands() []orchestrator.SlashCommand
	DispatchCommand(context.Context, string, []string) ([]orchestrator.CommandNotification, error)
	WaitForIdle(context.Context) error
	Snapshot() orchestrator.Snapshot
	SessionStats() orchestrator.SessionStats
	Messages() []ai.Message
	ResolveModel(ctx context.Context, provider, modelID string) (ai.Model, bool)
	AvailableModels(context.Context) []ai.Model
}

// Serve runs the bidirectional rpc protocol (pi modes/rpc/rpc-mode.ts):
// JSON-line commands on stdin, JSON-line responses and events on stdout. It is
// the backend a GUI (rpc-client) speaks to.
func Serve(ctx context.Context, orch *orchestrator.Orchestrator) error {
	return serve(ctx, orch, os.Stdin, os.Stdout)
}

func serve(ctx context.Context, target commandTarget, stdin io.Reader, stdout io.Writer) (err error) {
	s := &server{
		target: target,
		writer: newJSONLineWriter(stdout),
	}
	cancel := func() {}
	defer func() {
		_ = target.NotifySessionShutdown(context.WithoutCancel(ctx), "quit")
		s.wg.Wait() // let in-flight prompts finish streaming events...
		cancel()    // ...then unsubscribe
		if flushErr := s.writer.Flush(); flushErr != nil {
			err = errors.Join(err, flushErr)
		}
	}()

	// pi's bidirectional rpc mode emits NO header line — the first output is a
	// response or event (rpc-mode.ts:377+); only json print mode has a header.
	cancel = target.Subscribe(func(ev orchestrator.Event) {
		if value, ok := eventJSONFromOrchestrator(ev); ok {
			s.write(value)
		}
	})

	return s.readCommands(ctx, stdin)
}

type server struct {
	target commandTarget
	writer *jsonLineWriter
	wg     sync.WaitGroup
}

// write serializes one JSON line. The writer is mutex-guarded, so concurrent
// event, response, and async-prompt writes are safe.
func (s *server) write(value any) {
	_ = s.writer.WriteJSON(value)
}

func (s *server) readCommands(ctx context.Context, stdin io.Reader) error {
	// Read on a goroutine so a canceled ctx (e.g. SIGINT) unblocks the loop
	// even while ReadBytes is blocked. The reader goroutine cannot itself be
	// interrupted mid-Read, so it lingers until the next byte or process exit.
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stdin)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line := <-lines:
			// pi dispatches every input line as an unawaited async handler
			// (rpc-mode.ts:760-762), so abort/abort_bash stay processable while a
			// blocking command (bash, compact) runs.
			s.wg.Go(func() { s.dispatch(ctx, line) })
		case err := <-readErr:
			// stdin closed: pi shuts down on input end (rpc-mode.ts:754).
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *server) dispatch(ctx context.Context, line []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		// pi: error(undefined, "parse", ...) (rpc-mode.ts:707).
		s.write(failure("", "parse", fmt.Sprintf("Failed to parse command: %s", err)))
		return
	}
	if envelope.Type == "" {
		// pi: a command without type reaches the default branch and gets an
		// explicit error response, never silence (rpc-mode.ts:665-668).
		s.write(failure("", "undefined", "Unknown command: undefined"))
		return
	}
	var cmd command
	if err := json.Unmarshal(line, &cmd); err != nil {
		// pi: error(undefined, "parse", ...) (rpc-mode.ts:707).
		s.write(failure("", "parse", fmt.Sprintf("Failed to parse command: %s", err)))
		return
	}
	handler, ok := handlers[cmd.Type]
	if !ok {
		// pi: error(undefined, type, ...) — id dropped (rpc-mode.ts:666).
		s.write(failure("", cmd.Type, fmt.Sprintf("Unknown command: %s", cmd.Type)))
		return
	}

	// handler returns nil when it answers asynchronously (prompt), matching
	// pi handleCommand's RpcResponse | undefined (rpc-mode.ts:381).
	if resp := handler(s, ctx, cmd); resp != nil {
		s.write(resp)
	}
}

// command is the flat union of all rpc command fields (pi RpcCommand). Fields
// not used by a given type stay zero — one decode, no per-type juggling.
type command struct {
	ID                 string       `json:"id"`
	Type               string       `json:"type"`
	Message            string       `json:"message"`
	Images             []imageInput `json:"images"`
	StreamingBehavior  string       `json:"streamingBehavior"`
	Provider           string       `json:"provider"`
	ModelID            string       `json:"modelId"`
	Level              string       `json:"level"`
	Mode               string       `json:"mode"`
	Name               string       `json:"name"`
	Enabled            bool         `json:"enabled"`
	CustomInstructions string       `json:"customInstructions"`
	ParentSession      string       `json:"parentSession"`
	SessionPath        string       `json:"sessionPath"`
	Args               string       `json:"args"`
	EntryID            string       `json:"entryId"`
	// navigate_tree options (mirror orchestrator.NavOptions). EntryID above is
	// the target entry; CustomInstructions above is reused for the branch summary.
	Summarize           bool   `json:"summarize"`
	ReplaceInstructions bool   `json:"replaceInstructions"`
	Label               string `json:"label"`
}

type imageInput struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

type response struct {
	ID          string `json:"id,omitempty"`
	Type        string `json:"type"`
	Command     string `json:"command"`
	Success     bool   `json:"success"`
	Data        any    `json:"data,omitempty"`
	IncludeData bool   `json:"-"`
	Error       string `json:"error,omitempty"`
}

func (r response) MarshalJSON() ([]byte, error) {
	type responseWithoutData struct {
		ID      string `json:"id,omitempty"`
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
		Data    any    `json:"data,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	type responseWithData struct {
		ID      string `json:"id,omitempty"`
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
		Data    any    `json:"data"`
		Error   string `json:"error,omitempty"`
	}

	if r.IncludeData {
		return json.Marshal(responseWithData{
			ID:      r.ID,
			Type:    r.Type,
			Command: r.Command,
			Success: r.Success,
			Data:    r.Data,
			Error:   r.Error,
		})
	}
	return json.Marshal(responseWithoutData{
		ID:      r.ID,
		Type:    r.Type,
		Command: r.Command,
		Success: r.Success,
		Data:    r.Data,
		Error:   r.Error,
	})
}

// success/failure mirror pi's success()/error() helpers (rpc-mode.ts:63-76).
// A nil data omits the key, matching pi's `data === undefined` case.
func success(id, command string, data any) *response {
	return &response{
		ID:          id,
		Type:        "response",
		Command:     command,
		Success:     true,
		Data:        data,
		IncludeData: data != nil,
	}
}

func successNull(id, command string) *response {
	return &response{ID: id, Type: "response", Command: command, Success: true, IncludeData: true}
}

func failure(id, command, message string) *response {
	return &response{ID: id, Type: "response", Command: command, Success: false, Error: message}
}

func commandContent(message string, images []imageInput) []ai.ContentBlock {
	content := make([]ai.ContentBlock, 0, len(images)+1)
	if message != "" {
		content = append(content, ai.ContentBlock{Type: ai.ContentText, Text: message})
	}
	for _, img := range images {
		content = append(content, ai.ContentBlock{
			Type:      ai.ContentImage,
			ImageData: img.Data,
			MimeType:  img.MimeType,
		})
	}
	return content
}

// handlers maps a command type to its handler, returning the response to emit
// or nil for async commands (prompt). Deferred pi commands (fork,
// new_session) slot in here as one row each.
var handlers = map[string]func(*server, context.Context, command) *response{
	"prompt":                  (*server).handlePrompt,
	"steer":                   (*server).handleSteer,
	"follow_up":               (*server).handleFollowUp,
	"abort":                   (*server).handleAbort,
	"get_state":               (*server).handleGetState,
	"get_messages":            (*server).handleGetMessages,
	"set_model":               (*server).handleSetModel,
	"cycle_model":             (*server).handleCycleModel,
	"set_thinking_level":      (*server).handleSetThinkingLevel,
	"cycle_thinking_level":    (*server).handleCycleThinkingLevel,
	"set_steering_mode":       (*server).handleSetSteeringMode,
	"set_follow_up_mode":      (*server).handleSetFollowUpMode,
	"compact":                 (*server).handleCompact,
	"set_auto_compaction":     (*server).handleSetAutoCompaction,
	"set_auto_retry":          (*server).handleSetAutoRetry,
	"abort_retry":             (*server).handleAbortRetry,
	"new_session":             (*server).handleNewSession,
	"switch_session":          (*server).handleSwitchSession,
	"fork":                    (*server).handleFork,
	"clone":                   (*server).handleClone,
	"navigate_tree":           (*server).handleNavigateTree,
	"reload":                  (*server).handleReload,
	"get_fork_messages":       (*server).handleGetForkMessages,
	"get_entries":             (*server).handleGetEntries,
	"get_available_models":    (*server).handleGetAvailableModels,
	"get_last_assistant_text": (*server).handleGetLastAssistantText,
	"get_session_stats":       (*server).handleGetSessionStats,
	"set_session_name":        (*server).handleSetSessionName,
	"get_commands":            (*server).handleGetCommands,
	"dispatch_command":        (*server).handleDispatchCommand,
}

// handlePrompt is async (pi rpc-mode.ts:389): emit success once preflight
// passes, run the turn in the background, surface outcome through the event
// stream. If preflight fails the prompt returns an error and we emit it.
func (s *server) handlePrompt(ctx context.Context, cmd command) *response {
	s.wg.Go(func() {
		preflightSucceeded := false
		input := orchestrator.PromptInput{
			Text:              cmd.Message,
			Source:            "rpc",
			StreamingBehavior: cmd.StreamingBehavior,
			PreflightResult: func(ok bool) {
				if ok {
					preflightSucceeded = true
					s.write(success(cmd.ID, "prompt", nil))
				}
			},
		}
		if len(cmd.Images) > 0 {
			input.Text = ""
			input.Content = commandContent(cmd.Message, cmd.Images)
		}
		_, err := s.target.Prompt(ctx, input)
		if err != nil && !preflightSucceeded {
			s.write(failure(cmd.ID, "prompt", err.Error()))
		}
	})
	return nil
}

func (s *server) handleSteer(ctx context.Context, cmd command) *response {
	input := orchestrator.UserInput{Text: cmd.Message}
	if len(cmd.Images) > 0 {
		input.Text = ""
		input.Content = commandContent(cmd.Message, cmd.Images)
	}
	if err := s.target.Steer(ctx, input); err != nil {
		return failure(cmd.ID, "steer", err.Error())
	}
	return success(cmd.ID, "steer", nil)
}

func (s *server) handleFollowUp(ctx context.Context, cmd command) *response {
	input := orchestrator.UserInput{Text: cmd.Message}
	if len(cmd.Images) > 0 {
		input.Text = ""
		input.Content = commandContent(cmd.Message, cmd.Images)
	}
	if err := s.target.FollowUp(ctx, input); err != nil {
		return failure(cmd.ID, "follow_up", err.Error())
	}
	return success(cmd.ID, "follow_up", nil)
}

func (s *server) handleAbort(ctx context.Context, cmd command) *response {
	// pi abort cancels then waits for idle (agent-session.ts:1388).
	if _, err := s.target.Abort(ctx); err != nil {
		return failure(cmd.ID, "abort", err.Error())
	}
	if err := s.target.WaitForIdle(ctx); err != nil {
		return failure(cmd.ID, "abort", err.Error())
	}
	return success(cmd.ID, "abort", nil)
}

func (s *server) handleGetState(_ context.Context, cmd command) *response {
	snap := s.target.Snapshot()
	state := stateJSON{
		ThinkingLevel:         string(snap.ThinkingLevel),
		IsStreaming:           snap.IsStreaming,
		IsCompacting:          snap.IsCompacting,
		SteeringMode:          snap.SteeringMode,
		FollowUpMode:          snap.FollowUpMode,
		SessionFile:           snap.SessionFile,
		SessionID:             snap.SessionID,
		SessionName:           snap.SessionName,
		AutoCompactionEnabled: snap.AutoCompactionEnabled,
		MessageCount:          snap.MessageCount,
		PendingMessageCount:   snap.PendingMessageCount,
	}
	if snap.Model.ID != "" {
		model := snap.Model
		state.Model = &model
	}
	return success(cmd.ID, "get_state", state)
}

func (s *server) handleGetMessages(_ context.Context, cmd command) *response {
	return success(cmd.ID, "get_messages", messagesJSON{Messages: s.target.Messages()})
}

func (s *server) handleGetSessionStats(_ context.Context, cmd command) *response {
	// pi returns session.getSessionStats() directly:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:563-566.
	return success(
		cmd.ID,
		"get_session_stats",
		sessionStatsJSONFromOrchestrator(s.target.SessionStats()),
	)
}

func (s *server) handleSetModel(ctx context.Context, cmd command) *response {
	model, ok := s.target.ResolveModel(ctx, cmd.Provider, cmd.ModelID)
	if !ok {
		return failure(cmd.ID, "set_model", fmt.Sprintf("Model not found: %s/%s", cmd.Provider, cmd.ModelID))
	}
	if err := s.target.SetModel(ctx, model); err != nil {
		return failure(cmd.ID, "set_model", err.Error())
	}
	return success(cmd.ID, "set_model", model)
}

func (s *server) handleCycleModel(ctx context.Context, cmd command) *response {
	// pi passes null, not undefined, when no model cycle happens
	// (.agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:473-479).
	result, cycled, err := s.target.CycleModel(ctx)
	if err != nil {
		return failure(cmd.ID, "cycle_model", err.Error())
	}
	if !cycled {
		return successNull(cmd.ID, "cycle_model")
	}
	return success(cmd.ID, "cycle_model", cycleModelJSON{
		Model:         result.Model,
		ThinkingLevel: string(result.ThinkingLevel),
		IsScoped:      false,
	})
}

func (s *server) handleSetThinkingLevel(ctx context.Context, cmd command) *response {
	level, err := parseThinkingLevel(cmd.Level)
	if err != nil {
		return failure(cmd.ID, "set_thinking_level", err.Error())
	}
	if err := s.target.SetThinkingLevel(ctx, level); err != nil {
		return failure(cmd.ID, "set_thinking_level", err.Error())
	}
	return success(cmd.ID, "set_thinking_level", nil)
}

func (s *server) handleCycleThinkingLevel(ctx context.Context, cmd command) *response {
	// pi passes null, not undefined, when the current model cannot cycle thinking
	// (.agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:495-501).
	level, cycled, err := s.target.CycleThinkingLevel(ctx)
	if err != nil {
		return failure(cmd.ID, "cycle_thinking_level", err.Error())
	}
	if !cycled {
		return successNull(cmd.ID, "cycle_thinking_level")
	}
	return success(cmd.ID, "cycle_thinking_level", thinkingLevelJSON{Level: string(level)})
}

func (s *server) handleSetSteeringMode(_ context.Context, cmd command) *response {
	// pi calls session.setSteeringMode(command.mode) and returns success with no
	// data:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:507-510.
	mode, err := parseQueueMode(cmd.Mode)
	if err != nil {
		return failure(cmd.ID, "set_steering_mode", err.Error())
	}
	s.target.SetSteeringMode(mode)
	return success(cmd.ID, "set_steering_mode", nil)
}

func (s *server) handleSetFollowUpMode(_ context.Context, cmd command) *response {
	// pi calls session.setFollowUpMode(command.mode) and returns success with no
	// data:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:512-515.
	mode, err := parseQueueMode(cmd.Mode)
	if err != nil {
		return failure(cmd.ID, "set_follow_up_mode", err.Error())
	}
	s.target.SetFollowUpMode(mode)
	return success(cmd.ID, "set_follow_up_mode", nil)
}

func (s *server) handleCompact(ctx context.Context, cmd command) *response {
	// pi manual compact aborts the current agent operation first
	// (agent-session.ts:1611); abort is a no-op when already idle.
	if _, err := s.target.Abort(ctx); err != nil {
		return failure(cmd.ID, "compact", err.Error())
	}
	if err := s.target.WaitForIdle(ctx); err != nil {
		return failure(cmd.ID, "compact", err.Error())
	}
	result, err := s.target.Compact(ctx, orchestrator.CompactOptions{CustomInstructions: cmd.CustomInstructions})
	if err != nil {
		return failure(cmd.ID, "compact", err.Error())
	}
	return success(cmd.ID, "compact", compactionJSON{
		Summary:          result.Summary,
		FirstKeptEntryID: string(result.FirstKeptEntryID),
		TokensBefore:     result.TokensBefore,
		Details:          result.Details,
	})
}

func (s *server) handleSetAutoCompaction(_ context.Context, cmd command) *response {
	// pi calls session.setAutoCompactionEnabled(command.enabled) and returns
	// success with no data:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:526-529.
	s.target.SetAutoCompaction(cmd.Enabled)
	return success(cmd.ID, "set_auto_compaction", nil)
}

func (s *server) handleSetAutoRetry(_ context.Context, cmd command) *response {
	// pi calls session.setAutoRetryEnabled(command.enabled) and returns success
	// with no data:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:535-538.
	s.target.SetAutoRetry(cmd.Enabled)
	return success(cmd.ID, "set_auto_retry", nil)
}

func (s *server) handleAbortRetry(_ context.Context, cmd command) *response {
	// pi calls session.abortRetry() and returns success with no data:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:540-543.
	s.target.AbortRetry()
	return success(cmd.ID, "abort_retry", nil)
}

func (s *server) handleNavigateTree(ctx context.Context, cmd command) *response {
	// Navigate the session tree to the target entry, mirroring orchestrator
	// NavOptions (summarize/customInstructions/replaceInstructions/label) and
	// returning the NavResult (cancelled + optional editor text).
	result, err := s.target.NavigateTree(ctx, session.EntryID(cmd.EntryID), orchestrator.NavOptions{
		Summarize:           cmd.Summarize,
		CustomInstructions:  cmd.CustomInstructions,
		ReplaceInstructions: cmd.ReplaceInstructions,
		Label:               cmd.Label,
	})
	if err != nil {
		return failure(cmd.ID, "navigate_tree", err.Error())
	}
	return success(cmd.ID, "navigate_tree", navResultJSONFromOrchestrator(result))
}

func (s *server) handleReload(ctx context.Context, cmd command) *response {
	// Reload keybindings, extensions, skills, prompts, and themes via the
	// orchestrator and return success with no data.
	if err := s.target.ReloadSettings(ctx); err != nil {
		return failure(cmd.ID, "reload", err.Error())
	}
	return success(cmd.ID, "reload", nil)
}

func (s *server) handleNewSession(ctx context.Context, cmd command) *response {
	// pi passes optional parentSession through runtimeHost.newSession and
	// returns {cancelled}:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:428-435.
	cancelled, err := s.target.NewSession(ctx, cmd.ParentSession)
	if err != nil {
		return failure(cmd.ID, "new_session", err.Error())
	}
	return success(cmd.ID, "new_session", sessionReplacementJSON{Cancelled: cancelled})
}

func (s *server) handleSwitchSession(ctx context.Context, cmd command) *response {
	// pi runtimeHost.switchSession(command.sessionPath) returns {cancelled}:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:573-579.
	cancelled, err := s.target.SwitchSession(ctx, cmd.SessionPath)
	if err != nil {
		return failure(cmd.ID, "switch_session", err.Error())
	}
	return success(cmd.ID, "switch_session", sessionReplacementJSON{Cancelled: cancelled})
}

func (s *server) handleFork(ctx context.Context, cmd command) *response {
	// pi maps runtimeHost.fork selectedText to {text,cancelled}:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:581-587.
	text, cancelled, err := s.target.Fork(ctx, cmd.EntryID)
	if err != nil {
		return failure(cmd.ID, "fork", err.Error())
	}
	return success(cmd.ID, "fork", forkJSON{Text: text, Cancelled: cancelled})
}

func (s *server) handleClone(ctx context.Context, cmd command) *response {
	// pi clone forks at the current leaf and returns {cancelled}:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:589-599.
	cancelled, err := s.target.Clone(ctx)
	if err != nil {
		return failure(cmd.ID, "clone", err.Error())
	}
	return success(cmd.ID, "clone", sessionReplacementJSON{Cancelled: cancelled})
}

func (s *server) handleGetForkMessages(_ context.Context, cmd command) *response {
	// pi returns {messages:[{entryId,text}]}:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:601-604.
	messages := s.target.ForkMessages()
	out := make([]forkMessageJSON, 0, len(messages))
	for _, msg := range messages {
		out = append(out, forkMessageJSON{EntryID: msg.EntryID, Text: msg.Text})
	}
	return success(cmd.ID, "get_fork_messages", forkMessagesJSON{Messages: out})
}

func (s *server) handleGetEntries(_ context.Context, cmd command) *response {
	// Mirror get_fork_messages: return the current branch's navigable entries as
	// {entries:[{entryId,label}]} so a tree-navigation picker has data to drive
	// navigate_tree.
	entries := s.target.Entries()
	out := make([]entrySummaryJSON, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entrySummaryJSON{EntryID: entry.EntryID, Label: entry.Label})
	}
	return success(cmd.ID, "get_entries", entriesJSON{Entries: out})
}

func (s *server) handleGetAvailableModels(ctx context.Context, cmd command) *response {
	return success(cmd.ID, "get_available_models", availableModelsJSON{Models: s.target.AvailableModels(ctx)})
}

func (s *server) handleGetLastAssistantText(_ context.Context, cmd command) *response {
	// pi returns { text } from getLastAssistantText
	// (.agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:606-609).
	text, ok := s.target.LastAssistantText()
	var textPtr *string
	if ok {
		textPtr = &text
	}
	return success(cmd.ID, "get_last_assistant_text", lastAssistantTextJSON{Text: textPtr})
}

func (s *server) handleSetSessionName(ctx context.Context, cmd command) *response {
	// pi trims and rejects empty names before calling setSessionName
	// (.agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:611-618).
	name := strings.TrimSpace(cmd.Name)
	if name == "" {
		return failure(cmd.ID, "set_session_name", "Session name cannot be empty")
	}
	if err := s.target.SetSessionName(ctx, name); err != nil {
		return failure(cmd.ID, "set_session_name", err.Error())
	}
	return success(cmd.ID, "set_session_name", nil)
}

func (s *server) handleGetCommands(_ context.Context, cmd command) *response {
	// pi returns { commands } after concatenating extension commands, prompt
	// templates, then skills:
	// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:632-663.
	commands := s.target.SlashCommands()
	out := make([]slashCommandJSON, 0, len(commands))
	for _, command := range commands {
		out = append(out, slashCommandJSONFromOrchestrator(command))
	}
	return success(cmd.ID, "get_commands", slashCommandsJSON{Commands: out})
}

func (s *server) handleDispatchCommand(ctx context.Context, cmd command) *response {
	name := strings.TrimSpace(cmd.Name)
	if name == "" {
		return failure(cmd.ID, "dispatch_command", `command not found: ""`)
	}
	notifications, err := s.target.DispatchCommand(ctx, name, resource.ParseCommandArgs(cmd.Args))
	if err != nil {
		return failure(cmd.ID, "dispatch_command", err.Error())
	}
	data := commandNotificationsJSONFromOrchestrator(notifications)
	if len(data.Notifications) == 0 {
		return success(cmd.ID, "dispatch_command", nil)
	}
	return success(cmd.ID, "dispatch_command", data)
}

func parseThinkingLevel(level string) (agentloop.ThinkingLevel, error) {
	switch agentloop.ThinkingLevel(level) {
	case agentloop.ThinkingOff, agentloop.ThinkingMinimal, agentloop.ThinkingLow,
		agentloop.ThinkingMedium, agentloop.ThinkingHigh, agentloop.ThinkingXHigh:
		return agentloop.ThinkingLevel(level), nil
	default:
		return "", fmt.Errorf("invalid thinking level %q", level)
	}
}

func parseQueueMode(mode string) (orchestrator.QueueMode, error) {
	switch orchestrator.QueueMode(mode) {
	case orchestrator.QueueModeAll, orchestrator.QueueModeOneAtATime:
		return orchestrator.QueueMode(mode), nil
	default:
		return "", fmt.Errorf("invalid queue mode %q", mode)
	}
}

// stateJSON mirrors pi RpcSessionState (rpc-types.ts:90).
type stateJSON struct {
	Model                 *ai.Model `json:"model,omitempty"`
	ThinkingLevel         string    `json:"thinkingLevel"`
	IsStreaming           bool      `json:"isStreaming"`
	IsCompacting          bool      `json:"isCompacting"`
	SteeringMode          string    `json:"steeringMode"`
	FollowUpMode          string    `json:"followUpMode"`
	SessionFile           string    `json:"sessionFile,omitempty"`
	SessionID             string    `json:"sessionId"`
	SessionName           string    `json:"sessionName,omitempty"`
	AutoCompactionEnabled bool      `json:"autoCompactionEnabled"`
	MessageCount          int       `json:"messageCount"`
	PendingMessageCount   int       `json:"pendingMessageCount"`
}

type messagesJSON struct {
	Messages []ai.Message `json:"messages"`
}

type sessionStatsJSON struct {
	SessionFile       string                        `json:"sessionFile,omitempty"`
	SessionID         string                        `json:"sessionId"`
	UserMessages      int                           `json:"userMessages"`
	AssistantMessages int                           `json:"assistantMessages"`
	ToolCalls         int                           `json:"toolCalls"`
	ToolResults       int                           `json:"toolResults"`
	TotalMessages     int                           `json:"totalMessages"`
	Tokens            sessionStatsTokensJSON        `json:"tokens"`
	Cost              float64                       `json:"cost"`
	ContextUsage      *sessionStatsContextUsageJSON `json:"contextUsage,omitempty"`
}

type sessionStatsTokensJSON struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Total      int `json:"total"`
}

type sessionStatsContextUsageJSON struct {
	Tokens        int     `json:"tokens"`
	ContextWindow int     `json:"contextWindow"`
	Percent       float64 `json:"percent"`
}

func sessionStatsJSONFromOrchestrator(stats orchestrator.SessionStats) sessionStatsJSON {
	out := sessionStatsJSON{
		SessionFile:       stats.SessionFile,
		SessionID:         stats.SessionID,
		UserMessages:      stats.UserMessages,
		AssistantMessages: stats.AssistantMessages,
		ToolCalls:         stats.ToolCalls,
		ToolResults:       stats.ToolResults,
		TotalMessages:     stats.TotalMessages,
		Tokens: sessionStatsTokensJSON{
			Input:      stats.Tokens.Input,
			Output:     stats.Tokens.Output,
			CacheRead:  stats.Tokens.CacheRead,
			CacheWrite: stats.Tokens.CacheWrite,
			Total:      stats.Tokens.Total,
		},
		Cost: stats.Cost,
	}
	if stats.ContextUsage != nil {
		out.ContextUsage = &sessionStatsContextUsageJSON{
			Tokens:        stats.ContextUsage.Tokens,
			ContextWindow: stats.ContextUsage.ContextWindow,
			Percent:       stats.ContextUsage.Percent,
		}
	}
	return out
}

type availableModelsJSON struct {
	Models []ai.Model `json:"models"`
}

type cycleModelJSON struct {
	Model         ai.Model `json:"model"`
	ThinkingLevel string   `json:"thinkingLevel"`
	IsScoped      bool     `json:"isScoped"`
}

type thinkingLevelJSON struct {
	Level string `json:"level"`
}

type sessionReplacementJSON struct {
	Cancelled bool `json:"cancelled"`
}

type forkJSON struct {
	Text      string `json:"text"`
	Cancelled bool   `json:"cancelled"`
}

type forkMessagesJSON struct {
	Messages []forkMessageJSON `json:"messages"`
}

type forkMessageJSON struct {
	EntryID string `json:"entryId"`
	Text    string `json:"text"`
}

type entriesJSON struct {
	Entries []entrySummaryJSON `json:"entries"`
}

type entrySummaryJSON struct {
	EntryID string `json:"entryId"`
	Label   string `json:"label"`
}

type lastAssistantTextJSON struct {
	Text *string `json:"text"`
}

type slashCommandsJSON struct {
	Commands []slashCommandJSON `json:"commands"`
}

type commandNotificationsJSON struct {
	Notifications []commandNotificationJSON `json:"notifications"`
}

type commandNotificationJSON struct {
	Message string `json:"message"`
	Level   string `json:"level"`
}

type slashCommandJSON struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	ArgumentHint string         `json:"argumentHint,omitempty"`
	Source       string         `json:"source"`
	SourceInfo   sourceInfoJSON `json:"sourceInfo"`
}

type sourceInfoJSON struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	BaseDir string `json:"baseDir,omitempty"`
}

func slashCommandJSONFromOrchestrator(command orchestrator.SlashCommand) slashCommandJSON {
	return slashCommandJSON{
		Name:         command.Name,
		Description:  command.Description,
		ArgumentHint: command.ArgumentHint,
		Source:       command.Source,
		SourceInfo:   sourceInfoJSONFromResource(command.SourceInfo),
	}
}

func commandNotificationsJSONFromOrchestrator(
	notifications []orchestrator.CommandNotification,
) commandNotificationsJSON {
	out := make([]commandNotificationJSON, 0, len(notifications))
	for _, notification := range notifications {
		if notification.Message == "" {
			continue
		}
		out = append(out, commandNotificationJSON{
			Message: notification.Message,
			Level:   normalizeCommandNotificationLevel(notification.Level),
		})
	}
	return commandNotificationsJSON{Notifications: out}
}

func normalizeCommandNotificationLevel(level string) string {
	switch level {
	case "warning", "error":
		return level
	default:
		return "info"
	}
}

func sourceInfoJSONFromResource(info resource.SourceInfo) sourceInfoJSON {
	return sourceInfoJSON{
		Kind:    string(info.Kind),
		Path:    info.Path,
		BaseDir: info.BaseDir,
	}
}

// navResultJSON mirrors orchestrator.NavResult (harness.NavigationResult).
type navResultJSON struct {
	Cancelled  bool   `json:"cancelled"`
	EditorText string `json:"editorText,omitempty"`
}

func navResultJSONFromOrchestrator(result orchestrator.NavResult) navResultJSON {
	return navResultJSON{
		Cancelled:  result.Cancelled,
		EditorText: result.EditorText,
	}
}

// compactionJSON mirrors pi CompactionResult (compaction.ts:103).
type compactionJSON struct {
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int    `json:"tokensBefore"`
	Details          any    `json:"details,omitempty"`
}
