package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/runtime"
)

// canvasEvents records canvas.requested emissions so tests can assert on what reached the extension.
type canvasEvents struct {
	mu     sync.Mutex
	events []map[string]any
}

func (e *canvasEvents) record(name string, payload any) {
	if name != "canvas.requested" {
		return
	}
	m, _ := payload.(map[string]any)
	e.mu.Lock()
	e.events = append(e.events, m)
	e.mu.Unlock()
}

func (e *canvasEvents) list() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.events...)
}

func newCanvasCoord(t *testing.T, mode Mode) (*Coord, *canvasEvents, agent.Agent, string) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	c.CanvasEnabled = true
	c.CreateSession(t.TempDir(), "design a landing page", mode)
	sender := c.Agents.Create(agent.Agent{ID: "sender", Role: agent.Worker, Status: agent.Running})
	c.runtimeSessions[sender.ID] = &runtimeSession{id: "runtime-sender"}
	key, err := c.bindPeer(sender, "runtime-sender")
	if err != nil {
		t.Fatal(err)
	}
	ev := &canvasEvents{}
	c.SetEmitter(ev.record)
	return c, ev, sender, key
}

// awaitEvent waits for the first canvas.requested and returns its request ID.
func awaitEvent(t *testing.T, ev *canvasEvents) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if list := ev.list(); len(list) > 0 {
			return list[0]["requestId"].(string)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("canvas.requested was not emitted")
	return ""
}

func TestCanvasRequestBridgesAuthorizedReply(t *testing.T) {
	c, ev, sender, key := newCanvasCoord(t, ModeBuild)
	type outcome struct {
		v   any
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		v, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: "create", Format: "html", Entry: "index.html", Files: map[string]string{"index.html": "<h1>hi</h1>"}, Name: "Landing"})
		done <- outcome{v, err}
	}()
	reqID := awaitEvent(t, ev)
	evt := ev.list()[0]
	if evt["sessionId"] != c.SessionID() || evt["agentId"] != sender.ID || evt["workspace"] == "" {
		t.Fatalf("event envelope = %v", evt)
	}
	// The key is a capability: it must never be sent to the extension.
	if strings.Contains(mustJSON(t, evt), key) {
		t.Fatal("canvas.requested leaked the agent key")
	}
	if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err != nil {
		t.Fatalf("CanvasReply: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("canvasRequest: %v", got.err)
	}
	if !strings.Contains(mustJSON(t, got.v), "art_1") {
		t.Fatalf("result = %v", got.v)
	}
}

func TestCanvasReplyErrorIsReturnedToModel(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModeBuild)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: "open", ID: "art_9"})
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	if err := c.CanvasReply(reqID, nil, "artifact not found"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "artifact not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestCanvasModeDenyEmitsNoEvent(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeReview} {
		c, ev, _, key := newCanvasCoord(t, mode)
		for _, action := range []string{"create", "update"} {
			_, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: action, ID: "a", Format: "html", Entry: "a.html", Files: map[string]string{"a.html": "x"}})
			if err == nil || !strings.Contains(err.Error(), "build mode") {
				t.Fatalf("%s in %s: err = %v", action, mode, err)
			}
		}
		if n := len(ev.list()); n != 0 {
			t.Fatalf("%s: %d canvas events emitted for denied writes", mode, n)
		}
	}
}

func TestCanvasReadActionsAllowedInPlanMode(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModePlan)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: "list"})
		done <- err
	}()
	if err := c.CanvasReply(awaitEvent(t, ev), []any{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("list in plan mode: %v", err)
	}
}

func TestCanvasUnknownAndCrossSessionKeysRejected(t *testing.T) {
	c, ev, _, _ := newCanvasCoord(t, ModeBuild)
	if _, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: "not-a-key", Action: "list"}); err == nil {
		t.Fatal("unknown key accepted")
	}
	// A key minted in an earlier session must not work after the session changed.
	_, _, _, oldKey := newCanvasCoord(t, ModeBuild)
	c.mu.Lock()
	c.peerKeys[oldKey] = peerIdentity{session: "ses_other", agent: "sender", runtime: "runtime-sender"}
	c.mu.Unlock()
	if _, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: oldKey, Action: "list"}); err == nil {
		t.Fatal("cross-session key accepted")
	}
	if n := len(ev.list()); n != 0 {
		t.Fatalf("rejected keys produced %d events", n)
	}
}

func TestCanvasRejectsKeyAfterRuntimeEnded(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModeBuild)
	c.runtimeMu.Lock()
	delete(c.runtimeSessions, "sender")
	c.runtimeMu.Unlock()
	if _, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: "list"}); err == nil {
		t.Fatal("key accepted after runtime ended")
	}
	if n := len(ev.list()); n != 0 {
		t.Fatalf("event emitted after runtime ended: %d", n)
	}
}

func TestCanvasTimeoutReleasesPendingSlot(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModeBuild)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.canvasRequest(ctx, agentmsg.CanvasRequest{Key: key, Action: "capture", ID: "a"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	c.canvasMu.Lock()
	left := len(c.canvasPending)
	c.canvasMu.Unlock()
	if left != 0 {
		t.Fatalf("pending slot leaked after timeout: %d", left)
	}
	// A late reply after timeout is rejected and never reaches the model.
	if err := c.CanvasReply(awaitEvent(t, ev), map[string]any{"late": true}, ""); err == nil {
		t.Fatal("late reply after timeout accepted")
	}
}

func TestCanvasCancelReleasesWaiter(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModeBuild)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(ctx, agentmsg.CanvasRequest{Key: key, Action: "inspect", ID: "a"})
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request returned success")
	}
	if err := c.CanvasReply(reqID, map[string]any{"x": 1}, ""); err == nil {
		t.Fatal("reply after cancel accepted")
	}
}

func TestCanvasStaleReplyRejectedAfterSessionChange(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModeBuild)
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: "list"})
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	// Start a new session: the old request must not complete with the new session's state.
	c.CreateSession(t.TempDir(), "other work", ModeBuild)
	if err := c.CanvasReply(reqID, []any{}, ""); err == nil {
		t.Fatal("stale reply accepted after session change")
	}
	if err := <-done; err == nil {
		t.Fatal("request completed after session change")
	}
}

func TestCanvasConcurrencyAndRateLimits(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModeBuild)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < maxCanvasConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.canvasRequest(ctx, agentmsg.CanvasRequest{Key: key, Action: "list"})
		}()
	}
	awaitN(t, ev, maxCanvasConcurrent)
	if _, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: "list"}); err == nil || !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("ninth concurrent request: err = %v", err)
	}
	cancel()
	wg.Wait()

	// Per-agent rate limit: a fresh coord with no pending requests, filled to the limit.
	c2, _, _, key2 := newCanvasCoord(t, ModeBuild)
	c2.canvasMu.Lock()
	c2.canvasRate["sender"] = make([]time.Time, maxCanvasPerMinute)
	for i := range c2.canvasRate["sender"] {
		c2.canvasRate["sender"][i] = time.Now()
	}
	c2.canvasMu.Unlock()
	if _, err := c2.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key2, Action: "list"}); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("rate limited request: err = %v", err)
	}
}

func awaitN(t *testing.T, ev *canvasEvents, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(ev.list()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d canvas events, want %d", len(ev.list()), n)
}

func TestCanvasDisabledCoordRejectsDirectCalls(t *testing.T) {
	c, ev, _, key := newCanvasCoord(t, ModeBuild)
	c.SetCanvasEnabled(false)
	if _, err := c.canvasRequest(context.Background(), agentmsg.CanvasRequest{Key: key, Action: "list"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("disabled canvas call: err = %v", err)
	}
	if len(ev.list()) != 0 {
		t.Fatal("disabled canvas emitted a request to the extension")
	}
}

func TestCanvasDisabledCoordRegistersNoTool(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	if c.canvasOn() {
		t.Fatal("canvas enabled without extension support")
	}
	c.SetCanvasEnabled(true)
	if !c.canvasOn() {
		t.Fatal("SetCanvasEnabled(true) not applied")
	}
	c.SetCanvasEnabled(false)
	if c.canvasOn() {
		t.Fatal("SetCanvasEnabled(false) not applied")
	}
}

// Authoring is refused for every role that is not a builder, and the error names the refused role.
func TestCanvasAuthoringDeniedForEveryRestrictedRole(t *testing.T) {
	for _, role := range []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Reviewer, agent.Role("designer"), agent.Role("")} {
		c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, role)
		for _, action := range []string{"create", "update"} {
			_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, action))
			want := "canvas " + action + " is not available to the " + string(role) + " role"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("role %q %s: err = %v, want %q", role, action, err, want)
			}
		}
		if n := len(ev.list()); n != 0 {
			t.Fatalf("role %q: denied writes emitted %d canvas events", role, n)
		}
	}
}

// Every builder role may create an artifact: the request reaches the extension and the answer comes back.
func TestCanvasAuthoringAllowedForEveryBuilderRole(t *testing.T) {
	for _, role := range []agent.Role{agent.Worker, agent.Architect, agent.Debug, agent.Test, agent.Integration} {
		c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, role)
		done := make(chan error, 1)
		go func() {
			_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
			done <- err
		}()
		reqID := awaitEvent(t, ev)
		if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err != nil {
			t.Fatalf("builder %q: CanvasReply: %v", role, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("builder %q create: %v", role, err)
		}
	}
}

// A reply is refused when the owner changed to a restricted role while the request was pending.
func TestCanvasReplyRejectsRoleChangeToRestrictedRoles(t *testing.T) {
	for _, role := range []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Reviewer} {
		c, ev, sender, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
		done := make(chan error, 1)
		go func() {
			_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
			done <- err
		}()
		reqID := awaitEvent(t, ev)
		c.Agents.Update(sender.ID, func(x *agent.Agent) { x.Role = role })
		if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err == nil {
			t.Fatalf("reply after change to %q was accepted", role)
		}
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "changed role") {
				t.Fatalf("change to %q: err = %v", role, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("change to %q: the agent was not told that the reply was refused", role)
		}
	}
}

// The re-entry check runs the action policy too: a pending create whose owner may not create is refused even when
// the role itself did not change. The waiting agent receives the refusal as an error, never as a result.
func TestCanvasReplyReentryRejectsDisallowedAction(t *testing.T) {
	c, _, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Primary)
	c.mu.Lock()
	identity := c.peerKeys[key]
	c.mu.Unlock()
	call := &canvasCall{
		id:       "canvas_forged",
		session:  identity.session,
		agent:    identity.agent,
		role:     agent.Primary,
		action:   "create",
		identity: identity,
		reply:    make(chan canvasReply, 1),
	}
	c.canvasMu.Lock()
	c.canvasPending[call.id] = call
	c.canvasMu.Unlock()
	if err := c.CanvasReply(call.id, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err == nil || !strings.Contains(err.Error(), "may no longer create") {
		t.Fatalf("reply for a disallowed action: err = %v", err)
	}
	select {
	case r := <-call.reply:
		if r.err == "" || r.result != nil {
			t.Fatalf("waiting agent got %+v, want a refusal only", r)
		}
	default:
		t.Fatal("the waiting agent was not told that the reply was refused")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
