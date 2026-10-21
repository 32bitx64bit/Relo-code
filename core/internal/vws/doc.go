// Package vws is the Virtual Workspace Engine (VWE).
//
// Agents do not need independent physical copies of a repository; they need independent, consistent views of it.
// The engine gives every mutating agent a logical workspace made of three layers, resolved in this order:
//
//  1. the agent's current uncommitted overlay,
//  2. the agent's latest committed branch tree,
//  3. the immutable base snapshot commit it was cut from.
//
// All three are Git trees in the repository's own object database, so file content is stored once and shared. The
// overlay is not a directory: it is a tree object that is replaced, never edited, each time the agent writes. That
// tree already contains the effect of every earlier layer, so a deleted path is simply absent from it and can never
// fall back to the base (the tombstone is structural). Nothing here touches the user's checkout, index, HEAD or
// branches.
//
// # Durability
//
// Every published revision is one atomic ref update. The ref refs/relo/vws/<agent>/overlay points at a checkpoint
// commit whose tree is the overlay and whose message carries the workspace metadata, so the content and the
// metadata that describe it can never disagree, and a crash leaves either the old revision or the new one.
// refs/relo/vws/<agent>/base pins the base commit for the life of the workspace, and refs/heads/relo/<agent> is the
// agent's branch once it has committed. Git garbage collection therefore cannot remove an object a live overlay
// needs. Recover rebuilds every workspace from those refs alone.
//
// # Execution
//
// Some tools need real files: compilers, test runners, language servers, shells. For those the engine materializes
// a workspace revision into a disposable directory (a Materializer; the default is a detached Git worktree), and
// imports what the tools changed back into the overlay when asked (Capture). While a directory exists it is the
// authority for that agent: the engine refuses direct overlay edits so the model's view and its tools' view cannot
// diverge. Materialization is an implementation detail of execution, not the identity of the agent.
//
// # Safety
//
// Paths are canonicalised and traversal is rejected. Anything under .git (including NTFS and HFS lookalikes) and
// the top-level .relo folder is protected. Symlinks are never followed out of the workspace: a link may be read
// when it resolves inside the tree, nothing is written through one, and a directory that is a link is not entered.
// A workspace created read-only rejects every mutation.
package vws
