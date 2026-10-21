package app

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/workspace"
)

// TestVWEBenchmarkReport compares worktree and virtual workspaces on the workloads the design asks about and prints
// a Markdown report. It is skipped unless RELO_VWE_BENCH is set, because it builds large repositories:
//
//	env TMPDIR=/some/real/disk RELO_VWE_BENCH=1 go test -run TestVWEBenchmarkReport -count=1 -timeout 60m ./internal/app
//
// RELO_VWE_BENCH_FILES sets the size of the large repository (default 20000), RELO_VWE_BENCH_AGENTS the agent counts
// (default 1,4,16), RELO_VWE_BENCH_REPS the repetitions behind each median (default 3) and RELO_VWE_BENCH_OUT a file
// to write the report to and RELO_VWE_BENCH_ONLY a comma list of sections to run (init, work, generated,
// integration, reads; default all). TMPDIR matters: a tmpfs hides what a disk-backed checkout costs.
//
// What it does and does not measure is stated in the report. Filesystem operations are counted as entries created,
// not as system calls, and a page cache that is warm after the first repetition favors both modes alike.

type benchMode struct {
	label   string
	setting string // relo.workspace.mode
	viaView bool   // edits go through the WorkspaceView and no directory is laid out
}

var benchModes = []benchMode{
	{label: "worktree", setting: "worktree"},
	{label: "virtual, tools in a materialized dir", setting: "virtual"},
	{label: "virtual, edits through the view", setting: "virtual", viaView: true},
}

type benchRepo struct {
	name  string
	dir   string
	files []string
	bytes int64
}

func benchEnv(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func benchList(name string, def []int) []int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(raw, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// makeBenchRepo writes a repository of n source files of a few KB each, spread over directories, with one larger
// shared file the integration workloads edit, and commits it.
func makeBenchRepo(t *testing.T, name string, n int) benchRepo {
	t.Helper()
	dir, err := os.MkdirTemp("", "vwebench-src-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "bench@relo")
	git(t, dir, "config", "user.name", "Bench")
	repo := benchRepo{name: name, dir: dir}
	var body strings.Builder
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("src/d%03d/f%05d.go", i%400, i)
		body.Reset()
		fmt.Fprintf(&body, "// Package f%d is generated for the benchmark.\npackage f%d\n\n", i, i)
		for l := 0; l < 40+i%40; l++ {
			fmt.Fprintf(&body, "func F%d_%d(x int) int { return x*%d + %d } // line %d of file %d\n", i, l, l+1, i%97, l, i)
		}
		put(t, dir, rel, body.String())
		repo.files = append(repo.files, rel)
		repo.bytes += int64(body.Len())
	}
	var shared strings.Builder
	for l := 0; l < 2000; l++ {
		fmt.Fprintf(&shared, "shared line %04d\n", l)
	}
	put(t, dir, "shared/big.txt", shared.String())
	put(t, dir, ".gitignore", "node_modules/\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "bench")
	return repo
}

// benchCore opens a core over a copy of the repository with the given workspace mode.
func benchCore(t *testing.T, repo benchRepo, setting string) (*App, string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "vwebench-run-")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", "--reflink=auto", repo.dir+"/.", dir).CombinedOutput(); err != nil {
		t.Skipf("cannot copy the benchmark repository: %v\n%s", err, out)
	}
	db, err := os.MkdirTemp("", "vwebench-db-")
	if err != nil {
		t.Fatal(err)
	}
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(db, "relo.db"), Runtime: "mock"}, logging.New(io.Discard, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	core.applySettings(map[string]any{"workspaceMode": setting})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	return core, dir, func() { core.Close(); os.RemoveAll(dir); os.RemoveAll(db) }
}

type footprint struct {
	checkoutBytes, checkoutEntries int64 // everything under .relo/worktrees
	gitBytes, gitEntries           int64 // everything under .git
}

func (f footprint) minus(o footprint) footprint {
	return footprint{f.checkoutBytes - o.checkoutBytes, f.checkoutEntries - o.checkoutEntries, f.gitBytes - o.gitBytes, f.gitEntries - o.gitEntries}
}

func walkSize(root string) (bytes, entries int64) {
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		entries++
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				bytes += info.Size()
			}
		}
		return nil
	})
	return
}

func measure(dir string) footprint {
	var f footprint
	f.checkoutBytes, f.checkoutEntries = walkSize(filepath.Join(dir, ".relo", "worktrees"))
	f.gitBytes, f.gitEntries = walkSize(filepath.Join(dir, ".git"))
	return f
}

// counted counts git commands run by the workspace manager and by the virtual engine while fn runs.
func counted(core *App, fn func()) (spawns int64) {
	var probe atomic.Int64
	workspace.GitProbe = func([]string) { probe.Add(1) }
	before, _ := core.Coord.WS.VirtualMetrics()
	fn()
	workspace.GitProbe = nil
	after, _ := core.Coord.WS.VirtualMetrics()
	return probe.Load() + after.GitSpawns - before.GitSpawns
}

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.2f", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.0f", float64(d)/float64(time.Millisecond))
}

func mb(n int64) string {
	if n < 0 {
		return "-" + mb(-n)
	}
	if n < 1<<20 {
		return fmt.Sprintf("%.2f", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f", float64(n)/(1<<20))
}

type report struct {
	sb strings.Builder
}

func (r *report) f(format string, args ...any) { fmt.Fprintf(&r.sb, format, args...) }

func (r *report) table(header []string, rows [][]string) {
	r.f("| %s |\n|%s\n", strings.Join(header, " | "), strings.Repeat("---|", len(header)))
	for _, row := range rows {
		r.f("| %s |\n", strings.Join(row, " | "))
	}
	r.f("\n")
}

func agentName(i int) string { return fmt.Sprintf("agent_b%02d", i) }

// workDir is where a tool would write for this agent: the worktree, or a directory laid out for the run.
func workDir(t *testing.T, core *App, id string) string {
	t.Helper()
	tree, ok := core.Coord.WS.Tree(id)
	if !ok {
		t.Fatalf("no workspace for %s", id)
	}
	if !tree.Virtual {
		return tree.Path
	}
	dir, err := core.Coord.WS.Materialize(id)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

type edit struct {
	rel  string
	body string
}

func applyEdits(t *testing.T, core *App, m benchMode, id string, edits []edit) {
	t.Helper()
	if m.viaView {
		v, err := core.Coord.WS.View(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range edits {
			if err := v.WriteFile(e.rel, []byte(e.body), 0); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := workDir(t, core, id)
	for _, e := range edits {
		put(t, dir, e.rel, e.body)
	}
}

func editsFor(repo benchRepo, i, k int) []edit {
	var out []edit
	for j := 0; j < k; j++ {
		rel := repo.files[(i*7919+j*104729)%len(repo.files)]
		out = append(out, edit{rel, fmt.Sprintf("// edited by agent %d, change %d\npackage edited\n", i, j)})
	}
	out = append(out, edit{fmt.Sprintf("added/agent%d.txt", i), "new file\n"})
	return out
}

type benchRun struct {
	wall    time.Duration
	extra   map[string]string
	foot    footprint
	spawns  int64
	objects int64
}

func repeat(reps int, fn func() benchRun) []benchRun {
	runs := make([]benchRun, 0, reps)
	for i := 0; i < reps; i++ {
		runs = append(runs, fn())
	}
	return runs
}

func medianRun(runs []benchRun) benchRun {
	idx := make([]int, len(runs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return runs[idx[a]].wall < runs[idx[b]].wall })
	return runs[idx[len(idx)/2]]
}

func TestVWEBenchmarkReport(t *testing.T) {
	if os.Getenv("RELO_VWE_BENCH") == "" {
		t.Skip("set RELO_VWE_BENCH=1 to run the workspace benchmarks")
	}
	reps := benchEnv("RELO_VWE_BENCH_REPS", 3)
	agents := benchList("RELO_VWE_BENCH_AGENTS", []int{1, 4, 16})
	large := benchEnv("RELO_VWE_BENCH_FILES", 20000)
	repos := []benchRepo{makeBenchRepo(t, "small", 60), makeBenchRepo(t, "large", large)}

	var r report
	r.f("# Virtual workspace benchmarks\n\n")
	r.f("go %s on %s/%s, %d CPUs, TMPDIR=%s, %d repetitions per cell (median shown).\n\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), os.TempDir(), reps)
	for _, repo := range repos {
		r.f("- %s repository: %d files, %s MB of source plus a 2000-line shared file.\n", repo.name, len(repo.files), mb(repo.bytes))
	}
	r.f("\n")

	only := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("RELO_VWE_BENCH_ONLY"), ",") {
		only[strings.TrimSpace(name)] = true
	}
	want := func(name string) bool { return len(only) == 1 && only[""] || only[name] }
	for _, repo := range repos {
		r.f("## %s repository (%d files)\n\n", repo.name, len(repo.files))
		if want("init") {
			benchInit(t, &r, repo, agents, reps)
		}
		if want("work") {
			benchWork(t, &r, repo, agents, reps)
		}
		if want("generated") {
			benchGenerated(t, &r, repo, reps)
		}
		if want("integration") {
			benchIntegration(t, &r, repo, agents, reps)
		}
		if want("reads") {
			benchReads(t, &r, repo, reps)
		}
	}
	out := r.sb.String()
	t.Log("\n" + out)
	if path := os.Getenv("RELO_VWE_BENCH_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Creating N agents' workspaces: the latency to get an agent a place to work, and what the idle workspaces cost.
func benchInit(t *testing.T, r *report, repo benchRepo, agents []int, reps int) {
	r.f("### Creating workspaces\n\nTime to give N agents an isolated workspace, one after another and all at once. The footprint is what N idle workspaces cost on disk (checkout files plus the growth of `.git`).\n\n")
	header := []string{"agents", "mode", "sequential ms", "per agent ms", "parallel ms", "checkout MB", "checkout entries", ".git growth MB", ".git growth entries", "git commands"}
	var rows [][]string
	for _, n := range agents {
		for _, m := range benchModes[:2] { // the view variant creates workspaces the same way
			seq := repeat(reps, func() benchRun {
				core, dir, done := benchCore(t, repo, m.setting)
				defer done()
				base := measure(dir)
				var run benchRun
				run.spawns = counted(core, func() {
					start := time.Now()
					for i := 0; i < n; i++ {
						if _, err := core.Coord.WS.EnsureTree(agentName(i), false); err != nil {
							t.Fatal(err)
						}
					}
					run.wall = time.Since(start)
				})
				run.foot = measure(dir).minus(base)
				return run
			})
			par := repeat(reps, func() benchRun {
				core, _, done := benchCore(t, repo, m.setting)
				defer done()
				var run benchRun
				var wg sync.WaitGroup
				start := time.Now()
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						if _, err := core.Coord.WS.EnsureTree(agentName(i), false); err != nil {
							t.Error(err)
						}
					}(i)
				}
				wg.Wait()
				run.wall = time.Since(start)
				return run
			})
			s := medianRun(seq)
			rows = append(rows, []string{strconv.Itoa(n), m.label, ms(s.wall), ms(s.wall / time.Duration(n)), ms(medianRun(par).wall),
				mb(s.foot.checkoutBytes), strconv.FormatInt(s.foot.checkoutEntries, 10), mb(s.foot.gitBytes), strconv.FormatInt(s.foot.gitEntries, 10), strconv.FormatInt(s.spawns, 10)})
		}
	}
	r.table(header, rows)
}

// N agents each make a few edits and propose them. Peak is measured with every agent mid-work; steady with every
// proposal waiting for a decision.
func benchWork(t *testing.T, r *report, repo benchRepo, agents []int, reps int) {
	r.f("### Agents editing\n\nEach agent edits 5 files and adds one, then proposes. *Work* is creating the workspace and making the edits; *propose* is turning them into changes (for the materialized variant that includes importing and removing the directory). *Release* removes every agent's workspace once its changes are decided (the cost of deleting a checkout, which a worktree pays here and a virtual agent paid at settle). Peak is with all agents mid-work, steady is with every proposal waiting for a decision.\n\n")
	header := []string{"agents", "mode", "work ms", "propose ms", "release ms", "total ms", "peak checkout MB", "steady checkout MB", "steady .git growth MB", "objects written", "git commands"}
	var rows [][]string
	for _, n := range agents {
		for _, m := range benchModes {
			runs := repeat(reps, func() benchRun {
				core, dir, done := benchCore(t, repo, m.setting)
				defer done()
				base := measure(dir)
				var run benchRun
				var peak, steady footprint
				before, _ := core.Coord.WS.VirtualMetrics()
				run.spawns = counted(core, func() {
					start := time.Now()
					for i := 0; i < n; i++ {
						if _, err := core.Coord.WS.EnsureTree(agentName(i), false); err != nil {
							t.Fatal(err)
						}
						applyEdits(t, core, m, agentName(i), editsFor(repo, i, 5))
					}
					run.wall = time.Since(start)
					peak = measure(dir).minus(base)
					start = time.Now()
					for i := 0; i < n; i++ {
						if !m.viaView && isVirtual(core, agentName(i)) {
							if err := core.Coord.WS.Settle(agentName(i)); err != nil {
								t.Fatal(err)
							}
						}
						if files, _, err := core.Coord.WS.Proposed(agentName(i)); err != nil || len(files) != 6 {
							t.Fatalf("%s proposed %d files (%v)", agentName(i), len(files), err)
						}
					}
					proposeT := time.Since(start)
					run.extra = map[string]string{"propose": ms(proposeT)}
					steady = measure(dir).minus(base)
					start = time.Now()
					for i := 0; i < n; i++ {
						if err := core.Coord.WS.Release(agentName(i)); err != nil {
							t.Fatal(err)
						}
					}
					run.extra["release"] = ms(time.Since(start))
					run.extra["total"] = ms(run.wall + proposeT + time.Since(start))
				})
				after, _ := core.Coord.WS.VirtualMetrics()
				run.objects = after.ObjectsWritten - before.ObjectsWritten
				run.foot = steady
				run.extra["peak"] = mb(peak.checkoutBytes)
				return run
			})
			s := medianRun(runs)
			rows = append(rows, []string{strconv.Itoa(n), m.label, ms(s.wall), s.extra["propose"], s.extra["release"], s.extra["total"], s.extra["peak"], mb(s.foot.checkoutBytes), mb(s.foot.gitBytes),
				strconv.FormatInt(s.objects, 10), strconv.FormatInt(s.spawns, 10)})
		}
	}
	r.table(header, rows)
}

// A tool writes a large, unignored output tree. The worktree needs no import; the virtual engine has to read it into
// objects. This is the case where worktrees are expected to win.
func benchGenerated(t *testing.T, r *report, repo benchRepo, reps int) {
	const files, size = 3000, 8 << 10
	r.f("### Large generated output\n\nOne agent's tools write %d unignored files of %d KB (%s MB) and the agent proposes. *Tools write* is the same disk work in both modes and is shown only to size the job; *propose* is Relo's cost to turn it into changes.\n\n", files, size>>10, mb(files*size))
	header := []string{"mode", "tools write ms", "propose ms", "checkout MB (peak)", "checkout MB (after)", ".git growth MB", "objects written", "git commands"}
	var rows [][]string
	payload := strings.Repeat("generated output line\n", size/22)
	for _, m := range benchModes[:2] {
		runs := repeat(reps, func() benchRun {
			core, dir, done := benchCore(t, repo, m.setting)
			defer done()
			base := measure(dir)
			if _, err := core.Coord.WS.EnsureTree("agent_gen", false); err != nil {
				t.Fatal(err)
			}
			var run benchRun
			run.extra = map[string]string{}
			wd := workDir(t, core, "agent_gen")
			start := time.Now()
			for i := 0; i < files; i++ {
				put(t, wd, fmt.Sprintf("out/g%03d/o%05d.bin", i%60, i), fmt.Sprintf("%d\n%s", i, payload))
			}
			run.extra["write"] = ms(time.Since(start))
			peak := measure(dir).minus(base)
			before, _ := core.Coord.WS.VirtualMetrics()
			run.spawns = counted(core, func() {
				start = time.Now()
				if isVirtual(core, "agent_gen") {
					if err := core.Coord.WS.Settle("agent_gen"); err != nil {
						t.Fatal(err)
					}
				}
				if got, _, err := core.Coord.WS.Proposed("agent_gen"); err != nil || len(got) != files {
					t.Fatalf("proposed %d files (%v)", len(got), err)
				}
				run.wall = time.Since(start)
			})
			after, _ := core.Coord.WS.VirtualMetrics()
			run.objects = after.ObjectsWritten - before.ObjectsWritten
			run.foot = measure(dir).minus(base)
			run.extra["peak"] = mb(peak.checkoutBytes)
			return run
		})
		s := medianRun(runs)
		rows = append(rows, []string{m.label, s.extra["write"], ms(s.wall), s.extra["peak"], mb(s.foot.checkoutBytes), mb(s.foot.gitBytes), strconv.FormatInt(s.objects, 10), strconv.FormatInt(s.spawns, 10)})
	}
	r.table(header, rows)
}

// Deciding the proposals: N agents edit one shared file, either on different lines (all merge) or the same line (one
// wins, the rest are refused). The clock starts when the agents have finished their edits and covers everything up
// to the last decision, so the cost of removing a checkout is counted in both modes: a worktree pays it when its last
// change is decided, a virtual agent when its directory is settled.
func benchIntegration(t *testing.T, r *report, repo benchRepo, agents []int, reps int) {
	r.f("### Integration\n\nN agents edit `shared/big.txt` (2000 lines). *Disjoint*: each edits its own line and every proposal is accepted. *Same line*: all edit one line, so one is accepted and the rest are refused with nothing written. The clock starts when the agents have finished editing: *propose* is making the changes (for the virtual mode that includes importing each agent's directory and removing it), *accept* is deciding them all. The cost of removing a worktree's checkout falls in *accept*, because that is when a worktree is released.\n\n")
	header := []string{"agents", "mode", "propose ms", "accept all ms", "total ms", "merged correctly", "same line: propose + accept ms", "accepted", "refused"}
	var rows [][]string
	for _, n := range agents {
		if n < 2 {
			continue
		}
		for _, m := range benchModes[:2] {
			var proposeT, acceptT, totalT, sameT []time.Duration
			var correct string
			var accepted, refused int
			for rep := 0; rep < reps; rep++ {
				for _, overlap := range []bool{false, true} {
					core, dir, done := benchCore(t, repo, m.setting)
					lines := strings.Split(read(t, dir, "shared/big.txt"), "\n")
					for i := 0; i < n; i++ {
						id := agentName(i)
						if _, err := core.Coord.WS.EnsureTree(id, false); err != nil {
							t.Fatal(err)
						}
						mine := append([]string(nil), lines...)
						at := 100 + i*40
						if overlap {
							at = 500
						}
						mine[at] = fmt.Sprintf("edited by %s", id)
						put(t, workDir(t, core, id), "shared/big.txt", strings.Join(mine, "\n"))
					}
					start := time.Now()
					var ids []string
					for i := 0; i < n; i++ {
						id := agentName(i)
						if isVirtual(core, id) {
							if err := core.Coord.WS.Settle(id); err != nil {
								t.Fatal(err)
							}
						}
						for _, ch := range recordProposals(t, core, id) {
							ids = append(ids, ch.ID)
						}
					}
					proposed := time.Since(start)
					okCount, failCount := 0, 0
					for _, id := range ids {
						if _, err := core.acceptChange(id); err != nil {
							failCount++
						} else {
							okCount++
						}
					}
					total := time.Since(start)
					if overlap {
						sameT = append(sameT, total)
						accepted, refused = okCount, failCount
					} else {
						proposeT, acceptT, totalT = append(proposeT, proposed), append(acceptT, total-proposed), append(totalT, total)
						got := read(t, dir, "shared/big.txt")
						correct = "yes"
						for i := 0; i < n; i++ {
							if !strings.Contains(got, fmt.Sprintf("edited by %s\n", agentName(i))) {
								correct = "NO"
							}
						}
						if okCount != n {
							correct = fmt.Sprintf("NO (%d of %d accepted)", okCount, n)
						}
					}
					done()
				}
			}
			rows = append(rows, []string{strconv.Itoa(n), m.label, ms(median(proposeT)), ms(median(acceptT)), ms(median(totalT)), correct, ms(median(sameT)), strconv.Itoa(accepted), strconv.Itoa(refused)})
		}
	}
	r.table(header, rows)
}

// Reading files through the agent's view repeatedly, as a context refresh does.
func benchReads(t *testing.T, r *report, repo benchRepo, reps int) {
	const per, rounds = 25, 100
	r.f("### Repeated context refreshes\n\nOne agent reads %d files through its WorkspaceView, %d times over (what revalidating its context's file excerpts costs). Per-read time includes Relo's view layer, not just the filesystem.\n\n", per, rounds)
	header := []string{"state", "us per read"}
	type variant struct {
		label    string
		setting  string
		openTool bool
	}
	var rows [][]string
	for _, v := range []variant{{"worktree", "worktree", false}, {"virtual, directory not laid out", "virtual", false}, {"virtual, tool directory open", "virtual", true}} {
		var times []time.Duration
		for rep := 0; rep < reps; rep++ {
			core, _, done := benchCore(t, repo, v.setting)
			if _, err := core.Coord.WS.EnsureTree("agent_read", false); err != nil {
				t.Fatal(err)
			}
			if v.openTool {
				workDir(t, core, "agent_read")
			}
			view, err := core.Coord.WS.View("agent_read")
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			for round := 0; round < rounds; round++ {
				for i := 0; i < per; i++ {
					if _, err := view.ReadFile(repo.files[(i*131)%len(repo.files)]); err != nil {
						t.Fatal(err)
					}
				}
			}
			times = append(times, time.Since(start))
			done()
		}
		rows = append(rows, []string{v.label, fmt.Sprintf("%.1f", float64(median(times).Microseconds())/float64(per*rounds))})
	}
	r.table(header, rows)
}
