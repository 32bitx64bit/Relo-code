package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/workspace"
)

// referenceCoord is two workers in virtual workspaces, the first of which has written some files.
func referenceCoord(t *testing.T) (c *Coord, dir string, senderKey, targetKey string) {
	t.Helper()
	dir = gitRepo(t)
	ws := workspace.New(dir)
	ws.Configure(workspace.Config{Mode: workspace.ModeVirtual})
	c = New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(ws)
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	c.CreateSession(dir, "coordinate", ModeBuild)
	for _, id := range []string{"sender", "target"} {
		a := c.Agents.Create(agent.Agent{ID: id, Role: agent.Worker, Status: agent.Running})
		c.runtimeSessions[id] = &runtimeSession{id: runtime.SessionID("runtime-" + id)}
		key, err := c.bindPeer(a, runtime.SessionID("runtime-"+id))
		if err != nil {
			t.Fatal(err)
		}
		if id == "sender" {
			senderKey = key
		} else {
			targetKey = key
		}
	}
	tree, err := mountTree(c, "sender")
	if err != nil || !tree.Virtual {
		t.Fatalf("sender tree = %+v %v", tree, err)
	}
	for name, body := range map[string]string{"api.go": "package api\n", "api_doc.md": "docs\n", "private.txt": "not for sharing\n"} {
		if err := os.WriteFile(filepath.Join(tree.Path, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.WS.EnsureTree("target", false); err != nil {
		t.Fatal(err)
	}
	return c, dir, senderKey, targetKey
}

func peerCall(t *testing.T, c *Coord, req agentmsg.Request) (any, error) {
	t.Helper()
	return c.peerRequest(context.Background(), req)
}

func TestAMessageCanPointAtFilesAndTheRecipientImportsOnlyWhatItChooses(t *testing.T) {
	c, dir, senderKey, targetKey := referenceCoord(t)
	v, err := peerCall(t, c, agentmsg.Request{Key: senderKey, Action: "send", To: "target", Text: "api.go is ready", Paths: []string{"api.go", "api_doc.md"}})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(PeerMessage)
	if m.Change == nil || m.Change.SenderAgent != "sender" || m.Change.Commit == "" || strings.Join(m.Change.Paths, ",") != "api.go,api_doc.md" {
		t.Fatalf("message = %+v", m)
	}
	if strings.Contains(peerText(m), "package api") {
		t.Fatalf("the message carries the code instead of pointing at it:\n%s", peerText(m))
	}
	if !strings.Contains(peerText(m), "NOT in your workspace") {
		t.Fatalf("the recipient is not told the files are absent:\n%s", peerText(m))
	}

	// Its context knows the change exists; its workspace holds none of it.
	g, err := c.Engine.Refresh("target", "after message")
	if err != nil {
		t.Fatal(err)
	}
	if text := ctxeng.Render(g); !strings.Contains(text, "api_doc.md") || strings.Contains(text, "package api") {
		t.Fatalf("context after the message:\n%s", text)
	}
	tv, _ := c.WS.View("target")
	if _, err := tv.ReadFile("api.go"); !workspace.IsNotExist(err) {
		t.Fatalf("the message made the code visible: %v", err)
	}

	// Only the addressee can import, and only a message that names files.
	if _, err := peerCall(t, c, agentmsg.Request{Key: senderKey, Action: "import", Message: m.ID}); err == nil {
		t.Fatal("the sender imported its own message")
	}
	if _, err := peerCall(t, c, agentmsg.Request{Key: targetKey, Action: "import", Message: "peer_nope"}); err == nil {
		t.Fatal("imported a message that does not exist")
	}
	if _, err := peerCall(t, c, agentmsg.Request{Key: targetKey, Action: "import", Message: m.ID, Paths: []string{"private.txt"}}); err == nil {
		t.Fatal("imported a file the message does not name")
	}
	out, err := peerCall(t, c, agentmsg.Request{Key: targetKey, Action: "import", Message: m.ID, Paths: []string{"api.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(map[string]any)["imported"].([]string); strings.Join(got, ",") != "api.go" {
		t.Fatalf("imported = %v", got)
	}
	if got, err := tv.ReadFile("api.go"); err != nil || string(got) != "package api\n" {
		t.Fatalf("api.go = %q %v", got, err)
	}
	for _, name := range []string{"api_doc.md", "private.txt"} {
		if _, err := tv.ReadFile(name); !workspace.IsNotExist(err) {
			t.Fatalf("%s arrived without being asked for: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "api.go")); !os.IsNotExist(err) {
		t.Fatalf("an import reached the user's folder: %v", err)
	}
	if changes, err := c.WS.ChangedFiles("target"); err != nil || len(changes) != 1 || changes[0].Path != "api.go" {
		t.Fatalf("target's change = %+v %v", changes, err)
	}
	c.mu.Lock()
	imported := c.sess.PeerMessages[len(c.sess.PeerMessages)-1].Imported
	c.mu.Unlock()
	if strings.Join(imported, ",") != "api.go" {
		t.Fatalf("the message does not record what was imported: %v", imported)
	}
}

func TestAttachingFilesIsRefusedWhenItCannotBeDoneSafely(t *testing.T) {
	c, _, senderKey, _ := referenceCoord(t)
	for name, paths := range map[string][]string{
		"an unchanged file":   {"a.txt"},
		"protected metadata":  {".git/config"},
		"an escaping path":    {"../outside.txt"},
		"more than the limit": make([]string, agentmsg.MaxPaths+1),
	} {
		if _, err := peerCall(t, c, agentmsg.Request{Key: senderKey, Action: "send", To: "target", Text: "x", Paths: paths}); err == nil {
			t.Fatalf("%s: the message was sent", name)
		}
	}
	c.mu.Lock()
	n := len(c.sess.PeerMessages)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("a refused attachment still queued %d message(s)", n)
	}
	// A plain message is unaffected.
	if _, err := peerCall(t, c, agentmsg.Request{Key: senderKey, Action: "send", To: "target", Text: "just text"}); err != nil {
		t.Fatal(err)
	}
}

func TestAWorktreeSenderCanStillSendTextButNotAReference(t *testing.T) {
	dir := gitRepo(t)
	ws := workspace.New(dir)
	ws.Configure(workspace.Config{Mode: workspace.ModeWorktree})
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(ws)
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	c.CreateSession(dir, "coordinate", ModeBuild)
	a := c.Agents.Create(agent.Agent{ID: "sender", Role: agent.Worker, Status: agent.Running})
	c.Agents.Create(agent.Agent{ID: "target", Role: agent.Worker, Status: agent.Running})
	c.runtimeSessions["sender"] = &runtimeSession{id: "runtime-sender"}
	key, err := c.bindPeer(a, "runtime-sender")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := c.WS.EnsureTree("sender", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Path, "api.go"), []byte("package api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = peerCall(t, c, agentmsg.Request{Key: key, Action: "send", To: "target", Text: "ready", Paths: []string{"api.go"}})
	if err == nil || !strings.Contains(err.Error(), "virtual workspace") {
		t.Fatalf("worktree sender with paths: %v", err)
	}
	if _, err := peerCall(t, c, agentmsg.Request{Key: key, Action: "send", To: "target", Text: "ready, see api.go"}); err != nil {
		t.Fatalf("plain text from a worktree sender: %v", err)
	}
}

func TestAnAgentInTheUsersFolderCannotImport(t *testing.T) {
	c, _, senderKey, _ := referenceCoord(t)
	head := c.Agents.Create(agent.Agent{ID: "head", Role: agent.Primary, Status: agent.Running})
	c.runtimeSessions["head"] = &runtimeSession{id: "runtime-head"}
	headKey, err := c.bindPeer(head, "runtime-head")
	if err != nil {
		t.Fatal(err)
	}
	v, err := peerCall(t, c, agentmsg.Request{Key: senderKey, Action: "send", To: "head", Text: "done", Paths: []string{"api.go"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = peerCall(t, c, agentmsg.Request{Key: headKey, Action: "import", Message: v.(PeerMessage).ID})
	if err == nil || !strings.Contains(err.Error(), "user's own folder") {
		t.Fatalf("import into the user's folder: %v", err)
	}
}
