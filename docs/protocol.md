# Protocol

Stdio JSON-RPC-ish frames, one JSON object per line. Version field `v` is currently `1`.

Stdout is IPC only. Logs are JSON lines on stderr.

Requests execute concurrently (up to 32 at once); clients must correlate responses by `id`. `shutdown` drains active handlers and queued frames, then ends `Serve`. EOF also drains queued frames. Event output uses a bounded queue; when full, `chat.delta` replaces an older queued delta, other events replace oldest queued event, and responses/errors are never discarded.

Every request must use `v: 1`, `kind: "req"`, non-empty `id` and `method`, and object `params` when provided. Invalid JSON gets an uncorrelated `invalid_frame` error; invalid versions/requests/params get correlated error frames when an ID is available. Invalid frames do not terminate serving.

## Frame

```json
{ "v": 1, "kind": "req", "id": "1", "method": "prompt.submit", "params": { "text": "…" } }
{ "v": 1, "kind": "res", "id": "1", "result": { "ok": true } }
{ "v": 1, "kind": "err", "id": "1", "error": { "code": "error", "message": "…" } }
{ "v": 1, "kind": "evt", "id": "evt_…", "method": "agent.started", "params": { } }
```

## Selected methods

`initialize`, `shutdown`, `session.create|get|list|clear|open|delete`, `prompt.submit|cancel|retry`, `agent.list|get|stop|redirect|message|changeModel|refreshContext|inspectContext|openWorktree`, `task.list|get|cancel|retry|setPriority`, `context.get|generations|refresh|rebuild|pin|remove|promote`, `findings.list`, `memory.list|update|delete`, `changes.list|accept|reject|acceptAll|rejectAll|diff|undo|rebase|resolve|resolveConflicts`, `workspace.dirty`, `verification.list`, `models.list|set`, `backend.status|restart`, `settings.apply`, `providers.discover`, `mode.set`, `orchestrator.pause|resume|stopAll`, `permission.respond`, `logs.tail`, `snapshot`, `cache.get`.

## Events

`session.created`, `session.opened`, `session.updated`, `agent.created`, `agent.started`, `agent.activity`, `agent.updated`, `agent.completed`, `agent.failed`, `agent.escalated`, `context.created`, `context.refreshed`, `context.compressed`, `context.replaced`, `context.item.promoted`, `context.item.dropped`, `task.created`, `task.updated`, `task.completed`, `task.failed`, `finding.created`, `finding.verified`, `memory.updated`, `usage.updated`, `changes.updated`, `verification.started`, `verification.completed`, `backend.status`, `permission.asked`, `chat.message`, `chat.delta`, `chat.discard`, `cache.updated`, `log`.

`agent.*` events carry a full `AgentInfo` (partial legacy payloads such as `{ "id", "activity" }` still merge). `agent.updated` is emitted after every agent change and is coalesced by the transport.

`AgentInfo` additive fields: `phase` (`context|model|reasoning|writing|tool|verifying|approval|waiting|compacting|retry`, `""` when idle or finished), `phaseAt` (unix ms of last phase change), `startedAt` (unix ms of current run start), `endedAt` (unix ms when terminal; `0`/absent while running), `tools` (tool call count), `rev`, `inputTokens`, `outputTokens`. `activity` remains a human-readable label.

Token fields on `AgentInfo` mean different things: `usedTokens` is what the agent has spent, summed over every model call (a conversation re-sent on each tool step counts again each time). `contextTokens` is what the latest call held (input, cached input and output; updated after every model call of a turn, not only when it ends), and `window` is the context window planned for the agent's model (`min(relo.models.contextLimit, model max)`, `0` when unknown). How full the context is = `contextTokens / window`; `budget` is only Relo's own planning budget for the context pack and is not a limit. At `relo.context.compactAt` (default 0.9) of `window` Relo asks the runtime to summarize the agent's conversation and says so with a `system` chat message; see `docs/opencode-runtime.md`.

`rev` semantics: a per-agent monotonic revision. For `agent.*` events, when both the payload and the stored agent have a numeric `rev` and `payload.rev < stored.rev`, the update is dropped as stale. Equal or greater `rev` merges. A payload without `rev` always merges. Merge is a shallow spread, so a full payload can clear a field (`phase: ""` overwrites).

`chat.delta` payload: `{ "agentId": "…", "delta": "…", "text": "…" }`. `text` is the cumulative streamed text and is idempotent: the streaming bubble (`id` `stream-<agentId>`) is set to `text`. A `text` shorter than the current bubble that is a prefix of it is ignored (stale replay). Without `text`, `delta` is appended.

`chat.discard` payload: `{ "agentId": "…" }`. Removes that agent's streaming assistant bubble; no-op if none.

`chat.message` dedupes by `id`: a message with an existing `id` replaces it in place. A message for an agent with an active streaming bubble finalizes that bubble (`streaming: false`). Chat roles: `user`, `assistant`, `system`, `error`.

When an `agent.*` event leaves an agent in a terminal status (`completed|failed|cancelled|interrupted`), its streaming bubble is removed if empty, or marked `streaming: false` if it has text.

`session.created` / `session.updated` payload may include `running: boolean`.

Sessions: `session.create` (also `session.clear`) stops whatever is running, saves the session being left, starts a blank one and returns a `snapshot`. `session.list` returns the stored sessions of this workspace as `[{ id, title, mode, createdAt, updatedAt, messages, pendingChanges, current }]`, newest first; sessions nobody typed into are not listed. `session.open {id}` switches to a stored one and returns a `snapshot`; `session.delete {id}` removes one and returns `{ sessions, snapshot }` (deleting the open session leaves a blank one). `session.created` and `session.opened` mean the whole UI state is replaced: clients must drop agents, tasks, messages, changes and checks they hold. Every list in a `snapshot` is an array, never `null`.

Changes: a change is `{ id, agentId, taskId, path, status, kind, added, deleted, worktree, baseRev, needs, lineage, supersededBy, landed, integration }` with `status` `proposed | accepted | rejected | applied` (`applied` = edited in place in a non-Git workspace) and `kind` `added | modified | deleted`. `baseRev` is the commit the change is diffed against; `needs` lists the change ids its base was stacked on and `lineage` records each parent as `{ id, patchHash }`; both survive status changes. `landed` is the accept outcome (`created | updated | merged | deleted | unchanged`). `integration` is `{ state, kind, reason, ours, hunks, since, checkedAt, fingerprint }`: `state` is `clean | conflicted | blocked | unknown`, `kind` is the conflict class (`overlap`, `modify_delete`, `delete_modify`, `add_add`, `binary`, `symlink`, `protected`, `needs_parent`, `parent_rejected`, `parent_superseded`, `worktree_gone`, `concurrent_write`, `workspace_changed`, ...), `ours` is the workspace blob the check ran against (`-` when absent), `hunks` locates each changed region as `{ oursStart, oursLen, theirsStart, theirsLen, conflicting }`, `since` names accepted change ids (or `user`) that moved the path after the change's base, and `fingerprint` identifies the three inputs the preview was computed from. The legacy `conflict`, `resolving` and `superseded` keys are still emitted, derived from `integration` and `supersededBy`, until the webview is migrated. Rows stored before these fields existed load with `integration` absent; a stored `conflict` string is classified on load and the state reads as `conflicted`. It is read from Git in the agent's worktree, so it is exact whatever the model reported.

With `relo.changes.lazyConflicts` on, every accept or undo re-projects every other proposed change on the written path before the event loop yields, so a conflict is visible without calling `changes.resolveConflicts`. `changes.resolveConflicts` is then the explicit "send conflicted changes to the head agent" action, not the detector. `changes.resolve {id, choice}` resolves one conflicted change: `choice: "agent"` takes the agent's version through the journal, `"mine"` rejects it and keeps the agent's blob recoverable, and `"head"` sends it to the head agent with only the conflicting hunks.

`changes.resolveConflicts` first rejects, through the normal reject path (agent blob retained), every conflicted change whose path is ignored (`relo.changes.ignorePaths`, `.relo/**` always) or whose `integration.kind` is `protected`; those never reach the head agent. It then records the working tree once; if the snapshot fails or falls back to HEAD it sends nothing, posts a system note and returns `prompted: false` with `blocked: "<reason>"`, because replacement work cut from an old base would conflict again. Otherwise it asks the head for one small task per remaining change (worker, easy or medium, `paths` = that file) with both versions' conflicting hunks. The result is `{ conflicted, autoRejected, prompted, blocked?, changes }`, where `conflicted` counts only the changes actually sent to the head. A newer proposal on a `relo.changes.takeLatestPaths` path supersedes every older proposal for that path, conflicted or not.

The core reads `ignorePaths`, `unionPaths` and `takeLatestPaths` (arrays of glob strings) from `settings.apply`/`initialize`; an absent key keeps the current value and an empty array clears it. Entries are trimmed, non-strings and empties are dropped, and lists are capped at 100 entries of 256 characters. A pattern without `/` matches a file name anywhere; one with `/` is anchored at the workspace root; `**` matches any number of folders.

`changes.accept {id, expect?}` merges that file into the workspace three-way (base commit, your file now, the agent's file): edits you made meanwhile are kept; an overlap fails with `integration_conflict` naming the file and changes nothing. A non-empty `expect` must equal the current projection fingerprint, or the accept fails with `stale_preview` and writes nothing. A path in `workspace.dirty {paths}` is refused with `unsaved_buffer`; a journal that cannot be written refuses an overwrite with `journal_failed`. `changes.undo {id}` restores the journal pre-image of an accepted change when the file still holds exactly what Relo wrote; otherwise it fails with `undo_refused` and changes nothing. `changes.reject {id}` discards the agent's copy; the workspace is not touched. The extension pushes the paths with unsaved editor buffers through `workspace.dirty` whenever that set changes. When none of an agent's changes is waiting any more its worktree is removed. `changes.acceptAll` / `changes.rejectAll` apply to every proposed change and return `{ done, failed: [{ id, path, reason }], changes }`. `changes.diff {id}` returns `{ path, kind, status, before, after, hasBefore, hasAfter, binary, truncated, note }`.

With `relo.changes.dependentSemantics` on, `changes.accept` refuses a change whose `needs` are not all accepted with `needs_parent` and writes nothing. `changes.accept {id, withParents: true}` plans the change and its proposed ancestors, refuses the whole group if any member conflicts, then writes in dependency order and rolls earlier writes back from the journal; an unrestorable member is reported in `partial`, not as success. `changes.reject {id, withDependents: true}` rejects the change and every transitive dependent; a plain reject only marks dependents `blocked, parent_rejected`. `changes.rebase {id, apply?}` projects the change without its parents (the same direct three-way) and returns clean only when its edits are disjoint from theirs; `apply: true` writes the clean result and clears `lineage`. Accept-all groups changes by connected dependency component: each component is all-or-nothing, independent components keep partial success and the four-way concurrency bound. Auto-apply accepts pending parents first and releases a dependent only after all its `needs` are accepted.

With `relo.scheduler.pathLeases` on (setting `pathLeases`, default `false`), a task may declare `paths` (workspace-relative files or directories it expects to edit; `task.created` / `task.updated` carry them). Two tasks whose declared paths overlap are never running at the same time: the later task waits with `path leased by another task`, and once the earlier task finishes it starts with the earlier task's still-proposed patches for the overlapping paths applied to its worktree, recording them in `lineage`/`needs` like a declared dependency. Tasks without `paths`, and all tasks while the setting is off, are scheduled exactly as before. The core logs `path leases` with `medianStartDelayMs` when a run ends and any task waited; if that median exceeds the median agent run time the setting stays off.

Clients may coalesce state posts: the VS Code extension posts state at most once per ~50 ms (trailing edge, always the latest state).

`cache.updated` event and `cache.get` response payload: `{ "session": { "hitRate": 0, "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0 }, "agents": [{ "agentId": "…", "generation": 0, "model": "…", "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "hitRate": 0 }] }`.

`agent.escalated` payload: `{ "taskId": "…", "from": "…", "to": "…", "attempt": 2, "reason": "…" }`. Reducer updates matching task agent model, `escalatedFrom`, and attempt.

Snapshot payloads may include additive `efficiency: { "preset": "off", "profiles": { "<role>": "<profile id>" } }`.

## Settings and provider payloads

`settings.apply` params: the `initialize` settings payload (`routingMode`, `contextLimit`, `providers`, `providerKeys`, `modelEfforts`, and any existing key). Core re-applies settings; when provider or key config changed and no agent is running it restarts the OpenCode child. Result: `{ "applied": true, "restartPending": false, "backend": { ... } }`. `providerKeys` values are used for the child env only and are never logged or echoed.

`providers.discover` params: `{ "kind": "openai" | "anthropic", "baseURL": "…", "apiKey": "…" }`. Result: `{ "models": [ { "id": "…", "name": "…", "context": 0, "output": 0, "efforts": ["…"] } ], "error": "" }`. Upstream failures return `models: []` with a human-readable `error`, not an RPC error.
