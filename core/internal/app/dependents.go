package app

import (
	"fmt"
	"time"

	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/orchestrator"
)

// P4 dependent and stacked semantics. A change's dependencies are the parents whose patches were applied to its
// base; they are persisted in Change.Lineage, never inferred from a task graph that may have moved on.

// needsOf returns the change ids this change builds on. Lineage is authoritative; Needs is the derived copy kept
// for the wire format and for rows written before Lineage existed.
func needsOf(ch orchestrator.Change) []string {
	deps := ch.Lineage
	if len(deps) == 0 {
		return dedupeIDs(ch.Needs)
	}
	out := make([]string, 0, len(deps))
	seen := map[string]bool{}
	for _, d := range deps {
		if d.ID == "" || seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		out = append(out, d.ID)
	}
	return out
}

func dedupeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// unmetNeeds returns the parents that are not accepted yet, or that no longer exist.
func (a *App) unmetNeeds(ch orchestrator.Change) []string {
	changes := a.Coord.Changes()
	var pending []string
	for _, need := range needsOf(ch) {
		found := false
		for _, other := range changes {
			if other.ID != need {
				continue
			}
			found = true
			if other.Status != "accepted" {
				pending = append(pending, need)
			}
			break
		}
		if !found {
			pending = append(pending, need)
		}
	}
	return pending
}

// dependentChanges returns the transitive dependents of id that still wait for a decision.
func (a *App) dependentChanges(id string) []orchestrator.Change {
	changes := a.Coord.Changes()
	queue := []string{id}
	seen := map[string]bool{id: true}
	var out []orchestrator.Change
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, ch := range changes {
			if ch.Status != "proposed" || seen[ch.ID] || !containsID(needsOf(ch), parent) {
				continue
			}
			seen[ch.ID] = true
			out = append(out, ch)
			queue = append(queue, ch.ID)
		}
	}
	return out
}

// markDependentsLocked records a dep-reject/broken-parent state on every transitive dependent, keeping them
// proposed and leaving their Lineage untouched.
func (a *App) markDependentsLocked(parentID, kind, reason string) {
	deps := a.dependentChanges(parentID)
	if len(deps) == 0 {
		return
	}
	now := time.Now().UnixMilli()
	ids := make([]string, 0, len(deps))
	for _, d := range deps {
		ids = append(ids, d.ID)
	}
	a.Coord.UpdateChanges(ids, func(c *orchestrator.Change) {
		c.Integration = &orchestrator.Integration{State: "blocked", Kind: kind, Reason: reason, CheckedAt: now}
	})
}

// rejectWithDependents rejects a parent and every transitive dependent. It never accepts or deletes a dependent.
// The batch is marked with one store update; git cleanup runs after decisionMu is released.
func (a *App) rejectWithDependents(id string) (any, error) {
	a.decisionMu.Lock()
	target, ok := a.Coord.FindChange(id)
	if !ok {
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "not_found", M: "change not found"}
	}
	if target.Status == "rejected" {
		a.decisionMu.Unlock()
		return map[string]any{"done": 0, "failed": []bulkFailure{}, "changes": a.Coord.Changes()}, nil
	}
	if target.Status != "proposed" {
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "invalid_state", M: "this change is not waiting for a decision"}
	}
	deps := a.dependentChanges(id)
	ids := append([]string{id}, changeIDs(deps)...)
	decisions := make([]rejectDecision, 0, len(ids))
	failed := []bulkFailure{}
	for _, changeID := range ids {
		ch, found := a.Coord.FindChange(changeID)
		if !found {
			failed = append(failed, bulkFailure{ID: changeID, Reason: "change not found"})
			continue
		}
		if ch.AgentID == "" || ch.Path == "" {
			failed = append(failed, bulkFailure{ID: changeID, Path: ch.Path, Reason: "change lacks an agent or path"})
			continue
		}
		decisions = append(decisions, rejectDecision{changeID: ch.ID, agentID: ch.AgentID, worktree: ch.Worktree, path: ch.Path})
	}
	jobs := a.rejectDecisionsLocked(decisions)
	a.decisionMu.Unlock()
	if len(jobs) > 0 {
		a.cleanupMu.Lock()
		a.runRejects(jobs)
	}
	return map[string]any{"done": len(decisions), "failed": failed, "changes": a.Coord.Changes()}, nil
}

func changeIDs(changes []orchestrator.Change) []string {
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		out = append(out, ch.ID)
	}
	return out
}

// componentsOf groups change indices by the transitive Needs relation. Components are returned in the stable order
// of their first member.
func componentsOf(changes []orchestrator.Change) [][]int {
	n := len(changes)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	index := make(map[string]int, n)
	for i, ch := range changes {
		index[ch.ID] = i
	}
	for i, ch := range changes {
		for _, need := range needsOf(ch) {
			if j, ok := index[need]; ok {
				union(i, j)
			}
		}
	}
	order := []int{}
	groups := map[int][]int{}
	for i := range changes {
		root := find(i)
		if _, ok := groups[root]; !ok {
			order = append(order, root)
		}
		groups[root] = append(groups[root], i)
	}
	out := make([][]int, 0, len(order))
	for _, root := range order {
		out = append(out, groups[root])
	}
	return out
}

type groupPartial struct {
	Accepted      []string `json:"accepted"`
	Failed        []string `json:"failed"`
	RestoreFailed []string `json:"restoreFailed,omitempty"`
}

// groupWithParents collects the proposed ancestors of id in dependency order. An accepted parent needs no write.
func (a *App) groupWithParents(id string) ([]orchestrator.Change, []bulkFailure) {
	changes := a.Coord.Changes()
	byID := make(map[string]orchestrator.Change, len(changes))
	for _, ch := range changes {
		byID[ch.ID] = ch
	}
	target, ok := byID[id]
	if !ok {
		return nil, []bulkFailure{{ID: id, Reason: "change not found"}}
	}
	var members []orchestrator.Change
	seen := map[string]bool{}
	var visit func(orchestrator.Change) []bulkFailure
	visit = func(ch orchestrator.Change) []bulkFailure {
		if seen[ch.ID] {
			return nil
		}
		seen[ch.ID] = true
		for _, need := range needsOf(ch) {
			parent, ok := byID[need]
			if !ok {
				return []bulkFailure{{ID: ch.ID, Reason: fmt.Sprintf("parent %s no longer exists", need)}}
			}
			switch parent.Status {
			case "accepted":
				continue
			case "proposed":
				if fails := visit(parent); len(fails) > 0 {
					return fails
				}
			default:
				return []bulkFailure{{ID: ch.ID, Reason: fmt.Sprintf("parent %s is %s", need, parent.Status)}}
			}
		}
		members = append(members, ch)
		return nil
	}
	if fails := visit(target); len(fails) > 0 {
		return nil, fails
	}
	ordered, fails := orderGroup(members)
	if len(fails) > 0 {
		return nil, fails
	}
	return ordered, nil
}

// orderGroup sorts members so every parent is written before its dependent.
func orderGroup(members []orchestrator.Change) ([]orchestrator.Change, []bulkFailure) {
	byID := make(map[string]orchestrator.Change, len(members))
	for _, ch := range members {
		byID[ch.ID] = ch
	}
	const (
		unseen = iota
		visiting
		done
	)
	state := map[string]int{}
	var out []orchestrator.Change
	var fails []bulkFailure
	var visit func(orchestrator.Change) bool
	visit = func(ch orchestrator.Change) bool {
		switch state[ch.ID] {
		case done:
			return true
		case visiting:
			fails = append(fails, bulkFailure{ID: ch.ID, Reason: "dependency cycle"})
			return false
		}
		state[ch.ID] = visiting
		for _, need := range needsOf(ch) {
			if parent, ok := byID[need]; ok {
				if !visit(parent) {
					return false
				}
			}
		}
		state[ch.ID] = done
		out = append(out, ch)
		return true
	}
	for _, ch := range members {
		if !visit(ch) {
			return nil, fails
		}
	}
	return out, nil
}

// autoEligibleFor applies the P4 parent rule only while the phase flag is on.
func (a *App) autoEligibleFor(ch orchestrator.Change) bool {
	if a.dependent.Load() {
		return a.autoEligibleP4(ch)
	}
	return autoEligible(ch)
}

// autoEligibleP4 is autoEligible plus the dependent rule: every parent must already be accepted.
func (a *App) autoEligibleP4(ch orchestrator.Change) bool {
	if !autoEligible(ch) {
		return false
	}
	if !a.dependent.Load() {
		return true
	}
	return len(a.unmetNeeds(ch)) == 0
}

// planGroup checks every member before any write. A conflict in one member aborts the whole group.
func (a *App) planGroup(members []orchestrator.Change) ([]orchestrator.Change, []bulkFailure) {
	ordered, fails := orderGroup(members)
	if len(fails) > 0 {
		return nil, fails
	}
	inGroup := map[string]bool{}
	for _, ch := range ordered {
		inGroup[ch.ID] = true
	}
	// A dependent's base already holds its parents' patches. Check each member against the bytes the earlier
	// members of this plan would leave behind, so a parent that is still only planned is not read as an overlap.
	planned := map[string]plannedOurs{}
	for _, ch := range ordered {
		current, ok := a.Coord.FindChange(ch.ID)
		if !ok {
			return nil, []bulkFailure{{ID: ch.ID, Path: ch.Path, Reason: "change not found"}}
		}
		if current.Status != "proposed" {
			return nil, []bulkFailure{{ID: current.ID, Path: current.Path, Reason: "this change is not waiting for a decision"}}
		}
		for _, need := range needsOf(current) {
			if inGroup[need] {
				continue
			}
			parent, ok := a.Coord.FindChange(need)
			if !ok || parent.Status != "accepted" {
				status := "missing"
				if ok {
					status = parent.Status
				}
				return nil, []bulkFailure{{ID: current.ID, Path: current.Path, Reason: fmt.Sprintf("parent %s is %s", need, status)}}
			}
		}
		var side *plannedOurs
		if prior, ok := planned[current.Path]; ok {
			side = &prior
		}
		analysis, err := a.analyzeChangeAgainst(current, side)
		if err != nil {
			a.blockChange(current.ID, "check_failed", err.Error())
			return nil, []bulkFailure{{ID: current.ID, Path: current.Path, Reason: err.Error()}}
		}
		if analysis.Reason != "" {
			now := time.Now().UnixMilli()
			reason := analysis.Reason
			a.Coord.UpdateChange(current.ID, func(c *orchestrator.Change) {
				c.Conflict = reason
				c.Integration = &orchestrator.Integration{
					State: "conflicted", Kind: analysis.Kind, Reason: reason, Ours: analysis.Ours,
					Hunks: nil, CheckedAt: now, Fingerprint: analysis.Fingerprint,
				}
			})
			return nil, []bulkFailure{{ID: current.ID, Path: current.Path, Reason: reason}}
		}
		switch analysis.Action {
		case "created", "updated", "merged":
			planned[current.Path] = plannedOurs{data: analysis.Merged, exists: true}
		case "deleted":
			planned[current.Path] = plannedOurs{exists: false}
		}
	}
	return ordered, nil
}

// acceptOrderedLocked writes a planned group in dependency order. A write failure rolls every earlier write back
// from the journal; an unrestorable member is reported as partial, never as success.
func (a *App) acceptOrderedLocked(ordered []orchestrator.Change) (accepted []string, failures []bulkFailure, partial *groupPartial, actions map[string]string) {
	actions = map[string]string{}
	var written []string
	for _, ch := range ordered {
		outcome, err := a.mergeChange(ch)
		if err != nil {
			failures = append(failures, bulkFailure{ID: ch.ID, Path: ch.Path, Reason: err.Error()})
			restored, notRestored := a.rollbackAcceptedLocked(written)
			accepted = notRestored
			partial = &groupPartial{
				Accepted:      append([]string(nil), notRestored...),
				Failed:        []string{ch.ID},
				RestoreFailed: append([]string(nil), notRestored...),
			}
			_ = restored
			if len(notRestored) > 0 {
				a.Coord.UpdateChanges(notRestored, func(c *orchestrator.Change) {
					c.Status, c.Conflict, c.Resolving = "accepted", "", false
				})
			}
			a.persist()
			return accepted, failures, partial, actions
		}
		written = append(written, ch.ID)
		if outcome.Action != "" {
			actions[ch.ID] = outcome.Action
		}
	}
	a.Coord.UpdateChanges(written, func(c *orchestrator.Change) { c.Status, c.Conflict, c.Resolving = "accepted", "", false })
	for action, id := range actions {
		a.Coord.UpdateChange(id, func(c *orchestrator.Change) { c.Landed = action })
	}
	// Settling waits on cleanupMu, so callers do it after releasing decisionMu.
	if len(written) > 0 {
		a.persist()
	}
	return written, nil, nil, actions
}

// acceptedAgents returns the set of agents for changes that are now accepted, for settling after decisionMu.
func (a *App) acceptedAgents(ids []string) map[string]bool {
	agents := map[string]bool{}
	for _, id := range ids {
		if ch, ok := a.Coord.FindChange(id); ok && ch.AgentID != "" {
			agents[ch.AgentID] = true
		}
	}
	return agents
}

func (a *App) rollbackAcceptedLocked(ids []string) (restored, notRestored []string) {
	for i := len(ids) - 1; i >= 0; i-- {
		if _, err := a.Coord.WS.UndoChange(ids[i]); err != nil {
			a.Log.Warn("roll back group member", map[string]any{"changeId": ids[i], "error": err.Error()})
			notRestored = append(notRestored, ids[i])
			continue
		}
		restored = append(restored, ids[i])
	}
	return restored, notRestored
}

// acceptWithParents is the "Accept with parents" action: plan the target and its proposed ancestors, then write
// them in dependency order.
func (a *App) acceptWithParents(id string) (any, error) {
	a.decisionMu.Lock()
	members, fails := a.groupWithParents(id)
	if len(fails) > 0 {
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "needs_parent", M: fails[0].Reason}
	}
	ordered, fails := a.planGroup(members)
	if len(fails) > 0 {
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "integration_conflict", M: fails[0].Reason + ". Nothing was changed."}
	}
	accepted, failures, partial, _ := a.acceptOrderedLocked(ordered)
	a.decisionMu.Unlock()
	agents := a.acceptedAgents(accepted)
	for agentID := range agents {
		a.settle(agentID)
	}
	paths := make([]string, 0, len(accepted))
	for _, id := range accepted {
		if ch, ok := a.Coord.FindChange(id); ok {
			paths = append(paths, ch.Path)
		}
	}
	if len(paths) > 0 {
		a.projectPaths(paths)
	}
	out := map[string]any{"done": len(accepted), "failed": failures, "changes": a.Coord.Changes(), "group": changeIDs(ordered)}
	if partial != nil {
		out["partial"] = partial
	}
	if len(accepted) == 0 && len(failures) > 0 {
		return nil, ipc.Fail{C: "integration_failed", M: failures[0].Reason}
	}
	return out, nil
}

// rebaseChange projects a change without its parents. With apply it then accepts the clean result and forgets the
// recorded parents; a conflict writes nothing and leaves the change blocked.
func (a *App) rebaseChange(id string, apply bool) (any, error) {
	a.decisionMu.Lock()
	ch, ok := a.Coord.FindChange(id)
	if !ok {
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "not_found", M: "change not found"}
	}
	if ch.Status != "proposed" {
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "invalid_state", M: "this change is not waiting for a decision"}
	}
	analysis, err := a.analyzeChange(ch)
	if err != nil {
		a.blockChange(ch.ID, "check_failed", err.Error())
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "integration_failed", M: err.Error()}
	}
	now := time.Now().UnixMilli()
	if analysis.Reason != "" {
		a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) {
			c.Conflict = analysis.Reason
			c.Integration = &orchestrator.Integration{
				State: "conflicted", Kind: analysis.Kind, Reason: analysis.Reason, Ours: analysis.Ours,
				Hunks: integrationHunks(analysis.Hunks), CheckedAt: now, Fingerprint: analysis.Fingerprint,
			}
		})
		a.decisionMu.Unlock()
		return nil, ipc.Fail{C: "integration_conflict", M: analysis.Reason + ". Nothing was changed. The agent authored this change with its parent present."}
	}
	a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) {
		c.Conflict = ""
		c.Integration = &orchestrator.Integration{State: "clean", Ours: analysis.Ours, CheckedAt: now, Fingerprint: analysis.Fingerprint}
	})
	if !apply {
		a.decisionMu.Unlock()
		return map[string]any{"change": ch, "clean": true}, nil
	}
	a.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) { c.Lineage, c.Needs = nil, nil })
	out, err, path, agentID := a.acceptChangeLocked(id, "")
	a.decisionMu.Unlock()
	if err == nil && agentID != "" {
		a.settle(agentID)
	}
	if err == nil && path != "" {
		a.projectPaths([]string{path})
	}
	return out, err
}
