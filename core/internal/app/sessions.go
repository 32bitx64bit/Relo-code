package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/workspace"
)

// persistDelay is how long state changes are batched before they are written to the database.
const persistDelay = 750 * time.Millisecond

// persistEvents are the events after which the stored copy of the session is out of date.
var persistEvents = map[string]bool{
	"chat.message": true, "session.updated": true, "task.created": true, "task.updated": true, "task.completed": true,
	"task.failed": true, "agent.completed": true, "agent.failed": true, "changes.updated": true,
	"verification.completed": true, "finding.created": true,
}

// safely runs fn and logs a panic instead of letting it end the process.
func (a *App) safely(where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			a.Log.Error("panic recovered", map[string]any{"where": where, "panic": fmt.Sprint(r), "stack": string(debug.Stack())})
		}
	}()
	fn()
}

// persistSoon schedules one write of the session. Many events in a burst cost one write.
func (a *App) persistSoon() {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if a.closed || a.persistTimer != nil {
		return
	}
	a.persistTimer = time.AfterFunc(persistDelay, func() {
		a.persistMu.Lock()
		a.persistTimer = nil
		closed := a.closed
		a.persistMu.Unlock()
		if !closed {
			a.safely("persist", a.persist)
		}
	})
}

func (a *App) stopPersistTimer() {
	a.persistMu.Lock()
	a.closed = true
	if a.persistTimer != nil {
		a.persistTimer.Stop()
		a.persistTimer = nil
	}
	a.persistMu.Unlock()
}

// SessionSummary is one row of the session history.
type SessionSummary struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Mode     string    `json:"mode"`
	Created  time.Time `json:"createdAt"`
	Updated  time.Time `json:"updatedAt"`
	Messages int       `json:"messages"`
	Pending  int       `json:"pendingChanges"`
	Current  bool      `json:"current"`
}

func sessionTitle(s *orchestrator.Session) string {
	text := strings.TrimSpace(s.Objective)
	if text == "" {
		for _, m := range s.Messages {
			if m.Role == "user" {
				text = strings.TrimSpace(m.Text)
				break
			}
		}
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if r := []rune(text); len(r) > 80 {
		text = string(r[:80]) + "…"
	}
	if text == "" {
		return "Untitled session"
	}
	return text
}

// emptySession is one the user never typed into: not worth keeping or listing.
func emptySession(s *orchestrator.Session) bool {
	return s == nil || (len(s.Messages) == 0 && len(s.Changes) == 0)
}

func summarize(s *orchestrator.Session, current bool) SessionSummary {
	pending := 0
	for _, ch := range s.Changes {
		if ch.Status == "proposed" {
			pending++
		}
	}
	return SessionSummary{ID: s.ID, Title: sessionTitle(s), Mode: string(s.Mode), Created: s.Created, Updated: s.Updated, Messages: len(s.Messages), Pending: pending, Current: current}
}

func (a *App) listSessions() ([]SessionSummary, error) {
	raws, err := a.Store.List("sessions", a.Cfg.Workspace)
	if err != nil {
		return nil, err
	}
	cur := a.Coord.Current()
	out := []SessionSummary{}
	for _, raw := range raws {
		var s orchestrator.Session
		if json.Unmarshal(raw, &s) != nil || (cur != nil && s.ID == cur.ID) || emptySession(&s) {
			continue
		}
		out = append(out, summarize(&s, false))
	}
	if cur != nil && !emptySession(cur) {
		out = append(out, summarize(cur, true))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// sweepWorktrees removes agent worktrees nobody can use any more. A worktree is kept while a stored or current change
// of its agent still waits for a decision.
func (a *App) sweepWorktrees() {
	ws := a.Coord.WS
	if ws == nil || ws.Direct() {
		return
	}
	keep := map[string]bool{}
	note := func(s *orchestrator.Session) {
		for _, ch := range s.Changes {
			if ch.Status == "proposed" {
				keep[ch.AgentID] = true
			}
		}
	}
	if raws, err := a.Store.List("sessions", a.Cfg.Workspace); err == nil {
		for _, raw := range raws {
			var s orchestrator.Session
			if json.Unmarshal(raw, &s) == nil {
				note(&s)
			}
		}
	}
	if cur := a.Coord.Current(); cur != nil {
		note(cur)
	}
	if n := ws.Sweep(keep); n > 0 {
		a.Log.Info("removed stale agent worktrees", map[string]any{"count": n})
	}
}

// newSession stops whatever is running, saves the session being left and starts a blank one.
func (a *App) newSession(ctx context.Context, mode orchestrator.Mode, objective string) *orchestrator.Session {
	a.Coord.StopAll(ctx)
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()
	a.sessMu.Lock()
	a.persistLocked()
	old := a.Coord.Current()
	if _, err := a.Coord.WS.Snapshot(); err != nil {
		a.Log.Warn("workspace snapshot", map[string]any{"error": err.Error(), "workspace": a.Cfg.Workspace})
	}
	s := a.Coord.CreateSession(a.Cfg.Workspace, objective, mode)
	if old != nil && emptySession(old) {
		a.deleteStored(old.ID)
	}
	a.putJSON("sessions", s.ID, "", s.Workspace, s)
	a.sessMu.Unlock()
	a.sweepWorktrees()
	return s
}

func (a *App) deleteStored(id string) {
	if err := a.Store.DeleteSession(id); err != nil {
		a.Log.Warn("delete stored session", map[string]any{"sessionId": id, "error": err.Error()})
	}
}

// openSession switches to a stored session.
func (a *App) openSession(ctx context.Context, id string) (any, error) {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()
	cur := a.Coord.Current()
	if cur != nil && cur.ID == id {
		return a.Coord.Snapshot(), nil
	}
	var s orchestrator.Session
	if err := a.Store.Get("sessions", id, &s); err != nil {
		return nil, ipc.Fail{C: "not_found", M: "session not found"}
	}
	if s.Workspace != a.Cfg.Workspace {
		return nil, ipc.Fail{C: "invalid_session", M: "session belongs to another workspace"}
	}
	a.Coord.StopAll(ctx)
	a.sessMu.Lock()
	a.persistLocked()
	old := a.Coord.Current()
	a.Coord.OpenSession(ctx, &s)
	a.restoreSession(s.ID)
	a.clearResolvingLoaded()
	if old != nil && emptySession(old) {
		a.deleteStored(old.ID)
	}
	a.sessMu.Unlock()
	a.sweepWorktrees()
	return a.Coord.Snapshot(), nil
}

// deleteSession removes a stored session. Deleting the open one leaves a fresh blank session in its place.
func (a *App) deleteSession(ctx context.Context, id string) (any, error) {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()
	if id == "" {
		return nil, ipc.Fail{C: "invalid_params", M: "id is required"}
	}
	cur := a.Coord.Current()
	if cur != nil && cur.ID == id {
		a.Coord.StopAll(ctx)
		a.sessMu.Lock()
		s := a.Coord.CreateSession(a.Cfg.Workspace, "", cur.Mode)
		a.deleteStored(id)
		a.putJSON("sessions", s.ID, "", s.Workspace, s)
		a.sessMu.Unlock()
	} else {
		a.sessMu.Lock()
		a.deleteStored(id)
		a.sessMu.Unlock()
	}
	a.sweepWorktrees()
	sessions, err := a.listSessions()
	if err != nil {
		return nil, err
	}
	return map[string]any{"sessions": sessions, "snapshot": a.Coord.Snapshot()}, nil
}

// maxDiffBytes caps how much of one file is sent to the editor for a diff.
const maxDiffBytes = 1 << 20

func readCapped(path string) (text string, exists, binary, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, false, false
	}
	defer f.Close()
	buf := make([]byte, maxDiffBytes+1)
	n, _ := f.Read(buf)
	buf = buf[:n]
	if len(buf) > maxDiffBytes {
		buf, truncated = buf[:maxDiffBytes], true
	}
	if bytes.IndexByte(buf, 0) >= 0 {
		return "", true, true, false
	}
	return string(buf), true, false, truncated
}

// changeDiff returns the two sides of one change for the editor's diff view.
func (a *App) changeDiff(id string) (any, error) {
	ch, ok := a.Coord.FindChange(id)
	if !ok {
		return nil, ipc.Fail{C: "not_found", M: "change not found"}
	}
	if !filepath.IsLocal(filepath.FromSlash(ch.Path)) {
		return nil, ipc.Fail{C: "invalid_change", M: "change path is not inside the workspace"}
	}
	a.projectPaths([]string{ch.Path})
	ch, _ = a.Coord.FindChange(id)
	rel := filepath.FromSlash(ch.Path)
	out := map[string]any{"id": ch.ID, "path": ch.Path, "kind": ch.Kind, "status": ch.Status}
	if ch.Integration != nil {
		out["integration"] = ch.Integration
	}
	var before, after string
	var hasBefore, hasAfter, binary, truncated bool
	switch ch.Status {
	case "applied": // direct mode: the edit is already in the workspace and there is no earlier copy
		after, hasAfter, binary, truncated = readCapped(filepath.Join(a.Cfg.Workspace, rel))
		out["note"] = "Edited in place (no Git): there is no earlier version to compare with."
	default:
		if a.ownWorktree(ch.Worktree) {
			if _, err := os.Stat(ch.Worktree); err == nil {
				if _, found := a.Coord.WS.Tree(ch.AgentID); !found {
					_, _ = a.Coord.WS.EnsureTree(ch.AgentID, false)
				}
				if base, existed, err := a.Coord.WS.BaseContent(ch.AgentID, ch.Path); err == nil && existed {
					before, hasBefore = string(base), true
					if bytes.IndexByte(base, 0) >= 0 {
						binary, before = true, ""
					} else if len(before) > maxDiffBytes {
						before, truncated = before[:maxDiffBytes], true
					}
				}
				var bin bool
				var trunc bool
				after, hasAfter, bin, trunc = readCapped(filepath.Join(ch.Worktree, rel))
				binary, truncated = binary || bin, truncated || trunc
			}
		}
		if !hasAfter && !hasBefore && ch.Status == "accepted" {
			after, hasAfter, binary, truncated = readCapped(filepath.Join(a.Cfg.Workspace, rel))
			out["note"] = "Accepted: showing the file as it is in your workspace."
		}
		if !hasAfter && !hasBefore && out["note"] == nil {
			out["note"] = "The agent's worktree was cleaned up, so the diff is no longer available."
		}
	}
	out["before"], out["after"] = before, after
	out["hasBefore"], out["hasAfter"] = hasBefore, hasAfter
	out["binary"], out["truncated"] = binary, truncated
	return out, nil
}

func conflictFail(err error, path string) error {
	var ce integration.ConflictError
	if errors.As(err, &ce) {
		reason := ce.Report.Reason
		if reason == "" {
			reason = "your workspace and the agent's change overlap"
		}
		return ipc.Fail{C: "integration_conflict", M: fmt.Sprintf("%s: %s. Nothing was changed. Resolve it by hand or reject this change.", path, reason)}
	}
	return ipc.Fail{C: "integration_failed", M: fmt.Sprintf("%s: %v", path, err)}
}

// ownWorktree reports whether path is one of this workspace's agent worktrees. Change records come from the database,
// so a path is checked before anything is read from it or removed under it.
func (a *App) ownWorktree(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	root := filepath.Join(a.Cfg.Workspace, ".relo", "worktrees") + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(path)+string(filepath.Separator), root)
}

// settle releases an agent's worktree once none of its changes is waiting for a decision.
func (a *App) settle(agentID string) {
	if agentID == "" || a.Coord.OpenChanges(agentID) > 0 || a.Coord.WS == nil {
		return
	}
	if err := a.Coord.WS.Release(agentID); err != nil {
		a.Log.Warn("release worktree", map[string]any{"agentId": agentID, "error": err.Error()})
	}
}

func (a *App) acceptChange(id string) (any, error) {
	return a.acceptChangeExpect(id, "")
}

// acceptChangeExpect applies a change. When expect is non-empty it must equal the fingerprint of the current
// projection; otherwise a stale preview is refused and nothing is written.
func (a *App) acceptChangeExpect(id, expect string) (any, error) {
	a.decisionMu.Lock()
	out, err, path := a.acceptChangeLocked(id, expect)
	a.decisionMu.Unlock()
	// E1: every other proposal on the written path gets a fresh projection outside decisionMu.
	if err == nil && path != "" {
		a.projectPaths([]string{path})
	}
	return out, err
}

func (a *App) acceptChangeLocked(id, expect string) (any, error, string) {
	ch, ok := a.Coord.FindChange(id)
	if !ok {
		return nil, ipc.Fail{C: "not_found", M: "change not found"}, ""
	}
	switch ch.Status {
	case "applied", "accepted":
		return map[string]any{"change": ch}, nil, "" // direct mode edits are already in place
	case "rejected":
		return nil, ipc.Fail{C: "invalid_state", M: "this change was rejected"}, ""
	case "proposed":
	default:
		return nil, ipc.Fail{C: "invalid_state", M: "this change is not waiting for a decision"}, ""
	}
	if expect != "" {
		analysis, err := a.analyzeChange(ch)
		if err != nil {
			return nil, err, ""
		}
		if analysis.Fingerprint != expect {
			return nil, ipc.Fail{C: "stale_preview", M: "the file changed after the preview was made, so nothing was written; open the diff again and retry"}, ""
		}
	}
	outcome, err := a.mergeChange(ch)
	if err != nil {
		return nil, err, ""
	}
	updated, _ := a.Coord.UpdateChange(id, func(c *orchestrator.Change) {
		c.Status, c.Conflict, c.Resolving, c.Landed = "accepted", "", false, outcome.Action
	})
	a.settle(ch.AgentID)
	a.persist()
	return map[string]any{"change": updated, "integration": outcome}, nil, ch.Path
}

// resolveChange is the explicit resolution action for one change. head sends it to the head agent, mine rejects it
// and keeps the agent blob, and agent takes the agent's version through the journal.
func (a *App) resolveChange(id, choice string) (any, error) {
	switch choice {
	case "head":
		ch, ok := a.Coord.FindChange(id)
		if !ok {
			return nil, ipc.Fail{C: "not_found", M: "change not found"}
		}
		if ch.Status != "proposed" {
			return nil, ipc.Fail{C: "invalid_state", M: "this change is not waiting for a decision"}
		}
		a.Coord.UpdateChange(id, func(c *orchestrator.Change) { c.Resolving = true })
		a.sendHead(a.Coord.SessionID(), a.conflictPrompt([]orchestrator.Change{ch}))
		a.persist()
		return map[string]any{"change": ch, "choice": "head"}, nil
	case "mine":
		return a.rejectChange(id)
	case "agent":
		return a.takeAgentChange(id)
	default:
		return nil, ipc.Fail{C: "invalid_choice", M: "choice must be agent, mine or head"}
	}
}

// takeAgentChange writes the agent's version of a proposed change, journaling the user's pre-image first. It is
// only reachable through an explicit per-file choice.
func (a *App) takeAgentChange(id string) (any, error) {
	a.decisionMu.Lock()
	out, err, path := a.takeAgentChangeLocked(id)
	a.decisionMu.Unlock()
	if err == nil && path != "" {
		a.projectPaths([]string{path})
	}
	return out, err
}

func (a *App) takeAgentChangeLocked(id string) (any, error, string) {
	ch, ok := a.Coord.FindChange(id)
	if !ok {
		return nil, ipc.Fail{C: "not_found", M: "change not found"}, ""
	}
	if ch.Status != "proposed" {
		return nil, ipc.Fail{C: "invalid_state", M: "this change is not waiting for a decision"}, ""
	}
	if !a.ownWorktree(ch.Worktree) || ch.Path == "" {
		return nil, ipc.Fail{C: "invalid_change", M: "change lacks a worktree or path"}, ""
	}
	if a.pathDirty(ch.Path) {
		a.blockChange(ch.ID, "unsaved_buffer", "the file has unsaved changes in the editor; save it and try again")
		return nil, ipc.Fail{C: "unsaved_buffer", M: ch.Path + ": the file has unsaved changes in the editor, so nothing was written"}, ""
	}
	journal := func(path string, pre []byte, preExists bool, post []byte, postExists bool) error {
		_, jerr := a.Coord.WS.JournalChange(ch.ID, []workspace.JournalWrite{{Path: path, Post: post, PostExists: postExists}})
		if jerr == nil {
			return nil
		}
		if !preExists {
			a.Log.Warn("journal create", map[string]any{"changeId": ch.ID, "path": path, "error": jerr.Error()})
			return nil
		}
		a.blockChange(ch.ID, "journal_failed", "the recovery journal could not be written, so nothing was changed: "+jerr.Error())
		return ipc.Fail{C: "journal_failed", M: path + ": the recovery journal could not be written, so nothing was changed: " + jerr.Error()}
	}
	outcome, err := integration.TakeWithHook(a.Cfg.Workspace, ch.Worktree, ch.Path, journal)
	if err != nil {
		var fail ipc.Fail
		if errors.As(err, &fail) {
			return nil, fail, ""
		}
		return nil, ipc.Fail{C: "integration_failed", M: fmt.Sprintf("%s: %v", ch.Path, err)}, ""
	}
	now := time.Now().UnixMilli()
	updated, _ := a.Coord.UpdateChange(id, func(c *orchestrator.Change) {
		c.Status, c.Conflict, c.Resolving, c.Landed = "accepted", "", false, outcome.Action
		c.Integration = &orchestrator.Integration{State: "clean", CheckedAt: now}
	})
	a.settle(ch.AgentID)
	a.persist()
	return map[string]any{"change": updated, "integration": outcome}, nil, ch.Path
}

// undoChange restores the journal pre-image of an accepted change when every touched path still holds the bytes
// Relo wrote. It writes nothing when any path changed since then.
func (a *App) undoChange(id string) (any, error) {
	a.decisionMu.Lock()
	out, err, restored := a.undoChangeLocked(id)
	a.decisionMu.Unlock()
	if len(restored) > 0 {
		a.projectPaths(restored)
	}
	return out, err
}

func (a *App) undoChangeLocked(id string) (any, error, []string) {
	ch, ok := a.Coord.FindChange(id)
	if !ok {
		return nil, ipc.Fail{C: "not_found", M: "change not found"}, nil
	}
	if ch.Status != "accepted" {
		return nil, ipc.Fail{C: "invalid_state", M: "only an accepted change has a journal entry to undo"}, nil
	}
	res, err := a.Coord.WS.UndoChange(id)
	if err != nil {
		return nil, ipc.Fail{C: "undo_refused", M: err.Error()}, nil
	}
	updated, _ := a.Coord.UpdateChange(id, func(c *orchestrator.Change) {
		c.Status, c.Conflict, c.Resolving, c.Landed = "rejected", "", false, ""
	})
	a.persist()
	return map[string]any{"change": updated, "undo": res}, nil, append([]string(nil), res.Restored...)
}

// mergeChange three-way merges one proposed change into the workspace. It does not change the status or persist.
// The write is a compare-and-swap with a journal entry holding the user's pre-image, and an unsaved editor buffer
// blocks it before anything is read for writing.
func (a *App) mergeChange(ch orchestrator.Change) (integration.Outcome, error) {
	if !a.ownWorktree(ch.Worktree) || ch.Path == "" {
		return integration.Outcome{}, ipc.Fail{C: "invalid_change", M: "change lacks a worktree or path"}
	}
	if a.pathDirty(ch.Path) {
		a.blockChange(ch.ID, "unsaved_buffer", "the file has unsaved changes in the editor; save it and try again")
		return integration.Outcome{}, ipc.Fail{C: "unsaved_buffer", M: ch.Path + ": the file has unsaved changes in the editor, so nothing was written"}
	}
	if _, err := os.Stat(ch.Worktree); err != nil {
		return integration.Outcome{}, ipc.Fail{C: "not_found", M: "the agent's worktree no longer exists, so this change cannot be applied"}
	}
	tree, err := a.Coord.WS.EnsureTree(ch.AgentID, false)
	if err != nil || tree.Path != ch.Worktree {
		return integration.Outcome{}, ipc.Fail{C: "not_found", M: "the agent's worktree could not be opened"}
	}
	base, hadBase, err := a.baseOf(ch)
	if err != nil {
		return integration.Outcome{}, ipc.Fail{C: "integration_failed", M: err.Error()}
	}
	journal := func(path string, pre []byte, preExists bool, post []byte, postExists bool) error {
		_, jerr := a.Coord.WS.JournalChange(ch.ID, []workspace.JournalWrite{{Path: path, Post: post, PostExists: postExists}})
		if jerr == nil {
			return nil
		}
		// Creating a path cannot lose user bytes, so a journal failure there is reported but not fatal.
		if !preExists {
			a.Log.Warn("journal create", map[string]any{"changeId": ch.ID, "path": path, "error": jerr.Error()})
			return nil
		}
		a.blockChange(ch.ID, "journal_failed", "the recovery journal could not be written, so nothing was changed: "+jerr.Error())
		return ipc.Fail{C: "journal_failed", M: path + ": the recovery journal could not be written, so nothing was changed: " + jerr.Error()}
	}
	outcome, err := integration.MergeWithHook(a.Cfg.Workspace, tree.Path, ch.Path, base, hadBase, journal)
	if err != nil {
		var ce integration.ConflictError
		switch {
		case errors.As(err, &ce):
			integ := &orchestrator.Integration{State: "conflicted", Kind: orchestrator.LegacyConflictKind(ce.Report.Reason), Reason: ce.Report.Reason, CheckedAt: time.Now().UnixMilli()}
			// Re-run the non-writing analysis for the exact kind, hunks and workspace blob; the merge already
			// refused, so this is a rare path and adds no write.
			if analysis, aerr := a.analyzeChange(ch); aerr == nil {
				integ.Kind, integ.Ours, integ.Hunks = analysis.Kind, analysis.Ours, integrationHunks(analysis.Hunks)
			}
			a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) { c.Conflict, c.Integration = ce.Report.Reason, integ })
		case errors.As(err, new(integration.WorkspaceChangedError)):
			a.blockChange(ch.ID, "workspace_changed", err.Error())
			return integration.Outcome{}, ipc.Fail{C: "workspace_changed", M: ch.Path + ": the file kept changing while the change was being applied, so nothing was written"}
		case errors.As(err, new(integration.ConcurrentWriteError)):
			a.blockChange(ch.ID, "concurrent_write", err.Error())
			return integration.Outcome{}, ipc.Fail{C: "concurrent_write", M: ch.Path + ": another program wrote the file while the change was being applied; use Undo to restore your bytes"}
		}
		var fail ipc.Fail
		if errors.As(err, &fail) {
			return integration.Outcome{}, fail
		}
		return integration.Outcome{}, conflictFail(err, ch.Path)
	}
	if outcome.Action == "unchanged" {
		// Nothing was written, but every accept still gets an undo record; restoring the same bytes is a no-op.
		data, rerr := os.ReadFile(filepath.Join(a.Cfg.Workspace, filepath.FromSlash(ch.Path)))
		if rerr == nil || os.IsNotExist(rerr) {
			write := workspace.JournalWrite{Path: ch.Path, Post: data, PostExists: rerr == nil}
			if _, jerr := a.Coord.WS.JournalChange(ch.ID, []workspace.JournalWrite{write}); jerr != nil {
				a.Log.Warn("journal unchanged", map[string]any{"changeId": ch.ID, "path": ch.Path, "error": jerr.Error()})
			}
		}
	}
	a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) {
		if c.Integration == nil {
			c.Integration = &orchestrator.Integration{}
		}
		c.Integration.State, c.Integration.Kind, c.Integration.Reason, c.Integration.CheckedAt = "clean", "", "", time.Now().UnixMilli()
	})
	return outcome, nil
}

// blockChange records a write-blocking state without changing the change's status.
func (a *App) blockChange(id, kind, reason string) {
	a.Coord.UpdateChange(id, func(c *orchestrator.Change) {
		c.Conflict = reason
		c.Integration = &orchestrator.Integration{State: "blocked", Kind: kind, Reason: reason, CheckedAt: time.Now().UnixMilli()}
	})
}

func (a *App) rejectChange(id string) (any, error) {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()
	return a.rejectChangeLocked(id)
}

func (a *App) rejectChangeLocked(id string) (any, error) {
	ch, ok := a.Coord.FindChange(id)
	if !ok {
		return nil, ipc.Fail{C: "not_found", M: "change not found"}
	}
	switch ch.Status {
	case "rejected":
		return map[string]any{"change": ch}, nil
	case "applied":
		return nil, ipc.Fail{C: "direct_mode", M: "This project is not a Git repository, so the edit was made in place and cannot be reverted. Run git init to get isolated, reviewable changes."}
	case "accepted":
		return nil, ipc.Fail{C: "invalid_state", M: "this change was already accepted"}
	case "proposed":
	default:
		return nil, ipc.Fail{C: "invalid_state", M: "this change is not waiting for a decision"}
	}
	if ch.AgentID == "" || ch.Path == "" {
		return nil, ipc.Fail{C: "invalid_change", M: "change lacks an agent or path"}
	}
	// The worktree is separate from the workspace, so rejecting never touches the user's files. Discarding the file
	// there only keeps the agent's tree consistent; if the tree is already gone there is nothing left to do.
	if _, err := os.Stat(ch.Worktree); err == nil && a.ownWorktree(ch.Worktree) {
		if err := a.Coord.WS.KeepChangeBlob(ch.ID, ch.Worktree, ch.Path); err != nil {
			a.Log.Warn("keep rejected agent blob", map[string]any{"changeId": ch.ID, "path": ch.Path, "error": err.Error()})
		}
		if tree, terr := a.Coord.WS.EnsureTree(ch.AgentID, false); terr == nil && tree.Path == ch.Worktree {
			if err := a.Coord.WS.Discard(ch.AgentID, ch.Path); err != nil {
				a.Log.Warn("discard worktree file", map[string]any{"agentId": ch.AgentID, "path": ch.Path, "error": err.Error()})
			}
		}
	}
	updated, _ := a.Coord.UpdateChange(id, func(c *orchestrator.Change) { c.Status, c.Conflict, c.Resolving = "rejected", "", false })
	a.settle(ch.AgentID)
	a.persist()
	return map[string]any{"change": updated}, nil
}

type bulkFailure struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// decideAll accepts or rejects every change that is waiting, so a conflict in one file does not block the rest.
// Accepts merge changes to the same destination path in their original order and different paths concurrently, then update
// the session, release worktrees and persist once for the whole batch.
func (a *App) decideAll(accept bool) (any, error) {
	if !accept {
		return a.rejectAll()
	}
	return a.acceptAll(false)
}

func (a *App) acceptAll(auto bool) (any, error) {
	a.decisionMu.Lock()
	out, err, paths := a.acceptAllLocked(auto)
	a.decisionMu.Unlock()
	if len(paths) > 0 {
		a.projectPaths(paths)
	}
	return out, err
}

func (a *App) acceptAllLocked(auto bool) (any, error, []string) {
	var waiting []orchestrator.Change
	for _, ch := range a.Coord.Changes() {
		if ch.Status == "proposed" && (!auto || (a.autoApply.Load() && autoEligible(ch))) {
			waiting = append(waiting, ch)
		}
	}
	// With lazy projection, a change with no projection (for example a row written before P3) is projected before
	// any member of the batch is written. Its fresh state then decides whether it may be merged at all.
	if a.lazy.Load() {
		var paths []string
		seen := map[string]bool{}
		for _, ch := range waiting {
			if ch.Integration == nil && ch.Path != "" && !seen[ch.Path] {
				seen[ch.Path] = true
				paths = append(paths, ch.Path)
			}
		}
		if len(paths) > 0 {
			a.projectPaths(paths)
			for i := range waiting {
				if now, ok := a.Coord.FindChange(waiting[i].ID); ok {
					waiting[i] = now
				}
			}
		}
	}
	errs := make([]error, len(waiting))
	skipped := make([]bool, len(waiting))
	actions := make([]string, len(waiting))
	groups := map[string][]int{}
	var order []string
	for i, ch := range waiting {
		key := ch.Path
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	sem := make(chan struct{}, bulkMergeWorkers)
	var wg sync.WaitGroup
	for _, key := range order {
		wg.Add(1)
		go func(idx []int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for _, i := range idx {
				current, ok := a.Coord.FindChange(waiting[i].ID)
				if !ok {
					errs[i] = ipc.Fail{C: "not_found", M: "change not found"}
					continue
				}
				if auto && (!a.autoApply.Load() || !autoEligible(current)) {
					skipped[i] = true
					continue
				}
				if current.Status != "proposed" {
					errs[i] = ipc.Fail{C: "invalid_state", M: "this change is not waiting for a decision"}
					continue
				}
				if auto && a.pathDirty(current.Path) {
					a.blockChange(current.ID, "unsaved_buffer", "the file has unsaved changes in the editor; save it and try again")
					errs[i] = ipc.Fail{C: "unsaved_buffer", M: current.Path + ": the file has unsaved changes in the editor, so nothing was written"}
					continue
				}
				// With lazy projection, accept-all writes only changes that already project clean; a conflicted,
				// blocked or not-yet-projected member is reported and left proposed.
				if a.lazy.Load() && (current.Integration == nil || current.Integration.State != "clean") {
					reason := "the change could not be projected"
					if current.Integration != nil && current.Integration.Reason != "" {
						reason = current.Integration.Reason
					}
					errs[i] = ipc.Fail{C: "integration_conflict", M: current.Path + ": " + reason + ". Nothing was changed."}
					continue
				}
				var outcome integration.Outcome
				outcome, errs[i] = a.mergeChange(current)
				actions[i] = outcome.Action
			}
		}(groups[key])
	}
	wg.Wait()

	done := 0
	failed := []bulkFailure{}
	var accepted []string
	var acceptedPaths []string
	landed := map[string][]string{}
	agents := map[string]bool{}
	for i, ch := range waiting {
		if skipped[i] {
			continue
		}
		if errs[i] != nil {
			failed = append(failed, bulkFailure{ID: ch.ID, Path: ch.Path, Reason: errs[i].Error()})
			if auto {
				a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) { c.Conflict = errs[i].Error() })
			}
			continue
		}
		accepted = append(accepted, ch.ID)
		acceptedPaths = append(acceptedPaths, ch.Path)
		if actions[i] != "" {
			landed[actions[i]] = append(landed[actions[i]], ch.ID)
		}
		agents[ch.AgentID] = true
		done++
	}
	a.Coord.UpdateChanges(accepted, func(c *orchestrator.Change) { c.Status, c.Conflict, c.Resolving = "accepted", "", false })
	for action, ids := range landed {
		a.Coord.UpdateChanges(ids, func(c *orchestrator.Change) { c.Landed = action })
	}
	for agentID := range agents {
		a.settle(agentID)
	}
	if done > 0 || (auto && len(failed) > 0) {
		a.persist()
	}
	return map[string]any{"done": done, "failed": failed, "changes": a.Coord.Changes()}, nil, acceptedPaths
}

func (a *App) rejectAll() (any, error) {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()

	done := 0
	failed := []bulkFailure{}
	for _, ch := range a.Coord.Changes() {
		if ch.Status != "proposed" {
			continue
		}
		if _, err := a.rejectChangeLocked(ch.ID); err != nil {
			failed = append(failed, bulkFailure{ID: ch.ID, Path: ch.Path, Reason: err.Error()})
			continue
		}
		done++
	}
	return map[string]any{"done": done, "failed": failed, "changes": a.Coord.Changes()}, nil
}

// bulkMergeWorkers bounds how many files are merged at the same time during accept-all.
const bulkMergeWorkers = 4
