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

Indicators: high utilization, repeated tool signatures, repeated failed edits, stalled progress, obsolete phase material, transcript bloat.

When `relo.context.autoRefresh` is on:

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

## Inspector

The Context view shows generation, usage, included/excluded items, and composition by class. Manual controls: refresh, rebuild, pin, remove, promote to project memory, view previous generation.

Pinned user constraints survive automatic compression. Mandatory/pinned content can exceed budget; generation records overflow instead of silently claiming a fit.

## Retrieval

`Retrieve(task, files, symbols, keywords, role, phase, failures)` scores the pool afresh per build. Finding verification updates only the exact finding identity. Finding promotion stores a structured finding in the context engine pool; separate explicit promotion or orchestrator memory persistence writes project memory.
