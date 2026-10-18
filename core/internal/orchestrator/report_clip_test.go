package orchestrator

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
)

// designText is a summary of n characters that ends in a marker, so a test can see whether its end survived.
func designText(n int) string {
	unit := "The TLS handshake pins the CA bundle and rotates session keys. "
	return strings.Repeat(unit, n/len(unit)+1)[:n] + "END OF DESIGN"
}

func TestClampKeepsArchitectDesignWithinDesignLimit(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	design := designText(20000)
	if got := c.clampReportFor(agent.Architect, RawReport{Summary: design}).Summary; got != design {
		t.Fatalf("architect summary clipped to %d runes, want the %d it wrote", len([]rune(got)), len([]rune(design)))
	}
}

func TestClampCapsArchitectDesignAtDesignLimit(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	design := designText(30000)
	got := c.clampReportFor(agent.Architect, RawReport{Summary: design}).Summary
	if n := len([]rune(got)); n != designReportMaxChars {
		t.Fatalf("architect summary = %d runes, want %d", n, designReportMaxChars)
	}
	m := regexp.MustCompile(`\.\.\.\[clipped (\d+)\]$`).FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("no clip marker at the end: %q", got[len(got)-40:])
	}
	// The marker counts every rune cut, its own length included.
	removed, _ := strconv.Atoi(m[1])
	total := len([]rune(design))
	if want := total - (designReportMaxChars - len([]rune("...[clipped "+m[1]+"]"))); removed != want {
		t.Fatalf("marker counts %d removed, want %d", removed, want)
	}
}

func TestClampKeepsConfiguredLimitForOtherRoles(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	if c.settings().ReportMaxChars != 1200 {
		t.Fatalf("default ReportMaxChars = %d, want 1200", c.settings().ReportMaxChars)
	}
	for _, role := range []agent.Role{agent.Worker, agent.Probe, agent.Reviewer, agent.Debug, agent.Primary} {
		got := c.clampReportFor(role, RawReport{Summary: designText(20000)}).Summary
		if n := len([]rune(got)); n != 1200 || !strings.Contains(got, "...[clipped ") {
			t.Fatalf("%s: summary = %d runes, want the configured 1200 with a marker", role, n)
		}
	}
}

// The design reaches the dependent worker whole. The Primary, which only plans, keeps the short form.
func TestDependentWorkerReceivesFullArchitectDesign(t *testing.T) {
	designJSON, _ := json.Marshal(map[string]any{"status": "completed", "summary": designText(20000)})
	c, r, s := briefFixture(t, string(designJSON), `{"status":"completed","summary":"built it"}`)
	arch := c.Graph.Add(taskgraph.Task{Title: "Design TLS and auth", Role: string(agent.Architect), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "hard"})
	if err := c.execWorker(context.Background(), arch, c.spawn(arch, agent.Architect)); err != nil {
		t.Fatal(err)
	}

	for _, it := range c.Engine.SnapshotPool(c.primaryID(t)) {
		if strings.Contains(it.Content, "END OF DESIGN") {
			t.Fatalf("the Primary's context holds the full design: %d chars", len(it.Content))
		}
	}

	work := c.Graph.Add(taskgraph.Task{Title: "Implement TLS", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium", Depends: []string{arch.ID}})
	seeded := false
	for _, it := range c.workerSeed(work, agent.Worker) {
		if it.Class == "report" && strings.Contains(it.Content, "END OF DESIGN") {
			seeded = true
		}
	}
	if !seeded {
		t.Fatal("the dependent worker's seed does not hold the full architect design")
	}

	w := c.spawn(work, agent.Worker)
	if err := c.execWorker(context.Background(), work, w); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 2 {
		t.Fatalf("model calls = %d, want 2", len(r.tasks))
	}
	if first := r.tasks[1].System + r.tasks[1].Prompt; !strings.Contains(first, "END OF DESIGN") {
		t.Fatal("the dependent worker's first message does not contain the full architect design")
	}
}
