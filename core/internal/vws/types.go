package vws

import "time"

// WorkspaceState is where a workspace is in its life.
type WorkspaceState string

const (
	// StateReady is a purely logical workspace: no directory exists for it.
	StateReady WorkspaceState = "ready"
	// StateMaterialized means a directory holds the workspace for tools that need real files.
	StateMaterialized WorkspaceState = "materialized"
	// StateInterrupted is a materialization that was still open when Relo stopped. Its directory may hold work that
	// has not been imported yet; Recover imports it.
	StateInterrupted WorkspaceState = "interrupted"
	// StateDisposed is a workspace that has been removed.
	StateDisposed WorkspaceState = "disposed"
)

// VirtualWorkspace is the persistent identity of one agent's logical workspace. It survives context refreshes and
// restarts: a context refresh changes what the agent knows, never what repository state it holds.
type VirtualWorkspace struct {
	AgentID string `json:"agentId"`
	// Branch is the agent's branch name; refs/heads/<Branch> holds HeadCommit once the agent has committed.
	Branch string `json:"branch"`
	// BaseCommit is the immutable snapshot the agent was cut from. It is pinned for the life of the workspace.
	BaseCommit string `json:"baseCommit"`
	// HeadCommit is the agent's latest commit on its branch, or empty before the first Commit.
	HeadCommit string `json:"headCommit,omitempty"`
	// OverlayID names this overlay generation. A workspace created again for the same agent gets a new one.
	OverlayID string `json:"overlayId"`
	// Revision counts the published states of the overlay. Equal revisions of one workspace mean equal trees.
	Revision uint64         `json:"revision"`
	State    WorkspaceState `json:"state"`
	// EffectiveTree is the tree object of the current state (the overlay applied over head over base).
	EffectiveTree string `json:"effectiveTree"`
	BaseTree      string `json:"baseTree"`
	HeadTree      string `json:"headTree,omitempty"`
	// Dirty is true when the effective tree differs from the last committed one (or from base before any commit).
	Dirty    bool `json:"dirty"`
	ReadOnly bool `json:"readOnly,omitempty"`
	// Exec is the open materialization, recorded so a restart knows a directory may hold unimported work.
	Exec *Exec `json:"exec,omitempty"`
	// Meta carries caller-owned facts that must survive restarts, such as dependency lineage.
	Meta    map[string]string `json:"meta,omitempty"`
	Created time.Time         `json:"created"`
	Updated time.Time         `json:"updated"`
}

// ResolvedFile is a path looked up in the effective view of a workspace revision.
type ResolvedFile struct {
	Path    string
	Content []byte
	BlobOID string
	Mode    uint32
	// Revision is the workspace revision the lookup was made at.
	Revision uint64
	Exists   bool
	// Layer says which layer supplied the file: "overlay" when the agent changed it since its last commit, "branch"
	// when its last commit changed it, "base" when it is untouched. Empty when the file does not exist; a deleted
	// path reports "overlay" or "branch" with Exists false, which is how a tombstone shows itself.
	Layer string
}

// Entry is one name in a directory listing.
type Entry struct {
	Name string `json:"name"`
	// Kind is file, dir, symlink or submodule.
	Kind string `json:"kind"`
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
}

// FileInfo describes one path.
type FileInfo struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Mode     uint32 `json:"mode"`
	Size     int64  `json:"size"`
	OID      string `json:"oid"`
	Revision uint64 `json:"revision"`
}

// FileChange is one file a workspace differs from its base by, taken from Git, not from what an agent says it did.
type FileChange struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"` // added, modified, deleted
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// ChangeProposal is the logical information a worktree diff gives the integration engine, taken from the overlay.
type ChangeProposal struct {
	AgentID    string `json:"agentId"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"baseCommit"`
	// Commit is the frozen revision the proposal was generated from; Tree is its tree.
	Commit   string `json:"commit"`
	Tree     string `json:"tree"`
	Revision uint64 `json:"revision"`
	Path     string `json:"path"`
	Op       string `json:"op"` // added, modified, deleted
	// BeforeBlob and AfterBlob are blob ids; empty when the file did not exist on that side.
	BeforeBlob string `json:"beforeBlob,omitempty"`
	AfterBlob  string `json:"afterBlob,omitempty"`
	BeforeMode uint32 `json:"beforeMode,omitempty"`
	AfterMode  uint32 `json:"afterMode,omitempty"`
	Added      int    `json:"added"`
	Deleted    int    `json:"deleted"`
}

// Op is one edit in a batch.
type Op struct {
	Kind string // write, delete, rename, chmod, symlink
	Path string
	// To is the destination of a rename, or the target of a symlink.
	To   string
	Data []byte
	// Mode is the file mode for a write or chmod; zero keeps an existing file's mode, or 0644 for a new file.
	Mode uint32
	// OID writes a blob that is already in the object database, instead of Data.
	OID string
}

// Event is the durable record of one published revision.
type Event struct {
	AgentID  string    `json:"agentId"`
	Revision uint64    `json:"revision"`
	Kind     string    `json:"kind"` // write, delete, rename, chmod, symlink, commit, rebase, capture, discard, create, dispose
	Paths    []string  `json:"paths,omitempty"`
	Tree     string    `json:"tree"`
	Commit   string    `json:"commit,omitempty"`
	At       time.Time `json:"at"`
}

// Metrics counts the work the engine did, so a benchmark can compare it with a worktree.
type Metrics struct {
	Creates        int64 `json:"creates"`
	Revisions      int64 `json:"revisions"`
	ObjectsWritten int64 `json:"objectsWritten"`
	BytesWritten   int64 `json:"bytesWritten"`
	GitSpawns      int64 `json:"gitSpawns"`
	Materialized   int64 `json:"materializations"`
	Captures       int64 `json:"captures"`
}
