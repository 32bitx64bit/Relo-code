package orchestrator

import (
	"context"
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
	MaxUtil        float64           `json:"maxUtilization"`
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
		MaxUtil: 0.85, AutoRefresh: true, Threshold: 0.8, KeepHistory: true,
		Memory: true, Strategy: "variable", AutoVerify: true, AutoReview: true, Escalate: true, ReportMaxChars: 1200,
		Budget: 24000,
	}
}

type Session struct {
	ID        string                `json:"id"`
	Workspace string                `json:"workspace"`
	Objective string                `json:"objective"`
	Mode      Mode                  `json:"mode"`
	Status    string                `json:"status"`
	Created   time.Time             `json:"createdAt"`
	Updated   time.Time             `json:"updatedAt"`
	Messages  []Chat                `json:"messages,omitempty"`
	Changes   []Change              `json:"changes,omitempty"`
	Verify    []verification.Result `json:"verification,omitempty"`
	Usage     map[string]any        `json:"usage,omitempty"`
	Paused    bool                  `json:"paused"`
	Running   bool                  `json:"running"` // a Submit is in flight; the UI's single source for "busy"
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

	mu              sync.Mutex
	configMu        sync.RWMutex
	emitMu          sync.RWMutex
	emitFn          func(string, any)
	cacheMu         sync.RWMutex
	sess            *Session
	sessionCtx      context.Context
	runCancel       context.CancelFunc
	runID           uint64
	runWG           sync.WaitGroup
	pending         map[string]*PendingPerm
	reviewed        map[string]bool
	runtimeMu       sync.Mutex
	runtimeSessions map[string]*runtimeSession
	escalations     map[string]string
	directSem       chan struct{}   // guarded by mu; one mutating agent at a time in direct mode
	directNotified  string          // session ID that already received the direct-mode notice; guarded by mu
	baseline        map[string]bool // "baseline dir\x00command" -> command already fails without the agent's change; guarded by mu
	tasksPlanned    int             // tasks Primary has added this session; guarded by mu
	replans         int             // follow-up planning turns spent on the current request; guarded by mu
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
		runtimeSessions: map[string]*runtimeSession{},
		escalations:     map[string]string{},
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
