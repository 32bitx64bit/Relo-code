package workspace

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Snapshot struct {
	Root      string   `json:"root"`
	Branch    string   `json:"branch"`
	Head      string   `json:"head"`
	Dirty     []string `json:"dirty"`
	Untracked []string `json:"untracked"`
	Git       bool     `json:"git"`
	// Commit is what agents are cut from: HEAD when the tree is clean, otherwise a commit holding the working tree
	// exactly as the user has it, uncommitted edits and new files included.
	Commit string `json:"commit"`
	// HeadFallback reports that the snapshot could not be recorded and the agent would be cut from HEAD instead.
	HeadFallback bool `json:"headFallback,omitempty"`
	// UntrackedTruncated reports that more new files existed than maxSnapshotUntracked, so they were not recorded.
	UntrackedTruncated bool `json:"untrackedTruncated,omitempty"`
	// ElapsedMS is how long taking the snapshot took, so its cost can be measured.
	ElapsedMS int64 `json:"elapsedMs,omitempty"`
}

type Tree struct {
	AgentID string `json:"agentId"`
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	// Direct marks a mutating agent running in the workspace itself because the
	// workspace is not a Git repository. Changes are applied in place with no undo.
	Direct bool `json:"direct,omitempty"`
}

type Manager struct {
	mu    sync.Mutex
	root  string
	trees map[string]Tree
	base  Snapshot

	snapTree, snapCommit string                        // the last working tree recorded as a commit, so an unchanged tree keeps one base
	snapshotCommitFn     func(Snapshot) (string, bool) // test seam: forces the HEAD fallback path
	snapshotValidateFn   func(Snapshot) bool           // test seam: forces a failed snapshot validation
	journalMu            sync.Mutex                    // serialises journal entries and undo
}

func New(root string) *Manager {
	return &Manager{root: root, trees: map[string]Tree{}}
}

func (m *Manager) Snapshot() (Snapshot, error) {
	start := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Root: m.root}
	if _, err := os.Stat(filepath.Join(m.root, ".git")); err != nil {
		return s, nil
	}
	s.Git = true
	// Relo's own folder must never read as the user's untracked work. The private exclude file leaves their
	// .gitignore alone.
	ensureExcluded(m.root, ".relo")
	s.Branch, _ = git(m.root, "rev-parse", "--abbrev-ref", "HEAD")
	s.Head, _ = git(m.root, "rev-parse", "HEAD")
	status, _ := gitRaw(m.root, "status", "--porcelain", "-z", "--untracked-files=all")
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		path := entry[3:]
		if strings.HasPrefix(entry, "??") {
			s.Untracked = append(s.Untracked, path)
			continue
		}
		s.Dirty = append(s.Dirty, path)
		if strings.Contains(entry[:2], "R") || strings.Contains(entry[:2], "C") {
			i++
			if i < len(entries) && entries[i] != "" {
				s.Dirty = append(s.Dirty, entries[i])
			}
		}
	}
	s.UntrackedTruncated = len(s.Untracked) > maxSnapshotUntracked
	var lastErr error
	for attempt := 0; attempt < snapshotAttempts; attempt++ {
		if m.snapshotCommitFn != nil {
			s.Commit, s.HeadFallback = m.snapshotCommitFn(s)
		} else {
			s.Commit, s.HeadFallback = m.snapshotCommit(s)
		}
		if !m.snapshotConsistent(s) {
			lastErr = fmt.Errorf("snapshot tree kept changing while it was recorded (%d attempts)", snapshotAttempts)
			m.snapTree, m.snapCommit = "", ""
			continue
		}
		s.ElapsedMS = time.Since(start).Milliseconds()
		m.base = s
		return s, nil
	}
	// Fail closed: do not hand the agent a base that could not be verified. The caller must not spawn on it.
	s.Commit = ""
	s.ElapsedMS = time.Since(start).Milliseconds()
	return s, lastErr
}

// snapshotAttempts is how often a snapshot is re-recorded when the tree it wrote is not the tree it re-reads.
const snapshotAttempts = 3

// snapshotConsistent re-hashes the snapshot tree. A clean tree is HEAD, and an explicit HEAD fallback is already
// flagged, so neither needs a tree comparison. A mismatch is retried by Snapshot and then fails closed.
func (m *Manager) snapshotConsistent(s Snapshot) bool {
	if s.Commit == "" {
		return false
	}
	if s.HeadFallback || s.Commit == s.Head {
		return true
	}
	if m.snapshotValidateFn != nil {
		return m.snapshotValidateFn(s)
	}
	tree, err := m.snapshotTree(s)
	if err != nil {
		return false
	}
	return tree == m.snapTree
}

// maxSnapshotUntracked bounds how many new files a snapshot will read. Past it (a build folder that is not
// ignored) only tracked files are recorded, so taking the snapshot stays quick.
const maxSnapshotUntracked = 2000

// snapshotCommit records the user's working tree as a commit on top of HEAD, using a private copy of the index, so
// the user's branch, index and files are never touched. Agents are cut from it: a worktree made from HEAD alone
// would not contain the user's uncommitted work, and every agent would report on a repository the user is not
// looking at. Returns HEAD when nothing is uncommitted, and also when the snapshot cannot be made.
func (m *Manager) snapshotCommit(s Snapshot) (string, bool) {
	if len(s.Dirty) == 0 && len(s.Untracked) == 0 {
		return s.Head, false
	}
	if s.Head == "" {
		return "", false
	}
	_, env, cleanup, err := m.snapshotIndex(s)
	if err != nil {
		return s.Head, true
	}
	defer cleanup()
	tree, err := gitEnv(m.root, env, "write-tree")
	if err != nil || tree == "" {
		return s.Head, true
	}
	if tree == m.snapTree && m.snapCommit != "" {
		if _, err := git(m.root, "cat-file", "-e", m.snapCommit); err == nil {
			return m.snapCommit, false
		}
	}
	commit, err := gitEnv(m.root, env, "commit-tree", tree, "-p", s.Head, "-m", "relo: working tree snapshot")
	if err != nil || commit == "" {
		return s.Head, true
	}
	m.snapTree, m.snapCommit = tree, commit
	return commit, false
}

// snapshotTree re-reads the working tree into a fresh private index and returns its tree id. It is the re-hash
// half of snapshot validation; it never moves a ref or touches the user's index.
func (m *Manager) snapshotTree(s Snapshot) (string, error) {
	_, env, cleanup, err := m.snapshotIndex(s)
	if err != nil {
		return "", err
	}
	defer cleanup()
	return gitEnv(m.root, env, "write-tree")
}

// snapshotIndex builds a private index holding the snapshot's working tree. Starting from the user's own index
// keeps Git from re-reading every file; the copy is only written into a temporary file.
func (m *Manager) snapshotIndex(s Snapshot) (index string, env []string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "relo-index-*")
	if err != nil {
		return "", nil, func() {}, err
	}
	index = tmp.Name()
	_ = tmp.Close()
	cleanup = func() { _ = os.Remove(index) }
	if path, perr := git(m.root, "rev-parse", "--git-path", "index"); perr == nil {
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.root, path)
		}
		if data, rerr := os.ReadFile(path); rerr == nil {
			_ = os.WriteFile(index, data, 0o600)
		}
	}
	env = append(os.Environ(), "GIT_INDEX_FILE="+index,
		"GIT_AUTHOR_NAME=Relo", "GIT_AUTHOR_EMAIL=relo@localhost", "GIT_COMMITTER_NAME=Relo", "GIT_COMMITTER_EMAIL=relo@localhost")
	add := "-A"
	if len(s.Untracked) > maxSnapshotUntracked {
		add = "-u"
	}
	if _, err := gitEnv(m.root, env, "add", add); err != nil {
		cleanup()
		return "", nil, func() {}, err
	}
	return index, env, cleanup, nil
}

func (m *Manager) Base() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.base
}

func (m *Manager) EnsureTree(agentID string, readOnly bool) (Tree, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agentID == "" || strings.Contains(agentID, "..") || strings.HasSuffix(agentID, ".") || strings.HasSuffix(strings.ToLower(agentID), ".lock") {
		return Tree{}, fmt.Errorf("invalid agent id %q", agentID)
	}
	for _, r := range agentID {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return Tree{}, fmt.Errorf("invalid agent id %q", agentID)
		}
	}
	if t, ok := m.trees[agentID]; ok {
		return t, nil
	}
	if readOnly {
		t := Tree{AgentID: agentID, Path: m.root, Base: m.baseCommit()}
		m.trees[agentID] = t
		return t, nil
	}
	if !isGit(m.root) {
		// Direct mode: no worktree, no isolation. Caller must serialize mutating agents.
		t := Tree{AgentID: agentID, Path: m.root, Direct: true}
		m.trees[agentID] = t
		return t, nil
	}
	dir := filepath.Join(m.root, ".relo", "worktrees", agentID)
	if err := ensureWithin(m.root, dir); err != nil {
		return Tree{}, fmt.Errorf("unsafe worktree directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Tree{}, err
	}
	branch := "relo/" + agentID
	base := m.baseCommit()
	if base == "" {
		var err error
		base, err = git(m.root, "rev-parse", "HEAD")
		if err != nil {
			return Tree{}, err
		}
	}
	if _, err := os.Stat(dir); err == nil {
		top, gitErr := git(dir, "rev-parse", "--show-toplevel")
		resolvedDir, pathErr := filepath.EvalSymlinks(dir)
		resolvedTop, topErr := filepath.EvalSymlinks(top)
		if gitErr != nil || pathErr != nil || topErr != nil || resolvedDir != resolvedTop {
			return Tree{}, fmt.Errorf("existing worktree path is not an isolated Git worktree: %s", dir)
		}
		// An adopted worktree keeps the commit it was cut from, not whatever HEAD is now, so its diff stays exact
		// after the user commits or the core restarts.
		if rec := m.recordedBase(agentID); rec != "" {
			base = rec
		} else if head, herr := git(dir, "rev-parse", "HEAD"); herr == nil && head != "" {
			base = head
		}
		linkDeps(m.root, dir)
		t := Tree{AgentID: agentID, Path: dir, Branch: branch, Base: base}
		m.trees[agentID] = t
		return t, nil
	}
	if _, err := git(m.root, "worktree", "add", "-b", branch, dir, base); err != nil {
		if _, err2 := git(m.root, "worktree", "add", dir, base); err2 != nil {
			return Tree{}, fmt.Errorf("worktree: %v / %v", err, err2)
		}
	}
	m.recordBase(agentID, base)
	linkDeps(m.root, dir)
	t := Tree{AgentID: agentID, Path: dir, Branch: branch, Base: base}
	m.trees[agentID] = t
	return t, nil
}

// StackResult says whether one patch applied to the agent's worktree, and why not when it did not.
type StackResult struct {
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// Stack applies other agents' changes, which the user has not accepted yet, to this agent's worktree and makes the
// result the agent's base. The agent then sees the code it is meant to test, review or build on, and its own diff
// holds only what it adds. A patch that no longer applies is skipped; the result says which ones applied.
func (m *Manager) Stack(agentID string, patches []string) ([]StackResult, error) {
	m.mu.Lock()
	t, ok := m.trees[agentID]
	m.mu.Unlock()
	if !ok || t.Path == m.root {
		return nil, fmt.Errorf("no isolated worktree for agent %q", agentID)
	}
	results := make([]StackResult, len(patches))
	var failed []string
	applied := 0
	for i, patch := range patches {
		if strings.TrimSpace(patch) == "" {
			results[i] = StackResult{Applied: true}
			continue
		}
		if _, perr := gitStdin(t.Path, patch, "apply", "--whitespace=nowarn"); perr != nil {
			failed = append(failed, perr.Error())
			results[i] = StackResult{Error: perr.Error()}
			continue
		}
		applied++
		results[i] = StackResult{Applied: true}
	}
	if applied > 0 {
		env := append(os.Environ(), "GIT_AUTHOR_NAME=Relo", "GIT_AUTHOR_EMAIL=relo@localhost", "GIT_COMMITTER_NAME=Relo", "GIT_COMMITTER_EMAIL=relo@localhost")
		if _, aerr := git(t.Path, "add", "-A"); aerr != nil {
			return results, aerr
		}
		tree, werr := gitEnv(t.Path, env, "write-tree")
		if werr != nil {
			return results, werr
		}
		// Plumbing instead of `git commit`, so no hook or signing setting of the user's can get in the way.
		commit, cerr := gitEnv(t.Path, env, "commit-tree", tree, "-p", t.Base, "-m", "relo: changes this agent builds on")
		if cerr != nil {
			return results, cerr
		}
		if _, uerr := git(t.Path, "update-ref", "HEAD", commit); uerr != nil {
			return results, uerr
		}
		m.mu.Lock()
		t.Base = commit
		m.trees[agentID] = t
		m.recordBase(agentID, commit)
		m.mu.Unlock()
	}
	if len(failed) > 0 {
		return results, fmt.Errorf("%d of %d patches did not apply: %s", len(failed), len(patches), failed[0])
	}
	return results, nil
}

// Recut resets an unstarted agent's worktree to base, discarding whatever was in it. The caller re-stacks the
// agent's recorded lineage afterwards; Recut itself never touches the user's workspace.
func (m *Manager) Recut(agentID, base string) (Tree, error) {
	if strings.TrimSpace(base) == "" {
		return Tree{}, fmt.Errorf("no base to re-cut agent %q onto", agentID)
	}
	m.mu.Lock()
	t, ok := m.trees[agentID]
	m.mu.Unlock()
	if !ok || t.Path == m.root || t.Direct {
		return Tree{}, fmt.Errorf("no isolated worktree for agent %q", agentID)
	}
	if _, err := git(t.Path, "rev-parse", "--show-toplevel"); err != nil {
		return Tree{}, fmt.Errorf("re-cut %q: %w", agentID, err)
	}
	if _, err := git(t.Path, "reset", "--hard", base); err != nil {
		return Tree{}, fmt.Errorf("re-cut %q: %w", agentID, err)
	}
	m.mu.Lock()
	t.Base = base
	m.trees[agentID] = t
	m.recordBase(agentID, base)
	m.mu.Unlock()
	return t, nil
}

// Direct reports whether the workspace root is not a Git repository, so mutating
// agents run in place instead of in isolated worktrees.
func (m *Manager) Direct() bool { return !isGit(m.root) }

func (m *Manager) Tree(agentID string) (Tree, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.trees[agentID]
	return t, ok
}

func (m *Manager) Changed(agentID string) ([]string, error) {
	t, ok := m.Tree(agentID)
	if !ok {
		return nil, nil
	}
	if t.Path == m.root {
		out := append(append([]string(nil), m.Base().Dirty...), m.Base().Untracked...)
		return out, nil
	}
	if _, err := git(t.Path, "add", "-A"); err != nil {
		return nil, err
	}
	out, err := gitRaw(t.Path, "diff", "--cached", "--name-only", "-z", t.Base)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(out, "\x00") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

func (m *Manager) Patch(agentID string) (string, error) {
	t, ok := m.Tree(agentID)
	if !ok {
		return "", fmt.Errorf("no worktree")
	}
	if t.Path == m.root {
		return "", nil
	}
	if _, err := git(t.Path, "add", "-A"); err != nil {
		return "", err
	}
	return gitRaw(t.Path, "diff", "--cached", "--binary", t.Base)
}

func (m *Manager) PatchPath(agentID, rel string) (string, error) {
	t, ok := m.Tree(agentID)
	if !ok {
		return "", fmt.Errorf("no worktree")
	}
	if t.Path == m.root {
		return "", fmt.Errorf("cannot patch a shared workspace")
	}
	if err := validateRelative(rel); err != nil {
		return "", err
	}
	if _, err := git(t.Path, "add", "-A"); err != nil {
		return "", err
	}
	return gitRaw(t.Path, "diff", "--cached", "--binary", t.Base, "--", ":(literal)"+filepath.ToSlash(rel))
}

func (m *Manager) Discard(agentID, rel string) error {
	t, ok := m.Tree(agentID)
	if !ok || t.Path == m.root {
		return fmt.Errorf("no isolated worktree for agent %q", agentID)
	}
	if err := validateRelative(rel); err != nil {
		return err
	}
	if _, err := git(t.Path, "cat-file", "-e", t.Base+":"+filepath.ToSlash(rel)); err == nil {
		_, err = git(t.Path, "restore", "--source="+t.Base, "--staged", "--worktree", "--", ":(literal)"+filepath.ToSlash(rel))
		return err
	}
	_, _ = git(t.Path, "rm", "--cached", "--ignore-unmatch", "--", ":(literal)"+filepath.ToSlash(rel))
	path := filepath.Join(t.Path, filepath.FromSlash(rel))
	if err := ensureWithin(t.Path, path); err != nil {
		return err
	}
	return os.Remove(path)
}

func (m *Manager) Remove(agentID string) error {
	m.mu.Lock()
	t, ok := m.trees[agentID]
	if ok {
		delete(m.trees, agentID)
	}
	m.mu.Unlock()
	if !ok || t.Path == m.root {
		return nil
	}
	_, err := git(m.root, "worktree", "remove", "--force", t.Path)
	return err
}

func (m *Manager) Prune() error {
	if !isGit(m.root) {
		return nil
	}
	_, err := git(m.root, "worktree", "prune")
	return err
}

// FileChange is one file an agent changed in its worktree, taken from git, not from what the agent says it did.
type FileChange struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"` // added, modified, deleted
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// ChangedFiles lists what an isolated worktree differs from its base by, with line counts. Empty for a shared
// workspace, where edits cannot be told apart from the user's own.
func (m *Manager) ChangedFiles(agentID string) ([]FileChange, error) {
	t, ok := m.Tree(agentID)
	if !ok || t.Path == m.root {
		return nil, nil
	}
	if _, err := git(t.Path, "add", "-A"); err != nil {
		return nil, err
	}
	names, err := gitRaw(t.Path, "diff", "--cached", "--no-renames", "--name-status", "-z", t.Base)
	if err != nil {
		return nil, err
	}
	counts := map[string][2]int{}
	if nums, nerr := gitRaw(t.Path, "diff", "--cached", "--no-renames", "--numstat", "-z", t.Base); nerr == nil {
		for _, rec := range strings.Split(nums, "\x00") {
			f := strings.SplitN(rec, "\t", 3)
			if len(f) == 3 {
				a, _ := strconv.Atoi(f[0]) // "-" for binary files parses to 0
				d, _ := strconv.Atoi(f[1])
				counts[f[2]] = [2]int{a, d}
			}
		}
	}
	var out []FileChange
	parts := strings.Split(names, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		status, path := parts[i], parts[i+1]
		if status == "" || path == "" {
			continue
		}
		kind := "modified"
		switch status[0] {
		case 'A':
			kind = "added"
		case 'D':
			kind = "deleted"
		}
		c := counts[path]
		out = append(out, FileChange{Path: path, Kind: kind, Added: c[0], Deleted: c[1]})
	}
	return out, nil
}

func (m *Manager) baseFile(agentID string) string {
	return filepath.Join(m.root, ".relo", "base", agentID)
}

func (m *Manager) recordBase(agentID, base string) {
	if base == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.baseFile(agentID)), 0o755); err == nil {
		_ = os.WriteFile(m.baseFile(agentID), []byte(base+"\n"), 0o644)
	}
}

func (m *Manager) recordedBase(agentID string) string {
	b, err := os.ReadFile(m.baseFile(agentID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// BaseContent returns the file as it was at the agent's base commit; ok is false when it did not exist there.
func (m *Manager) BaseContent(agentID, rel string) (content []byte, ok bool, err error) {
	t, found := m.Tree(agentID)
	if !found {
		return nil, false, fmt.Errorf("no worktree")
	}
	if err := validateRelative(rel); err != nil {
		return nil, false, err
	}
	spec := t.Base + ":" + filepath.ToSlash(rel)
	if _, err := git(m.root, "cat-file", "-e", spec); err != nil {
		return nil, false, nil
	}
	out, err := gitRaw(m.root, "show", spec)
	if err != nil {
		return nil, false, err
	}
	return []byte(out), true, nil
}

// Baseline returns a pristine checkout of the commit agents branch from, for telling a failure the agent caused from
// one the repository already had. It is created on first use and reused until the base commit changes.
func (m *Manager) Baseline() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isGit(m.root) {
		return "", fmt.Errorf("not a git repository")
	}
	head := m.baseCommit()
	if head == "" {
		var err error
		if head, err = git(m.root, "rev-parse", "HEAD"); err != nil {
			return "", err
		}
	}
	short := head
	if len(short) > 12 {
		short = short[:12]
	}
	dir := filepath.Join(m.root, ".relo", "worktrees", "_baseline-"+short)
	if err := ensureWithin(m.root, dir); err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if _, err := git(m.root, "worktree", "add", "--detach", dir, head); err != nil {
		return "", err
	}
	linkDeps(m.root, dir)
	return dir, nil
}

// Sweep removes worktrees, branches and base records that no pending change refers to. keep holds agent ids whose
// worktree still backs a change waiting for accept or reject. Safe to call at any time; failures are best effort.
func (m *Manager) Sweep(keep map[string]bool) (removed int) {
	if !isGit(m.root) {
		return 0
	}
	dir := filepath.Join(m.root, ".relo", "worktrees")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	m.mu.Lock()
	live := map[string]bool{}
	for id := range m.trees {
		live[id] = true
	}
	m.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || keep[name] || live[name] {
			continue
		}
		if strings.HasPrefix(name, "_baseline-") {
			m.removeDir(filepath.Join(dir, name))
			removed++
			continue
		}
		m.removeDir(filepath.Join(dir, name))
		_, _ = git(m.root, "branch", "-D", "relo/"+name)
		_ = os.Remove(m.baseFile(name))
		removed++
	}
	_, _ = git(m.root, "worktree", "prune")
	m.pruneJournal()
	m.pruneChangeRefs()
	// Branches left behind by worktrees that are already gone.
	if out, err := git(m.root, "for-each-ref", "--format=%(refname:short)", "refs/heads/relo/"); err == nil {
		for _, ref := range strings.Fields(out) {
			id := strings.TrimPrefix(ref, "relo/")
			if keep[id] || live[id] {
				continue
			}
			if _, statErr := os.Stat(filepath.Join(dir, id)); statErr != nil {
				_, _ = git(m.root, "branch", "-D", ref)
			}
		}
	}
	return removed
}

func (m *Manager) removeDir(path string) {
	if _, err := git(m.root, "worktree", "remove", "--force", path); err != nil {
		_ = os.RemoveAll(path)
	}
}

// Release drops an agent's worktree and branch once nothing refers to it any more.
func (m *Manager) Release(agentID string) error {
	m.mu.Lock()
	t, ok := m.trees[agentID]
	delete(m.trees, agentID)
	m.mu.Unlock()
	if ok && t.Path == m.root {
		return nil
	}
	dir := filepath.Join(m.root, ".relo", "worktrees", agentID)
	if _, err := os.Stat(dir); err == nil {
		m.removeDir(dir)
	}
	_, _ = git(m.root, "branch", "-D", "relo/"+agentID)
	_ = os.Remove(m.baseFile(agentID))
	return nil
}

func validateRelative(rel string) error {
	canonical := strings.ReplaceAll(rel, `\`, "/")
	if strings.ContainsAny(rel, "\x00\n\r") || filepath.IsAbs(rel) || strings.HasPrefix(canonical, "/") || strings.HasPrefix(canonical, "//") || (len(canonical) > 1 && canonical[1] == ':') {
		return fmt.Errorf("invalid worktree path %q", rel)
	}
	for _, part := range strings.Split(canonical, "/") {
		if part == ".." {
			return fmt.Errorf("invalid worktree path %q", rel)
		}
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || filepath.IsAbs(rel) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid worktree path %q", rel)
	}
	return nil
}

func cleanPath(path string) (string, error) {
	if err := validateRelative(path); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))), nil
}

func ensureWithin(root, target string) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	probe := target
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(probe)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			rel, relErr := filepath.Rel(root, resolved)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("path escapes worktree: %s", target)
			}
			return nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return resolveErr
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

// linkDeps symlinks the root's installed node_modules into a fresh worktree. They are gitignored so a
// worktree never has them, and verification (`npm test` -> tsx) dies with "command not found" without them.
// The ignore rule `node_modules/` only matches directories, so a symlink would be staged by `git add -A`;
// a slashless rule goes in the shared info/exclude first. Best effort: verification reports any real failure.
// ponytail: node_modules only; add other ignored dep dirs (.venv, vendor) when a project needs them.
func linkDeps(root, dir string) {
	if !ensureExcluded(root, "node_modules") {
		return
	}
	out, err := gitRaw(root, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return
	}
	for _, p := range strings.Split(out, "\x00") {
		p = strings.TrimSuffix(p, "/")
		if filepath.Base(p) != "node_modules" {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if os.MkdirAll(filepath.Dir(dst), 0o755) == nil {
			_ = os.Symlink(filepath.Join(root, filepath.FromSlash(p)), dst)
		}
	}
}

// ensureExcluded appends line to the repository's private ignore file (.git/info/exclude) when it is not there yet,
// so Relo's own files never show up as untracked changes in the user's repository. Reports whether the line is in place.
func ensureExcluded(root, line string) bool {
	ex, err := git(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return false
	}
	if !filepath.IsAbs(ex) {
		ex = filepath.Join(root, ex)
	}
	data, _ := os.ReadFile(ex)
	if strings.Contains("\n"+string(data), "\n"+line+"\n") {
		return true
	}
	if err := os.MkdirAll(filepath.Dir(ex), 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(ex, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	_, werr := f.WriteString(prefix + line + "\n")
	return f.Close() == nil && werr == nil
}

// baseCommit is the commit new agents are cut from. Callers hold m.mu.
func (m *Manager) baseCommit() string {
	if m.base.Commit != "" {
		return m.base.Commit
	}
	return m.base.Head
}

func isGit(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitEnv runs git with extra environment variables (a private index, a committer identity).
func gitEnv(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitStdin runs git with input on its standard input (a patch to apply).
func gitStdin(dir, input string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}
