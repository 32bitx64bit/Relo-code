package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/orchestrator"
)

const overlapReason = "your edits and the agent's edits overlap"

// promptDiffBytes caps the agent's version of each conflicted file in the head message.
const promptDiffBytes = 4000

// hunkContext is how many unchanged lines around a conflicting hunk are shown to the head agent.
const hunkContext = 3

var blobRe = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// projectionMemo is the stat key for one change's last projection. A hit proves the workspace file Relo read is
// still the same file, so the projection can be reused without running git merge-file again.
type projectionMemo struct {
	path  string
	mtime int64
	size  int64
	blob  string
}

// resolveConflicts is the explicit "send conflicted changes to the head agent" action. With lazy conflicts on it
// sends the changes whose stored projection is already conflicted; it is not the detector. With the flag off it
// keeps the pre-P3 behavior of re-checking every proposal now.
func (a *App) resolveConflicts() (any, error) {
	if a.lazy.Load() {
		return a.sendConflictedToHead()
	}
	return a.resolveConflictsLegacy()
}

func (a *App) resolveConflictsLegacy() (any, error) {
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
	if len(conflicted) == 0 {
		return map[string]any{"conflicted": 0, "prompted": false, "changes": a.Coord.Changes()}, nil
	}
	a.sendHead(sid, a.conflictPrompt(conflicted))
	a.persist()
	return map[string]any{"conflicted": len(conflicted), "prompted": true, "changes": a.Coord.Changes()}, nil
}

// sendConflictedToHead projects every proposal once, then asks the head agent to re-apply only the ones whose
// projection is conflicted. No merge runs under decisionMu.
func (a *App) sendConflictedToHead() (any, error) {
	a.projectPaths(a.proposedPaths())
	var conflicted []orchestrator.Change
	var ids []string
	for _, ch := range a.Coord.Changes() {
		if ch.Status != "proposed" || !isConflicted(ch) {
			continue
		}
		conflicted = append(conflicted, ch)
		ids = append(ids, ch.ID)
	}
	a.Coord.UpdateChanges(ids, func(c *orchestrator.Change) { c.Resolving = true })
	if len(conflicted) == 0 {
		return map[string]any{"conflicted": 0, "prompted": false, "changes": a.Coord.Changes()}, nil
	}
	a.sendHead(a.Coord.SessionID(), a.conflictPrompt(conflicted))
	a.persist()
	return map[string]any{"conflicted": len(conflicted), "prompted": true, "changes": a.Coord.Changes()}, nil
}

// isConflicted reports whether a change waits on the user because its projection conflicts.
func isConflicted(ch orchestrator.Change) bool {
	if ch.Integration != nil {
		return ch.Integration.State == "conflicted"
	}
	return ch.Conflict != ""
}

// proposedPaths returns the workspace-relative paths that still wait for a decision.
func (a *App) proposedPaths() []string {
	seen := map[string]bool{}
	var paths []string
	for _, ch := range a.Coord.Changes() {
		if ch.Status == "proposed" && ch.Path != "" && !seen[ch.Path] {
			seen[ch.Path] = true
			paths = append(paths, ch.Path)
		}
	}
	return paths
}

// projectPaths re-projects every proposed change whose path is in paths, as one batched update. It is the E1 hook:
// call it after any accept or undo wrote a path, outside decisionMu.
func (a *App) projectPaths(paths []string) {
	if !a.lazy.Load() || len(paths) == 0 {
		return
	}
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
	var updates []orchestrator.IntegrationUpdate
	for _, ch := range a.Coord.Changes() {
		if ch.Status != "proposed" || !want[ch.Path] {
			continue
		}
		updates = append(updates, a.projectChange(ch))
	}
	a.Coord.UpdateIntegration(updates)
}

// projectChange computes one change's current projection without writing anything.
func (a *App) projectChange(ch orchestrator.Change) orchestrator.IntegrationUpdate {
	lock := a.pathLock(ch.Path)
	lock.Lock()
	defer lock.Unlock()

	if a.projectionFresh(ch) {
		since := a.sinceFor(ch, integration.Analysis{Ours: ch.Integration.Ours})
		if !reflect.DeepEqual(since, ch.Integration.Since) {
			updated := *ch.Integration
			updated.Since = since
			return orchestrator.IntegrationUpdate{ID: ch.ID, Integration: &updated, Conflict: ch.Conflict}
		}
		return orchestrator.IntegrationUpdate{ID: ch.ID, Integration: ch.Integration, Conflict: ch.Conflict}
	}
	now := time.Now().UnixMilli()
	analysis, err := a.analyzeChange(ch)
	if err != nil {
		reason := err.Error()
		state, kind := "unknown", "check_failed"
		if strings.Contains(reason, "worktree is unavailable") {
			state, kind = "blocked", "worktree_gone"
		}
		integ := &orchestrator.Integration{State: state, Kind: kind, Reason: reason, CheckedAt: now}
		return orchestrator.IntegrationUpdate{ID: ch.ID, Integration: integ, Conflict: reason}
	}
	state := "clean"
	if analysis.Reason != "" {
		state = "conflicted"
	}
	integ := &orchestrator.Integration{
		State: state, Kind: analysis.Kind, Reason: analysis.Reason, Ours: analysis.Ours,
		Hunks: integrationHunks(analysis.Hunks), Since: a.sinceFor(ch, analysis),
		CheckedAt: now, Fingerprint: analysis.Fingerprint,
	}
	a.rememberProjection(ch, analysis.Ours)
	return orchestrator.IntegrationUpdate{ID: ch.ID, Integration: integ, Conflict: analysis.Reason}
}

// projectionFresh reports whether the stored projection's workspace file is unchanged since it was computed.
func (a *App) projectionFresh(ch orchestrator.Change) bool {
	if ch.Integration == nil || ch.Integration.Ours == "" {
		return false
	}
	info, err := a.workspaceStat(ch.Path)
	if err != nil {
		return false
	}
	a.projMu.Lock()
	memo, ok := a.projMemo[ch.ID]
	a.projMu.Unlock()
	if ok && memo.path == ch.Path && memo.mtime == info.mtime && memo.size == info.size && memo.blob == ch.Integration.Ours {
		return true
	}
	// E3: even after a stat change, hash the file. The same blob means the projection is still current.
	full := filepath.Join(a.Cfg.Workspace, filepath.FromSlash(ch.Path))
	data, rerr := os.ReadFile(full)
	exists := rerr == nil
	if rerr != nil && !os.IsNotExist(rerr) {
		return false
	}
	if integration.BlobID(data, exists) != ch.Integration.Ours {
		return false
	}
	a.rememberProjection(ch, ch.Integration.Ours)
	return true
}

func (a *App) rememberProjection(ch orchestrator.Change, blob string) {
	info, err := a.workspaceStat(ch.Path)
	if err != nil {
		return
	}
	a.projMu.Lock()
	if a.projMemo == nil {
		a.projMemo = map[string]projectionMemo{}
	}
	a.projMemo[ch.ID] = projectionMemo{path: ch.Path, mtime: info.mtime, size: info.size, blob: blob}
	a.projMu.Unlock()
}

type fileStat struct {
	mtime int64
	size  int64
}

func (a *App) workspaceStat(path string) (fileStat, error) {
	info, err := os.Stat(filepath.Join(a.Cfg.Workspace, filepath.FromSlash(path)))
	if err != nil {
		if os.IsNotExist(err) {
			return fileStat{mtime: -1, size: -1}, nil
		}
		return fileStat{}, err
	}
	return fileStat{mtime: info.ModTime().UnixNano(), size: info.Size()}, nil
}

// pathLock returns the projection lock for one path. Projection takes this lock, never decisionMu, so an O(N)
// merge-file loop cannot block a decision.
func (a *App) pathLock(path string) *sync.Mutex {
	a.projMu.Lock()
	defer a.projMu.Unlock()
	if a.projLocks == nil {
		a.projLocks = map[string]*sync.Mutex{}
	}
	lock := a.projLocks[path]
	if lock == nil {
		lock = &sync.Mutex{}
		a.projLocks[path] = lock
	}
	return lock
}

// sinceFor names the accepted changes, or "user", that moved the file after the change's base. An accept that
// landed before the cut is already part of BaseRev and is not a cause of the current projection.
func (a *App) sinceFor(ch orchestrator.Change, analysis integration.Analysis) []string {
	base, hadBase, baseErr := a.baseOf(ch)
	baseBlob := ""
	if baseErr == nil {
		baseBlob = integration.BlobID(base, hadBase)
	}
	var since []string
	for _, other := range a.Coord.Changes() {
		if other.ID == ch.ID || other.Path != ch.Path || other.Status != "accepted" {
			continue
		}
		if !a.landedAfterCut(other, baseBlob, baseErr == nil) {
			continue
		}
		since = append(since, other.ID)
	}
	if len(since) == 0 && (baseErr != nil || baseBlob != analysis.Ours) {
		since = []string{"user"}
	}
	return since
}

// landedAfterCut reports whether an accepted change wrote this path after the cut. The journal entry written by
// the accept holds its post-image: when that image is already the change's base content, the accept was part of
// the base and must not be named in Since. When the journal is unavailable, keep the attribution.
func (a *App) landedAfterCut(other orchestrator.Change, baseBlob string, haveBase bool) bool {
	if !haveBase {
		return true
	}
	rec, _, err := a.Coord.WS.JournalEntryFor(other.ID)
	if err != nil {
		return true
	}
	for _, f := range rec.Files {
		if f.Path == other.Path {
			return f.Post != baseBlob
		}
	}
	return true
}

// checkChange returns "conflicted", "clean" or "skipped". It sets or clears Change.Conflict and records the full
// Integration (state, kind, hunks, workspace blob) and never writes a file.
func (a *App) checkChange(ch orchestrator.Change) (string, error) {
	if !a.ownWorktree(ch.Worktree) || ch.Path == "" || !filepath.IsLocal(filepath.FromSlash(ch.Path)) {
		return "skipped", nil
	}
	now := time.Now().UnixMilli()
	analysis, err := a.analyzeChange(ch)
	if err != nil {
		reason := err.Error()
		state, kind := "unknown", "check_failed"
		if strings.Contains(reason, "worktree is unavailable") {
			state, kind = "blocked", "worktree_gone"
		}
		integ := &orchestrator.Integration{State: state, Kind: kind, Reason: reason, CheckedAt: now}
		a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) { c.Conflict, c.Integration = reason, integ })
		return "conflicted", nil
	}
	state := "clean"
	if analysis.Reason != "" {
		state = "conflicted"
	}
	integ := &orchestrator.Integration{
		State: state, Kind: analysis.Kind, Reason: analysis.Reason, Ours: analysis.Ours,
		Hunks: integrationHunks(analysis.Hunks), Since: a.sinceFor(ch, analysis),
		CheckedAt: now, Fingerprint: analysis.Fingerprint,
	}
	a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) { c.Conflict, c.Integration = analysis.Reason, integ })
	if analysis.Reason != "" {
		return "conflicted", nil
	}
	return "clean", nil
}

// integrationHunks converts the integration package's hunk locations for storage on the change.
func integrationHunks(hunks []integration.Hunk) []orchestrator.Hunk {
	if len(hunks) == 0 {
		return nil
	}
	out := make([]orchestrator.Hunk, 0, len(hunks))
	for _, h := range hunks {
		out = append(out, orchestrator.Hunk{OursStart: h.OursStart, OursLen: h.OursLen, TheirsStart: h.TheirsStart, TheirsLen: h.TheirsLen, Conflicting: h.Conflicting})
	}
	return out
}

// conflictReason recomputes why a change cannot be accepted as it is, or "" when it merges cleanly.
func (a *App) conflictReason(ch orchestrator.Change) (string, error) {
	analysis, err := a.analyzeChange(ch)
	if err != nil {
		return "", err
	}
	return analysis.Reason, nil
}

// analyzeChange projects the change against the workspace file as it is now, without writing.
func (a *App) analyzeChange(ch orchestrator.Change) (integration.Analysis, error) {
	// Do not recreate a released worktree: its proposed edits are no longer available.
	if _, err := os.Stat(ch.Worktree); err != nil {
		return integration.Analysis{}, fmt.Errorf("the agent's worktree is unavailable: %w", err)
	}
	tree, err := a.Coord.WS.EnsureTree(ch.AgentID, false)
	if err != nil {
		return integration.Analysis{}, fmt.Errorf("the agent's worktree could not be opened: %w", err)
	}
	if tree.Path != ch.Worktree {
		return integration.Analysis{}, fmt.Errorf("the agent's worktree does not match the change")
	}
	base, hadBase, err := a.baseOf(ch)
	if err != nil {
		return integration.Analysis{}, ipc.Fail{C: "integration_failed", M: fmt.Sprintf("%s: %v", ch.Path, err)}
	}
	analysis, err := integration.Analyze(a.Cfg.Workspace, ch.Worktree, ch.Path, base, hadBase)
	if err != nil {
		return integration.Analysis{}, ipc.Fail{C: "integration_failed", M: fmt.Sprintf("%s: %v", ch.Path, err)}
	}
	return analysis, nil
}

// conflictPrompt is the head message. It lists each conflicted change and asks for its intent to be re-applied.
// When the projection has hunk locations, only the conflicting hunks and their context are shown.
func (a *App) conflictPrompt(conflicted []orchestrator.Change) string {
	var b strings.Builder
	b.WriteString("Resolve conflicts. These proposed changes overlap with edits made in the workspace after the agent started, so none was applied. " +
		"Re-apply each change's intent on the current tree, keep the user's edits, and delegate the work. The changes below will be replaced by the new ones.\n")
	for _, ch := range conflicted {
		title := ch.Path
		if t, ok := a.Coord.Graph.Get(ch.TaskID); ok && t.Title != "" {
			title = t.Title
		}
		fmt.Fprintf(&b, "\n- id: %s\n  title: %s\n  path: %s\n  reason: %s\n", ch.ID, title, ch.Path, ch.Conflict)
		b.WriteString(a.agentConflictCopy(ch))
	}
	return b.String()
}

// agentConflictCopy returns only the conflicting hunks of the agent's file with context. A change without hunk
// locations falls back to the capped whole file.
func (a *App) agentConflictCopy(ch orchestrator.Change) string {
	data, err := os.ReadFile(filepath.Join(ch.Worktree, filepath.FromSlash(ch.Path)))
	if err != nil {
		return "  agent version:\n  (agent version unavailable)\n"
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "  agent version:\n  (binary, not shown)\n"
	}
	var conflicting []orchestrator.Hunk
	if ch.Integration != nil {
		for _, h := range ch.Integration.Hunks {
			if h.Conflicting && h.TheirsLen > 0 {
				conflicting = append(conflicting, h)
			}
		}
	}
	if len(conflicting) == 0 {
		return "  agent version:\n" + agentCopy(ch)
	}
	lines := strings.Split(string(data), "\n")
	var b strings.Builder
	b.WriteString("  agent version (only the conflicting hunks, with context):\n")
	for _, h := range conflicting {
		start := h.TheirsStart - hunkContext
		if start < 1 {
			start = 1
		}
		end := h.TheirsStart + h.TheirsLen - 1 + hunkContext
		if end > len(lines) {
			end = len(lines)
		}
		fmt.Fprintf(&b, "  lines %d-%d:\n```\n", start, end)
		for i := start; i <= end; i++ {
			b.WriteString(lines[i-1])
			b.WriteByte('\n')
		}
		b.WriteString("```\n")
	}
	if b.Len() > promptDiffBytes {
		return "  agent version:\n" + agentCopy(ch)
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

// supersedeStale replaces a conflicted proposal for a path once a newer proposal for that path exists. The old
// change is kept with SupersededBy set and its agent blob retained; it is not silently discarded.
func (a *App) supersedeStale() {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()

	changes := a.Coord.Changes()
	newest := map[string]orchestrator.Change{}
	for _, ch := range changes {
		if ch.Status != "proposed" || isConflicted(ch) || ch.SupersededBy != "" {
			continue
		}
		newest[ch.Path] = ch
	}
	for _, old := range changes {
		if old.Status != "proposed" || !isConflicted(old) {
			continue
		}
		newer, ok := newest[old.Path]
		if !ok || newer.ID == old.ID {
			continue
		}
		if _, err := a.rejectChangeLocked(old.ID); err != nil {
			a.Log.Warn("supersede conflicted change", map[string]any{"changeId": old.ID, "error": err.Error()})
			continue
		}
		a.Coord.UpdateChange(old.ID, func(c *orchestrator.Change) {
			c.Superseded, c.SupersededBy, c.Conflict, c.Resolving = true, newer.ID, "", false
		})
	}
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
