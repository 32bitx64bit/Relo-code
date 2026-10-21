package vws

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures an Engine. The zero value is a durable engine with sensible limits.
type Options struct {
	// NoFsync skips syncing objects before the ref that publishes them. Faster; a power loss can then lose the
	// newest revision. A process crash cannot, either way.
	NoFsync bool
	// MaxFileBytes bounds one file. Zero means 256 MiB.
	MaxFileBytes int64
	// AllowFilters lets the engine work in a repository whose content filters or line-ending rules would make a
	// view of stored blobs differ from a checkout. Only tests that compare both should set it.
	AllowFilters bool
	Now          func() time.Time
	// Observer is told about every published revision, after the workspace lock is released.
	Observer func(Event)
	// Allow is consulted before every mutation of a workspace. Returning an error refuses it.
	Allow func(agentID, op, path string) error
	// Ignore names paths a capture never imports (build output, Relo's own folders).
	Ignore func(rel string) bool
	// Materializer turns a revision into a directory for tools that need real files. Defaults to a Git worktree.
	Materializer Materializer
	// ExecRoot is the folder execution directories are made in. Defaults to <repo>/.relo/worktrees.
	ExecRoot string
}

// Engine owns the virtual workspaces of one repository.
type Engine struct {
	repo *repo
	opts Options

	mu     sync.Mutex
	spaces map[string]*Workspace

	compatMu sync.RWMutex
	compat   Compat
	attrMu   sync.Mutex
	attrSeen map[string][]string // base tree -> reasons its attributes rewrite content

	// fault, when set by a test, runs before every ref update and may fail it, to prove that a failure between
	// writing objects and publishing them leaves the previous revision intact.
	fault func(ref string) error

	revisions atomic.Int64
	creates   atomic.Int64
	captures  atomic.Int64
	mats      atomic.Int64
}

// Open prepares an engine for the repository at root. It does not create or change anything.
func Open(root string, opts Options) (*Engine, error) {
	r, err := openRepo(root, !opts.NoFsync)
	if err != nil {
		return nil, err
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 256 << 20
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	e := &Engine{repo: r, opts: opts, spaces: map[string]*Workspace{}, attrSeen: map[string][]string{}}
	e.compat = checkCompat(r)
	if opts.Materializer == nil {
		e.opts.Materializer = &WorktreeMaterializer{}
	}
	return e, nil
}

// Close stops the helper processes. Workspaces and their refs are untouched.
func (e *Engine) Close() error {
	e.repo.close()
	return nil
}

// Root is the repository directory the engine was opened on.
func (e *Engine) Root() string { return e.repo.root }

// Compat reports whether this repository can be viewed without a checkout, and if not, why.
func (e *Engine) Compat() Compat {
	e.compatMu.RLock()
	defer e.compatMu.RUnlock()
	return e.compat
}

// RefreshCompat reads the repository's settings again, so a user who turns on core.autocrlf or adds a filter while
// Relo is running moves new workspaces back to worktrees. Workspaces that already exist are not affected.
func (e *Engine) RefreshCompat() Compat {
	c := checkCompat(e.repo)
	e.compatMu.Lock()
	e.compat = c
	e.compatMu.Unlock()
	return c
}

// Metrics returns the work the engine has done since it opened.
func (e *Engine) Metrics() Metrics {
	return Metrics{
		Creates: e.creates.Load(), Revisions: e.revisions.Load(), Captures: e.captures.Load(), Materialized: e.mats.Load(),
		ObjectsWritten: e.repo.objectCount.Load(), BytesWritten: e.repo.byteCount.Load(), GitSpawns: e.repo.spawnCount.Load(),
	}
}

// CreateOptions tunes one workspace.
type CreateOptions struct {
	// Branch defaults to relo/<agent>.
	Branch   string
	ReadOnly bool
	Meta     map[string]string
}

// Workspace is one agent's logical workspace. All methods are safe for concurrent use.
type Workspace struct {
	e  *Engine
	id string

	mu            sync.Mutex // serialises mutations and state changes
	execMu        sync.Mutex // serialises materialize, capture and release, which scan directories and must not hold mu
	vw            VirtualWorkspace
	overlayCommit string // the checkpoint commit refs/relo/vws/<id>/overlay points at
	exec          *Exec  // the open materialization, if any
}

// CreateVirtualWorkspace creates a workspace for agentID whose base is baseCommit. No directory is created and the
// user's checkout is not touched.
func (e *Engine) CreateVirtualWorkspace(agentID, baseCommit string) (*Workspace, error) {
	return e.Create(agentID, baseCommit, CreateOptions{})
}

// Create is CreateVirtualWorkspace with options.
func (e *Engine) Create(agentID, baseRev string, opt CreateOptions) (*Workspace, error) {
	if !validAgentID(agentID) {
		return nil, fmt.Errorf("vws: invalid agent id %q", agentID)
	}
	if c := e.Compat(); !e.opts.AllowFilters && !c.OK {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedRepo, strings.Join(c.Reasons, "; "))
	}
	e.mu.Lock()
	if _, ok := e.spaces[agentID]; ok {
		e.mu.Unlock()
		return nil, pathErr(agentID, ErrExists)
	}
	e.mu.Unlock()
	base, err := e.resolveCommit(baseRev)
	if err != nil {
		return nil, err
	}
	ci, err := e.repo.readCommit(base)
	if err != nil {
		return nil, err
	}
	if !e.opts.AllowFilters {
		if reasons := e.attributeRisk(ci.Tree); len(reasons) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedRepo, strings.Join(reasons, "; "))
		}
	}
	now := e.opts.Now().UTC()
	branch := opt.Branch
	if branch == "" {
		branch = "relo/" + agentID
	}
	w := &Workspace{e: e, id: agentID}
	w.vw = VirtualWorkspace{
		AgentID: agentID, Branch: branch, BaseCommit: base, BaseTree: ci.Tree, EffectiveTree: ci.Tree,
		OverlayID: fmt.Sprintf("%s-%d", agentID, now.UnixNano()), State: StateReady, ReadOnly: opt.ReadOnly,
		Meta: copyMeta(opt.Meta), Created: now, Updated: now,
	}
	commit, err := e.repo.writeCheckpoint(w.vw, now)
	if err != nil {
		return nil, err
	}
	zero := e.repo.zero()
	if err := e.repo.updateRefs(refOp{ref: baseRef(agentID), new: base, old: zero}, refOp{ref: overlayRef(agentID), new: commit, old: zero}); err != nil {
		if errors.Is(err, ErrRefConflict) {
			return nil, pathErr(agentID, ErrExists)
		}
		return nil, err
	}
	w.overlayCommit = commit
	e.mu.Lock()
	if _, ok := e.spaces[agentID]; ok {
		e.mu.Unlock()
		_ = e.repo.updateRefs(refOp{ref: baseRef(agentID), old: base}, refOp{ref: overlayRef(agentID), old: commit})
		return nil, pathErr(agentID, ErrExists)
	}
	e.spaces[agentID] = w
	e.mu.Unlock()
	e.creates.Add(1)
	e.emit(Event{AgentID: agentID, Kind: "create", Tree: ci.Tree, Commit: commit, At: now})
	return w, nil
}

// resolveCommit turns a revision name into a full commit id.
func (e *Engine) resolveCommit(rev string) (string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	out, err := e.repo.git("rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil || !e.repo.isOID(out) {
		return "", fmt.Errorf("vws: %q is not a commit in this repository", rev)
	}
	return out, nil
}

// Get returns the live workspace of an agent.
func (e *Engine) Get(agentID string) (*Workspace, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.spaces[agentID]
	return w, ok
}

func (e *Engine) mustGet(agentID string) (*Workspace, error) {
	if w, ok := e.Get(agentID); ok {
		return w, nil
	}
	return nil, fmt.Errorf("%w: virtual workspace %q", errNotFound, agentID)
}

var errNotFound = errors.New("no such workspace")

// List describes every live workspace, oldest first.
func (e *Engine) List() []VirtualWorkspace {
	e.mu.Lock()
	spaces := make([]*Workspace, 0, len(e.spaces))
	for _, w := range e.spaces {
		spaces = append(spaces, w)
	}
	e.mu.Unlock()
	out := make([]VirtualWorkspace, 0, len(spaces))
	for _, w := range spaces {
		out = append(out, w.Info())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return out[i].AgentID < out[j].AgentID
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out
}

func (e *Engine) emit(ev Event) {
	if e.opts.Observer != nil {
		e.opts.Observer(ev)
	}
}

func copyMeta(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ID is the agent id the workspace belongs to.
func (w *Workspace) ID() string { return w.id }

// Info returns a copy of the workspace descriptor.
func (w *Workspace) Info() VirtualWorkspace {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.infoLocked()
}

func (w *Workspace) infoLocked() VirtualWorkspace {
	v := w.vw
	v.Meta = copyMeta(v.Meta)
	return v
}

// committedTreeLocked is layer two: the agent's last commit, or the base when it has not committed.
func (w *Workspace) committedTreeLocked() string {
	if w.vw.HeadTree != "" {
		return w.vw.HeadTree
	}
	return w.vw.BaseTree
}

func (w *Workspace) live() error {
	if w.vw.State == StateDisposed {
		return ErrDisposed
	}
	return nil
}

// checkpoint is the metadata a checkpoint commit carries.
const checkpointMagic = "relo-vws checkpoint v1"

// writeCheckpoint stores the commit that publishes a workspace state: its tree is the overlay, its parent the
// agent's latest commit (or the base), and its message the descriptor, so content and metadata move together.
func (r *repo) writeCheckpoint(vw VirtualWorkspace, at time.Time) (string, error) {
	meta, err := json.Marshal(vw)
	if err != nil {
		return "", err
	}
	parent := vw.HeadCommit
	if parent == "" {
		parent = vw.BaseCommit
	}
	subject := fmt.Sprintf("%s %s r%d", checkpointMagic, vw.AgentID, vw.Revision)
	return r.writeCommit(vw.EffectiveTree, []string{parent}, subject, string(meta), at.Unix())
}

// readCheckpoint recovers a descriptor from a checkpoint commit.
func (r *repo) readCheckpoint(commit string) (VirtualWorkspace, error) {
	ci, err := r.readCommit(commit)
	if err != nil {
		return VirtualWorkspace{}, err
	}
	subject, body, _ := strings.Cut(ci.Message, "\n\n")
	if !strings.HasPrefix(subject, checkpointMagic) {
		return VirtualWorkspace{}, fmt.Errorf("vws: commit %s is not a workspace checkpoint", commit)
	}
	var vw VirtualWorkspace
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &vw); err != nil {
		return VirtualWorkspace{}, fmt.Errorf("vws: checkpoint %s: %w", commit, err)
	}
	if vw.EffectiveTree != ci.Tree {
		return VirtualWorkspace{}, fmt.Errorf("vws: checkpoint %s describes tree %s but holds %s", commit, vw.EffectiveTree, ci.Tree)
	}
	return vw, nil
}

// publishLocked makes next the workspace state: one checkpoint commit, one atomic ref update. A failure leaves the
// previous state in place. The caller holds w.mu and passes the descriptor it wants published.
func (w *Workspace) publishLocked(next VirtualWorkspace, extra ...refOp) error {
	now := w.e.opts.Now().UTC()
	next.Updated = now
	next.Dirty = next.EffectiveTree != treeOr(next.HeadTree, next.BaseTree)
	commit, err := w.e.repo.writeCheckpoint(next, now)
	if err != nil {
		return err
	}
	ops := append([]refOp{{ref: overlayRef(w.id), new: commit, old: w.overlayCommit}}, extra...)
	if w.e.fault != nil {
		if err := w.e.fault(overlayRef(w.id)); err != nil {
			return err
		}
	}
	if err := w.e.repo.updateRefs(ops...); err != nil {
		return err
	}
	w.vw, w.overlayCommit = next, commit
	return nil
}

func treeOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// SetMeta stores caller-owned facts (dependency lineage, for instance) with the workspace. They survive restarts.
func (w *Workspace) SetMeta(key, value string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return err
	}
	next := w.vw
	next.Meta = copyMeta(w.vw.Meta)
	if next.Meta == nil {
		next.Meta = map[string]string{}
	}
	if value == "" {
		delete(next.Meta, key)
	} else {
		next.Meta[key] = value
	}
	return w.publishLocked(next)
}

// Meta returns one stored fact.
func (w *Workspace) Meta(key string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.vw.Meta[key]
}

// Dispose removes the workspace and every ref that kept its objects alive, including its branch unless keepBranch
// is set. The objects themselves are left for Git to collect once nothing else refers to them. A workspace whose
// execution directory is still open is refused: release it first.
func (w *Workspace) Dispose(keepBranch bool) error {
	w.mu.Lock()
	if w.vw.State == StateDisposed {
		w.mu.Unlock()
		return nil
	}
	if w.exec != nil {
		w.mu.Unlock()
		return ErrMaterialized
	}
	id := w.id
	ops := []refOp{{ref: overlayRef(id), old: w.overlayCommit}, {ref: baseRef(id)}}
	refs, _ := w.e.repo.listRefs(refPrefix + id + "/")
	for name := range refs {
		if name != overlayRef(id) && name != baseRef(id) {
			ops = append(ops, refOp{ref: name})
		}
	}
	if !keepBranch && w.vw.HeadCommit != "" {
		ops = append(ops, refOp{ref: branchRef(w.vw.Branch), old: w.vw.HeadCommit})
	}
	// Each delete is its own transaction apart from the overlay: a ref that is already gone must not keep the
	// others alive.
	err := w.e.repo.updateRefs(ops[:1]...)
	if err != nil && !errors.Is(err, ErrRefConflict) {
		w.mu.Unlock()
		return err
	}
	for _, op := range ops[1:] {
		_ = w.e.repo.updateRefs(op)
	}
	w.vw.State = StateDisposed
	rev, tree := w.vw.Revision, w.vw.EffectiveTree
	w.mu.Unlock()
	w.e.mu.Lock()
	if w.e.spaces[id] == w {
		delete(w.e.spaces, id)
	}
	w.e.mu.Unlock()
	w.e.emit(Event{AgentID: id, Revision: rev, Kind: "dispose", Tree: tree, At: time.Now().UTC()})
	return nil
}

// Dispose removes an agent's workspace.
func (e *Engine) Dispose(agentID string) error {
	w, ok := e.Get(agentID)
	if !ok {
		return nil
	}
	return w.Dispose(false)
}
