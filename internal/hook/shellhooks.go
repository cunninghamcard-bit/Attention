package hook

// shellhooks.go is a declarative shell-hooks runner: a hooks.json file maps a
// lifecycle event to a shell command; the command receives the hook Event as
// JSON on stdin and may print a JSON {decision} on stdout to allow/block/mutate.
//
// It lives in package hook (not a separate package) for two reasons:
//   - it must return the EXACT concrete result types the pipeline folds and the
//     harness/orchestrator emit sites type-assert on (ToolCallResult for
//     tool_call, ToolResultPatch for tool_result, BeforeAgentStartResult for
//     before_agent_start, SessionBeforeCompactResult for session_before_compact,
//     ...); a pointer or map is silently ignored by those strict type
//     assertions, the single most likely silent-failure bug.
//   - it reuses the Event* constants without an import cycle.
//
// Every event in events.go is wired here EXCEPT queue_update: hook.QueueUpdateEvent
// is declared but never emitted through any hook.Registry (the orchestrator
// publishes its own orchestrator.Event for queue changes), so a shell hook on it
// would never fire. It is the single genuinely-inert event and is warned about
// at registration.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
)

// defaultShellHookTimeout bounds a single hook command invocation when the
// hooks.json entry omits timeoutMs.
const defaultShellHookTimeout = 30 * time.Second

// shellHookEntry is one rule from hooks.json.
//
//	[
//	  {"event": "PreToolUse",  "toolName": "Bash", "command": "./deny-rm.sh", "timeoutMs": 5000},
//	  {"event": "PostToolUse", "command": "./scrub-secrets.sh"}
//	]
//
// event uses either the ecosystem name or the native hook Event* constant; it
// is translated to a native constant via the alias map. toolName,
// when set, is a filepath.Match glob filtered against the event's ToolName
// (tool_call / tool_result only); empty => all tools / all events.
type shellHookEntry struct {
	Event     string   `json:"event"`
	ToolName  string   `json:"toolName,omitempty"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	TimeoutMs int      `json:"timeoutMs,omitempty"`
	Timeout   int      `json:"timeout,omitempty"`
}

type ShellHookInputFormat string

const (
	ShellHookInputNative ShellHookInputFormat = ""
	ShellHookInputPlugin ShellHookInputFormat = "plugin"
)

type ShellHooksOptions struct {
	Path        string
	CWD         string
	Env         map[string]string
	InputFormat ShellHookInputFormat
}

type groupedHookConfig struct {
	Hooks map[string][]groupedHookMatcher `json:"hooks"`
}

type groupedHookMatcher struct {
	Matcher string               `json:"matcher"`
	Hooks   []groupedHookCommand `json:"hooks"`
}

type groupedHookCommand struct {
	Type      string   `json:"type"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	TimeoutMs int      `json:"timeoutMs,omitempty"`
	Timeout   int      `json:"timeout,omitempty"`
}

// shellHookDecision is the JSON a hook command prints on stdout. It is a unified
// envelope: a command targeting any event populates the fields meaningful for
// that event and leaves the rest empty.
//
//	{"decision": "block", "reason": "rm -rf is not allowed"}        # tool_call / tool_result
//	{"decision": "allow", "input": {"path": "/safe"}}               # tool_call
//	{"content": [...], "isError": true, "terminate": true}          # tool_result
//	{"systemPrompt": "..."}                                         # before_agent_start
//	{"messages": [...]}                                             # context
//	{"action": "transform", "text": "..."}                          # input
//	{"cancel": true}                                                # session_before_*
//	{"skillPaths": ["..."]}                                         # resources_discover
//
// Empty stdout, "null", or a parse miss => no opinion (allow / no-change).
//
// The structured payload-shaped fields (messages/payload/message/streamOptions)
// map to result types whose Go field is `any`; they are decoded generically.
type shellHookDecision struct {
	// Common control fields.
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`

	// tool_call.
	Input map[string]any `json:"input,omitempty"`

	// tool_result.
	Content   json.RawMessage `json:"content,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
	IsError   *bool           `json:"isError,omitempty"`
	Terminate *bool           `json:"terminate,omitempty"`

	// before_agent_start.
	SystemPrompt *string `json:"systemPrompt,omitempty"`

	// before_agent_start / context: a list of messages (each `any`).
	Messages json.RawMessage `json:"messages,omitempty"`

	// input.
	Action string         `json:"action,omitempty"`
	Text   string         `json:"text,omitempty"`
	Images []ImageContent `json:"images,omitempty"`

	// before_provider_request.
	StreamOptions json.RawMessage `json:"streamOptions,omitempty"`

	// before_provider_payload.
	Payload json.RawMessage `json:"payload,omitempty"`

	// message_end.
	Message json.RawMessage `json:"message,omitempty"`

	// session_before_switch / session_before_fork / session_before_compact /
	// session_before_tree.
	Cancel                  bool `json:"cancel,omitempty"`
	SkipConversationRestore bool `json:"skipConversationRestore,omitempty"`

	// session_before_compact override.
	Compaction *CompactionResult `json:"compaction,omitempty"`

	// session_before_tree override.
	Summary             *BranchSummaryResult `json:"summary,omitempty"`
	CustomInstructions  *string              `json:"customInstructions,omitempty"`
	ReplaceInstructions *bool                `json:"replaceInstructions,omitempty"`
	Label               *string              `json:"label,omitempty"`

	// resources_discover.
	SkillPaths  []string `json:"skillPaths,omitempty"`
	PromptPaths []string `json:"promptPaths,omitempty"`
	ThemePaths  []string `json:"themePaths,omitempty"`
}

type pluginHookResponse struct {
	Decision           string                   `json:"decision,omitempty"`
	Reason             string                   `json:"reason,omitempty"`
	HookSpecificOutput pluginHookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type pluginHookSpecificOutput struct {
	HookEventName            string          `json:"hookEventName,omitempty"`
	UpdatedInput             map[string]any  `json:"updatedInput,omitempty"`
	UpdatedToolOutput        json.RawMessage `json:"updatedToolOutput,omitempty"`
	PermissionDecision       string          `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string          `json:"permissionDecisionReason,omitempty"`
	Content                  json.RawMessage `json:"content,omitempty"`
	IsError                  *bool           `json:"isError,omitempty"`
}

// resolvedHook is a parsed, ready-to-register rule.
type resolvedHook struct {
	nativeEvent string
	toolGlob    string
	command     string
	args        []string
	timeout     time.Duration
	cwd         string
	env         map[string]string
	inputFormat ShellHookInputFormat
}

// ShellHooksRunner registers declarative shell-hook handlers on a Registry.
type ShellHooksRunner struct {
	hooks []resolvedHook
}

type ShellHookHandler struct {
	EventType string
	Handle    func(context.Context, any, string) (any, error)
}

// decisionEvents are the events whose handler result influences behavior. The
// makeHandler dispatch returns the EXACT concrete result type each emit site
// type-asserts for these.
var decisionEvents = map[string]bool{
	EventBeforeAgentStart:      true,
	EventContext:               true,
	EventInput:                 true,
	EventBeforeProviderRequest: true,
	EventBeforeProviderPayload: true,
	EventToolCall:              true,
	EventToolResult:            true,
	EventMessageEnd:            true,
	EventSessionBeforeSwitch:   true,
	EventSessionBeforeFork:     true,
	EventSessionBeforeCompact:  true,
	EventSessionBeforeTree:     true,
	EventResourcesDiscover:     true,
}

// eventAliases maps every hooks.json "event" name to its native hook Event*
// constant. It accepts BOTH the native event name (so any kernel event can be
// targeted by its real name) AND ecosystem aliases (PreToolUse, PostToolUse,
// UserPromptSubmit, SessionStart, Stop, ...).
//
// Every native event in events.go that has a live emit site is reachable here.
// queue_update is intentionally accepted too (so a typo is not the silent cause
// of nothing happening) but warned about at registration as genuinely inert.
var eventAliases = map[string]string{
	// Ecosystem aliases.
	"PreToolUse":       EventToolCall,
	"PostToolUse":      EventToolResult,
	"UserPromptSubmit": EventInput,
	"SessionStart":     EventBeforeAgentStart,
	"Stop":             EventSettled,
	"SubagentStop":     EventAgentEnd,
	"PreCompact":       EventSessionBeforeCompact,

	// Native decision events.
	EventBeforeAgentStart:      EventBeforeAgentStart,
	EventContext:               EventContext,
	EventInput:                 EventInput,
	EventBeforeProviderRequest: EventBeforeProviderRequest,
	EventBeforeProviderPayload: EventBeforeProviderPayload,
	EventToolCall:              EventToolCall,
	EventToolResult:            EventToolResult,
	EventMessageEnd:            EventMessageEnd,
	EventSessionBeforeSwitch:   EventSessionBeforeSwitch,
	EventSessionBeforeFork:     EventSessionBeforeFork,
	EventSessionBeforeCompact:  EventSessionBeforeCompact,
	EventSessionBeforeTree:     EventSessionBeforeTree,
	EventResourcesDiscover:     EventResourcesDiscover,

	// Native notification events.
	EventAgentStart:            EventAgentStart,
	EventAgentEnd:              EventAgentEnd,
	EventTurnStart:             EventTurnStart,
	EventTurnEnd:               EventTurnEnd,
	EventMessageStart:          EventMessageStart,
	EventMessageUpdate:         EventMessageUpdate,
	EventToolExecutionStart:    EventToolExecutionStart,
	EventToolExecutionUpdate:   EventToolExecutionUpdate,
	EventToolExecutionEnd:      EventToolExecutionEnd,
	EventAfterProviderResponse: EventAfterProviderResponse,
	EventSessionStart:          EventSessionStart,
	EventSessionCompact:        EventSessionCompact,
	EventSessionTree:           EventSessionTree,
	EventSessionShutdown:       EventSessionShutdown,
	EventModelSelect:           EventModelSelect,
	EventThinkingLevelSelect:   EventThinkingLevelSelect,
	EventResourcesUpdate:       EventResourcesUpdate,
	EventQueueUpdate:           EventQueueUpdate,
	EventSavePoint:             EventSavePoint,
	EventAbort:                 EventAbort,
	EventSettled:               EventSettled,
}

// inertEvents are native events with no live Emit call site through any
// hook.Registry; a hook registered on one of these compiles and registers but
// never fires. queue_update is the only such event: hook.QueueUpdateEvent is
// declared in events.go but the orchestrator publishes its own orchestrator
// Event for queue changes rather than emitting hook.QueueUpdateEvent.
var inertEvents = map[string]bool{
	EventQueueUpdate: true,
}

// LoadShellHooks reads and parses a hooks.json file. A missing file or an empty
// rule set returns (nil, nil) so the caller preserves today's no-hooks
// behavior. A malformed file returns an error.
func LoadShellHooks(path string) (*ShellHooksRunner, error) {
	return LoadShellHooksWithOptions(ShellHooksOptions{Path: path})
}

func LoadShellHooksWithOptions(opts ShellHooksOptions) (*ShellHooksRunner, error) {
	if opts.Path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(opts.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("hook: read hooks file %q: %w", opts.Path, err)
	}
	return LoadShellHooksData(data, opts)
}

func LoadShellHooksData(data []byte, opts ShellHooksOptions) (*ShellHooksRunner, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}

	entries, err := parseShellHookEntries(data)
	if err != nil {
		return nil, fmt.Errorf("hook: parse hooks file %q: %w", opts.Path, err)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	resolved := make([]resolvedHook, 0, len(entries))
	for i, e := range entries {
		if strings.TrimSpace(e.Command) == "" {
			return nil, fmt.Errorf("hook: hooks[%d] (%q) missing command", i, e.Event)
		}
		native, ok := eventAliases[e.Event]
		if !ok {
			return nil, fmt.Errorf("hook: hooks[%d] unknown event %q", i, e.Event)
		}
		timeout := shellHookTimeout(e)
		resolved = append(resolved, resolvedHook{
			nativeEvent: native,
			toolGlob:    e.ToolName,
			command:     e.Command,
			args:        append([]string(nil), e.Args...),
			timeout:     timeout,
			cwd:         opts.CWD,
			env:         mapsClone(opts.Env),
			inputFormat: opts.InputFormat,
		})
	}
	return &ShellHooksRunner{hooks: resolved}, nil
}

func parseShellHookEntries(data []byte) ([]shellHookEntry, error) {
	var entries []shellHookEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		return entries, nil
	}

	var cfg groupedHookConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	entries = []shellHookEntry{}
	for event, groups := range cfg.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				if hook.Type != "" && hook.Type != "command" {
					continue
				}
				entries = append(entries, shellHookEntry{
					Event:     event,
					ToolName:  group.Matcher,
					Command:   hook.Command,
					Args:      append([]string(nil), hook.Args...),
					TimeoutMs: hook.TimeoutMs,
					Timeout:   hook.Timeout,
				})
			}
		}
	}
	return entries, nil
}

func shellHookTimeout(e shellHookEntry) time.Duration {
	if e.TimeoutMs > 0 {
		return time.Duration(e.TimeoutMs) * time.Millisecond
	}
	if e.Timeout > 0 {
		return time.Duration(e.Timeout) * time.Second
	}
	return defaultShellHookTimeout
}

func mapsClone(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// HasHandlers reports whether any rules were loaded.
func (r *ShellHooksRunner) HasHandlers() bool {
	return r != nil && len(r.hooks) > 0
}

// Register attaches one Registry handler per loaded rule. The handler marshals
// the event to JSON, runs the command with a ctx-bounded timeout (stdin = event
// JSON), and parses stdout into the CONCRETE result type the consumer expects.
func (r *ShellHooksRunner) Register(reg *Registry, sessionID string) {
	if r == nil || reg == nil {
		return
	}
	for _, handler := range r.Handlers() {
		handler := handler
		reg.On(handler.EventType, func(ctx context.Context, event any) (any, error) {
			return handler.Handle(ctx, event, sessionID)
		})
	}
}

func (r *ShellHooksRunner) Handlers() []ShellHookHandler {
	if r == nil {
		return nil
	}
	handlers := make([]ShellHookHandler, 0, len(r.hooks))
	for _, h := range r.hooks {
		if inertEvents[h.nativeEvent] {
			fmt.Fprintf(os.Stderr,
				"hook: shell hook on %q registered but INERT (no emit site through hook.Registry; it will never fire)\n",
				h.nativeEvent)
		}
		h := h
		handlers = append(handlers, ShellHookHandler{
			EventType: h.nativeEvent,
			Handle: func(ctx context.Context, event any, sessionID string) (any, error) {
				return r.handle(ctx, h, event, sessionID)
			},
		})
	}
	return handlers
}

func (r *ShellHooksRunner) handle(ctx context.Context, h resolvedHook, event any, sessionID string) (any, error) {
	// Tool-name glob filter (tool_call / tool_result only; other events have
	// no ToolName and are unaffected).
	if h.toolGlob != "" {
		name := eventToolName(event)
		if name == "" {
			return nil, nil
		}
		if !toolNameMatches(h.toolGlob, name) {
			return nil, nil
		}
	}

	// Notification events run the command for side effects and never shape a
	// result. Run unconditionally (no decision parse needed) and return
	// (nil, nil).
	if !decisionEvents[h.nativeEvent] {
		r.runCommandNotify(ctx, h, sessionID, event)
		return nil, nil
	}

	decision, ran := r.runCommand(ctx, h, sessionID, event)
	if !ran {
		// Exec/parse error or no opinion => allow / no-change default.
		return nil, nil
	}

	switch h.nativeEvent {
	case EventToolCall:
		return toolCallResultFrom(decision), nil
	case EventToolResult:
		return toolResultPatchFrom(decision), nil
	case EventBeforeAgentStart:
		return beforeAgentStartResultFrom(decision), nil
	case EventContext:
		return contextResultFrom(decision), nil
	case EventInput:
		return inputResultFrom(decision), nil
	case EventBeforeProviderRequest:
		return beforeProviderRequestResultFrom(decision), nil
	case EventBeforeProviderPayload:
		return beforeProviderPayloadResultFrom(decision), nil
	case EventMessageEnd:
		return messageEndResultFrom(decision), nil
	case EventSessionBeforeSwitch:
		return sessionBeforeSwitchResultFrom(decision), nil
	case EventSessionBeforeFork:
		return sessionBeforeForkResultFrom(decision), nil
	case EventSessionBeforeCompact:
		return sessionBeforeCompactResultFrom(decision), nil
	case EventSessionBeforeTree:
		return sessionBeforeTreeResultFrom(decision), nil
	case EventResourcesDiscover:
		return resourcesDiscoverResultFrom(decision), nil
	default:
		// Should not happen: decisionEvents and this switch are kept in sync.
		return nil, nil
	}
}

// runCommandNotify runs a notification-event hook command for side effects,
// discarding stdout. Errors are swallowed (a failing notify hook must not abort
// the emitting operation).
func (r *ShellHooksRunner) runCommandNotify(
	ctx context.Context,
	h resolvedHook,
	sessionID string,
	event any,
) {
	_, _ = r.exec(ctx, h, sessionID, event)
}

// runCommand marshals the event, runs the hook command with stdin = event JSON,
// and parses stdout. ran=false means allow/no-change (empty stdout, parse miss,
// or exec error — allow-on-error default).
func (r *ShellHooksRunner) runCommand(
	ctx context.Context,
	h resolvedHook,
	sessionID string,
	event any,
) (shellHookDecision, bool) {
	out, err := r.exec(ctx, h, sessionID, event)
	if err != nil {
		// Allow-on-error: a failing hook does not block the tool. (Deny-on-error
		// would be a documented config knob; not enabled here.)
		return shellHookDecision{}, false
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return shellHookDecision{}, false
	}

	var decision shellHookDecision
	if err := json.Unmarshal([]byte(trimmed), &decision); err == nil && !isZeroDecision(decision) {
		return decision, true
	}

	pluginDecision, ok := pluginDecisionFromJSON([]byte(trimmed))
	if !ok {
		return shellHookDecision{}, false
	}
	return pluginDecision, true
}

func isZeroDecision(d shellHookDecision) bool {
	return d.Decision == "" &&
		d.Reason == "" &&
		d.Input == nil &&
		len(d.Content) == 0 &&
		len(d.Details) == 0 &&
		d.IsError == nil &&
		d.Terminate == nil &&
		d.SystemPrompt == nil &&
		len(d.Messages) == 0 &&
		d.Action == "" &&
		d.Text == "" &&
		len(d.Images) == 0 &&
		len(d.StreamOptions) == 0 &&
		len(d.Payload) == 0 &&
		len(d.Message) == 0 &&
		!d.Cancel &&
		!d.SkipConversationRestore &&
		d.Compaction == nil &&
		d.Summary == nil &&
		d.CustomInstructions == nil &&
		d.ReplaceInstructions == nil &&
		d.Label == nil &&
		len(d.SkillPaths) == 0 &&
		len(d.PromptPaths) == 0 &&
		len(d.ThemePaths) == 0
}

func pluginDecisionFromJSON(data []byte) (shellHookDecision, bool) {
	var resp pluginHookResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return shellHookDecision{}, false
	}
	out := shellHookDecision{
		Decision: resp.Decision,
		Reason:   resp.Reason,
	}
	hookOut := resp.HookSpecificOutput
	if hookOut.UpdatedInput != nil {
		out.Input = hookOut.UpdatedInput
	}
	if hookOut.PermissionDecision != "" {
		out.Decision = hookOut.PermissionDecision
	}
	if hookOut.PermissionDecisionReason != "" {
		out.Reason = hookOut.PermissionDecisionReason
	}
	if content, ok := pluginUpdatedToolOutputContent(hookOut.UpdatedToolOutput); ok {
		out.Content = content
	}
	if len(hookOut.Content) > 0 {
		out.Content = hookOut.Content
	}
	if hookOut.IsError != nil {
		out.IsError = hookOut.IsError
	}
	if isZeroDecision(out) {
		return shellHookDecision{}, false
	}
	return out, true
}

func pluginUpdatedToolOutputContent(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		content, err := json.Marshal([]ai.ContentBlock{{Type: ai.ContentText, Text: text}})
		return content, err == nil
	}
	if strings.HasPrefix(trimmed, "[") {
		return raw, true
	}
	var wrapped struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Content) > 0 {
		return wrapped.Content, true
	}
	return nil, false
}

// exec runs the hook command with stdin = event JSON and returns its stdout.
func (r *ShellHooksRunner) exec(
	ctx context.Context,
	h resolvedHook,
	sessionID string,
	event any,
) ([]byte, error) {
	input := event
	if h.inputFormat == ShellHookInputPlugin {
		input = pluginHookInput(h.nativeEvent, event, sessionID)
	}
	eventJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	cmd := shellHookExecCommand(cctx, h, h.env)
	if h.cwd != "" {
		cmd.Dir = h.cwd
	}
	cmd.Stdin = strings.NewReader(string(eventJSON))
	cmd.Env = mergeHookEnv(h.env, sessionID)
	return cmd.Output()
}

func mergeHookEnv(extra map[string]string, sessionID string) []string {
	env := os.Environ()
	seen := map[string]bool{}
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			seen[key] = true
		}
	}
	set := func(key string, value string) {
		entry := key + "=" + value
		if !seen[key] {
			env = append(env, entry)
			seen[key] = true
			return
		}
		for i, existing := range env {
			if strings.HasPrefix(existing, key+"=") {
				env[i] = entry
				return
			}
		}
	}
	for key, value := range extra {
		set(key, value)
	}
	set("ALONG_SESSION_ID", sessionID)
	return env
}

func replaceHookEnv(text string, env map[string]string) string {
	for key, value := range env {
		text = strings.ReplaceAll(text, "${"+key+"}", value)
		text = strings.ReplaceAll(text, "$"+key, value)
	}
	return text
}

func shellHookExecCommand(ctx context.Context, h resolvedHook, env map[string]string) *exec.Cmd {
	command := replaceHookEnv(h.command, env)
	if len(h.args) == 0 {
		return exec.CommandContext(ctx, "sh", "-c", command)
	}
	args := make([]string, 0, len(h.args))
	for _, arg := range h.args {
		args = append(args, replaceHookEnv(arg, env))
	}
	return exec.CommandContext(ctx, command, args...)
}

func pluginHookInput(nativeEvent string, event any, sessionID string) any {
	base := map[string]any{
		"hook_event_name": nativeToPluginHookEvent(nativeEvent),
		"session_id":      sessionID,
	}
	switch ev := event.(type) {
	case ToolCallEvent:
		base["tool_name"] = nativeToPluginToolName(ev.ToolName)
		base["tool_input"] = ev.Input
		base["tool_call_id"] = ev.ToolCallId
	case ToolResultEvent:
		base["tool_name"] = nativeToPluginToolName(ev.ToolName)
		base["tool_input"] = ev.Input
		base["tool_response"] = map[string]any{
			"content":  ev.Content,
			"details":  ev.Details,
			"is_error": ev.IsError,
		}
		base["tool_call_id"] = ev.ToolCallId
	case InputEvent:
		base["prompt"] = ev.Text
	case SessionBeforeCompactEvent:
		base["custom_instructions"] = ev.CustomInstructions
	case SessionStartEvent:
		base["source"] = ev.Reason
	}
	return base
}

func nativeToPluginHookEvent(native string) string {
	switch native {
	case EventToolCall:
		return "PreToolUse"
	case EventToolResult:
		return "PostToolUse"
	case EventInput:
		return "UserPromptSubmit"
	case EventBeforeAgentStart, EventSessionStart:
		return "SessionStart"
	case EventSessionBeforeCompact:
		return "PreCompact"
	case EventSettled:
		return "Stop"
	default:
		return native
	}
}

func nativeToPluginToolName(name string) string {
	switch strings.ToLower(name) {
	case "bash":
		return "Bash"
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "edit":
		return "Edit"
	case "grep":
		return "Grep"
	case "find":
		return "Glob"
	case "ls":
		return "LS"
	default:
		return name
	}
}

// --- Decision result converters. Each returns the EXACT concrete result type
// the corresponding emit site type-asserts. ---

// toolCallResultFrom builds the CONCRETE ToolCallResult prompt.go's
// BeforeToolCall fold type-asserts. block/deny => Block:true; an "input"
// mutation threads new args.
func toolCallResultFrom(d shellHookDecision) ToolCallResult {
	return ToolCallResult{
		Block:  isBlock(d.Decision),
		Reason: d.Reason,
		Input:  d.Input,
	}
}

// toolResultPatchFrom builds the CONCRETE ToolResultPatch prompt.go's
// AfterToolCall fold type-asserts. block/deny => Terminate+IsError; content /
// details / isError / terminate fields patch directly.
func toolResultPatchFrom(d shellHookDecision) ToolResultPatch {
	patch := ToolResultPatch{}
	if isBlock(d.Decision) {
		term := true
		patch.Terminate = &term
		isErr := true
		patch.IsError = &isErr
	}
	if len(d.Content) > 0 {
		var blocks []ai.ContentBlock
		if err := json.Unmarshal(d.Content, &blocks); err == nil {
			patch.Content = blocks
		}
	}
	if len(d.Details) > 0 {
		var details any
		if err := json.Unmarshal(d.Details, &details); err == nil {
			patch.Details = details
		}
	}
	if d.IsError != nil {
		patch.IsError = d.IsError
	}
	if d.Terminate != nil {
		patch.Terminate = d.Terminate
	}
	return patch
}

// beforeAgentStartResultFrom builds the CONCRETE BeforeAgentStartResult
// emitBeforeAgentStart type-asserts (prompt.go). systemPrompt replaces the
// running system prompt; messages are decoded into concrete ai.Message VALUES
// (which implement message.AgentMessage via ai.Message.IsAgentMessage). The
// harness keeps only []any elements that satisfy message.AgentMessage
// (prompt.go:153-157), so a raw map[string]any would be dropped — decoding to
// ai.Message is what makes injected messages survive.
func beforeAgentStartResultFrom(d shellHookDecision) BeforeAgentStartResult {
	return BeforeAgentStartResult{
		Messages:     decodeHookMessages(d.Messages),
		SystemPrompt: d.SystemPrompt,
	}
}

// contextResultFrom builds the CONCRETE ContextResult TransformContext
// type-asserts (prompt.go). Only a non-nil Messages slice is treated as a
// change by the harness; fromAnySlice (prompt.go:1024-1032) keeps only
// message.AgentMessage values, so we decode into concrete ai.Message values
// (not raw maps, which would be dropped).
func contextResultFrom(d shellHookDecision) ContextResult {
	return ContextResult{
		Messages: decodeHookMessages(d.Messages),
	}
}

// inputResultFrom builds the CONCRETE InputResult emitInput type-asserts
// (orchestrator.go). Action "handled" short-circuits the turn; "transform"
// rewrites text/images.
func inputResultFrom(d shellHookDecision) InputResult {
	return InputResult{
		Action: d.Action,
		Text:   d.Text,
		Images: d.Images,
	}
}

// beforeProviderRequestResultFrom builds the CONCRETE
// BeforeProviderRequestResult emitBeforeProviderRequest type-asserts
// (prompt.go). applyStreamOptionsPatch (prompt.go:844-872) switches only on
// concrete ai.SimpleStreamOptions / hook.StreamOptionsPatch values; a raw
// map[string]any no-ops. So we decode d.StreamOptions into a concrete
// StreamOptionsPatch (its pointer fields make a partial JSON patch
// unambiguous). On empty/invalid input StreamOptions stays nil (no change).
func beforeProviderRequestResultFrom(d shellHookDecision) BeforeProviderRequestResult {
	res := BeforeProviderRequestResult{}
	if patch, ok := decodeStreamOptionsPatch(d.StreamOptions); ok {
		res.StreamOptions = patch
	}
	return res
}

// beforeProviderPayloadResultFrom builds the CONCRETE BeforeProviderPayloadResult
// emitBeforeProviderPayload type-asserts (prompt.go). Payload is the new payload.
func beforeProviderPayloadResultFrom(d shellHookDecision) BeforeProviderPayloadResult {
	return BeforeProviderPayloadResult{
		Payload: rawAny(d.Payload),
	}
}

// messageEndResultFrom builds the CONCRETE MessageEndResult
// emitMessageEndChain type-asserts (prompt.go). resultAgentMessage does
// value.(message.AgentMessage), so a raw map[string]any fails and the original
// message is kept. We decode into a concrete ai.Message value (which satisfies
// message.AgentMessage). NOTE: the harness requires the replacement to carry
// the SAME role as the streamed message (sameMessageEndRole, prompt.go), so a
// hook replacing an assistant turn must emit role:"assistant". When the decode
// yields no message, Message stays nil (no change).
func messageEndResultFrom(d shellHookDecision) MessageEndResult {
	res := MessageEndResult{}
	if msg, ok := decodeHookMessage(d.Message); ok {
		res.Message = msg
	}
	return res
}

// sessionBeforeSwitchResultFrom builds the CONCRETE SessionBeforeSwitchResult
// emitSessionBeforeSwitch type-asserts (session_runtime.go).
func sessionBeforeSwitchResultFrom(d shellHookDecision) SessionBeforeSwitchResult {
	return SessionBeforeSwitchResult{
		Cancel: d.Cancel,
	}
}

// sessionBeforeForkResultFrom builds the CONCRETE SessionBeforeForkResult
// emitSessionBeforeFork type-asserts (session_runtime.go).
func sessionBeforeForkResultFrom(d shellHookDecision) SessionBeforeForkResult {
	return SessionBeforeForkResult{
		Cancel:                  d.Cancel,
		SkipConversationRestore: d.SkipConversationRestore,
	}
}

// sessionBeforeCompactResultFrom builds the CONCRETE SessionBeforeCompactResult
// the Compact emit site type-asserts (compact.go). cancel cancels compaction; a
// "compaction" object supplies a hook-provided compaction result.
func sessionBeforeCompactResultFrom(d shellHookDecision) SessionBeforeCompactResult {
	return SessionBeforeCompactResult{
		Cancel:     d.Cancel,
		Compaction: d.Compaction,
	}
}

// sessionBeforeTreeResultFrom builds the CONCRETE SessionBeforeTreeResult the
// NavigateTree emit site type-asserts (navigate.go). cancel cancels navigation;
// summary/customInstructions/replaceInstructions/label override the branch
// summary behavior.
func sessionBeforeTreeResultFrom(d shellHookDecision) SessionBeforeTreeResult {
	return SessionBeforeTreeResult{
		Cancel:              d.Cancel,
		Summary:             d.Summary,
		CustomInstructions:  d.CustomInstructions,
		ReplaceInstructions: d.ReplaceInstructions,
		Label:               d.Label,
	}
}

// resourcesDiscoverResultFrom builds the CONCRETE ResourcesDiscoverResult the
// resources_discover emit site type-asserts (orchestrator.go).
func resourcesDiscoverResultFrom(d shellHookDecision) ResourcesDiscoverResult {
	return ResourcesDiscoverResult{
		SkillPaths:  d.SkillPaths,
		PromptPaths: d.PromptPaths,
		ThemePaths:  d.ThemePaths,
	}
}

// hookMessageJSON is the per-message wire shape a hook command prints for the
// before_agent_start / context / message_end events. It is deliberately lax:
//
//	{"role": "user", "text": "hi"}
//	{"role": "assistant", "content": [{"type":"text","text":"rewritten"}]}
//
// "text" is sugar for a single text content block; "content" (when present) is
// decoded into ai.ContentBlock values. timestamp is optional.
type hookMessageJSON struct {
	Role      string            `json:"role"`
	Text      string            `json:"text"`
	Content   []ai.ContentBlock `json:"content"`
	Timestamp int64             `json:"timestamp"`
}

// decodeHookMessage unmarshals one per-message JSON object into a concrete
// ai.Message. ai.Message implements message.AgentMessage (ai.Message
// .IsAgentMessage), so the value survives the harness type assertions that drop
// raw maps.
//
// Rules:
//   - role defaults to RoleUser. ai.Role has no system role (RoleUser /
//     RoleAssistant / RoleToolResult only), so "system" is coerced to RoleUser.
//   - a top-level "text" string is sugar for one ContentText block; an explicit
//     "content" array takes precedence and a "text" block is appended after it
//     so both can be supplied (content first, text sugar last).
//   - Timestamp defaults to time.Now().UnixMilli() when zero.
//
// Returns (zero, false) when raw is empty or the message carries neither text
// nor content (nothing to inject).
func decodeHookMessage(raw json.RawMessage) (ai.Message, bool) {
	if len(raw) == 0 {
		return ai.Message{}, false
	}
	var m hookMessageJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		return ai.Message{}, false
	}

	blocks := append([]ai.ContentBlock(nil), m.Content...)
	if m.Text != "" {
		blocks = append(blocks, ai.ContentBlock{Type: ai.ContentText, Text: m.Text})
	}
	if len(blocks) == 0 {
		return ai.Message{}, false
	}

	msg := ai.Message{
		Role:      hookRole(m.Role),
		Content:   blocks,
		Timestamp: m.Timestamp,
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	return msg, true
}

// decodeHookMessages unmarshals a JSON array of per-message objects, decodes
// each into an ai.Message, and collects the VALUES into a []any (so each
// element satisfies message.AgentMessage). Returns nil when raw is empty or no
// element decoded, so "no opinion" stays a no-change: a nil Messages slice must
// not be treated as a change by the harness.
func decodeHookMessages(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil
	}
	var out []any
	for _, e := range elems {
		if msg, ok := decodeHookMessage(e); ok {
			out = append(out, msg)
		}
	}
	return out
}

// hookRole maps a hook's role string onto an ai.Role. ai.Role has no system
// role, so "system" (and any unrecognized value) coerces to RoleUser.
func hookRole(role string) ai.Role {
	switch ai.Role(strings.TrimSpace(role)) {
	case ai.RoleAssistant:
		return ai.RoleAssistant
	case ai.RoleToolResult:
		return ai.RoleToolResult
	default:
		// "user", "system" (no system role exists), "" and anything else.
		return ai.RoleUser
	}
}

// decodeStreamOptionsPatch unmarshals d.StreamOptions into a concrete
// hook.StreamOptionsPatch (the only patch shape applyStreamOptionsPatch
// recognizes besides ai.SimpleStreamOptions). Its pointer fields keep a partial
// JSON object unambiguous (an omitted key stays a nil pointer = leave alone).
// Returns ok=false when raw is empty, invalid, or carries no set fields, so an
// empty/invalid patch leaves StreamOptions nil (no change).
func decodeStreamOptionsPatch(raw json.RawMessage) (StreamOptionsPatch, bool) {
	if len(raw) == 0 {
		return StreamOptionsPatch{}, false
	}
	var patch StreamOptionsPatch
	if err := json.Unmarshal(raw, &patch); err != nil {
		return StreamOptionsPatch{}, false
	}
	if streamOptionsPatchEmpty(patch) {
		return StreamOptionsPatch{}, false
	}
	return patch, true
}

// streamOptionsPatchEmpty reports whether no field of the patch is set, i.e.
// applying it would change nothing.
func streamOptionsPatchEmpty(p StreamOptionsPatch) bool {
	return p.Temperature == nil &&
		p.MaxTokens == nil &&
		p.APIKey == nil &&
		p.Transport == nil &&
		p.CacheRetention == nil &&
		p.SessionID == nil &&
		!p.ClearHeaders &&
		p.Headers == nil &&
		p.Timeout == nil &&
		p.MaxRetries == nil &&
		!p.ClearMetadata &&
		p.Metadata == nil &&
		p.Reasoning == nil &&
		p.ThinkingBudgets == nil
}

// rawAny decodes a raw JSON value into an any.
func rawAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

func isBlock(decision string) bool {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "block", "deny":
		return true
	default:
		return false
	}
}

// eventToolName reads ToolName off the concrete tool_call / tool_result events
// for glob filtering. Unknown event shapes yield "".
func eventToolName(event any) string {
	switch e := event.(type) {
	case ToolCallEvent:
		return e.ToolName
	case ToolResultEvent:
		return e.ToolName
	default:
		return ""
	}
}

func toolNameMatches(pattern string, name string) bool {
	for _, part := range strings.FieldsFunc(pattern, func(r rune) bool { return r == '|' || r == ',' }) {
		if part != pattern && toolNameMatches(strings.TrimSpace(part), name) {
			return true
		}
	}
	candidates := []string{name, nativeToPluginToolName(name), strings.ToLower(name)}
	if re := compileToolNameMatcher(pattern); re != nil {
		for _, candidate := range candidates {
			if re.MatchString(candidate) {
				return true
			}
		}
		return false
	}
	patterns := []string{pattern, strings.ToLower(pattern)}
	for _, pat := range patterns {
		for _, candidate := range candidates {
			if matched, _ := filepath.Match(pat, candidate); matched {
				return true
			}
		}
	}
	return false
}

func compileToolNameMatcher(pattern string) *regexp.Regexp {
	pattern = strings.TrimSpace(pattern)
	if !strings.HasPrefix(pattern, "/") {
		return nil
	}
	end := strings.LastIndex(pattern, "/")
	if end <= 0 {
		return nil
	}
	expr := pattern[1:end]
	flags := pattern[end+1:]
	if strings.Contains(flags, "i") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil
	}
	return re
}
