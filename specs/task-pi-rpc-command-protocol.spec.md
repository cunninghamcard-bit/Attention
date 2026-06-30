spec: task
name: "Pi RPC Command Protocol"
inherits: project
tags: [phase-2, rpc, slash-command, parity]
---

## Intent

Complete the RPC slash-command execution path so commands returned by
`get_commands` can be executed by the kernel instead of being accidentally sent
to the model. This provides the generic command protocol needed by file
plugins without adding plugin-specific TUI behavior.

## Decisions

- Keep `extension.CommandDefinition.Handler` as `func(context.Context, []string, ExtensionContext) error`.
- Add `ExtensionContext.Notify(message, level string)` for Pi-style user-visible command feedback.
- Add `orchestrator.CommandNotification` with `Message string` and `Level string`.
- `Orchestrator.DispatchCommand` returns collected command notifications plus an error.
- Empty or invalid command notification levels normalize to `info`.
- Add RPC command type `dispatch_command` with fields `name` and `args`.
- RPC parses `args` exactly once with the existing Pi-style command argument parser before calling `Orchestrator.DispatchCommand`.
- RPC `dispatch_command` returns optional `notifications` data when handlers emit notifications.
- `source=="extension"` in the TUI calls RPC `dispatch_command`.
- `source=="builtin"` keeps existing builtin-specific RPC actions.
- `source=="prompt"` and `source=="skill"` continue submitting the full slash line as a prompt.
- `get_commands` remains the single source of slash-command completion data.

## Boundaries

### Allowed Changes
- internal/extension/**
- internal/orchestrator/**
- internal/mode/rpc/**
- internal/resource/**
- cmd/tui/**

### Forbidden
- Do not add plugin-specific files or behavior.
- Do not hardcode any extension command name in the TUI.
- Do not change builtin command semantics except where signatures must compile.
- Do not change prompt template or skill expansion behavior.

### Out of Scope
- Tool execution event mutation.
- File plugin system.
- Interactive command UIs.
- Asynchronous UI event streaming for slash-command feedback.

## Completion Criteria

Scenario: RPC executes extension command
  Test: TestDispatchCommandRoutesToRegisteredHandler
  Given `get_commands` includes an extension command named "run"
  When RPC receives `dispatch_command` with name "run"
  Then `Orchestrator.DispatchCommand` executes the registered handler
  And RPC returns success

Scenario: command notifications are returned to RPC
  Test: TestServeDispatchCommandReturnsNotifications
  Given an extension command calls `Notify("Plugin available", "info")`
  When RPC dispatches that command
  Then the response data contains a notification with message "Plugin available"
  And the notification level is "info"

Scenario: quoted args are parsed once
  Test: TestServeDispatchCommandParsesQuotedArgsOnceAndAllowsNoNotification
  Given an extension command records its received args
  When RPC dispatches it with args `set mode "suggest mode"`
  Then the handler receives exactly `["set", "mode", "suggest mode"]`
  And no later layer reparses the args
  And RPC success does not require a notification

Scenario: handler error returns RPC failure
  Test: TestServeDispatchCommandHandlerErrorReturnsFailure
  Given an extension command handler returns an error
  When RPC dispatches that command
  Then the RPC response has `success=false`
  And the error text is preserved

Scenario: unknown command returns RPC failure
  Test: TestServeDispatchCommandUnknownCommandReturnsFailure
  Given no extension command named "missing" is registered
  When RPC dispatches name "missing"
  Then the RPC response has `success=false`
  And the error explains that the command was not found

Scenario: TUI extension command does not submit prompt
  Test: TestTUIDispatchesExtensionCommandOverRPC
  Given the command list contains source "extension" for command "plugin-run"
  When the user submits `/plugin-run show`
  Then the TUI calls RPC `dispatch_command`
  And the TUI does not call prompt submission for that slash line

Scenario: prompt and skill commands still submit prompt
  Test: TestTUIPromptAndSkillCommandsStillSubmitPrompt
  Given the command list contains a prompt command and a skill command
  When the user submits either slash command
  Then the TUI submits the full slash line as a prompt
  And `dispatch_command` is not called
