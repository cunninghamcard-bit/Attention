package hook

// Pure-Go tests for the declarative shell-hooks runner. NO node, NO JS host.
// A temp hooks.json + a tiny shell script drive the registry and we assert the
// handler returns the EXACT concrete result types the pipeline folds
// type-assert on (ToolCallResult / ToolResultPatch).

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/message"
)

// textOf returns the concatenated text of an ai.Message's text content blocks.
func textOf(m ai.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == ai.ContentText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// writeScript writes an executable shell script and returns a command string
// that runs it via the path. Using `sh <path>` keeps it portable.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return "sh " + p
}

func writeHooks(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}
	return p
}

func loadAndRegister(t *testing.T, path string) *Registry {
	t.Helper()
	runner, err := LoadShellHooks(path)
	if err != nil {
		t.Fatalf("LoadShellHooks: %v", err)
	}
	if !runner.HasHandlers() {
		t.Fatalf("runner has no handlers")
	}
	reg := NewRegistry()
	runner.Register(reg, "sess-1")
	return reg
}

func TestShellHooksPreToolUseBlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	cmd := writeScript(t, dir, "deny.sh", `echo '{"decision":"block","reason":"nope"}'`)
	hooks := writeHooks(t, dir, `[{"event":"PreToolUse","command":"`+cmd+`"}]`)

	reg := loadAndRegister(t, hooks)

	res, err := reg.Emit(context.Background(), ToolCallEvent{
		Type:       EventToolCall,
		ToolCallId: "call-1",
		ToolName:   "Bash",
		Input:      map[string]any{"command": "rm -rf /"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// CRITICAL: the fold does result.(hook.ToolCallResult); a pointer/map is
	// silently ignored. Prove the concrete value type.
	tc, ok := res.(ToolCallResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolCallResult", res)
	}
	if !tc.Block {
		t.Fatalf("Block = false, want true")
	}
	if tc.Reason != "nope" {
		t.Fatalf("Reason = %q, want nope", tc.Reason)
	}
}

func TestShellHooksPluginPreToolUseEnvelope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "stdin.json")
	cmd := writeScript(t, dir, "rewrite.sh", `cat > "`+inputPath+`"
echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"wrapped npm test"}}}'`)
	hooks := writeHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"`+cmd+`"}]}]}}`)

	runner, err := LoadShellHooksWithOptions(ShellHooksOptions{
		Path:        hooks,
		InputFormat: ShellHookInputPlugin,
	})
	if err != nil {
		t.Fatalf("LoadShellHooksWithOptions: %v", err)
	}
	reg := NewRegistry()
	runner.Register(reg, "sess-1")

	res, err := reg.Emit(context.Background(), ToolCallEvent{
		Type:       EventToolCall,
		ToolCallId: "call-1",
		ToolName:   "bash",
		Input:      map[string]any{"command": "npm test"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	tc, ok := res.(ToolCallResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolCallResult", res)
	}
	if tc.Input["command"] != "wrapped npm test" {
		t.Fatalf("Input = %#v, want rewritten command", tc.Input)
	}
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"hook_event_name":"PreToolUse"`, `"tool_name":"Bash"`, `"tool_input":{"command":"npm test"}`} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdin = %s, missing %s", text, want)
		}
	}
}

func TestShellHooksAllowOnEmptyStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	// Script prints nothing => no opinion => handler returns (nil, nil) => allow.
	cmd := writeScript(t, dir, "noop.sh", `exit 0`)
	hooks := writeHooks(t, dir, `[{"event":"PreToolUse","command":"`+cmd+`"}]`)

	reg := loadAndRegister(t, hooks)

	res, err := reg.Emit(context.Background(), ToolCallEvent{
		Type:     EventToolCall,
		ToolName: "Bash",
		Input:    map[string]any{"command": "ls"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if res != nil {
		t.Fatalf("result = %#v, want nil (allow/no-opinion)", res)
	}
}

func TestShellHooksAllowOnExecError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	// Non-zero exit (and stderr noise) => allow-on-error default => (nil, nil).
	cmd := writeScript(t, dir, "boom.sh", `echo oops 1>&2; exit 7`)
	hooks := writeHooks(t, dir, `[{"event":"PreToolUse","command":"`+cmd+`"}]`)

	reg := loadAndRegister(t, hooks)

	res, err := reg.Emit(context.Background(), ToolCallEvent{
		Type:     EventToolCall,
		ToolName: "Bash",
		Input:    map[string]any{"command": "ls"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if res != nil {
		t.Fatalf("result = %#v, want nil (allow-on-error)", res)
	}
}

func TestShellHooksToolCallInputMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	cmd := writeScript(t, dir, "mutate.sh",
		`echo '{"decision":"allow","input":{"command":"echo safe"}}'`)
	hooks := writeHooks(t, dir, `[{"event":"PreToolUse","command":"`+cmd+`"}]`)

	reg := loadAndRegister(t, hooks)

	res, err := reg.Emit(context.Background(), ToolCallEvent{
		Type:     EventToolCall,
		ToolName: "Bash",
		Input:    map[string]any{"command": "echo unsafe"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	tc, ok := res.(ToolCallResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolCallResult", res)
	}
	if tc.Block {
		t.Fatalf("Block = true, want false (allow + mutate)")
	}
	if got := tc.Input["command"]; got != "echo safe" {
		t.Fatalf("Input[command] = %v, want echo safe", got)
	}
}

func TestShellHooksToolNameGlobFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	cmd := writeScript(t, dir, "deny.sh", `echo '{"decision":"block","reason":"only bash"}'`)
	// Glob matches Bash* only.
	hooks := writeHooks(t, dir,
		`[{"event":"PreToolUse","toolName":"Bash*","command":"`+cmd+`"}]`)

	reg := loadAndRegister(t, hooks)

	// Non-matching tool => filtered out => allow.
	res, err := reg.Emit(context.Background(), ToolCallEvent{
		Type:     EventToolCall,
		ToolName: "Read",
		Input:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if res != nil {
		t.Fatalf("non-matching tool result = %#v, want nil", res)
	}

	// Matching tool => block.
	res, err = reg.Emit(context.Background(), ToolCallEvent{
		Type:     EventToolCall,
		ToolName: "Bash",
		Input:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if tc, ok := res.(ToolCallResult); !ok || !tc.Block {
		t.Fatalf("matching tool result = %#v, want blocked ToolCallResult", res)
	}
}

func TestShellHooksPostToolUsePatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	cmd := writeScript(t, dir, "term.sh", `echo '{"decision":"block","reason":"stop"}'`)
	hooks := writeHooks(t, dir, `[{"event":"PostToolUse","command":"`+cmd+`"}]`)

	reg := loadAndRegister(t, hooks)

	res, err := reg.Emit(context.Background(), ToolResultEvent{
		Type:     EventToolResult,
		ToolName: "Bash",
		Content:  []ai.ContentBlock{{Type: ai.ContentText, Text: "out"}},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// CRITICAL: afterToolCallFold does result.(hook.ToolResultPatch).
	patch, ok := res.(ToolResultPatch)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolResultPatch", res)
	}
	if patch.Terminate == nil || !*patch.Terminate {
		t.Fatalf("Terminate = %v, want true", patch.Terminate)
	}
	if patch.IsError == nil || !*patch.IsError {
		t.Fatalf("IsError = %v, want true", patch.IsError)
	}
}

func TestShellHooksPluginPostToolUseUpdatedToolOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	cmd := writeScript(t, dir, "compact.sh", `echo '{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":"compact output"}}'`)
	hooks := writeHooks(t, dir, `{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"`+cmd+`"}]}]}}`)

	runner, err := LoadShellHooksWithOptions(ShellHooksOptions{
		Path:        hooks,
		InputFormat: ShellHookInputPlugin,
	})
	if err != nil {
		t.Fatalf("LoadShellHooksWithOptions: %v", err)
	}
	reg := NewRegistry()
	runner.Register(reg, "sess-1")

	res, err := reg.Emit(context.Background(), ToolResultEvent{
		Type:     EventToolResult,
		ToolName: "bash",
		Content:  []ai.ContentBlock{{Type: ai.ContentText, Text: "raw output"}},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	patch, ok := res.(ToolResultPatch)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolResultPatch", res)
	}
	if len(patch.Content) != 1 || patch.Content[0].Type != ai.ContentText || patch.Content[0].Text != "compact output" {
		t.Fatalf("Content = %#v, want compact text block", patch.Content)
	}
}

func TestShellHooksGroupedCommandArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "script with spaces.sh")
	if err := os.WriteFile(script, []byte(`cat >/dev/null
echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"args worked"}}}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := writeHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"sh","args":["`+script+`"],"timeout":8}]}]}}`)

	runner, err := LoadShellHooksWithOptions(ShellHooksOptions{
		Path:        hooks,
		InputFormat: ShellHookInputPlugin,
	})
	if err != nil {
		t.Fatalf("LoadShellHooksWithOptions: %v", err)
	}
	reg := NewRegistry()
	runner.Register(reg, "sess-1")

	res, err := reg.Emit(context.Background(), ToolCallEvent{
		Type:     EventToolCall,
		ToolName: "bash",
		Input:    map[string]any{"command": "npm test"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	toolPatch, ok := res.(ToolCallResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolCallResult", res)
	}
	if toolPatch.Input["command"] != "args worked" {
		t.Fatalf("command = %#v, want args command to run", toolPatch.Input["command"])
	}

	builtinHooks := writeHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"eval","args":["printf '%s' '{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"updatedInput\":{\"command\":\"shell builtin ran\"}}}'"]}]}]}}`)
	builtinRunner, err := LoadShellHooksWithOptions(ShellHooksOptions{
		Path:        builtinHooks,
		InputFormat: ShellHookInputPlugin,
	})
	if err != nil {
		t.Fatalf("LoadShellHooksWithOptions builtin: %v", err)
	}
	builtinReg := NewRegistry()
	builtinRunner.Register(builtinReg, "sess-1")
	res, err = builtinReg.Emit(context.Background(), ToolCallEvent{
		Type:     EventToolCall,
		ToolName: "bash",
		Input:    map[string]any{"command": "npm test"},
	})
	if err != nil {
		t.Fatalf("Emit builtin: %v", err)
	}
	if res != nil {
		t.Fatalf("exec-form command ran through shell: %#v", res)
	}
}

func TestShellHooksMatcherAlternatives(t *testing.T) {
	if !toolNameMatches("Bash|Read", "bash") {
		t.Fatal("Bash|Read did not match bash")
	}
	if !toolNameMatches("Bash,Read", "read") {
		t.Fatal("Bash,Read did not match read")
	}
	if !toolNameMatches("/^(Bash|Read)$/i", "read") {
		t.Fatal("regex matcher did not match read")
	}
	if toolNameMatches("Read|Grep", "bash") {
		t.Fatal("Read|Grep matched bash")
	}
	if toolNameMatches("/^Read$/", "bash") {
		t.Fatal("regex matcher matched bash")
	}
}

func TestLoadShellHooksMissingFileIsNil(t *testing.T) {
	runner, err := LoadShellHooks(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("LoadShellHooks(missing): %v", err)
	}
	if runner != nil {
		t.Fatalf("runner = %#v, want nil for missing file", runner)
	}
	if runner.HasHandlers() {
		t.Fatalf("nil runner reports handlers")
	}
}

func TestLoadShellHooksEmptyPathIsNil(t *testing.T) {
	runner, err := LoadShellHooks("")
	if err != nil {
		t.Fatalf("LoadShellHooks(\"\"): %v", err)
	}
	if runner != nil {
		t.Fatalf("runner = %#v, want nil for empty path", runner)
	}
}

func TestLoadShellHooksUnknownEventErrors(t *testing.T) {
	dir := t.TempDir()
	hooks := writeHooks(t, dir, `[{"event":"NopeEvent","command":"true"}]`)
	if _, err := LoadShellHooks(hooks); err == nil {
		t.Fatalf("LoadShellHooks accepted unknown event, want error")
	}
}

// requirePOSIX skips on Windows where these tests cannot run a POSIX shell.
func requirePOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks test requires a POSIX shell")
	}
}

// registerEcho loads a single-hook hooks.json whose command echoes body and
// returns the registry. event is the hooks.json event name (native or alias).
func registerEcho(t *testing.T, event, body string) *Registry {
	t.Helper()
	dir := t.TempDir()
	cmd := writeScript(t, dir, "h.sh", body)
	hooks := writeHooks(t, dir,
		`[{"event":"`+event+`","command":"`+cmd+`"}]`)
	return loadAndRegister(t, hooks)
}

// firstHandler returns the single handler registered for eventType, failing if
// there is not exactly one — the shell-hooks runner registers one handler per
// rule, and the decision emit sites drive handlers directly (not Emit) for the
// per-handler-loop events, so the tests call the handler the same way.
func firstHandler(t *testing.T, reg *Registry, eventType string) Handler {
	t.Helper()
	handlers := reg.Handlers(eventType)
	if len(handlers) != 1 {
		t.Fatalf("handlers for %q = %d, want 1", eventType, len(handlers))
	}
	return handlers[0]
}

// --- DECISION events: assert the EXACT concrete result type + value. ---

func TestShellHooksBeforeAgentStart(t *testing.T) {
	requirePOSIX(t)
	// before_agent_start via the SessionStart alias; replace the system prompt.
	reg := registerEcho(t, "SessionStart",
		`echo '{"systemPrompt":"REPLACED"}'`)
	h := firstHandler(t, reg, EventBeforeAgentStart)

	res, err := h(context.Background(), BeforeAgentStartEvent{
		Type:         EventBeforeAgentStart,
		Prompt:       "hi",
		SystemPrompt: "ORIGINAL",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(BeforeAgentStartResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.BeforeAgentStartResult", res)
	}
	if r.SystemPrompt == nil || *r.SystemPrompt != "REPLACED" {
		t.Fatalf("SystemPrompt = %v, want REPLACED", r.SystemPrompt)
	}
}

func TestShellHooksBeforeAgentStartNativeName(t *testing.T) {
	requirePOSIX(t)
	// The native event name is accepted directly too.
	reg := registerEcho(t, "before_agent_start",
		`echo '{"systemPrompt":"X"}'`)
	h := firstHandler(t, reg, EventBeforeAgentStart)
	res, err := h(context.Background(), BeforeAgentStartEvent{Type: EventBeforeAgentStart})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if r, ok := res.(BeforeAgentStartResult); !ok || r.SystemPrompt == nil || *r.SystemPrompt != "X" {
		t.Fatalf("result = %#v, want BeforeAgentStartResult{SystemPrompt:X}", res)
	}
}

func TestShellHooksBeforeAgentStartNoOpinion(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "before_agent_start", `exit 0`)
	h := firstHandler(t, reg, EventBeforeAgentStart)
	res, err := h(context.Background(), BeforeAgentStartEvent{Type: EventBeforeAgentStart})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res != nil {
		t.Fatalf("result = %#v, want nil (no opinion)", res)
	}
}

func TestShellHooksBeforeAgentStartInjectsMessage(t *testing.T) {
	requirePOSIX(t)
	// Declaratively inject a message at agent start.
	reg := registerEcho(t, "before_agent_start",
		`echo '{"messages":[{"role":"user","text":"injected note"}]}'`)
	h := firstHandler(t, reg, EventBeforeAgentStart)
	res, err := h(context.Background(), BeforeAgentStartEvent{Type: EventBeforeAgentStart})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(BeforeAgentStartResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.BeforeAgentStartResult", res)
	}
	if len(r.Messages) != 1 {
		t.Fatalf("Messages = %#v, want 1 message", r.Messages)
	}
	// The harness keeps only message.AgentMessage values (prompt.go:153-157); a
	// raw map is dropped. The element must be a concrete ai.Message that also
	// satisfies message.AgentMessage.
	msg, ok := r.Messages[0].(ai.Message)
	if !ok {
		t.Fatalf("Messages[0] type = %T, want ai.Message", r.Messages[0])
	}
	if _, ok := r.Messages[0].(message.AgentMessage); !ok {
		t.Fatalf("Messages[0] does not satisfy message.AgentMessage")
	}
	if msg.Role != ai.RoleUser {
		t.Fatalf("Role = %q, want %q", msg.Role, ai.RoleUser)
	}
	if got := textOf(msg); got != "injected note" {
		t.Fatalf("text = %q, want %q", got, "injected note")
	}
	if msg.Timestamp == 0 {
		t.Fatalf("Timestamp = 0, want a default UnixMilli timestamp")
	}
}

// TestShellHooksMessageRoleCoercion proves "system" (which ai.Role lacks)
// coerces to RoleUser, and that an explicit "content" array is honored.
func TestShellHooksMessageRoleCoercion(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "before_agent_start",
		`echo '{"messages":[{"role":"system","text":"x"}]}'`)
	h := firstHandler(t, reg, EventBeforeAgentStart)
	res, err := h(context.Background(), BeforeAgentStartEvent{Type: EventBeforeAgentStart})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := res.(BeforeAgentStartResult)
	if len(r.Messages) != 1 {
		t.Fatalf("Messages = %#v, want 1 message", r.Messages)
	}
	msg := r.Messages[0].(ai.Message)
	if msg.Role != ai.RoleUser {
		t.Fatalf("Role = %q, want %q (system coerces to user)", msg.Role, ai.RoleUser)
	}
}

func TestShellHooksContext(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "context",
		`echo '{"messages":[{"role":"user","text":"ctx note"}]}'`)
	h := firstHandler(t, reg, EventContext)
	res, err := h(context.Background(), ContextEvent{Type: EventContext})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(ContextResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.ContextResult", res)
	}
	if len(r.Messages) != 1 {
		t.Fatalf("Messages = %#v, want 1 message", r.Messages)
	}
	// fromAnySlice keeps only message.AgentMessage values; a raw map is dropped.
	// The element must be a concrete ai.Message carrying the text.
	msg, ok := r.Messages[0].(ai.Message)
	if !ok {
		t.Fatalf("Messages[0] type = %T, want ai.Message", r.Messages[0])
	}
	if _, ok := r.Messages[0].(message.AgentMessage); !ok {
		t.Fatalf("Messages[0] does not satisfy message.AgentMessage")
	}
	if msg.Role != ai.RoleUser {
		t.Fatalf("Role = %q, want %q", msg.Role, ai.RoleUser)
	}
	if got := textOf(msg); got != "ctx note" {
		t.Fatalf("text = %q, want %q", got, "ctx note")
	}
}

func TestShellHooksInput(t *testing.T) {
	requirePOSIX(t)
	// input via the UserPromptSubmit alias; transform the prompt text.
	reg := registerEcho(t, "UserPromptSubmit",
		`echo '{"action":"transform","text":"clean"}'`)
	h := firstHandler(t, reg, EventInput)
	res, err := h(context.Background(), InputEvent{
		Type:   EventInput,
		Text:   "dirty",
		Source: "interactive",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(InputResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.InputResult", res)
	}
	if r.Action != "transform" || r.Text != "clean" {
		t.Fatalf("InputResult = %#v, want {transform clean}", r)
	}
}

func TestShellHooksBeforeProviderRequest(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "before_provider_request",
		`echo '{"streamOptions":{"temperature":0.5,"maxTokens":128}}'`)
	h := firstHandler(t, reg, EventBeforeProviderRequest)
	res, err := h(context.Background(), BeforeProviderRequestEvent{Type: EventBeforeProviderRequest})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(BeforeProviderRequestResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.BeforeProviderRequestResult", res)
	}
	if r.StreamOptions == nil {
		t.Fatalf("StreamOptions = nil, want decoded value")
	}
	// applyStreamOptionsPatch recognizes only ai.SimpleStreamOptions /
	// hook.StreamOptionsPatch; a raw map no-ops. Prove the concrete patch type.
	patch, ok := r.StreamOptions.(StreamOptionsPatch)
	if !ok {
		t.Fatalf("StreamOptions type = %T, want hook.StreamOptionsPatch", r.StreamOptions)
	}
	if patch.Temperature == nil || *patch.Temperature != 0.5 {
		t.Fatalf("Temperature = %v, want 0.5", patch.Temperature)
	}
	if patch.MaxTokens == nil || *patch.MaxTokens != 128 {
		t.Fatalf("MaxTokens = %v, want 128", patch.MaxTokens)
	}
}

func TestShellHooksBeforeProviderRequestNoOpinion(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "before_provider_request", `exit 0`)
	h := firstHandler(t, reg, EventBeforeProviderRequest)
	res, err := h(context.Background(), BeforeProviderRequestEvent{Type: EventBeforeProviderRequest})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res != nil {
		t.Fatalf("result = %#v, want nil (no opinion)", res)
	}
}

func TestShellHooksBeforeProviderPayload(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "before_provider_payload",
		`echo '{"payload":{"k":"v"}}'`)
	h := firstHandler(t, reg, EventBeforeProviderPayload)
	res, err := h(context.Background(), BeforeProviderPayloadEvent{Type: EventBeforeProviderPayload})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(BeforeProviderPayloadResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.BeforeProviderPayloadResult", res)
	}
	m, ok := r.Payload.(map[string]any)
	if !ok || m["k"] != "v" {
		t.Fatalf("Payload = %#v, want map{k:v}", r.Payload)
	}
}

func TestShellHooksMessageEnd(t *testing.T) {
	requirePOSIX(t)
	// The harness requires the replacement to carry the SAME role as the
	// streamed message (sameMessageEndRole), so an assistant turn is replaced
	// with role:"assistant".
	reg := registerEcho(t, "message_end",
		`echo '{"message":{"role":"assistant","text":"rewritten"}}'`)
	h := firstHandler(t, reg, EventMessageEnd)
	res, err := h(context.Background(), MessageEndEvent{Type: EventMessageEnd})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(MessageEndResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.MessageEndResult", res)
	}
	if r.Message == nil {
		t.Fatalf("Message = nil, want decoded value")
	}
	// resultAgentMessage does value.(message.AgentMessage); a raw map fails.
	// The decoded value must be a concrete ai.Message satisfying it.
	msg, ok := r.Message.(ai.Message)
	if !ok {
		t.Fatalf("Message type = %T, want ai.Message", r.Message)
	}
	if _, ok := r.Message.(message.AgentMessage); !ok {
		t.Fatalf("Message does not satisfy message.AgentMessage")
	}
	if msg.Role != ai.RoleAssistant {
		t.Fatalf("Role = %q, want %q", msg.Role, ai.RoleAssistant)
	}
	if got := textOf(msg); got != "rewritten" {
		t.Fatalf("text = %q, want %q", got, "rewritten")
	}
}

func TestShellHooksMessageEndNoOpinion(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "message_end", `exit 0`)
	h := firstHandler(t, reg, EventMessageEnd)
	res, err := h(context.Background(), MessageEndEvent{Type: EventMessageEnd})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res != nil {
		t.Fatalf("result = %#v, want nil (no opinion => no change)", res)
	}
}

func TestShellHooksToolResultContentPatch(t *testing.T) {
	requirePOSIX(t)
	// content override + isError patch (no block) on tool_result.
	reg := registerEcho(t, "PostToolUse",
		`echo '{"content":[{"type":"text","text":"scrubbed"}],"isError":true}'`)
	h := firstHandler(t, reg, EventToolResult)
	res, err := h(context.Background(), ToolResultEvent{
		Type:     EventToolResult,
		ToolName: "Bash",
		Content:  []ai.ContentBlock{{Type: ai.ContentText, Text: "secret"}},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	patch, ok := res.(ToolResultPatch)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolResultPatch", res)
	}
	if len(patch.Content) != 1 || patch.Content[0].Text != "scrubbed" {
		t.Fatalf("Content = %#v, want [scrubbed]", patch.Content)
	}
	if patch.IsError == nil || !*patch.IsError {
		t.Fatalf("IsError = %v, want true", patch.IsError)
	}
	if patch.Terminate != nil {
		t.Fatalf("Terminate = %v, want nil (no block)", patch.Terminate)
	}
}

func TestShellHooksSessionBeforeSwitch(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "session_before_switch", `echo '{"cancel":true}'`)
	res, err := reg.Emit(context.Background(), SessionBeforeSwitchEvent{Type: EventSessionBeforeSwitch})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	r, ok := res.(SessionBeforeSwitchResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.SessionBeforeSwitchResult", res)
	}
	if !r.Cancel {
		t.Fatalf("Cancel = false, want true")
	}
}

func TestShellHooksSessionBeforeFork(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "session_before_fork",
		`echo '{"cancel":false,"skipConversationRestore":true}'`)
	res, err := reg.Emit(context.Background(), SessionBeforeForkEvent{Type: EventSessionBeforeFork})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	r, ok := res.(SessionBeforeForkResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.SessionBeforeForkResult", res)
	}
	if r.Cancel {
		t.Fatalf("Cancel = true, want false")
	}
	if !r.SkipConversationRestore {
		t.Fatalf("SkipConversationRestore = false, want true")
	}
}

func TestShellHooksSessionBeforeCompact(t *testing.T) {
	requirePOSIX(t)
	// PreCompact alias maps to session_before_compact; cancel compaction.
	reg := registerEcho(t, "PreCompact", `echo '{"cancel":true}'`)
	res, err := reg.Emit(context.Background(), SessionBeforeCompactEvent{Type: EventSessionBeforeCompact})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	r, ok := res.(SessionBeforeCompactResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.SessionBeforeCompactResult", res)
	}
	if !r.Cancel {
		t.Fatalf("Cancel = false, want true")
	}
}

func TestShellHooksSessionBeforeCompactOverride(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "session_before_compact",
		`echo '{"compaction":{"summary":"S","tokensBefore":42}}'`)
	res, err := reg.Emit(context.Background(), SessionBeforeCompactEvent{Type: EventSessionBeforeCompact})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	r, ok := res.(SessionBeforeCompactResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.SessionBeforeCompactResult", res)
	}
	if r.Compaction == nil || r.Compaction.Summary != "S" || r.Compaction.TokensBefore != 42 {
		t.Fatalf("Compaction = %#v, want {S 42}", r.Compaction)
	}
}

func TestShellHooksSessionBeforeTree(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "session_before_tree",
		`echo '{"cancel":false,"summary":{"summary":"branch"}}'`)
	res, err := reg.Emit(context.Background(), SessionBeforeTreeEvent{Type: EventSessionBeforeTree})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	r, ok := res.(SessionBeforeTreeResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.SessionBeforeTreeResult", res)
	}
	if r.Cancel {
		t.Fatalf("Cancel = true, want false")
	}
	if r.Summary == nil || r.Summary.Summary != "branch" {
		t.Fatalf("Summary = %#v, want {branch}", r.Summary)
	}
}

func TestShellHooksResourcesDiscover(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "resources_discover",
		`echo '{"skillPaths":["/a"],"promptPaths":["/b"],"themePaths":["/c"]}'`)
	h := firstHandler(t, reg, EventResourcesDiscover)
	res, err := h(context.Background(), ResourcesDiscoverEvent{Type: EventResourcesDiscover})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r, ok := res.(ResourcesDiscoverResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.ResourcesDiscoverResult", res)
	}
	if len(r.SkillPaths) != 1 || r.SkillPaths[0] != "/a" {
		t.Fatalf("SkillPaths = %#v, want [/a]", r.SkillPaths)
	}
	if len(r.PromptPaths) != 1 || r.PromptPaths[0] != "/b" {
		t.Fatalf("PromptPaths = %#v, want [/b]", r.PromptPaths)
	}
	if len(r.ThemePaths) != 1 || r.ThemePaths[0] != "/c" {
		t.Fatalf("ThemePaths = %#v, want [/c]", r.ThemePaths)
	}
}

// --- NOTIFICATION events: command runs (sees event JSON on stdin) and the
// handler returns (nil, nil). ---

// notifyEventCase pairs a notification event name/native-type with a constructed
// event carrying a recognizable field so the stdin capture can assert it.
type notifyEventCase struct {
	name      string // hooks.json event name
	nativeKey string // native event type registered
	event     any    // constructed event with a probe field
	probe     string // substring expected in the captured stdin JSON
}

func TestShellHooksNotificationEventsRunWithStdin(t *testing.T) {
	requirePOSIX(t)

	cases := []notifyEventCase{
		{
			name:      "agent_start",
			nativeKey: EventAgentStart,
			event:     AgentStartEvent{Type: EventAgentStart},
			probe:     `"Type":"agent_start"`,
		},
		{
			name:      "Stop",
			nativeKey: EventSettled,
			event:     SettledEvent{Type: EventSettled, NextTurnCount: 3},
			probe:     `"NextTurnCount":3`,
		},
		{
			name:      "SubagentStop",
			nativeKey: EventAgentEnd,
			event:     AgentEndEvent{Type: EventAgentEnd},
			probe:     `"Type":"agent_end"`,
		},
		{
			name:      "turn_start",
			nativeKey: EventTurnStart,
			event:     TurnStartEvent{Type: EventTurnStart, TurnIndex: 7},
			probe:     `"TurnIndex":7`,
		},
		{
			name:      "tool_execution_end",
			nativeKey: EventToolExecutionEnd,
			event:     ToolExecutionEndEvent{Type: EventToolExecutionEnd, ToolName: "Bash"},
			probe:     `"ToolName":"Bash"`,
		},
		{
			name:      "model_select",
			nativeKey: EventModelSelect,
			event:     ModelSelectEvent{Type: EventModelSelect, Source: "set"},
			probe:     `"Source":"set"`,
		},
		{
			name:      "session_start",
			nativeKey: EventSessionStart,
			event:     SessionStartEvent{Type: EventSessionStart, Reason: "boot"},
			probe:     `"Reason":"boot"`,
		},
		{
			name:      "abort",
			nativeKey: EventAbort,
			event:     AbortEvent{Type: EventAbort},
			probe:     `"Type":"abort"`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "stdin.json")
			// Notification command writes its stdin to a file and prints nothing.
			cmd := writeScript(t, dir, "notify.sh", `cat > `+capture)
			hooks := writeHooks(t, dir,
				`[{"event":"`+tc.name+`","command":"`+cmd+`"}]`)
			reg := loadAndRegister(t, hooks)

			h := firstHandler(t, reg, tc.nativeKey)
			res, err := h(context.Background(), tc.event)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if res != nil {
				t.Fatalf("notification result = %#v, want nil", res)
			}

			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("read stdin capture: %v", err)
			}
			if !strings.Contains(string(data), tc.probe) {
				t.Fatalf("stdin %q does not contain probe %q", string(data), tc.probe)
			}
		})
	}
}

// TestShellHooksInertEventRegisters proves a queue_update hook still loads and
// registers (forward-compat) even though it can never fire — the runner warns at
// registration but does not error.
func TestShellHooksInertQueueUpdateRegisters(t *testing.T) {
	requirePOSIX(t)
	reg := registerEcho(t, "queue_update", `echo '{}'`)
	if !reg.HasHandlers(EventQueueUpdate) {
		t.Fatalf("queue_update handler not registered")
	}
}
