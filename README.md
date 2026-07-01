# Attention

A pure, headless **agent kernel** in Go — derived from `pi`'s core, before any
transport/server was layered on. It drives a single agent (LLM + tools +
sessions) and nothing else. No HTTP server, no worker/two-plane, no plugin host.

The unified agent-driver abstraction (plug in pi-native / Claude Code / Codex)
and the transport layer (REST+SSE, multi-tenant cloud) are **separate layers
built on top of this kernel** — they do not live here.

## What's in here

- `internal/**` — the kernel: `agentloop`, `ai` (providers), `session`, `tool`
  (+ `builtin`), `provider`, `mode` (print / rpc), `harness`, `orchestrator`,
  `extension` (+ `jshost`), `hook`, `execenv`, `config`, `auth`, `render`,
  `resource`, `exporthtml`, `message`, `obs`.
- `cmd/along` — headless CLI (print / rpc stdio modes).

## Provenance

Extracted from the `along` Go codebase at commit `a4702cc` — the last commit
**before** `along` added its two-plane REST+SSE control-plane (the `backend` /
`protocol` / `server` / `worker` packages, June 12 2026). That is the cleanest
"TUI-less pi" point in `along`'s history: the agent core was complete, the
transport had not yet been bolted on. Layout merged into a single `internal/`
tree (upstream had it split across `src/core/` + `internal/`); module path
rewritten to `github.com/cunninghamcard-bit/Attention`.

## Build & run

```sh
go build ./...
go build -o along ./cmd/along
go test ./...
```

## Plugins

Install a plugin from a git URL or local plugin directory:

```sh
along plugin install https://github.com/you/plugin.git
```

The installer copies or clones the plugin into `~/.along/plugins/<name>` and
adds the plugin name to global settings:

```json
{
  "plugins": ["plugin-name"]
}
```

Project plugins at `.along/plugins/<name>` override global plugins at
`~/.along/plugins/<name>`.

Known test failures (all pre-existing at `a4702cc`, none are code regressions):
- `internal/execenv/local` and `internal/resource` — macOS-only env quirks
  (symlinked `/var`, case-insensitive FS).
- `internal/extension/jshost` and `internal/orchestrator` — two JS-extension
  tests need `bun` on PATH plus a `pi` example-extension fixture that is not part
  of the kernel. The extension JS host runs extensions via `bun` (pi-style).
