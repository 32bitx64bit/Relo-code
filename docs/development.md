# Development

## Layout

```text
core/                 Relo Core (Go)
  cmd/relo-core/
  internal/context    variable context engine
  internal/orchestrator
  internal/opencode   documented HTTP/SSE adapter + process manager
  internal/runtime    AgentRuntime interface + mock
  internal/vws        Virtual Workspace Engine (standalone, Git-object-backed)
  internal/workspace  worktree and virtual workspaces behind one Manager and WorkspaceView
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

`RELO_TEST_WORKSPACE_MODE=worktree|auto|virtual` runs the app, orchestrator and workspace suites with agents' copies held another way, to show behavior does not depend on it (fish: `env RELO_TEST_WORKSPACE_MODE=virtual go test -race ./...`). `core/internal/vws` is a standalone package with its own tests and needs no worktree. The workspace benchmark is skipped unless asked for; see [virtual-workspaces.md](virtual-workspaces.md#benchmarks).

TypeScript tests cover IPC framing and UI state reduction.

## Packaging targets

`make core-all` builds:

- linux/amd64
- windows/amd64
- darwin/arm64
- darwin/amd64

The extension locates `extension/bin/relo-core-${os}-${arch}`. In development it can `go run ./cmd/relo-core` from `core/`.
