package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/taskgraph"
)

const recallQuery = "Propose renames for the efficiency tab labels"

// recallSetup is a git repo with a session on it, a Primary and an observing worker, with memory on.
func recallSetup(t *testing.T) (*Coord, *Session, agent.Agent, agent.Agent, testRepo) {
	t.Helper()
	r := initRepo(t)
	c := New(logging.New(nil, logging.Error))
	openTestMemory(t, c, r.dir)
	c.SetRuntime(newDispatchRuntime())
	s := c.CreateSession(r.dir, recallQuery, ModeBuild)
	c.Settings.Memory = true
	p := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	observer := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: r.dir})
	return c, s, p, observer, r
}

// promoteFileFact proposes a candidate citing a.txt as observed now, then has the Primary promote it through the head
// gate. The promotion stamps the repo's HEAD onto the evidence, so the entry is current until a.txt changes.
func promoteFileFact(t *testing.T, c *Coord, s *Session, head, observer agent.Agent, r testRepo, ref, title, body string) memory.Entry {
	t.Helper()
	sum, err := hashFile(filepath.Join(r.dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ev := memory.Evidence{Type: memory.EvidenceFileObservation, SessionID: s.ID, AgentID: observer.ID, Ref: ref,
		ObservedAt: time.Now().UTC(), Result: memory.ResultObserved, Files: []memory.EvidenceFile{{Path: "a.txt", SHA256: sum}}}
	e, err := c.Memory.ProposeCandidate(memory.Entry{Kind: "fact", Title: title, Body: body, Files: []string{"a.txt"}, Evidence: []memory.Evidence{ev}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, e)
	got, ok := c.Memory.Get(e.ID)
	if !ok || got.State != memory.StateVerified {
		t.Fatalf("promotion of %q left state %q, want verified", title, got.State)
	}
	return got
}

// recalledPool returns the recalled memory items the Primary currently holds.
func recalledPool(c *Coord, agentID string) []ctxeng.Item {
	var out []ctxeng.Item
	for _, it := range c.Engine.SnapshotPool(agentID) {
		if it.Class == ctxeng.ClassMemory && it.Source == ctxeng.SourceRecalled {
			out = append(out, it)
		}
	}
	return out
}

func TestPrimaryMemoryDoesNotGrowWithEveryTurn(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	for i := 0; i < 6; i++ {
		promoteFileFact(t, c, s, p, observer, r, fmt.Sprintf("ev-%d", i), fmt.Sprintf("Efficiency tab label rule %d", i),
			fmt.Sprintf("The efficiency tab labels were renamed in webview main.js, note %d.", i))
	}

	c.seedPrimary(recallQuery, nil)
	first := len(recalledPool(c, p.ID))
	if first < 1 || first > memory.RecallLimit {
		t.Fatalf("first turn recalled %d entries, want 1..%d", first, memory.RecallLimit)
	}
	for _, text := range []string{"ALright looks good.\n\nI approve", "Alright, rebuild the client and install it"} {
		c.seedPrimary(text, nil)
		if got := len(recalledPool(c, p.ID)); got > memory.RecallLimit {
			t.Fatalf("turn %q holds %d recalled entries, want at most %d", text, got, memory.RecallLimit)
		}
	}
	c.seedPrimary(recallQuery, nil)
	if got := len(recalledPool(c, p.ID)); got != first {
		t.Fatalf("repeated query holds %d recalled entries, want %d: recall must replace, not accumulate", got, first)
	}
}

func TestRecalledMemoryIsUntrustedAndCapped(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	long := strings.Repeat("efficiency tab labels — héllo wörld ", 8)
	for i := 0; i < 6; i++ {
		promoteFileFact(t, c, s, p, observer, r, fmt.Sprintf("ev-%d", i), fmt.Sprintf("Efficiency tab label %d", i), long)
	}
	c.seedPrimary(recallQuery, nil)
	items := recalledPool(c, p.ID)
	if len(items) == 0 || len(items) > memory.RecallLimit {
		t.Fatalf("recalled %d items, want 1..%d", len(items), memory.RecallLimit)
	}
	block := ctxeng.RenderItems(items)
	if n := ctxeng.Tokens(block); n > recallMaxTokens {
		t.Fatalf("rendered block estimates %d tokens, want at most %d", n, recallMaxTokens)
	}
	if n := len(block); n > recallMaxBytes {
		t.Fatalf("rendered block is %d bytes, want at most %d", n, recallMaxBytes)
	}
	for _, it := range items {
		if it.Verified || it.Source == ctxeng.SourceVerified || !strings.HasPrefix(it.Content, recalledNote) {
			t.Fatalf("recalled item %q not labelled as untrusted: source %q verified %t", it.Title, it.Source, it.Verified)
		}
	}
}

func TestStaleRecallIsDroppedAndNotMarked(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	e := promoteFileFact(t, c, s, p, observer, r, "ev-0", "Efficiency tab label rule", "The efficiency tab labels were renamed.")
	c.seedPrimary(recallQuery, nil)
	if n := len(recalledPool(c, p.ID)); n != 1 {
		t.Fatalf("before the file changed recalled %d entries, want 1", n)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.seedPrimary(recallQuery, nil)
	if n := len(recalledPool(c, p.ID)); n != 0 {
		t.Fatalf("after the file changed recalled %d entries, want 0", n)
	}
	// A view-specific hash mismatch is computed per recipient; the entry itself stays verified.
	if got, _ := c.Memory.Get(e.ID); got.State != memory.StateVerified {
		t.Fatalf("state after view mismatch = %q, want verified", got.State)
	}
}

func TestCommandEvidenceExpiresAfterThirtyDays(t *testing.T) {
	now := time.Now().UTC()
	old := memory.Entry{Evidence: []memory.Evidence{{Type: memory.EvidenceCommandResult, ObservedAt: now.Add(-31 * 24 * time.Hour)}}}
	fresh := memory.Entry{Evidence: []memory.Evidence{{Type: memory.EvidenceCommandResult, ObservedAt: now.Add(-29 * 24 * time.Hour)}}}
	file := memory.Entry{Evidence: []memory.Evidence{{Type: memory.EvidenceFileObservation, ObservedAt: now.Add(-90 * 24 * time.Hour)}}}
	if !commandExpired(old, now) || commandExpired(fresh, now) || commandExpired(file, now) {
		t.Fatalf("expiry: old=%t fresh=%t file=%t, want true false false", commandExpired(old, now), commandExpired(fresh, now), commandExpired(file, now))
	}
}

func TestSettingsMemoryOffDisablesRecallForPrimaryAndWorkers(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	promoteFileFact(t, c, s, p, observer, r, "ev-0", "Efficiency tab label rule", "The efficiency tab labels were renamed.")
	task := taskgraph.Task{ID: "t1", Title: "Propose renames for the efficiency tab labels"}

	c.Settings.Memory = false
	c.seedPrimary(recallQuery, nil)
	if n := len(recalledPool(c, p.ID)); n != 0 {
		t.Fatalf("Primary recalled %d entries with memory off, want 0", n)
	}
	if n := memoryClassCount(c.workerRecall(observer, task)); n != 0 {
		t.Fatalf("worker recalled %d memory items with memory off, want 0", n)
	}

	c.Settings.Memory = true
	if n := memoryClassCount(c.workerRecall(observer, task)); n < 1 {
		t.Fatalf("worker recalled %d memory items with memory on, want at least 1", n)
	}
}

// promoteFactOn is promoteFileFact for an arbitrary workspace file, so one test can stale some entries and not others.
func promoteFactOn(t *testing.T, c *Coord, s *Session, head, observer agent.Agent, r testRepo, file, ref, title string) memory.Entry {
	t.Helper()
	sum, err := hashFile(filepath.Join(r.dir, file))
	if err != nil {
		t.Fatal(err)
	}
	ev := memory.Evidence{Type: memory.EvidenceFileObservation, SessionID: s.ID, AgentID: observer.ID, Ref: ref,
		ObservedAt: time.Now().UTC(), Result: memory.ResultObserved, Files: []memory.EvidenceFile{{Path: file, SHA256: sum}}}
	e, err := c.Memory.ProposeCandidate(memory.Entry{Kind: "fact", Title: title, Body: "Efficiency tab labels note.", Files: []string{file}, Evidence: []memory.Evidence{ev}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, e)
	return e
}

// TestCommandResultEntryIsRecalledWithCurrentAnchor: a promoted command_result is anchored to the observing worktree's
// HEAD, so it is current and recalled on a matching Primary turn.
func TestCommandResultEntryIsRecalledWithCurrentAnchor(t *testing.T) {
	c, s, p, _, r := recallSetup(t)
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: r.dir})
	seedCheck(c, w.ID, "chk-recall", "passed")
	e, err := c.Memory.ProposeCandidate(memory.Entry{Kind: "fact", Title: "Efficiency tab label check", Body: "Efficiency tab labels pass the tests.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceCommandResult, SessionID: s.ID, AgentID: w.ID, Ref: "chk-recall",
			Command: "go test ./internal/memory", ObservedAt: time.Now().UTC(), Result: memory.ResultPassed}}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, p, e)
	if got, _ := c.Memory.Get(e.ID); got.State != memory.StateVerified {
		t.Fatalf("command_result promotion left state %q, want verified", got.State)
	}
	c.seedPrimary(recallQuery, nil)
	items := recalledPool(c, p.ID)
	if len(items) != 1 || items[0].Title != "Efficiency tab label check" {
		t.Fatalf("recalled %d items, want the command_result entry once", len(items))
	}
}

// TestRecallJudgesWorkerFreshnessInItsOwnWorktree: the worker's clone still holds the anchored bytes, while the session
// workspace has changed a.txt. The worker recalls the entry and the session view does not.
func TestRecallJudgesWorkerFreshnessInItsOwnWorktree(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	promoteFileFact(t, c, s, p, observer, r, "ev-0", "Efficiency tab label rule", "The efficiency tab labels were renamed.")
	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", r.dir, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	worker := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: clone})
	if err := os.WriteFile(filepath.Join(r.dir, "a.txt"), []byte("changed in the session workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := taskgraph.Task{ID: "t1", Title: "Propose renames for the efficiency tab labels"}
	if n := memoryClassCount(c.workerRecall(worker, task)); n != 1 {
		t.Fatalf("worker in its own unchanged worktree recalled %d items, want 1", n)
	}
	if n := memoryClassCount(c.workerRecall(agent.Agent{Role: agent.Worker, Workspace: r.dir}, task)); n != 0 {
		t.Fatalf("worktree with changed a.txt recalled %d items, want 0", n)
	}
}

// TestStaleMatchesDoNotUseUpRecallSlots: six matches, two stale, still yield four fresh entries.
func TestStaleMatchesDoNotUseUpRecallSlots(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	for i := 0; i < 4; i++ {
		promoteFactOn(t, c, s, p, observer, r, "a.txt", fmt.Sprintf("ev-a%d", i), fmt.Sprintf("Efficiency tab label fresh %d", i))
	}
	if err := os.WriteFile(filepath.Join(r.dir, "b.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The stale entries are promoted last, so they rank first and would fill the slots without the freshness check.
	for i := 0; i < 2; i++ {
		promoteFactOn(t, c, s, p, observer, r, "b.txt", fmt.Sprintf("ev-b%d", i), fmt.Sprintf("Efficiency tab label stale %d", i))
	}
	if err := os.WriteFile(filepath.Join(r.dir, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.seedPrimary(recallQuery, nil)
	items := recalledPool(c, p.ID)
	if len(items) != memory.RecallLimit {
		t.Fatalf("recalled %d entries with 2 of 6 stale, want %d fresh", len(items), memory.RecallLimit)
	}
	for _, it := range items {
		if strings.Contains(it.Title, "stale") {
			t.Fatalf("stale entry %q was recalled", it.Title)
		}
	}
}

// TestIntegrationRecallIsRecomputedWithinCaps: the post-worker integration refresh recalls for the objective, not the
// turn that came before it, and stays within the four entry, 300 token and 1200 byte caps.
func TestIntegrationRecallIsRecomputedWithinCaps(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	c.seedPrimary("zebra pricing question", nil)
	if n := len(recalledPool(c, p.ID)); n != 0 {
		t.Fatalf("unrelated turn recalled %d entries, want 0", n)
	}
	long := strings.Repeat("efficiency tab labels — héllo wörld ", 8)
	for i := 0; i < 6; i++ {
		promoteFileFact(t, c, s, p, observer, r, fmt.Sprintf("ev-%d", i), fmt.Sprintf("Efficiency tab label %d", i), long)
	}
	c.refreshRecall(p.ID, s.Workspace, c.integrationRecallQuery(s))
	items := recalledPool(c, p.ID)
	if len(items) == 0 || len(items) > memory.RecallLimit {
		t.Fatalf("integration recalled %d items, want 1..%d", len(items), memory.RecallLimit)
	}
	block := ctxeng.RenderItems(items)
	if n := ctxeng.Tokens(block); n > recallMaxTokens {
		t.Fatalf("integration block estimates %d tokens, want at most %d", n, recallMaxTokens)
	}
	if n := len(block); n > recallMaxBytes {
		t.Fatalf("integration block is %d bytes, want at most %d", n, recallMaxBytes)
	}
}

// TestSettingsMemoryOffClearsAlreadyRenderedRecall: recall rendered while memory was on is gone from the next prompt.
func TestSettingsMemoryOffClearsAlreadyRenderedRecall(t *testing.T) {
	c, s, p, observer, r := recallSetup(t)
	promoteFileFact(t, c, s, p, observer, r, "ev-0", "Efficiency tab label rule", "The efficiency tab labels were renamed.")
	c.seedPrimary(recallQuery, nil)
	if n := len(recalledPool(c, p.ID)); n != 1 {
		t.Fatalf("memory on recalled %d entries, want 1", n)
	}
	c.Settings.Memory = false
	c.refreshRecall(p.ID, s.Workspace, c.integrationRecallQuery(s))
	if n := len(recalledPool(c, p.ID)); n != 0 {
		t.Fatalf("after memory turned off, next prompt holds %d recalled entries, want 0", n)
	}
}

func memoryClassCount(items []ctxeng.Item) int {
	n := 0
	for _, it := range items {
		if it.Class == ctxeng.ClassMemory {
			n++
		}
	}
	return n
}
