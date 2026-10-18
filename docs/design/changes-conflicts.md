# Changes tab: conflict-minimizing architecture

Status: proposal, awaiting approval. No code is changed by this document.
Scope: how an agent's pending change relates to the user's workspace, to sibling agents' changes and to its own dependencies, from worktree cut to accept or reject.

Evidence labels used throughout:

- **[measured]** read from the workspace database `/home/gavin/Desktop/Relo code/.relo/relo.db` (copied to `/tmp/opencode`, WAL included), re-derived while writing this document.
- **[code]** read in the repository; not observed in the data.
- **[experiment]** run in a scratch repo with git 2.56.0.
- **[hypothesis]** plausible, unmeasured. Never used as a premise for a decision without a gate.

Paths are relative to the repo root. `core/internal/` is shortened to `…/`.

---

## 1. Evidence base

### 1.1 What the data is, and is not

One workspace, 7 sessions with changes, 23 agents with changes, 115 change rows (103 from `worker`, 12 from `debug`). The data is small. Two conflict observations come from a single agent. Everything below is a sample, not a rate.

These counts are point-in-time, not a live baseline; Query 5 later returned 75 rows versus the 67 recorded here.

Cut time is approximated by `agent.createdAt` (the worktree is cut in `spawn`, `…/orchestrator/worker.go:96-139`). Decision time is the first `changes.updated` event that carries the new status. The events table logs every `changes.updated` (67 rows), so no transient state is missing.

| Fact | Value | Label |
|---|---|---|
| Change status | accepted 95, rejected 12, proposed 8 | measured |
| Changes that were ever flagged `conflict` | 2 (`core/internal/app/sessions.go`, `…/sessions_test.go`), same agent, same session, both ended `rejected`. That agent's third change (`app.go`) was accepted. | measured |
| Reason strings stored | 1 distinct: `your edits and the agent's edits overlap`. No stale-base, add/delete or binary reason ever stored. | measured |
| `Change.BaseBlob` set | 0 of 115. `Superseded` 0, `Resolving` 0. | measured |
| Changes on a path touched by more than one agent in the same session | 67 of 115 (58%). 18 of 66 `(session, path)` pairs are multi-agent. | measured |
| Of those 18 pairs, runs that overlapped in time | **1**. The other 17 were sequential rounds. | measured |
| Later-vs-earlier same-path change pairs (122) where the earlier change was **already decided** when the later agent was cut | 113, **0 conflicts** | measured |
| Same, where the earlier change was **still pending** when the later agent was cut | 9, **2 conflicts** (22%) | measured, n=9 |
| Of those 9 pending earlier changes, final status | accepted 3, rejected 6 | measured |
| Changes whose path received **another agent's accept between their cut and their own decision** ("exposed") | 3 of 115; **2 of those 3 conflicted**, 1 merged clean. The other 112: **0 conflicts**. Both conflicts are the same agent, so effective n is smaller (§10). | measured |
| Other agents' pending changes at the moment an agent was cut | median 0, p90 3, max 12; 7 of 23 agents saw any; only 8 of 27 such pending changes shared a path with the new agent | measured |
| Agent run time (agents with changes) | median 568 s, p90 1702 s | measured |
| Finish to accept | median 517 s, p90 3081 s | measured |
| Cut to accept | median 2219 s, p90 4618 s, max 42673 s | measured |
| `relo.changes.autoApply` default | `false` | code (`extension/package.json:494`, `extension/src/settingsBridge.test.ts:23`) |
| Tasks 73; tasks with `Depends` 21; changes whose task has `Depends` 18; of those, changes whose dependency task also produced changes | **0** | measured |

### 1.2 Corrections to the probe reports

These change the design, so they come first.

1. **"Overlap between user edits and agent edits" is an unproven label.** The two stored conflicts carry the string `your edits and the agent's edits overlap` (`…/integration/threeway.go:108`). The database does not record user edits. It does show that between the agent's cut and the conflict being seen, **3 sibling agents' changes to `sessions.go` and 2 to `sessions_test.go` were accepted** into the workspace, one of which was already pending when the agent was cut. Sibling accepts are a sufficient explanation. The message wording hides the cause. [measured]
2. **Hunk-level merging already exists; what is missing is salvage and localization.** `git merge-file` merges non-overlapping hunks of one file (`…/integration/threeway.go:159`). That is why 94 of 95 accepts succeeded with 59 of them on multi-agent paths. What the code does on a conflict is discard the whole result: `mergeFile` returns `(nil, false, nil)` when exit code is 1–127 (`threeway.go:167-169`) and `merge` refuses the file (`threeway.go:107-109`). [experiment] confirms git still produces the merged clean hunks when it exits 1: with `ours` changing line 1, `theirs` changing lines 1 and 8, the output contained the conflict block for line 1 **and** the cleanly merged line 8. The conflicting-hunk count is not stored anywhere, so how often a "conflict" had clean hunks is unknown.
3. **`BaseBlob` is read but never written.** `…/app/conflicts.go:300-313` reads `Change.BaseBlob`; nothing assigns it (`grep` over `core/`; 0 of 115 rows). The "rebase stores a base blob" path exists only as a dead read.
4. **The patch-based integration path is dead code, and the docs describe it as live.** `Coord.Integrate` (`…/orchestrator/integrate.go:10`) has no callers. `integration.Apply/ApplyAgainst/Detect` are reached only from `Integrate` and tests; `Overlap` and `CopyFile` have no production callers; `Manager.UserProtected` (`…/workspace/manager.go:359`) is referenced only by a test. `docs/worktrees.md:22` says integration "refuses patches that overwrite" dirty and untracked user paths. The live accept path (`mergeChange` → `integration.Merge`, `…/app/sessions.go:403`) never consults `Snapshot.Dirty/Untracked`; it protects user edits by 3-way merging, and refuses only `.git`/`.relo`, symlink escapes and true overlaps (`threeway.go:47-55`, `125-128`).
5. **The stacked-dependency path is unexercised in the data.** Probe statement "Changed() diffs against the stacked base" is correct (`…/workspace/manager.go:253-263`, `295`). But 0 of 115 changes had a dependency that also produced changes, so every stacked-base hazard below is **[code]**, not **[measured]**.

---

## 2. Root-cause taxonomy, ranked by evidence

Ranking is by strength of evidence that the cause produced or would produce a conflict or a lost change, not by imagined severity. "Cause" creates a conflict; "amplifier" makes it more expensive; "gap" is a data-safety hole that is not a conflict at all.

| Rank | ID | Root cause | Type | Evidence | Where |
|---|---|---|---|---|---|
| 1 | **R1** | **Frozen base.** An agent's base is a commit chosen at spawn and never moved. Sibling edits that were pending (R1a) or accepted after the cut (R1c) are absent from it. R1b below makes it worse. | cause | **measured**: both conflicts; 22% (2/9) when an earlier same-path change was pending at cut vs 0% (0/113) otherwise | `…/workspace/manager.go:183`, `210`, `215`; `…/orchestrator/stack.go:27-76` stacks only declared dependencies |
| 1b | R1b | **Base is per-round, not per-agent-start.** `refreshSnapshot` runs once per request and once per round (`…/orchestrator/session.go:362`, `824`; `facts.go:17-23`). Tasks admitted later in the same `dispatch` loop (`…/orchestrator/dispatch.go:102-167`), including agents that queue holding a worktree (`dispatch.go:20-22`, `133-136`), are cut from the round-start snapshot. If a dependency was **accepted** mid-round (always so with auto-apply on), it is in neither the base (round-start) nor the stack (`stack.go:60` keeps only `proposed`). | cause | **code**. Frequency unmeasured; P1 measures it | as listed |
| 2 | **R2** | **Decision latency widens the window.** Cut-to-accept median 37 min, p90 77 min, max 11.9 h. Nothing re-evaluates a pending change while it waits. | amplifier | **measured** | `…/app/auto_apply.go` (opt-in, default off) |
| 3 | **R3** | **Conflict handling is eager, destructive and forgetful.** A conflict is a string (`coord.go:54`). Resolution is "reject and redo with the head agent": the head prompt carries the agent's file capped at 4000 bytes (`…/app/conflicts.go:22`, `139-153`), and `supersedeStale` calls `rejectChangeLocked`, which discards the file in the agent's worktree (`conflicts.go:251-279`, `sessions.go:441-447`). The supersession map is memory-only (`…/app/app.go:74`) and is lost on restart. A stored reason is sticky until rechecked. | amplifier | **measured**: 2 of 2 conflicts ended rejected, 0 resolved in place | as listed |
| 4 | **R4** | **No write ownership for same-path work.** 58% of changes sit on multi-agent paths. But only 1 of 18 such pairs ran concurrently, so scheduler-level exclusion addresses a small slice. The Task type has no file scope (`…/taskgraph/graph.go:24-46`); the Primary's schema has `deps` but no paths (`…/prompt/prompt.go:225`). | cause | **measured**, low yield | as listed |
| 5 | **R5** | **Whole-file refusal.** One conflicting hunk discards the clean hunks and gives no hunk locations. | amplifier | **hypothesis** (no hunk counts stored); mechanism **experiment**-verified | `…/integration/threeway.go:103-109` |
| 6 | **R6** | **Dependent/stacked semantics are undefined.** (a) Accepting a dependent without its parent: base contains the parent's patch, workspace does not, so a disjoint-hunk edit merges cleanly into a half-state, and a file the parent created reports "you deleted this file after the agent started" (`threeway.go:97-98`). (b) Rejecting a parent only discards the parent's file in the parent's worktree; dependents stay stacked on it (`sessions.go:441-448`). (c) A retry of the parent's task drops the old attempt's proposal and releases its worktree (`…/orchestrator/worker.go:783-809`), silently orphaning what stacked on it. (d) `acceptAll` groups by path only (`sessions.go:482-490`), so parent and dependent on different paths can land in either order or half-land. | cause | **code**; 0 of 115 observed | as listed |
| 7 | **R7** | **Observability gap.** One reason string; no kind, hunk count, merge action (`Outcome.Action`, `threeway.go:16`) or "who moved the file" is stored. This is why R1/R5 cannot be separated from user edits today. | gap (enabler) | **measured** (absence) | `…/app/sessions.go:384` returns the outcome but does not store it |
| — | **S1** | **Non-atomic workspace write.** `merge` reads `ours` at `threeway.go:58` and writes at `:96/:101/:111` via `writeLike` (`:131-140`) with `os.WriteFile`. `decisionMu` (`app.go:73`) serializes Relo's own decisions, not the user's editor. `writeLike` also takes the file mode from the agent's file, not the user's. | safety gap | **code** | `threeway.go` |
| — | **S2** | **No unsaved-buffer awareness.** The core reads disk; the extension has no dirty-document check (no `isDirty` anywhere in `extension/src`). Auto-apply (`auto_apply.go:27-43`) never asks the extension anything. | safety gap | **code** | `extension/src/extension.ts:816-841` |
| — | **S3** | **No pre-image.** The user's bytes from just before an accept are stored nowhere. The snapshot commit predates any later user edit, is a dangling `commit-tree` object (`workspace/manager.go:136`), and loses its only reference when `Release` runs `branch -D` after the last decision (`sessions.go:351-358`, `workspace/manager.go:586-601`). | safety gap | **code** | as listed |
| — | **S4** | **Snapshot failures fall back to HEAD silently** (`workspace/manager.go:99-141`, returns `s.Head` on four error paths). The agent then works on a tree without the user's uncommitted edits. | correctness gap | **code** | as listed |

Not a root cause of conflict, but must be named: **semantic conflicts that merge textually clean** (two agents edit different hunks that break each other). No design here detects them. See §10.

---

## 3. Design principles

1. **A check never writes.** Detecting a conflict, projecting a merge, and showing a diff touch only Relo's own objects.
2. **A write is a compare-and-swap with an undo record.** No workspace byte is replaced unless it is the byte Relo just read, and the pre-image is kept (§6).
3. **Conflicts are data, not errors.** They persist, are cheap to recompute, never block unrelated changes, and never delete the agent's work.
4. **The base is explicit.** Every change records the commit it was diffed against and the changes it was stacked on.
5. **Recompute only when an input changed.** Inputs are the workspace blob for the path, the base blob, the agent blob. Same inputs, same answer, no `git merge-file`.
6. **Prefer the git machinery already required.** Everything uses `git` plumbing as `workspace/manager.go` does. No new dependency in `go.mod`.

---

## 4. Target architecture

### 4.1 Decision table

| Option | Verdict | Why (repo facts) |
|---|---|---|
| **Stacked bases: workspace + pending dependency changes** | **Keep for declared dependencies. Reject extending to all pending siblings.** | Keep: reviewers and test tasks must see the code they check (`stack.go:13-16`). Reject for siblings: of 9 pending-at-cut earlier changes, 6 were later **rejected**, so stacking all would build on rejected work in 6 of 9 cases. Only 8 of 27 pending changes shared a path with the new agent (precision 0.30). Median pending at cut is 0, so the benefit is concentrated in few agents while the coupling cost (reject cascades, §5) applies to all. Instead make the *dependency* stack first-class (`Needs`) and move the base to the current workspace at start (R1b). |
| **Auto-rebase of pending changes on workspace/accept events** | **Adopt as a virtual projection. Do not rewrite agent worktrees.** | A projection `merge(workspace-now, base, agent)` is recomputed when the path's workspace blob changes. It yields early conflict detection and an exact preview. Physically rewriting the agent's worktree adds nothing for the clean case: the accept-time 3-way already yields the same bytes, and 94 of 95 accepts passed without it. It also races a still-running agent and an agent whose worktree is read at finish (`worker.go:760`). A "write merged content and store `BaseBlob`" rebase is not in the code either (§1.2 item 3). |
| **Hunk-level 3-way merge** | **Adopt localization; defer partial apply behind telemetry.** | Engine stays `git merge-file` (`threeway.go:159`, already used and tested). Add: return the hunk list and which hunks conflict. Use it to shrink the head prompt from a 4000-byte file head to the conflicting hunks. Reject auto-applying clean hunks of a conflicted file: it produces a half-applied change that may not compile. Reject writing conflict markers into the user's file: a saved or committed marker is silent corruption. Reject `git merge-tree --write-tree` as the engine: needs a newer git than the docs promise (docs state no minimum), and our changes are per file with `--no-renames` (`workspace/manager.go:419`). Reject a Go diff3 library: new dependency for no measured gain. |
| **Scheduler file-ownership leases** | **Adopt late, advisory, declared-path only.** | Only 1 of 18 multi-agent pairs was concurrent, so the measured yield is small. The scheduler already has the right shape (`Reserve`/release, `scheduler.go:99-117`; deterministic ordering in `dispatch.go:109-118`). Leases cannot be inferred: the Task has no paths and workers edit through OpenCode's tools (`docs/architecture.md`, Responsibility table), so Relo has no write-interception point. So leases are acquired on **declared** `paths` at admit, held until the agent **finishes** (not until accept: finish-to-accept median is 517 s, max much more), and convert a predicted overlap into a *soft dependency* that stacks (§4.4). |
| **First-class lazy conflict state** | **Adopt.** | Replaces `Conflict string`/`Resolving`/`Superseded` (`coord.go:54-58`) and the memory-only `conflictSet` (`app.go:74`). Stored in the existing JSON payload (`persistence/store.go:214`), so no schema migration. Gives R3 a non-destructive resolution path and gives R7 its data. |

### 4.2 Data model

Additive fields on `orchestrator.Change` (`…/orchestrator/coord.go:43`). Old rows load with zero values.

```go
type Change struct {
    // existing fields unchanged
    BaseRev      string       `json:"baseRev,omitempty"`      // commit this change is diffed against (stacked commit when Needs is set)
    Needs        []string     `json:"needs,omitempty"`        // ids of changes stacked into BaseRev; derived from Lineage
    Lineage      []Dep        `json:"lineage,omitempty"`      // dependencies applied at stack time; rules in §10 "Lineage"
    SupersededBy string       `json:"supersededBy,omitempty"` // replaces the Superseded bool; the superseded change is kept, not discarded
    Landed       string       `json:"landed,omitempty"`       // Outcome.Action recorded on accept: created|updated|merged|deleted|unchanged
    Integration  *Integration `json:"integration,omitempty"`
}

type Dep struct {
    ID        string `json:"id"`        // parent change id
    PatchHash string `json:"patchHash"` // sha256 of the parent patch as stacked
}

type Integration struct {
    State     string   `json:"state"`            // clean | conflicted | blocked | unknown
    Kind      string   `json:"kind,omitempty"`   // overlap | modify_delete | delete_modify | add_add | binary | symlink | protected |
                                                 // needs_parent | parent_rejected | parent_superseded | unsaved_buffer |
                                                 // worktree_gone (exact phrase: "worktree is unavailable") | check_failed (non-worktree checkChange error) |
                                                 // concurrent_write (I1 post-write verify mismatch) | workspace_changed (I1 retries exhausted)
    Reason    string   `json:"reason,omitempty"` // text derived from Kind and Since
    Ours      string   `json:"ours,omitempty"`   // blob id of the workspace file the check ran against; "-" if absent
    Hunks     []Hunk   `json:"hunks,omitempty"`  // {oursStart, oursLen, theirsStart, theirsLen, conflicting}
    Since     []string `json:"since,omitempty"`  // accepted change ids, or "user", that moved the file after BaseRev
    CheckedAt int64    `json:"checkedAt,omitempty"`
}
```

Wire compatibility: the JSON keys `conflict`, `resolving`, `superseded` keep being emitted, derived from `Integration`/`SupersededBy`, until the webview (`webview/src/main.js:495-526`) is migrated in Phase 3. `docs/protocol.md:48` is updated alongside.

Blobs are not stored beyond `Ours`. The base and agent blobs are recomputable while the change is `proposed`, because `settle` releases a worktree only when none of its changes is open (`sessions.go:351-358`) and the agent's branch keeps `BaseRev` reachable. `MergedBlob` is recomputed, never cached. Only pre-images are persisted (§6) because user bytes may exist nowhere else.

### 4.3 Lazy projection (auto-rebase)

`Project(change)` = `merge(ours = workspace file now, base = BaseRev:path, theirs = agent worktree file)` run with `write=false` (this is `integration.Check`, `threeway.go:27-39`, extended to return kind and hunks).

Triggers:

| Event | Action | Cost control |
|---|---|---|
| E1: any accept or undo wrote path P | re-project every `proposed` change whose path is P, and set `Since` | path-indexed lookup; other paths untouched |
| E2: agent finishes and its changes are recorded (`recordChanges`, `worker.go:774`) | project each new change before it is shown | once per change |
| E3: `changes.list`, `changes.diff`, `changes.accept` | validate: hash the path's workspace file; if it equals `Integration.Ours`, reuse the state | stat-keyed memo `(path) → (mtime, size, blob)`; no process spawn on a hit |
| user edits file | covered lazily by E3. **No file watcher in v1.** `ponytail:` add an extension `createFileSystemWatcher` push when time-to-notice complaints or P1 telemetry show stale badges. | |

Rules that prevent regressions seen in the current code:

- Projection updates go through one batched `UpdateChanges` call and emit `changes.updated` **only if a visible field changed**. Today every `UpdateChange` emits (`orchestrator/changes.go:47`) and `changes.updated` triggers `supersedeStale` and `scheduleAutoApply` (`app.go:153-156`), whose drain loop re-arms on its own events (`auto_apply.go:34`). An unconditional emit from projection would be a feedback loop.
- Projection must not hold `decisionMu` for O(N) `git merge-file` runs. `resolveConflicts` does today (`conflicts.go:28-81`). Projection takes a per-path lock; `decisionMu` is held only for the write step.
- `resolveConflicts` stops being the detector. It remains as the action "send conflicted changes to the head agent".

Preview equals result. `Integration` carries a fingerprint `sha1(Ours | BaseRev:path blob | agent blob)`. `changes.accept` takes an optional `expect` fingerprint (the webview sends it). If the freshly computed fingerprint differs, the core returns `stale_preview` with the new `Integration` and writes nothing. `acceptAll` and auto-apply do not need a token: they write only changes that project `clean`, which is the consent the user gave.

Base movement at start (fixes R1b). Before a mutating or stacking agent's worktree is created, call `Snapshot()` (cheap when unchanged: same tree keeps the same commit, `workspace/manager.go:131-135`). For an agent that queued with an unstarted worktree, re-cut to the fresh base at launch with `git reset --hard` **inside the agent's own pristine worktree**, never the user's. The recut re-stacks every `Lineage` parent first (see §10 Lineage); `reset --hard` alone would discard dependency patches stacked at spawn. Each agent records its own `BaseRev`. If a snapshot falls back to HEAD (S4), or truncates untracked files above `maxSnapshotUntracked` (2,000, `manager.go:89`), the agent is flagged and the user sees one notice. Snapshot validation and fail-closed behavior are specified in §6.2 I10.

### 4.4 Stacked bases and leases together

A declared dependency or an acquired lease conflict both produce the same artifact: the new agent is cut from the current workspace with the earlier agent's pending patches applied (`Manager.Stack`, `manager.go:225-270`), and its changes record `Needs`. There is one mechanism and one set of accept/reject rules (§5).

- Dependency stacking is unchanged except that `stackParts` returns the ids it applied and `Stack` returns which patches applied (today it returns a count and a first error, `manager.go:233-269`).
- A lease overlap becomes a *soft* dependency: task B waits until A **finishes**, then B is cut stacked on A's pending patches for the leased paths only. B does not wait for A's accept, so human latency never stalls the scheduler.
- Leases are all-or-nothing at admit with a total order on path keys: no hold-and-wait, so no deadlock. FIFO by task `Created` (the order `dispatch.go:109-118` already uses) prevents starvation.

### 4.5 Conflict state machine

```text
                 project()                 workspace blob changed (E1/E3)
  proposed ──────────────────► clean ◄────────────────────────────────┐
     │                          │  ▲                                   │
     │                          │  └── resolved: user edit / new proposal / take-agent
     │                          ▼                                       │
     │                     conflicted ──── send to head ──► new proposal (old kept, SupersededBy set)
     │                          │
     │                          └──────── keep mine ──► rejected (agent blob retained, §6.4)
     ▼
  blocked (needs_parent | parent_rejected | parent_superseded | unsaved_buffer)
```

A conflicted change never blocks others, is excluded from `autoEligible` (`auto_apply.go:5-7`, rewritten in terms of `State`), survives restart, and is shown with its kind, hunks and cause (`Since`).

---

## 5. Accept and reject semantics for stacked and dependent changes

Definitions. `Needs(D)` is the set of changes whose patches were applied into D's base. `Dependents(P)` is the reverse. Both are transitive. A change is *clean* when it projects clean (§4.3) and is *unblocked* when every member of `Needs` is `accepted`.

| # | Situation | Behavior |
|---|---|---|
| A1 | Accept D, all `Needs(D)` accepted (or empty) | Normal 3-way, base `BaseRev`. Parent content is already in the workspace, so parent hunks merge as no-ops. |
| A2 | Accept D, some parent still `proposed` | **Refused**: `state=blocked, kind=needs_parent`, nothing written. Reason names the parent. Offer "Accept with parents": merge parents and D as one group in dependency order. Rationale: today's merge silently builds half-states (R6a). |
| A3 | Accept group | **Plan first, write second.** Compute all merges in memory; if any member is conflicted, abort with no write. Then write in topological order under the §6 CAS and journal. If a write fails midway, run journaled best-effort rollback per I8: restore already-written members (each only if its on-disk blob is still the one Relo wrote). A refused restore yields `partial` with the explicit member lists; it is not reported as success. The group's journal entry is one unit for undo, and undo is subject to the same per-member blob check. |
| A4 | Reject parent P, dependents exist and are `proposed` | Dependents become `blocked, kind=parent_rejected`, transitively. **Never auto-rejected, never auto-accepted, never deleted.** Offered actions: **Reject all** (P and dependents); **Keep blocked**; **Rebase without parent**: project D with `ours = workspace`, `base = BaseRev`, `theirs = D`. That is the existing 3-way and is clean exactly when D's edits are disjoint from P's. Applying requires explicit confirmation because D was authored and verified with P present. |
| A5 | Reject parent P while D is still running | D is not killed. When D records, it carries `Lineage` (includes P's `{id, patchHash}`), `Needs` derived from `Lineage`, and enters `blocked, parent_rejected`. `Lineage` is persisted at stack time, so it survives P's status change even though P's record no longer proves it was applied. |
| A6 | Parent's task retried or superseded (`recordChanges` drops the old attempt, `worker.go:783-809`) | If the new attempt proposes a change for the same path, dependents' `Lineage` is rewritten to the new attempt's `{id, patchHash}` and `Needs` recomputed; the rewrite is recorded, and the old `patchHash` is kept in the audit event. Otherwise dependents become `blocked, parent_superseded`. Today: silent. |
| A7 | Worktree lifetime | D's base is a commit in the shared object store reachable from D's branch (`manager.go:257`, `update-ref HEAD`). Releasing P's worktree after P is decided does not affect D. |
| A8 | `acceptAll` | Group by connected component of `Needs`, not only by path (`sessions.go:482-490`). Each component is planned all-or-refuse (no conflicted member is written) and written under journaled best-effort rollback (I8, not atomic); independent components keep today's partial-success behavior and its 4-way concurrency (`bulkMergeWorkers`, `sessions.go:568`). Conflicted or blocked changes stay `proposed` and are reported in `failed`. |
| A9 | Auto-apply | `autoEligible` becomes `State == clean && all Needs accepted`. Parents are auto-accepted first when eligible; the resulting `changes.updated` re-arms the drain (`app.go:153-156`) and releases dependents in the next pass. Auto-apply never accepts a group on its own. |
| A10 | Stale verification | A dependent verified with a now-rejected parent shows that fact in its blocked reason. `ponytail:` recording `verifiedOn []changeId` on the verification record, upgrade when a verification result is shown to the user as green for a blocked change. |

Siblings (no `Needs` relation) keep today's rule: independent, merged 3-way against whatever is in the workspace at accept time, same-path changes serialized in original order. What changes is that their state is current and attributable (§4.3).

---

## 6. No-silent-data-loss guarantee

### 6.1 Statement

> Relo never replaces, deletes or reorders bytes in a workspace file unless (1) a pre-write hash compare (CAS) shows those bytes are exactly the bytes it read when it computed the write, (2) the user's pre-write content is stored in a recoverable journal before the write, (3) the file has no unsaved editor buffer as reported to the core, and (4) the write is a clean 3-way merge or an action the user explicitly chose for that file. Any check failure writes nothing and says why.
>
> Scope of the guarantee. Relo does not make writes atomic against non-cooperating external writers. The window between the CAS compare and the rename is not closed by any file-system primitive available to Relo. Concurrent external writes inside that window are detected after the fact by the post-write verify (I1) and reported with the journal entry, and are recoverable through undo (I3). Relo's own writes are serialized by `decisionMu`. A group accept is a journaled best-effort rollback, not an atomic transaction (I8).

### 6.2 Invariants (each is tested)

| # | Invariant | Today | Mechanism |
|---|---|---|---|
| I1 | **CAS write, detect-and-report.** (a) Read `ours` and record its blob hash. (b) Compute the merge in memory. (c) Pre-write hash compare: re-hash the target; if it differs from (a), recompute from (a) up to 3 times, then fail with `workspace_changed`. (d) Write a temp file in the same directory and `rename` it over the target using the portable replace in §10. (e) Post-write verify: re-hash the target; if it equals neither the pre-image hash nor the written blob, journal the observed bytes, mark the change `conflicted, kind=concurrent_write`, and report. The gap between (c) and (d) is not closed; an external write in that gap is detected by (e) and recovered via I3, not prevented. Keep the existing file mode and do not follow symlinks. | not met (`threeway.go:58` → `:139`; mode from agent file) | `integration` write path; test seam `beforeCommit func()` for fault injection |
| I2 | **Journal.** Before any write or delete, the pre-image of every touched path is stored in a journal commit (private index, `commit-tree`, same pattern as `snapshotCommit`, `workspace/manager.go:95-142`) under `refs/relo/journal`, so garbage collection cannot drop it. The entry records change ids, paths, pre-blob and post-blob. A failed journal write **fails the accept closed**, except creating a path that does not exist. | not met (S3) | new `workspace/journal.go` (the `workspace` package owns the unexported git helpers, `manager.go:729-779`) |
| I3 | **Undo.** `changes.undo {id}` restores the pre-image for each path **iff** the on-disk blob still equals the recorded post-blob. Otherwise it refuses and offers a 3-way revert (pre-image as theirs, post-blob as base, current as ours), through the same engine. | absent | `app/sessions.go`, journal |
| I4 | **No markers, no half-applies.** A conflicted file is never written. Clean hunks of a conflicted file are never applied without a per-hunk user action (deferred, §8). | met (`threeway.go:107-109`) | keep, add test |
| I5 | **No overwrite of differing existing files.** add/add, modify/delete and delete/modify refuse (`threeway.go:72-79`, `91-94`, `97-98`). Binary on any side is a conflict (`threeway.go:145-146`). | met | keep, add test |
| I6 | **Unsaved buffers.** The extension checks `TextDocument.isDirty` for target paths before `changes.accept`, `acceptAll` and `resolve`, offering Save / Skip / Cancel, and keeps the core informed of the dirty set (notification on dirty-state change, debounced). The core refuses to write a path in that set (`blocked, unsaved_buffer`), including from auto-apply. When no extension is connected, there is no buffer awareness; this is stated, not hidden. VS Code's own "file is newer on disk" prompt on save is defense in depth and is not relied on. | not met (S2) | `extension/src/extension.ts`, `protocol.ts`, `app/app.go`, `auto_apply.go` |
| I7 | **Preview equals result or stop.** Single accept carries `expect`; a mismatch returns `stale_preview` and writes nothing (§4.3). | not met | `app/sessions.go` |
| I8 | **Group journaled rollback (best-effort, not atomic).** Plan all in memory; abort with no write on any conflict. Write in topological order under I1 and I2, recording each member's journal step. On mid-group failure, restore each already-written member from its journal pre-image, **only if** its on-disk blob still equals the post-blob Relo wrote (same rule as I3). If a member's restore is refused because of a concurrent edit, stop, leave that member and the remaining members in their current state, and report the group as `partial` with the list of applied, restored and unrestored members. The partial outcome is a defined, reported state, not a silent one. | n/a | `app/sessions.go` |
| I9 | **Protected paths.** `.git`, `.relo`, symlink escapes and absolute/parent paths always refuse (`threeway.go:47-55`, `125-128`; `merge.go:340-369`). | met | keep |
| I10 | **Honest base.** A snapshot that fell back to HEAD is recorded on the agent and reported once (S4). A snapshot that truncates untracked files (`maxSnapshotUntracked`, `manager.go:89`, `:121-123`) is recorded the same way, with the count omitted, and reported once. Snapshot validation: after each snapshot, re-hash the snapshot tree and compare to the tree Relo just wrote; on mismatch, retry the snapshot up to N=2 more times; if still mismatched, **fail closed**: the agent is not spawned against that base and the user sees one notice. No silent HEAD fallback on the validation path. | not met | `workspace/manager.go` `Snapshot`/`snapshotCommit` (`:95-141`), `orchestrator/facts.go` |

### 6.3 Boundaries (explicitly outside the guarantee)

- Direct mode (non-git workspace): agents edit in place and the change status is `applied` (`docs/worktrees.md:24`). There is nothing to protect or undo; the existing notice stays.
- Writes by other tools while an accept runs fall inside the CAS window and are caught by I1 only if they change size, mtime or content.
- A user who commits, resets or checks out while an accept is running.
- Semantic breakage of a textually clean merge (§9).

### 6.4 Agent-side retention (not user data, but cheap)

When a change is rejected or superseded, its agent blob is kept as `refs/relo/changes/<id>` for the journal retention period. Today a reject deletes it (`sessions.go:441-447`, `manager.go:586-601` with `git branch -D`). Retention: 14 days or 200 entries, whichever is smaller, pruned by `Sweep` (`manager.go:533`).

---

## 7. Root cause → mechanism map

| Root cause | Eliminated or reduced by | Eliminates or reduces? | Phase |
|---|---|---|---|
| R1 frozen base (R1a pending sibling, R1c accepted-after-cut) | explicit `BaseRev`; E1 re-projection on every accept; `Since` attribution; leases + soft stacking for declared overlaps | **Reduces.** Real overlap of two intents on the same lines cannot be removed; it is found within one event instead of at the user's click, and is attributed. Declared overlaps are avoided entirely. | P1, P3, P5 |
| R1b per-round base | `Snapshot()` before each isolated agent is created; re-cut of unstarted queued worktrees | **Eliminates** the stale round-start base | P1 |
| R2 decision latency | E1/E3 projection keeps pending state current while waiting; no re-run needed to learn a change is stale | **Reduces** the cost of waiting. It does not shorten the wait. | P3 |
| R3 destructive, forgetful conflict handling | `Integration` persisted in the payload; `SupersededBy` replaces reject-and-discard; hunk-scoped head prompt; "take agent's" with journal; "keep mine" retains the agent blob | **Eliminates** the memory-only state and the silent discard; **reduces** head-agent redo cost | P3 |
| R4 same-path ownership | declared-path leases at admit, held to finish, converting overlap to a soft dependency | **Reduces**, for the declared and concurrent slice only (1 of 18 pairs measured) | P5 |
| R5 whole-file refusal | `Analyze` returns hunks; conflicts are localized to hunks; partial apply stays opt-in and gated | **Reduces** resolution cost now; **may** reduce conflicts later, gated on P1 telemetry | P1, P3 |
| R6 stacked/dependent semantics | `Needs`, `blocked` states, group accept, reject cascade-mark, retry rewrite | **Eliminates** silent half-states and orphaned dependents | P4 |
| R7 observability gap | `Kind`, `Hunks`, `Since`, `Landed`, `BaseRev`; telemetry events | **Eliminates** the gap; precondition for judging every other row | P1 |
| S1 non-atomic write | I1 CAS (pre-write hash compare) + temp-file rename (§10) + post-write verify + mode preservation | **Reduces.** Writes are not atomic against external writers; an external write in the CAS-to-rename gap is detected and reported, then recovered via I3, not prevented | P2 |
| S2 unsaved buffers | I6 extension gate + core dirty set | **Eliminates** for connected-extension use | P2 |
| S3 no pre-image | I2 journal + I3 undo + §6.4 retention | **Eliminates** unrecoverable overwrite | P2 |
| S4 silent HEAD fallback | I10 flag and notice | **Reduces** (still falls back, but visibly) | P1 |

---

## 8. Migration from the current model

1. **Data.** All new fields are additive JSON in the existing `changes.payload` column (`persistence/store.go:214`; schema `store.go:76-150`). No SQL migration. A row with no `Integration` is `unknown` and is projected lazily on first read (E3).
2. **Legacy fields.**
   - `Conflict` non-empty and no `Integration`: synthesize `State=conflicted`, classify `Kind` from the legacy string (`overlap`, `modify_delete`, `delete_modify`, `add_add`, `binary`, otherwise `overlap`), keep `Reason`.
   - `Resolving` and `Superseded` stay as derived output for the webview until P3 ships its UI, then are removed from the wire after one release.
   - In-memory `conflictSet` is deleted in P3; `SupersededBy` replaces it, so supersession survives restart.
3. **Base.** `.relo/base/<agent>` (`manager.go:454-473`) remains authoritative for worktrees that predate `BaseRev`. On load, `BaseRev := recordedBase(agent)` when empty.
4. **Dead code removal (P2 cleanup).** After re-confirming no callers: `Coord.Integrate` (`orchestrator/integrate.go`), `integration.Apply/ApplyAgainst/Detect/Overlap/CopyFile` and their tests (`integration/merge_test.go`), `Manager.UserProtected` and its test. Keep `normalizePath`, `checkSymlinkPath` and `ConflictError`, which the live path uses. `docs/worktrees.md:22` is rewritten to describe the live behavior (3-way merge protects user edits; dirty paths are not refused).
5. **IPC.** `changes.list/accept/reject/acceptAll/rejectAll/diff/resolveConflicts` keep their contracts. Additive: `changes.accept {id, expect?, withParents?}`, `changes.reject {id, cascade?}`, `changes.undo {id}`, `changes.resolve {id, choice}`, notification `workspace.dirty {paths}`. Update `docs/protocol.md:22`, `:48`, `extension/src/protocol.ts:11`, `tests/protocol-contract.test.mjs`.
6. **Flags.** `relo.changes.lazyConflicts` (P3, default off until soak), `relo.changes.dependentSemantics` (P4), `relo.scheduler.pathLeases` (P5). P1 and P2 are safe by construction (stricter, not looser) and have no flag. Each flagged phase first runs **in shadow**: compute and log the new state, keep old behavior, compare.
7. **Rollback.** Flags off returns to the current behavior. Journal and `refs/relo/*` are inert if unused.
8. **Existing 8 `proposed` rows** (two failed agents, `docs/*.md`, worktrees present) load as `unknown` and project on first view.

---

## 9. Phases

Rollout order and why: P1 first because it is behavior-neutral, adds the data to judge everything else and fixes R1b. P2 before any automation grows, so every later write is already CAS and journaled. P3 delivers the main conflict reduction. P4 needs P2's journal for group rollback. P5 last: lowest measured yield and depends on P4's `Needs`.

```text
P1 ──► P2 ──► P3 ──► P4 ──► P5
 └──────────► P3 (needs P1 data model)
```

Hot files touched by several phases (`app/sessions.go`, `app/conflicts.go`, `orchestrator/worker.go`) are edited in phase order; a phase lists a file as *owned* when it makes the structural change there.

### P1. Explicit base, honest base, measured state (shadow) — landed

Goal: remove R1b and S4, add the data model, change no accept behavior.

Owned files:
- `core/internal/orchestrator/coord.go` (`Change` fields, `Dep` lineage, `Integration`)
- `core/internal/orchestrator/worker.go` (`spawn`: snapshot before create; `changesFor`/`recordChanges`: set `BaseRev`, `Needs`)
- `core/internal/orchestrator/stack.go` (`stackParts` returns applied change ids)
- `core/internal/orchestrator/dispatch.go` (re-cut an unstarted queued worktree at launch)
- `core/internal/workspace/manager.go` (`Stack` returns per-patch result; `Recut`; flag HEAD fallback; time `Snapshot`)
- `core/internal/integration/threeway.go` (`Analyze` returning kind, hunks, action; `Merge`/`Check` reimplemented on it, same results)
- `core/internal/app/sessions.go` (store `Landed` on accept), `core/internal/app/conflicts.go` (`checkChange` stores `Integration`)
- `docs/protocol.md`

Tests (extend existing):
- `integration/threeway_test.go`: every refusal returns a `Kind`; `Analyze` hunk counts for a 2-hunk file with 1 conflicting hunk; `Merge` result unchanged for all current cases (`TestMergeKeepsLocalEditsAndAppliesAgentEdits`, `…ReportsOverlapWithoutTouchingTheFile`, `…CreateDeleteAndFastForward`).
- `workspace/snapshot_test.go`: a second agent created after the workspace changed gets the new base; forced snapshot failure sets the fallback flag; `Stack` reports which patches applied.
- `orchestrator/stack_test.go` (extends `TestDependentTasksSeeTheChangesTheyDependOn`): recorded changes carry `Needs`.
- `app/conflicts_test.go`: `Integration` persists and reloads; legacy `Conflict` string maps to a kind.
- Shadow comparison: run `histogram` and default diff algorithm (`git merge-file --diff-algorithm`, [experiment] present in 2.56.0) on every check and log disagreements. `histogram` is a **hypothesis** for fewer spurious conflicts; adopt only if it never turns a clean merge into a conflict.

Acceptance:
- Existing `sessions_test.go` and `conflicts_test.go` pass unchanged.
- An agent spawned after a mid-round accept sees the accepted file in its worktree (new regression test for R1b).
- For rows written after P1, every stored conflict has a `Kind`, a hunk count and a `Landed` value for accepts; legacy rows are normalized on load, not backfilled.
- The P1 query (a documented SQL/script over `changes` and `events`) reproduces §1.1's table from live data, so later phases are judged against a baseline.

Rollback: revert; fields are additive.

### P2. Safe writes, journal, undo, unsaved-buffer gate

Goal: the §6 guarantee, before more automation.

Owned files:
- `core/internal/integration/threeway.go` (CAS + temp + rename + mode; `beforeCommit` test seam)
- `core/internal/workspace/journal.go` (new: journal commits, restore, retention) and `workspace/manager.go` (`Sweep` prunes journal and `refs/relo/changes`)
- `core/internal/app/sessions.go` (`mergeChange` journaled; `changes.undo`; `expect` check), `core/internal/app/app.go` (routes, dirty set), `core/internal/app/auto_apply.go` (skip dirty paths)
- `extension/src/extension.ts`, `extension/src/protocol.ts` (dirty gate, undo command, `workspace.dirty` push)
- Deletions listed in §8.4; `docs/worktrees.md`, `docs/protocol.md`

Tests:
- Fault injection: modify the file inside `beforeCommit`; assert `workspace_changed` (or a successful recompute) and that the injected bytes survive in the final file. Run 200 iterations with random interleavings.
- Journal: accept → entry exists → `changes.undo` restores bytes exactly; undo after a user edit refuses and leaves the file intact; journal write failure fails closed for an overwrite and succeeds for a create.
- Mode preserved on update; symlink target never followed.
- `app/sessions_test.go`: dirty path refused, other paths in the same `acceptAll` still land; auto-apply skips a dirty path.
- Extension (`extension/src/*.test.ts` style, fake documents): dirty document triggers prompt; Save proceeds, Skip omits, Cancel aborts.
- `scheduler`/`workspace` suites still pass (`go test ./internal/integration ./internal/workspace ./internal/scheduler` passes today).

Acceptance:
- No test in the fault-injection suite loses a user byte.
- Every accepted change has a journal entry; undo round-trips byte-for-byte.
- With no extension attached, the stated limitation appears in `docs/worktrees.md`.

Rollback: revert the phase. The journal ref is inert.

### P3. Lazy conflict state and virtual auto-rebase (flag `relo.changes.lazyConflicts`)

Goal: R1c, R2, R3, R5-localization.

Owned files:
- `core/internal/app/conflicts.go` (projection, per-path lock, memo, `Since`; `resolveConflicts` becomes "send to head"; hunk-scoped `conflictPrompt`; delete `conflictSet`, `supersedeStale` rewritten to set `SupersededBy` and keep the old blob)
- `core/internal/orchestrator/changes.go` (batched `UpdateIntegration`, emit only on visible change)
- `core/internal/app/auto_apply.go` (`autoEligible` on `State`)
- `core/internal/app/sessions.go` (E1 hook after write; `changes.resolve {id, choice: agent|mine|head}`; `choice=agent` explicit and journaled)
- `core/internal/app/app.go` (remove `conflictSet`, route `changes.resolve`)
- `webview/src/main.js`, `webview/src/main.test.js` (state/kind/hunks/cause badges, fingerprint on accept), `extension/src/extension.ts` (resolve actions), `docs/protocol.md`

Tests:
- Sequence: two agents cut together; accept A; B's state changes without any click and names A in `Since`.
- Memo: counter around `git merge-file` proves no run when the path's blob is unchanged.
- No write during check: assert workspace mtimes unchanged across `Project`.
- No feedback loop: N projections of unchanged state produce zero `changes.updated` emits and do not re-arm `drainAutoApply`.
- Restart: conflicted state and `SupersededBy` survive reload.
- `stale_preview`: workspace edit between preview and accept returns the error and writes nothing.
- Head prompt contains only conflicting hunks with context, and is shorter than the 4000-byte file head for a large file.
- Existing `TestNewProposalSupersedesConflictedChange` and `TestResolveConflicts*` pass in shadow mode.
- Webview: `node --test` (`webview/package.json`) covers the new badges; extension suite via `extension/package.json:641`.

Acceptance:
- After any accept or undo, every other proposed change on that path has a fresh `Integration` within one event loop.
- No "Resolve conflicts" click is needed to see a conflict.
- A superseded change's agent blob is still recoverable.
- Soak metric (from P1's query): median time from the causing accept to the conflict being visible falls to under one second, and conflicted-then-rejected falls below the P1 baseline (2 of 2 today).

Rollback: flag off restores string-based behavior; persisted `Integration` is ignored.

### P4. Dependent and stacked semantics (flag `relo.changes.dependentSemantics`)

Goal: R6. Rules A1–A10 of §5.

Owned files:
- `core/internal/orchestrator/stack.go`, `core/internal/orchestrator/worker.go` (`recordChanges` rewrites `Needs` on retry, A6)
- `core/internal/app/sessions.go` (`acceptAll` components, group accept with plan-then-write, `rejectChangeLocked` cascade-mark, `withParents`/`cascade` params)
- `core/internal/app/auto_apply.go` (A9)
- `webview/src/main.js`, `webview/src/main.test.js`, `extension/src/extension.ts` (blocked state, "Accept with parents", "Reject all", "Rebase without parent")
- `docs/worktrees.md` (replace the stacking paragraph, `:36`, `:40`), `docs/protocol.md`

Tests (new, `app/sessions_test.go` and `workspace/snapshot_test.go` style, real git repo):
- First write a failing test that pins today's hazard: accept a dependent alone, assert the half-state (to be inverted).
- A2: dependent alone is refused, nothing written.
- A3: group accept order; a forced failure of the second member rolls back the first from the journal.
- A4: reject parent → dependents `blocked, parent_rejected`, still `proposed`; "rebase without parent" result equals a direct 3-way and is clean iff hunks are disjoint.
- A5/A6: running dependent after parent reject; parent retry rewrites `Needs`.
- A8: components all-or-nothing, independent components partially succeed, concurrency bound still 4.
- A9: auto-apply releases a dependent only after its parent is accepted.
- `orchestrator` e2e (`e2e_git_test.go:18`) still passes.

Acceptance:
- No code path writes a dependent whose parents are not accepted unless it is in a group accept.
- Rejecting a parent never deletes or accepts a dependent.
- Evidence gate: ship behind the flag; enable by default only once real sessions contain parent→dependent change pairs (0 of 115 today). Until then it is correctness insurance, not a measured win.

Rollback: flag off.

### P5. Advisory path leases (flag `relo.scheduler.pathLeases`)

Goal: R4, for declared overlap only.

Owned files:
- `core/internal/taskgraph/graph.go` (`Task.Paths []string`), `core/internal/prompt/prompt.go` (schema field `paths`, next to `deps` at `:225`), `core/internal/orchestrator/parse.go` (validate)
- `core/internal/scheduler/lease.go` (new: all-or-nothing acquire, total order on path keys, release at finish), `core/internal/scheduler/scheduler.go` (compose with `Reserve`)
- `core/internal/orchestrator/dispatch.go` (`admit` gate, FIFO preserved), `core/internal/orchestrator/stack.go` (soft dependency stacking for leased paths)
- `docs/architecture.md`

Tests:
- `scheduler/lease_test.go`: two tasks with the same path never run together; disjoint paths do; property test over random admit orders shows no deadlock and no starvation.
- `scheduler/scheduler_test.go` (`TestReserveEnforcesCapsConcurrently`) still passes.
- `orchestrator`: a leased overlap produces `Needs` on the later task's changes and the later agent sees the earlier agent's pending patch.
- Throughput guard: with no `paths` declared, schedule order and concurrency are identical to today.

Acceptance:
- No two tasks that declare overlapping paths are ever concurrent.
- Median task start delay attributable to leases is reported; if it exceeds the median agent run time (568 s today) the feature stays off.

Rollback: flag off; `paths` is ignored.

Success metrics, all queryable from the database after P1: conflicts per accepted change; time from causing accept to visible conflict; conflicted changes resolved without an agent run; head-agent redo runs per conflict; accepts without a journal entry (target 0); `stale_preview` rate; `git merge-file` invocations per accept event.

---

## 10. Risks, open questions, skipped work

Risks:
- **Small sample.** 2 conflicts, 1 agent, 1 workspace. The 22% vs 0% contrast rests on 9 and 113 pairs, and the "exposed" 2-of-3 vs 0-of-112 contrast has one agent behind both conflicts. It supports "stale base is the only observed trigger", not a rate. P1 exists to replace this with real rates before P3 is judged.
- **Snapshot cost.** Per-spawn `Snapshot()` runs `git add -A` into a private index and `write-tree` (`manager.go:118-141`). Cheap when unchanged, unmeasured on large trees. P1 times it; if it is slow, only snapshot when an accept/undo or a user-visible change happened since the last one.
- **Process spawn cost** of `git merge-file` per projection. Mitigated by the stat/blob memo and path-indexed triggers; unmeasured.
- **Lock scope.** `acceptAll` holds `decisionMu` for the whole batch (`sessions.go:470-472`). Phase 3 must not extend that.
- **Journal growth** in `.git` objects. Retention bounds it; `refs/relo/*` is a new namespace in the user's repo, following the precedent of `relo/<agent>` branches and snapshot objects (`manager.go:136`, `210`).
- **Unsaved-buffer coverage** depends on the extension being attached (I6).
- **Windows/macOS** rename-over-existing and case-insensitive paths need the CAS implementation tested on both; `docs/worktrees.md:22` already acknowledges case rules.
- **Semantic conflicts.** Two textually clean, individually verified changes may break each other. `ponytail:` run the project's verification against the workspace after an accept that lands on a file another agent also changed; upgrade when the first post-merge breakage is observed. No evidence of one exists in the data.

Portable replace (used by I1 step d):
- POSIX: write the temp file in the target's directory, `fsync` the temp file, `rename(temp, target)`, then `fsync` the directory. `rename` replaces an existing target atomically within one file system.
- Windows: write the temp file in the same directory, then `MoveFileEx(temp, target, MOVEFILE_REPLACE_EXISTING)`. Do not use `MOVEFILE_WRITE_THROUGH` unless measured; directory fsync has no equivalent. Replace of a file held open without `FILE_SHARE_DELETE` by another process fails; this surfaces as a write error and the accept fails closed with no partial file.
- Both: the temp name is `.<basename>.relo-<random>.tmp` in the target directory and is removed on failure. The target's mode is copied from the pre-image before rename; symlink targets are refused, not followed.

Snapshot validation (I10): re-hash after snapshot; retry N=2 more times; then fail closed. Behavior is specified in §6.2 I10.

Lineage (dependency persistence across status changes and recuts):
- `Change.Lineage` holds `{id, patchHash}` for each parent applied at stack time. It is written once at stack time and is never cleared by a status change (`proposed`, `blocked`, `accepted`, `rejected`, `superseded`) or by a recut.
- A recut of an unstarted queued worktree (§4.3, `Recut`) re-stacks the same `Lineage` parents onto the fresh base. Each parent's current patch must hash to its recorded `patchHash`. Match: re-apply, recompute `Needs` from `Lineage`. Mismatch or missing parent: the recut fails and `Needs` is not recomputed; the change is `blocked, kind=parent_superseded` (or `parent_rejected` if the parent is rejected). `git reset --hard` is never run without a successful re-stack of every recorded parent.
- Dep-reject: a parent rejected while a dependent is queued or running keeps the dependent's `Lineage` unchanged and sets dependent state to `blocked, kind=parent_rejected` (A4/A5). The dependent's `Lineage` is not rewritten to drop the rejected parent.

Open questions for approval:
1. Is a new `refs/relo/*` namespace in the user's repository acceptable, or should the journal live outside `.git` (e.g. `.relo/journal/` as plain files)? The ref approach keeps pre-images reachable for free; plain files avoid touching `.git`.
2. Should "take agent's version" of a conflicted file (P3, `changes.resolve choice=agent`) exist at all in v1, or only "keep mine" and "send to head"?
3. Are declared `paths` acceptable to ask of the Primary (P5), given it adds output tokens per task?

Skipped, with upgrade triggers:
- Per-hunk partial accept. `ponytail:` upgrade when P1 telemetry shows at least 30% of conflicts have both a clean and a conflicting hunk (the number is a placeholder to be set from the P1 baseline).
- Extension file watcher. `ponytail:` upgrade on stale-badge complaints or measured time-to-notice above a few seconds.
- Rename-aware merge. Changes are per-file with `--no-renames` (`manager.go:419`), so a user rename of a file an agent edited reports delete/modify. No evidence either way.
- Line-ending/`autocrlf` behavior of `git merge-file`: not examined.
- The second database `~/.local/share/relo/relo.db`: not analyzed.
