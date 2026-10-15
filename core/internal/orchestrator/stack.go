package orchestrator

import (
	"fmt"
	"strings"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
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
}

// stackParts collects the unaccepted changes of every task t builds on, oldest first.
func (c *Coord) stackParts(t taskgraph.Task) []stackPart {
	ws := c.workspace()
	if ws == nil || ws.Direct() || len(t.Depends) == 0 {
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
	for _, d := range order {
		if d.AgentID == "" {
			continue
		}
		part := stackPart{title: d.Title}
		for _, ch := range changes {
			if ch.AgentID != d.AgentID || ch.Status != "proposed" {
				continue
			}
			patch, err := ws.PatchPath(d.AgentID, ch.Path)
			if err != nil || strings.TrimSpace(patch) == "" {
				c.logWarn("read a dependency's change", fmt.Errorf("%s: %v", ch.Path, err))
				continue
			}
			part.files = append(part.files, ch)
			part.patches = append(part.patches, patch)
		}
		if len(part.patches) > 0 {
			parts = append(parts, part)
		}
	}
	return parts
}

// stackInto applies the parts to a's worktree and returns what the agent should be told about its workspace.
func (c *Coord) stackInto(ws *workspace.Manager, a agent.Agent, parts []stackPart) []ctxeng.Item {
	var patches, titles []string
	for _, p := range parts {
		patches = append(patches, p.patches...)
		titles = append(titles, fmt.Sprintf("%q", p.title))
	}
	applied, err := ws.Stack(a.ID, patches)
	if err != nil {
		c.logWarn("stack dependency changes", err)
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
		items = append(items, ctxeng.Item{Class: ctxeng.ClassCode, Source: ctxeng.SourceRepo, Title: "Diff under review", Content: diff, Durability: 1, Relevance: 1})
	}
	return items
}
