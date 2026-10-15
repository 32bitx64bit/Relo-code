# Development

## Layout

```text
core/                 Relo Core (Go)
  cmd/relo-core/
  internal/context    variable context engine
  internal/orchestrator
  internal/opencode   documented HTTP/SSE adapter + process manager
  internal/runtime    AgentRuntime interface + mock
extension/            VS Code extension
webview/              webview source (copied into extension/media)
protocol/             shared TypeScript frame types
docs/
```

## Commands

```text
make build
make test
make lint
make package
```

VS Code: **Run Extension** builds then launches an Extension Development Host. Set `relo.runtime` to `mock` to iterate without OpenCode.

Relo Core flags: `-workspace`, `-db`, `-opencode`, `-runtime opencode|mock`, `-log-level`.

## Tests

Go coverage includes task graph, scheduler limits, context generations, compression, pinned constraints, unverified memory, parent summaries, SQLite, IPC, worktrees, merge conflicts, OpenCode HTTP client (httptest), and the mock acceptance scenario (`internal/orchestrator/acceptance_test.go`).

TypeScript tests cover IPC framing and UI state reduction.

## Packaging targets

`make core-all` builds:

- linux/amd64
- windows/amd64
- darwin/arm64
- darwin/amd64

The extension locates `extension/bin/relo-core-${os}-${arch}`. In development it can `go run ./cmd/relo-core` from `core/`.
