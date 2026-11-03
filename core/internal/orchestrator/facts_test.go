package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
	"github.com/relocode/relo/internal/workspace"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// The Primary used to learn about the repository only from worker reports, which disagreed, so it kept planning
// probes to find out which was right. The facts come from git and the check runner and cannot disagree.
func TestWorkspaceFactsStateWhatReloMeasured(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(logging.New(nil, logging.Error))
	ws := workspace.New(dir)
	c.SetWorkspaceManager(ws)
	c.CreateSession(dir, "rename", ModeBuild)
	c.refreshSnapshot()
	task := c.Graph.Add(taskgraph.Task{Title: "Apply names", Role: "worker"})
	c.mu.Lock()
	c.sess.Changes = []Change{
		{ID: "c1", TaskID: task.ID, Path: "web/main.js", Status: "proposed", Added: 3, Deleted: 1},
		{ID: "c2", TaskID: task.ID, Path: "docs/x.md", Status: "accepted", Added: 2},
		{ID: "c3", TaskID: task.ID, Path: "gone.txt", Status: "rejected", Added: 9},
	}
	c.sess.Verify = []verification.Result{
		{Command: "go test ./...", Status: "failed"},
		{Command: "go test ./...", Status: "passed"},
		{Command: "npm test", Status: "skipped", Note: "no script"},
	}
	c.mu.Unlock()

	facts := c.workspaceFacts()
	for _, want := range []string{
		"branch ", "2 uncommitted path(s)", "a.txt", "new.txt",
		`Waiting in the Changes tab, not yet in the working tree: "Apply names": 1 file (+3 -1): web/main.js`,
		`Accepted into the working tree: "Apply names": 1 file (+2 -0): docs/x.md`,
		"Check go test ./...: passed", "Check npm test: skipped (no script)",
		"cannot see them yet",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if strings.Contains(facts, "gone.txt") || strings.Contains(facts, "Check go test ./...: failed") {
		t.Errorf("facts include a rejected change or a superseded check result:\n%s", facts)
	}
}

func TestWorkspaceFactsWithNothingYet(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.CreateSession(dir, "x", ModeBuild)
	c.refreshSnapshot()
	facts := c.workspaceFacts()
	for _, want := range []string{"working tree clean", "No worker has changed any file yet", "none have run"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
}

// A run past the old round caps (ten drive rounds, three follow-up plans) keeps going until Primary stops planning.
func TestPrimaryRunPastOldCapContinues(t *testing.T) {
	plan := func(n int) string {
		return fmt.Sprintf(`{"status":"working","userMessage":"step %d","tasks":[{"id":"p%d","title":"Probe %d","role":"probe","description":"look"}]}`, n, n, n)
	}
	const report = `{"status":"completed","summary":"looked"}`
	const rounds = 12
	var script []string
	for i := 0; i < rounds; i++ {
		script = append(script, plan(i), report)
	}
	script = append(script, `{"status":"completed","userMessage":"all done","tasks":[]}`)
	r := newDispatchRuntime(script...)
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.CreateSession(t.TempDir(), "q", ModeBuild)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Submit(ctx, "do it", nil); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	sent := len(r.tasks)
	r.mu.Unlock()
	if sent != len(script) {
		t.Fatalf("the model was called %d times, want %d", sent, len(script))
	}
	probes := 0
	for _, task := range c.Graph.List() {
		if task.Role == "probe" {
			probes++
		}
	}
	if probes != rounds {
		t.Fatalf("%d probes were planned, want %d: no round may be dropped", probes, rounds)
	}
	for _, m := range c.Current().Messages {
		if m.Role == "system" && strings.Contains(m.Text, "stopped") {
			t.Fatalf("run stopped early with notice %q", m.Text)
		}
	}
	if c.Busy() {
		t.Fatal("agents still active after the request ended")
	}
}

// A "rebuild and install" task sent a Sonnet worker through ten model calls to find the repository's Makefile.
func TestWorkspaceFactsNameTheProjectsOwnCommands(t *testing.T) {
	dir := gitRepo(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", ".PHONY: build dev test\nVERSION := 1\n\nbuild: core web\n\tgo build ./...\ndev install: build\n\t./install.sh\nlint:\n\tgolangci-lint run\n%.o: %.c\n\tcc $<\n")
	write("package.json", `{"scripts":{"test":"jest","build":"tsc"}}`)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.CreateSession(dir, "x", ModeBuild)
	c.refreshSnapshot()
	facts := c.workspaceFacts()
	for _, want := range []string{"Project commands: make build, make dev, make test, make install, make lint, npm run build, npm run test."} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if strings.Contains(facts, "make VERSION") || strings.Contains(facts, "make %") {
		t.Errorf("facts list a variable or a pattern rule as a command:\n%s", facts)
	}
}

// A worker says a file is missing, the head believes it is there: Relo looks and settles it, and for a file that is
// gone names the snapshot copy to restore from. The head has no tools, so without this it can only guess.
func TestFactsSettleWhetherANamedFileExists(t *testing.T) {
	dir := gitRepo(t)
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("engine/src/ai/facts.c", "int facts;\n")
	write("engine/src/ai/here.c", "int here;\n")

	c := New(logging.New(nil, logging.Error))
	ws := workspace.New(dir)
	c.SetWorkspaceManager(ws)
	c.CreateSession(dir, "fix it", ModeBuild)
	snap, err := ws.Snapshot() // an agent is cut from the tree while facts.c still exists
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "update-ref", "refs/relo/vws/agent_x/base", snap.Commit)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	if err := os.Remove(filepath.Join(dir, "engine/src/ai/facts.c")); err != nil {
		t.Fatal(err)
	}

	c.Graph.Add(taskgraph.Task{Title: "Build it", Role: "worker", Status: taskgraph.Failed,
		Description: "engine/src/ai/facts.c is missing, so engine/src/ai/here.c cannot link; see ../outside/x.c and /etc/passwd.x"})
	facts := c.workspaceFacts()
	for _, want := range []string{
		"`engine/src/ai/facts.c` is NOT in the working tree",
		"git show " + snap.Commit[:12] + ":engine/src/ai/facts.c > engine/src/ai/facts.c",
		"`engine/src/ai/here.c` is in the working tree (10 bytes)",
		"it outranks what an agent or you believed",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if strings.Contains(facts, "outside/x.c") || strings.Contains(facts, "passwd") {
		t.Errorf("facts looked outside the workspace:\n%s", facts)
	}

	// Nothing blocked or failed names a file: the facts stay as short as before.
	quiet := New(logging.New(nil, logging.Error))
	quiet.SetWorkspaceManager(workspace.New(dir))
	quiet.CreateSession(dir, "q", ModeBuild)
	if strings.Contains(quiet.workspaceFacts(), "Relo looked at the files") {
		t.Error("facts listed files with no blocked or failed task naming one")
	}
}
