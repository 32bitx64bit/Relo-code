# Virtual workspaces

Agents do not need their own physical copy of the repository. They need their own consistent view of it, and a real directory only at the moments a tool needs real files. The Virtual Workspace Engine (VWE) gives each mutating agent a logical workspace held in Git's object database, and lays files out on disk only for execution. Cognitive state (what the agent knows), repository state (what the agent has) and execution state (the directory a tool runs in) are kept apart and linked by stable revision ids.

It is on by default. Relo cuts virtual workspaces for every isolated agent whose repository allows it; worktrees, the opt-in alternative, are described in [worktrees.md](worktrees.md) and are used when `relo.workspace.mode` says otherwise or when a virtual workspace cannot be used.

## Settings

| Setting | Values | Default | Meaning |
|---|---|---|---|
| `relo.workspace.mode` | `worktree`, `virtual`, `auto` | `virtual` | `worktree`: one Git worktree per agent, unchanged, opt-in. `virtual`: a virtual workspace for every isolated agent whose repository allows it. The default. `auto`: virtual where the repository allows it, a worktree otherwise. |
| `relo.workspace.materialization` | `automatic`, `worktree` | `automatic` | How a directory is made when tools need real files. Both values use a detached Git worktree today; the setting exists so a cheaper backend can be added without a migration. |
| `relo.workspace.virtualReads` | boolean | `false` | Serve the model's file reads from the overlay instead of a directory. It only takes effect where the runtime guarantees every file tool is routed through Relo. OpenCode 2.x does not, so the setting is ignored with one notice. |

A change applies to workspaces created afterwards. Existing workspaces keep the representation they were made with, and virtual workspaces left by an earlier run are adopted whatever the mode is now.

`virtual` and `auto` fall back to a worktree when the repository would make a view differ from a checkout: `core.autocrlf` (`true` or `input`), `core.eol=crlf`, a `core.attributesfile` or `.git/info/attributes` or tracked `.gitattributes` that sets `text`, `eol`, `filter`, `ident` or `working-tree-encoding`, or any configured `filter.*.clean|smudge|process` (Git LFS). A worktree and a view of such a repository would hold different bytes for one file, and a three-way merge of one against the other would invent conflicts. A repository that cannot open the engine for any other reason also falls back. In `virtual` mode the user is told once per reason (`workspace.fallback`); `auto` falls back silently, because choosing the better of the two is what it is for.

## How a workspace is held

A workspace is three layers resolved in order: the agent's current overlay, its latest committed branch tree, and the immutable base commit it was cut from. The overlay is a full Git tree that already contains the effect of the other two layers, replaced (never edited) on each write. A deleted path is simply absent from it, so it can never fall back to the base; the tombstone is structural. All objects live in the repository's own object database, so a file is stored once however many agents read it.

```text
refs/relo/vws/<agent>/overlay   checkpoint commit: tree = overlay, message = workspace metadata
refs/relo/vws/<agent>/base      the base commit, pinned for the life of the workspace
refs/relo/vws/<agent>/exec      the commit an open directory was cut from
refs/relo/vws/<agent>/snap/<n>  revision n, pinned while a proposal or another consumer needs it
refs/heads/relo/<agent>         the agent's branch, created on its first commit
```

Every published revision is one atomic multi-ref update, so a crash leaves the old revision or the new one. The content and the metadata that describe it are the same commit and cannot disagree. Objects are fsynced before the ref that publishes them. Git garbage collection cannot remove an object a live overlay, a pinned revision or an open directory needs, because each is reachable from a ref. The user's checkout, index, `HEAD` and branches are never touched, and nothing is pushed.

The engine is a standalone package (`core/internal/vws`) that works without worktrees and without the rest of Relo. `core/internal/workspace` puts it behind the same `Manager` the worktree code uses, and exposes both representations as one `WorkspaceView` (read, write, delete, stat, list, changes, patch, base file, identity, and an integration `Source`).

### Execution

A tool that needs real files calls `Materialize`, which lays the current revision out in `.relo/worktrees/<agent>` and pins it. While the directory exists it is the authority for that agent: direct overlay writes are refused (`ErrMaterialized`) so the model's view and the tool's view cannot diverge. `Capture` imports what the tools did as one new revision: `git add -A` and a diff against the materialized commit, then a tree built from the result. A capture rejects, and reports once, protected paths, paths the engine was told to ignore, files over the size limit, links that leave the workspace and nested repositories. It is idempotent and can repeat while the directory stays open. `Settle` captures and removes the directory; if the capture fails the directory is kept so nothing is lost.

Every reader of an agent's files imports first, so a diff or a read never misses what a compiler produced. A single-file read from an open directory (`ReadLive`) reads that file from the directory without scanning the rest, when the overlay already holds the path; anything it cannot answer exactly as an import would (a new file, a link, an ignored path) is left to a capture.

### Proposals, accept, reject, undo

When an agent finishes, `Proposed` captures, commits the overlay on the agent's branch and pins the revision. Each change records that revision (`Change.Rev`), so accepting reads exactly what was proposed however the workspace moves afterwards, including across a restart. The integration engine is not rewritten: it reads the agent's side through a `Source` (a directory or a pinned snapshot) and everything else is the code described in [worktrees.md](worktrees.md): the three-way merge, rules, `needs` and `lineage`, conflict classes, the journal and undo, retained rejected blobs. The only difference visible to a user is that an agent's files are not on disk between runs.

A retried accept is safe after a stop between the journal write and the stored decision: the retry sees that the file already holds the agent's bytes and keeps the first journal entry, so undo still restores the user's original.

### Stacking and dependents

An agent that depends on another agent's unaccepted work starts from the parent's patch applied to the same base (`Stack`), `Recut` moves a base, and `RebaseOnto` replays an agent's edits onto a new base reporting conflicts. These are the operations the worktree code performed with `git apply`, done on trees. A dependent is rebased explicitly, never because the parent's workspace changed.

## Context consistency

The context engine ties what an agent knows to the revision it holds.

- Each generation records a `WorkspaceContextRef{agentId, baseCommit, overlayRevision, effectiveTreeOid}`.
- File-derived items carry the Git blob id of the bytes they were derived from. At a context boundary (a new generation, a refresh, a context-limit refresh) each is compared with the file now. A stale excerpt is dropped when it is replaceable and otherwise rendered with a "Stale" marker; a deleted file makes its excerpt stale; a file that cannot be read leaves its excerpt alone. Mid-run builds do not re-check, so the cached prompt prefix stays stable.
- Refreshing context preserves the agent's edits and rebinds the new generation to the current revision.
- No other agent's code enters a context implicitly. Items derived from another workspace are rejected unless they are brought in explicitly, and a forked agent's inherited items are marked imported.

`context.stale {agentId, items, paths}` is emitted when a boundary found stale excerpts. Agents that work in the user's folder have no workspace ref.

## Messaging

Agents can still message each other as text. A message can also carry a `ChangeReference{senderAgent, branch, commit, revision, baseCommit, paths}`: a pointer to files the sender changed, pinned to the sender's snapshot commit and verified when it is resolved. It is a pointer and not a delivery. The recipient sees the paths and a note that they are not in its workspace, and takes files only with an explicit `import` action naming the paths it wants; they are written into the recipient's own isolated workspace (or its open directory, then captured) and nowhere else. Selective visibility is the default and the only mode.

The `agents` tool of the `relo-agentmsg` MCP server accepts `paths` on `send` and a `message` plus `paths` on `import`; paths are bounded (32 per message, 512 bytes each). An import is refused for a read-only recipient, a stale writer, a reference whose commit no longer matches, and a path that is not a change the sender made. A sender in a worktree can send text only; an agent in the user's folder cannot import.

## Security

- Paths are canonicalised; traversal, absolute paths, NTFS and HFS lookalikes of `.git`, and the top-level `.relo` folder are refused for read, write and capture.
- Symlinks are never followed out of a workspace. A link can be stored and read when it resolves inside the tree; nothing is written through one, an import never follows a link planted by a tool, and a captured link that escapes is not imported (the user is told).
- A read-only workspace rejects every mutation and imports nothing.
- The engine writes only under `refs/relo/**`, the object database and `.relo/`. The user's working tree changes only through accept, which goes through the journal and a compare-and-swap, and through undo. There is no implicit push.
- Unsaved editor buffers block accept, accept-all, resolve and auto-apply exactly as before; the core cannot see buffers without the extension.
- Repositories that rewrite content stay on worktrees (above).

## Persistence and recovery

Git is the source of truth. `Recover` rebuilds every workspace from the refs alone at start, adopts a directory left by an interrupted run and imports what its tools had written, and reports damaged workspaces without touching the rest. A ref that was moved by someone else is reported, never overwritten.

SQLite holds a mirror of each workspace (`vworkspaces`: workspace root, agent, session, base commit, revision, effective tree, state, JSON payload) so the UI and diagnostics can list them without running Git. It is rebuilt from Git, never the other way round, updated monotonically on each revision, and reconciled once at start: workspaces Git holds are mirrored, and a mirrored one that Git no longer holds is forgotten, with one notice only if it held edits nobody had decided on. The table is additive, created idempotently, and needs no schema version. Every revision is also written to the durable event log as `workspace.<kind>` (`create`, `write`, `delete`, `rename`, `chmod`, `symlink`, `commit`, `rebase`, `capture`, `discard`, `dispose`). These are logged, not sent over IPC.

Workspaces are released with their last decided change, and swept at start when no stored change refers to them.

## Runtime compatibility

- **Mode B, materialized** (what runs today): a tool that needs real files gets a directory, and its edits are imported by the next reader or at settle. Shell, build and test output is captured like any other edit.
- **Mode A, virtual tools** (`virtualReads`): the model's file tools read and write the overlay with no directory. This needs a runtime in which every file tool is routed through Relo, with no bypass. OpenCode 2.x runs its own file tools, so the setting is gated off for it; the code path and its tests exist.
- **Fallback**: worktrees, always available, and the fallback when a virtual workspace cannot be used.

## What is tested

The 15-row correctness matrix, and where each row is pinned down. `RELO_TEST_WORKSPACE_MODE=worktree|auto|virtual` runs the whole app, orchestrator and workspace suites in another mode, to show behavior does not depend on how files are held; the three modes are green under `-race`. `app/vwe_matrix_test.go` runs the end-to-end rows with virtual workspaces forced on.

| # | Row | Tests |
|---|---|---|
| 1 | Separate files | `vws.TestAgentsAreIsolatedAndTheUserCheckoutIsUntouched`, `workspace.TestVirtualModeReportsTheSameChangesAsAWorktree` |
| 2 | Same file | `app.TestMatrixTwoAgentsEditingTheSameFile` |
| 3 | Add, delete, rename | `app.TestMatrixAddDeleteAndRenameLandAndUndo`, `vws.TestRenameChmodAndReplacement`, `vws.TestDeletionsAreTombstonesAndLayersAreReported` |
| 4 | Concurrent user modification | `app.TestMatrixUserEditsWhileTheAgentWorks`, `workspace.TestMergeFromAnOverlayKeepsTheUsersConcurrentEdits` |
| 5 | Unsaved buffers | `app.TestMatrixUnsavedBufferBlocksAcceptAndKeepsTheProposal` (the extension's dirty gate is tested in `extension/`) |
| 6 | Interrupted overlay writes | `vws.TestAFailedPublishLeavesThePreviousRevision`, `vws.TestBatchesAreAtomic`, `vws.TestConcurrentWritersToOneOverlayAreSerialised` |
| 7 | Crash during commit or integration | `app.TestMatrixCrashDuringAcceptIsRetriedSafely`, `workspace.TestRetriedJournalKeepsTheOriginalPreImage`, `vws.TestInterruptedMaterializationIsRecovered` |
| 8 | Dependent revisions | `vws.TestStackMakesParentWorkTheBaseOfADependent`, `vws.TestRebaseOntoKeepsEditsAndReportsConflicts`, `workspace.TestVirtualStackRecutAndDiscardMatchWorktreeBehavior`, plus the dependents suites in `app` and `orchestrator` in all three modes |
| 9 | Messaging during edits | `orchestrator.TestAMessageCanPointAtFilesAndTheRecipientImportsOnlyWhatItChooses`, `vws.TestImportIntoARunningDirectoryIsCapturedLikeToolOutput` |
| 10 | Context refresh | `orchestrator.TestContextRefreshPreservesEditsAndRebindsToTheCurrentRevision`, `context.TestStaleExcerptsAreDetectedFlaggedAndDroppedAtTheNextBoundary` |
| 11 | Compiler-generated files | `vws.TestCaptureImportsToolEditsAndSkipsWhatMustNotBeImported`, `vws.TestToolOutputNeverReachesTheBaseOrOtherAgents`, `workspace.TestVirtualViewShowsWhatToolsWrote` |
| 12 | Symlinks and protected paths | `vws.TestSymlinksAreNeverFollowedOutOfTheWorkspace`, `vws.TestPathSafety`, `vws.TestImportRefusesALinkThatLeavesTheWorkspace`, `workspace.TestSourceCheckRefusesPathsThroughLinks` |
| 13 | Binary conflicts | `app.TestMatrixBinaryFiles`, `app.TestCheckChangeBinaryOverlapConflicts` |
| 14 | Restart and recovery | `app.TestMatrixRestartKeepsAProposalPinned`, `app.TestWorkspaceSurvivesAnInterruptedRun`, `vws.TestRestartPreservesEveryWorkspace`, `workspace.TestVirtualWorkspacesSurviveARestartAndAreAdoptedInAnyMode` |
| 15 | GC with live overlays | `app.TestMatrixGCKeepsEverythingLiveWorkNeeds`, `vws.TestGitGarbageCollectionKeepsLiveOverlays`, `vws.TestMaterializedRevisionSurvivesGarbageCollection` |

Other guards: `vws.TestNativeTreesMatchGitWriteTree` and `vws.TestOverlayDiffMatchesAnEquivalentWorktree` compare the engine's trees and diffs against Git's own on the same edits, and `workspace.TestViewsOfIdenticalEditsHaveTheSameTree` shows a worktree agent and a virtual agent are interchangeable witnesses of one state.

## Benchmarks

`TestVWEBenchmarkReport` (skipped unless `RELO_VWE_BENCH` is set) builds a small and a large repository and compares worktree and virtual workspaces on workspace creation, agents editing, large generated output, integration, and repeated context refreshes:

```text
env TMPDIR=/some/real/disk RELO_VWE_BENCH=1 go test -run TestVWEBenchmarkReport -count=1 -timeout 60m ./internal/app
```

`RELO_VWE_BENCH_FILES` (default 20000), `RELO_VWE_BENCH_AGENTS` (default `1,4,16`), `RELO_VWE_BENCH_REPS` (default 3) and `RELO_VWE_BENCH_OUT` (a file for the report) tune it. Point `TMPDIR` at a real disk: a tmpfs hides what a checkout costs.

### Results

One run on a Linux machine (16 CPUs, Git 2.56, Go 1.27) with the repositories on a btrfs SSD, a warm page cache, three repetitions per cell (median shown), 20 October 2026. Treat the numbers as the shape of the trade, not as a promise: they are one machine and one filesystem. *Checkout MB* is the apparent size of the files under `.relo/worktrees` and *git commands* counts every git process the workspace layer and the engine started; filesystem work is counted as entries created, not as system calls. The "virtual, tools in a materialized dir" rows are what runs today, because OpenCode 2.x's own file tools need a real directory. The "edits through the view" rows are what Mode A would cost and are not reachable from OpenCode 2.x. Large repository: 20,000 files, 81 MB.

**Creating workspaces (large repository)**

| agents | mode | sequential ms | parallel ms | checkout MB | checkout entries | `.git` growth MB |
|---|---|---|---|---|---|---|
| 1 | worktree | 641 | 612 | 81.1 | 20,407 | 1.7 |
| 1 | virtual | 20 | 22 | 0 | 0 | 0.00 |
| 4 | worktree | 2,743 | 2,679 | 324.2 | 81,625 | 6.8 |
| 4 | virtual | 47 | 49 | 0 | 0 | 0.00 |
| 16 | worktree | 10,743 | 10,711 | 1,296.8 | 326,497 | 27.1 |
| 16 | virtual | 172 | 162 | 0 | 0 | 0.01 |

A virtual workspace is a handful of refs: creation is flat in the size of the repository and costs no checkout. In the 60-file repository the two are equal (about 10 ms per agent).

**N agents edit five files and add one, then every change is decided (large repository)**

| agents | mode | work ms | propose ms | release ms | total ms | peak checkout MB | steady checkout MB |
|---|---|---|---|---|---|---|---|
| 1 | worktree | 634 | 273 | 339 | 1,245 | 81.0 | 81.0 |
| 1 | virtual, tools in a dir | 679 | 661 | 4 | 1,344 | 81.0 | 0 |
| 1 | virtual, edits through the view | 172 | 25 | 4 | 201 | 0 | 0 |
| 4 | worktree | 2,697 | 835 | 1,352 | 4,884 | 324.1 | 324.1 |
| 4 | virtual, tools in a dir | 2,720 | 2,230 | 20 | 4,970 | 324.1 | 0 |
| 4 | virtual, edits through the view | 620 | 75 | 17 | 713 | 0 | 0 |
| 16 | worktree | 10,770 | 2,565 | 5,477 | 18,812 | 1,296.5 | 1,296.5 |
| 16 | virtual, tools in a dir | 10,897 | 9,560 | 70 | 20,528 | 1,296.5 | 0 |
| 16 | virtual, edits through the view | 2,511 | 275 | 76 | 2,861 | 0 | 0 |

An agent that runs tools needs a checkout, so while it works a virtual workspace costs what a worktree costs: peak disk is identical. Where the virtual workspace wins is afterwards. A finished agent whose changes wait for a decision holds nothing on disk (0 MB against 1.3 GB for 16 agents), and the end-to-end time is about the same (virtual 9% slower at 16 agents, 2% at 4, 8% at 1: importing a directory and removing it costs about what a worktree's release does, plus a capture). An agent that does not need a directory at all, which is the case Mode A serves, finishes the same work in 15% of the time with no checkout.

**The same on the 60-file repository: worktrees win**

| agents | mode | total ms | git commands |
|---|---|---|---|
| 1 | worktree | 33 | 8 |
| 1 | virtual, tools in a dir | 114 | 32 |
| 4 | worktree | 83 | 32 |
| 4 | virtual, tools in a dir | 403 | 107 |
| 16 | worktree | 359 | 128 |
| 16 | virtual, tools in a dir | 1,736 | 407 |

When a checkout is a few dozen files the virtual path's extra git commands (a capture, a commit, a pinned snapshot, a diff, several ref updates) cost more than the checkout they save: about 5x slower end to end, in absolute terms 80 ms per agent. Against a model run that takes tens of seconds this is small, but it is not a saving. The same 60-file repository costs the virtual view path 25 ms per write on this disk, against about 2 ms on a tmpfs: every revision fsyncs the objects it creates before the ref that publishes them, and the setting that would skip that (`NoFsync`) is deliberately not exposed.

**A tool writes 3,000 unignored files of 8 KB (23.4 MB)**

| repository | mode | propose ms | checkout MB after | `.git` growth MB |
|---|---|---|---|---|
| small | worktree | 292 | 23.7 | 0.64 |
| small | virtual, tools in a dir | 676 | 0 | 0.46 |
| large | worktree | 449 | 104.5 | 2.3 |
| large | virtual, tools in a dir | 1,137 | 0 | 0.46 |

Large generated output is the case the design expected worktrees to win on time, and they do: 2.3x to 2.5x faster to propose, because a worktree has nothing to import. The virtual workspace drops the directory afterwards.

**Integration (large repository; agents edit one 2,000-line file)**

| agents | mode | propose ms | accept all ms | total ms | merged correctly | same line: accepted / refused |
|---|---|---|---|---|---|---|
| 4 | worktree | 596 | 1,525 | 2,253 | yes | 1 / 3 |
| 4 | virtual, tools in a dir | 2,279 | 128 | 2,402 | yes | 1 / 3 |
| 16 | worktree | 2,907 | 6,462 | 9,369 | yes | 1 / 15 |
| 16 | virtual, tools in a dir | 9,810 | 637 | 10,382 | yes | 1 / 15 |

The merge itself costs the same in both modes and gives the same result, including which agents are refused. An accept is much faster for a virtual agent only because its directory was already removed at settle; the worktree pays the deletion of its 20,000-file checkout when its last change is decided. The totals are the fair comparison: virtual is 7% to 11% slower here.

**Repeated context refreshes (25 file reads through the agent's view, 100 times)**

| state | microseconds per read (small / large repository) |
|---|---|
| worktree | 26 / 25 |
| virtual, no directory laid out | 38 / 29 |
| virtual, tool directory open | 12 / 13 |

Reads from the overlay go through a long-lived `git cat-file --batch` and cost a little more than a file read. While a tool directory is open, a read of a tracked file goes straight to the directory (`ReadLive`). Before that fast path every such read imported the whole directory first: 3 ms per read with 60 files and 9 ms with 1,500, which would have made a context refresh in a large repository cost a scan per excerpt.

### What this shows

- Lower filesystem overhead is demonstrated where it was claimed: workspace creation is 30x to 60x faster on a large repository and creates no files; finished and idle agents hold no checkout (0 MB against 81 MB per agent); `.git` growth is orders of magnitude smaller (27 MB against 0.01 MB for 16 agents).
- It is not demonstrated for an agent that is running tools. That agent has a checkout either way, and on a small repository the virtual path is slower.
- No benchmark here measures a model run, so none says how much of an agent's wall time any of this is. Creation and release happen once per agent; for a run of seconds to minutes, 600 ms is noise on a large repository and the checkout's disk space is the real cost.

## Limits

- Both materialization backends use a Git worktree. A virtual agent that is running tools has a checkout just as a worktree agent does; what it saves is the checkout of every agent that is not running tools (queued, finished and waiting for a decision, reading, reviewing) and the cost of creating them.
- Mode A is not reachable from OpenCode 2.x, so `virtualReads` does nothing there.
- Message references need a virtual sender. An import into a recipient does not add `needs`/`lineage` entries; a recipient that depends on a sender's work is still expressed through the task graph.
- Per-write cost through the view is dominated by the fsync of the objects each revision creates, which is real on a journaling or copy-on-write disk (see the benchmark). The engine has a `NoFsync` option that the settings do not expose.
- Repositories with line-ending rewriting, attributes that change content, or clean/smudge filters stay on worktrees.
- A checkout cheaper than a Git worktree (reflink or overlay mounts) is not implemented; the `Materializer` interface is the place for it.
