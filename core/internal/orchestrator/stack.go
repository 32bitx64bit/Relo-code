package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

// A worker's edits stay in its own worktree until the user accepts them. A task that depends on that worker (the
// tests for it, the review of it, the next step that builds on it) would otherwise start from a copy of the
// user's tree that does not contain the change, and test, review or extend code that is not there.

// maxDiffChars bounds the diff handed to a reviewer; the files themselves are there for the rest.
const maxDiffChars = 20000

type stackPart struct {
	title   string
	files   []Change
	patches []string
	hashes  []string
}

// stackParts collects the unaccepted changes of every task t builds on, oldest first.
func (c *Coord) stackParts(t taskgraph.Task) []stackPart {
	return c.stackPartsWith(t, nil)
}

// stackPartsWith adds the pending changes of earlier path holders. For a required dependency every path is stacked;
// for a lease holder only the paths the two tasks declared overlap, because the lease promised nothing else.
func (c *Coord) stackPartsWith(t taskgraph.Task, soft []scheduler.SoftDep) []stackPart {
	ws := c.workspace()
	if ws == nil || ws.Direct() || (len(t.Depends) == 0 && len(soft) == 0) {
		return nil
	}
	var order []taskgraph.Task
	seen := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		d, ok := c.Graph.Get(id)
		if !ok {
			return
		}
		for _, dep := range d.Depends {
			visit(dep)
		}
		order = append(order, d)
	}
	for _, dep := range t.Depends {
		visit(dep)
	}
	changes := c.Changes()
	var parts []stackPart
	add := func(d taskgraph.Task, only []string) {
		if d.AgentID == "" {
			return
		}
		part := stackPart{title: d.Title}
		for _, ch := range changes {
			if ch.AgentID != d.AgentID || ch.Status != "proposed" {
				continue
			}
			if len(only) > 0 && !declaredPath(ch.Path, only) {
				continue
			}
			patch, err := ws.PatchPath(d.AgentID, ch.Path)
			if err != nil || strings.TrimSpace(patch) == "" {
				c.logWarn("read a dependency's change", fmt.Errorf("%s: %v", ch.Path, err))
				continue
			}
			part.files = append(part.files, ch)
			part.patches = append(part.patches, patch)
			sum := sha256.Sum256([]byte(patch))
			part.hashes = append(part.hashes, hex.EncodeToString(sum[:]))
		}
		if len(part.patches) > 0 {
			parts = append(parts, part)
		}
	}
	for _, d := range order {
		add(d, nil)
	}
	for _, sd := range soft {
		if sd.TaskID == t.ID || seen[sd.TaskID] {
			continue
		}
		seen[sd.TaskID] = true
		d, ok := c.Graph.Get(sd.TaskID)
		if !ok {
			continue
		}
		add(d, sd.Paths)
	}
	return parts
}

// declaredPath reports whether a change is one of the paths a task declared, including files below a declared
// directory.
func declaredPath(changePath string, declared []string) bool {
	for _, d := range declared {
		d = strings.TrimSpace(strings.ReplaceAll(d, "\\", "/"))
		if d == "" {
			continue
		}
		d = strings.TrimSuffix(d, "/")
		if changePath == d || strings.HasPrefix(changePath, d+"/") {
			return true
		}
	}
	return false
}

// leaseLineage adds the pending changes of earlier path holders that are not recorded yet, so a queued task that is
// re-cut onto a fresh base stacks the work it waited for.
func (c *Coord) leaseLineage(a agent.Agent, soft []scheduler.SoftDep) {
	if len(soft) == 0 {
		return
	}
	ws := c.workspace()
	if ws == nil || ws.Direct() {
		return
	}
	current := c.lineageOf(a.ID)
	next := append([]Dep(nil), current...)
	index := map[string]int{}
	for i, dep := range current {
		index[dep.ID] = i
	}
	changed := false
	changes := c.Changes()
	for _, sd := range soft {
		if sd.TaskID == a.TaskID {
			continue
		}
		holder, ok := c.Graph.Get(sd.TaskID)
		if !ok || holder.AgentID == "" {
			continue
		}
		for _, ch := range changes {
			if ch.AgentID != holder.AgentID || ch.Status != "proposed" {
				continue
			}
			if !declaredPath(ch.Path, sd.Paths) {
				continue
			}
			patch, err := ws.PatchPath(ch.AgentID, ch.Path)
			if err != nil || strings.TrimSpace(patch) == "" {
				continue
			}
			sum := sha256.Sum256([]byte(patch))
			hash := hex.EncodeToString(sum[:])
			if i, recorded := index[ch.ID]; recorded {
				if next[i].PatchHash != hash {
					next[i].PatchHash = hash
					changed = true
				}
				continue
			}
			next = append(next, Dep{ID: ch.ID, PatchHash: hash})
			index[ch.ID] = len(next) - 1
			changed = true
		}
	}
	if changed {
		c.setLineage(a.ID, next)
	}
}

// stackInto applies the parts to a's worktree and returns what the agent should be told about its workspace, plus
// the dependencies that actually applied. Those become the change's Lineage and Needs.
func (c *Coord) stackInto(ws *workspace.Manager, a agent.Agent, parts []stackPart) ([]ctxeng.Item, []Dep) {
	var patches, titles []string
	var deps []Dep
	for _, p := range parts {
		patches = append(patches, p.patches...)
		titles = append(titles, fmt.Sprintf("%q", p.title))
	}
	results, err := ws.Stack(a.ID, patches)
	if err != nil {
		c.logWarn("stack dependency changes", err)
	}
	applied, index := 0, 0
	for _, p := range parts {
		for i := range p.patches {
			if index < len(results) && results[index].Applied {
				applied++
				if i < len(p.hashes) && i < len(p.files) {
					deps = append(deps, Dep{ID: p.files[i].ID, PatchHash: p.hashes[i]})
				}
			}
			index++
		}
	}
	note := "This workspace is the user's working tree with the changes of " + strings.Join(titles, ", ") + " already applied, although the user has not accepted them yet. Test, review or build on that."
	if err != nil {
		note = fmt.Sprintf("This workspace is the user's working tree with %d of %d patches from %s applied (%s). Whatever is missing is described in those tasks' reports.", applied, len(patches), strings.Join(titles, ", "), firstLine(err.Error()))
	}
	items := []ctxeng.Item{{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Kind: ctxeng.KindConstraint, Title: "Workspace", Content: note, Pinned: true, Durability: 1}}
	if a.Role == agent.Reviewer {
		diff := strings.Join(patches, "\n")
		if len(diff) > maxDiffChars {
			diff = diff[:maxDiffChars] + "\n... (diff cut here; read the files for the rest)"
		}
		var paths []string
		seen := map[string]bool{}
		for _, p := range parts {
			for _, f := range p.files {
				if !seen[f.Path] {
					seen[f.Path] = true
					paths = append(paths, f.Path)
				}
			}
		}
		items = append(items, ctxeng.Item{Class: ctxeng.ClassCode, Source: ctxeng.SourceRepo, Title: "Diff under review", Content: diff, Files: paths, Durability: 1, Relevance: 1, From: c.deriveFiles(a.ID, paths)})
	}
	return items, deps
}
