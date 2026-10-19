package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

// Peer messaging against the real opencode and a fake model that needs no credentials. Three things must hold:
// the relo_agents MCP tool is offered to the model for Primary (whose wildcard deny would otherwise hide it) and to
// a worker; a call to it reaches Relo's handler with the arguments the model sent; and a peer message delivered
// while a worker's turn is running reaches the model on its next request, without interrupting or restarting the
// turn. It runs only when RELO_E2E_OPENCODE=1.

const (
	msgToolName     = "relo_agents"
	primaryKey      = "KEY-MARKER-primary"
	coordinationTxt = "COORDINATION-MARKER-7f3a"
	steerMarker     = "STEER-MARKER-9c1d"
	turnWait        = 30 * time.Second
)

type agentRequest struct {
	body  string
	tools []string
}

// fakeChat answers OpenAI-style chat completions. OpenCode also sends title-generation requests, which carry no
// tools and no turn prompt; they get a fixed reply and are not counted. Every other request is an agent call: it is
// recorded, announced on started, and answered by reply(n), with n counting agent calls from 1. An agent call that
// was offered no tools is answered NO-TOOLS, so the missing tool shows up as a failed turn rather than a timeout.
type fakeChat struct {
	marker  string
	reply   func(n int, w http.ResponseWriter)
	started chan agentRequest

	mu     sync.Mutex
	agents []agentRequest
	titles int
}

func newFakeChat(marker string, reply func(n int, w http.ResponseWriter)) *fakeChat {
	return &fakeChat{marker: marker, reply: reply, started: make(chan agentRequest, 16)}
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(b, &req)
	body := string(b)
	names := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		names = append(names, tl.Function.Name)
	}
	if len(names) == 0 && !strings.Contains(body, f.marker) {
		f.mu.Lock()
		f.titles++
		f.mu.Unlock()
		streamChunks(w, map[string]any{"role": "assistant", "content": "Title"}, "stop")
		return
	}
	f.mu.Lock()
	f.agents = append(f.agents, agentRequest{body: body, tools: names})
	n := len(f.agents)
	f.mu.Unlock()
	f.started <- agentRequest{body: body, tools: names}
	if len(names) == 0 {
		streamChunks(w, map[string]any{"role": "assistant", "content": "NO-TOOLS"}, "stop")
		return
	}
	f.reply(n, w)
}

func (f *fakeChat) agentRequests() []agentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentRequest(nil), f.agents...)
}

func (f *fakeChat) agentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.agents)
}

// describe summarises the requests the model received, for failure messages.
func (f *fakeChat) describe() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []string
	for i, a := range f.agents {
		parts = append(parts, fmt.Sprintf("agent#%d tools=%v", i+1, a.tools))
	}
	return fmt.Sprintf("titles=%d %s", f.titles, strings.Join(parts, " "))
}

// streamChunks writes one assistant delta, a final chunk with usage and the SSE terminator.
func streamChunks(w http.ResponseWriter, delta map[string]any, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(d map[string]any, fin any, usage bool) {
		c := map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake",
			"choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": fin}}}
		if usage {
			c["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}
		j, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", j)
	}
	chunk(delta, nil, false)
	chunk(map[string]any{}, finish, true)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func toolCallDelta(id, name, args string) map[string]any {
	call := map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
	return map[string]any{"role": "assistant", "tool_calls": []any{call}}
}

func hasTool(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

// eventLog records what the runtime reports for a session while the test runs.
type eventLog struct {
	mu          sync.Mutex
	tools       []string
	interrupted bool
	doneCount   int
	seen        []string
	done        chan runtime.Event
	model       *fakeChat
}

// watchEvents consumes the session's events in the background. A done event ends the turn only when it carries
// the turn's output or an error, or when it follows an agent call: OpenCode also finishes the title request on the
// same session, and that one has neither.
func watchEvents(evs <-chan runtime.Event, model *fakeChat) *eventLog {
	l := &eventLog{done: make(chan runtime.Event, 1), model: model}
	go func() {
		for ev := range evs {
			l.mu.Lock()
			l.seen = append(l.seen, fmt.Sprintf("type=%s tool=%q done=%v json=%q err=%q", ev.Type, ev.Tool, ev.Done, ev.JSON, ev.Error))
			if ev.Tool != "" {
				l.tools = append(l.tools, ev.Tool)
			}
			if ev.Interrupted {
				l.interrupted = true
			}
			turnEnd := ev.Done && (ev.JSON != "" || ev.Error != "" || model.agentCount() > 0)
			if turnEnd {
				l.doneCount++
			}
			l.mu.Unlock()
			if turnEnd {
				select {
				case l.done <- ev:
				default:
				}
			}
		}
	}()
	return l
}

func (l *eventLog) waitDone(t *testing.T, d time.Duration) runtime.Event {
	t.Helper()
	select {
	case ev := <-l.done:
		return ev
	case <-time.After(d):
		l.mu.Lock()
		events := strings.Join(l.seen, " | ")
		l.mu.Unlock()
		t.Fatalf("the turn never finished within %v; model: %s; events: %s", d, l.model.describe(), events)
	}
	return runtime.Event{}
}

func (l *eventLog) snapshot() (tools []string, interrupted bool, doneCount int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.tools...), l.interrupted, l.doneCount
}

// startMessagingE2E starts the real opencode against the fake model, with a fresh working directory and isolated
// state. Cleanups run in reverse order, so the process stops before the model server closes.
func startMessagingE2E(t *testing.T, model http.Handler) (context.Context, runtime.AgentRuntime, string) {
	t.Helper()
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)
	work := t.TempDir()
	ps := []provider.Provider{{ID: "fake", Name: "fake", Kind: provider.KindOpenAI, BaseURL: srv.URL + "/v1", Models: []provider.Model{{ID: "claude-fake", Context: 200000, Output: 8000}}}}
	penv, err := provider.Env(ps, map[string]string{"fake": "k"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	proc, err := Start(ctx, bin, work, append(Isolation(t.TempDir()), penv...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Stop() })
	return ctx, proc.Runtime(), work
}

// startHub runs Relo's MCP endpoint with the given handler and registers it with the runtime for dir.
func startHub(ctx context.Context, t *testing.T, rt runtime.AgentRuntime, dir string, handler agentmsg.Handler) *agentmsg.Server {
	t.Helper()
	hub, err := agentmsg.Start(handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	mr, ok := rt.(runtime.MessageRuntime)
	if !ok {
		t.Fatal("the opencode runtime does not implement runtime.MessageRuntime")
	}
	if err := mr.ConfigureMessages(ctx, dir, runtime.MessageTools{URL: hub.URL, Secret: hub.Secret}); err != nil {
		t.Fatalf("ConfigureMessages: %v", err)
	}
	return hub
}

func TestPrimaryCallsReloAgentsThroughTheHub(t *testing.T) {
	model := newFakeChat("PRIMARY-PROMPT", func(n int, w http.ResponseWriter) {
		if n == 1 {
			args := fmt.Sprintf(`{"key":%q,"action":"send","to":"worker-7","text":%q}`, primaryKey, coordinationTxt)
			streamChunks(w, toolCallDelta("call_relo", msgToolName, args), "tool_calls")
			return
		}
		streamChunks(w, map[string]any{"role": "assistant", "content": `{"status":"completed","summary":"sent"}`}, "stop")
	})
	ctx, rt, work := startMessagingE2E(t, model)
	calls := make(chan agentmsg.Request, 4)
	startHub(ctx, t, rt, work, func(_ context.Context, r agentmsg.Request) (any, error) {
		calls <- r
		return map[string]any{"delivered": true}, nil
	})

	sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: "primary", Directory: work, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.CloseSession(context.Background(), sid) }()
	evs, err := rt.Events(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	log := watchEvents(evs, model)
	task := runtime.AgentTask{AgentID: "p", Role: runtime.RolePrimary, Model: "claude-fake", Provider: "fake", System: "SYSTEM", Prompt: "PRIMARY-PROMPT", Tools: runtime.ToolsFor(runtime.RolePrimary, "build", false), Directory: work}
	if err := rt.SendTask(ctx, sid, task); err != nil {
		t.Fatal(err)
	}

	var first agentRequest
	select {
	case first = <-model.started:
	case <-time.After(turnWait):
		t.Fatalf("primary never made a model call; model: %s", model.describe())
	}
	t.Logf("primary tools offered to the model: %v", first.tools)
	if !hasTool(first.tools, msgToolName) {
		t.Errorf("primary's first model call was not offered %s despite the wildcard deny: tools = %v", msgToolName, first.tools)
	}
	for _, denied := range []string{"shell", "bash", "read", "write", "edit", "webfetch", "glob", "grep"} {
		if hasTool(first.tools, denied) {
			t.Errorf("primary was offered %s, which Primary must not have: tools = %v", denied, first.tools)
		}
	}

	ev := log.waitDone(t, turnWait)
	if ev.Error != "" || !strings.Contains(ev.JSON, "sent") {
		t.Fatalf("the primary turn ended badly (a NO-TOOLS reply means relo_agents was missing): %+v; model: %s", ev, model.describe())
	}
	turns := model.agentRequests()
	if len(turns) != 2 {
		t.Fatalf("model made %d agent calls, want 2 (the relo_agents call, then the completion): %s", len(turns), model.describe())
	}
	tools, _, _ := log.snapshot()
	t.Logf("tool events reported by the runtime: %v", tools)
	if !hasTool(tools, msgToolName) {
		t.Errorf("the runtime never reported the %s call: %v", msgToolName, tools)
	}

	select {
	case got := <-calls:
		want := agentmsg.Request{Key: primaryKey, Action: "send", To: "worker-7", Text: coordinationTxt}
		t.Logf("handler received: %+v", got)
		if got != want {
			t.Errorf("handler received %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relo_agents call never reached the handler")
	}
	if !strings.Contains(turns[1].body, "delivered") {
		t.Errorf("the handler's result did not go back to the model in its next request")
	}
}

func TestPeerMessageSteersActiveWorkerWithoutRestart(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }

	model := newFakeChat("WORKER-PROMPT", func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			streamChunks(w, toolCallDelta("call_1", "shell", `{"command":"echo one","timeout":0,"description":"first step"}`), "tool_calls")
		case 2:
			// The worker's first tool call has finished and this model call is pending: the turn is active.
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-time.After(turnWait):
			}
			streamChunks(w, toolCallDelta("call_2", "shell", `{"command":"echo two","timeout":0,"description":"second step"}`), "tool_calls")
		default:
			streamChunks(w, map[string]any{"role": "assistant", "content": `{"status":"completed","summary":"steered"}`}, "stop")
		}
	})
	ctx, rt, work := startMessagingE2E(t, model)
	// Registered after startMessagingE2E, so it runs before the process stops and the model server closes.
	t.Cleanup(releaseFn)
	startHub(ctx, t, rt, work, func(context.Context, agentmsg.Request) (any, error) {
		return map[string]any{"delivered": false}, nil
	})

	sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: "worker", Directory: work})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.CloseSession(context.Background(), sid) }()
	evs, err := rt.Events(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	log := watchEvents(evs, model)
	task := runtime.AgentTask{AgentID: "w", Role: "worker", Model: "claude-fake", Provider: "fake", System: "SYSTEM", Prompt: "WORKER-PROMPT", Tools: runtime.ToolsFor("worker", "build", true), Directory: work}
	if err := rt.SendTask(ctx, sid, task); err != nil {
		t.Fatal(err)
	}

	var first agentRequest
	select {
	case first = <-model.started:
	case <-time.After(turnWait):
		t.Fatalf("worker never made a model call; model: %s", model.describe())
	}
	if !hasTool(first.tools, msgToolName) || !hasTool(first.tools, "shell") {
		t.Errorf("worker's first model call tools = %v, want %s and shell", first.tools, msgToolName)
	}

	select {
	case <-entered:
	case <-time.After(turnWait):
		t.Fatalf("the worker never reached its second model call; model: %s", model.describe())
	}
	mr := rt.(runtime.MessageRuntime)
	if err := mr.DeliverMessage(ctx, sid, "peer-msg-1", "Message from peer: "+steerMarker); err != nil {
		t.Fatalf("DeliverMessage while the turn was active: %v", err)
	}
	releaseFn()

	ev := log.waitDone(t, turnWait)
	if ev.Error != "" || !strings.Contains(ev.JSON, "steered") {
		t.Fatalf("the steered turn ended badly: %+v; model: %s", ev, model.describe())
	}
	tools, interrupted, doneCount := log.snapshot()
	t.Logf("tool events: %v; turn ends: %d", tools, doneCount)
	if interrupted {
		t.Error("the turn was interrupted by the peer message")
	}
	if doneCount != 1 {
		t.Errorf("turn ended %d times, want exactly 1", doneCount)
	}

	turns := model.agentRequests()
	if len(turns) != 3 {
		t.Fatalf("worker made %d agent model calls, want 3: a steer must not start another run: %s", len(turns), model.describe())
	}
	if strings.Contains(turns[1].body, steerMarker) {
		t.Error("the peer message appeared in a request sent before it was delivered")
	}
	if !strings.Contains(turns[2].body, steerMarker) {
		t.Error("the peer message was not in the model's next request after delivery")
	}
}

func TestPrimaryCallsCanvasWithNativeToolNameAndReceivesScreenshot(t *testing.T) {
	model := newFakeChat("CANVAS-PROMPT", func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamChunks(w, toolCallDelta("call_canvas", runtime.ReloCanvasTool, `{"key":"canvas-key","action":"capture","id":"inventory"}`), "tool_calls")
			return
		}
		streamChunks(w, map[string]any{"role": "assistant", "content": `{"status":"completed","summary":"canvas captured"}`}, "stop")
	})
	ctx, rt, work := startMessagingE2E(t, model)
	hub, err := agentmsg.Start(func(context.Context, agentmsg.Request) (any, error) { return nil, nil }, agentmsg.WithCanvas(func(_ context.Context, request agentmsg.CanvasRequest) (any, error) {
		return map[string]any{"artifact": map[string]any{"id": request.ID}, "capture": map[string]any{"mime": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aWQAAAABJRU5ErkJggg==", "visualInspectionAvailable": false}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: "canvas", Directory: work, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.CloseSession(context.Background(), sid) }()
	if err := rt.(runtime.MessageRuntime).ConfigureMessages(ctx, work, runtime.MessageTools{URL: hub.URL, Secret: hub.Secret}); err != nil {
		t.Fatal(err)
	}
	events, err := rt.Events(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	log := watchEvents(events, model)
	if err := rt.SendTask(ctx, sid, runtime.AgentTask{Role: runtime.RolePrimary, Model: "claude-fake", Provider: "fake", Prompt: "CANVAS-PROMPT", Tools: runtime.WithCanvas(runtime.ToolsFor(runtime.RolePrimary, "build", false), true), Directory: work}); err != nil {
		t.Fatal(err)
	}
	if result := log.waitDone(t, turnWait); result.Error != "" || !strings.Contains(result.JSON, "captured") {
		t.Fatalf("Canvas turn: %+v", result)
	}
	requests := model.agentRequests()
	if len(requests) != 2 || !hasTool(requests[0].tools, runtime.ReloCanvasTool) || hasTool(requests[0].tools, "shell") {
		t.Fatalf("Canvas tool requests: %s", model.describe())
	}
	if !strings.Contains(requests[1].body, "image/png") {
		t.Fatalf("capture image did not reach model: %.500s", requests[1].body)
	}
}
