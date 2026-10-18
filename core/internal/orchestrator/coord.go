package orchestrator

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/metrics"
	"github.com/relocode/relo/internal/permission"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/style"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
	"github.com/relocode/relo/internal/workspace"
)

type Mode string

const (
	ModeBuild  Mode = "build"
	ModePlan   Mode = "plan"
	ModeReview Mode = "review"
)

type Chat struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Text    string `json:"text"`
	AgentID string `json:"agentId,omitempty"`
	At      string `json:"createdAt"`
	// Queued marks a user message that waits for the running head agent's next step. It clears when delivered.
	Queued bool `json:"queued,omitempty"`
}

// Change is one file an agent changed. Status is proposed (waiting in the agent's worktree), accepted (merged into
// the workspace), rejected (discarded) or applied (made in place, no worktree). Kind is added, modified or deleted.
type Change struct {
	ID       string `json:"id"`
	AgentID  string `json:"agentId"`
	TaskID   string `json:"taskId"`
	Path     string `json:"path"`
	Status   string `json:"status"`
	Kind     string `json:"kind,omitempty"`
	Added    int    `json:"added,omitempty"`
	Deleted  int    `json:"deleted,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	BaseBlob string `json:"baseBlob,omitempty"`
	Conflict string `json:"conflict,omitempty"`
	// Resolving is set while the head agent is re-applying this change after a conflict check.
	Resolving bool `json:"resolving,omitempty"`
	// Superseded is set when a newer proposal for the same path replaced this conflicted change.
	Superseded bool `json:"superseded,omitempty"`

	// The fields below are additive. Rows written before they existed load with zero values.

	// BaseRev is the commit this change is diffed against: the agent's base, or the stacked commit when Needs is set.
	BaseRev string `json:"baseRev,omitempty"`
	// Needs lists the ids of the changes stacked into BaseRev. It is derived from Lineage.
	Needs []string `json:"needs,omitempty"`
	// Lineage holds the dependencies applied at stack time. It is written once and survives status changes and recuts.
	Lineage []Dep `json:"lineage,omitempty"`
	// SupersededBy replaces the Superseded bool: the superseded change is kept, not discarded.
	SupersededBy string `json:"supersededBy,omitempty"`
	// Landed is the accept outcome: created, updated, merged, deleted or unchanged.
	Landed string `json:"landed,omitempty"`
	// Integration is the result of the last projection of this change against the workspace.
	Integration *Integration `json:"integration,omitempty"`
	// Reason is why a change was discarded, such as "resolved by agent". Empty for other decisions.
	Reason string `json:"reason,omitempty"`
}

// Dep is a dependency applied when a change's base was stacked.
type Dep struct {
	ID        string `json:"id"`        // parent change id
	PatchHash string `json:"patchHash"` // sha256 of the parent patch as stacked
}

// Integration is the visible state of a change against the workspace at CheckedAt.
type Integration struct {
	State     string   `json:"state"`            // clean | conflicted | blocked | unknown
	Kind      string   `json:"kind,omitempty"`   // overlap, modify_delete, delete_modify, add_add, binary, symlink, protected, ...
	Reason    string   `json:"reason,omitempty"` // text derived from Kind
	Ours      string   `json:"ours,omitempty"`   // blob id of the workspace file the check ran against; "-" if absent
	Hunks     []Hunk   `json:"hunks,omitempty"`  // hunk locations, with conflicting hunks marked
	Since     []string `json:"since,omitempty"`  // accepted change ids, or "user", that moved the file after BaseRev
	CheckedAt int64    `json:"checkedAt,omitempty"`
	// Fingerprint identifies the three inputs this projection was computed from. An accept that carries it is
	// refused as stale_preview when the inputs changed since the preview.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Hunk is one changed region of a three-way merge. A side with no change has a zero start and length.
type Hunk struct {
	OursStart   int  `json:"oursStart"`
	OursLen     int  `json:"oursLen"`
	TheirsStart int  `json:"theirsStart"`
	TheirsLen   int  `json:"theirsLen"`
	Conflicting bool `json:"conflicting"`
}

// NormalizeIntegration fills Integration from the legacy Conflict string for rows written before Integration
// existed. It does nothing when an Integration is already present. It reports whether it filled the state.
func (ch *Change) NormalizeIntegration() bool {
	if ch.Integration != nil {
		return false
	}
	if ch.Conflict == "" {
		return false
	}
	ch.Integration = &Integration{State: "conflicted", Kind: LegacyConflictKind(ch.Conflict), Reason: ch.Conflict}
	return true
}

// LegacyConflictKind classifies a stored Conflict reason from before Integration existed.
func LegacyConflictKind(reason string) string {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "binary"):
		return "binary"
	case strings.Contains(lower, "agent deleted it"):
		return "modify_delete"
	case strings.Contains(lower, "you deleted this file"):
		return "delete_modify"
	case strings.Contains(lower, "agent created"):
		return "add_add"
	case strings.Contains(lower, "protected"):
		return "protected"
	case strings.Contains(lower, "symlink"):
		return "symlink"
	case strings.Contains(lower, "overlap"):
		return "overlap"
	}
	return "overlap"
}

type Settings struct {
	PrimaryModel   string            `json:"primaryModel"`
	Tier1Model     string            `json:"tier1Model"`
	Tier2Model     string            `json:"tier2Model"`
	MaxTotal       int               `json:"maxConcurrentAgents"`
	MaxTier1       int               `json:"maxTier1Agents"`
	MaxTier2       int               `json:"maxTier2Agents"`
	MaxDepth       int               `json:"maxHierarchyDepth"`
	MaxRetries     int               `json:"maxRetries"`
	PathLeases     bool              `json:"pathLeases"`
	MaxUtil        float64           `json:"maxUtilization"`
	CompactAt      float64           `json:"compactAt"`
	AutoRefresh    bool              `json:"autoRefresh"`
	Threshold      float64           `json:"refreshThreshold"`
	KeepHistory    bool              `json:"keepRawHistory"`
	Memory         bool              `json:"projectMemory"`
	Strategy       string            `json:"compressionStrategy"`
	AutoVerify     bool              `json:"autoVerify"`
	AutoReview     bool              `json:"autoReview"`
	Escalate       bool              `json:"escalate"`
	EscalateSet    bool              `json:"-"`
	ReportMaxChars int               `json:"reportMaxChars"`
	Budget         int               `json:"budget"`
	ByDifficulty   map[string]string `json:"byDifficulty,omitempty"`
	RoutingMode    string            `json:"routingMode"`
	ContextLimit   int               `json:"contextLimit"`
	Efficiency     style.Prefs       `json:"-"`
	// Models is the catalog snapshot used for per-model windows and efforts. Not persisted or sent to the UI here.
	Models map[string]runtime.Model `json:"-"`
	// ModelsKnown is true once the runtime's catalog has been read successfully. Only then does an empty or
	// missing Models entry mean "not available" rather than "not loaded yet".
	ModelsKnown  bool              `json:"-"`
	ModelEfforts map[string]string `json:"-"`
}

func DefaultSettings() Settings {
	return Settings{
		MaxTotal: 6, MaxTier1: 3, MaxTier2: 4, MaxDepth: 3, MaxRetries: 1,
		MaxUtil: 0.85, CompactAt: 0.9, AutoRefresh: true, Threshold: 0.8, KeepHistory: true,
		Memory: true, Strategy: "variable", AutoVerify: true, AutoReview: true, Escalate: true, ReportMaxChars: 1200,
		Budget: 24000,
	}
}

type Session struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Objective string `json:"objective"`
	// Constraints are the Primary's reading of what the request and context require of every sub agent.
	Constraints []string              `json:"constraints,omitempty"`
	Mode        Mode                  `json:"mode"`
	Status      string                `json:"status"`
	Created     time.Time             `json:"createdAt"`
	Updated     time.Time             `json:"updatedAt"`
	Messages    []Chat                `json:"messages,omitempty"`
	Changes     []Change              `json:"changes,omitempty"`
	Verify      []verification.Result `json:"verification,omitempty"`
	Usage       map[string]any        `json:"usage,omitempty"`
	Paused      bool                  `json:"paused"`
	Running     bool                  `json:"running"` // a Submit is in flight; the UI's single source for "busy"
}

type PendingPerm struct {
	ID      string
	Session string
	AgentID string
	Title   string
	Command string
	Reply   chan string
}

type Coord struct {
	Log      *logging.Logger
	Engine   *ctxeng.Engine
	Memory   *memory.Store
	Graph    *taskgraph.Graph
	Agents   *agent.Registry
	Sched    *scheduler.Scheduler
	Route    routing.Config
	WS       *workspace.Manager
	Verify   verification.Runner
	Perm     *permission.Policy
	Runtime  runtime.AgentRuntime
	Cache    *metrics.CacheStats
	Settings Settings
	Emit     func(string, any)
	// ResolveChanges is installed by the app to discard changes the head marked resolved. It is nil in tests that
	// do not need the app's reject path.
	ResolveChanges func([]string)

	mu              sync.Mutex
	configMu        sync.RWMutex
	emitMu          sync.RWMutex
	emitFn          func(string, any)
	cacheMu         sync.RWMutex
	sess            *Session
	lineage         map[string][]Dep          // agent ID -> dependencies applied to its base at stack time; guarded by mu
	lineageProblems map[string]lineageProblem // agent ID -> a recorded base that could not be re-stacked; guarded by mu
	inbox           map[string][]queuedNote   // session ID -> user messages waiting for the head's next step; guarded by mu
	wake            chan struct{}             // buffered nudge: the inbox has something for a head waiting on its sub agents
	sessionCtx      context.Context
	runCancel       context.CancelFunc
	runID           uint64
	runWG           sync.WaitGroup
	pending         map[string]*PendingPerm
	reviewed        map[string]bool
	runtimeMu       sync.Mutex
	runtimeSessions map[string]*runtimeSession
	escalations     map[string]string
	failures        map[string]string // task ID -> why its last attempt failed, for the attempt that replaces it; guarded by mu
	directSem       chan struct{}     // guarded by mu; one mutating agent at a time in direct mode
	directNotified  string            // session ID that already received the direct-mode notice; guarded by mu
	snapshotNoticed map[string]bool   // session ID -> the snapshot notice was already shown; guarded by mu
	baseline        map[string]bool   // "baseline dir\x00command" -> command already fails without the agent's change; guarded by mu
}

func New(log *logging.Logger) *Coord {
	c := &Coord{
		Log:             log,
		Engine:          ctxeng.New(),
		Memory:          memory.New(),
		Graph:           taskgraph.New(),
		Sched:           scheduler.New(scheduler.Limits{}),
		Perm:            permission.Default(),
		Settings:        DefaultSettings(),
		Cache:           &metrics.CacheStats{},
		pending:         map[string]*PendingPerm{},
		reviewed:        map[string]bool{},
		inbox:           map[string][]queuedNote{},
		wake:            make(chan struct{}, 1),
		runtimeSessions: map[string]*runtimeSession{},
		escalations:     map[string]string{},
		failures:        map[string]string{},
		lineageProblems: map[string]lineageProblem{},
		Emit:            func(string, any) {},
	}
	c.Agents = c.newRegistry()
	return c
}

// newRegistry returns an agent registry that announces every change as agent.updated. The hook runs outside the
// registry lock; it must never be reached while c.mu is held, because the emitter reads the session.
func (c *Coord) newRegistry() *agent.Registry {
	r := agent.NewRegistry()
	r.OnChange = func(a agent.Agent) { c.emit("agent.updated", a) }
	return r
}

// SessionID returns the current session id without copying the session.
func (c *Coord) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return ""
	}
	return c.sess.ID
}

func (c *Coord) settings() Settings {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.Settings
}

// SettingsSnapshot returns a copy of the settings; safe for concurrent use.
func (c *Coord) SettingsSnapshot() Settings { return c.settings() }

// UpdateSettings applies fn to the settings under the config lock and rebuilds the default route.
func (c *Coord) UpdateSettings(fn func(*Settings)) Settings {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	fn(&c.Settings)
	c.Route = c.Settings.RouteConfig()
	return c.Settings
}

func (c *Coord) routing() routing.Config {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.Route
}

func (c *Coord) scheduler() *scheduler.Scheduler {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.Sched
}

func (c *Coord) verifier() verification.Runner {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.Verify
}

func (c *Coord) permissions() *permission.Policy {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.Perm
}

func (c *Coord) workspace() *workspace.Manager {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.WS
}

func (c *Coord) SetWorkspaceManager(m *workspace.Manager) {
	c.configMu.Lock()
	c.WS = m
	c.configMu.Unlock()
}
