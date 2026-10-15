package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/id"
	"github.com/relocode/relo/internal/metrics"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/style"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
)

// Coord.mu protects session contents, pending permissions, and run lifecycle.
// configMu protects runtime and configuration fields; snapshots precede external calls.
func (c *Coord) SetRuntime(rt runtime.AgentRuntime) {
	c.configMu.Lock()
	c.Runtime = rt
	c.configMu.Unlock()
}

func (c *Coord) RuntimeSnapshot() runtime.AgentRuntime {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.Runtime
}

func (c *Coord) SetEmitter(fn func(string, any)) {
	c.emitMu.Lock()
	c.emitFn = fn
	c.emitMu.Unlock()
}

func (c *Coord) SetEmitterIfUnset(fn func(string, any)) {
	c.emitMu.Lock()
	if c.emitFn == nil {
		c.emitFn = fn
	}
	c.emitMu.Unlock()
}

func (c *Coord) Configure(settings Settings, sched *scheduler.Scheduler, route routing.Config) {
	c.configMu.Lock()
	if sched == nil {
		sched = scheduler.New(scheduler.Limits{})
	}
	if !settings.EscalateSet && !settings.Escalate {
		settings.Escalate = true
	}
	if settings.ReportMaxChars <= 0 {
		settings.ReportMaxChars = 1200
	}
	if settings.Efficiency.ReportMaxChars <= 0 {
		settings.Efficiency.ReportMaxChars = settings.ReportMaxChars
	}
	c.Settings, c.Sched, c.Route = settings, sched, route
	c.configMu.Unlock()
}

func cloneSession(s *Session) *Session {
	if s == nil {
		return nil
	}
	out := *s
	out.Messages = append([]Chat(nil), s.Messages...)
	out.Changes = append([]Change(nil), s.Changes...)
	out.Verify = append([]verification.Result(nil), s.Verify...)
	if s.Usage != nil {
		out.Usage = make(map[string]any, len(s.Usage))
		for k, v := range s.Usage {
			out.Usage[k] = v
		}
	}
	return &out
}

func (c *Coord) Current() *Session { c.mu.Lock(); defer c.mu.Unlock(); return cloneSession(c.sess) }

func (c *Coord) CurrentMode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil || c.sess.Mode == "" {
		return ModeBuild
	}
	return c.sess.Mode
}

func (c *Coord) CreateSession(workspace, objective string, mode Mode) *Session {
	c.runtimeMu.Lock()
	var oldSessions []*runtimeSession
	for id, session := range c.runtimeSessions {
		oldSessions = append(oldSessions, session)
		delete(c.runtimeSessions, id)
	}
	c.runtimeMu.Unlock()
	for _, session := range oldSessions {
		c.closeRuntimeSession(c.RuntimeSnapshot(), session)
	}
	c.mu.Lock()
	if c.runCancel != nil {
		c.runCancel()
		c.runCancel = nil
		c.sessionCtx = nil
	}
	s := &Session{ID: id.New("sess"), Workspace: workspace, Objective: objective, Mode: mode, Status: "active", Created: time.Now().UTC(), Updated: time.Now().UTC(), Usage: map[string]any{}}
	c.sess, c.reviewed = s, map[string]bool{}
	c.Graph, c.Agents, c.Engine = taskgraph.New(), c.newRegistry(), ctxeng.New()
	c.baseline, c.tasksPlanned = nil, 0
	c.mu.Unlock()
	c.emit("session.created", s)
	return cloneSession(s)
}

// OpenSession switches to a stored session: it stops whatever is running, drops all live state (agents, tasks,
// context) and installs s. Callers reload the per-session records afterwards.
func (c *Coord) OpenSession(ctx context.Context, s *Session) {
	c.StopAll(ctx)
	c.runtimeMu.Lock()
	var old []*runtimeSession
	for id, session := range c.runtimeSessions {
		old = append(old, session)
		delete(c.runtimeSessions, id)
	}
	c.runtimeMu.Unlock()
	for _, session := range old {
		c.closeRuntimeSession(c.RuntimeSnapshot(), session)
	}
	c.mu.Lock()
	c.runCancel, c.sessionCtx = nil, nil
	c.sess, c.reviewed = cloneSession(s), map[string]bool{}
	if c.sess != nil {
		c.sess.Running = false
		if c.sess.Usage == nil {
			c.sess.Usage = map[string]any{}
		}
	}
	c.Graph, c.Agents, c.Engine = taskgraph.New(), c.newRegistry(), ctxeng.New()
	c.baseline, c.tasksPlanned = nil, 0
	opened := cloneSession(c.sess)
	c.mu.Unlock()
	c.emit("session.opened", opened)
}

// Restore installs a persisted session. Nothing is running after a restart, whatever the stored flag says.
func (c *Coord) Restore(s *Session) {
	c.mu.Lock()
	c.sess = cloneSession(s)
	if c.sess != nil {
		c.sess.Running = false
	}
	c.mu.Unlock()
}

func (c *Coord) SetMode(mode Mode) {
	c.mu.Lock()
	if c.sess != nil {
		c.sess.Mode = mode
	}
	s := cloneSession(c.sess)
	c.mu.Unlock()
	c.emit("session.updated", s)
}

func (c *Coord) Pause() {
	c.scheduler().Pause()
	c.mu.Lock()
	if c.sess != nil {
		c.sess.Paused, c.sess.Status = true, "paused"
	}
	s := cloneSession(c.sess)
	c.mu.Unlock()
	c.emit("session.updated", s)
}

func (c *Coord) Resume() {
	c.scheduler().Resume()
	c.mu.Lock()
	if c.sess != nil {
		c.sess.Paused, c.sess.Status = false, "active"
	}
	s := cloneSession(c.sess)
	c.mu.Unlock()
	c.emit("session.updated", s)
}

func (c *Coord) StopAll(ctx context.Context) {
	c.mu.Lock()
	cancel := c.runCancel
	c.mu.Unlock()
	rt := c.RuntimeSnapshot()
	if cancel != nil {
		cancel()
	}
	// Detach tracked runtime sessions so each is cancelled and closed exactly once.
	c.runtimeMu.Lock()
	var sessions []*runtimeSession
	for id, session := range c.runtimeSessions {
		sessions = append(sessions, session)
		delete(c.runtimeSessions, id)
	}
	c.runtimeMu.Unlock()
	// Cancel only agents that are still active: a repeat StopAll finds them terminal and skips them.
	seen := map[runtime.SessionID]bool{}
	var targets []runtime.SessionID
	for _, session := range sessions {
		if !seen[session.id] {
			seen[session.id] = true
			targets = append(targets, session.id)
		}
	}
	for _, a := range c.Agents.List() {
		sid := runtime.SessionID(a.RuntimeID)
		if a.RuntimeID == "" || seen[sid] || !activeStatus(a.Status) {
			continue
		}
		seen[sid] = true
		targets = append(targets, sid)
	}
	errs := make(chan error, len(targets))
	var wg sync.WaitGroup
	for _, sid := range targets {
		if rt == nil {
			break
		}
		wg.Add(1)
		go func(id runtime.SessionID) {
			defer wg.Done()
			if err := rt.Cancel(ctx, id); err != nil {
				errs <- err
			}
		}(sid)
	}
	wg.Wait()
	for _, session := range sessions {
		c.closeRuntimeSession(rt, session)
	}
	for _, a := range c.Agents.List() {
		if !activeStatus(a.Status) {
			continue
		}
		c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Cancelled; x.Activity = "Stopped" })
		c.emit("agent.failed", c.freshAgent(a))
	}
	close(errs)
	for err := range errs {
		c.logGone("cancel runtime session", err)
	}
	c.runWG.Wait()
}

// Busy reports whether any agent is queued, starting, running or waiting. Restarting the runtime while busy
// would orphan their sessions, so callers defer such work.
func (c *Coord) Busy() bool {
	for _, a := range c.Agents.List() {
		if activeStatus(a.Status) {
			return true
		}
	}
	return false
}

// Idle reports that nothing is running and nothing is about to: no active agent, and no task waiting for one.
func (c *Coord) Idle() bool {
	if c.Busy() {
		return false
	}
	for _, t := range c.Graph.List() {
		if t.Status == taskgraph.Queued || t.Status == taskgraph.Running {
			return false
		}
	}
	return true
}

func activeStatus(s agent.Status) bool {
	return s == agent.Queued || s == agent.Starting || s == agent.Running || s == agent.Waiting
}

// modelProblem explains why Primary cannot run, or "" when it can. It only judges against a catalog that was
// actually loaded; before that, nothing is refused. A loaded but empty catalog means no provider is set up.
func (s Settings) modelProblem() string {
	if !s.ModelsKnown {
		return ""
	}
	if len(s.Models) == 0 {
		return "No models are available. Add a provider under Settings, Providers, then pick your models under Settings, Models."
	}
	m := strings.TrimSpace(s.PrimaryModel)
	if m == "" {
		return "No Primary model is selected. Pick one in Settings, Models."
	}
	if _, ok := s.Models[m]; !ok {
		return fmt.Sprintf("Primary model %q is not available. It may belong to a provider that was removed. Pick another in Settings, Models, or add the provider in Settings, Providers.", m)
	}
	return ""
}

func (c *Coord) Submit(ctx context.Context, text string, refs []string) error {
	if c.RuntimeSnapshot() == nil {
		err := fmt.Errorf("runtime is not ready")
		c.chat("system", "OpenCode is not ready yet. Wait for the backend to connect, then try again.", "")
		return err
	}
	if msg := c.settings().modelProblem(); msg != "" {
		c.chat("system", msg, "")
		return fmt.Errorf("%s", msg)
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		cancel()
		return fmt.Errorf("no session")
	}
	if c.runCancel != nil {
		c.mu.Unlock()
		cancel()
		c.chat("system", "Still working on the previous request. Stop it first, or wait for it to finish.", "")
		return fmt.Errorf("session already running")
	}
	if c.sess.Objective == "" {
		c.sess.Objective = text
	}
	c.runID++
	runID := c.runID
	c.replans = 0
	c.runCancel, c.sessionCtx = cancel, runCtx
	c.runWG.Add(1)
	c.sess.Running = true
	c.addChatLocked(Chat{Role: "user", Text: text})
	user := c.sess.Messages[len(c.sess.Messages)-1]
	started := cloneSession(c.sess)
	c.mu.Unlock()
	defer c.runWG.Done()
	// The user's own message must reach the UI at once, not on the next snapshot.
	c.emit("chat.message", user)
	c.emit("session.updated", started)
	c.refreshSnapshot()
	primary := c.ensurePrimary()
	c.Agents.Update(primary.ID, func(x *agent.Agent) {
		x.Status, x.Phase, x.Activity = agent.Running, "context", "Building context"
	})
	c.seedPrimary(text, refs)
	err := c.guard("primary", func() error { return c.loopPrimary(runCtx, primary, text) })
	c.reportFailure(primary, "Primary failed", err)
	c.settlePrimary(primary.ID, runCtx, err)
	cancel()
	c.mu.Lock()
	var finished *Session
	if c.runID == runID {
		c.runCancel, c.sessionCtx = nil, nil
		for id, p := range c.pending {
			select {
			case p.Reply <- "reject":
			default:
			}
			delete(c.pending, id)
		}
		if c.sess != nil {
			c.sess.Running = false
			finished = cloneSession(c.sess)
		}
	}
	c.mu.Unlock()
	if finished != nil {
		c.emit("session.updated", finished)
	}
	return err
}

// settlePrimary ends the Primary's run. Without it the agent would stay "running" after a successful turn, which
// blocks Busy (and so deferred restarts) and leaves the Stop button lit forever.
func (c *Coord) settlePrimary(id string, ctx context.Context, err error) {
	cur, ok := c.Agents.Get(id)
	if !ok || agent.Terminal(cur.Status) {
		return // StopAll or fail() already ended it
	}
	switch {
	case err == nil:
		c.Agents.Update(id, func(x *agent.Agent) { x.Status, x.Activity = agent.Completed, "Done" })
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		c.fail(id, context.Canceled)
	default:
		c.fail(id, err)
	}
}

func (c *Coord) Snapshot() map[string]any {
	c.mu.Lock()
	s := cloneSession(c.sess)
	c.mu.Unlock()
	messages, changes, verify := []Chat{}, []Change{}, []verification.Result{}
	if s != nil {
		messages, changes, verify = nonNil(s.Messages), nonNil(s.Changes), nonNil(s.Verify)
	}
	settings := c.settings()
	profiles := map[string]string{}
	seen := map[string]bool{}
	for _, ag := range c.Agents.List() {
		role := string(ag.Role)
		if seen[role] {
			continue
		}
		seen[role] = true
		res := style.Resolve(settings.Efficiency, ag.Role, "medium", ag.Attempt, ag.Budget)
		profiles[role] = res.Profile.ID()
	}
	preset := strings.TrimSpace(settings.Efficiency.Preset)
	if preset == "" {
		preset = "off"
	}
	return map[string]any{"session": s, "agents": nonNil(c.Agents.List()), "tasks": nonNil(c.Graph.List()), "findings": nonNil(c.Engine.Findings()), "memory": nonNil(c.Memory.List()), "messages": messages, "changes": changes, "verification": verify, "cache": c.CacheSnapshot(), "efficiency": map[string]any{"preset": preset, "profiles": profiles}, "routing": map[string]any{"mode": routing.NormalizeMode(settings.RoutingMode), "contextLimit": settings.ContextLimit}}
}

// nonNil keeps empty lists as [] on the wire: a null would look to the UI like "no update", and stale rows would stay.
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

func (c *Coord) CacheSnapshot() metrics.CacheSnapshot {
	c.cacheMu.RLock()
	cache := c.Cache
	c.cacheMu.RUnlock()
	if cache == nil {
		return metrics.CacheSnapshot{}
	}
	return cache.Snapshot()
}

func (c *Coord) sessMessages() []Chat {
	if s := c.Current(); s != nil {
		return s.Messages
	}
	return nil
}
func (c *Coord) sessChanges() []Change {
	if s := c.Current(); s != nil {
		return s.Changes
	}
	return nil
}
func (c *Coord) sessVerify() []verification.Result {
	if s := c.Current(); s != nil {
		return s.Verify
	}
	return nil
}

func (c *Coord) chat(role, text, agentID string) {
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return
	}
	c.addChatLocked(Chat{Role: role, Text: text, AgentID: agentID})
	m := c.sess.Messages[len(c.sess.Messages)-1]
	c.mu.Unlock()
	c.emit("chat.message", m)
}

func (c *Coord) addChatLocked(m Chat) {
	if m.ID == "" {
		m.ID = id.New("msg")
	}
	if m.At == "" {
		m.At = time.Now().UTC().Format(time.RFC3339)
	}
	c.sess.Messages = append(c.sess.Messages, m)
	c.sess.Updated = time.Now().UTC()
}

// fail ends an agent's run. A cancelled context is the user pressing Stop, not an error: it shows as stopped.
func (c *Coord) fail(id string, err error) {
	status, label := agent.Failed, err.Error()
	if errors.Is(err, context.Canceled) {
		status, label = agent.Cancelled, "Stopped"
	}
	updated, ok := c.Agents.Update(id, func(x *agent.Agent) { x.Status = status; x.Activity = label })
	if ok {
		c.emit("agent.failed", updated)
	} else {
		c.emit("agent.failed", map[string]any{"id": id, "status": string(status), "error": label})
	}
	if c.Log != nil && status == agent.Failed {
		c.Log.Error("agent failed", map[string]any{"agent_id": id, "error": err.Error()})
	}
}

// surfaced marks an error whose cause the user was already told about in chat, so it is not reported twice.
type surfaced struct{ error }

func (e surfaced) Unwrap() error { return e.error }

// reportFailure tells the user why a Primary turn failed, unless it was a Stop or was already shown.
func (c *Coord) reportFailure(primary agent.Agent, what string, err error) {
	var s surfaced
	if err == nil || errors.Is(err, context.Canceled) || errors.As(err, &s) {
		return
	}
	c.chat("error", what+": "+err.Error(), primary.ID)
}

func (c *Coord) ensurePrimary() agent.Agent {
	if a, ok := c.Agents.Primary(); ok {
		return a
	}
	s := c.Current()
	settings := c.settings()
	a := c.Agents.Create(agent.Agent{Role: agent.Primary, Title: "Primary", Status: agent.Queued, Model: settings.PrimaryModel, Scope: "orchestration", Workspace: s.Workspace, Budget: settings.BudgetFor(settings.PrimaryModel, settings.Budget), Window: settings.Window(settings.PrimaryModel)})
	c.Engine.Bind(a.ID, "", string(agent.Primary), a.Budget)
	c.Engine.SetStrategy(a.ID, settings.Strategy)
	c.emit("agent.created", a)
	return a
}

func (c *Coord) seedPrimary(text string, refs []string) {
	settings := c.settings()
	s := c.Current()
	if s == nil {
		return
	}
	items := []ctxeng.Item{{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Objective", Content: s.Objective, Durability: 1, Relevance: 1, Pinned: true}, {Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Latest user message", Content: text, Durability: 0.9, Relevance: 1}}
	if s.Mode != "" {
		items = append(items, ctxeng.Item{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Kind: ctxeng.KindConstraint, Title: "Mode", Content: "Active mode: " + string(s.Mode) + ". Plan/review modes must not mutate the repo.", Pinned: true, Durability: 1})
	}
	for _, ref := range refs {
		items = append(items, ctxeng.Item{Class: ctxeng.ClassCode, Source: ctxeng.SourceRepo, Content: ref, Durability: 0.6})
	}
	if settings.Memory {
		for _, e := range c.Memory.Recall(text) {
			items = append(items, ctxeng.Item{Class: ctxeng.ClassMemory, Source: ctxeng.SourceVerified, Title: staleMemory + e.Title, Content: e.Body, Files: e.Files, Verified: e.Verified, Durability: 0.8})
		}
	}
	p, _ := c.Agents.Primary()
	c.Engine.Ingest(p.ID, items)
}

func (c *Coord) loopPrimary(ctx context.Context, primary agent.Agent, text string) error {
	settings := c.settings()
	s := c.Current()
	if s == nil {
		return fmt.Errorf("no session")
	}
	resolvedPrimary := style.Resolve(settings.Efficiency, primary.Role, primary.Difficulty, primary.Attempt, settings.Budget)
	primaryBudget := settings.Budget
	if resolvedPrimary.Scope.Budget != 0 {
		primaryBudget = resolvedPrimary.Scope.Budget
	}
	primaryBudget = settings.BudgetFor(primary.Model, primaryBudget)
	gen, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: primary.ID, Role: string(agent.Primary), Phase: ctxeng.PhaseDesign, Budget: primaryBudget, Reason: "primary turn", Query: ctxeng.Query{Task: s.Objective, Keywords: strings.Fields(s.Objective)}})
	if err != nil {
		return err
	}
	c.emit("context.created", map[string]any{"agentId": primary.ID, "generation": gen.Number, "tokens": gen.Tokens})
	extra := c.workspaceFacts() + "\nUser message:\n" + text
	if line := style.PlanLeanLine(settings.Efficiency); line != "" {
		extra += "\n" + line
	}
	parts := prompt.ComposePartsWith(primary.Role, *gen, extra, resolvedPrimary.Profile.Fragment(primary.Role))
	turn, err := c.primaryTurn(ctx, primary, parts, "Plan the request and return the structured turn.")
	c.closeAgentSession(primary.ID)
	if err != nil {
		return err
	}
	if turn.UserMessage != "" {
		c.chat("assistant", turn.UserMessage, primary.ID)
	}
	if turn.Objective != "" {
		c.mu.Lock()
		if c.sess != nil {
			c.sess.Objective = turn.Objective
		}
		c.mu.Unlock()
	}
	c.applyTurn(primary, turn)
	if len(turn.Tasks) > 0 {
		c.primaryWaits()
	}
	if turn.RefreshSelf {
		if _, err := c.Engine.Refresh(primary.ID, "primary requested refresh"); err != nil {
			c.logWarn("refresh primary context", err)
		} else {
			c.emit("context.refreshed", map[string]any{"agentId": primary.ID, "reason": "primary requested refresh"})
		}
	}
	if err := c.drive(ctx); err != nil {
		return err
	}
	if turn.Status == "completed" && len(c.Graph.Runnable()) == 0 {
		c.mu.Lock()
		if c.sess != nil {
			c.sess.Status = "completed"
		}
		s := cloneSession(c.sess)
		c.mu.Unlock()
		c.emit("session.updated", s)
	}
	return nil
}

// primaryWaits shows Primary as waiting on its sub-agents rather than as a model that is still thinking.
func (c *Coord) primaryWaits() {
	p, ok := c.Agents.Primary()
	if !ok || agent.Terminal(p.Status) {
		return
	}
	c.Agents.Update(p.ID, func(x *agent.Agent) {
		x.Status, x.Phase, x.Activity = agent.Waiting, "waiting", "Coordinating sub-agents"
	})
}

func (c *Coord) applyTurn(primary agent.Agent, turn Turn) {
	settings := c.settings()
	s := c.Current()
	if s == nil {
		return
	}
	idmap := map[string]string{}
	turn.Tasks = c.admitTasks(turn.Tasks)
	for _, spec := range turn.Tasks {
		role := parseRole(spec.Role)
		t := c.Graph.Add(taskgraph.Task{Title: spec.Title, Description: spec.Description, Acceptance: spec.Acceptance, Creator: primary.ID, Priority: spec.Priority, Role: string(role), Workspace: s.Workspace, Difficulty: spec.Difficulty, MaxAttempts: settings.MaxRetries, Model: c.modelForDifficulty(role, spec.Difficulty)})
		idmap[spec.ID] = t.ID
		c.emit("task.created", t)
	}
	for _, spec := range turn.Tasks {
		real := idmap[spec.ID]
		var deps []string
		for _, dep := range spec.Deps {
			if idmap[dep] != "" {
				deps = append(deps, idmap[dep])
			} else if c.knownTask(dep) {
				deps = append(deps, dep)
			}
		}
		if _, err := c.Graph.SetDependencies(real, deps); err != nil {
			c.logWarn("set task dependencies", err)
		}
	}
	for _, d := range turn.Decisions {
		c.Engine.Ingest(primary.ID, []ctxeng.Item{{Class: ctxeng.ClassTask, Source: ctxeng.SourceVerified, Title: "Decision", Content: d, Verified: true, Durability: 0.9}})
	}
}

// maxPlannedTasks bounds how much work one session can fan out into. A model that keeps re-planning would otherwise
// spend tokens without limit.
const maxPlannedTasks = 40

// admitTasks drops tasks that repeat work already planned (same title and role, not failed) and stops adding once
// the session has planned maxPlannedTasks. What was dropped is said in chat, once.
func (c *Coord) admitTasks(specs []TurnTask) []TurnTask {
	if len(specs) == 0 {
		return specs
	}
	seen := map[string]bool{}
	for _, t := range c.Graph.List() {
		if t.Status != taskgraph.Failed && t.Status != taskgraph.Canceled {
			seen[taskKey(t.Title, t.Role)] = true
		}
	}
	var kept []TurnTask
	dupes, capped := 0, 0
	c.mu.Lock()
	for _, spec := range specs {
		key := taskKey(spec.Title, string(parseRole(spec.Role)))
		switch {
		case seen[key]:
			dupes++
		case c.tasksPlanned >= maxPlannedTasks:
			capped++
		default:
			seen[key] = true
			c.tasksPlanned++
			kept = append(kept, spec)
		}
	}
	c.mu.Unlock()
	if dupes > 0 {
		c.logWarn("planner repeated tasks", fmt.Errorf("dropped %d duplicate task(s)", dupes))
	}
	if capped > 0 {
		c.chat("system", fmt.Sprintf("Relo stopped planning more work: this session already has %d tasks. Start a new session or narrow the request.", maxPlannedTasks), "")
	}
	// Dependencies on a dropped duplicate point at the original task of the same name.
	return kept
}

func taskKey(title, role string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " ")) + "|" + strings.ToLower(role)
}

func (c *Coord) primaryTurn(ctx context.Context, primary agent.Agent, parts prompt.Parts, ask string) (Turn, error) {
	body, err := c.runAgent(ctx, primary, parts, ask, prompt.PrimarySchema)
	if err != nil {
		return Turn{}, err
	}
	turn, parseErr := ParseTurn(body, c.knownTask)
	if parseErr == nil {
		return turn, nil
	}
	c.emit("chat.discard", map[string]any{"agentId": primary.ID}) // the bubble streamed from output that did not validate
	// Prose with no JSON in it is just an answer: show it. Tool-call markup is not prose; it is the model trying to
	// call something it does not have. Only malformed JSON or such an attempt is worth a (paid) repair round.
	prose, strayCall := stripToolMarkup(body)
	if !strings.Contains(prose, "{") && prose != "" {
		return Turn{Status: "completed", UserMessage: prose}, nil
	}
	repair := "Previous output failed validation: " + parseErr.Error() + ". Return corrected JSON only."
	if strayCall {
		repair = "Your last reply was a tool call. You have no tools and nothing was run. Reply with the JSON turn only: what was done in userMessage, and tasks [] when nothing is left to do."
	}
	body, err = c.runAgent(ctx, primary, parts, repair, prompt.PrimarySchema)
	if err != nil {
		return Turn{}, err
	}
	turn, parseErr = ParseTurn(body, c.knownTask)
	if parseErr != nil {
		c.logWarn("parse primary turn", parseErr)
		c.emit("chat.discard", map[string]any{"agentId": primary.ID})
		// Show what the model said rather than an error about its formatting; the user can ask again.
		if text, _ := stripToolMarkup(body); text != "" && !strings.HasPrefix(text, "{") {
			return Turn{Status: "completed", UserMessage: text}, nil
		}
		// The work was done whatever the Primary managed to say about it: report what the workers reported.
		if done := c.workDone(); done != "" {
			return Turn{Status: "completed", UserMessage: done}, nil
		}
		err = fmt.Errorf("invalid primary turn after one repair: %w", parseErr)
		c.chat("error", "The model's plan could not be read ("+parseErr.Error()+"). Try again, or pick a different Primary model in Settings.", primary.ID)
		return Turn{}, surfaced{err}
	}
	return turn, nil
}

// workDone is what the most recently finished tasks reported, in their own words, for when the Primary cannot be made
// to write the answer itself.
func (c *Coord) workDone() string {
	var done []taskgraph.Task
	for _, t := range c.Graph.List() {
		if rep, ok := t.Report.(agent.Report); ok && t.Status == taskgraph.Done && strings.TrimSpace(rep.Summary) != "" {
			done = append(done, t)
		}
	}
	sort.SliceStable(done, func(i, j int) bool { return done[i].Finished.Before(done[j].Finished) })
	if len(done) > 4 {
		done = done[len(done)-4:]
	}
	if len(done) == 0 {
		return ""
	}
	lines := []string{"Relo's planner did not write a summary, so these are the finished tasks' own reports:"}
	for _, t := range done {
		lines = append(lines, "- "+t.Title+": "+strings.TrimSpace(t.Report.(agent.Report).Summary))
	}
	return strings.Join(lines, "\n")
}

func (c *Coord) knownTask(taskID string) bool {
	for _, task := range c.Graph.List() {
		if task.ID == taskID {
			return true
		}
	}
	return false
}

func (c *Coord) drive(ctx context.Context) error {
	sched := c.scheduler()
	for i := 0; i < maxDriveRounds; i++ {
		if err := sched.Wait(ctx); err != nil {
			return err
		}
		tasks := sched.Next(c.Graph, 4)
		if len(tasks) == 0 {
			c.cancelOrphans()
			return nil
		}
		c.refreshSnapshot()
		c.primaryWaits()
		var wg sync.WaitGroup
		errs := make(chan error, len(tasks))
		started := 0
		for _, task := range tasks {
			if err := sched.Wait(ctx); err != nil {
				break
			}
			if ok, _ := sched.CanReserve(c.Agents, agent.Agent{Role: parseRole(task.Role), Depth: 1}); !ok {
				break
			}
			a := c.spawn(task, parseRole(task.Role))
			release, ok, why := sched.Reserve(c.Agents, a)
			if !ok {
				c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Queued; x.Activity = why })
				continue
			}
			updated, err := c.Graph.Update(task.ID, func(x *taskgraph.Task) {
				x.Status = taskgraph.Running
				x.AgentID = a.ID
				x.Started = time.Now().UTC()
				x.Attempt++
				x.Attempts++
			})
			if err != nil {
				release()
				c.logWarn("mark task running", err)
				continue
			}
			c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Starting; x.Phase = "context"; x.Activity = "Starting" })
			c.Agents.Update(a.ID, func(x *agent.Agent) { x.Attempt = updated.Attempt })
			c.mu.Lock()
			delete(c.escalations, task.ID)
			c.mu.Unlock()
			c.emit("task.updated", updated)
			wg.Add(1)
			started++
			go func(t taskgraph.Task, ag agent.Agent) {
				defer wg.Done()
				defer sched.Unreserve(ag.ID)
				err := c.guard("worker "+t.ID, func() error { return c.execWorker(ctx, t, ag) })
				if err != nil && strings.HasPrefix(err.Error(), "internal error") {
					// A panic skipped the worker's own bookkeeping: close the task here so the run can end.
					c.fail(ag.ID, err)
					c.failTask(t, ag, err)
				}
				c.Agents.Update(ag.ID, func(x *agent.Agent) {
					if err != nil && ctx.Err() != nil {
						x.Status, x.Activity = agent.Cancelled, "Stopped"
					} else if err != nil {
						x.Status, x.Activity = agent.Failed, err.Error()
					}
				})
				if err != nil {
					errs <- err
				}
			}(updated, a)
		}
		if started == 0 {
			if sched.Paused() {
				continue
			}
			c.cancelOrphans()
			return nil
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && ctx.Err() == nil {
				c.logWarn("worker", err)
			}
		}
		c.cancelOrphans()
		c.repromptPrimary(ctx)
	}
	c.chat("system", fmt.Sprintf("Relo stopped after %d rounds of work. Ask it to continue if more is needed.", maxDriveRounds), "")
	return nil
}

// maxDriveRounds bounds the plan, run, re-plan loop of one request.
const maxDriveRounds = 10

// cancelOrphans retires tasks that can never run because something they depend on failed for good. Left Blocked
// they would keep Primary from ever being asked to wrap up, and the user would see work silently stop.
func (c *Coord) cancelOrphans() {
	for _, t := range c.Graph.List() {
		if t.Status != taskgraph.Blocked {
			continue
		}
		for _, dep := range t.Depends {
			if d, ok := c.Graph.Get(dep); ok && (d.Status == taskgraph.Failed || d.Status == taskgraph.Canceled) {
				updated, err := c.Graph.Update(t.ID, func(x *taskgraph.Task) {
					x.Status = taskgraph.Canceled
					x.Finished = time.Now().UTC()
				})
				if err == nil {
					c.emit("task.updated", updated)
				}
				break
			}
		}
	}
}

// maxReplans is how many times Primary may be asked to plan again after workers finish. The last of them only
// reports: a Primary that keeps finding something to double-check never finishes, and each round costs a full set
// of agents.
const maxReplans = 3

func (c *Coord) repromptPrimary(ctx context.Context) {
	settings := c.settings()
	p, ok := c.Agents.Primary()
	if !ok || !c.settled() {
		return
	}
	c.mu.Lock()
	c.replans++
	final := c.replans >= maxReplans
	c.mu.Unlock()
	gen, err := c.Engine.Refresh(p.ID, "collapse completed worker reports")
	if err != nil {
		c.logWarn("refresh primary after workers", err)
		return
	}
	c.emit("context.compressed", map[string]any{"agentId": p.ID, "generation": gen.Number})
	resolvedPrimary := style.Resolve(settings.Efficiency, p.Role, p.Difficulty, p.Attempt, settings.Budget)
	extra := c.workspaceFacts() + "\nAll scheduled workers finished. If every task is done and no check failed, put what was done in userMessage and return tasks as []. Plan follow-up work only for a task that failed or an acceptance criterion that is still unmet."
	if final {
		extra += " This is the last planning turn of this request: return tasks as [] and tell the user what is done and what remains."
	}
	parts := prompt.ComposePartsWith(p.Role, *gen, extra, resolvedPrimary.Profile.Fragment(p.Role))
	turn, err := c.primaryTurn(ctx, p, parts, "Integrate worker reports and return the structured turn.")
	c.closeAgentSession(p.ID)
	if err != nil {
		c.logWarn("reprompt primary", err)
		c.reportFailure(p, "Primary could not integrate the results", err)
		return
	}
	if turn.UserMessage != "" {
		c.chat("assistant", turn.UserMessage, p.ID)
	}
	if final && len(turn.Tasks) > 0 {
		turn.Tasks = nil
		c.chat("system", fmt.Sprintf("Relo stopped planning more work after %d follow-up rounds. Ask it to continue if more is needed.", maxReplans), "")
	}
	c.applyTurn(p, turn)
}

func (c *Coord) nextAttempt(t taskgraph.Task, a agent.Agent) (string, bool) {
	model := a.Model
	if c.settings().Escalate && t.Attempt >= 2 {
		if next, ok := c.routeConfig().Escalate(a.Model); ok {
			model = next
		}
	}
	return model, t.Attempt <= c.scheduler().Limits.MaxRetry
}

func (c *Coord) retryTask(t taskgraph.Task, a agent.Agent) {
	model, retry := c.nextAttempt(t, a)
	if !retry {
		c.chat("system", fmt.Sprintf("%q failed after %s and was not retried again. Ask Relo to try a different approach.%s", t.Title, plural(t.Attempt, "attempt"), c.keptNote(t, a)), "")
		return
	}
	from := a.Model
	updated, err := c.Graph.Update(t.ID, func(x *taskgraph.Task) {
		x.Status, x.AgentID, x.Model = taskgraph.Queued, "", model
		x.Started, x.Finished = time.Time{}, time.Time{}
	})
	if err != nil {
		c.logWarn("retry failed task", err)
		return
	}
	if model != from {
		c.mu.Lock()
		if c.escalations == nil {
			c.escalations = map[string]string{}
		}
		c.escalations[t.ID] = from
		c.mu.Unlock()
		c.emit("agent.escalated", map[string]any{"taskId": t.ID, "from": from, "to": model, "attempt": t.Attempt + 1, "reason": "repeated attempt failed; escalating model"})
	}
	c.emit("task.updated", updated)
}

// Automatic review is for work big or hard enough that a second reader pays for itself. On a small diff the
// reviewer costs about what the worker did and finds what the tests already show.
const (
	reviewMinFiles = 6
	reviewMinLines = 200
)

func reviewWorthwhile(t taskgraph.Task, changes []Change) bool {
	if len(changes) == 0 || t.Difficulty == routing.Trivial {
		return false
	}
	if t.Difficulty == routing.Hard || t.Difficulty == routing.Expert {
		return true
	}
	lines := 0
	for _, ch := range changes {
		lines += ch.Added + ch.Deleted
	}
	return len(changes) >= reviewMinFiles || lines >= reviewMinLines
}

// scheduleReview adds the automatic reviewer for t, unless one already exists: the Primary often plans its own
// reviewer for the same work, and two reviewers read the same diff and say the same thing.
func (c *Coord) scheduleReview(t taskgraph.Task, a agent.Agent) {
	c.mu.Lock()
	if c.reviewed[t.ID] {
		c.mu.Unlock()
		return
	}
	c.reviewed[t.ID] = true
	c.mu.Unlock()
	for _, other := range c.Graph.List() {
		if other.Role == string(agent.Reviewer) && other.Status != taskgraph.Failed && other.Status != taskgraph.Canceled && slices.Contains(other.Depends, t.ID) {
			return
		}
	}
	r := c.Graph.Add(taskgraph.Task{Title: "Review: " + t.Title, Description: "Independently review completed changes for correctness and acceptance.", Creator: a.ID, Priority: t.Priority, Role: string(agent.Reviewer), Workspace: t.Workspace, Depends: []string{t.ID}, MaxAttempts: c.scheduler().Limits.MaxRetry})
	c.emit("task.created", r)
}

func (c *Coord) failTask(t taskgraph.Task, a agent.Agent, cause error) {
	status := taskgraph.Failed
	if errors.Is(cause, context.Canceled) {
		status = taskgraph.Canceled // the user pressed Stop: nothing to retry or report
	}
	updated, err := c.Graph.Update(t.ID, func(x *taskgraph.Task) { x.Status = status; x.Finished = time.Now().UTC() })
	if err != nil {
		c.logWarn("fail task", err)
		return
	}
	c.emit("task.failed", updated)
	if status == taskgraph.Canceled {
		if kept := c.keptNote(t, a); kept != "" {
			c.chat("system", fmt.Sprintf("%q was stopped.%s", t.Title, kept), "")
		}
		return
	}
	if cause != nil && !retryable(cause) {
		c.chat("system", fmt.Sprintf("%q failed and will not be retried: %s.%s", t.Title, strings.TrimRight(cause.Error(), "."), c.keptNote(t, a)), "")
		return
	}
	var stalled *stalledError
	if errors.As(cause, &stalled) {
		// A new attempt would begin from nothing and meet the same wall; what this one got done is worth more.
		if kept := c.keptNote(t, a); kept != "" {
			c.chat("system", fmt.Sprintf("%q stopped responding and could not be brought back (%s), so it was not restarted from scratch.%s", t.Title, strings.TrimPrefix(stalled.Error(), "agent timed out: "), kept), "")
			return
		}
	}
	c.retryTask(updated, a)
}

// keepWork records what an agent that did not finish left in its worktree. Edits in a worktree are invisible until
// they are recorded, so a worker that was stopped or gave out looked as if it had done nothing: its work sat on disk
// while the Changes tab stayed empty.
func (c *Coord) keepWork(t taskgraph.Task, a agent.Agent) []Change {
	changes := c.changesFor(a, RawReport{})
	if len(changes) == 0 {
		return nil
	}
	files := make([]string, 0, len(changes))
	for _, ch := range changes {
		files = append(files, ch.Path)
	}
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Files = files })
	c.recordChanges(t, a, changes)
	return changes
}

// keptNote is the sentence that says where a stopped agent's edits went, or nothing if it made none.
func (c *Coord) keptNote(t taskgraph.Task, a agent.Agent) string {
	if n := len(c.keepWork(t, a)); n > 0 {
		return fmt.Sprintf(" Its edits so far (%s) are in the Changes tab.", plural(n, "file"))
	}
	return ""
}

// retryable reports whether trying the same task again could change the outcome. A rejected key, an exhausted quota
// or an unknown model fail identically every time; retrying only spends more.
func retryable(err error) bool {
	e := strings.ToLower(err.Error())
	for _, marker := range []string{"401", "403", "unauthorized", "forbidden", "invalid api key", "invalid x-api-key", "incorrect api key", "authentication", "insufficient", "quota", "billing", "credit balance", "model not found", "unknown model", "does not exist", "not available", "no models"} {
		if strings.Contains(e, marker) {
			return false
		}
	}
	return true
}

func (c *Coord) sessionContext() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionCtx != nil {
		return c.sessionCtx
	}
	return context.TODO()
}

func (c *Coord) logWarn(operation string, err error) {
	if c.Log != nil && err != nil {
		c.Log.Warn(operation, map[string]any{"error": err.Error()})
	}
}
