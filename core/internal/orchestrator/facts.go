package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/taskgraph"
)

// refreshSnapshot re-reads the user's working tree so that agents started from now on are cut from what the user
// has now, not from the last commit or from how the folder looked when the session began.
func (c *Coord) refreshSnapshot() {
	if ws := c.workspace(); ws != nil {
		snap, err := ws.Snapshot()
		if err != nil {
			c.noticeSnapshotFailure(err)
			return
		}
		c.noticeSnapshot(snap.HeadFallback, snap.UntrackedTruncated)
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

	if line := c.pathFacts(); line != "" {
		b.WriteString(line)
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
	return c.Graph.Settled()
}

// recalledNote heads every recalled memory. It is what was observed in an earlier session, quoted as data: the code may
// have changed since, and a recalled "the rename is done" has misled the Primary about the present.
const recalledNote = "Untrusted historical project observation; verify before use. "

// Recall limits for one prompt, per docs/design/project-memory.md §5. Tokens are estimated with ctxeng.Tokens over the
// rendered item; bytes are the rendered text. Both caps and memory.RecallLimit apply together.
const (
	recallMaxTokens = 300
	recallMaxBytes  = 1200
	// memoryExpiry is how long command evidence stays current; older command evidence marks its entry stale.
	memoryExpiry = 30 * 24 * time.Hour
)

// recallItems returns the memory fit for q as seen from root: verified entries only, whose evidence is current in that
// view. Expired command evidence marks its entry stale for every view; a view-specific mismatch only drops the entry here
// and never changes it. Result is capped by CapRecall.
func (c *Coord) recallItems(root string, q memory.RecallQuery) []ctxeng.Item {
	view := newWorktreeView(root)
	now := time.Now().UTC()
	var expired []memory.Entry
	// Freshness is judged before the entry cap, so stale matches never use up a recall slot.
	recalled := c.Memory.RecallScopedWhere(q, func(e memory.Entry) bool {
		if commandExpired(e, now) {
			expired = append(expired, e)
			return false
		}
		f, _ := memory.CheckFreshness(e, view)
		return f == memory.FreshCurrent
	})
	for _, e := range expired {
		if _, err := c.Memory.MarkStale(e.ID, e.Revision, "expired: command evidence older than 30 days"); err != nil {
			c.logWarn("memory stale", err)
		}
	}
	items := make([]ctxeng.Item, 0, len(recalled))
	for _, e := range recalled {
		items = append(items, recalledItem(e))
	}
	return ctxeng.CapRecall(items, memory.RecallLimit, recallMaxTokens, recallMaxBytes)
}

// refreshRecall replaces the recalled memory of agentID with what q recalls as seen from root. With Settings.Memory off it
// clears the group, so a prompt rendered after the flag turns off carries no recalled memory.
func (c *Coord) refreshRecall(agentID, root string, q memory.RecallQuery) {
	if !c.settings().Memory {
		c.Engine.SetRecall(agentID, nil)
		return
	}
	c.Engine.SetRecall(agentID, c.recallItems(root, q))
}

// integrationRecallQuery is the recall query for a Primary prompt after workers finish: the objective, with the files the
// completed tasks name. It is recomputed each time so recall reflects the work done, not the first turn's text.
func (c *Coord) integrationRecallQuery(s *Session) memory.RecallQuery {
	q := memory.RecallQuery{Text: s.Objective}
	seen := map[string]bool{}
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			q.Files = append(q.Files, f)
		}
	}
	for _, t := range c.Graph.List() {
		if t.Status != taskgraph.Done {
			continue
		}
		for f := range taskScopeFiles(t) {
			add(f)
		}
		for _, f := range t.Paths {
			add(f)
		}
	}
	return q
}

// commandExpired reports whether any command evidence of e was observed more than memoryExpiry before now.
func commandExpired(e memory.Entry, now time.Time) bool {
	for _, ev := range e.Evidence {
		if ev.Type == memory.EvidenceCommandResult && now.Sub(ev.ObservedAt) > memoryExpiry {
			return true
		}
	}
	return false
}

// recalledItem renders a recalled entry as untrusted data: its ID, observation time and evidence paths, then the body
// quoted. It is never Verified and never carries the verified_fact source.
func recalledItem(e memory.Entry) ctxeng.Item {
	var observed time.Time
	for _, ev := range e.Evidence {
		if ev.ObservedAt.After(observed) {
			observed = ev.ObservedAt
		}
	}
	content := fmt.Sprintf("%sID %s, observed %s, evidence %s.\nQuoted data:\n%s", recalledNote, e.ID,
		observed.Format(time.DateOnly), strings.Join(e.Files, ", "), e.Body)
	return ctxeng.Item{Class: ctxeng.ClassMemory, Source: ctxeng.SourceRecalled, Title: e.Title, Content: content, Files: e.Files, Durability: 0.8}
}

// maxPendingMemoryListed bounds the head's review queue so a large candidate backlog cannot flood the context.
const maxPendingMemoryListed = 20

// pendingMemoryItems lists the agent candidates the head may promote, reject or retire. The listing carries ids,
// revisions, kinds, evidence refs and titles only: what a memoryActions decision cites, never the entry body.
func (c *Coord) pendingMemoryItems() []ctxeng.Item {
	// Only the candidates listed below may be decided in the Primary's next turn: a decision on any other id, or on a
	// listed id at a different revision, is refused (see applyMemoryActions).
	presented := map[string]int64{}
	defer func() { c.setPresentedMemory(presented) }()
	entries, err := c.Memory.Load()
	if err != nil {
		// A failed load lists nothing, so nothing may be decided: clear the set rather than leave the previous turn's
		// listing in place.
		c.dropPresentedMemory()
		c.logWarn("load pending memory candidates", err)
		return nil
	}
	var lines []string
	overflow := 0
	for _, e := range entries {
		// Candidates may be promoted or rejected; verified agent entries may be retired. User entries are never listed.
		listable := e.State == memory.StateCandidate || (e.State == memory.StateVerified && e.Source != memory.SourceUser)
		if !listable {
			continue
		}
		if len(lines) == maxPendingMemoryListed {
			overflow++
			continue
		}
		refs := make([]string, 0, len(e.Evidence))
		for _, ev := range e.Evidence {
			refs = append(refs, ev.Ref)
		}
		presented[e.ID] = e.Revision
		lines = append(lines, fmt.Sprintf("- id %s revision %d state %s kind %s evidence [%s]: %s", e.ID, e.Revision, e.State, e.Kind, strings.Join(refs, ", "), e.Title))
	}
	if len(lines) == 0 {
		return nil
	}
	if overflow > 0 {
		lines = append(lines, fmt.Sprintf("- %d more pending candidates are not listed; decide them in a later turn.", overflow))
	}
	return []ctxeng.Item{{Source: ctxeng.SourceSystem, Kind: ctxeng.KindConstraint, Durability: 0.8,
		Content: "## Pending memory candidates (agent-proposed, not yet permanent) and verified agent entries. Decide each with memoryActions {op,id,revision,evidence,rationale}. Cite the id and revision exactly as listed; decide only listed ids. promote makes it permanent and needs its listed evidence refs; reject discards a candidate and needs no evidence; retire withdraws a listed verified entry. Core applies each decision through the head gate and refuses the rest. A turn may carry memoryActions with tasks [].\n" + strings.Join(lines, "\n")}}
}

// factsMaxFiles bounds the files whose existence the facts settle.
const factsMaxFiles = 6

// namedPath finds repository paths in free text: a few directories and a file name with an extension.
var namedPath = regexp.MustCompile("[A-Za-z0-9_][A-Za-z0-9_.+-]*(?:/[A-Za-z0-9_.+-]+)+\\.[A-Za-z][A-Za-z0-9]{0,5}\\b")

// pathFacts settles by looking which files exist, for the paths that blocked or failed tasks and the agents' messages to
// the Primary name. The Primary has no tools, so a worker's "this file does not exist" and the head's own belief that
// it does are both guesses; one session spent six turns on a source file that had vanished from the folder, the head
// first telling the user the file was in the working tree, then that agents are cut from git alone, neither true.
func (c *Coord) pathFacts() string {
	s := c.Current()
	if s == nil || s.Workspace == "" {
		return ""
	}
	var texts []string
	for _, t := range c.Graph.List() {
		if t.Status != taskgraph.Asking && t.Status != taskgraph.Failed && t.Status != taskgraph.Blocked {
			continue
		}
		texts = append(texts, t.Title, t.Description, t.Scope)
		if t.BlockedOn != nil {
			texts = append(texts, t.BlockedOn.Question)
		}
		if rep, ok := t.Report.(agent.Report); ok {
			texts = append(texts, rep.Summary, rep.Question)
		}
	}
	if p, ok := c.Agents.Primary(); ok {
		c.mu.Lock()
		for _, m := range s.PeerMessages {
			if m.To == p.ID && (m.State == "queued" || m.State == "injected") {
				texts = append(texts, m.Text)
			}
		}
		c.mu.Unlock()
	}
	seen := map[string]bool{}
	var lines []string
	for _, text := range texts {
		for _, at := range namedPath.FindAllStringIndex(text, -1) {
			// A path that continues one written before it (../x, /etc/x, ~/x, $HOME/x) is not a path in the workspace.
			if at[0] > 0 && strings.ContainsRune("/.~$", rune(text[at[0]-1])) {
				continue
			}
			rel := filepath.ToSlash(filepath.Clean(text[at[0]:at[1]]))
			if seen[rel] || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, ".relo/") {
				continue
			}
			seen[rel] = true
			if len(lines) == factsMaxFiles {
				continue
			}
			info, err := os.Stat(filepath.Join(s.Workspace, filepath.FromSlash(rel)))
			switch {
			case err == nil && !info.IsDir():
				lines = append(lines, fmt.Sprintf("`%s` is in the working tree (%d bytes)", rel, info.Size()))
			case os.IsNotExist(err):
				line := fmt.Sprintf("`%s` is NOT in the working tree", rel)
				if ws := c.workspace(); ws != nil {
					if copy, found := ws.RecoverableCopy(rel); found {
						line += fmt.Sprintf("; Relo recorded a copy in commit %s (%s), restore it with `git show %s:%s > %s`", copy.Commit[:min(7, len(copy.Commit))], copy.At.Format("2006-01-02 15:04Z"), copy.Commit[:min(12, len(copy.Commit))], rel, rel)
					}
				}
				lines = append(lines, line)
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "- Relo looked at the files that blocked or failed tasks and agent messages name: " + strings.Join(lines, "; ") + ". This is measured, not reported: it outranks what an agent or you believed.\n"
}
