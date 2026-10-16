package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/id"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/metrics"
	"github.com/relocode/relo/internal/permission"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/style"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
)

// isAlreadyGone reports whether a runtime error means the session no longer exists.
// Such errors are expected during cancel/close races and are logged at debug level.
func isAlreadyGone(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "404") || strings.Contains(s, "not found") || strings.Contains(s, "unknown session")
}

func (c *Coord) logGone(operation string, err error) {
	if isAlreadyGone(err) {
		if c.Log != nil {
			c.Log.Debug(operation+" (session already gone)", map[string]any{"error": err.Error()})
		}
		return
	}
	c.logWarn(operation, err)
}

type runtimeSession struct {
	id          runtime.SessionID
	events      <-chan runtime.Event
	generation  int
	model       string
	lastDynamic string
}

var schemaContracts sync.Map

func schemaContract(schema map[string]any) string {
	key := sortedSchemaJSON(schema)
	if cached, ok := schemaContracts.Load(key); ok {
		return cached.(string)
	}
	contract := "\n\nReturn JSON matching this schema:\n" + key
	actual, _ := schemaContracts.LoadOrStore(key, contract)
	return actual.(string)
}

func sortedSchemaJSON(schema map[string]any) string {
	keys := make([]string, 0, len(schema))
	for key := range schema {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		keyJSON, _ := json.Marshal(key)
		valueJSON, _ := json.Marshal(schema[key])
		b.Write(keyJSON)
		b.WriteByte(':')
		b.Write(valueJSON)
	}
	b.WriteByte('}')
	return b.String()
}

func (c *Coord) spawn(t taskgraph.Task, role agent.Role) agent.Agent {
	parent, _ := c.Agents.Primary()
	s := c.Current()
	mode, ws := ModeBuild, ""
	if s != nil {
		mode, ws = s.Mode, s.Workspace
	}
	settings := c.settings()
	mutating := agent.Mutating(role) && mode == ModeBuild
	model := t.Model
	if model == "" {
		model = c.modelForDifficulty(role, t.Difficulty)
	}
	a := c.Agents.Create(agent.Agent{
		ParentID: parent.ID, Role: role, Title: t.Title, TaskID: t.ID,
		Status: agent.Queued, Model: model, Scope: t.Description, Attempt: t.Attempt + 1,
		Difficulty: t.Difficulty, Workspace: ws, Worktree: ws, Budget: settings.BudgetFor(model, workerBudget(role, settings.Budget)), Window: settings.Window(model),
	})
	resolved := style.Resolve(settings.Efficiency, role, t.Difficulty, a.Attempt, a.Budget)
	if resolved.Scope.Budget != 0 {
		a.Budget = settings.BudgetFor(model, resolved.Scope.Budget)
		c.Agents.Update(a.ID, func(x *agent.Agent) { x.Budget = a.Budget })
	}
	c.mu.Lock()
	a.EscalatedFrom = c.escalations[t.ID]
	c.mu.Unlock()
	if a.EscalatedFrom != "" {
		c.Agents.Update(a.ID, func(x *agent.Agent) { x.EscalatedFrom = a.EscalatedFrom })
	}
	var stackNotes []ctxeng.Item
	if wsManager := c.workspace(); wsManager != nil {
		// An agent that builds on unaccepted work needs a copy of its own to hold it, even a read-only one.
		stack := c.stackParts(t)
		isolated := mutating || len(stack) > 0
		tree, err := wsManager.EnsureTree(a.ID, !isolated)
		if err != nil {
			c.logWarn("create worktree", err)
		} else if isolated {
			c.Agents.Update(a.ID, func(x *agent.Agent) {
				x.Workspace = tree.Path
				x.Worktree = tree.Path
			})
			a.Workspace = tree.Path
			a.Worktree = tree.Path
			if tree.Direct {
				c.noticeDirectMode()
			} else if len(stack) > 0 {
				stackNotes = c.stackInto(wsManager, a, stack)
			}
		}
	}
	c.Engine.Bind(a.ID, t.ID, string(role), a.Budget)
	if len(stackNotes) > 0 {
		c.Engine.Ingest(a.ID, stackNotes)
	}
	c.Engine.SetStrategy(a.ID, settings.Strategy)
	c.Engine.SetPhase(a.ID, phaseFor(role))
	c.emit("agent.created", a)
	return a
}

// directMutating reports whether a mutates the non-Git workspace in place.
func (c *Coord) directMutating(a agent.Agent) bool {
	if !agent.Mutating(a.Role) || c.CurrentMode() != ModeBuild {
		return false
	}
	ws := c.workspace()
	return ws != nil && ws.Direct()
}

// directSlot is the single token that serializes mutating agents in direct mode.
func (c *Coord) directSlot() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.directSem == nil {
		c.directSem = make(chan struct{}, 1)
	}
	return c.directSem
}

// noticeDirectMode posts the direct-mode warning once per session.
func (c *Coord) noticeDirectMode() {
	c.mu.Lock()
	if c.sess == nil || c.directNotified == c.sess.ID {
		c.mu.Unlock()
		return
	}
	c.directNotified = c.sess.ID
	c.mu.Unlock()
	c.chat("system", "Workspace is not a Git repository. Changes are applied directly with no isolation or undo. Run `git init` to enable isolated worktrees.", "")
}

func (c *Coord) execWorker(ctx context.Context, t taskgraph.Task, a agent.Agent) error {
	defer c.closeAgentSession(a.ID)
	defer func() {
		if a.EscalatedFrom != "" {
			c.mu.Lock()
			delete(c.escalations, a.TaskID)
			c.mu.Unlock()
		}
	}()
	if c.directMutating(a) {
		// Direct mode writes into the shared workspace: one mutating agent at a time.
		slot := c.directSlot()
		c.Agents.Update(a.ID, func(x *agent.Agent) { x.Phase, x.Activity = "waiting", "Waiting for the workspace" })
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() { <-slot }()
	}
	settings := c.settings()
	resolved := style.Resolve(settings.Efficiency, a.Role, t.Difficulty, a.Attempt, a.Budget)
	scope := (*ctxeng.Scope)(nil)
	if !(settings.Efficiency.Preset == "" || settings.Efficiency.Preset == "off") {
		sc := ctxeng.Scope{MaxItemTokens: resolved.Scope.MaxItemTokens, Memory: resolved.Scope.Memory, DependencyReportsOnly: resolved.Scope.DependencyReportsOnly, ReadOnly: resolved.Scope.ReadOnly}
		for _, cls := range resolved.Scope.Classes {
			sc.Classes = append(sc.Classes, ctxeng.Class(cls))
		}
		scope = &sc
	}
	if resolved.Scope.Budget != 0 {
		a.Budget = resolved.Scope.Budget
	}
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Starting; x.Phase = "context"; x.Activity = "Building context" })
	c.emit("agent.started", c.freshAgent(a))
	c.Engine.Ingest(a.ID, c.workerSeed(t))
	gen, err := c.Engine.Build(ctxeng.BuildRequest{
		AgentID: a.ID, TaskID: t.ID, Role: string(a.Role), Phase: phaseFor(a.Role),
		Budget: a.Budget, Reason: "worker start", Query: ctxeng.Query{Task: t.Title, Keywords: strings.Fields(t.Title + " " + t.Description), Failures: a.Role == agent.Debug},
		Scope: scope, DependencyTaskIDs: t.Depends,
	})
	if err != nil {
		c.fail(a.ID, err)
		c.failTask(t, a, err)
		return err
	}
	c.emit("context.created", map[string]any{"agentId": a.ID, "generation": gen.Number, "tokens": gen.Tokens})
	// The task, its acceptance criteria and the objective are in the context pack already; sending them a second time
	// in the message cost every agent a few hundred tokens in every call.
	parts := prompt.ComposePartsWith(a.Role, *gen, "", resolved.Profile.Fragment(a.Role))
	body, err := c.runAgent(ctx, a, parts, "Do the assigned task described in your context, then return the report as JSON.", prompt.ReportSchema)
	if err != nil {
		c.fail(a.ID, err)
		c.failTask(t, a, err)
		return err
	}
	// An agent that has answered is not run again. Its loop heuristics describe what it already did, and a second
	// run from a fresh context would only repeat the work and double its cost.
	rep, err := c.workerReport(ctx, a, parts, body)
	if err != nil {
		c.fail(a.ID, err)
		c.failTask(t, a, err)
		return err
	}
	var outcome verifyOutcome
	if c.shouldVerify(a, rep) {
		rep, outcome, err = c.verifyAndRepair(ctx, a, parts, rep)
		if err != nil {
			c.fail(a.ID, err)
			c.failTask(t, a, err)
			return err
		}
	}
	c.acceptReport(t, a, rep, outcome)
	return nil
}

// workerReport turns the model's final message into a report. One repair round is spent only when the reply looks
// like JSON that failed validation. A reply that is plain prose is kept as the summary: the work is in the
// worktree whether or not the model formatted its last message, so losing it to a parse error would be waste.
func (c *Coord) workerReport(ctx context.Context, a agent.Agent, parts prompt.Parts, body string) (RawReport, error) {
	_, ferr := ParseReport(body)
	// Only a role that has to hand back a diff is worth a repair round; the others keep what they said.
	if ferr != nil && strings.Contains(body, "{") && deliversChanges(a.Role) {
		retry, err := c.runAgent(ctx, a, parts, "Previous report failed validation: "+ferr.Error()+". Return corrected report JSON only.", prompt.ReportSchema)
		if err != nil {
			if ctx.Err() != nil {
				return RawReport{}, err
			}
			c.logWarn("repair worker report", err)
		} else {
			body = retry
			_, ferr = ParseReport(body)
		}
	}
	if ferr != nil {
		// A reply that is only a tool-call attempt (see stripToolMarkup) says nothing, whatever its bytes.
		text, strayCall := stripToolMarkup(body)
		if text == "" {
			c.logWarn("parse worker report", ferr)
			if len(c.changesFor(a, RawReport{})) > 0 {
				// The work is in the worktree; only the write-up is missing.
				return RawReport{Status: "completed", Summary: "The agent changed files but ended without writing a report.", Risks: []string{"The agent ended without a report; check its changes."}}, nil
			}
			summary := "The agent returned an empty report."
			if strayCall {
				summary = "The agent ended with a tool call instead of a report."
			}
			return RawReport{Status: "failed", Summary: summary, ValidationError: ferr.Error()}, nil
		}
		if strings.HasPrefix(text, "{") && !deliversChanges(a.Role) {
			c.logWarn("worker report was not valid JSON; keeping it as text", ferr)
			return RawReport{Status: "completed", Summary: text, Risks: []string{"The agent's report was not valid JSON; this summary is its raw reply."}}, nil
		}
		if strings.HasPrefix(text, "{") {
			// A broken report object is not a summary worth showing. If git saw no change either, there is no
			// evidence the task was done, so it fails; otherwise the work stands and only the write-up is missing.
			if len(c.changesFor(a, RawReport{})) == 0 {
				c.logWarn("parse worker report", ferr)
				return RawReport{Status: "failed", Summary: "Invalid structured worker report: " + ferr.Error(), ValidationError: ferr.Error()}, nil
			}
			return RawReport{Status: "completed", Summary: "The agent changed files but did not write a valid summary.", Risks: []string{"The agent's report was not valid: " + ferr.Error()}}, nil
		}
		c.logWarn("worker report was not structured JSON; keeping it as text", ferr)
		return RawReport{Status: "completed", Summary: text, Risks: []string{"The agent did not return a structured report; this summary is its raw reply."}}, nil
	}
	rep, perr := ParseJSON[RawReport](body)
	if perr != nil {
		return RawReport{Status: "completed", Summary: strings.TrimSpace(body)}, nil
	}
	if report, parseErr := ParseReport(body); parseErr == nil {
		rep.Status = string(report.Status)
		rep.Summary, rep.Findings, rep.Files = report.Summary, report.Findings, report.Files
		rep.Risks, rep.Next, rep.Cause = report.Risks, report.Next, report.Cause
		rep.Verify = make([]RawVerify, 0, len(report.Verify))
		for _, item := range report.Verify {
			rep.Verify = append(rep.Verify, RawVerify{Command: item.Command, Status: item.Status, Output: item.Output})
		}
	}
	return rep, nil
}

// maxRepairRounds is how many times a worker is sent back, in its own session and worktree, to fix failing checks.
const maxRepairRounds = 2

// verifyOutcome is what the project's checks said about a worker's result.
type verifyOutcome struct {
	ran     bool
	passed  bool
	failure string // tail of the failing commands' output, for the repair prompt and the report
	repairs int
}

func (c *Coord) shouldVerify(a agent.Agent, rep RawReport) bool {
	sess := c.Current()
	if sess == nil || sess.Mode != ModeBuild || !c.settings().AutoVerify || strings.EqualFold(rep.Status, "failed") {
		return false
	}
	return a.Role == agent.Worker || a.Role == agent.Debug
}

// verifyAndRepair runs the checks and, when they fail because of the agent's change, sends the agent back with the
// failure output instead of throwing the work away and starting a fresh (and pricier) attempt.
func (c *Coord) verifyAndRepair(ctx context.Context, a agent.Agent, parts prompt.Parts, rep RawReport) (RawReport, verifyOutcome, error) {
	var out verifyOutcome
	if len(c.changesFor(a, rep)) == 0 {
		return rep, out, nil // nothing was changed, so there is nothing to check
	}
	for {
		c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status, x.Phase, x.Activity = agent.Running, "verifying", "Running checks" })
		passed, failure := c.runVerify(ctx, a)
		if ctx.Err() != nil {
			return rep, out, ctx.Err()
		}
		out.ran, out.passed, out.failure = true, passed, failure
		if passed || out.repairs >= maxRepairRounds {
			return rep, out, nil
		}
		out.repairs++
		c.Agents.Update(a.ID, func(x *agent.Agent) {
			x.Status, x.Phase, x.Activity = agent.Running, "model", fmt.Sprintf("Fixing failed checks (%d/%d)", out.repairs, maxRepairRounds)
		})
		body, err := c.runAgent(ctx, a, parts, "The project's checks failed on your change:\n\n"+failure+"\n\nFix the cause in your worktree, then return the worker report JSON again.", prompt.ReportSchema)
		if err != nil {
			return rep, out, err
		}
		if next, perr := ParseJSON[RawReport](body); perr == nil && strings.TrimSpace(next.Summary) != "" {
			rep.Summary = next.Summary
			if len(next.Files) > 0 {
				rep.Files = next.Files
			}
			if next.Status != "" {
				rep.Status = next.Status
			}
		}
	}
}

func (c *Coord) clampReport(rep RawReport) RawReport {
	maxSummary := c.settings().ReportMaxChars
	if maxSummary <= 0 {
		maxSummary = 1200
	}
	clipped := false
	clip := func(s string, limit int) string {
		runes := []rune(s)
		if len(runes) <= limit {
			return s
		}
		clipped = true
		removed := len(runes) - limit
		marker := fmt.Sprintf("...[clipped %d]", removed)
		for {
			keep := max(0, limit-len([]rune(marker)))
			next := fmt.Sprintf("...[clipped %d]", len(runes)-keep)
			if len([]rune(next)) == len([]rune(marker)) {
				marker = next
				break
			}
			marker = next
		}
		keep := max(0, limit-len([]rune(marker)))
		if len([]rune(marker)) > limit {
			return string([]rune(marker)[:limit])
		}
		return string(runes[:keep]) + marker
	}
	clampList := func(items []string) []string {
		out := make([]string, 0, min(8, len(items)))
		seen := map[string]bool{}
		for _, item := range items {
			if seen[item] {
				continue
			}
			seen[item] = true
			if len(out) == 8 {
				clipped = true
				break
			}
			item = clip(item, 240)
			out = append(out, item)
		}
		return out
	}
	rep.Summary = clip(rep.Summary, maxSummary)
	rep.Cause = clip(rep.Cause, 240)
	rep.Findings, rep.Risks, rep.Next = clampList(rep.Findings), clampList(rep.Risks), clampList(rep.Next)
	if len(rep.Structured) > 8 {
		rep.Structured = rep.Structured[:8]
		clipped = true
	}
	for i := range rep.Structured {
		rep.Structured[i].Summary = clip(rep.Structured[i].Summary, 240)
		rep.Structured[i].Evidence = clampList(rep.Structured[i].Evidence)
		rep.Structured[i].RootCause = clip(rep.Structured[i].RootCause, 240)
	}
	// Rebuilt rather than edited in place: deleting while ranging over the same slice walks past its new end.
	unique := make([]RawFinding, 0, len(rep.Structured))
	for _, item := range rep.Structured {
		dup := false
		for _, kept := range unique {
			if reflect.DeepEqual(item, kept) {
				dup = true
				break
			}
		}
		if dup {
			clipped = true
			continue
		}
		unique = append(unique, item)
	}
	rep.Structured = unique
	if clipped {
		c.logWarn("clamp worker report", fmt.Errorf("report exceeded configured field limits"))
	}
	return rep
}

func (c *Coord) workerSeed(t taskgraph.Task) []ctxeng.Item {
	settings := c.settings()
	s := c.Current()
	objective := ""
	if s != nil {
		objective = s.Objective
	}
	items := []ctxeng.Item{
		{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Objective", Content: objective, Pinned: true, Durability: 1},
		{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Task", Content: t.Title + "\n" + t.Description, Durability: 1},
		{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Acceptance", Content: strings.Join(t.Acceptance, "\n"), Durability: 1},
	}
	for _, f := range c.Engine.Findings() {
		items = append(items, ctxeng.Item{
			Class: ctxeng.ClassFinding, Source: ctxeng.SourceVerified, Title: f.Summary, Content: f.Summary + "\n" + f.RootCause,
			Files: f.Files, Verified: f.Verified, Confidence: f.Confidence, Durability: 0.9, Kind: f.Kind,
		})
	}
	if settings.Memory {
		for _, e := range c.Memory.Recall(t.Title + " " + t.Description) {
			items = append(items, ctxeng.Item{Class: ctxeng.ClassMemory, Title: staleMemory + e.Title, Content: e.Body, Files: e.Files, Verified: e.Verified, Durability: 0.75})
		}
	}
	return items
}

func (c *Coord) acceptReport(t taskgraph.Task, a agent.Agent, rep RawReport, outcome verifyOutcome) {
	rep = c.clampReport(rep)
	settings := c.settings()
	st := agent.Completed
	if strings.EqualFold(rep.Status, "failed") {
		st = agent.Failed
	}
	checksFailed := outcome.ran && !outcome.passed
	if checksFailed {
		rep.Risks = append([]string{"Checks still failing after " + plural(outcome.repairs, "repair attempt") + ": " + firstLine(outcome.failure)}, rep.Risks...)
	}
	changes := c.changesFor(a, rep)
	// Git is the witness: a worker that reports success and left nothing in its worktree delivered no change, and
	// "completed" next to an empty Changes tab reads as a bug. Say so on the card and in the report the Primary reads.
	unchanged := deliversChanges(a.Role) && st == agent.Completed && !checksFailed && len(changes) == 0 && c.CurrentMode() == ModeBuild && !c.directMutating(a)
	if unchanged {
		rep.Risks = append([]string{"Changed no files."}, rep.Risks...)
	}
	report := agent.Report{Status: st, Summary: rep.Summary, Findings: rep.Findings, Files: rep.Files, Risks: rep.Risks, Next: rep.Next, Cause: rep.Cause, Raw: rep.Summary}
	for _, v := range rep.Verify {
		report.Verify = append(report.Verify, agent.Verify{Command: v.Command, Status: v.Status, Output: v.Output})
	}
	sess := c.Current()
	files := make([]string, 0, len(changes))
	for _, ch := range changes {
		files = append(files, ch.Path)
	}
	if len(files) == 0 {
		files = rep.Files
	}
	c.Agents.SetReport(a.ID, report)
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		x.Files = files
		x.Generation = c.Engine.CurrentNumber(a.ID)
		switch {
		case checksFailed:
			x.Status, x.Activity = agent.Failed, "Checks failed"
		case st == agent.Failed:
			x.Status, x.Activity = st, "Failed"
		case unchanged:
			x.Status, x.Activity = st, "Done, no changes"
		default:
			x.Status, x.Activity = st, "Done"
		}
	})
	for _, sf := range rep.Structured {
		f := c.Engine.PromoteFinding(ctxeng.Finding{
			TaskID: t.ID, AgentID: a.ID, Kind: sf.Kind, Summary: sf.Summary, Evidence: sf.Evidence,
			RootCause: sf.RootCause, Files: sf.Files, Symbols: sf.Symbols, Confidence: sf.Confidence, Verified: sf.Verified,
		})
		c.emit("finding.created", f)
	}
	for _, line := range rep.Findings {
		f := c.Engine.PromoteFinding(ctxeng.Finding{
			TaskID: t.ID, AgentID: a.ID, Kind: ctxeng.KindArchitecture, Summary: line, Confidence: 0.7, Verified: a.Role == agent.Probe,
			RootCause: rep.Cause, Files: files,
		})
		c.emit("finding.created", f)
	}
	if settings.Memory {
		c.Memory.Upsert(memory.Entry{Kind: "finding", Title: firstLine(rep.Summary), Body: ctxeng.CompressReport(rep.Summary, files, verifyLines(rep), rep.Risks, rep.Cause), Files: files, Verified: st == agent.Completed && !checksFailed && a.Role != agent.Probe, Score: 0.6})
		c.emit("memory.updated", map[string]any{"agentId": a.ID})
	}
	if parent, ok := c.Agents.Primary(); ok {
		sum := ctxeng.CompressReport(rep.Summary, files, verifyLines(rep), rep.Risks, rep.Cause)
		c.Engine.Ingest(parent.ID, []ctxeng.Item{{
			Class: ctxeng.ClassReport, Source: ctxeng.SourceParent, Title: a.Title, Content: sum,
			Files: files, Durability: 0.95, Verified: st == agent.Completed && !checksFailed, AgentID: a.ID, TaskID: t.ID,
		}})
		c.emit("context.item.promoted", map[string]any{"agentId": parent.ID, "from": a.ID})
	} else {
		c.logWarn("promote worker report", fmt.Errorf("primary agent missing"))
	}
	c.recordChanges(t, a, changes)
	gst := taskgraph.Done
	if st == agent.Failed || checksFailed {
		gst = taskgraph.Failed
	}
	updated, err := c.Graph.Update(t.ID, func(x *taskgraph.Task) {
		x.Status = gst
		x.Finished = time.Now().UTC()
		x.Report = report
	})
	if err != nil {
		c.logWarn("finish task", err)
		return
	}
	if gst == taskgraph.Done {
		c.emit("task.completed", updated)
		c.emit("agent.completed", c.freshAgent(a))
		// Review only real, isolated work: nothing to read in direct mode, trivial tasks, or read-only roles.
		if settings.AutoReview && (a.Role == agent.Worker || a.Role == agent.Debug || a.Role == agent.Architect || a.Role == agent.Integration) &&
			sess != nil && sess.Mode == ModeBuild && !c.directMutating(a) && reviewWorthwhile(t, changes) {
			c.scheduleReview(updated, a)
		}
		return
	}
	c.emit("task.failed", updated)
	c.emit("agent.failed", c.freshAgent(a))
	switch {
	case checksFailed:
		// The work is intact in the worktree; a blind retry would redo it from scratch. Say so and let the user decide.
		c.chat("system", fmt.Sprintf("%q finished, but the project's checks still fail (%s). Its changes are in the Changes tab for you to review.", t.Title, firstLine(outcome.failure)), "")
	case rep.ValidationError != "":
		c.chat("system", fmt.Sprintf("%q did not return a usable report (%s).", t.Title, firstLine(rep.Summary)), "")
	default:
		c.retryTask(updated, a)
	}
}

// changesFor lists what the agent changed. In an isolated worktree this is read from git, so it is exact whatever the
// model claims; in a shared (non-git) workspace the model's own list is all there is.
func (c *Coord) changesFor(a agent.Agent, rep RawReport) []Change {
	if !agent.Mutating(a.Role) || c.CurrentMode() != ModeBuild {
		return nil
	}
	ws := c.workspace()
	if ws == nil {
		return nil
	}
	if c.directMutating(a) {
		var out []Change
		for _, f := range rep.Files {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, Change{ID: id.New("chg"), AgentID: a.ID, Path: f, Kind: "modified", Status: "applied"})
			}
		}
		return out
	}
	found, err := ws.ChangedFiles(a.ID)
	if err != nil {
		c.logWarn("list worktree changes", err)
		return nil
	}
	out := make([]Change, 0, len(found))
	for _, f := range found {
		out = append(out, Change{ID: id.New("chg"), AgentID: a.ID, Path: f.Path, Kind: f.Kind, Added: f.Added, Deleted: f.Deleted, Status: "proposed", Worktree: a.Worktree})
	}
	return out
}

// recordChanges stores an agent's changes on the session. An earlier attempt at the same task is superseded: its
// proposals are dropped so the same file is never offered twice, and its worktree is released.
func (c *Coord) recordChanges(t taskgraph.Task, a agent.Agent, changes []Change) {
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return
	}
	var stale []string
	kept := c.sess.Changes[:0:0]
	for _, ch := range c.sess.Changes {
		if ch.TaskID == t.ID && ch.AgentID != a.ID && ch.Status == "proposed" {
			stale = append(stale, ch.AgentID)
			continue
		}
		kept = append(kept, ch)
	}
	kept2 := kept[:0:0]
	for _, ch := range kept {
		if ch.AgentID == a.ID && ch.Status == "proposed" {
			continue // recorded again below, from the worktree as it is now
		}
		kept2 = append(kept2, ch)
	}
	for i := range changes {
		changes[i].TaskID = t.ID
	}
	c.sess.Changes = append(kept2, changes...)
	snapshot := append([]Change(nil), c.sess.Changes...)
	c.mu.Unlock()
	if len(changes) > 0 || len(stale) > 0 {
		c.emit("changes.updated", snapshot)
	}
	if ws := c.workspace(); ws != nil {
		for _, id := range stale {
			_ = ws.Release(id)
		}
	}
}

// runVerify runs the project's checks in the agent's workspace. It reports whether the change is clear of blame: a
// check that cannot run here, or that fails identically on the untouched base, does not count against the agent.
func (c *Coord) runVerify(ctx context.Context, a agent.Agent) (bool, string) {
	runner := c.verifier()
	steps := verification.Plan(a.Workspace)
	if len(steps) == 0 {
		return true, ""
	}
	cmds := make([]string, 0, len(steps))
	for _, s := range steps {
		cmds = append(cmds, s.Command)
	}
	c.emit("verification.started", map[string]any{"agentId": a.ID, "commands": cmds})
	var failures []string
	for _, step := range steps {
		if ctx.Err() != nil {
			return false, ""
		}
		var res verification.Result
		if step.Skip != "" {
			res = verification.Result{ID: id.New("ver"), Command: step.Command, Dir: a.Workspace, Status: "skipped", Note: step.Skip}
		} else {
			vctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			res = runner.Run(vctx, step.Command, a.Workspace)
			cancel()
			if res.Status == "failed" && ctx.Err() == nil && c.failsOnBase(ctx, runner, step.Command) {
				res.Status, res.Note = "skipped", "fails the same way without this change"
			}
		}
		c.mu.Lock()
		if c.sess != nil {
			c.sess.Verify = append(c.sess.Verify, res)
		}
		c.mu.Unlock()
		c.emit("verification.completed", res)
		if res.Status == "failed" {
			head := fmt.Sprintf("$ %s (exit %d)", res.Command, res.Code)
			if res.Note != "" {
				head += ": " + res.Note
			}
			failures = append(failures, head+"\n"+tailText(res.Output, 3000))
		}
	}
	return len(failures) == 0, strings.Join(failures, "\n\n")
}

// failsOnBase runs a failing command once on a pristine checkout of the base commit (cached per command) to learn
// whether the failure was already there before the agent touched anything.
func (c *Coord) failsOnBase(ctx context.Context, runner verification.Runner, command string) bool {
	ws := c.workspace()
	if ws == nil || ws.Direct() {
		return false
	}
	dir, err := ws.Baseline()
	if err != nil {
		c.logWarn("baseline checkout", err)
		return false
	}
	key := dir + "\x00" + command
	c.mu.Lock()
	if c.baseline == nil {
		c.baseline = map[string]bool{}
	}
	if known, ok := c.baseline[key]; ok {
		c.mu.Unlock()
		return known
	}
	c.mu.Unlock()
	vctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	res := runner.Run(vctx, command, dir)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	failed := res.Status == "failed"
	c.mu.Lock()
	c.baseline[key] = failed
	c.mu.Unlock()
	return failed
}

func tailText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (c *Coord) runAgent(ctx context.Context, a agent.Agent, parts prompt.Parts, ask string, schema map[string]any) (string, error) {
	rt := c.RuntimeSnapshot()
	if rt == nil {
		return "", fmt.Errorf("no runtime")
	}
	sess := c.Current()
	if sess == nil {
		return "", fmt.Errorf("no session")
	}
	prov, model := routing.Split(a.Model)
	gen := c.Engine.CurrentNumber(a.ID)
	key := a.ID
	c.runtimeMu.Lock()
	session := c.runtimeSessions[key]
	if session != nil && (session.generation != gen || session.model != a.Model) {
		delete(c.runtimeSessions, key)
		c.runtimeMu.Unlock()
		c.closeRuntimeSession(rt, session)
		c.runtimeMu.Lock()
		session = nil
	}
	if session == nil {
		sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: a.Title, Directory: a.Workspace, ReadOnly: !agent.Mutating(a.Role) || sess.Mode != ModeBuild})
		if err != nil {
			c.runtimeMu.Unlock()
			return "", err
		}
		evs, err := rt.Events(ctx, sid)
		if err != nil {
			c.runtimeMu.Unlock()
			_ = rt.CloseSession(context.Background(), sid)
			return "", err
		}
		session = &runtimeSession{id: sid, events: evs, generation: gen, model: a.Model}
		c.runtimeSessions[key] = session
	}
	sid, evs := session.id, session.events
	previousDynamic := session.lastDynamic
	c.runtimeMu.Unlock()
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		x.RuntimeID = string(sid)
		x.Status = agent.Running
		x.Phase, x.Activity = "model", "Waiting for model"
		x.Generation = gen
	})
	system := parts.Static + schemaContract(schema)
	dynamic := parts.Dynamic
	if previousDynamic != "" {
		if strings.HasPrefix(dynamic, previousDynamic) {
			dynamic = dynamic[len(previousDynamic):]
		} else {
			c.logWarn("cache: dynamic prefix changed within generation", fmt.Errorf("agent %s generation %d", a.ID, gen))
		}
	}
	if dynamic != "" && ask != "" {
		dynamic += "\n\n"
	}
	promptText := dynamic + ask
	tools := runtime.ToolsFor(string(a.Role), string(sess.Mode), agent.Mutating(a.Role) && sess.Mode == ModeBuild)
	send := func(text string) error {
		return rt.SendTask(ctx, sid, runtime.AgentTask{
			AgentID: a.ID, Role: string(a.Role), Model: model, Provider: prov,
			Effort: c.settings().EffortFor(a.Model),
			System: system, Prompt: text, Tools: tools, Schema: schema, Directory: a.Workspace,
		})
	}
	if err := send(promptText); err != nil {
		return "", err
	}
	c.runtimeMu.Lock()
	if current := c.runtimeSessions[key]; current == session {
		current.lastDynamic = parts.Dynamic
	}
	c.runtimeMu.Unlock()
	var body strings.Builder
	st := &runState{tools: map[string]string{}, inputs: map[string]string{}, phase: "model", label: "Waiting for model", last: time.Now()}
	armed := st.quietLimit()
	quiet := time.NewTimer(time.Hour)
	defer quiet.Stop()
	rearm(quiet, armed)
	tick := time.NewTicker(silenceTick)
	defer tick.Stop()
	usageRecorded := false
	recoveries := 0
	for {
		select {
		case <-ctx.Done():
			c.abortAgentSession(a.ID, rt, sid)
			c.discardStream(a, st)
			return "", ctx.Err()
		case now := <-tick.C:
			c.showSilence(a, st, now)
		case <-quiet.C:
			if !st.interrupting {
				st.stalledFor = armed
			}
			if !st.interrupting && recoveries < agentRecoveries {
				// Interrupting the silent model call and telling the agent so keeps everything it has done;
				// failing the task starts it again from nothing.
				if c.interruptStalled(ctx, a, rt, sid, st) {
					recoveries++
					armed = st.quietLimit()
					rearm(quiet, armed)
					continue
				}
			}
			c.abortAgentSession(a.ID, rt, sid)
			c.discardStream(a, st)
			return "", st.stalledErr()
		case ev, ok := <-evs:
			if !ok {
				return body.String(), nil
			}
			// Any event is progress: the clock measures silence, not how long the agent has been working.
			st.last = time.Now()
			c.showProgress(a, st)
			st.noteTool(ev)
			if ev.Interrupted && st.interrupting {
				// The runtime has stopped the stuck call. The session is intact: tell the agent what happened.
				st.resume()
				body.Reset()
				c.Agents.Update(a.ID, func(x *agent.Agent) {
					if !agent.Terminal(x.Status) {
						x.Phase, x.Activity = st.phase, st.label
					}
				})
				if err := send(st.nudge); err != nil {
					c.abortAgentSession(a.ID, rt, sid)
					c.discardStream(a, st)
					return "", err
				}
				armed = st.quietLimit()
				rearm(quiet, armed)
				continue
			}
			armed = st.quietLimit()
			rearm(quiet, armed)
			c.handleEvent(ctx, a, ev, &body, st)
			c.watchContext(ctx, a, rt, sid, ev, st)
			// A terminal event can carry the provider's failure (session.execution.failed is Done and Error
			// together). The error check must come first, or the failure is returned as an empty body and
			// surfaces as "invalid JSON" instead of the real cause.
			if ev.Error == "" && (ev.Done || ev.Idle) {
				if ev.Done && ev.Usage != nil && !usageRecorded {
					c.recordUsage(a, gen, *ev.Usage, st.live)
					usageRecorded = true
				}
				if ev.JSON != "" {
					return ev.JSON, nil
				}
				return body.String(), nil
			}
			if ev.Error != "" {
				c.abortAgentSession(a.ID, rt, sid)
				c.discardStream(a, st)
				if isContextLimit(ev.Error) {
					gen, reason, unhealthy := c.Engine.MaybeRefresh(a.ID, 0.01, true)
					if unhealthy && gen != nil {
						c.emit("context.refreshed", map[string]any{"agentId": a.ID, "reason": "context-limit error: " + reason})
					} else if unhealthy {
						c.logWarn("refresh after context limit", fmt.Errorf("refresh requested but generation unavailable: %s", reason))
					}
				}
				return body.String(), fmt.Errorf("%s", ev.Error)
			}
		}
	}
}

// closeAgentSession detaches and closes the agent's runtime session. Idempotent:
// the map entry is removed before the runtime is touched, so a second call is a no-op.
func (c *Coord) closeAgentSession(agentID string) {
	c.runtimeMu.Lock()
	session := c.runtimeSessions[agentID]
	delete(c.runtimeSessions, agentID)
	c.runtimeMu.Unlock()
	if session != nil {
		c.closeRuntimeSession(c.RuntimeSnapshot(), session)
	}
}

func (c *Coord) closeRuntimeSession(rt runtime.AgentRuntime, session *runtimeSession) {
	if rt == nil || session == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.CloseSession(ctx, session.id); err != nil {
		c.logGone("close runtime session", err)
	}
}

// abortAgentSession cancels then closes the runtime session for agentID, but only if
// it is still tracked. Detaching from the map first means a concurrent close or
// StopAll cannot cause a second Cancel/Close on the same session.
func (c *Coord) abortAgentSession(agentID string, rt runtime.AgentRuntime, sid runtime.SessionID) {
	c.runtimeMu.Lock()
	session := c.runtimeSessions[agentID]
	if session == nil || session.id != sid {
		c.runtimeMu.Unlock()
		return
	}
	delete(c.runtimeSessions, agentID)
	c.runtimeMu.Unlock()
	if rt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Cancel(ctx, sid); err != nil {
		c.logGone("cancel runtime session", err)
	}
	if err := rt.CloseSession(ctx, sid); err != nil {
		c.logGone("close runtime session", err)
	}
}

// recordUsage books the turn's provider usage. streamed is the rough count sampleUsage already added to
// UsedTokens for this reply, which the exact figures replace.
func (c *Coord) recordUsage(a agent.Agent, generation int, usage runtime.Usage, streamed int) {
	u := metrics.Usage{Input: usage.Input, Output: usage.Output, Reasoning: usage.Reasoning, CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite}
	c.cacheMu.RLock()
	cache := c.Cache
	c.cacheMu.RUnlock()
	if cache == nil {
		cache = &metrics.CacheStats{}
		c.cacheMu.Lock()
		if c.Cache == nil {
			c.Cache = cache
		} else {
			cache = c.Cache
		}
		c.cacheMu.Unlock()
	}
	profileID := c.styleFor(a).Profile.ID()
	cache.AddLabeled(a.ID, generation, a.Model, string(a.Role), profileID, u)
	if c.Log != nil {
		c.Log.Debug("prompt usage", map[string]any{"agent": a.ID, "role": string(a.Role), "generation": generation, "input": u.Input, "cacheRead": u.CacheRead, "output": u.Output})
	}
	c.mu.Lock()
	if c.sess != nil {
		for name, value := range map[string]int{"input": u.Input, "output": u.Output, "reasoning": u.Reasoning, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite} {
			total, _ := c.sess.Usage[name].(int)
			c.sess.Usage[name] = total + value
		}
	}
	c.mu.Unlock()
	held := usage.Context
	if held <= 0 {
		held = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
	}
	window := c.settings().Window(a.Model)
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		x.InputTokens += u.Input
		x.OutputTokens += u.Output
		x.UsedTokens = max(0, x.UsedTokens-streamed) + u.Input + u.Output + u.Reasoning
		x.ContextTokens = held
		x.Window = window
	})
	snapshot := cache.Snapshot()
	agents := make([]map[string]any, 0, len(snapshot.Agents))
	for _, row := range snapshot.Agents {
		agents = append(agents, map[string]any{"agentId": row.AgentID, "generation": row.Generation, "model": row.Model, "role": row.Role, "profile": row.Profile, "input": row.Usage.Input, "output": row.Usage.Output, "cacheRead": row.Usage.CacheRead, "cacheWrite": row.Usage.CacheWrite, "hitRate": row.HitRate})
	}
	usageMap := func(row metrics.CacheRow) map[string]any {
		return map[string]any{"hitRate": row.HitRate, "input": row.Usage.Input, "output": row.Usage.Output, "cacheRead": row.Usage.CacheRead, "cacheWrite": row.Usage.CacheWrite}
	}
	c.emit("cache.updated", map[string]any{"session": usageMap(snapshot.Session), "agents": agents})
}

// runState is what handleEvent remembers about one model turn.
type runState struct {
	tools    map[string]string // tool call id to tool name; only the start event carries the name
	inputs   map[string]string // tool call id to what it was asked to do (its command or path), once known
	phase    string
	label    string
	reply    replyStream // Primary only: pulls the human-readable userMessage out of the streamed JSON
	streamed bool        // a chat.delta bubble is open for this agent
	lastTick time.Time
	bodyLen  int
	live     int // rough tokens of the streaming reply already added to the agent's UsedTokens

	inflight  map[string]struct{} // tool calls started and not finished, by call id
	anonymous int                 // the same for runtimes whose tool events carry no id

	last         time.Time     // when the runtime last said anything
	silent       bool          // the status line is showing how long it has been quiet
	interrupting bool          // a stuck call was interrupted and the runtime has not yet confirmed
	nudge        string        // what to tell the agent once it has
	stalledFor   time.Duration // how long the agent had been quiet when it was interrupted

	watch contextWatch // how full the context is and whether it is being summarized; see compact.go
}

// An agent is stopped when it goes quiet, never for being busy. It used to be stopped 8 minutes after it began,
// which killed an architect that was still reading code (17 model calls in, the last one 47 seconds before the
// cut) and started it again from nothing.
//
// A running tool call is not counted at all. OpenCode says nothing while one runs, and a build or a test suite can
// take as long as it takes (workers ran project suites of four to nine minutes, which looked like a freeze but was
// not), so the clock stops until the call ends. The tool's own timeout, the status line (which names the command and
// how long it has been quiet) and the Stop button are what bound it. When the model itself goes quiet, the call is
// interrupted and the agent is told, which keeps its context and its edits; only if that does not bring it back is
// it stopped.
var (
	agentIdleLimit  = 5 * time.Minute  // the model has said nothing, and no tool call is running
	agentRecoveries = 2                // how many times one turn may be interrupted and resumed before it fails
	interruptWait   = 15 * time.Second // how long the runtime has to confirm an interrupt
	silenceShown    = 30 * time.Second // quiet for this long, the status line says so
	silenceTick     = 5 * time.Second
)

// stalledError is an agent that stopped responding and could not be brought back.
type stalledError struct{ msg string }

func (e *stalledError) Error() string { return e.msg }

// noteTool keeps track of the tool calls that have started and not finished.
func (st *runState) noteTool(ev runtime.Event) {
	if st.inflight == nil {
		st.inflight = map[string]struct{}{}
	}
	if ev.Type == "session.step.started" {
		// A model call begins only after the previous step's tool calls have all ended. An end event that was lost
		// (the stream drops oversized events) must not leave the agent looking as if it were still in a tool call,
		// which would switch its silence clock off for good.
		clear(st.inflight)
		st.anonymous = 0
	}
	switch {
	case ev.ToolDone && ev.ToolID != "":
		delete(st.inflight, ev.ToolID)
		delete(st.inputs, ev.ToolID)
	case ev.ToolDone:
		if st.anonymous > 0 {
			st.anonymous--
		}
	case ev.Tool != "" && ev.ToolID != "":
		st.inflight[ev.ToolID] = struct{}{}
	case ev.Tool != "":
		st.anonymous++
	}
}

// quietLimit is how long the agent may stay silent from now on; zero means as long as it likes, because a tool
// call is running.
func (st *runState) quietLimit() time.Duration {
	switch {
	case st.interrupting:
		return interruptWait
	case len(st.inflight)+st.anonymous > 0:
		return 0
	}
	return agentIdleLimit
}

// rearm restarts the quiet timer for limit. A zero limit leaves it stopped, so nothing fires until the next event.
func rearm(t *time.Timer, limit time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	if limit > 0 {
		t.Reset(limit)
	}
}

func (st *runState) stalledErr() error {
	return &stalledError{"agent timed out: no activity for " + span(st.stalledFor)}
}

// resume clears what the interrupted turn left behind.
func (st *runState) resume() {
	st.interrupting = false
	st.inflight, st.anonymous = map[string]struct{}{}, 0
	st.phase, st.label = "model", "Waiting for model"
	st.silent, st.last = false, time.Now()
}

// span writes a silence for a person: whole seconds, unless it is shorter than one.
func span(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// stallNudge tells an interrupted agent why it was interrupted. It repeats nothing it already has.
func stallNudge(silent time.Duration) string {
	return fmt.Sprintf("The model call went %s without a response, so Relo interrupted it. Carry on with the task from where you were, and answer in the same format as before.", span(silent))
}

// interruptStalled interrupts the model call an agent has been silent on. It reports whether it asked the runtime to;
// the agent is resumed when the runtime confirms (see runAgent).
func (c *Coord) interruptStalled(ctx context.Context, a agent.Agent, rt runtime.AgentRuntime, sid runtime.SessionID, st *runState) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.Cancel(cctx, sid); err != nil {
		c.logGone("interrupt stalled agent", err)
		return false
	}
	st.interrupting, st.nudge = true, stallNudge(st.stalledFor)
	st.label = "Interrupting a silent model call"
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		if !agent.Terminal(x.Status) {
			x.Phase, x.Activity = "model", st.label
		}
	})
	c.chat("system", fmt.Sprintf("%q: the model did not respond for %s. Relo interrupted the call and told the agent to carry on.", a.Title, span(st.stalledFor)), "")
	return true
}

// showSilence puts how long an agent has been quiet in its status line, so a long build and a hung one can be told
// apart without waiting for a timeout.
func (c *Coord) showSilence(a agent.Agent, st *runState, now time.Time) {
	quiet := now.Sub(st.last)
	if quiet < silenceShown || st.interrupting {
		return
	}
	st.silent = true
	label := fmt.Sprintf("Quiet for %s · %s", span(quiet), st.label)
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		if !agent.Terminal(x.Status) {
			x.Activity = label
		}
	})
}

// showProgress takes the silence note off the status line once the runtime speaks again.
func (c *Coord) showProgress(a agent.Agent, st *runState) {
	if !st.silent {
		return
	}
	st.silent = false
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		if !agent.Terminal(x.Status) {
			x.Activity = st.label
		}
	})
}

const tickEvery = 400 * time.Millisecond

// handleEvent folds one runtime event into the agent's state. It must stay cheap: it runs for every streamed
// token, so it only touches the registry when the phase or tool changes, and only samples token usage on a clock.
func (c *Coord) handleEvent(ctx context.Context, a agent.Agent, ev runtime.Event, body *strings.Builder, st *runState) {
	settings := c.settings()
	// The terminal event repeats the whole turn's text, so it replaces what was streamed instead of adding a second
	// copy. Only reply text belongs in the body: reasoning and tool-argument deltas are progress, never output.
	if ev.Done && ev.Text != "" {
		body.Reset()
		body.WriteString(ev.Text)
	} else if ev.Phase == "" || ev.Phase == "writing" {
		if ev.Delta != "" {
			body.WriteString(ev.Delta)
			if a.Role == agent.Primary {
				c.streamReply(a, ev.Delta, st)
			}
		} else if ev.Text != "" {
			body.WriteString(ev.Text)
		}
	}
	// One history item per finished turn. Ingesting every streamed update counted the same growing text as a new
	// "repeated action" each time, which made the loop detector fire on almost every agent.
	if ev.Done && ev.Text != "" && settings.KeepHistory {
		c.Engine.Ingest(a.ID, []ctxeng.Item{{Class: ctxeng.ClassHistory, Source: ctxeng.SourceSummary, Content: ev.Text, Durability: 0.1}})
	}
	c.trackPhase(a, ev, st)
	if ev.Permission != nil {
		c.askPerm(ctx, a, ev)
	}
	c.sampleUsage(a, body, st)
}

// streamReply forwards the growing userMessage. The payload carries the whole text so far, which makes a dropped or
// coalesced frame harmless: the next one repeats everything.
func (c *Coord) streamReply(a agent.Agent, delta string, st *runState) {
	prev := st.reply.Text()
	text, changed := st.reply.Feed(delta)
	if !changed {
		return
	}
	st.streamed = true
	c.emit("chat.delta", map[string]any{"agentId": a.ID, "delta": text[len(prev):], "text": text})
}

// discardStream removes the half-written bubble when the turn that produced it failed or was rejected.
func (c *Coord) discardStream(a agent.Agent, st *runState) {
	if st != nil && st.streamed {
		st.streamed = false
		st.reply.Reset()
		c.emit("chat.discard", map[string]any{"agentId": a.ID})
	}
}

// trackPhase turns an event into the agent's visible phase and label.
func (c *Coord) trackPhase(a agent.Agent, ev runtime.Event, st *runState) {
	if ev.Done || ev.Idle || ev.Error != "" || ev.Phase == "" {
		return
	}
	if ev.ToolInput != "" && ev.ToolID != "" {
		if st.inputs == nil {
			st.inputs = map[string]string{}
		}
		st.inputs[ev.ToolID] = ev.ToolInput
	}
	started := false
	if ev.Tool != "" {
		if ev.ToolID == "" {
			started = true
		} else if _, seen := st.tools[ev.ToolID]; !seen {
			started = true
		}
		if ev.ToolID != "" {
			st.tools[ev.ToolID] = ev.Tool
		}
	}
	label := ""
	switch ev.Phase {
	case "reasoning":
		label = "Reasoning"
	case "writing":
		label = "Writing reply"
		if a.Role != agent.Primary {
			label = "Writing report"
		}
	case "tool":
		label = ev.Tool
		if label == "" {
			label = st.tools[ev.ToolID]
		}
		if label == "" {
			label = "tool"
		}
		if in := st.inputs[ev.ToolID]; in != "" {
			label += ": " + in
		}
	case "model":
		label = "Waiting for model"
	case "compacting":
		label = "Compacting context"
	case "retry":
		label = "Retrying"
		if ev.Attempt > 1 {
			label = fmt.Sprintf("Retrying (attempt %d)", ev.Attempt)
		}
	default:
		return
	}
	if ev.Phase == st.phase && label == st.label && !started {
		return
	}
	st.phase, st.label = ev.Phase, label
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		if agent.Terminal(x.Status) {
			return // a late event after Stop must not revive the agent
		}
		x.Phase, x.Activity = ev.Phase, label
		if started {
			x.Tools++
		}
	})
}

// sampleUsage reports a rough token count for the growing reply, at most every tickEvery.
func (c *Coord) sampleUsage(a agent.Agent, body *strings.Builder, st *runState) {
	if body.Len() == st.bodyLen || time.Since(st.lastTick) < tickEvery {
		return
	}
	st.lastTick, st.bodyLen = time.Now(), body.Len()
	n := ctxeng.Tokens(body.String())
	// Added on top of what earlier calls cost, so the running total never drops back to the reply size.
	delta := n - st.live
	st.live = n
	c.Agents.Update(a.ID, func(x *agent.Agent) {
		if !agent.Terminal(x.Status) {
			x.UsedTokens = max(0, x.UsedTokens+delta)
		}
	})
	c.emit("usage.updated", map[string]any{"agentId": a.ID, "tokens": n})
}

func (c *Coord) askPerm(ctx context.Context, a agent.Agent, ev runtime.Event) {
	policy := c.permissions()
	if policy == nil {
		return
	}
	title := fmt.Sprint(ev.Permission["title"])
	cmd := fmt.Sprint(ev.Permission["pattern"])
	sess := c.Current()
	workspace := ""
	if sess != nil {
		workspace = sess.Workspace
	}
	act := permission.Classify(title, cmd, "", workspace)
	dec := policy.Get(act)
	reply := permission.MapReply(dec)
	if dec == permission.Ask {
		reply = ""
		p := &PendingPerm{ID: fmt.Sprint(ev.Permission["id"]), Session: ev.Session, AgentID: a.ID, Title: title, Command: cmd, Reply: make(chan string, 1)}
		c.mu.Lock()
		if _, exists := c.pending[p.ID]; exists {
			c.mu.Unlock()
			return
		}
		c.pending[p.ID] = p
		c.mu.Unlock()
		defer func() { c.mu.Lock(); delete(c.pending, p.ID); c.mu.Unlock() }()
		c.Agents.Update(a.ID, func(x *agent.Agent) {
			if !agent.Terminal(x.Status) {
				x.Status, x.Phase, x.Activity = agent.Waiting, "approval", "Needs approval: "+title
			}
		})
		defer c.Agents.Update(a.ID, func(x *agent.Agent) {
			if x.Status == agent.Waiting && x.Phase == "approval" {
				x.Status, x.Phase, x.Activity = agent.Running, "model", "Waiting for model"
			}
		})
		c.emit("permission.asked", map[string]any{"id": p.ID, "title": title, "command": cmd, "agentId": a.ID, "session": ev.Session})
		timer := time.NewTimer(10 * time.Minute)
		defer timer.Stop()
		select {
		case reply = <-p.Reply:
		case <-ctx.Done():
			reply = "reject"
		case <-timer.C:
			reply = "reject"
		}
	}
	if oc, ok := c.RuntimeSnapshot().(interface {
		ReplyPermission(ctx context.Context, session, permID, response string) error
	}); ok {
		if err := oc.ReplyPermission(ctx, ev.Session, fmt.Sprint(ev.Permission["id"]), reply); err != nil {
			c.logWarn("reply permission", err)
		}
	}
}

// freshAgent returns the registry's current copy of a; the spawn-time value is stale after status updates.
func (c *Coord) freshAgent(a agent.Agent) agent.Agent {
	if cur, ok := c.Agents.Get(a.ID); ok {
		return cur
	}
	return a
}
