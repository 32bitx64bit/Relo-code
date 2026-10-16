# Architecture

Relo Code is a VS Code product with a Go core and an OpenCode execution backend.

## Layers

```text
VS Code extension (TypeScript)
        typed stdio JSON-RPC v1
Relo Core (Go)
        Variable Context Engine
        Orchestrator / task graph / scheduler / routing
        Workspace worktrees / integration / verification / SQLite
        OpenCode adapter
OpenCode server (HTTP + SSE)
        models, tools, files, shell
```

Relo does **not** fork OpenCode and does **not** let OpenCode own orchestration. OpenCode receives a curated system prompt and a task. Relo owns whether that session continues, is aborted, or is replaced by a new OpenCode session under the same Relo agent id.

## Responsibility

| Layer | Owns |
|---|---|
| Relo | context generations, memory, findings, task graph, scheduling, model routing, worktrees, merge policy, verification policy, persistence, IPC |
| OpenCode | provider auth, model execution, tool loop, filesystem/shell tools |
| VS Code | UI, diffs, commands, approvals, settings |

## Runtime lifecycle

```text
VS Code activate
  → spawn relo-core --stdio
  → initialize (settings + workspace)
  → detect OpenCode or mock
  → health + model discovery
  → restore last SQLite session
  → Ready
```

On shutdown Relo persists sessions, agents, tasks, findings, and memory. Running agents become `interrupted` after reload and can be retried. Secrets are never stored in SQLite; provider credentials stay with OpenCode.

## Agent hierarchy

Dynamic, not a fixed org chart. Default roles:

- **Primary** — engineering director. Thinks. Does not mechanically edit.
- **Department head** — optional subsystem coordinator.
- **Probe (tier 2)** — reconnaissance, structured findings.
- **Worker / architect / debug / integration (tier 1)** — implementation.
- **Reviewer / test** — independent verification.

Scheduler enforces `relo.maxConcurrentAgents`, tier caps, and hierarchy depth. A task that does not fit shows as a queued agent saying which limit it waits on (at most four wait at a time), and takes a slot the moment one frees, the agent that has waited longest first; a request does not wait for a whole batch of workers to finish before the queue moves. When a run ends with agents still waiting they are retired, not left queued. Duplicate mutating work is isolated with Git worktrees under `.relo/worktrees/<agent-id>/`.

## Primary loop

1. Build Primary context from objective, decisions, task graph, reports, memory.
2. Call OpenCode with the Primary system prompt and a JSON schema.
3. Materialize the returned task graph.
4. Run ready tasks (probes before implementers).
5. Each worker gets its own context generation (findings in, transcripts out).
6. Worker reports compress before they enter Primary context.
7. Verification and optional review.
8. Re-prompt Primary until `completed` or blocked.

## Persistence

SQLite at `<workspace>/.relo/relo.db` (WAL). Tables: sessions, agents, tasks, generations, findings, memory, reports, usage, changes, verification, events.

## IPC

Newline-delimited JSON frames: `req` / `res` / `err` / `evt`. See [protocol.md](protocol.md).
