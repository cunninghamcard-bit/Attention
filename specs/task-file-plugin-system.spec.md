spec: task
name: "File Plugin System"
inherits: project
tags: [phase-3, plugin, extension, hooks]
---

## Intent

Load Attention-managed plugins from Attention-managed plugin directories. A
plugin should be able to provide JSON-configured hooks, executable `bin/`
entries, skills, and slash-command prompt templates without being rewritten as a
Go module.
RTK is the first target use case, but Attention must not duplicate RTK's rewrite
engine or compile an RTK-specific Go extension.

## Decisions

- Enabled plugin names come from settings key `plugins`.
- Named plugins resolve under project `.along/plugins/<name>` first, then
  global `~/.along/plugins/<name>`.
- `along plugin install <git-url-or-path>` installs a plugin into
  `~/.along/plugins/<manifest.name>` and enables that name in global settings.
- Plugin settings entries are names, not arbitrary filesystem paths.
- A plugin root must contain `.attention-plugin/plugin.json`.
- `hooks/hooks.json` supports grouped hook JSON and Attention's legacy array form.
- Plugin hooks receive plugin hook stdin when loaded from a plugin.
- `hookSpecificOutput.updatedInput` mutates `PreToolUse` input.
- Plugin `bin/` directories are added to Bash PATH and hook command PATH.
- Plugin `skills/` directories are loaded through the existing skill loader.
- Plugin `commands/*.md` files are loaded through the existing prompt-template command loader.
- Plugin `commands/commands.json` registers handler slash commands through the
  existing extension command path.
- Handler commands support only `{ "type": "command" }` in this phase. They run
  the declared command/args directly with a default 10s timeout unless
  `timeout` is set in seconds.
- Handler stdin is JSON with `command_name`, `args`, `session_id`, and `cwd`.
- Handler stdout must be JSON. `notifications: [{ level, message }]` are routed
  through existing command dispatch notifications; non-JSON stdout is a command
  error.
- Handler env includes `ATTENTION_PLUGIN_ROOT`, `ATTENTION_PROJECT_DIR`,
  `ATTENTION_AGENT_DIR`, and plugin `bin/` prepended to `PATH`.
- Do not add a JavaScript or TypeScript extension host in this phase.
- Do not add an RTK Go port.

## Boundaries

### Allowed Changes
- internal/extension/**
- internal/hook/**
- internal/orchestrator/**
- internal/tool/builtin/**
- internal/resource/**
- internal/plugin/**
- cmd/along/**

### Forbidden
- Do not add npm, TypeScript, or JS host runtime dependencies.
- Do not hardcode RTK behavior in cmd/tui or cmd/along.
- Do not duplicate RTK's rewrite rule database in Go.
- Do not load arbitrary plugin paths from settings.

### Out of Scope
- MCP server runtime.
- LSP server runtime.
- Subagent/worktree plugin runtime.
- Background monitors.
- Theme/output-style protocol compatibility.

## Completion Criteria

Scenario: file plugin source resolves from settings
  Test: TestLoadFilePluginSourcesHooksBinAndResources
  Given settings include plugin "rtk-optimizer"
  And `~/.along/plugins/rtk-optimizer/.attention-plugin/plugin.json` exists
  When the file plugin loader resolves settings
  Then it returns source path "plugin:rtk-optimizer"
  And it returns the plugin `bin/` directory

Scenario: plugin hook envelope mutates bash input
  Test: TestShellHooksPluginPreToolUseEnvelope
  Given a plugin `hooks/hooks.json` declares a `PreToolUse` command matcher for `Bash`
  And the hook command returns `hookSpecificOutput.updatedInput`
  When a `tool_call` for bash is emitted
  Then the hook stdin includes `hook_event_name`, `tool_name`, and `tool_input`
  And the returned `ToolCallResult` contains the updated command input

Scenario: plugin resources are discovered through existing loaders
  Test: TestLoadFilePluginSourcesHooksBinAndResources
  Given a plugin has `skills/` and `commands/` directories
  When `resources_discover` runs
  Then the result includes the plugin skill path
  And the result includes the plugin command path

Scenario: plugin command registry is loaded
  Test: TestLoadFilePluginSourcesHooksBinAndResources
  Given a plugin has `commands/commands.json`
  When the file plugin loader resolves settings
  Then the resulting extension registers the declared command handler metadata

Scenario: plugin handler command dispatches notifications
  Test: TestReloadSettingsLoadsNewlyEnabledFilePlugin
  Given a newly enabled plugin has `commands/commands.json`
  And the declared handler executable is in the plugin `bin/` directory
  When settings are reloaded and the command is dispatched
  Then `get_commands` can see the handler command
  And the handler receives command stdin and plugin env
  And JSON stdout notifications are returned through dispatch
  And non-JSON stdout returns a command error

Scenario: plugin hook commands are lazy at startup
  Test: TestFilePluginHookCommandDoesNotBreakStartup
  Given a file plugin hook command names an unavailable external command
  When Attention starts a session
  Then startup succeeds
  And the hook command is not executed until the matching event fires

Scenario: plugin bin is available to bash
  Test: TestBashToolPrependsPluginBinDirs
  Given the built-in bash tool is created with a plugin bin directory
  When bash executes a command from that plugin bin directory
  Then the command is resolved through PATH

Scenario: TypeScript plugin files are not executed by the loader
  Test: TestFilePluginSystemDoesNotAddTypeScriptRuntime
  Given a file plugin contains `package.json` and `index.ts`
  When the file plugin loader resolves settings
  Then it returns a Go extension source
  And no npm or TypeScript entrypoint is executed

Scenario: plugin setting path is rejected
  Test: TestLoadRejectsPluginPathSetting
  Given settings include plugin entry "./plugin"
  When the file plugin loader resolves settings
  Then no source is returned
  And an error diagnostic explains plugin entries must be names

Scenario: missing plugin reports a diagnostic
  Test: TestLoadMissingFilePluginReportsDiagnostic
  Given settings include a plugin name with no plugin directory
  When the file plugin loader resolves settings
  Then no source is returned
  And an error diagnostic identifies the missing plugin

Scenario: local plugin install copies and enables
  Test: TestInstallLocalPluginCopiesToGlobalDirAndEnables
  Given a local plugin directory contains `.attention-plugin/plugin.json`
  When `along plugin install <path>` installs it
  Then it copies the plugin to `~/.along/plugins/<manifest.name>`
  And global settings include that plugin name

Scenario: git plugin install clones and enables
  Test: TestInstallGitPluginClonesSource
  Given an install source is not a local directory
  When `along plugin install <source>` installs it
  Then the installer runs `git clone --depth=1 <source>`
  And global settings include the cloned plugin name

Scenario: project plugin overrides global plugin
  Test: TestLoadProjectPluginOverridesGlobalPlugin
  Given settings include plugin "rtk-optimizer"
  And both project and global plugin directories exist
  When the file plugin loader resolves settings
  Then it loads `.along/plugins/rtk-optimizer`
  And ignores the global plugin of the same name
