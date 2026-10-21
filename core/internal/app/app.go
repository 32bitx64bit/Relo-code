package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/id"
	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/metrics"
	"github.com/relocode/relo/internal/opencode"
	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/permission"
	"github.com/relocode/relo/internal/persistence"
	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/style"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
	"github.com/relocode/relo/internal/workspace"
)

type Config struct {
	Workspace string
	DBPath    string
	StateDir  string // Relo-owned OpenCode state root; empty = per-user default
	OCPath    string
	Runtime   string
	LogLevel  string
}

type App struct {
	Cfg        Config
	Log        *logging.Logger
	Store      *persistence.Store
	Coord      *orchestrator.Coord
	IPC        *ipc.Server
	Efficiency map[string]any
	once       sync.Once

	// procMu guards proc and serialises runtime start/stop, so two lifecycle calls can never leave an
	// orphaned OpenCode child behind.
	procMu sync.Mutex
	proc   *opencode.Process

	provMu          sync.Mutex
	providers       []provider.Provider
	providerKeys    map[string]string // id -> API key; memory only, never logged, persisted or emitted
	runningProvider string            // provider.Fingerprint of the config the running OpenCode was started with
	restartPending  bool
	restarting      atomic.Bool // a deferred restart is under way

	statusMu sync.RWMutex
	status   runtime.Health

	// sessMu serialises writes of the session to the database with switching sessions, so one session's agents are
	// never saved under another's id.
	sessMu     sync.Mutex
	decisionMu sync.Mutex // serialises change decisions for the current session
	vwMu       sync.Mutex // serialises mirroring of virtual workspaces into the store
	cleanupMu  sync.Mutex // serialises worktree discard and release outside decisionMu
	// cleanupHoldMu guards cleanupHolds. A rejected agent is held here from the decision until its blob is
	// retained, so a concurrent accept cannot release the worktree in between.
	cleanupHoldMu sync.Mutex
	cleanupHolds  map[string]int
	// keepBlobs and keepBlob are test seams for injected retention failures; nil uses the workspace manager.
	keepBlobs    func([]workspace.BlobRef) error
	keepBlob     func(changeID, worktree, rel string) error
	lazy         atomic.Bool
	dependent    atomic.Bool // P4: dependent/stacked accept and reject semantics
	projMu       sync.Mutex
	projLocks    map[string]*sync.Mutex // workspace-relative path -> projection lock
	projMemo     map[string]projectionMemo
	dirtyMu      sync.Mutex      // guards dirtyPaths
	dirtyPaths   map[string]bool // workspace-relative paths with an unsaved editor buffer
	autoApply    atomic.Bool
	autoMu       sync.Mutex
	autoDirty    bool
	autoStopped  bool
	autoDone     chan struct{} // non-nil while the coalesced auto-apply worker is running
	headMu       sync.Mutex
	headQueue    []headMessage // head messages waiting for the running turn to end; guarded by headMu
	headBusy     bool          // a goroutine is draining headQueue; guarded by headMu
	persistMu    sync.Mutex
	persistTimer *time.Timer
	closed       bool
}

func (a *App) setStatus(h runtime.Health) runtime.Health {
	a.statusMu.Lock()
	a.status = h
	a.statusMu.Unlock()
	return h
}

func (a *App) getStatus() runtime.Health {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	return a.status
}

// setDirtyPaths replaces the extension-reported set of paths with an unsaved editor buffer.
func (a *App) setDirtyPaths(paths []string) {
	next := make(map[string]bool, len(paths))
	for _, p := range paths {
		if clean := cleanWorkspacePath(p); clean != "" {
			next[clean] = true
		}
	}
	a.dirtyMu.Lock()
	a.dirtyPaths = next
	a.dirtyMu.Unlock()
}

// pathDirty reports whether the extension says path has an unsaved buffer.
func (a *App) pathDirty(path string) bool {
	clean := cleanWorkspacePath(path)
	if clean == "" {
		return false
	}
	a.dirtyMu.Lock()
	defer a.dirtyMu.Unlock()
	return a.dirtyPaths[clean]
}

// dirtyPathList returns the current dirty set, sorted for stable output.
func (a *App) dirtyPathList() []string {
	a.dirtyMu.Lock()
	defer a.dirtyMu.Unlock()
	out := make([]string, 0, len(a.dirtyPaths))
	for p := range a.dirtyPaths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func cleanWorkspacePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.ContainsAny(p, "\x00\n\r") || filepath.IsAbs(p) || strings.HasPrefix(p, "../") {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
}

// noPersistEvents are streaming/high-frequency events that are forwarded over IPC but not written to the event log.
var noPersistEvents = map[string]bool{
	"chat.delta":     true,
	"agent.updated":  true,
	"agent.activity": true,
	// Canvas requests carry artifact sources and screenshots, and replies carry capture data. Neither payload
	// is logged. The canvas audit sink stores a metadata-only row under the same event names instead.
	"canvas.requested": true,
	"canvas.reply":     true,
}

// logCanvasAudit writes one metadata-only canvas row to the event log. The raw canvas.requested and
// canvas.reply payloads carry artifact source and are never persisted; this row keeps only the facts
// needed to prove which role called canvas and why a call was refused. The event log is read back for
// the operator log view only, never replayed into an agent's context.
func (a *App) logCanvasAudit(rec orchestrator.CanvasAudit) {
	sess := a.Coord.SessionID()
	if sess == "" {
		return
	}
	if err := a.Store.LogEvent(sess, rec.Event, rec.Payload()); err != nil {
		a.Log.Error("persist canvas audit failed", map[string]any{"event": rec.Event, "sessionId": sess, "error": err.Error()})
	}
}

func New(cfg Config, log *logging.Logger) (*App, error) {
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.Workspace, ".relo", "relo.db")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return nil, err
	}
	store, err := persistence.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	a := &App{Cfg: cfg, Log: log, Store: store, Coord: orchestrator.New(log)}
	a.Coord.ResolveChanges = a.resolveHeadChanges
	// Canvas calls are audited in the event log with metadata only: the raw payloads stay out of the log.
	a.Coord.SetCanvasAudit(a.logCanvasAudit)
	ws := workspace.New(cfg.Workspace)
	ws.SetNotifier(a.workspaceNotice)
	ws.SetObserver(a.workspaceEvent)
	a.Coord.SetWorkspaceManager(ws)
	a.Coord.Verify = verification.Runner{Root: cfg.Workspace}
	a.Coord.Configure(a.Coord.Settings, scheduler.New(scheduler.Limits{PathLeases: a.Coord.Settings.PathLeases}), a.Coord.Route)
	if _, err := ws.Snapshot(); err != nil {
		log.Warn("workspace snapshot", map[string]any{"error": err.Error(), "workspace": cfg.Workspace})
	}
	a.reconcileWorkspaces()
	return a, nil
}

func (a *App) AttachIPC(srv *ipc.Server) {
	a.IPC = srv
	a.Coord.SetEmitter(func(name string, payload any) {
		if err := srv.Emit(name, payload); err != nil {
			a.Log.Warn("ipc emit failed", map[string]any{"event": name, "error": err.Error()})
		}
		if sess := a.Coord.SessionID(); sess != "" {
			// High-frequency streaming events are not persisted; they flood the DB and are
			// superseded by later state. They are still forwarded to IPC above.
			if !noPersistEvents[name] {
				if err := a.Store.LogEvent(sess, name, payload); err != nil {
					a.Log.Error("persist event failed", map[string]any{"event": name, "sessionId": sess, "error": err.Error()})
				}
			}
			if name == "usage.updated" || name == "cache.updated" {
				if err := a.Store.PutUsage(sess, payload); err != nil {
					a.Log.Error("persist usage failed", map[string]any{"event": name, "sessionId": sess, "error": err.Error()})
				}
			}
			if persistEvents[name] {
				a.persistSoon()
			}
		}
		if name == "changes.updated" {
			go a.safely("supersede conflicts", a.supersedeStale)
			a.scheduleAutoApply()
		}
		if name == "agent.completed" || name == "agent.failed" {
			go a.safely("deferred restart", a.applyPendingRestart)
		}
	})
}

func (a *App) Handle(method string, params json.RawMessage) (any, error) {
	p := ipc.MustParams(params)
	ctx := context.Background()
	switch method {
	case "initialize":
		return a.initialize(ctx, p)
	case "shutdown":
		a.Close()
		return map[string]any{"ok": true}, nil
	case "session.create", "session.clear":
		mode := orchestrator.Mode(str(p, "mode", string(a.Coord.CurrentMode())))
		if method == "session.create" {
			mode = orchestrator.Mode(str(p, "mode", "build"))
		}
		a.newSession(ctx, mode, str(p, "objective", ""))
		return a.Coord.Snapshot(), nil
	case "session.get":
		return a.Coord.Current(), nil
	case "session.list":
		return a.listSessions()
	case "session.open":
		return a.openSession(ctx, str(p, "id", ""))
	case "session.delete":
		return a.deleteSession(ctx, str(p, "id", ""))
	case "prompt.submit":
		text := str(p, "text", "")
		var refs []string
		if raw, ok := p["refs"].([]any); ok {
			for _, r := range raw {
				refs = append(refs, fmt.Sprint(r))
			}
		}
		if a.Coord.Current() == nil {
			a.Coord.CreateSession(a.Cfg.Workspace, text, orchestrator.ModeBuild)
		}
		go a.safely("submit", func() {
			if err := a.Coord.Submit(context.Background(), text, refs); err != nil {
				a.Log.Error("submit", map[string]any{"error": err.Error()})
				if err := a.IPCEmit("log", map[string]any{"error": err.Error()}); err != nil {
					a.Log.Warn("ipc emit failed", map[string]any{"event": "log", "error": err.Error()})
				}
			}
			a.persist()
		})
		return map[string]any{"ok": true}, nil
	case "prompt.cancel":
		a.Coord.StopAll(ctx)
		return map[string]any{"ok": true}, nil
	case "prompt.retry":
		if s := a.Coord.Current(); s != nil {
			text := s.Objective
			for i := len(s.Messages) - 1; i >= 0; i-- {
				if s.Messages[i].Role == "user" && !s.Messages[i].Queued && !s.Messages[i].Dropped {
					text = s.Messages[i].Text
					break
				}
			}
			if text == "" {
				return map[string]any{"ok": true}, nil
			}
			go a.safely("retry", func() {
				if err := a.Coord.Submit(context.Background(), text, nil); err != nil {
					a.Log.Error("retry submit failed", map[string]any{"sessionId": s.ID, "error": err.Error()})
				}
				a.persist()
			})
		}
		return map[string]any{"ok": true}, nil
	case "agent.list":
		return a.Coord.Agents.List(), nil
	case "agent.get":
		ag, _ := a.Coord.Agents.Get(str(p, "id", ""))
		return ag, nil
	case "agent.stop":
		id := str(p, "id", "")
		rt := a.Coord.RuntimeSnapshot()
		if ag, ok := a.Coord.Agents.Get(id); ok && ag.RuntimeID != "" && rt != nil {
			if err := rt.Cancel(ctx, runtime.SessionID(ag.RuntimeID)); err != nil {
				a.Log.Warn("cancel agent failed", map[string]any{"agentId": id, "error": err.Error()})
			}
		}
		a.Coord.Agents.Update(id, func(x *agent.Agent) { x.Status = agent.Cancelled })
		return map[string]any{"ok": true}, nil
	case "agent.redirect":
		a.Coord.Redirect(str(p, "id", ""), str(p, "instruction", ""))
		return map[string]any{"ok": true}, nil
	case "agent.message":
		a.Coord.Engine.Ingest(str(p, "id", ""), []ctxeng.Item{{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Content: str(p, "text", ""), Pinned: true, Durability: 1}})
		return map[string]any{"ok": true}, nil
	case "agent.changeModel":
		a.Coord.Agents.Update(str(p, "id", ""), func(x *agent.Agent) { x.Model = str(p, "model", x.Model) })
		return map[string]any{"ok": true}, nil
	case "agent.refreshContext":
		return a.Coord.RefreshAgent(str(p, "id", ""), str(p, "reason", "manual"))
	case "agent.inspectContext":
		return a.Coord.Engine.Inspect(str(p, "id", "")), nil
	case "agent.openWorktree":
		ag, ok := a.Coord.Agents.Get(str(p, "id", ""))
		if !ok {
			return nil, ipc.Fail{C: "not_found", M: "agent not found"}
		}
		return a.openAgentDirectory(ag)
	case "task.list":
		return a.Coord.Graph.List(), nil
	case "task.get":
		t, _ := a.Coord.Graph.Get(str(p, "id", ""))
		return t, nil
	case "task.cancel":
		return map[string]any{"ok": a.Coord.Graph.Cancel(str(p, "id", "")) == nil}, nil
	case "task.retry":
		return a.Coord.Graph.Retry(str(p, "id", ""))
	case "task.setPriority":
		id := str(p, "id", "")
		pr := int(num(p, "priority", 0))
		return a.Coord.Graph.Update(id, func(t *taskgraph.Task) { t.Priority = pr })
	case "context.get":
		return a.Coord.Engine.Inspect(str(p, "agentId", "")), nil
	case "context.generations":
		return a.Coord.Engine.Generations(str(p, "agentId", "")), nil
	case "context.refresh":
		return a.Coord.RefreshAgent(str(p, "agentId", ""), str(p, "reason", "manual refresh"))
	case "context.rebuild":
		id := str(p, "agentId", "")
		return a.Coord.Engine.Build(ctxeng.BuildRequest{AgentID: id, Reason: "rebuild"})
	case "context.pin":
		return map[string]any{"ok": a.Coord.Engine.Pin(str(p, "agentId", ""), str(p, "itemId", "")) == nil}, nil
	case "context.remove":
		return map[string]any{"ok": a.Coord.Engine.Drop(str(p, "agentId", ""), str(p, "itemId", "")) == nil}, nil
	case "context.promote":
		item := str(p, "itemId", "")
		agentID := str(p, "agentId", "")
		for _, it := range a.Coord.Engine.SnapshotPool(agentID) {
			if it.ID == item {
				a.Coord.Memory.Upsert(memFrom(it))
				a.Coord.Notify("context.item.promoted", it)
				a.Coord.Notify("memory.updated", it)
				return it, nil
			}
		}
		return nil, ipc.Fail{C: "not_found", M: "item not found"}
	case "findings.list":
		return a.Coord.Engine.Findings(), nil
	case "memory.list":
		return a.Coord.Memory.List(), nil
	case "memory.update":
		e := a.Coord.Memory.Upsert(memFromParams(p))
		return e, nil
	case "memory.delete":
		return map[string]any{"ok": a.Coord.Memory.Delete(str(p, "id", ""))}, nil
	case "changes.list":
		a.projectPaths(a.proposedPaths())
		if s := a.Coord.Current(); s != nil {
			return s.Changes, nil
		}
		return []any{}, nil
	case "changes.accept":
		if withParents, _ := p["withParents"].(bool); withParents {
			return a.acceptWithParents(str(p, "id", ""))
		}
		return a.acceptChangeExpect(str(p, "id", ""), str(p, "expect", ""))
	case "changes.reject":
		if withDependents, _ := p["withDependents"].(bool); withDependents {
			return a.rejectWithDependents(str(p, "id", ""))
		}
		return a.rejectChange(str(p, "id", ""))
	case "changes.acceptAll":
		return a.decideAll(true)
	case "changes.rejectAll":
		return a.decideAll(false)
	case "changes.undo":
		return a.undoChange(str(p, "id", ""))
	case "changes.diff":
		return a.changeDiff(str(p, "id", ""))
	case "changes.rebase":
		apply, _ := p["apply"].(bool)
		return a.rebaseChange(str(p, "id", ""), apply)
	case "workspace.dirty":
		var paths []string
		if raw, ok := p["paths"].([]any); ok {
			for _, item := range raw {
				paths = append(paths, fmt.Sprint(item))
			}
		}
		a.setDirtyPaths(paths)
		return map[string]any{"ok": true, "paths": a.dirtyPathList()}, nil
	case "changes.resolveConflicts":
		return a.resolveConflicts()
	case "changes.resolve":
		return a.resolveChange(str(p, "id", ""), str(p, "choice", ""))
	case "verification.list":
		if s := a.Coord.Current(); s != nil {
			return s.Verify, nil
		}
		return []any{}, nil
	case "models.list":
		ms, err := a.models(ctx)
		if ms == nil && err == nil {
			return []any{}, nil
		}
		return ms, err
	case "models.set":
		s := a.Coord.UpdateSettings(func(s *orchestrator.Settings) {
			s.PrimaryModel = str(p, "primary", s.PrimaryModel)
			s.Tier1Model = str(p, "tier1", s.Tier1Model)
			s.Tier2Model = str(p, "tier2", s.Tier2Model)
		})
		if err := a.Store.SetKV("models", fmt.Sprintf("%s|%s|%s", s.PrimaryModel, s.Tier1Model, s.Tier2Model)); err != nil {
			a.Log.Error("persist models failed", map[string]any{"error": err.Error()})
		}
		return s, nil
	case "backend.status":
		return a.backendStatus(), nil
	case "cache.get":
		return cachePayload(a.Coord.Cache.Snapshot()), nil
	case "backend.restart":
		return a.restartRuntime(ctx)
	case "settings.apply":
		return a.settingsApply(ctx, p)
	case "providers.discover":
		return a.providersDiscover(ctx, p)
	case "mode.set":
		a.Coord.SetMode(orchestrator.Mode(str(p, "mode", "build")))
		return map[string]any{"mode": a.Coord.CurrentMode()}, nil
	case "orchestrator.pause":
		a.Coord.Pause()
		return map[string]any{"ok": true}, nil
	case "orchestrator.resume":
		a.Coord.Resume()
		return map[string]any{"ok": true}, nil
	case "orchestrator.stopAll":
		a.Coord.StopAll(ctx)
		return map[string]any{"ok": true}, nil
	case "canvas.reply":
		// The extension answers each canvas.requested with its requestId. The reply has no session of its own:
		// the core matches it to the pending call and rejects any call whose session has changed.
		requestID := str(p, "requestId", "")
		var errText string
		if raw, ok := p["error"].(string); ok {
			errText = raw
		}
		if err := a.Coord.CanvasReply(requestID, p["result"], errText); err != nil {
			return nil, ipc.Fail{C: "canvas_reply_rejected", M: err.Error()}
		}
		return map[string]any{"ok": true}, nil
	case "permission.respond":
		response := str(p, "response", "reject")
		if response != "once" && response != "always" && response != "reject" {
			return nil, ipc.Fail{C: "invalid_response", M: "permission response must be once, always, or reject"}
		}
		a.Coord.RespondPermission(str(p, "id", ""), response)
		return map[string]any{"ok": true}, nil
	case "logs.tail":
		if s := a.Coord.Current(); s != nil {
			return a.Store.Events(s.ID, 200)
		}
		return []any{}, nil
	case "snapshot":
		return a.Coord.Snapshot(), nil
	default:
		return nil, ipc.Fail{C: "unknown_method", M: method}
	}
}

func (a *App) initialize(ctx context.Context, p map[string]any) (any, error) {
	a.decisionMu.Lock()
	defer a.decisionMu.Unlock()
	if v := str(p, "workspace", ""); v != "" {
		a.Cfg.Workspace = v
	}
	a.applySettings(p)
	// The extension sends canvas:true only when it can answer canvas.requested. Anything else keeps Canvas off.
	a.Coord.SetCanvasEnabled(canvasFlag(p))
	st, err := a.startRuntime(ctx)
	if err != nil {
		a.Log.Warn("runtime", map[string]any{"error": err.Error()})
	}
	if raw, err := a.Store.LatestSession(a.Cfg.Workspace); err == nil {
		var s orchestrator.Session
		if json.Unmarshal(raw, &s) == nil {
			a.Coord.Restore(&s)
			a.restoreSession(s.ID)
			a.clearResolvingLoaded()
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		a.Log.Error("restore session failed", map[string]any{"workspace": a.Cfg.Workspace, "error": err.Error()})
	}
	if a.Coord.Current() == nil {
		a.Coord.CreateSession(a.Cfg.Workspace, "", orchestrator.ModeBuild)
	}
	go a.safely("sweep", a.sweepWorktrees) // worktrees left by earlier runs that no change refers to
	{
		if models, err := a.models(ctx); err == nil && len(models) > 0 {
			a.Coord.UpdateSettings(func(s *orchestrator.Settings) {
				if s.PrimaryModel == "" {
					s.PrimaryModel = routing.PickDefault(models, []string{"opus", "sonnet", "gpt-4", "relo-primary"})
				}
				if s.Tier1Model == "" {
					s.Tier1Model = routing.PickDefault(models, []string{"sonnet", "gpt-4", "relo-tier1"})
				}
				if s.Tier2Model == "" {
					s.Tier2Model = routing.PickDefault(models, []string{"haiku", "mini", "flash", "relo-tier2"})
				}
			})
		}
	}
	a.refreshModels(ctx)
	settings := a.Coord.SettingsSnapshot()
	a.Coord.Configure(settings, scheduler.New(scheduler.Limits{MaxTotal: settings.MaxTotal, MaxTier1: settings.MaxTier1, MaxTier2: settings.MaxTier2, MaxDepth: settings.MaxDepth, MaxRetry: settings.MaxRetries, PathLeases: settings.PathLeases}), settings.RouteConfig())
	return map[string]any{
		"session":  a.Coord.Current(),
		"backend":  st,
		"settings": a.Coord.SettingsSnapshot(),
	}, nil
}

// applyWorkspaceSettings reads relo.workspace.* from an initialize/settings.apply payload. Only keys that are present
// change anything, and a mode the core does not know means worktrees, so a bad value can never turn the feature on.
func (a *App) applyWorkspaceSettings(p map[string]any) {
	ws := a.Coord.WS
	if ws == nil {
		return
	}
	cfg, changed := ws.Config(), false
	if v := str(p, "workspaceMode", ""); v != "" {
		cfg.Mode, changed = workspace.Mode(v), true
	}
	if v := str(p, "workspaceMaterialization", ""); v != "" {
		cfg.Materialization, changed = v, true
	}
	if v, ok := p["workspaceVirtualReads"].(bool); ok {
		cfg.VirtualReads, changed = v, true
	}
	if changed {
		ws.Configure(cfg)
	}
}

// workspaceNotice shows the user what the workspace layer decided on its own: a fallback to worktrees, a recovery
// after a stop, a file that was not kept. It never blocks the caller, which may hold workspace locks.
func (a *App) workspaceNotice(kind, message string) {
	a.Log.Info("workspace notice", map[string]any{"kind": kind, "message": message})
	go a.Coord.Note(message)
}

func (a *App) applySettings(p map[string]any) {
	if enabled, ok := p["autoApply"].(bool); ok {
		a.autoApply.Store(enabled)
		if enabled {
			a.scheduleAutoApply()
		}
	}
	if enabled, ok := p["lazyConflicts"].(bool); ok {
		a.lazy.Store(enabled)
	}
	if enabled, ok := p["dependentSemantics"].(bool); ok {
		a.dependent.Store(enabled)
	}
	s := a.Coord.SettingsSnapshot()
	if enabled, ok := p["pathLeases"].(bool); ok {
		s.PathLeases = enabled
	}
	if v := str(p, "primaryModel", ""); v != "" {
		s.PrimaryModel = v
	}
	if v := str(p, "tier1Model", ""); v != "" {
		s.Tier1Model = v
	}
	if v := str(p, "tier2Model", ""); v != "" {
		s.Tier2Model = v
	}
	if n := int(num(p, "maxConcurrentAgents", 0)); n > 0 {
		s.MaxTotal = n
	}
	if n := int(num(p, "maxTier1Agents", 0)); n > 0 {
		s.MaxTier1 = n
	}
	if n := int(num(p, "maxTier2Agents", 0)); n > 0 {
		s.MaxTier2 = n
	}
	if n := int(num(p, "maxHierarchyDepth", 0)); n > 0 {
		s.MaxDepth = n
	}
	if n := int(num(p, "maxRetries", 0)); n > 0 {
		s.MaxRetries = n
	}
	if n := num(p, "maxUtilization", 0); n > 0 {
		s.MaxUtil = n
	}
	if _, ok := p["compactAt"]; ok { // 0 is a real value here: it turns compaction off
		if n := num(p, "compactAt", 0.9); n >= 0 && n <= 1 {
			s.CompactAt = n
		}
	}
	if n := num(p, "refreshThreshold", 0); n > 0 {
		s.Threshold = n
	}
	if v, ok := p["autoRefresh"].(bool); ok {
		s.AutoRefresh = v
	}
	if v, ok := p["keepRawHistory"].(bool); ok {
		s.KeepHistory = v
	}
	if v, ok := p["projectMemory"].(bool); ok {
		s.Memory = v
	}
	if v := str(p, "compressionStrategy", ""); v != "" {
		s.Strategy = v
	}
	if v, ok := p["autoVerify"].(bool); ok {
		s.AutoVerify = v
	}
	if v, ok := p["autoReview"].(bool); ok {
		s.AutoReview = v
	}
	if v, ok := p["escalate"].(bool); ok {
		s.Escalate, s.EscalateSet = v, true
	}
	if n := int(num(p, "reportMaxChars", 0)); n > 0 {
		s.ReportMaxChars = n
	}
	a.applyWorkspaceSettings(p)
	if list, ok := pathList(p, "ignorePaths"); ok {
		if ws := a.Coord.WS; ws != nil {
			ws.SetIgnore(list)
		}
	}
	union, hasUnion := pathList(p, "unionPaths")
	takeLatest, hasTakeLatest := pathList(p, "takeLatestPaths")
	if hasUnion || hasTakeLatest {
		pol := integration.CurrentPolicy()
		if hasUnion {
			pol.Union = union
		}
		if hasTakeLatest {
			pol.TakeLatest = takeLatest
		}
		integration.SetPolicy(pol)
	}
	if efficiency, ok := p["efficiency"].(map[string]any); ok {
		a.Efficiency = efficiency
		prefs := parseEfficiencyPrefs(efficiency)
		prefs.ReportMaxChars = s.ReportMaxChars
		s.Efficiency = prefs
	}
	if byDifficulty, ok := p["byDifficulty"].(map[string]any); ok {
		models := make(map[string]string, len(byDifficulty))
		for level, model := range byDifficulty {
			if value, ok := model.(string); ok {
				models[level] = value
			}
		}
		if len(models) > 0 {
			s.ByDifficulty = models
		}
	}
	if _, ok := p["routingMode"]; ok {
		s.RoutingMode = routing.NormalizeMode(str(p, "routingMode", ""))
	}
	if _, ok := p["contextLimit"]; ok {
		if n := int(num(p, "contextLimit", 0)); n >= 0 {
			s.ContextLimit = n
		}
	}
	if raw, ok := p["modelEfforts"].(map[string]any); ok {
		efforts := make(map[string]string, len(raw))
		for model, v := range raw {
			if e, ok := v.(string); ok && e != "" {
				efforts[model] = e
			}
		}
		s.ModelEfforts = efforts
	}
	a.applyProviders(p)
	permissions := []struct {
		key    string
		action permission.Action
	}{
		{"permissionRead", permission.Read}, {"permissionWrite", permission.Write},
		{"permissionTest", permission.Test}, {"permissionBuild", permission.Build},
		{"permissionInstall", permission.Install}, {"permissionDelete", permission.Delete},
		{"permissionGitCommit", permission.GitCommit}, {"permissionGitPush", permission.GitPush},
		{"permissionNetwork", permission.Network}, {"permissionOutside", permission.Outside},
		{"permissionDestructive", permission.Destructive},
	}
	for _, item := range permissions {
		switch str(p, item.key, "") {
		case "allow":
			a.Coord.Perm.Set(item.action, permission.Allow)
		case "ask":
			a.Coord.Perm.Set(item.action, permission.Ask)
		case "deny":
			a.Coord.Perm.Set(item.action, permission.Deny)
		}
	}
	if level := str(p, "logLevel", ""); level != "" && a.Log != nil {
		a.Log.SetLevel(logging.ParseLevel(level))
	}
	a.Coord.Configure(s, scheduler.New(scheduler.Limits{MaxTotal: s.MaxTotal, MaxTier1: s.MaxTier1, MaxTier2: s.MaxTier2, MaxDepth: s.MaxDepth, MaxRetry: s.MaxRetries, PathLeases: s.PathLeases}), s.RouteConfig())
	a.Coord.UpdateSettings(func(ss *orchestrator.Settings) { ss.Efficiency = s.Efficiency })
	if a.Cfg.Runtime == "" {
		a.Cfg.Runtime = str(p, "runtime", "opencode")
	}
	if v := str(p, "openCodePath", ""); v != "" {
		a.Cfg.OCPath = v
	}
}

// startRuntime brings up a runtime and retires the previous OpenCode child, whatever path it takes
// (OpenCode, mock, or mock fallback). The new runtime is installed before the old child is stopped, so
// the coordinator never points at a closed client.
func (a *App) startRuntime(ctx context.Context) (runtime.Health, error) {
	a.procMu.Lock()
	defer a.procMu.Unlock()
	kind := strings.ToLower(a.Cfg.Runtime)
	if kind == "" {
		kind = "opencode"
	}
	fallback := func(err error) (runtime.Health, error) {
		a.Coord.SetRuntime(runtime.NewMock())
		a.swapProcLocked(nil)
		h := a.setStatus(runtime.Health{Healthy: true, Kind: "mock", Version: "mock-fallback",
			Error: err.Error() + " — using mock runtime so Relo still starts."})
		a.emit("backend.status", a.backendStatus())
		return h, err
	}
	if kind == "mock" {
		a.Coord.SetRuntime(runtime.NewMock())
		a.swapProcLocked(nil)
		h := a.setStatus(runtime.Health{Healthy: true, Version: "mock-1", Kind: "mock"})
		a.emit("backend.status", a.backendStatus())
		return h, nil
	}
	bin, err := opencode.Look(a.Cfg.OCPath)
	if err != nil {
		return fallback(err)
	}
	env, err := a.providerEnv()
	if err != nil {
		return fallback(err)
	}
	env = append(env, opencode.Isolation(a.stateRoot())...)
	proc, err := opencode.Start(ctx, bin, a.Cfg.Workspace, env)
	if err != nil {
		return fallback(err)
	}
	a.markProviderRunning()
	a.Coord.SetRuntime(proc.Runtime())
	a.swapProcLocked(proc)
	h := a.setStatus(proc.Client().Health(ctx))
	a.emit("backend.status", a.backendStatus())
	return h, nil
}

// stateRoot is Relo's private directory for the OpenCode child's config, sessions, auth and logs. It is
// separate from any OpenCode install the user has. RELO_SHARE_OPENCODE=1 turns isolation (and the built-in
// provider filter) off for development, so the child sees the user's own OpenCode setup again.
func (a *App) stateRoot() string {
	if shareOpenCode() {
		return ""
	}
	if a.Cfg.StateDir != "" {
		return a.Cfg.StateDir
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "relo-code", "opencode")
	}
	return filepath.Join(a.Cfg.Workspace, ".relo", "opencode")
}

func shareOpenCode() bool { return os.Getenv("RELO_SHARE_OPENCODE") == "1" }

// swapProcLocked makes next the current child and stops the one it replaces. procMu must be held.
func (a *App) swapProcLocked(next *opencode.Process) {
	old := a.proc
	a.proc = next
	if old == nil || old == next {
		return
	}
	if err := old.Stop(); err != nil {
		a.Log.Warn("stop OpenCode process failed", map[string]any{"error": err.Error()})
	}
}

func (a *App) backendStatus() map[string]any {
	models := []runtime.Model{}
	rt := a.Coord.RuntimeSnapshot()
	if rt != nil {
		ms, err := a.models(context.Background())
		if err != nil {
			a.Log.Warn("list models failed", map[string]any{"error": err.Error()})
		} else if ms != nil {
			models = ms
		}
	}
	st := a.getStatus()
	ready := st.Healthy && rt != nil
	cfg := a.Coord.SettingsSnapshot()
	return map[string]any{
		"core":         "ready",
		"opencode":     st.Kind,
		"version":      st.Version,
		"error":        st.Error,
		"models":       models,
		"ready":        ready,
		"primary":      cfg.PrimaryModel,
		"tier1":        cfg.Tier1Model,
		"tier2":        cfg.Tier2Model,
		"routingMode":  routing.NormalizeMode(cfg.RoutingMode),
		"contextLimit": cfg.ContextLimit,
	}
}

func (a *App) persist() {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	a.persistLocked()
}

// persistLocked writes the current session; the caller holds sessMu.
func (a *App) persistLocked() {
	s := a.Coord.Current()
	if s == nil {
		return
	}
	a.putJSON("sessions", s.ID, "", s.Workspace, s)
	for _, ag := range a.Coord.Agents.List() {
		a.putJSON("agents", ag.ID, s.ID, "", ag)
		if report, ok := a.Coord.Agents.Report(ag.ID); ok {
			a.putJSON("reports", ag.ID, s.ID, ag.ID, report)
		}
	}
	for _, t := range a.Coord.Graph.List() {
		a.putJSON("tasks", t.ID, s.ID, "", t)
	}
	for _, f := range a.Coord.Engine.Findings() {
		a.putJSON("findings", f.ID, s.ID, "", f)
	}
	for _, e := range a.Coord.Memory.List() {
		a.putJSON("memory", e.ID, "", s.Workspace, e)
	}
	for _, ch := range s.Changes {
		a.putJSON("changes", ch.ID, s.ID, "", ch)
	}
	for _, result := range s.Verify {
		a.putJSON("verification", result.ID, s.ID, "", result)
	}
	for _, ag := range a.Coord.Agents.List() {
		for _, gen := range a.Coord.Engine.Generations(ag.ID) {
			if err := a.Store.PutGeneration(s.ID, ag.ID, fmt.Sprintf("%s-%d", ag.ID, gen.Number), gen.Number, gen); err != nil {
				a.Log.Error("persist generation failed", map[string]any{"sessionId": s.ID, "agentId": ag.ID, "generation": gen.Number, "error": err.Error()})
			}
		}
	}
}

func (a *App) putJSON(table, id, session, extra string, v any) {
	if err := a.Store.PutJSON(table, id, session, extra, v); err != nil {
		a.Log.Error("persist failed", map[string]any{"table": table, "id": id, "sessionId": session, "error": err.Error()})
	}
}

func (a *App) emit(name string, payload any) {
	if err := a.IPCEmit(name, payload); err != nil {
		a.Log.Warn("ipc emit failed", map[string]any{"event": name, "error": err.Error()})
	}
}

func (a *App) IPCEmit(name string, payload any) error {
	if a.IPC == nil {
		return nil
	}
	return a.IPC.Emit(name, payload)
}

func (a *App) restoreSession(sessionID string) {
	var cache *metrics.CacheSnapshot
	if raws, err := a.Store.List("agents", sessionID); err == nil {
		for _, raw := range raws {
			var ag agent.Agent
			if json.Unmarshal(raw, &ag) != nil {
				continue
			}
			if ag.Status == agent.Running || ag.Status == agent.Starting || ag.Status == agent.Waiting {
				ag.Status = agent.Interrupted
				ag.Activity = "interrupted by reload"
			}
			a.Coord.Agents.Create(ag)
			var report agent.Report
			if err := a.Store.Get("reports", ag.ID, &report); err == nil {
				a.Coord.Agents.SetReport(ag.ID, report)
			}
		}
	} else {
		a.Log.Error("restore agents failed", map[string]any{"sessionId": sessionID, "error": err.Error()})
	}
	if raws, err := a.Store.List("tasks", sessionID); err == nil {
		for _, raw := range raws {
			var t taskgraph.Task
			if json.Unmarshal(raw, &t) != nil {
				continue
			}
			if t.Status == taskgraph.Running {
				t.Status = taskgraph.Queued
			}
			a.Coord.Graph.Add(t)
		}
	} else {
		a.Log.Error("restore tasks failed", map[string]any{"sessionId": sessionID, "error": err.Error()})
	}
	if raws, err := a.Store.List("findings", sessionID); err == nil {
		for _, raw := range raws {
			var f ctxeng.Finding
			if json.Unmarshal(raw, &f) != nil {
				continue
			}
			a.Coord.Engine.PromoteFinding(f)
		}
	} else {
		a.Log.Error("restore findings failed", map[string]any{"sessionId": sessionID, "error": err.Error()})
	}
	s := a.Coord.Current()
	if s == nil {
		return
	}
	if s.Usage == nil { // omitempty drops an empty map on save; a stored usage record would panic on write
		s.Usage = map[string]any{}
	}
	if raws, err := a.Store.List("memory", s.Workspace); err == nil {
		for _, raw := range raws {
			var e memory.Entry
			if json.Unmarshal(raw, &e) == nil {
				a.Coord.Memory.Upsert(e)
			}
		}
	} else {
		a.Log.Error("restore memory failed", map[string]any{"workspace": s.Workspace, "error": err.Error()})
	}
	if raws, err := a.Store.List("changes", sessionID); err == nil {
		for _, raw := range raws {
			var change orchestrator.Change
			if json.Unmarshal(raw, &change) != nil {
				continue
			}
			found := false
			for i := range s.Changes {
				if s.Changes[i].ID == change.ID {
					s.Changes[i] = change
					found = true
					break
				}
			}
			if !found {
				s.Changes = append(s.Changes, change)
			}
		}
	} else {
		a.Log.Error("restore changes failed", map[string]any{"sessionId": sessionID, "error": err.Error()})
	}
	if raws, err := a.Store.List("verification", sessionID); err == nil {
		for _, raw := range raws {
			var result verification.Result
			if json.Unmarshal(raw, &result) != nil {
				continue
			}
			found := false
			for _, got := range s.Verify {
				if got.ID == result.ID {
					found = true
					break
				}
			}
			if !found {
				s.Verify = append(s.Verify, result)
			}
		}
	} else {
		a.Log.Error("restore verification failed", map[string]any{"sessionId": sessionID, "error": err.Error()})
	}
	if raws, err := a.Store.List("usage", sessionID); err == nil {
		for i, raw := range raws {
			var snapshot struct {
				Agents []struct {
					AgentID    string        `json:"agentId"`
					Generation int           `json:"generation"`
					Model      string        `json:"model"`
					Usage      metrics.Usage `json:"usage"`
					Input      int           `json:"input"`
					Output     int           `json:"output"`
					CacheRead  int           `json:"cacheRead"`
					CacheWrite int           `json:"cacheWrite"`
				} `json:"agents"`
			}
			if json.Unmarshal(raw, &snapshot) == nil && snapshot.Agents != nil {
				parsed := &metrics.CacheSnapshot{}
				for _, row := range snapshot.Agents {
					usage := row.Usage
					if usage == (metrics.Usage{}) {
						usage = metrics.Usage{Input: row.Input, Output: row.Output, CacheRead: row.CacheRead, CacheWrite: row.CacheWrite}
					}
					parsed.Agents = append(parsed.Agents, metrics.CacheRow{AgentID: row.AgentID, Generation: row.Generation, Model: row.Model, Usage: usage})
				}
				cache = parsed
				continue
			}
			var usage map[string]any
			if json.Unmarshal(raw, &usage) == nil {
				if agentID, ok := usage["agentId"].(string); ok {
					s.Usage[agentID] = usage
				} else {
					s.Usage[fmt.Sprintf("record-%d", i)] = usage
				}
			}
		}
	} else {
		a.Log.Error("restore usage failed", map[string]any{"sessionId": sessionID, "error": err.Error()})
	}
	// Current() returns a clone; the mutations above are only kept if written back.
	a.Coord.Restore(s)
	if cache != nil {
		for _, row := range cache.Agents {
			a.Coord.Cache.Add(row.AgentID, row.Generation, row.Model, row.Usage)
		}
	}
	if raws, err := a.Store.List("generations", sessionID); err == nil {
		for _, raw := range raws {
			var generation ctxeng.Generation
			if json.Unmarshal(raw, &generation) == nil {
				a.Coord.Engine.RestoreGeneration(generation)
			}
		}
	} else {
		a.Log.Error("restore generations failed", map[string]any{"sessionId": sessionID, "error": err.Error()})
	}
}

func cachePayload(snapshot metrics.CacheSnapshot) map[string]any {
	row := func(v metrics.CacheRow) map[string]any {
		return map[string]any{"hitRate": v.HitRate, "input": v.Usage.Input, "output": v.Usage.Output, "cacheRead": v.Usage.CacheRead, "cacheWrite": v.Usage.CacheWrite}
	}
	agents := make([]map[string]any, 0, len(snapshot.Agents))
	for _, v := range snapshot.Agents {
		agents = append(agents, map[string]any{"agentId": v.AgentID, "generation": v.Generation, "model": v.Model, "input": v.Usage.Input, "output": v.Usage.Output, "cacheRead": v.Usage.CacheRead, "cacheWrite": v.Usage.CacheWrite, "hitRate": v.HitRate})
	}
	return map[string]any{"session": row(snapshot.Session), "agents": agents}
}

func (a *App) Close() {
	a.once.Do(func() {
		a.stopAutoApply()
		a.stopPersistTimer()
		a.persist()
		a.Coord.StopAll(context.Background())
		a.Coord.CloseMessages()
		if ws := a.Coord.WS; ws != nil {
			_ = ws.Close()
		}
		a.procMu.Lock()
		a.swapProcLocked(nil)
		a.procMu.Unlock()
		if rt := a.Coord.RuntimeSnapshot(); rt != nil {
			if err := rt.Close(); err != nil {
				a.Log.Warn("close runtime failed", map[string]any{"error": err.Error()})
			}
		}
		if err := a.Store.Close(); err != nil {
			a.Log.Error("close store failed", map[string]any{"error": err.Error()})
		}
	})
}

func parseEfficiencyPrefs(m map[string]any) style.Prefs {
	p := style.Prefs{}
	if v, ok := m["preset"].(string); ok {
		p.Preset = v
	}
	if v, ok := m["voice"].(string); ok {
		p.Voice = v
	}
	if v, ok := m["craft"].(string); ok {
		p.Craft = v
	}
	if v, ok := m["planLean"].(bool); ok {
		p.PlanLean = v
	}
	if n, ok := m["reportMaxChars"].(float64); ok {
		p.ReportMaxChars = int(n)
	}
	if n, ok := m["reportMaxChars"].(int); ok {
		p.ReportMaxChars = n
	}
	if r, ok := m["roles"].(map[string]any); ok {
		p.Roles = map[string]style.RolePref{}
		for k, v := range r {
			rm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			rp := style.RolePref{}
			if vv, ok := rm["voice"].(string); ok {
				rp.Voice = vv
			}
			if cv, ok := rm["craft"].(string); ok {
				rp.Craft = cv
			}
			p.Roles[k] = rp
		}
	}
	return p
}

// canvasFlag reads the extension's Canvas capability from the initialize payload. Only an explicit true enables it.
func canvasFlag(p map[string]any) bool {
	if v, ok := p["canvas"].(bool); ok {
		return v
	}
	if caps, ok := p["capabilities"].(map[string]any); ok {
		v, _ := caps["canvas"].(bool)
		return v
	}
	return false
}

func str(m map[string]any, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

// Limits for path-glob settings from the extension (relo.changes.*Paths). Entries past them are dropped.
const (
	maxPathEntries = 100
	maxPathChars   = 256
)

// pathList returns the usable strings of the array at m[k]: trimmed, non-empty, at most maxPathChars characters,
// and at most maxPathEntries of them. ok is false when the key is absent or not an array, so the caller keeps its
// current value; an empty array is ok and means none.
func pathList(m map[string]any, k string) (list []string, ok bool) {
	var raw []any
	switch v := m[k].(type) {
	case []any:
		raw = v
	case []string:
		for _, s := range v {
			raw = append(raw, s)
		}
	default:
		return nil, false
	}
	list = []string{}
	for _, item := range raw {
		if len(list) == maxPathEntries {
			break
		}
		s, isString := item.(string)
		if !isString {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" || utf8.RuneCountInString(s) > maxPathChars {
			continue
		}
		list = append(list, s)
	}
	return list, true
}

func num(m map[string]any, k string, def float64) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func memFrom(it ctxeng.Item) memory.Entry {
	return memory.Entry{ID: id.New("mem"), Kind: it.Kind, Title: it.Title, Body: it.Content, Files: it.Files, Verified: it.Verified, Score: it.Durability, Source: string(it.Source)}
}

func memFromParams(p map[string]any) memory.Entry {
	files, _ := p["files"].([]any)
	var fs []string
	for _, f := range files {
		fs = append(fs, fmt.Sprint(f))
	}
	return memory.Entry{ID: str(p, "id", ""), Kind: str(p, "kind", "note"), Title: str(p, "title", ""), Body: str(p, "body", ""), Files: fs, Verified: false}
}
