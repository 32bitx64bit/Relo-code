package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/relocode/relo/internal/taskgraph"
)

// refreshSnapshot re-reads the user's working tree so that agents started from now on are cut from what the user
// has now, not from the last commit or from how the folder looked when the session began.
func (c *Coord) refreshSnapshot() {
	if ws := c.workspace(); ws != nil {
		if _, err := ws.Snapshot(); err != nil {
			c.logWarn("workspace snapshot", err)
		}
	}
}

// Caps keep the facts short: they go to the Primary on every turn.
const (
	factsMaxPaths  = 10
	factsMaxChecks = 6
)

// workspaceFacts is what Relo measured itself about the repository and the work in flight, for the Primary.
//
// Without it the Primary has only worker reports to go on. Reports disagree whenever agents look at different
// copies of the code (a probe reads the user's folder, a worker edits its own worktree), and a Primary that cannot
// tell which is right plans another round of probes to find out, then another. Facts from git and from the check
// runner cannot disagree, so they end that loop.
func (c *Coord) workspaceFacts() string {
	var b strings.Builder
	b.WriteString("## Workspace facts (measured by Relo just now; they outrank any worker report, memory or earlier message)\n")
	ws := c.workspace()
	switch {
	case ws == nil:
		b.WriteString("- No workspace is attached.\n")
	case ws.Direct():
		b.WriteString("- The folder is not a Git repository. Workers edit it directly; there is nothing to review or accept.\n")
	default:
		snap := ws.Base()
		dirty := append(append([]string(nil), snap.Dirty...), snap.Untracked...)
		head := snap.Head
		if len(head) > 7 {
			head = head[:7]
		}
		if len(dirty) == 0 {
			fmt.Fprintf(&b, "- Git: branch %s at %s, working tree clean.\n", orUnknown(snap.Branch), orUnknown(head))
		} else {
			fmt.Fprintf(&b, "- Git: branch %s at %s. The user's working tree has %d uncommitted path(s): %s. Agents start from this tree, not from the last commit.\n",
				orUnknown(snap.Branch), orUnknown(head), len(dirty), pathList(dirty))
		}
	}

	waiting, applied := c.changesByTask()
	for _, line := range waiting {
		b.WriteString("- Waiting in the Changes tab, not yet in the working tree: " + line + "\n")
	}
	for _, line := range applied {
		b.WriteString("- Accepted into the working tree: " + line + "\n")
	}
	if len(waiting) == 0 && len(applied) == 0 {
		b.WriteString("- No worker has changed any file yet.\n")
	}

	checks := c.latestChecks()
	if len(checks) == 0 {
		b.WriteString("- Project checks: none have run.\n")
	}
	for _, line := range checks {
		b.WriteString("- Check " + line + "\n")
	}

	if cmds := c.projectCommands(); cmds != "" {
		b.WriteString("- Project commands: " + cmds + "\n")
	}

	b.WriteString("Worker edits wait in the Changes tab, so a probe, which reads the user's folder, cannot see them yet. Relo reads changed files from git and runs the project's checks itself.\n")
	return b.String()
}

// factsMaxCommands bounds the project's commands in the facts.
const factsMaxCommands = 14

var makeTarget = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_. -]*?)\s*:($|[^=])`)

// projectCommands lists the Makefile targets and package.json scripts at the workspace root.
//
// A task like "rebuild and install the client" sent a Sonnet worker through ten model calls to find out that the
// repository has a Makefile; with the targets in front of it, the Primary can put the exact command in the task and
// the worker runs it.
func (c *Coord) projectCommands() string {
	s := c.Current()
	if s == nil || s.Workspace == "" {
		return ""
	}
	var cmds []string
	seen := map[string]bool{}
	add := func(cmd string) {
		if !seen[cmd] {
			seen[cmd] = true
			cmds = append(cmds, cmd)
		}
	}
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		data, err := os.ReadFile(filepath.Join(s.Workspace, name))
		if err != nil {
			continue
		}
		var targets []string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, ".PHONY:") {
				targets = append(targets, strings.Fields(strings.TrimPrefix(line, ".PHONY:"))...)
			} else if m := makeTarget.FindStringSubmatch(line); m != nil && !strings.HasPrefix(line, ".") {
				targets = append(targets, strings.Fields(m[1])...)
			}
		}
		for _, t := range targets {
			if !strings.ContainsAny(t, "$%/") {
				add("make " + t)
			}
		}
		break
	}
	if data, err := os.ReadFile(filepath.Join(s.Workspace, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			names := make([]string, 0, len(pkg.Scripts))
			for name := range pkg.Scripts {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				add("npm run " + name)
			}
		}
	}
	if len(cmds) == 0 {
		return ""
	}
	if len(cmds) > factsMaxCommands {
		cmds = cmds[:factsMaxCommands]
	}
	return strings.Join(cmds, ", ") + "."
}

// changesByTask summarises the session's changes, one line per task, split into those still waiting for the user
// and those already accepted. Rejected changes are not part of the repository's story and are left out.
func (c *Coord) changesByTask() (waiting, applied []string) {
	type tally struct {
		files          []string
		added, deleted int
	}
	var order []string
	byKey := map[string]*tally{}
	add := func(status string, ch Change) {
		key := status + "\x00" + ch.TaskID
		t := byKey[key]
		if t == nil {
			t = &tally{}
			byKey[key] = t
			order = append(order, key)
		}
		t.files = append(t.files, ch.Path)
		t.added += ch.Added
		t.deleted += ch.Deleted
	}
	for _, ch := range c.Changes() {
		switch ch.Status {
		case "proposed", "accepted", "applied":
			add(ch.Status, ch)
		}
	}
	for _, key := range order {
		status, taskID, _ := strings.Cut(key, "\x00")
		t := byKey[key]
		title := taskID
		if task, ok := c.Graph.Get(taskID); ok {
			title = task.Title
		}
		line := fmt.Sprintf("%q: %s (+%d -%d): %s", title, plural(len(t.files), "file"), t.added, t.deleted, pathList(t.files))
		if status == "proposed" {
			waiting = append(waiting, line)
		} else {
			applied = append(applied, line)
		}
	}
	return waiting, applied
}

// latestChecks lists the newest result of each project check run in this session.
func (c *Coord) latestChecks() []string {
	var order []string
	last := map[string]string{}
	for _, r := range c.sessVerify() {
		line := fmt.Sprintf("%s: %s", r.Command, r.Status)
		if r.Note != "" {
			line += " (" + r.Note + ")"
		}
		if _, seen := last[r.Command]; !seen {
			order = append(order, r.Command)
		}
		last[r.Command] = line
	}
	if len(order) > factsMaxChecks {
		order = order[len(order)-factsMaxChecks:]
	}
	out := make([]string, 0, len(order))
	for _, cmd := range order {
		out = append(out, last[cmd])
	}
	return out
}

func pathList(paths []string) string {
	if len(paths) <= factsMaxPaths {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:factsMaxPaths], ", ") + fmt.Sprintf(", and %d more", len(paths)-factsMaxPaths)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// settled reports whether every task has finished, one way or another.
func (c *Coord) settled() bool {
	for _, t := range c.Graph.List() {
		if t.Status == taskgraph.Queued || t.Status == taskgraph.Running || t.Status == taskgraph.Blocked {
			return false
		}
	}
	return true
}

// staleMemory prefixes what memory recalls. It is what a worker reported in an earlier session, true then; the
// code may have changed since, and a recalled "the rename is done" has misled the Primary about the present.
const staleMemory = "From an earlier session, may be out of date: "
