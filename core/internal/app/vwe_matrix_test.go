package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/workspace"
)

// The correctness matrix of the virtual workspace design, run through the real propose, accept, reject and undo
// flow with virtual workspaces forced on, whatever mode the suite was started in. Rows the lower layers pin down
// on their own (interrupted overlay writes, symlinks and protected paths, recovery of refs) are listed next to
// their tests in docs/virtual-workspaces.md.

func virtualCore(t *testing.T, files map[string]string) (*App, string) {
	t.Helper()
	core, dir := newGitCore(t, files)
	core.applySettings(map[string]any{"workspaceMode": "virtual"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	return core, dir
}

// toolWrites runs fn in the agent's directory, which exists only while a tool needs it.
func toolWrites(t *testing.T, core *App, agentID string, fn func(dir string)) {
	t.Helper()
	tree, err := core.Coord.WS.EnsureTree(agentID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Virtual {
		t.Fatalf("%s is not a virtual workspace: %+v", agentID, tree)
	}
	dir, err := core.Coord.WS.Materialize(agentID)
	if err != nil {
		t.Fatal(err)
	}
	fn(dir)
}

func put(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func absent(t *testing.T, root, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return os.IsNotExist(err)
}

// proposeWork records an agent's work the way a finished worker does: through Proposed, which pins the revision.
func proposeWork(t *testing.T, core *App, agentID string) []orchestrator.Change {
	t.Helper()
	out := recordProposals(t, core, agentID)
	for _, ch := range out {
		if ch.Rev == 0 {
			t.Fatalf("%s: a virtual proposal has no pinned revision", agentID)
		}
	}
	return out
}

// recordProposals is proposeWork for an agent in either kind of workspace; a worktree has no revision to pin.
func recordProposals(t *testing.T, core *App, agentID string) []orchestrator.Change {
	t.Helper()
	files, rev, err := core.Coord.WS.Proposed(agentID)
	if err != nil {
		t.Fatal(err)
	}
	tree, _ := core.Coord.WS.Tree(agentID)
	s := core.Coord.Current()
	var out []orchestrator.Change
	for _, f := range files {
		ch := orchestrator.Change{ID: changeIDOf(agentID, f.Path), AgentID: agentID, Path: f.Path, Kind: f.Kind, Added: f.Added, Deleted: f.Deleted,
			Status: "proposed", Worktree: tree.Path, Rev: rev, BaseRev: tree.Base}
		s.Changes = append(s.Changes, ch)
		out = append(out, ch)
	}
	core.Coord.Restore(s)
	return out
}

func changeIDOf(agentID, path string) string {
	return "chg_" + agentID + "_" + strings.NewReplacer("/", "_", ".", "_").Replace(path)
}

func statusOf(core *App, id string) string {
	ch, ok := core.Coord.FindChange(id)
	if !ok {
		return "missing"
	}
	return ch.Status
}

// Matrix 2: two agents edit the same file. Each sees only its own edit; accepting both merges them when they touch
// different lines, and refuses the second when they touch the same ones, with the first agent's bytes untouched.
func TestMatrixTwoAgentsEditingTheSameFile(t *testing.T) {
	core, dir := virtualCore(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\nfive\n", "s.txt": "x\ny\nz\n"})
	toolWrites(t, core, "agent_a", func(d string) { put(t, d, "a.txt", "ONE\ntwo\nthree\nfour\nfive\n") })
	toolWrites(t, core, "agent_b", func(d string) { put(t, d, "a.txt", "one\ntwo\nthree\nfour\nFIVE\n") })
	a := proposeWork(t, core, "agent_a")
	b := proposeWork(t, core, "agent_b")

	for id, want := range map[string]string{"agent_a": "ONE\ntwo\nthree\nfour\nfive\n", "agent_b": "one\ntwo\nthree\nfour\nFIVE\n"} {
		v, err := core.Coord.WS.View(id)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := v.ReadFile("a.txt"); err != nil || string(got) != want {
			t.Fatalf("%s sees %q (%v), want %q: an agent saw another agent's edit", id, got, err, want)
		}
	}
	if got := read(t, dir, "a.txt"); got != "one\ntwo\nthree\nfour\nfive\n" {
		t.Fatalf("proposing wrote into the user's folder: %q", got)
	}

	if _, err := core.acceptChange(a[0].ID); err != nil {
		t.Fatalf("accept A: %v", err)
	}
	if _, err := core.acceptChange(b[0].ID); err != nil {
		t.Fatalf("accept B: %v", err)
	}
	if got := read(t, dir, "a.txt"); got != "ONE\ntwo\nthree\nfour\nFIVE\n" {
		t.Fatalf("merged = %q", got)
	}

	// The same lines: the second is refused and the first agent's bytes stay.
	toolWrites(t, core, "agent_c", func(d string) { put(t, d, "s.txt", "x\nC\nz\n") })
	toolWrites(t, core, "agent_d", func(d string) { put(t, d, "s.txt", "x\nD\nz\n") })
	c := proposeWork(t, core, "agent_c")
	d := proposeWork(t, core, "agent_d")
	if _, err := core.acceptChange(c[0].ID); err != nil {
		t.Fatalf("accept C: %v", err)
	}
	if _, err := core.acceptChange(d[0].ID); err == nil {
		t.Fatal("two agents' edits to the same lines were both accepted")
	}
	if got := read(t, dir, "s.txt"); got != "x\nC\nz\n" {
		t.Fatalf("a refused accept changed the file: %q", got)
	}
	if statusOf(core, d[0].ID) != "proposed" {
		t.Fatalf("the refused change is %q", statusOf(core, d[0].ID))
	}
	// The refusal costs the agent nothing: its version is still there to be taken explicitly, and undone.
	if _, err := core.resolveChange(d[0].ID, "agent"); err != nil {
		t.Fatalf("take agent's version: %v", err)
	}
	if got := read(t, dir, "s.txt"); got != "x\nD\nz\n" {
		t.Fatalf("after taking D: %q", got)
	}
	if _, err := core.undoChange(d[0].ID); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := read(t, dir, "s.txt"); got != "x\nC\nz\n" {
		t.Fatalf("after undo: %q", got)
	}
}

// Matrix 3: add, delete and rename by tools. A rename is a delete plus an add, and each lands and undoes on its own.
func TestMatrixAddDeleteAndRenameLandAndUndo(t *testing.T) {
	core, dir := virtualCore(t, map[string]string{"keep.txt": "keep\n", "del.txt": "delete me\n", "old.txt": "rename me\n"})
	toolWrites(t, core, "agent_r", func(d string) {
		put(t, d, "new/dir/added.txt", "added\n")
		if err := os.Remove(filepath.Join(d, "del.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(d, "moved"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(d, "old.txt"), filepath.Join(d, "moved", "old.txt")); err != nil {
			t.Fatal(err)
		}
	})
	changes := proposeWork(t, core, "agent_r")
	var got []string
	for _, c := range changes {
		got = append(got, c.Kind+":"+c.Path)
	}
	sort.Strings(got)
	if want := "added:moved/old.txt,added:new/dir/added.txt,deleted:del.txt,deleted:old.txt"; strings.Join(got, ",") != want {
		t.Fatalf("proposed = %s, want %s", strings.Join(got, ","), want)
	}

	res, err := core.decideAll(true)
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["done"] != 4 || len(m["failed"].([]bulkFailure)) != 0 {
		t.Fatalf("bulk result = %+v", m)
	}
	if read(t, dir, "moved/old.txt") != "rename me\n" || read(t, dir, "new/dir/added.txt") != "added\n" || !absent(t, dir, "del.txt") || !absent(t, dir, "old.txt") {
		t.Fatalf("workspace after accept: %v", treeBytes(t, dir))
	}
	if read(t, dir, "keep.txt") != "keep\n" {
		t.Fatal("an untouched file changed")
	}

	for _, c := range changes {
		if _, err := core.undoChange(c.ID); err != nil {
			t.Fatalf("undo %s: %v", c.Path, err)
		}
	}
	if read(t, dir, "old.txt") != "rename me\n" || read(t, dir, "del.txt") != "delete me\n" || !absent(t, dir, "moved/old.txt") || !absent(t, dir, "new/dir/added.txt") {
		t.Fatalf("workspace after undo: %v", treeBytes(t, dir))
	}
}

// Matrix 4: the user edits while an agent works. A different region merges and keeps the user's lines; the same region,
// a deletion, and a file created on both sides are refused with the user's bytes intact.
func TestMatrixUserEditsWhileTheAgentWorks(t *testing.T) {
	core, dir := virtualCore(t, map[string]string{"m.txt": "1\n2\n3\n4\n5\n", "g.txt": "gone soon\n", "same.txt": "a\nb\nc\n"})
	toolWrites(t, core, "agent_u", func(d string) {
		put(t, d, "m.txt", "ONE\n2\n3\n4\n5\n")
		put(t, d, "g.txt", "agent edit\n")
		put(t, d, "same.txt", "a\nAGENT\nc\n")
		put(t, d, "both.txt", "agent made this\n")
	})
	work := map[string]orchestrator.Change{}
	for _, c := range proposeWork(t, core, "agent_u") {
		work[c.Path] = c
	}

	// The user keeps working in the real folder, uncommitted and untracked, after the agent was cut.
	put(t, dir, "m.txt", "1\n2\n3\n4\nFIVE\n")
	put(t, dir, "same.txt", "a\nUSER\nc\n")
	put(t, dir, "both.txt", "user made this\n")
	put(t, dir, "mine.txt", "the user's own file\n")
	if err := os.Remove(filepath.Join(dir, "g.txt")); err != nil {
		t.Fatal(err)
	}

	if _, err := core.acceptChange(work["m.txt"].ID); err != nil {
		t.Fatalf("a different region should merge: %v", err)
	}
	if got := read(t, dir, "m.txt"); got != "ONE\n2\n3\n4\nFIVE\n" {
		t.Fatalf("merged = %q", got)
	}
	for _, c := range []struct{ path, user string }{{"same.txt", "a\nUSER\nc\n"}, {"both.txt", "user made this\n"}} {
		if _, err := core.acceptChange(work[c.path].ID); err == nil {
			t.Fatalf("%s: the agent overwrote the user's edit", c.path)
		}
		if got := read(t, dir, c.path); got != c.user {
			t.Fatalf("%s: the user's bytes were changed to %q", c.path, got)
		}
	}
	if _, err := core.acceptChange(work["g.txt"].ID); err == nil || !absent(t, dir, "g.txt") {
		t.Fatalf("a file the user deleted came back (err=%v)", err)
	}
	if got := read(t, dir, "mine.txt"); got != "the user's own file\n" {
		t.Fatalf("an unrelated user file changed: %q", got)
	}
	for path, c := range work {
		if path != "m.txt" && statusOf(core, c.ID) != "proposed" {
			t.Fatalf("%s is %q after a refusal", path, statusOf(core, c.ID))
		}
	}
}

// Matrix 5: unsaved editor buffers. A buffer the user has not saved blocks the write before anything is read for
// it; the proposal is left exactly as it was and lands once the buffer is saved.
func TestMatrixUnsavedBufferBlocksAcceptAndKeepsTheProposal(t *testing.T) {
	core, dir := virtualCore(t, map[string]string{"a.txt": "one\n", "b.txt": "uno\n"})
	toolWrites(t, core, "agent_b", func(d string) { put(t, d, "a.txt", "agent a\n"); put(t, d, "b.txt", "agent b\n") })
	work := map[string]orchestrator.Change{}
	for _, c := range proposeWork(t, core, "agent_b") {
		work[c.Path] = c
	}

	core.setDirtyPaths([]string{"a.txt"})
	if _, err := core.acceptChange(work["a.txt"].ID); err == nil {
		t.Fatal("an unsaved buffer was overwritten")
	}
	if got := read(t, dir, "a.txt"); got != "one\n" {
		t.Fatalf("the file was written under an unsaved buffer: %q", got)
	}
	now, _ := core.Coord.FindChange(work["a.txt"].ID)
	if now.Status != "proposed" || now.Integration == nil || now.Integration.Kind != "unsaved_buffer" || now.Rev != work["a.txt"].Rev {
		t.Fatalf("blocked change = %+v", now)
	}
	if _, err := core.acceptChange(work["b.txt"].ID); err != nil {
		t.Fatalf("a clean path was held up by another path's buffer: %v", err)
	}
	if _, err := core.resolveChange(work["a.txt"].ID, "agent"); err == nil {
		t.Fatal("taking the agent's version bypassed the unsaved buffer")
	}

	core.setDirtyPaths(nil) // saved
	if _, err := core.acceptChange(work["a.txt"].ID); err != nil {
		t.Fatalf("accept after saving: %v", err)
	}
	if got := read(t, dir, "a.txt"); got != "agent a\n" {
		t.Fatalf("a.txt = %q", got)
	}
}

// Matrix 13: binary files. Overlapping binary edits cannot be merged and are refused whole; a binary file the agent
// adds lands byte for byte, including every byte value.
func TestMatrixBinaryFiles(t *testing.T) {
	core, dir := virtualCore(t, map[string]string{"bin.dat": "\x00old", "text.txt": "t\n"})
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	toolWrites(t, core, "agent_bin", func(d string) {
		put(t, d, "bin.dat", "\x00agent")
		if err := os.WriteFile(filepath.Join(d, "all.bin"), all, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	work := map[string]orchestrator.Change{}
	for _, c := range proposeWork(t, core, "agent_bin") {
		work[c.Path] = c
	}
	put(t, dir, "bin.dat", "\x00mine")

	if _, err := core.acceptChange(work["bin.dat"].ID); err == nil {
		t.Fatal("two edits of a binary file were merged")
	}
	if got := read(t, dir, "bin.dat"); got != "\x00mine" {
		t.Fatalf("the user's binary file was changed: %q", got)
	}
	if _, err := core.acceptChange(work["all.bin"].ID); err != nil {
		t.Fatalf("accept binary add: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "all.bin"))
	if err != nil || !bytes.Equal(got, all) {
		t.Fatalf("binary file was altered on the way: %v (%d bytes)", err, len(got))
	}
	if _, err := core.undoChange(work["all.bin"].ID); err != nil || !absent(t, dir, "all.bin") {
		t.Fatalf("undo of a binary add: %v", err)
	}
}

// Matrix 15: garbage collection with live overlays. Git's own gc is run with everything unreachable pruned at once;
// every object a proposal, an undo record, a retained rejected blob or an agent's working overlay needs is still
// there and still correct.
func TestMatrixGCKeepsEverythingLiveWorkNeeds(t *testing.T) {
	core, dir := virtualCore(t, map[string]string{"a.txt": "a0\n", "b.txt": "b0\n", "c.txt": "c0\n"})
	toolWrites(t, core, "agent_a", func(d string) { put(t, d, "a.txt", "a1\n") })
	toolWrites(t, core, "agent_b", func(d string) { put(t, d, "b.txt", "b1\n") })
	toolWrites(t, core, "agent_c", func(d string) { put(t, d, "c.txt", "c1\n") })
	a := proposeWork(t, core, "agent_a")[0]
	b := proposeWork(t, core, "agent_b")[0]
	c := proposeWork(t, core, "agent_c")[0]

	if _, err := core.acceptChange(a.ID); err != nil { // an undo record
		t.Fatal(err)
	}
	if _, err := core.rejectChange(c.ID); err != nil { // a retained blob
		t.Fatal(err)
	}
	// An agent that is mid-work: edits in its overlay that no proposal and no branch refers to.
	if _, err := core.Coord.WS.EnsureTree("agent_live", false); err != nil {
		t.Fatal(err)
	}
	live, err := core.Coord.WS.View("agent_live")
	if err != nil {
		t.Fatal(err)
	}
	if err := live.WriteFile("live/work.txt", []byte("not yet proposed\n"), 0); err != nil {
		t.Fatal(err)
	}
	before, _ := live.Identity()

	git(t, dir, "reflog", "expire", "--expire=now", "--all")
	git(t, dir, "gc", "--prune=now", "--quiet")
	if out := gitText(t, dir, "fsck", "--full", "--no-dangling"); strings.Contains(out, "missing") {
		t.Fatalf("fsck after gc:\n%s", out)
	}

	if after, err := live.Identity(); err != nil || after.Tree != before.Tree {
		t.Fatalf("a live overlay changed under gc: %+v vs %+v (%v)", after, before, err)
	}
	if got, err := live.ReadFile("live/work.txt"); err != nil || string(got) != "not yet proposed\n" {
		t.Fatalf("live overlay lost its file: %q %v", got, err)
	}
	if _, err := core.acceptChange(b.ID); err != nil { // the pinned proposal
		t.Fatalf("accept after gc: %v", err)
	}
	if got := read(t, dir, "b.txt"); got != "b1\n" {
		t.Fatalf("pinned proposal read %q after gc", got)
	}
	if _, err := core.undoChange(a.ID); err != nil { // the journal pre-image
		t.Fatalf("undo after gc: %v", err)
	}
	if got := read(t, dir, "a.txt"); got != "a0\n" {
		t.Fatalf("undo restored %q", got)
	}
	var ref string
	for _, r := range strings.Fields(gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes")) {
		if strings.HasSuffix(r, "-"+c.ID) {
			ref = r
		}
	}
	if ref == "" || gitText(t, dir, "cat-file", "-p", ref) != "c1" {
		t.Fatalf("the rejected blob did not survive gc (ref %q)", ref)
	}
}

// reopen stops the core the way a crash does for everything on disk, and starts another over the same repository and
// database with the stored session restored.
func reopen(t *testing.T, core *App, dir string) *App {
	t.Helper()
	db := core.Cfg.DBPath
	core.Close()
	next, _ := openVirtualCore(t, dir, db)
	t.Cleanup(next.Close)
	raw, err := next.Store.LatestSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	var s orchestrator.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	next.Coord.Restore(&s)
	next.restoreSession(s.ID)
	return next
}

// Matrix 7 and 14: a stop between the proposal and the decision. The proposal is read from the revision it was
// pinned at, not from whatever the directory holds after the restart, and deciding it after the restart is safe.
func TestMatrixRestartKeepsAProposalPinned(t *testing.T) {
	core, dir := virtualCore(t, map[string]string{"a.txt": "one\n"})
	toolWrites(t, core, "agent_p", func(d string) { put(t, d, "a.txt", "proposed\n") })
	ch := proposeWork(t, core, "agent_p")[0]
	// The agent kept working after it proposed; the tool's file is in the directory, imported by nobody.
	toolWrites(t, core, "agent_p", func(d string) { put(t, d, "a.txt", "LATER, never proposed\n"); put(t, d, "later.txt", "x\n") })

	core2 := reopen(t, core, dir)
	got, ok := core2.Coord.FindChange(ch.ID)
	if !ok || got.Status != "proposed" || got.Rev != ch.Rev {
		t.Fatalf("restored change = %+v %v", got, ok)
	}
	if _, err := core2.acceptChange(ch.ID); err != nil {
		t.Fatalf("accept after restart: %v", err)
	}
	if body := read(t, dir, "a.txt"); body != "proposed\n" {
		t.Fatalf("the accepted bytes are %q, not the pinned proposal", body)
	}
	if !absent(t, dir, "later.txt") {
		t.Fatal("unproposed work reached the user's folder")
	}
	if _, err := core2.undoChange(ch.ID); err != nil {
		t.Fatalf("undo after restart: %v", err)
	}
	if body := read(t, dir, "a.txt"); body != "one\n" {
		t.Fatalf("undo restored %q", body)
	}
}

// Matrix 7: a stop in the middle of an accept. Whichever side of the file write it fell on, the retry lands the
// proposal once and the undo returns the user's original bytes, never the agent's.
func TestMatrixCrashDuringAcceptIsRetriedSafely(t *testing.T) {
	for _, tc := range []struct {
		name         string
		writtenFirst bool // the stop was after the file was written but before the decision was stored
	}{{"before the write", false}, {"after the write", true}} {
		t.Run(tc.name, func(t *testing.T) {
			core, dir := virtualCore(t, map[string]string{"a.txt": "original\n"})
			toolWrites(t, core, "agent_x", func(d string) { put(t, d, "a.txt", "agent\n") })
			ch := proposeWork(t, core, "agent_x")[0]

			// What accept does first: journal the user's pre-image, then write.
			if _, err := core.Coord.WS.JournalChange(ch.ID, []workspace.JournalWrite{{Path: "a.txt", Post: []byte("agent\n"), PostExists: true}}); err != nil {
				t.Fatal(err)
			}
			if tc.writtenFirst {
				put(t, dir, "a.txt", "agent\n")
			}

			core2 := reopen(t, core, dir)
			if statusOf(core2, ch.ID) != "proposed" {
				t.Fatalf("the half-made decision was stored as %q", statusOf(core2, ch.ID))
			}
			if _, err := core2.acceptChange(ch.ID); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if body := read(t, dir, "a.txt"); body != "agent\n" {
				t.Fatalf("after retry: %q", body)
			}
			if statusOf(core2, ch.ID) != "accepted" {
				t.Fatalf("status = %q", statusOf(core2, ch.ID))
			}
			if _, err := core2.undoChange(ch.ID); err != nil {
				t.Fatalf("undo: %v", err)
			}
			if body := read(t, dir, "a.txt"); body != "original\n" {
				t.Fatalf("undo restored %q, not the user's original", body)
			}
		})
	}
}
