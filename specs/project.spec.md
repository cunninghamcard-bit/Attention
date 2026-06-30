spec: project
name: "Attention Pi-Compatible Runtime"
tags: [attention, pi-compatible, runtime]
---

## Intent

Keep Attention's Go kernel compatible with Pi's observable runtime contracts
while preserving the existing TUI/RPC split. Project work should prefer file
plugins and generic protocols over feature-specific TUI branches.

## Constraints

- Kernel behavior must remain driven by orchestrator, harness, hook, extension, tool, and RPC contracts rather than TUI special cases.
- Slash-command completion data must continue to come from the kernel `get_commands` response.
- Tool lifecycle events exposed to RPC/TUI must match the final kernel event state after hooks have run.
- File plugins must degrade safely when optional external commands are missing at startup.
- Do not add a TypeScript/npm extension runtime unless a separate contract explicitly asks for it.
- Do not add new dependencies for behavior already covered by existing packages or the Go standard library.

## Decisions

- Attention remains a Go implementation with Pi-compatible event, session, tool, resource, and RPC shapes.
- The TUI remains a client of the RPC protocol; it must not know plugin-specific behavior.
- File plugins should adapt into the existing `extension.Factory` and `hook.Registry` path.

## Completion Criteria

Scenario: kernel command list remains the slash-command source
  Test: TestSlashCommandsRemainKernelDriven
  Given the kernel returns builtin, prompt, skill, and extension commands
  When the TUI builds slash-command completion candidates
  Then the candidates come from `get_commands`
  And the TUI does not maintain a separate plugin command list

Scenario: mutated lifecycle events reach RPC and TUI
  Test: TestToolLifecyclePublishesFinalMutatedState
  Given a native hook mutates a tool lifecycle event before publication
  When the orchestrator publishes the event to subscribers
  Then RPC receives the final mutated event state
  And TUI rendering receives the final mutated event state

Scenario: plugin hook command absence does not break startup
  Test: TestFilePluginHookCommandDoesNotBreakStartup
  Given a file plugin hook command names an unavailable executable
  When Attention starts a session
  Then startup succeeds
  And the hook command is not executed until its matching event fires

Scenario: plugin behavior stays out of TUI dispatch
  Test: TestTUIDispatchesExtensionCommandOverRPC
  Given the TUI dispatches a command with source "extension"
  When the command name belongs to a plugin
  Then the TUI uses the generic extension command RPC path
  And no plugin-specific branch is required

Scenario: TypeScript extension runtime is not introduced
  Test: TestFilePluginSystemDoesNotAddTypeScriptRuntime
  Given a file plugin is enabled
  When the runtime loads plugin hooks, skills, commands, and bin paths
  Then it uses Go `extension.Factory` registration
  And it does not execute npm or TypeScript extension loaders

Scenario: new dependency is rejected when existing code suffices
  Test: TestFilePluginSystemAvoidsUnneededDependencies
  Given the file plugin system needs path resolution, JSON parsing, shell hooks, and PATH updates
  When implementation code is reviewed
  Then existing repository packages or Go standard library APIs provide those behaviors
  And no new dependency is added for them
