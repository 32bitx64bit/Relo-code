package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/workspace"
)

// The whole request path in a real repository with uncommitted work: snapshot, worktrees, stacked dependents,
// facts for the Primary, driven by the mock runtime.
func TestWholeRequestInADirtyRepository(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(logging.New(nil, logging.Error))
	c.Runtime = runtime.NewMock()
	c.SetWorkspaceManager(workspace.New(dir))
	c.Settings.PrimaryModel, c.Settings.Tier1Model, c.Settings.Tier2Model = "mock/relo-primary", "mock/relo-tier1", "mock/relo-tier2"
	c.Settings.AutoVerify = false
	c.CreateSession(dir, "Add Google and GitHub OAuth login.", ModeBuild)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Submit(ctx, "Add Google and GitHub OAuth login.", nil); err != nil {
		t.Fatal(err)
	}
	if c.Busy() {
		t.Fatal("agents still active")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "uncommitted\n" {
		t.Fatalf("the user's file changed: %q", got)
	}
	for _, m := range c.Current().Messages {
		if m.Role == "error" {
			t.Fatalf("error in chat: %s", m.Text)
		}
	}
	facts := c.workspaceFacts()
	if !strings.Contains(facts, "1 uncommitted path(s): a.txt") {
		t.Fatalf("facts do not describe the user's tree:\n%s", facts)
	}
	for _, ch := range c.Changes() {
		if ch.Path == "a.txt" {
			t.Fatalf("the user's own edit was reported as an agent's change: %+v", ch)
		}
	}
	t.Logf("tasks=%d changes=%d\n%s", len(c.Graph.List()), len(c.Changes()), facts)
}
