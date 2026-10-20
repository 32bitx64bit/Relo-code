package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/runtime"
)

// newCanvasRoleCoord builds a coord whose only agent has the given role and holds a live canvas capability key.
func newCanvasRoleCoord(t *testing.T, mode Mode, role agent.Role) (*Coord, *canvasEvents, agent.Agent, string) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	c.CanvasEnabled = true
	c.CreateSession(t.TempDir(), "design a landing page", mode)
	sender := c.Agents.Create(agent.Agent{ID: "sender", Role: role, Status: agent.Running})
	c.runtimeSessions[sender.ID] = &runtimeSession{id: "runtime-sender"}
	key, err := c.bindPeer(sender, "runtime-sender")
	if err != nil {
		t.Fatal(err)
	}
	ev := &canvasEvents{}
	c.SetEmitter(ev.record)
	return c, ev, sender, key
}

// canvasRoleRequest builds a request for an action that writes or reads artifact state.
func canvasRoleRequest(key, action string) agentmsg.CanvasRequest {
	req := agentmsg.CanvasRequest{Key: key, Action: action}
	if action == "create" {
		req.Format = "html"
		req.Entry = "index.html"
		req.Name = "Landing"
		req.Files = map[string]string{"index.html": "<h1>hi</h1>"}
	}
	if action == "update" {
		req.ID = "art_1"
		req.Revision = 1
		req.Format = "html"
		req.Entry = "index.html"
		req.Files = map[string]string{"index.html": "<h1>hi</h1>"}
	}
	if action == "open" || action == "inspect" || action == "capture" || action == "popout" || action == "close" {
		req.ID = "art_1"
	}
	return req
}

// The Primary may not author a canvas. Every authoring action is refused at the orchestrator, before the request
// reaches the extension, and no canvas.requested is emitted for it.
func TestCanvasRolePolicyPrimaryCannotAuthor(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Primary)
	for _, action := range []string{"create", "update"} {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, action))
		if err == nil {
			t.Fatalf("primary %s: accepted", action)
		}
		if !strings.Contains(err.Error(), "not available to the primary role") {
			t.Fatalf("primary %s: err = %v", action, err)
		}
	}
	if n := len(ev.list()); n != 0 {
		t.Fatalf("denied primary writes emitted %d canvas events", n)
	}
}

// The Primary may not render or drive the user's canvas panel either: it only reads a canvas.
func TestCanvasRolePolicyPrimaryCannotRenderOrDriveCanvas(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Primary)
	for _, action := range []string{"capture", "popout", "close"} {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, action))
		if err == nil {
			t.Fatalf("primary %s: accepted", action)
		}
		if !strings.Contains(err.Error(), "not available to the primary role") {
			t.Fatalf("primary %s: err = %v", action, err)
		}
	}
	if n := len(ev.list()); n != 0 {
		t.Fatalf("denied primary actions emitted %d canvas events", n)
	}
}

// The Primary keeps a read-only path: it may list, open and inspect a canvas to check a worker's result.
func TestCanvasRolePolicyPrimaryMayInspect(t *testing.T) {
	for _, action := range []string{"list", "open", "inspect"} {
		c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Primary)
		done := make(chan error, 1)
		go func() {
			_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, action))
			done <- err
		}()
		reqID := awaitEvent(t, ev)
		if err := c.CanvasReply(reqID, []any{}, ""); err != nil {
			t.Fatalf("%s: CanvasReply: %v", action, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("primary %s: %v", action, err)
		}
	}
}

// The roles that only investigate or verify other agents' work may not author either.
func TestCanvasRolePolicyNonBuilderRolesCannotAuthor(t *testing.T) {
	for _, role := range []agent.Role{agent.Head, agent.Probe, agent.Reviewer} {
		c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, role)
		for _, action := range []string{"create", "update"} {
			_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, action))
			if err == nil {
				t.Fatalf("%s %s: accepted", role, action)
			}
			if !strings.Contains(err.Error(), "not available to the "+string(role)+" role") {
				t.Fatalf("%s %s: err = %v", role, action, err)
			}
		}
		if n := len(ev.list()); n != 0 {
			t.Fatalf("%s: denied writes emitted %d canvas events", role, n)
		}
	}
}

// A role the core does not know may not author: the policy fails closed on an unknown role.
func TestCanvasRolePolicyUnknownRoleCannotAuthor(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Role("designer"))
	for _, action := range []string{"create", "update"} {
		if _, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, action)); err == nil {
			t.Fatalf("unknown role %s: accepted", action)
		}
	}
	if n := len(ev.list()); n != 0 {
		t.Fatalf("unknown role: denied writes emitted %d canvas events", n)
	}
}

// A builder role keeps the full canvas capability: the request reaches the extension and the answer comes back.
func TestCanvasRolePolicyBuilderMayAuthor(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	evt := ev.list()[0]
	if evt["agentId"] != "sender" || evt["role"] != "worker" || evt["owner"] != "sender" {
		t.Fatalf("event envelope = %v", evt)
	}
	if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err != nil {
		t.Fatalf("CanvasReply: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("builder create: %v", err)
	}
}

// A builder may not author outside build mode: the mode rule still applies after the role rule.
func TestCanvasRolePolicyBuilderStillNeedsBuildMode(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModePlan, agent.Worker)
	_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
	if err == nil || !strings.Contains(err.Error(), "build mode") {
		t.Fatalf("builder create in plan mode: err = %v", err)
	}
	if n := len(ev.list()); n != 0 {
		t.Fatalf("plan mode builder emitted %d canvas events", n)
	}
}

// The envelope tells the extension whether the role may read an artifact that carries no owner. Only an authoring
// role may, so the flag is false for the Primary and for every other role that does not author.
func TestCanvasRequestSendsTheReadUnownedFlag(t *testing.T) {
	for _, tc := range []struct {
		role agent.Role
		want bool
	}{
		{agent.Primary, false},
		{agent.Head, false},
		{agent.Probe, false},
		{agent.Reviewer, false},
		{agent.Role("designer"), false},
		{agent.Worker, true},
		{agent.Architect, true},
		{agent.Debug, true},
		{agent.Test, true},
		{agent.Integration, true},
	} {
		c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, tc.role)
		done := make(chan error, 1)
		go func() {
			_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "list"))
			done <- err
		}()
		reqID := awaitEvent(t, ev)
		got, ok := ev.list()[0]["readUnowned"].(bool)
		if !ok || got != tc.want {
			t.Fatalf("%s readUnowned = %v, want %v", tc.role, ev.list()[0]["readUnowned"], tc.want)
		}
		if err := c.CanvasReply(reqID, []any{}, ""); err != nil {
			t.Fatalf("%s: CanvasReply: %v", tc.role, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%s list: %v", tc.role, err)
		}
	}
}

// The answer is refused when the owner of the request is no longer active.
func TestCanvasReplyRejectsInactiveOwner(t *testing.T) {
	c, ev, sender, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	c.Agents.Update(sender.ID, func(x *agent.Agent) { x.Status = agent.Completed })
	if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err == nil {
		t.Fatal("reply for a finished owner was accepted")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no longer active") {
			t.Fatalf("owner error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent was not told that the reply was refused")
	}
}

// The answer is refused when the owner changed role: the role that asked is not the role that answers.
func TestCanvasReplyRejectsChangedRole(t *testing.T) {
	c, ev, sender, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	c.Agents.Update(sender.ID, func(x *agent.Agent) { x.Role = agent.Primary })
	if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err == nil {
		t.Fatal("reply for a changed role was accepted")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "changed role") {
			t.Fatalf("role error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent was not told that the reply was refused")
	}
}

// The answer is refused when the turn that made the request has ended: the capability key is gone.
func TestCanvasReplyRejectsEndedTurn(t *testing.T) {
	c, ev, sender, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	c.revokePeer(sender.ID)
	if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err == nil {
		t.Fatal("reply for an ended turn was accepted")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the agent received a result for an ended turn")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent was not told that the reply was refused")
	}
}

// The answer is refused when the owner id no longer matches the agent record: the id that made the request is not
// the agent the record describes any more.
func TestCanvasReplyRejectsMismatchedOwnerID(t *testing.T) {
	c, ev, sender, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	c.Agents.Update(sender.ID, func(x *agent.Agent) { x.ID = "someone-else" })
	if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err == nil {
		t.Fatal("reply for a mismatched owner id was accepted")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no longer matches") {
			t.Fatalf("owner id error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent was not told that the reply was refused")
	}
}

// A reply for a request nobody made is refused, and the pending slot is released.
func TestCanvasReplyRejectsUnknownRequest(t *testing.T) {
	c, _, _, _ := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	if err := c.CanvasReply("canvas_missing", map[string]any{}, ""); err == nil {
		t.Fatal("reply for an unknown request was accepted")
	}
}
