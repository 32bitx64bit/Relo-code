# Worktrees

Parallel mutating workers do not share the user's working copy.

```text
.relo/worktrees/<agent-id>/
```

Created with `git worktree add -b relo/<agent-id> <path> <base-commit>`. Agent IDs must contain only letters, digits, `.`, `_`, and `-`, and cannot start with a path traversal.

Relo records base commit, branch, and changed files. Probes and reviewers are read-only and stay on the main workspace, unless they depend on a task whose changes are not accepted yet (see Integration).

## Git safety

At the start of each request, and again before each agent is cut, Relo snapshots:

- current branch
- HEAD
- dirty files
- untracked files

The snapshot is re-hashed after it is recorded. If the re-hash does not match, Relo retries twice and then fails closed: no agent is started from an unverified base, and the user sees one notice. A snapshot that falls back to HEAD, or that leaves out untracked files above 2,000, is flagged and reported once so the user knows what the agent did not see.

Dirty paths are not refused. The live accept path protects user edits by three-way merging the workspace file at accept time and refusing only true overlaps, protected metadata paths (`.git`, `.relo`), absolute/parent paths and symlink escapes. Relo never `reset --hard`s the user workspace and never pushes without permission.

Non-Git workspaces run in **direct mode**. Mutating workers run in the workspace directory itself (no worktree). Only one mutating worker runs at a time; read-only workers run concurrently. Changes are recorded with status `applied`: accept is a no-op and reject says the edit was made in place and cannot be reverted (run `git init` to enable isolation). Relo posts one system chat notice per session. Auto-review is skipped (no diff). Verification still runs in the workspace; failing to find Git never fails a task. Run `git init` to enable isolation.

Permission defaults allow read, write, test, and build; delete, install, commit, push, network, outside-workspace access, and destructive commands require approval. Unknown actions default to approval. Commands are split into individual operators and dangerous commands default to approval. `MapReply(Ask)` maps to OpenCode's `reject` response; ask is not implicitly approved because runtime permission requests do not expose an interactive hold at this layer.

## Integration

What an agent changed is read from Git in its worktree (`git add -A` there, then `git diff --cached --name-status --numstat <base>`), not from the model's report, so the Changes tab is exact: added, modified or deleted, with line counts. The base commit is recorded in `.relo/base/<agent-id>` so it survives restarts.

Accepting a change merges that one file three-way: base (the commit the worktree was cut from), ours (the file in your workspace now) and theirs (the agent's file), using `git merge-file`. Edits you made while the agent worked are kept. If the two sides touch the same lines, or you deleted a file the agent changed (or the reverse), or the file is binary on both sides, nothing is written and the accept fails with a message naming the file. `.git/`, `.relo/`, absolute paths, parent traversal and symlink escapes are always refused. Once none of an agent's changes is waiting, its worktree and branch are removed. At startup, worktrees that no stored change refers to are swept.

Before a write, Relo records the user's pre-image in a journal commit under `refs/relo/journal/`. The write is a compare-and-swap: the workspace file is re-hashed after the merge is computed, and a file that changed in the meantime is merged again up to three times, then refused as `workspace_changed`. The write goes through a temporary file in the same directory and is renamed into place, keeping the existing file's mode and never following a symlink. A journal write that fails refuses an overwrite; creating a new path still succeeds because it cannot lose user bytes. `changes.undo {id}` restores the pre-image when the file still holds exactly what Relo wrote, and refuses, changing nothing, after a later user edit. Rejected and superseded agent blobs are kept under `refs/relo/changes/<id>`. The journal keeps the newest 200 entries from the last 14 days; `Sweep` prunes both refs.

The VS Code extension checks `TextDocument.isDirty` before accept, accept-all and resolve, offering Save / Skip / Cancel, and pushes the dirty set to the core. A path with an unsaved buffer is blocked with kind `unsaved_buffer`, including from auto-apply. Without a connected extension there is no buffer awareness, so the core cannot see an unsaved editor buffer.

Worktrees are cut from your working tree as it is, uncommitted edits and new files included, not from the last commit. At the start of each request (and each round of follow-up work) Relo records the tree as a commit on top of `HEAD`, using a private copy of the Git index, so your branch, index, stash and files are never touched; ignored files are left out, and `.relo/` is added to `.git/info/exclude`. An unchanged tree keeps the same base. Agents start from that commit, so what they see is what you see, and the Changes tab shows only what an agent added on top of it. If the tree has more than 2000 untracked files, only tracked files are recorded.

An agent that depends on another agent's task (the tests for a worker's change, the review of it, a step that builds on it) starts from the same commit with that task's not-yet-accepted changes applied on top, and that result is its base, so its own diff holds only what it adds. A reviewer is also given the diff (capped at 20,000 characters). A patch that no longer applies is skipped and the agent is told which. Probes with no such dependency still read your folder directly.

Auto-review runs only for non-trivial mutating tasks in Git workspaces. Trivial tasks skip review.

Rejecting a proposed change discards that path only in its isolated worktree. Removing worktrees prunes stale Git worktree metadata; it does not delete user workspace files.
