package orchestrator

import (
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/taskgraph"
)

// Reviewers produce structured findings and often repeat one. Removing a repeat while ranging over the same slice
// ran past its new end ("slice bounds out of range [3:2]") and failed the reviewer after it had done all the work.
func TestClampReportDropsRepeatedFindingsWithoutPanicking(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	f := func(s string) RawFinding { return RawFinding{Kind: "risk", Summary: s} }
	got := c.clampReport(RawReport{Status: "completed", Summary: "ok", Structured: []RawFinding{f("a"), f("a"), f("b"), f("a"), f("b"), f("c"), f("c")}})
	if len(got.Structured) != 3 || got.Structured[0].Summary != "a" || got.Structured[1].Summary != "b" || got.Structured[2].Summary != "c" {
		t.Fatalf("structured = %+v, want a, b, c once each in order", got.Structured)
	}
	if got := c.clampReport(RawReport{Structured: []RawFinding{f("x"), f("x")}}); len(got.Structured) != 1 {
		t.Fatalf("pair of repeats left %d", len(got.Structured))
	}
	if got := c.clampReport(RawReport{}); len(got.Structured) != 0 {
		t.Fatalf("empty report grew findings: %+v", got.Structured)
	}
}

func TestReviewIsForBigOrHardChangesOnly(t *testing.T) {
	files := func(n, lines int) []Change {
		out := make([]Change, n)
		for i := range out {
			out[i] = Change{Path: string(rune('a' + i)), Added: lines / max(n, 1)}
		}
		return out
	}
	for _, tc := range []struct {
		name       string
		difficulty string
		changes    []Change
		want       bool
	}{
		{"small medium change", routing.Medium, files(4, 65), false},
		{"small easy change", routing.Easy, files(1, 10), false},
		{"trivial never", routing.Trivial, files(10, 1000), false},
		{"nothing changed", routing.Hard, nil, false},
		{"hard change", routing.Hard, files(1, 10), true},
		{"expert change", routing.Expert, files(1, 10), true},
		{"many files", routing.Medium, files(reviewMinFiles, 12), true},
		{"many lines", routing.Medium, files(2, reviewMinLines), true},
	} {
		if got := reviewWorthwhile(taskgraph.Task{Difficulty: tc.difficulty}, tc.changes); got != tc.want {
			t.Errorf("%s: reviewWorthwhile = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func reviewers(c *Coord) int {
	n := 0
	for _, got := range c.Graph.List() {
		if got.Role == string(agent.Reviewer) {
			n++
		}
	}
	return n
}

// The Primary usually plans a reviewer of its own; the automatic one must not add a second for the same work.
func TestAutoReviewYieldsToAReviewerTheMainAgentPlanned(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	work := c.Graph.Add(taskgraph.Task{ID: "w", Title: "Rename", Role: "worker", Status: taskgraph.Done})
	c.Graph.Add(taskgraph.Task{ID: "planned", Title: "Independent review", Role: "reviewer", Depends: []string{"w"}, Status: taskgraph.Queued})
	c.scheduleReview(work, agent.Agent{ID: "a"})
	if n := reviewers(c); n != 1 {
		t.Fatalf("reviewers = %d, want only the planned one", n)
	}

	// A planned reviewer that already failed does not count: the work still wants one.
	c2 := New(logging.New(nil, logging.Error))
	c2.CreateSession(t.TempDir(), "", ModeBuild)
	work2 := c2.Graph.Add(taskgraph.Task{ID: "w", Title: "Rename", Role: "worker", Status: taskgraph.Done})
	c2.Graph.Add(taskgraph.Task{ID: "planned", Title: "Independent review", Role: "reviewer", Depends: []string{"w"}, Status: taskgraph.Failed})
	c2.scheduleReview(work2, agent.Agent{ID: "a"})
	if n := reviewers(c2); n != 2 {
		t.Fatalf("reviewers = %d, want the failed one plus a fresh one", n)
	}
}
