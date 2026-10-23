package orchestrator

import (
	"context"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/taskgraph"
)

// A prose reply that says "Status: blocked" or "Status: failed" must keep that status, never become completed.
func TestProseStatusReplyKeepsItsVerdict(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime())
	s := c.CreateSession(t.TempDir(), "prose", ModeBuild)
	a := agent.Agent{ID: "w", Role: agent.Worker, Workspace: s.Workspace}
	cases := []struct{ body, want string }{
		{"I cannot continue.\n\nStatus: blocked\nQuestion: which branch should I merge into?", "blocked"},
		{"The build broke.\n**Status:** failed", "failed"},
		{"Done reviewing.\n- Status: **failed**", "failed"},
	}
	for _, tc := range cases {
		rep, err := c.workerReport(context.Background(), a, prompt.Parts{}, tc.body)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Status != tc.want {
			t.Fatalf("prose %q: status = %q, want %q", tc.body, rep.Status, tc.want)
		}
	}
	rep, err := c.workerReport(context.Background(), a, prompt.Parts{}, "Merged the branch cleanly.")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "completed" {
		t.Fatalf("prose without a status line: status = %q, want completed", rep.Status)
	}
}

// A valid failed verdict from a reviewer is delivered, not retried. An invalid or prose reply still retries.
func TestNegativeReviewerVerdictIsNotRetried(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "verdict", ModeBuild)
	c.Settings.AutoReview = false
	c.Sched = schedulerForRetry(3)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	rev := c.Agents.Create(agent.Agent{Role: agent.Reviewer, Workspace: s.Workspace})

	valid := c.Graph.Add(taskgraph.Task{ID: "valid-verdict", Role: string(agent.Reviewer), Workspace: s.Workspace, Attempt: 1})
	c.acceptReport(valid, rev, RawReport{Status: "failed", Summary: "The diff breaks the login flow."}, verifyOutcome{})
	got, _ := c.Graph.Get(valid.ID)
	if got.Status != taskgraph.Failed || got.Attempt != 1 {
		t.Fatalf("valid negative verdict was retried: status=%s attempt=%d", got.Status, got.Attempt)
	}
	if report, ok := c.Agents.Report(rev.ID); !ok || report.Status != agent.Failed || report.Summary != "The diff breaks the login flow." {
		t.Fatalf("valid negative verdict not delivered to the Primary: %+v", report)
	}

	prose := c.Graph.Add(taskgraph.Task{ID: "prose-verdict", Role: string(agent.Reviewer), Workspace: s.Workspace, Attempt: 1})
	c.acceptReport(prose, rev, RawReport{Status: "failed", Summary: "Status: failed", unstructured: true}, verifyOutcome{})
	if got, _ := c.Graph.Get(prose.ID); got.Status != taskgraph.Queued {
		t.Fatalf("invalid prose reply was not retried: status=%s", got.Status)
	}
}
