package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/orchestrator"
)

const overlapReason = "your edits and the agent's edits overlap"

// promptDiffBytes caps the agent's version of each conflicted file in the head message.
const promptDiffBytes = 4000

var blobRe = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// resolveConflicts checks every waiting change against the workspace file as it is now. Nothing is written. Changes
// that overlap are recorded by path, and the head agent is asked in chat to re-apply their intent on the current tree.
func (a *App) resolveConflicts() (any, error) {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()

	sid := a.Coord.SessionID()
	var conflicted []orchestrator.Change
	flagged := map[string]bool{}
	for _, ch := range a.Coord.Changes() {
		if ch.Status != "proposed" {
			continue
		}
		outcome, err := a.checkChange(ch)
		if err != nil {
			return nil, err
		}
		if outcome == "conflicted" {
			if cur, ok := a.Coord.FindChange(ch.ID); ok {
				conflicted = append(conflicted, cur)
				flagged[ch.ID] = true
			}
		}
	}
	var resolving, idle []string
	for _, ch := range a.Coord.Changes() {
		if ch.Status != "proposed" {
			continue
		}
		if flagged[ch.ID] {
			resolving = append(resolving, ch.ID)
		} else {
			idle = append(idle, ch.ID)
		}
	}
	a.Coord.UpdateChanges(resolving, func(c *orchestrator.Change) { c.Resolving = true })
	a.Coord.UpdateChanges(idle, func(c *orchestrator.Change) { c.Resolving = false })
	if a.conflictSet == nil {
		a.conflictSet = map[string]map[string][]string{}
	}
	set := map[string][]string{}
	for _, ch := range conflicted {
		set[ch.Path] = append(set[ch.Path], ch.ID)
	}
	if len(set) == 0 {
		delete(a.conflictSet, sid)
	} else {
		a.conflictSet[sid] = set
	}
	if len(conflicted) == 0 {
		return map[string]any{"conflicted": 0, "prompted": false, "changes": a.Coord.Changes()}, nil
	}
	a.sendHead(sid, a.conflictPrompt(conflicted))
	a.persist()
	return map[string]any{"conflicted": len(conflicted), "prompted": true, "changes": a.Coord.Changes()}, nil
}

// checkChange returns "conflicted", "clean" or "skipped". It sets or clears Change.Conflict and never writes a file.
func (a *App) checkChange(ch orchestrator.Change) (string, error) {
	if !a.ownWorktree(ch.Worktree) || ch.Path == "" || !filepath.IsLocal(filepath.FromSlash(ch.Path)) {
		return "skipped", nil
	}
	reason, err := a.conflictReason(ch)
	if err != nil {
		reason = err.Error()
	}
	a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) { c.Conflict = reason })
	if reason != "" {
		return "conflicted", nil
	}
	return "clean", nil
}

// conflictReason recomputes why a change cannot be accepted as it is, or "" when it merges cleanly.
func (a *App) conflictReason(ch orchestrator.Change) (string, error) {
	// Do not recreate a released worktree: its proposed edits are no longer available.
	if _, err := os.Stat(ch.Worktree); err != nil {
		return "", fmt.Errorf("the agent's worktree is unavailable: %w", err)
	}
	tree, err := a.Coord.WS.EnsureTree(ch.AgentID, false)
	if err != nil {
		return "", fmt.Errorf("the agent's worktree could not be opened: %w", err)
	}
	if tree.Path != ch.Worktree {
		return "", fmt.Errorf("the agent's worktree does not match the change")
	}
	base, hadBase, err := a.baseOf(ch)
	if err != nil {
		return "", ipc.Fail{C: "integration_failed", M: fmt.Sprintf("%s: %v", ch.Path, err)}
	}
	reason, err := integration.Check(a.Cfg.Workspace, ch.Worktree, ch.Path, base, hadBase)
	if err != nil {
		return "", ipc.Fail{C: "integration_failed", M: fmt.Sprintf("%s: %v", ch.Path, err)}
	}
	return reason, nil
}

// conflictPrompt is the head message. It lists each conflicted change and asks for its intent to be re-applied.
func (a *App) conflictPrompt(conflicted []orchestrator.Change) string {
	var b strings.Builder
	b.WriteString("Resolve conflicts. These proposed changes overlap with edits made in the workspace after the agent started, so none was applied. " +
		"Re-apply each change's intent on the current tree, keep the user's edits, and delegate the work. The changes below will be replaced by the new ones.\n")
	for _, ch := range conflicted {
		title := ch.Path
		if t, ok := a.Coord.Graph.Get(ch.TaskID); ok && t.Title != "" {
			title = t.Title
		}
		fmt.Fprintf(&b, "\n- id: %s\n  title: %s\n  path: %s\n  reason: %s\n  agent version:\n", ch.ID, title, ch.Path, ch.Conflict)
		b.WriteString(agentCopy(ch))
	}
	return b.String()
}

// agentCopy returns the agent's version of the file, capped at promptDiffBytes.
func agentCopy(ch orchestrator.Change) string {
	data, err := os.ReadFile(filepath.Join(ch.Worktree, filepath.FromSlash(ch.Path)))
	if err != nil {
		return "  (agent version unavailable)\n"
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "  (binary, not shown)\n"
	}
	suffix := ""
	if len(data) > promptDiffBytes {
		data, suffix = data[:promptDiffBytes], "\n[truncated]"
	}
	return "```\n" + string(data) + suffix + "\n```\n"
}

type headMessage struct {
	session string
	text    string
}

// sendHead queues a head message for session sid and submits it once no other turn is running, so a message sent mid-turn is kept.
func (a *App) sendHead(sid, text string) {
	a.headMu.Lock()
	a.headQueue = append(a.headQueue, headMessage{session: sid, text: text})
	if a.headBusy {
		a.headMu.Unlock()
		return
	}
	a.headBusy = true
	a.headMu.Unlock()
	go a.safely("head queue", a.drainHead)
}

func (a *App) drainHead() {
	for {
		msg, ok := a.nextHead()
		if !ok {
			return
		}
		err := a.Coord.Submit(context.Background(), msg.text, nil)
		if s := a.Coord.Current(); err != nil && s != nil && s.Running {
			a.requeueHead(msg)
			continue
		}
		if err != nil {
			a.Log.Error("head message", map[string]any{"error": err.Error()})
		}
		a.clearResolving(msg.session)
		a.persist()
	}
}

// nextHead pops the oldest queued head message whose session is current and idle. It waits while none is, and
// clears headBusy and returns false when the queue is empty.
func (a *App) nextHead() (headMessage, bool) {
	for {
		a.headMu.Lock()
		if len(a.headQueue) == 0 {
			a.headBusy = false
			a.headMu.Unlock()
			return headMessage{}, false
		}
		for i, m := range a.headQueue {
			if a.headReady(m.session) {
				a.headQueue = append(a.headQueue[:i:i], a.headQueue[i+1:]...)
				a.headMu.Unlock()
				return m, true
			}
		}
		a.headMu.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
}

// headReady reports whether sid is the current session and no turn is running in it.
func (a *App) headReady(sid string) bool {
	cur := a.Coord.Current()
	return cur != nil && !cur.Running && cur.ID == sid
}

func (a *App) requeueHead(msg headMessage) {
	a.headMu.Lock()
	a.headQueue = append([]headMessage{msg}, a.headQueue...)
	a.headMu.Unlock()
}

// clearResolving ends the Resolving state once sid has no head message still queued. Conflict stays so a retry is possible.
func (a *App) clearResolving(sid string) {
	a.headMu.Lock()
	for _, m := range a.headQueue {
		if m.session == sid {
			a.headMu.Unlock()
			return
		}
	}
	a.headMu.Unlock()
	a.clearResolvingLoaded()
}

// clearResolvingLoaded clears Resolving on every change of the open session. Used on load, when no head turn survives.
func (a *App) clearResolvingLoaded() {
	var ids []string
	for _, ch := range a.Coord.Changes() {
		if ch.Resolving {
			ids = append(ids, ch.ID)
		}
	}
	a.Coord.UpdateChanges(ids, func(c *orchestrator.Change) { c.Resolving = false })
}

// supersedeStale rejects the recorded conflicted changes for a path once a new proposal for that path exists.
func (a *App) supersedeStale() {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()

	sid := a.Coord.SessionID()
	set := a.conflictSet[sid]
	if len(set) == 0 {
		return
	}
	changes := a.Coord.Changes()
	for path, ids := range set {
		if !hasNewProposal(changes, path, ids) {
			continue
		}
		for _, id := range ids {
			if ch, ok := a.Coord.FindChange(id); ok && ch.Status == "proposed" {
				if _, err := a.rejectChangeLocked(id); err != nil {
					a.Log.Warn("supersede conflicted change", map[string]any{"changeId": id, "error": err.Error()})
					continue
				}
				a.Coord.UpdateChange(id, func(c *orchestrator.Change) { c.Superseded, c.Conflict, c.Resolving = true, "", false })
			}
		}
		delete(set, path)
	}
	if len(set) == 0 {
		delete(a.conflictSet, sid)
	}
}

func hasNewProposal(changes []orchestrator.Change, path string, old []string) bool {
	for _, ch := range changes {
		if ch.Path != path || ch.Status != "proposed" {
			continue
		}
		known := false
		for _, id := range old {
			if id == ch.ID {
				known = true
				break
			}
		}
		if !known {
			return true
		}
	}
	return false
}

// baseOf returns the change's base: the blob stored by an earlier check, or else the file at the agent's base commit.
func (a *App) baseOf(ch orchestrator.Change) ([]byte, bool, error) {
	if ch.BaseBlob == "" {
		return a.Coord.WS.BaseContent(ch.AgentID, ch.Path)
	}
	if !blobRe.MatchString(ch.BaseBlob) {
		return nil, false, fmt.Errorf("invalid base blob")
	}
	out, err := exec.Command("git", "-C", a.Cfg.Workspace, "cat-file", "blob", ch.BaseBlob).Output()
	if err != nil {
		return nil, false, fmt.Errorf("git cat-file: %w", err)
	}
	return out, true, nil
}
