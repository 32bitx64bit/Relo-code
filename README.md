# Relo Code

Relo Code is a **context-managed multi-agent AI software engineering system** for Visual Studio Code.

Relo is not a chat log with extra buttons. It treats **model context as mutable computational state**. Agents keep their identity while Relo rebuilds, compresses, replaces, and garbage-collects what the model actually sees.

```text
Variable Context Engine  →  Multi-Agent Orchestrator  →  OpenCode runtime  →  Models + tools
```

- **Relo orchestrates.** Task graphs, agent hierarchy, context generations, memory, verification, worktrees.
- **OpenCode executes.** Provider access, model calls, tools, filesystem, shell.
- **VS Code presents.** Chat, trees, diffs, approvals, settings.

## Why variable context matters

Long-running agents rot because transcripts only grow:

```text
request + exploration + tool dumps + stale plans + failures + more messages
```

That produces forgotten constraints, repeated mistakes, circular reasoning, and wasted tokens. Relo constructs a fresh **context generation** from durable task state, verified findings, relevant code, and recent failures — not from the entire history.

## Architecture

```text
Visual Studio Code  --stdio JSON-RPC-->  Relo Core (Go)
                                              |
                                              | HTTP + SSE (documented OpenCode API)
                                              v
                                        OpenCode serve
```

See [docs/architecture.md](docs/architecture.md) and [docs/context-engine.md](docs/context-engine.md). Agents work in Git worktrees by default; virtual workspaces, an opt-in alternative that lays files out only when a tool needs them, are described in [docs/virtual-workspaces.md](docs/virtual-workspaces.md). A proposal for running part of the work on a native runtime next to OpenCode is in [docs/hybrid-runtime.md](docs/hybrid-runtime.md). How Relo tells which models can see images, and how agents view screenshots, is in [docs/vision.md](docs/vision.md). Running agents on your installed Claude Code CLI, with its own sign-in, is in [docs/claude-code.md](docs/claude-code.md).

## Quick start

On Linux, with Go, Node.js 20+, VS Code and `opencode` 2.x installed:

```sh
make dev        # build, package and install the extension into VS Code
make preflight  # read-only environment checklist
```

Full guide (install, models, free smoke-test model `opencode/cat-jam-free`, mock runtime, logs, reporting issues): [docs/quickstart.md](docs/quickstart.md).

Go version is read from [core/go.mod](core/go.mod) (currently Go 1.25). Other requirements: Node.js 20+, VS Code 1.90+, Git (for worktrees), OpenCode on `PATH` for live models (`relo.runtime=mock` works without it).

## Development

```text
make dev      # build + package + install into VS Code (Linux)
make vsix     # build .vsix only (no install)
make build    # core + extension + webview
make test     # go test ./... and TypeScript tests
make lint     # go vet, gofmt check, tsc
make package  # cross-compile cores + vsix
```

Launch configs:

- **Run Extension**
- **Debug Relo Core**
- **Debug Extension + Core**

Core stdio IPC is versioned (`v: 1`). Logs go to stderr; stdout is IPC only.

## Building / testing / packaging

| Command | What it runs |
|---|---|
| `make core` | Host-platform `relo-core` into `extension/bin/` |
| `make core-all` | linux-amd64, windows-amd64, darwin-arm64, darwin-amd64 |
| `make test` | Go tests including the mock acceptance scenario |
| `make package` | VS Code extension with bundled cores |

From `core/`: `go test ./...`

From `extension/`: `npm test` and `npm run compile`

## Troubleshooting

**Relo: Failed** — open **Relo: Open Logs**. Confirm `relo-core` exists (`make core`) and the workspace is writable for `.relo/relo.db`.

**OpenCode missing** — install from https://opencode.ai/docs or set `relo.openCode.path`. Relo still starts on the mock runtime.

**No models** — authenticate providers inside OpenCode, then **Relo: Restart Backend**.

**Agents stuck** — **Relo: Pause** / **Resume**, **Stop All Agents**, or **Refresh Primary Context**.

**Dirty Git files** — Relo snapshots user modifications at session start and will not apply patches that overwrite them.

## License

MIT
