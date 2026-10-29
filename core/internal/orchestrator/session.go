package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
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
		sched = scheduler.New(scheduler.Limits{PathLeases: settings.PathLeases})
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
	out.Constraints = append([]string(nil), s.Constraints...)
	out.Decisions = append([]string(nil), s.Decisions...)
	out.PeerMessages = append([]PeerMessage(nil), s.PeerMessages...)
	if s.PeerWakes != nil {
		out.PeerWakes = make(map[string]int, len(s.PeerWakes))
		for k, v := range s.PeerWakes {
			out.PeerWakes[k] = v
		}
	}
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
	// The replaced session's waiting messages can never be delivered now.
	c.inbox = map[string][]queuedNote{}
	c.peerKeys = map[string]peerIdentity{}
	s := &Session{ID: id.New("sess"), Workspace: workspace, Objective: objective, Mode: mode, Status: "active", Created: time.Now().UTC(), Updated: time.Now().UTC(), Usage: map[string]any{}}
	c.sess, c.reviewed = s, map[string]bool{}
	c.Graph, c.Agents, c.Engine = taskgraph.New(), c.newRegistry(), ctxeng.New()
	c.baseline = nil
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
	c.peerKeys = map[string]peerIdentity{}
	clearQueuedMarks(c.sess)
	clearPeerDelivery(c.sess)
	if c.sess != nil {
		c.sess.Running = false
		if c.sess.Usage == nil {
			c.sess.Usage = map[string]any{}
		}
		for i := range c.sess.Changes {
			c.sess.Changes[i].NormalizeIntegration()
		}
	}
	c.lineage = map[string][]Dep{}
	c.lineageProblems = map[string]lineageProblem{}
	c.Graph, c.Agents, c.Engine = taskgraph.New(), c.newRegistry(), ctxeng.New()
	c.baseline = nil
	opened := cloneSession(c.sess)
	c.mu.Unlock()
	c.emit("session.opened", opened)
}

// Restore installs a persisted session. Nothing is running after a restart, whatever the stored flag says.
func (c *Coord) Restore(s *Session) {
	c.mu.Lock()
	c.sess = cloneSession(s)
	c.peerKeys = map[string]peerIdentity{}
	c.lineage = map[string][]Dep{}
	c.lineageProblems = map[string]lineageProblem{}
	clearQueuedMarks(c.sess)
	clearPeerDelivery(c.sess)
	if c.sess != nil {
		c.sess.Running = false
		for i := range c.sess.Changes {
			c.sess.Changes[i].NormalizeIntegration()
		}
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
	// Queued user messages that never reached the head are dropped, not silently lost.
	if n := c.dropQueued(); n > 0 {
		c.chat("system", fmt.Sprintf("Dropped %s that had not reached the head agent yet: the run was stopped.", plural(n, "queued message")), "")
	}
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
	c.settlePeerMessages(c.SessionID())
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
		// A request is already running: the message waits for the head's next step instead of being refused.
		queued := c.enqueueLocked(text, refs)
		c.mu.Unlock()
		cancel()
		// The user's own message must reach the UI at once, in its queued state.
		c.emit("chat.message", queued)
		c.wakeHead()
		return nil
	}
	if c.sess.Objective == "" {
		c.sess.Objective = text
	}
	c.runID++
	runID := c.runID
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
	c.mu.Lock()
	if c.sess != nil {
		for i := range c.sess.PeerMessages {
			m := &c.sess.PeerMessages[i]
			if m.To == primary.ID && (m.State == "queued" || m.State == "injected") {
				m.State, m.Reason = "undelivered", "previous run ended before acknowledgement"
			}
		}
	}
	c.mu.Unlock()
	c.Agents.Update(primary.ID, func(x *agent.Agent) {
		x.Status, x.Phase, x.Activity = agent.Running, "context", "Building context"
	})
	c.seedPrimary(text, refs)
	sid := c.SessionID()
	err := c.guard("primary", func() error {
		if err := c.loopPrimary(runCtx, primary, text); err != nil {
			return err
		}
		// Guidance that arrived too late for a turn starts a follow-up turn, so nothing the user typed is lost.
		for runCtx.Err() == nil && (c.queuedPending(sid) || c.peerPending(primary.ID)) {
			if err := c.loopPrimary(runCtx, primary, ""); err != nil {
				return err
			}
		}
		return nil
	})
	c.reportFailure(primary, "Primary failed", err)
	c.settlePrimary(primary.ID, runCtx, err)
	c.settlePeerMessages(sid)
	cancel()
	c.mu.Lock()
	var finished *Session
	var saved []Chat
	var savedNote Chat
	if c.runID == runID {
		// Capture messages admitted just before shutdown, including after the last follow-up check.
		// Ingest under the lifecycle lock so the next Submit cannot build before they are saved.
		var ids []string
		var lines, refs []string
		for _, note := range c.inbox[sid] {
			lines = append(lines, note.text)
			refs = append(refs, note.refs...)
			ids = append(ids, note.id)
		}
		delete(c.inbox, sid)
		saved = c.clearQueuedLocked(ids, false)
		if len(saved) > 0 {
			c.ingestGuidanceWithSession(primary.ID, strings.Join(lines, "\n\n"), refs, c.sess)
			c.addChatLocked(Chat{Role: "system", Text: fmt.Sprintf("Saved %s for the next request: the run ended before the head agent could read them.", plural(len(saved), "queued message"))})
			savedNote = c.sess.Messages[len(c.sess.Messages)-1]
		}
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
	for _, m := range saved {
		c.emit("chat.message", m)
	}
	if savedNote.ID != "" {
		c.emit("chat.message", savedNote)
	}
	if finished != nil {
		c.emit("session.updated", finished)
	}
	return err
}

// settlePrimary ends the Primary's run. Without it the agent would stay "running" after a successful turn, which
// blocks Busy (and so deferred restarts) and leaves the Stop button lit forever.
func (c *Coord) settlePrimary(id string, ctx context.Context, err error) {
	defer c.finishPeerTurn(c.SessionID(), id)
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

// Note adds a visible system message to the current session.
func (c *Coord) Note(text string) { c.chat("system", text, "") }

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
	p, _ := c.Agents.Primary()
	c.syncPeerContext(s.ID, p.ID)
	// Objective, latest user message and mode are singletons: each turn replaces them rather than appending.
	c.Engine.SetTaskState(p.ID, "Objective", []ctxeng.Item{{Source: ctxeng.SourceSummary, Content: s.Objective, Durability: 1, Relevance: 1, Pinned: true}})
	var mode []ctxeng.Item
	if s.Mode != "" {
		mode = []ctxeng.Item{{Source: ctxeng.SourceUser, Kind: ctxeng.KindConstraint, Content: "Active mode: " + string(s.Mode) + ". Plan/review modes must not mutate the repo.", Pinned: true, Durability: 1}}
	}
	c.Engine.SetTaskState(p.ID, "Mode", mode)
	if len(s.Decisions) > 0 {
		c.applyDecisions(p.ID, s.Decisions)
	}
	c.syncPrimaryConversation(p.ID, text, s.Messages)
	var items []ctxeng.Item
	for _, ref := range refs {
		items = append(items, ctxeng.Item{Class: ctxeng.ClassCode, Source: ctxeng.SourceRepo, Content: ref, Durability: 0.6})
	}
	if settings.Memory {
		for _, e := range c.Memory.Recall(text) {
			items = append(items, ctxeng.Item{Class: ctxeng.ClassMemory, Source: ctxeng.SourceVerified, Title: staleMemory + e.Title, Content: e.Body, Files: e.Files, Verified: e.Verified, Durability: 0.8})
		}
	}
	c.Engine.Ingest(p.ID, items)
}

// Only user-authored instructions and the preceding reply cross chat turns, not the full execution transcript.
func (c *Coord) syncPrimaryConversation(primaryID, text string, messages []Chat) {
	c.Engine.SetTaskState(primaryID, "Latest user message", []ctxeng.Item{{Source: ctxeng.SourceUser, Content: text, Durability: 0.9, Relevance: 1}})
	first, earlier, reply := primaryHistory(messages)
	var original []ctxeng.Item
	if first != "" && first != text {
		original = []ctxeng.Item{{Source: ctxeng.SourceUser, Content: first, Pinned: true, Durability: 1, Relevance: 1}}
	}
	c.Engine.SetTaskState(primaryID, "Original user request", original)
	var prior []ctxeng.Item
	if len(earlier) > 0 {
		prior = []ctxeng.Item{{Source: ctxeng.SourceUser, Content: strings.Join(earlier, "\n\n"), Durability: 0.9, Relevance: 0.9}}
	}
	c.Engine.SetTaskState(primaryID, "Earlier user instructions", prior)
	var last []ctxeng.Item
	if reply != "" {
		last = []ctxeng.Item{{Source: ctxeng.SourceSummary, Content: reply, Durability: 0.9}}
	}
	c.Engine.SetTaskState(primaryID, "Previous assistant reply", last)
}

// primaryHistory reads the transcript for the Primary's canonical user-side context. first is the earliest delivered
// user message, earlier are the delivered user messages between it and the latest one (oldest first), and reply is the
// assistant message immediately before the latest user message. Messages still queued are not delivered and are left out.
// The latest user message is the one being answered, so it is excluded from earlier and does not count as its own reply.
func primaryHistory(messages []Chat) (first string, earlier []string, reply string) {
	latest := -1
	for i, m := range messages {
		if m.Role == "user" && !m.Queued && !m.Dropped {
			latest = i
		}
	}
	firstIdx := -1
	for i, m := range messages {
		if m.Role == "user" && !m.Queued && !m.Dropped {
			firstIdx = i
			first = m.Text
			break
		}
	}
	if firstIdx < 0 {
		return "", nil, ""
	}
	for i := firstIdx + 1; i < latest; i++ {
		if m := messages[i]; m.Role == "user" && !m.Queued && !m.Dropped && strings.TrimSpace(m.Text) != "" {
			earlier = append(earlier, m.Text)
		}
	}
	for i := latest - 1; i >= 0; i-- {
		if m := messages[i]; m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
			reply = m.Text
			break
		}
		if messages[i].Role == "user" {
			break
		}
	}
	return first, earlier, reply
}

func (c *Coord) loopPrimary(ctx context.Context, primary agent.Agent, text string) error {
	settings := c.settings()
	s := c.Current()
	if s == nil {
		return fmt.Errorf("no session")
	}
	// Guidance queued during an earlier turn is part of this step's user message.
	if queued, refs, ok := c.takeQueued(c.SessionID()); ok {
		c.ingestGuidance(primary.ID, queued, refs)
		if text != "" {
			text += "\n\n" + queued
		} else {
			text = queued
		}
	}
	resolvedPrimary := style.Resolve(settings.Efficiency, primary.Role, primary.Difficulty, primary.Attempt, settings.Budget)
	primaryBudget := settings.Budget
	if resolvedPrimary.Scope.Budget != 0 {
		primaryBudget = resolvedPrimary.Scope.Budget
	}
	primaryBudget = settings.BudgetFor(primary.Model, primaryBudget)
	c.pinBlocks(primary.ID)
	gen, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: primary.ID, Role: string(agent.Primary), Phase: ctxeng.PhaseDesign, Budget: primaryBudget, Reason: "primary turn", Query: ctxeng.Query{Task: s.Objective, Keywords: strings.Fields(s.Objective)}})
	if err != nil {
		return err
	}
	c.emit("context.created", map[string]any{"agentId": primary.ID, "generation": gen.Number, "tokens": gen.Tokens})
	extra := c.workspaceFacts() + c.constraintsNote() + c.waitNote() + "\nUser message:\n" + text
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
	c.applyTurn(primary, turn, text)
	if len(turn.Tasks) > 0 {
		c.primaryWaits()
	}
	c.honorRefreshSelf(primary.ID, turn)
	if err := c.drive(ctx); err != nil {
		return err
	}
	if turn.Status == "completed" && len(c.Graph.Runnable()) == 0 && len(c.openQuestions()) == 0 {
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

// honorRefreshSelf rebuilds the head's context when its turn set refreshSelf. Every head path calls it after applyTurn
// so the request works on any turn, not only the first. Pinned items survive: Engine.Refresh keeps them in gcPool.
func (c *Coord) honorRefreshSelf(agentID string, turn Turn) {
	if !turn.RefreshSelf {
		return
	}
	if _, err := c.Engine.Refresh(agentID, "primary requested refresh"); err != nil {
		c.logWarn("refresh primary context", err)
		return
	}
	c.emit("context.refreshed", map[string]any{"agentId": agentID, "reason": "primary requested refresh"})
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

func (c *Coord) applyTurn(primary agent.Agent, turn Turn, userText ...string) {
	settings := c.settings()
	s := c.Current()
	if s == nil {
		return
	}
	if len(turn.ResolvedChanges) > 0 && c.ResolveChanges != nil {
		c.ResolveChanges(turn.ResolvedChanges)
	}
	// Answers come before admission, so a family whose question is answered here can be planned in this same turn.
	c.applyResolvedBlocks(turn.ResolvedBlocks, userText...)
	c.pinBlocks(primary.ID)
	c.setConstraints(turn.Constraints)
	idmap := map[string]string{}
	var existing, inTurn map[string]string
	turn.Tasks, existing, inTurn = c.admitTaskAliases(turn.Tasks)
	// A dropped duplicate's spec ID resolves to the task it repeats: an existing graph task, or the surviving spec of this turn.
	for dropped, realID := range existing {
		idmap[dropped] = realID
	}
	for _, spec := range turn.Tasks {
		role := parseRole(spec.Role)
		t := c.Graph.Add(taskgraph.Task{Title: spec.Title, Description: spec.Description, Acceptance: spec.Acceptance, Creator: primary.ID, Priority: spec.Priority, Role: string(role), Workspace: s.Workspace, Difficulty: spec.Difficulty, Vision: spec.Vision || spec.Art, Art: spec.Art && c.artOn(), MaxAttempts: settings.MaxRetries, Model: c.modelForTask(role, spec.Difficulty, spec.Vision || spec.Art), Paths: spec.Paths, Scope: spec.Scope})
		idmap[spec.ID] = t.ID
		c.emit("task.created", t)
	}
	for dropped, survivor := range inTurn {
		if realID := idmap[survivor]; realID != "" {
			idmap[dropped] = realID
		}
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
	c.applyObjective(primary.ID, turn.Objective)
	c.applyDecisions(primary.ID, turn.Decisions)
}

// applyObjective replaces the session objective and the Primary's Objective context with the Primary's latest value.
// An empty value is "unchanged". It runs on every turn path, so the Primary never reasons from a superseded objective.
func (c *Coord) applyObjective(agentID, objective string) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return
	}
	c.mu.Lock()
	if c.sess != nil {
		c.sess.Objective = objective
	}
	c.mu.Unlock()
	c.Engine.SetTaskState(agentID, "Objective", []ctxeng.Item{{Source: ctxeng.SourceSummary, Content: objective, Durability: 1, Relevance: 1, Pinned: true}})
}

// applyDecisions replaces the Decision group with the Primary's full current list. A decision is the Primary's
// interpretation of the request, not evidence, so it is summarized and unverified. An empty list is "unchanged".
func (c *Coord) applyDecisions(agentID string, decisions []string) {
	var items []ctxeng.Item
	for _, d := range decisions {
		if d = strings.TrimSpace(d); d != "" {
			items = append(items, ctxeng.Item{Source: ctxeng.SourceSummary, Content: d, Durability: 0.9})
		}
	}
	if len(items) == 0 {
		return
	}
	current := make([]string, 0, len(items))
	for _, item := range items {
		current = append(current, item.Content)
	}
	c.mu.Lock()
	if c.sess != nil {
		c.sess.Decisions = current
	}
	c.mu.Unlock()
	c.Engine.SetTaskState(agentID, "Decision", items)
}

// setConstraints replaces the session's constraints with the Primary's latest list. An empty list is "unchanged": a
// model that has nothing to add says nothing, and must not wipe what it set earlier.
func (c *Coord) setConstraints(list []string) {
	if len(list) == 0 {
		return
	}
	c.mu.Lock()
	if c.sess == nil || slices.Equal(c.sess.Constraints, list) {
		c.mu.Unlock()
		return
	}
	c.sess.Constraints = append([]string(nil), list...)
	c.sess.Updated = time.Now().UTC()
	s := cloneSession(c.sess)
	c.mu.Unlock()
	c.emit("session.updated", s)
}

// constraintsNote shows the Primary the list it set, so it can keep it, amend it or drop one.
func (c *Coord) constraintsNote() string {
	s := c.Current()
	if s == nil || len(s.Constraints) == 0 {
		return ""
	}
	return "\n## Your standing constraints (every sub agent is given these; send the full list in constraints to change it)\n- " + strings.Join(s.Constraints, "\n- ") + "\n"
}

// admitTasks drops tasks that repeat work already planned (same title and role, not failed or canceled). There is
// no cap on how many tasks a session can plan: long requests need many tasks, and a runaway planner is caught by
// the duplicate check. Dropped duplicates are logged.
func (c *Coord) admitTasks(specs []TurnTask) []TurnTask {
	kept, _, _ := c.admitTaskAliases(specs)
	return kept
}

// admitTaskAliases is admitTasks plus the mapping for dropped duplicates. existing maps a dropped spec ID to the
// persisted task it repeats; inTurn maps a dropped spec ID to the kept spec ID of the same turn that it repeats.
func (c *Coord) admitTaskAliases(specs []TurnTask) (kept []TurnTask, existing, inTurn map[string]string) {
	if len(specs) == 0 {
		return specs, nil, nil
	}
	existing, inTurn = map[string]string{}, map[string]string{}
	refusals := c.refuseAroundQuestions(specs)
	notes := map[string][]string{} // asking task ID -> what was refused for it, pinned for the Primary
	dupes, refused := 0, 0
	for i, spec := range specs {
		// Work that touches a question waiting for the user is refused under any id, title, role or wording until
		// the user answers.
		if r, ok := refusals[i]; ok {
			note := fmt.Sprintf("Skipped %q because it %s task %s (%q), which is waiting for the user's answer: %s. Wait for the user; do not plan around it.", spec.Title, r.why, r.asking.ID, r.asking.Title, r.asking.BlockedOn.Question)
			notes[r.asking.ID] = append(notes[r.asking.ID], note)
			c.chat("system", note, "")
			refused++
			continue
		}
		candidate := specTask(spec)
		matched := false
		for _, t := range c.Graph.List() {
			if t.Status != taskgraph.Failed && t.Status != taskgraph.Canceled && taskgraph.FamilyKey(t) == taskgraph.FamilyKey(candidate) {
				existing[spec.ID] = t.ID
				dupes++
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		for _, prior := range kept {
			other := taskgraph.Task{Title: prior.Title, Role: string(parseRole(prior.Role)), Paths: prior.Paths, Scope: prior.Scope}
			if taskgraph.FamilyKey(other) == taskgraph.FamilyKey(candidate) {
				inTurn[spec.ID] = prior.ID
				dupes++
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		kept = append(kept, spec)
	}
	if c.Agents != nil && c.Engine != nil && len(notes) > 0 {
		if primary, ok := c.Agents.Primary(); ok {
			for id, lines := range notes {
				c.Engine.SetTaskState(primary.ID, "Refused blocked replans: "+id, []ctxeng.Item{{Source: ctxeng.SourceSystem, Kind: ctxeng.KindConstraint, Content: strings.Join(lines, "\n"), Durability: 1, Relevance: 1, Pinned: true}})
			}
		}
	}
	if dupes > 0 {
		c.logWarn("planner repeated tasks", fmt.Errorf("dropped %d duplicate task(s)", dupes))
	}
	if refused > 0 {
		c.logWarn("planner re-planned a blocked task", fmt.Errorf("dropped %d task(s) in a family waiting for an answer", refused))
	}
	return kept, existing, inTurn
}

// specTask is the task shape the spec would have once admitted, as far as family and path checks go.
func specTask(spec TurnTask) taskgraph.Task {
	return taskgraph.Task{ID: spec.ID, Title: spec.Title, Description: spec.Description, Role: string(parseRole(spec.Role)), Paths: spec.Paths, Scope: spec.Scope, Depends: spec.Deps}
}

// specFamily is the family key the task spec would get once admitted.
func specFamily(spec TurnTask) string {
	return taskgraph.FamilyKey(specTask(spec))
}

// questionRefusal says which waiting question a planned task touches and how.
type questionRefusal struct {
	asking taskgraph.Task
	why    string // a verb phrase that ends before the task, such as "shares path a.go with"
}

// refuseAroundQuestions maps the index of each spec that must not be admitted to the open question it touches.
// While a question waits for the user, Primary may not act around it: no new task may repeat or reword its work
// under another role, reuse its id, share one of its paths, depend on it (or on a task that does), or name it; and a
// task that depends on a refused spec goes with it. Work that touches none of these continues. The guard lifts when
// the question is answered, and for a question the user delegated to Primary, unless only the user may answer it.
// A parked task that is not Asking (an older store shape) keeps only the near-copy rule.
func (c *Coord) refuseAroundQuestions(specs []TurnTask) map[int]questionRefusal {
	open := c.openQuestions()
	if len(open) == 0 {
		return nil
	}
	delegated := false
	if s := c.Current(); s != nil {
		delegated = s.DelegateAnswers
	}
	graph := c.Graph.List()
	refused := map[int]questionRefusal{}
	for i, spec := range specs {
		candidate := specTask(spec)
		for _, t := range open {
			strict := t.Status == taskgraph.Asking && !(delegated && !userOnlyQuestion(t.BlockedOn.Question))
			if why := questionConflict(t, candidate, spec, strict, graph); why != "" {
				refused[i] = questionRefusal{asking: t, why: why}
				break
			}
		}
	}
	// A task that waits on a refused one cannot run without it, so it is part of the same workaround.
	for changed := true; changed; {
		changed = false
		for i, spec := range specs {
			if _, done := refused[i]; done {
				continue
			}
			for j, other := range specs {
				if r, bad := refused[j]; bad && other.ID != "" && slices.Contains(spec.Deps, other.ID) {
					refused[i] = questionRefusal{asking: r.asking, why: fmt.Sprintf("depends on the skipped task %q, which is tied to", other.Title)}
					changed = true
					break
				}
			}
		}
	}
	return refused
}

// questionConflict explains how the candidate touches the waiting task t, or returns "" when it does not.
func questionConflict(t, candidate taskgraph.Task, spec TurnTask, strict bool, graph []taskgraph.Task) string {
	if strict {
		switch {
		case spec.ID != "" && spec.ID == t.ID:
			return "would change or replace"
		case dependsOnTask(spec.Deps, t.ID, graph):
			return "depends on"
		case (t.BlockedOn.Family != "" && t.BlockedOn.Family == taskgraph.FamilyKey(candidate)) || taskgraph.Related(t, candidate):
			return "is the same work as"
		case mentionsTask(spec, t):
			return "refers to"
		}
		if shared := taskgraph.SharedPaths(candidate, t); len(shared) > 0 {
			return fmt.Sprintf("shares %s with", strings.Join(shared, ", "))
		}
	}
	if (t.Status == taskgraph.Asking || t.Status == taskgraph.Failed || t.Status == taskgraph.Canceled) && taskgraph.NearCopy(t, candidate) {
		return "repeats"
	}
	return ""
}

// dependsOnTask reports whether any dep is the task or a task that depends on it, however indirectly.
func dependsOnTask(deps []string, id string, graph []taskgraph.Task) bool {
	down := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for _, t := range graph {
			if down[t.ID] {
				continue
			}
			for _, d := range t.Depends {
				if down[d] {
					down[t.ID], changed = true, true
					break
				}
			}
		}
	}
	for _, d := range deps {
		if down[d] {
			return true
		}
	}
	return false
}

// mentionsTask reports whether the spec names the waiting task by id, or by a title long enough to be specific.
func mentionsTask(spec TurnTask, t taskgraph.Task) bool {
	text := strings.ToLower(strings.Join(append([]string{spec.Title, spec.Description, spec.Scope}, spec.Acceptance...), "\n"))
	if id := strings.ToLower(t.ID); id != "" && strings.Contains(text, id) {
		return true
	}
	title := strings.Join(strings.Fields(strings.ToLower(t.Title)), " ")
	return len(title) >= 12 && strings.Contains(strings.Join(strings.Fields(text), " "), title)
}

// openQuestions returns the tasks still waiting on an answer to a blocked question.
func (c *Coord) openQuestions() []taskgraph.Task {
	var open []taskgraph.Task
	for _, t := range c.Graph.List() {
		if t.BlockedOn.Unresolved() {
			open = append(open, t)
		}
	}
	return open
}

// unresolvedFamilies returns the family keys of the open questions.
func (c *Coord) unresolvedFamilies() map[string]bool {
	families := map[string]bool{}
	for _, t := range c.openQuestions() {
		families[t.BlockedOn.Family] = true
	}
	return families
}

// applyResolvedBlocks records the user's answers on the tasks waiting for them. Unknown ids, empty answers and tasks
// already answered are ignored.
func (c *Coord) applyResolvedBlocks(answers []ResolvedBlock, userText ...string) {
	for _, r := range answers {
		cite := strings.TrimSpace(r.Cite)
		source := taskgraph.AnswerSource("")
		if cite != "" && len(userText) > 0 && strings.Contains(userText[0], cite) {
			source = taskgraph.AnsweredByUser
		} else if len([]rune(cite)) >= 12 && c.answerEvidence(cite) {
			source = taskgraph.AnsweredByPrimary
		} else if s := c.Current(); s != nil && s.DelegateAnswers {
			source = taskgraph.AnsweredByDelegated
		}
		t, ok := c.Graph.Get(r.TaskID)
		if !ok || !t.BlockedOn.Unresolved() || source == "" || (userOnlyQuestion(t.BlockedOn.Question) && source != taskgraph.AnsweredByUser) {
			continue
		}
		if err := c.recordQuestionAnswer(r.TaskID, r.Answer, source, cite); err != nil {
			c.logWarn("record answer", err)
		}
	}
}

func (c *Coord) answerEvidence(cite string) bool {
	if s := c.Current(); s != nil {
		if strings.Contains(s.Objective, cite) {
			return true
		}
		for _, text := range append(append([]string{}, s.Decisions...), s.Constraints...) {
			if strings.Contains(text, cite) {
				return true
			}
		}
		for _, m := range s.Messages {
			if m.Role == "user" && !m.Queued && !m.Dropped && strings.Contains(m.Text, cite) {
				return true
			}
		}
	}
	for _, f := range c.Engine.Findings() {
		if !f.Verified {
			continue
		}
		for _, text := range append([]string{f.Summary, f.Detail, f.RootCause}, f.Evidence...) {
			if strings.Contains(text, cite) {
				return true
			}
		}
	}
	return false
}

func userOnlyQuestion(question string) bool {
	q := strings.ToLower(question)
	for _, marker := range []string{"quota", "provider", "billing", "credit balance", "token exhaustion", "token limit", "rate limit"} {
		if strings.Contains(q, marker) {
			return true
		}
	}
	return false
}

// AnswerQuestion records an explicit user choice or custom answer.
func (c *Coord) AnswerQuestion(taskID, answer string) error {
	return c.recordQuestionAnswer(taskID, answer, taskgraph.AnsweredByUser, "")
}

// SetDelegateAnswers changes whether Primary may answer without cited evidence.
func (c *Coord) SetDelegateAnswers(enabled bool) {
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return
	}
	c.sess.DelegateAnswers = enabled
	c.sess.Updated = time.Now().UTC()
	s := cloneSession(c.sess)
	c.mu.Unlock()
	c.emit("session.updated", s)
}

func (c *Coord) recordQuestionAnswer(taskID, answer string, source taskgraph.AnswerSource, cite string) error {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return fmt.Errorf("answer is empty")
	}
	changed := false
	updated, err := c.Graph.Update(taskID, func(x *taskgraph.Task) {
		if !x.BlockedOn.Unresolved() {
			return
		}
		b := *x.BlockedOn
		now := time.Now().UTC()
		b.Resolution, b.Answer, b.AnsweredBy, b.Cite, b.ResolvedAt, b.AnsweredAt = answer, answer, source, cite, now, now
		x.BlockedOn = &b
		x.Status, x.Finished = taskgraph.Queued, time.Time{}
		changed = true
	})
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("question is not open")
	}
	c.emit("task.updated", updated)
	c.chat("system", fmt.Sprintf("%q answered by %s: %s", updated.Title, source, answer), "")
	if c.Agents != nil && c.Engine != nil {
		if primary, ok := c.Agents.Primary(); ok {
			c.Engine.SetTaskState(primary.ID, "Refused blocked replans: "+taskID, nil)
		}
	}
	if len(c.openQuestions()) == 0 {
		c.mu.Lock()
		var s *Session
		if c.sess != nil && c.sess.Status == "blocked" {
			c.sess.Status = "active"
			c.sess.Updated = time.Now().UTC()
			s = cloneSession(c.sess)
		}
		c.mu.Unlock()
		if s != nil {
			c.emit("session.updated", s)
		}
	}
	c.wakeHead()
	return nil
}

// pinBlocks pins each open question to the Primary's state, so every Primary turn shows it until it is answered.
func (c *Coord) pinBlocks(primaryID string) {
	var items []ctxeng.Item
	for _, t := range c.openQuestions() {
		where := ""
		if len(t.Paths) > 0 {
			where = " Its paths: " + strings.Join(t.Paths, ", ") + "."
		}
		items = append(items, ctxeng.Item{Source: ctxeng.SourceSystem, Kind: ctxeng.KindConstraint, Content: fmt.Sprintf("WAIT FOR USER: task %s (%q) is blocked, unanswered: %s.%s Do not replan, cancel, replace or work around it, and do not plan tasks that share its family or paths or depend on it: core refuses them. Work that is disjoint from it may continue. Return resolvedBlocks [{taskId,answer,answeredBy,cite}] only with authorized evidence; provider and quota questions require the user.", t.ID, t.Title, t.BlockedOn.Question, where), Durability: 1, Relevance: 1, Pinned: true})
	}
	c.Engine.SetTaskState(primaryID, "Unresolved questions", items)
}

// waitNote lists the questions waiting for the user in the Primary's turn message, so a turn that plans while one is
// open sees it where it plans and not only in the pinned context.
func (c *Coord) waitNote() string {
	open := c.openQuestions()
	if len(open) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Waiting for the user (do not plan around these; core refuses tasks that share their family or paths or depend on them)\n")
	for _, t := range open {
		fmt.Fprintf(&b, "- %s (%q): %s\n", t.ID, t.Title, t.BlockedOn.Question)
	}
	return b.String()
}

// surfaceBlocks shows the user each open question once per drive exit and marks the session blocked while any remain.
func (c *Coord) surfaceBlocks() {
	open := c.openQuestions()
	c.mu.Lock()
	changed := false
	if c.sess != nil {
		switch {
		case len(open) > 0 && c.sess.Status == "active":
			c.sess.Status, changed = "blocked", true
		case len(open) == 0 && c.sess.Status == "blocked":
			c.sess.Status, changed = "active", true
		}
	}
	var s *Session
	if changed {
		s = cloneSession(c.sess)
	}
	c.mu.Unlock()
	if s != nil {
		c.emit("session.updated", s)
	}
	for _, t := range open {
		c.chat("system", fmt.Sprintf("%q is blocked, needs a decision: %s", t.Title, t.BlockedOn.Question), "")
	}
}

func (c *Coord) primaryTurn(ctx context.Context, primary agent.Agent, parts prompt.Parts, ask string) (Turn, error) {
	body, err := c.runAgent(ctx, primary, parts, ask, prompt.PrimarySchemaFor(c.artOn()))
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
		repair = "Your last reply was tool-call markup, not an executed tool. You have no repository tools; use relo_agents or relo_canvas only if offered. Reply with the JSON turn: what was done in userMessage, and tasks [] when nothing is left to do."
	}
	body, err = c.runAgent(ctx, primary, parts, repair, prompt.PrimarySchemaFor(c.artOn()))
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
		summary, _ := clipText(strings.TrimSpace(t.Report.(agent.Report).Summary), c.summaryLimit(""))
		lines = append(lines, "- "+t.Title+": "+summary)
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
	for {
		if err := sched.Wait(ctx); err != nil {
			return err
		}
		if len(sched.Next(c.Graph, 1)) == 0 {
			c.cancelOrphans()
			c.surfaceBlocks()
			return nil
		}
		c.refreshSnapshot()
		c.primaryWaits()
		if c.dispatch(ctx, sched) == 0 {
			if sched.Paused() {
				continue
			}
			c.cancelOrphans()
			c.surfaceBlocks()
			return nil
		}
		c.cancelOrphans()
		c.repromptPrimary(ctx)
	}
}

// cancelOrphans retires tasks that can never run because something they depend on failed for good. Left Blocked
// they would keep Primary from ever being asked to wrap up, and the user would see work silently stop. Repeat to a
// fixed point because Graph.List is unordered: cancelling a parent can orphan a child that the first pass already saw.
func (c *Coord) cancelOrphans() {
	for {
		changed := false
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
						changed = true
						c.emit("task.updated", updated)
					}
					break
				}
			}
		}
		if !changed {
			return
		}
	}
}

func (c *Coord) repromptPrimary(ctx context.Context) {
	settings := c.settings()
	p, ok := c.Agents.Primary()
	if !ok {
		return
	}
	asking := len(c.openQuestions()) > 0
	for _, t := range c.Graph.List() {
		if t.Status == taskgraph.Queued || t.Status == taskgraph.Running || (t.Status == taskgraph.Blocked && !asking) {
			return
		}
	}
	// Guidance queued while the workers ran is part of this step.
	queued, refs, hasQueued := c.takeQueued(c.SessionID())
	if hasQueued {
		c.ingestGuidance(p.ID, queued, refs)
	}
	c.pinBlocks(p.ID)
	gen, err := c.Engine.Refresh(p.ID, "collapse completed worker reports")
	if err != nil {
		c.logWarn("refresh primary after workers", err)
		return
	}
	c.emit("context.compressed", map[string]any{"agentId": p.ID, "generation": gen.Number})
	resolvedPrimary := style.Resolve(settings.Efficiency, p.Role, p.Difficulty, p.Attempt, settings.Budget)
	extra := c.workspaceFacts() + c.constraintsNote() + c.waitNote() + "\nAll scheduled workers finished. If every task is done and no check failed, put what was done in userMessage and return tasks as []. Plan follow-up work only for a task that failed or an acceptance criterion that is still unmet."
	if hasQueued {
		extra += "\nThe user sent this while you were working:\n" + queued
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
	c.applyTurn(p, turn, queued)
	c.honorRefreshSelf(p.ID, turn)
}

func (c *Coord) nextAttempt(t taskgraph.Task, a agent.Agent) (string, bool) {
	model := a.Model
	if settings := c.settings(); settings.Escalate && t.Attempt >= 2 {
		escalate := c.routeConfig().Escalate
		if t.Vision {
			escalate = func(current string) (string, bool) { return c.routeConfig().EscalateSeeing(current, settings.Sees) }
		}
		if next, ok := escalate(a.Model); ok {
			model = next
		}
	}
	return model, t.CanRetry()
}

// rememberFailure keeps why an attempt failed so the attempt that replaces it does not start blind. Attempts are new
// agents with new contexts; without this the next one repeats the approach that just failed.
func (c *Coord) rememberFailure(taskID, why string) {
	c.mu.Lock()
	if c.failures == nil {
		c.failures = map[string]string{}
	}
	c.failures[taskID] = why
	c.mu.Unlock()
}

// clearSucceededRetry retires retry context only for the attempt that actually completed.
func (c *Coord) clearSucceededRetry(taskID, agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cleared := false
	_, err := c.Graph.Update(taskID, func(t *taskgraph.Task) {
		if t.Status == taskgraph.Done && t.AgentID == agentID {
			t.RetryPatch, t.RetryState = "", ""
			cleared = true
		}
	})
	if err == nil && cleared {
		delete(c.failures, taskID)
	}
}

// failureNote is the item that tells a retried agent what went wrong last time, or nothing on a first attempt.
func (c *Coord) failureNote(t taskgraph.Task) []ctxeng.Item {
	c.mu.Lock()
	why := c.failures[t.ID]
	c.mu.Unlock()
	if why == "" && t.RetryState == "" {
		return nil
	}
	if why == "" {
		why = "it did not finish"
	}
	state := t.RetryState
	if state == "" {
		state = "The earlier changes were not carried over."
	}
	prefix, suffix := "An earlier attempt at this task failed because ", ". "+state+" Take a different approach to what went wrong."
	reason := []rune(why)
	if maxReason := 1500 - len([]rune(prefix+suffix)); len(reason) > maxReason {
		reason = reason[:max(0, maxReason)]
	}
	return []ctxeng.Item{{
		Class: ctxeng.ClassTask, Source: ctxeng.SourceParent, Title: "Previous attempt failed", Durability: 1,
		Content: prefix + string(reason) + suffix,
	}}
}

func (c *Coord) retryTask(t taskgraph.Task, a agent.Agent) {
	c.retryTaskAfter(t, a, nil)
}

// retryDelay uses the dispatched attempt count, adding jitter without exceeding one minute.
func retryDelay(attempt int, jitter float64) time.Duration {
	base := 5 * time.Second
	if attempt == 2 {
		base = 20 * time.Second
	} else if attempt >= 3 {
		return 54*time.Second + time.Duration(float64(6*time.Second)*jitter)
	}
	delay := base + time.Duration(float64(base)*0.2*jitter)
	return delay
}

func (c *Coord) retryTaskAfter(t taskgraph.Task, a agent.Agent, cause error) {
	model, retry := c.nextAttempt(t, a)
	if !retry {
		c.chat("system", fmt.Sprintf("%q failed after %s and was not retried again. Ask Relo to try a different approach.%s", t.Title, plural(t.Attempt, "attempt"), c.keptNote(t, a)), "")
		return
	}
	from := a.Model
	patch, state := "", "The earlier changes were not carried over."
	if ws := c.workspace(); ws != nil {
		if tree, ok := ws.Tree(a.ID); ok && tree.Direct {
			state = "Earlier edits remain in the shared workspace."
		} else if ok && tree.Path != "" {
			var err error
			patch, err = ws.Patch(a.ID)
			if err != nil {
				state = "The earlier diff could not be captured; starting fresh."
				c.logWarn("capture retry diff", err)
			} else if patch != "" {
				state = "The earlier diff is waiting to be applied."
			} else {
				state = "The earlier attempt left no changes."
			}
		}
	}
	var notBefore time.Time
	if class := runtime.Classify(cause); class == runtime.ClassRateLimit || class == runtime.ClassServer {
		notBefore = time.Now().UTC().Add(retryDelay(t.Attempt, rand.Float64()))
	}
	updated, err := c.Graph.Update(t.ID, func(x *taskgraph.Task) {
		x.Status, x.AgentID, x.Model = taskgraph.Queued, "", model
		x.Started, x.Finished = time.Time{}, time.Time{}
		x.RetryPatch, x.RetryState, x.NotBefore = patch, state, notBefore
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
	}
	c.emit("task.updated", updated)
}

// announceEscalation reports a model change once its attempt has started, so the attempt number is the one dispatch
// recorded rather than a prediction made when the retry was queued.
func (c *Coord) announceEscalation(started taskgraph.Task) {
	c.mu.Lock()
	from, ok := c.escalations[started.ID]
	delete(c.escalations, started.ID)
	c.mu.Unlock()
	if ok {
		c.emit("agent.escalated", map[string]any{"taskId": started.ID, "from": from, "to": started.Model, "attempt": started.Attempt, "reason": "repeated attempt failed; escalating model"})
	}
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
	r := c.Graph.Add(taskgraph.Task{Title: "Review: " + t.Title, Description: "Independently review completed changes for correctness and acceptance.", Creator: a.ID, Priority: t.Priority, Role: string(agent.Reviewer), Workspace: t.Workspace, Depends: []string{t.ID}, Vision: t.Vision, MaxAttempts: c.scheduler().Limits.MaxRetry})
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
	c.endReport(a, status, cause)
	c.emit("task.failed", updated)
	if status == taskgraph.Canceled {
		if kept := c.keptNote(t, a); kept != "" {
			c.chat("system", fmt.Sprintf("%q was stopped.%s", t.Title, kept), "")
		}
		return
	}
	// A workspace that stayed busy for the whole wait will still be busy on a retry, so retrying only hides the
	// holder's name behind another long wait. Retryability is decided by type, before the string markers below.
	var busy *workspaceBusyError
	if errors.As(cause, &busy) {
		c.chat("system", fmt.Sprintf("%q failed and will not be retried: %s.%s", t.Title, strings.TrimRight(cause.Error(), "."), c.keptNote(t, a)), "")
		return
	}
	var carry *retryCarryError
	if errors.As(cause, &carry) {
		c.chat("system", fmt.Sprintf("%q failed while restoring prior work: %s. The prior attempt's worktree remains available.", t.Title, cause), "")
		return
	}
	if cause != nil && !retryable(cause) {
		c.chat("system", fmt.Sprintf("%q failed and will not be retried: %s.%s", t.Title, strings.TrimRight(cause.Error(), "."), c.keptNote(t, a)), "")
		return
	}
	if cause != nil {
		c.rememberFailure(t.ID, "it stopped with an error: "+tailText(cause.Error(), 300))
	}
	var stalled *stalledError
	if errors.As(cause, &stalled) {
		// A new attempt would begin from nothing and meet the same wall; what this one got done is worth more.
		if kept := c.keptNote(t, a); kept != "" {
			c.chat("system", fmt.Sprintf("%q stopped responding and could not be brought back (%s), so it was not restarted from scratch.%s", t.Title, strings.TrimPrefix(stalled.Error(), "agent timed out: "), kept), "")
			return
		}
	}
	c.retryTaskAfter(updated, a, cause)
}

// endReport gives an agent that ended without writing a report a final message of its own. A stopped or failed run
// reached no report path, so it left nothing to read, and its card said only "Stopped". A report the agent wrote
// is never replaced.
func (c *Coord) endReport(a agent.Agent, status taskgraph.Status, cause error) {
	if rep, ok := c.Agents.Report(a.ID); ok && strings.TrimSpace(rep.Summary) != "" {
		return
	}
	rep := agent.Report{Status: agent.Failed, Summary: "The agent stopped before it wrote a report."}
	switch {
	case status == taskgraph.Canceled:
		rep.Status, rep.Summary = agent.Cancelled, "Stopped before it finished, so it wrote no report."
	case cause != nil && strings.TrimSpace(cause.Error()) != "":
		rep.Summary = "Failed before it wrote a report: " + tailText(cause.Error(), 300)
	}
	rep.Raw = rep.Summary
	c.Agents.SetReport(a.ID, rep)
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
	class := runtime.Classify(err)
	if class == runtime.ClassRateLimit || class == runtime.ClassServer {
		return true
	}
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
