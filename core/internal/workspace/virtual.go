package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/relocode/relo/internal/vws"
)

// Mode says how an agent's isolated copy of the repository is held.
type Mode string

const (
	// ModeWorktree gives every isolated agent a Git worktree. It is the fallback for a mode the core does not know.
	ModeWorktree Mode = "worktree"
	// ModeVirtual holds the agent's state as Git objects and refs, and lays out a directory only while its tools run.
	ModeVirtual Mode = "virtual"
	// ModeAuto uses ModeVirtual wherever the repository supports it and ModeWorktree everywhere else.
	ModeAuto Mode = "auto"
)

// Config is the workspace rollout setting (relo.workspace.*).
type Config struct {
	Mode Mode `json:"mode"`
	// Materialization names how a directory is made when tools need real files: "automatic" or "worktree". Both use
	// a Git worktree today; the setting exists so a cheaper backend can be offered without a migration.
	Materialization string `json:"materialization"`
	// VirtualReads asks for the model's file tools to read the overlay directly. It only takes effect where the
	// runtime can guarantee that every file tool is routed through Relo; otherwise it is ignored with a notice.
	VirtualReads bool `json:"virtualReads"`
}

// DefaultConfig is the setting every Manager starts with. Production leaves it empty, which means virtual workspaces;
// a test run sets it to put a whole suite through another mode and compare.
var DefaultConfig Config

// Normalized fills defaults and replaces values it does not know with the safe ones. An empty mode means virtual; a
// mode the core does not know means worktrees, so a bad value can never turn virtual workspaces on.
func (c Config) Normalized() Config {
	switch Mode(strings.ToLower(strings.TrimSpace(string(c.Mode)))) {
	case "", ModeVirtual:
		c.Mode = ModeVirtual
	case ModeAuto:
		c.Mode = ModeAuto
	default:
		c.Mode = ModeWorktree
	}
	switch strings.ToLower(strings.TrimSpace(c.Materialization)) {
	case "worktree":
		c.Materialization = "worktree"
	default:
		c.Materialization = "automatic"
	}
	return c
}

// Configure sets the rollout mode. It applies to agents created after the call: an agent already holding a
// worktree or a virtual workspace keeps it until it is released. Asking for virtual reads on a runtime that cannot
// route its file tools through Relo is answered with a notice, once, and the files are read from the directory.
func (m *Manager) Configure(c Config) {
	c = c.Normalized()
	m.cfgMu.Lock()
	m.cfg = c
	routed := m.routedTools
	m.cfgMu.Unlock()
	if c.VirtualReads && !routed {
		m.tell("workspace.virtualReads", "relo.workspace.virtualReads is on, but this runtime reads files itself and cannot be routed through Relo, so agents read their directories. Nothing else changes.")
	}
}

// SetRoutedFileTools records whether the runtime guarantees that every file tool of an agent goes through Relo. Only
// then can the model's reads be served from an overlay (Mode A); with any tool that touches the directory directly,
// the directory must be real (Mode B). OpenCode 2.x runs its own file tools, so this stays false for it.
func (m *Manager) SetRoutedFileTools(routed bool) {
	m.cfgMu.Lock()
	m.routedTools = routed
	m.cfgMu.Unlock()
}

// VirtualReads reports whether the model's reads are served from the overlay: it was asked for and the runtime can
// guarantee that no tool bypasses it.
func (m *Manager) VirtualReads() bool {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg.VirtualReads && m.routedTools
}

// Config returns the rollout settings in force.
func (m *Manager) Config() Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	if m.cfg.Mode == "" {
		return DefaultConfig.Normalized()
	}
	return m.cfg
}

// SetNotifier registers where fallbacks and recoveries are reported. kind is a short stable word; message is for the user.
func (m *Manager) SetNotifier(fn func(kind, message string)) {
	m.vmu.Lock()
	m.notify = fn
	m.vmu.Unlock()
}

// SetObserver registers a function told about every published revision of every virtual workspace.
func (m *Manager) SetObserver(fn func(vws.Event)) {
	m.vmu.Lock()
	m.observer = fn
	m.vmu.Unlock()
}

func (m *Manager) tell(kind, message string) {
	m.vmu.Lock()
	fn := m.notify
	m.vmu.Unlock()
	if fn != nil {
		fn(kind, message)
	}
}

func (m *Manager) observe(ev vws.Event) {
	m.vmu.Lock()
	fn := m.observer
	m.vmu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// reloWorktrees is the folder execution directories live in. It is where worktree mode keeps its worktrees too, so
// an agent's path is the same in both modes.
func (m *Manager) reloWorktrees() string { return filepath.Join(m.root, ".relo", "worktrees") }

// engine returns the virtual workspace engine, opening it on first use. Without create it opens only when Git
// already holds virtual workspaces to adopt, so a repository that never used the feature pays nothing for it.
func (m *Manager) engine(create bool) *vws.Engine {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	if m.eng != nil {
		return m.eng
	}
	if !isGit(m.root) {
		return nil
	}
	if !create {
		if m.vwsAbsent {
			return nil
		}
		out, err := git(m.root, "for-each-ref", "--count=1", "--format=%(refname)", "refs/relo/vws/")
		if err != nil || out == "" {
			m.vwsAbsent = true
			return nil
		}
	}
	eng, err := vws.Open(m.root, vws.Options{
		// Only Relo's own folders are dropped at capture. The user's ignore paths hide a change from listings, as in a
		// worktree, so an edit to one is still in the overlay if the setting changes.
		Ignore:       isReloPath,
		ExecRoot:     m.reloWorktrees(),
		Materializer: &vws.WorktreeMaterializer{Prepare: func(dir string) { linkDeps(m.root, dir) }},
		Observer:     func(ev vws.Event) { go m.observe(ev) },
	})
	if err != nil {
		m.engErr = err
		return nil
	}
	m.eng, m.vwsAbsent = eng, false
	if rep, err := eng.Recover(context.Background()); err == nil && (len(rep.Recovered) > 0 || len(rep.Problems) > 0) {
		m.recovered = rep
		go m.reportRecovery(rep)
	}
	return eng
}

// Recovered is what the last start found: workspaces restored, materializations that were still open, and
// workspaces that could not be restored. It is empty when nothing virtual existed.
func (m *Manager) Recovered() vws.RecoverReport {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	return m.recovered
}

func (m *Manager) reportRecovery(rep vws.RecoverReport) {
	if len(rep.Interrupted) > 0 {
		m.tell("workspace.recovered", fmt.Sprintf("Relo stopped while %d agent(s) were working. Their unfinished edits were kept.", len(rep.Interrupted)))
	}
	for _, p := range rep.Problems {
		m.tell("workspace.damaged", fmt.Sprintf("Agent %s's virtual workspace could not be restored: %s", p.AgentID, p.Err))
	}
}

// wantVirtual says whether a new isolated copy should be a virtual workspace, and why not when it should not.
func (m *Manager) wantVirtual() (bool, string) {
	if cfg := m.Config(); cfg.Mode != ModeVirtual && cfg.Mode != ModeAuto {
		return false, ""
	}
	eng := m.engine(true)
	if eng == nil {
		return false, "the virtual workspace engine could not be opened"
	}
	if c := eng.RefreshCompat(); !c.OK {
		return false, "this repository rewrites file content on checkout (" + strings.Join(c.Reasons, "; ") + ")"
	}
	return true, ""
}

// reportRejected tells the user, once per path, about files an agent's tools made that were not imported: a link that
// leaves the workspace, a nested repository, a file over the size limit. A worktree would list these and let the
// merge refuse them; here they never reach Git, so the refusal must not be silent.
func (m *Manager) reportRejected(agentID string, res vws.CaptureResult) {
	for _, r := range res.Rejected {
		if r.Reason == "read-only workspace" {
			continue
		}
		key := agentID + "\x00" + r.Path + "\x00" + r.Reason
		m.vmu.Lock()
		if m.fallbackSeen == nil {
			m.fallbackSeen = map[string]bool{}
		}
		seen := m.fallbackSeen[key]
		m.fallbackSeen[key] = true
		m.vmu.Unlock()
		if !seen {
			m.tell("workspace.skipped", fmt.Sprintf("Agent %s made %s, which was not kept: %s.", agentID, r.Path, r.Reason))
		}
	}
}

// fellBack tells the user once per reason that a repository is on worktrees although virtual mode was asked for.
func (m *Manager) fellBack(reason string) {
	m.vmu.Lock()
	if m.fallbackSeen == nil {
		m.fallbackSeen = map[string]bool{}
	}
	seen := m.fallbackSeen[reason]
	m.fallbackSeen[reason] = true
	m.vmu.Unlock()
	if !seen && m.Config().Mode == ModeVirtual {
		m.tell("workspace.fallback", "Using Git worktrees for agents: "+reason+".")
	}
}

// virtualTreeLocked adopts an existing virtual workspace for agentID or makes a new one when the rollout mode
// asks for it. It reports ok=false when the agent should use a worktree. The caller holds m.mu.
func (m *Manager) virtualTreeLocked(agentID, dir string) (Tree, bool, error) {
	if eng := m.engine(false); eng != nil {
		if w, ok := eng.Get(agentID); ok {
			info := w.Info()
			t := Tree{AgentID: agentID, Path: dir, Branch: info.Branch, Base: info.BaseCommit, Virtual: true}
			m.trees[agentID] = t
			return t, true, nil
		}
	}
	if _, err := os.Stat(dir); err == nil {
		return Tree{}, false, nil // an existing worktree is adopted as one
	}
	want, why := m.wantVirtual()
	if !want {
		if why != "" {
			m.fellBack(why)
		}
		return Tree{}, false, nil
	}
	base := m.baseCommit()
	if base == "" {
		var err error
		if base, err = git(m.root, "rev-parse", "HEAD"); err != nil {
			return Tree{}, false, err
		}
	}
	w, err := m.engine(true).Create(agentID, base, vws.CreateOptions{})
	if err != nil {
		if errors.Is(err, vws.ErrUnsupportedRepo) {
			m.fellBack(err.Error())
			return Tree{}, false, nil
		}
		return Tree{}, false, err
	}
	info := w.Info()
	t := Tree{AgentID: agentID, Path: dir, Branch: info.Branch, Base: info.BaseCommit, Virtual: true}
	m.trees[agentID] = t
	return t, true, nil
}

// virtual returns the virtual workspace behind an agent.
func (m *Manager) virtual(agentID string) (*vws.Workspace, error) {
	eng := m.engine(false)
	if eng == nil {
		return nil, fmt.Errorf("no virtual workspace for agent %q", agentID)
	}
	w, ok := eng.Get(agentID)
	if !ok {
		return nil, fmt.Errorf("no virtual workspace for agent %q", agentID)
	}
	return w, nil
}

// hasVirtual reports whether the agent has a virtual workspace, opening the engine only if Git holds any.
func (m *Manager) hasVirtual(agentID string) bool {
	eng := m.engine(false)
	if eng == nil {
		return false
	}
	_, ok := eng.Get(agentID)
	return ok
}

// agentOf names the agent a worktree path belongs to, or "" for any other path.
func (m *Manager) agentOf(worktree string) string {
	if worktree == "" || filepath.Clean(filepath.Dir(worktree)) != m.reloWorktrees() {
		return ""
	}
	return filepath.Base(worktree)
}

// virtualOf returns the virtual workspace that a change's worktree path stands for, if there is one.
func (m *Manager) virtualOf(worktree string) (*vws.Workspace, bool) {
	id := m.agentOf(worktree)
	if id == "" {
		return nil, false
	}
	eng := m.engine(false)
	if eng == nil {
		return nil, false
	}
	return eng.Get(id)
}

func execContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

// Materialize makes the agent's files exist in a directory for tools that need real ones, and returns it. For an
// agent on a worktree it returns that worktree, which always exists. The directory is the agent's path in both
// modes, so a caller never needs to know which one it got. Call Settle when the tools are done.
func (m *Manager) Materialize(agentID string) (string, error) {
	t, ok := m.Tree(agentID)
	if !ok {
		return "", fmt.Errorf("no worktree for agent %q", agentID)
	}
	if !t.Virtual {
		return t.Path, nil
	}
	w, err := m.virtual(agentID)
	if err != nil {
		return "", err
	}
	ctx, cancel := execContext()
	defer cancel()
	ex, err := w.Materialize(ctx)
	if err != nil {
		return "", err
	}
	return ex.Dir, nil
}

// Sync imports what tools wrote in the agent's directory into its overlay, keeping the directory. Every reader of
// an agent's changes calls it first, so a diff never misses what a shell command produced.
func (m *Manager) Sync(agentID string) error {
	t, ok := m.Tree(agentID)
	if !ok || !t.Virtual {
		return nil
	}
	w, err := m.virtual(agentID)
	if err != nil {
		return err
	}
	ctx, cancel := execContext()
	defer cancel()
	res, err := w.Capture(ctx)
	m.reportRejected(agentID, res)
	return err
}

// Settle imports the agent's tool edits and removes its directory. What the agent made stays in its overlay, in Git,
// until the changes are decided. If the import fails the directory is kept so nothing is lost.
func (m *Manager) Settle(agentID string) error {
	t, ok := m.Tree(agentID)
	if !ok || !t.Virtual {
		return nil
	}
	w, err := m.virtual(agentID)
	if err != nil {
		return err
	}
	ctx, cancel := execContext()
	defer cancel()
	res, err := w.ReleaseExec(ctx, false)
	m.reportRejected(agentID, res)
	return err
}

// quiesce makes the overlay authoritative again: tools are done and their edits are imported.
func (m *Manager) quiesce(w *vws.Workspace) error {
	if w.Info().Exec == nil {
		return nil
	}
	ctx, cancel := execContext()
	defer cancel()
	_, err := w.ReleaseExec(ctx, false)
	return err
}

// Proposal identifies the frozen state of an agent's work that its changes were generated from.
type Proposal struct {
	Revision   uint64 `json:"revision"`
	Commit     string `json:"commit"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"baseCommit"`
}

// Propose imports any tool edits, commits the agent's overlay on its branch and pins the revision, so the changes
// recorded now are read exactly as they are now however the workspace moves afterwards. It is safe while the
// agent's directory is open and can be repeated: an unchanged state gives the same commit. ok is false for an
// agent on a worktree, which has no revisions.
func (m *Manager) Propose(agentID string) (p Proposal, ok bool, err error) {
	t, found := m.Tree(agentID)
	if !found || !t.Virtual {
		return Proposal{}, false, nil
	}
	w, err := m.virtual(agentID)
	if err != nil {
		return Proposal{}, false, err
	}
	if err := m.Sync(agentID); err != nil {
		return Proposal{}, false, err
	}
	if _, err := w.Commit(); err != nil {
		return Proposal{}, false, err
	}
	snap, err := w.Snapshot()
	if err != nil {
		return Proposal{}, false, err
	}
	return Proposal{Revision: snap.Revision, Commit: snap.Commit, Branch: snap.Branch, BaseCommit: snap.BaseCommit}, true, nil
}

// Proposed lists what the agent changed and, for a virtual workspace, the pinned revision the list was taken from.
// The files come from that frozen revision, so the list and the revision cannot disagree, and the proposal is not
// read again from a directory. For an agent on a worktree it is ChangedFiles and the revision is zero.
func (m *Manager) Proposed(agentID string) (files []FileChange, rev uint64, err error) {
	p, ok, err := m.Propose(agentID)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		files, err = m.ChangedFiles(agentID)
		return files, 0, err
	}
	w, err := m.virtual(agentID)
	if err != nil {
		return nil, 0, err
	}
	snap, err := w.SnapshotAt(p.Revision)
	if err != nil {
		return nil, 0, err
	}
	props, err := w.Proposals(snap)
	if err != nil {
		return nil, 0, err
	}
	for _, c := range props {
		if m.ignored(c.Path) {
			continue
		}
		files = append(files, FileChange{Path: c.Path, Kind: c.Op, Added: c.Added, Deleted: c.Deleted})
	}
	return files, p.Revision, nil
}

func (m *Manager) vChanged(agentID string) ([]string, error) {
	files, err := m.vChangedFiles(agentID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out, nil
}

func (m *Manager) vChangedFiles(agentID string) ([]FileChange, error) {
	w, err := m.virtual(agentID)
	if err != nil {
		return nil, err
	}
	if err := m.Sync(agentID); err != nil {
		return nil, err
	}
	diff, err := w.ReadDiff()
	if err != nil {
		return nil, err
	}
	out := make([]FileChange, 0, len(diff))
	for _, c := range diff {
		if m.ignored(c.Path) {
			continue
		}
		out = append(out, FileChange{Path: c.Path, Kind: c.Kind, Added: c.Added, Deleted: c.Deleted})
	}
	return out, nil
}

func (m *Manager) vPatch(agentID string) (string, error) {
	w, err := m.virtual(agentID)
	if err != nil {
		return "", err
	}
	if err := m.Sync(agentID); err != nil {
		return "", err
	}
	return w.Patch()
}

func (m *Manager) vPatchPath(agentID, rel string) (string, error) {
	w, err := m.virtual(agentID)
	if err != nil {
		return "", err
	}
	if err := m.Sync(agentID); err != nil {
		return "", err
	}
	return w.PatchPath(rel)
}

// vDiscard puts paths back to the agent's base. It is how rejecting a change leaves the agent's other edits alone.
func (m *Manager) vDiscard(agentID string, rels []string) error {
	w, err := m.virtual(agentID)
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if err := validateRelative(rel); err != nil {
			return err
		}
	}
	if err := m.quiesce(w); err != nil {
		return err
	}
	_, err = w.RestoreToBase(rels...)
	return err
}

func (m *Manager) vBaseContent(agentID, rel string) ([]byte, bool, error) {
	w, err := m.virtual(agentID)
	if err != nil {
		return nil, false, err
	}
	return w.BaseContent(rel)
}

func (m *Manager) vStack(agentID string, patches []string) ([]StackResult, error) {
	w, err := m.virtual(agentID)
	if err != nil {
		return nil, err
	}
	res, err := w.Stack(patches)
	out := make([]StackResult, len(res))
	for i, r := range res {
		out[i] = StackResult{Applied: r.Applied, Error: r.Error}
	}
	m.adoptBase(agentID, w)
	return out, err
}

func (m *Manager) vRecut(t Tree, base string) (Tree, error) {
	w, err := m.virtual(t.AgentID)
	if err != nil {
		return Tree{}, err
	}
	if err := w.Recut(base); err != nil {
		return Tree{}, fmt.Errorf("re-cut %q: %w", t.AgentID, err)
	}
	return m.adoptBase(t.AgentID, w), nil
}

// adoptBase records the base the workspace moved to, so Tree.Base and every diff taken from it agree with Git.
func (m *Manager) adoptBase(agentID string, w *vws.Workspace) Tree {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trees[agentID]
	t.Base = w.Info().BaseCommit
	m.trees[agentID] = t
	return t
}

// vRelease drops the workspace, its directory and its branch once nothing refers to it any more.
func (m *Manager) vRelease(agentID string) error {
	eng := m.engine(false)
	if eng == nil {
		return nil
	}
	w, ok := eng.Get(agentID)
	if !ok {
		return nil
	}
	ctx, cancel := execContext()
	defer cancel()
	if w.Info().Exec != nil {
		if err := w.DiscardExec(ctx); err != nil {
			return err
		}
	}
	return w.Dispose(false)
}

// sweepVirtual disposes the virtual workspaces that no pending change and no running agent needs.
func (m *Manager) sweepVirtual(keep, live map[string]bool) (removed int) {
	eng := m.engine(false)
	if eng == nil {
		return 0
	}
	for _, info := range eng.List() {
		if keep[info.AgentID] || live[info.AgentID] {
			continue
		}
		if err := m.vRelease(info.AgentID); err == nil {
			removed++
		}
	}
	return removed
}

// VirtualWorkspaces lists the virtual workspaces that exist, oldest first. It opens the engine only when Git already
// holds some, so a repository that never used the feature pays nothing for asking.
func (m *Manager) VirtualWorkspaces() []vws.VirtualWorkspace {
	eng := m.engine(false)
	if eng == nil {
		return nil
	}
	return eng.List()
}

// VirtualWorkspace returns one agent's workspace record.
func (m *Manager) VirtualWorkspace(agentID string) (vws.VirtualWorkspace, bool) {
	eng := m.engine(false)
	if eng == nil {
		return vws.VirtualWorkspace{}, false
	}
	w, ok := eng.Get(agentID)
	if !ok {
		return vws.VirtualWorkspace{}, false
	}
	return w.Info(), true
}

// VirtualMetrics reports the work the virtual engine has done since it opened, for comparison with worktrees.
func (m *Manager) VirtualMetrics() (vws.Metrics, bool) {
	m.vmu.Lock()
	eng := m.eng
	m.vmu.Unlock()
	if eng == nil {
		return vws.Metrics{}, false
	}
	return eng.Metrics(), true
}

// Close stops the helper processes of the virtual engine. Workspaces and their refs are untouched.
func (m *Manager) Close() error {
	m.vmu.Lock()
	eng := m.eng
	m.eng = nil
	m.vmu.Unlock()
	if eng != nil {
		return eng.Close()
	}
	return nil
}

// virtualBlob finds the Git blob of a file in the virtual workspace a change's worktree path stands for. virtual
// says the path belongs to one, even when the file is absent (oid is then empty).
func (m *Manager) virtualBlob(worktree, rel string, rev uint64) (oid string, virtual bool, err error) {
	w, ok := m.virtualOf(worktree)
	if !ok {
		return "", false, nil
	}
	var fi vws.FileInfo
	if snap, serr := m.pinned(w, rev); serr == nil && rev != 0 {
		fi, err = w.StatAt(snap, rel)
	} else {
		// The live overlay: what a tool wrote in the directory is not in it until it is imported.
		_ = m.Sync(w.ID())
		fi, err = w.Stat(rel)
	}
	if err != nil {
		if vws.IsNotExist(err) {
			return "", true, nil
		}
		return "", true, err
	}
	if fi.Kind == "dir" || fi.Kind == "submodule" {
		return "", true, nil
	}
	return fi.OID, true, nil
}

// pinned returns the snapshot a change was proposed at. It fails when the revision was never pinned or has been
// released, and callers then fall back to the live overlay only where that is safe.
func (m *Manager) pinned(w *vws.Workspace, rev uint64) (vws.Snapshot, error) {
	if rev == 0 {
		return vws.Snapshot{}, fmt.Errorf("no revision")
	}
	return w.SnapshotAt(rev)
}

// Available reports whether an agent's files can still be read: its virtual workspace exists, or its worktree
// directory does. It reads nothing and runs no Git command.
func (m *Manager) Available(agentID, worktree string) bool {
	if w, ok := m.virtualOf(worktree); ok && (agentID == "" || w.ID() == agentID) {
		return true
	}
	if worktree == "" {
		return false
	}
	st, err := os.Stat(worktree)
	return err == nil && st.IsDir()
}
