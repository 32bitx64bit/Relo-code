# Variable Context Engine

Context is a variable. Relo does not feed models an append-only transcript.

## Context generations

Every Relo agent has an identity (`agent_id`, parent, role, task, scope, model, workspace, budget) that **survives** conversation replacement.

Each **generation** is a snapshot of what was actually injected:

- number, phase, strategy, token use, budget
- included items with provenance
- dropped items and reasons
- compressed items

Generation 3 of an auth worker might contain implementation files and the latest test failure, and none of the generation 1 repository grep dump. Older generations remain inspectable.

## Prompt cache layers

Prompts split into four layers. L0 is byte-identical role instructions shared across agents. L1 is session-stable objective, pinned constraints, and architecture memory, sorted by a stable key and free of timestamps, random IDs, and counters. L2 is generation findings, code, and reports in deterministic durability/content-hash order; within a generation, new items append after emitted items. L3 is volatile user input and generation status at the tail. `prompt.ComposeParts` returns `Static` (L0+L1) and `Dynamic` (L2+L3); `PrefixHash` hashes `Static` for adapters to avoid resending unchanged instructions.

Builds within a generation keep prior emitted order. Reordering, compression, and eviction occur at refresh/replace boundaries; a 10% budget hysteresis avoids small-churn eviction. A refresh intentionally misses cache for L2/L3 only; L0+L1 remain cacheable if unchanged. Track provider cache hit rate as `CacheRead / (Input + CacheRead + CacheWrite)`; a zero denominator yields zero.

`BuildRequest.Scope` optionally limits classes, excludes project memory, restricts parent reports to dependency task IDs, and clips oversized non-mandatory items to head/tail excerpts with a token marker. Exclusions carry reasons into the inspector. Scope is fixed within a generation and takes effect at build/refresh boundaries; pinned and mandatory items are never clipped. Prompt report schemas cap summaries at 1200 characters and task/report list fields by item count and length.

The target is at least 95% prompt-prefix reuse over long-lived agents. Tests estimate reuse using consecutive prompt longest-common-prefix bytes; provider cache accounting remains authoritative.

## Information classes

| Class | Survival | Examples |
|---|---|---|
| Ephemeral | dropped aggressively | grep hits, partial hypotheses |
| Task state | survives refresh | objective, acceptance, blockers, latest failures |
| Findings | structured, reusable | root cause, symbols, API behavior |
| Project memory | durable, bounded, scored | build commands, conventions, user decisions |
| Raw history | observability only | tool logs, transcripts |

## Provenance

Each item carries `source`, agent, task, time, confidence, relevance, durability, verification, files, and symbols.

Sources are distinct: user requirement, verified fact, worker finding, unverified hypothesis, generated summary, test result, repository source, parent report.

Unverified hypotheses **cannot** silently become verified memory. Promotion to memory preserves `verified=false` until an explicit verify.

## Building

`BuildContext` retrieves from the durable pool (keyword/file/symbol overlap, no required vector DB), scores by task relevance, recency, confidence, durability, role, and phase, then packs into a **budget**. Priority:

1. pinned items, user objective, task, acceptance, hard constraints
2. relevant code, verified findings, recent failures
3. architecture memory, parent reports
4. historical / optional material

A 200k-token model is not filled because the window exists. Low-priority material is dropped or compressed when the budget is exceeded.

## Compression

Compression is heuristic truncation/section extraction, not semantic-preserving summarization. It attempts to preserve engineering meaning: decisions, constraints, failures, verification, remaining risks, exact paths, symbols.

Bad: "Authentication was changed and tests were done."

Good: file path, root cause, implementation, verification commands, open risk.

Worker transcripts are compressed into **parent reports** before Primary sees them.

## Garbage collection / refresh

Indicators: high utilization, repeated tool signatures, repeated failed edits, stalled progress, obsolete phase material, transcript bloat. No indicator triggers a refresh by itself.

Refresh runs only when a turn fails with a context-limit error (`MaybeRefresh` in `core/internal/context/gc.go`, called from `core/internal/orchestrator/worker.go`). Then Relo refreshes the context:

```text
extract durable state → persist findings → drop ephemeral/history → new generation → new OpenCode session → same Relo agent
```

Restarting the model is not failure. The UI reports `Context refreshed · Generation 2 → 3 · Reason: …`.

A context-window provider error prefers this path over failing the task.

## Strategies

`relo.context.compressionStrategy`:

| Name | Behavior |
|---|---|
| `variable` (default) | class + score packing |
| `full_history` | chronological fill, including raw history, until budget |
| `summary` | keep task/findings/reports; omit the rest |
| `aggressive_refresh` | tighter budget, drop other-phase noise |
| `role_optimized` | Primary omits raw code dumps; probes omit unrelated reports |

The engine records generation count, tokens, refresh reasons, retries, and repeated actions so strategies can be compared later.

## Project memory storage and erasure

Project memory lives only in the `memory` table of the workspace catalog; it is never a project file or a separate index. Rows are versioned JSON (`schemaVersion: 2`) and only `core/internal/memory` reads or writes them: the generic persistence accessors (`PutJSON`, `Get`, `GetIn`, `List`) refuse the table, so no code path can bypass the intake, the provenance fields or the revision check-and-set.

### Guard

`persistence.Open` installs the database guard (`persistence.MemoryGuardSQL`) before the legacy import can run, and `memory.Open` installs it again after its migration. The triggers refuse every insert or update whose payload is not schema v2, whose partition is not an opaque `ws_` ID, or whose `state` is not `candidate`, `verified`, `stale`, `retired` or `deleted`. An older binary that writes a legacy payload or a raw workspace path fails at the database instead of silently reintroducing unvalidated memory.

### Legacy import

`importLegacyCatalog` never copies `memory` rows with `copyRows`. It hands them to the intake registered by `core/internal/memory` (`EnsureLegacyMemoryIntake`), inside the same catalog transaction, so the import either commits only schema v2 candidates with an opaque partition or rolls back. Rows that fail intake (unsafe content, over-limit strings, invalid ID or path) are dropped and counted; a payload that is not JSON fails the whole import. The guard is lifted for that one transaction and reinstalled before the commit. If the intake is not registered, the import fails rather than copying raw rows.

### WAL and free pages

Deleting or rewriting a row does not erase the bytes from earlier files, so the policy is explicit and deliberately not a claim of erasure:

| Step | When | Effect |
|---|---|---|
| `PRAGMA secure_delete=ON` | catalog open, and every `memory.Open` | future writes of this connection overwrite the cells they free instead of leaving them on the free list |
| `VACUUM` | `memory.Open`, best effort | rewrites free pages where SQLite performs it; under modernc with `journal_mode=wal` it is a no-op |
| WAL checkpoint, then `PRAGMA wal_checkpoint(TRUNCATE)` | `memory.Open`, best effort | copies the WAL into the main file and truncates the WAL to zero, so removed payloads are not left in `catalog.db-wal` |

Residual exposure: pages written before the catalog adopted `secure_delete`, and pages SQLite has not rewritten since, can still be read from `catalog.db` by anyone with the file. Backups made before this policy can contain the same payloads. Migration cannot honestly guarantee erasure from old files or backups; deleting the catalog and its `-wal`/`-shm` files is the only complete removal. No new backup containing unsafe payloads is created.

## Inspector

The Context view shows generation, usage, included/excluded items, and composition by class. Manual controls: refresh, rebuild, pin, remove, promote to project memory, view previous generation.

Pinned user constraints survive automatic compression. Mandatory/pinned content can exceed budget; generation records overflow instead of silently claiming a fit.

## Retrieval

`Retrieve(task, files, symbols, keywords, role, phase, failures)` scores the pool afresh per build. Finding verification updates only the exact finding identity. Finding promotion stores a structured finding in the context engine pool; separate explicit promotion or orchestrator memory persistence writes project memory.

## Recalled project memory

`Settings.Memory` (`relo.context.projectMemory`) gates recall for the Primary (`seedPrimary`) and for Worker, Debug and Integration agents (`workerSeed`). Probes and Reviewers are excluded by their `style` scope; Architect and Test follow their scope defaults, and `workerSeed` is the only worker path. Off, recall is empty and any prior recalled group is removed.

Each prompt recalls at most `memory.RecallLimit` (4) entries through `memory.Store.RecallScoped`, which selects only verified entries. Before rendering, each entry must pass `memory.CheckFreshness` as `current` in the recipient's own `worktreeView`; `stale` and `unknown` entries are dropped and never rendered. Command evidence older than 30 days marks its entry stale for every view. A view-specific hash or ancestry mismatch only drops the entry for that view.

Recalled items enter as `ctxeng.SourceRecalled` (`recalled_memory`), never `verified_fact`, unverified, and headed by `Untrusted historical project observation; verify before use.` with ID, observation date, evidence paths and the body quoted. `Engine.SetRecall` replaces the previous group on every prompt. `ctxeng.CapRecall` bounds the combined rendered block to 300 estimated tokens (`ctxeng.Tokens`) and 1200 bytes, skipping any item that does not fit whole.
