package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/workspace"
)

// A context refresh changes what the agent knows and never what it holds. The new generation names the revision the
// agent has by then, the one before it keeps its own, the edits are still there byte for byte, and an excerpt of a
// file the agent has since changed does not come along.
func TestContextRefreshPreservesEditsAndRebindsToTheCurrentRevision(t *testing.T) {
	for _, mode := range []workspace.Mode{workspace.ModeWorktree, workspace.ModeVirtual} {
		t.Run(string(mode), func(t *testing.T) {
			dir := gitRepo(t)
			ws := workspace.New(dir)
			ws.Configure(workspace.Config{Mode: mode})
			c := New(logging.New(nil, logging.Error))
			c.SetWorkspaceManager(ws)
			c.CreateSession(dir, "", ModeBuild)
			var mu sync.Mutex
			var stale []map[string]any
			c.SetEmitter(func(name string, payload any) {
				if m, ok := payload.(map[string]any); ok && name == "context.stale" {
					mu.Lock()
					stale = append(stale, m)
					mu.Unlock()
				}
			})

			tree, err := mountTree(c, "agent_x")
			if err != nil {
				t.Fatal(err)
			}
			c.Engine.Bind("agent_x", "t1", "worker", 8000)
			c.Engine.Ingest("agent_x", []ctxeng.Item{{ID: "obj", Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Objective", Content: "Edit a.txt.", Durability: 1}})
			write := func(body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(tree.Path, "a.txt"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("one\nfirst edit\n")

			c.bindWorkspaceContext("agent_x")
			from := c.deriveFiles("agent_x", []string{"a.txt"})
			if from == nil || from.Hashes["a.txt"] != ctxeng.BlobHash([]byte("one\nfirst edit\n")) || from.AgentID != "agent_x" {
				t.Fatalf("derivation = %+v", from)
			}
			c.Engine.Ingest("agent_x", []ctxeng.Item{{ID: "excerpt", Class: ctxeng.ClassCode, Source: ctxeng.SourceRepo, Title: "a.txt", Content: "one\nfirst edit\n", Files: []string{"a.txt"}, Durability: 1, Relevance: 1, From: from}})
			g1, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: "agent_x", Reason: "start"})
			if err != nil {
				t.Fatal(err)
			}
			if g1.Workspace == nil || g1.Workspace.AgentID != "agent_x" || g1.Workspace.BaseCommit == "" || g1.Workspace.EffectiveTreeOID == "" {
				t.Fatalf("generation 1 workspace = %+v", g1.Workspace)
			}
			if !strings.Contains(ctxeng.Render(g1), "first edit") {
				t.Fatalf("setup: excerpt missing\n%s", ctxeng.Render(g1))
			}
			patchBefore, err := ws.PatchPath("agent_x", "a.txt")
			if err != nil {
				t.Fatal(err)
			}

			// The agent edits the file again, then its context is refreshed.
			write("one\nsecond edit\n")
			g2, err := c.RefreshAgent("agent_x", "manual refresh")
			if err != nil {
				t.Fatal(err)
			}
			if g2.Workspace == nil || g2.Workspace.BaseCommit != g1.Workspace.BaseCommit {
				t.Fatalf("the base moved: %+v -> %+v", g1.Workspace, g2.Workspace)
			}
			if g2.Workspace.EffectiveTreeOID == g1.Workspace.EffectiveTreeOID {
				t.Fatalf("generation 2 names the old tree %s", g2.Workspace.EffectiveTreeOID)
			}
			if mode == workspace.ModeVirtual && g2.Workspace.OverlayRevision <= g1.Workspace.OverlayRevision {
				t.Fatalf("revision %d -> %d", g1.Workspace.OverlayRevision, g2.Workspace.OverlayRevision)
			}
			if got := c.Engine.Generation("agent_x", g1.Number).Workspace; got.EffectiveTreeOID != g1.Workspace.EffectiveTreeOID {
				t.Fatalf("an earlier generation was rewritten: %+v", got)
			}
			if text := ctxeng.Render(g2); strings.Contains(text, "first edit") {
				t.Fatalf("a refreshed context still shows the file as it was:\n%s", text)
			}
			mu.Lock()
			if len(stale) != 1 || stale[0]["agentId"] != "agent_x" {
				t.Fatalf("context.stale = %+v", stale)
			}
			mu.Unlock()

			// The workspace is exactly what the agent left.
			if got, err := agentFile(c, agentOf("agent_x"), "a.txt"); err != nil || got != "one\nsecond edit\n" {
				t.Fatalf("a.txt = %q %v", got, err)
			}
			patchAfter, err := ws.PatchPath("agent_x", "a.txt")
			if err != nil || !strings.Contains(patchAfter, "+second edit") || patchAfter == patchBefore {
				t.Fatalf("patch after refresh = %q (before %q) %v", patchAfter, patchBefore, err)
			}
			if files, err := ws.ChangedFiles("agent_x"); err != nil || len(files) != 1 || files[0].Path != "a.txt" {
				t.Fatalf("changed files = %+v %v", files, err)
			}
		})
	}
}

// An agent that works in the user's own folder has no workspace to bind to, and nothing is claimed about one.
func TestAgentsWithoutAnIsolatedWorkspaceGetNoWorkspaceRef(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.CreateSession(dir, "", ModeBuild)
	if _, err := c.WS.EnsureTree("reader", true); err != nil { // read-only agents read the user's folder
		t.Fatal(err)
	}
	c.Engine.Bind("reader", "t1", "probe", 8000)
	c.bindWorkspaceContext("reader")
	if d := c.deriveFiles("reader", []string{"a.txt"}); d != nil {
		t.Fatalf("derivation for a shared folder = %+v", d)
	}
	g, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: "reader", Reason: "start"})
	if err != nil || g.Workspace != nil {
		t.Fatalf("generation = %+v %v", g.Workspace, err)
	}
	if res := c.revalidateContext("reader"); res.Checked != 0 {
		t.Fatalf("revalidation = %+v", res)
	}
}
