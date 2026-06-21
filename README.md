# Attention

A standalone, local-first Go engine for an AI coding/chat agent — the headless
core (no TUI), corresponding to a TUI-less `pi`. Extracted from the `along` Go
codebase: the `cmd/along` headless CLI plus its `src/core` dependency closure,
with the server-form bindings (Postgres / Redis / object store) removed.

## What's in here

- `cmd/along` — headless CLI entry (print / json / rpc modes).
- `internal/**` — the engine: agent loop, AI providers, sessions, tools,
  pipeline, hooks, plugins, the local event store + control-plane HTTP (REST+SSE).
- `plugins/**` — bundled example plugins (hello-world, session-list, todo).

Excluded on purpose (server form): `backend/pg`, `backend/redis`, `store/pg`,
`envhost`. The local bindings (`backend/local`) are kept, so it runs fully
self-contained on one machine.

## Build & run

```sh
go build ./...          # build everything
go build -o along ./cmd/along
go test ./...           # tests (two pre-existing macOS-only env failures)
```

## Provenance

Lifted from `along` (a Go rewrite of `pi`'s core) at its current HEAD. The core
engine has no import edges into the server-form packages, so the slice is clean:
only the module path and bundled plugins differ from upstream.
