package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/runtime"
)

// fakeRuntime is a Runtime on the fake CLI (fake_test.go), with its log file.
func fakeRuntime(t *testing.T, mode string) (*Runtime, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "fake.log")
	t.Setenv("RELO_FAKE_CLAUDE", mode)
	t.Setenv("RELO_FAKE_CLAUDE_LOG", log)
	rt, err := New(context.Background(), os.Args[0], t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt, log
}

func readLog(t *testing.T, path string) []fakeLog {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []fakeLog
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e fakeLog
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// startArgs is the command line of the stream-mode process the session started (the probe's comes first).
func startArgs(t *testing.T, path string) fakeLog {
	t.Helper()
	var last fakeLog
	for _, e := range readLog(t, path) {
		if len(e.Args) > 0 && strings.Contains(strings.Join(e.Args, " "), "--system-prompt-file") {
			last = e
		}
	}
	if last.Args == nil {
		t.Fatal("no session process was started")
	}
	return last
}

func flag(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// collect reads events until one ends the turn (Done, or an interrupt confirmation).
func collect(t *testing.T, evs <-chan runtime.Event, on func(runtime.Event)) []runtime.Event {
	t.Helper()
	var got []runtime.Event
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				t.Fatal("event stream closed before the turn ended")
			}
			got = append(got, ev)
			if on != nil {
				on(ev)
			}
			if ev.Done || ev.Interrupted {
				return got
			}
		case <-timeout:
			t.Fatalf("the turn did not end; events so far: %+v", got)
		}
	}
}

func start(t *testing.T, rt *Runtime, cfg runtime.SessionConfig) (runtime.SessionID, <-chan runtime.Event) {
	t.Helper()
	sid, err := rt.StartSession(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(sid), SessionPrefix) {
		t.Fatalf("session id %q lacks the prefix the Mux routes by", sid)
	}
	evs, err := rt.Events(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return sid, evs
}

func task(prompt string) runtime.AgentTask {
	return runtime.AgentTask{AgentID: "a1", Role: "worker", Provider: ProviderID, Model: "claude-sonnet-5-5", Effort: "high",
		System: "SYSTEM-MARKER: you are a Relo worker.", Prompt: prompt, Tools: runtime.ToolsFor("worker", "build", true)}
}

func TestProbeListsEachModelOnceUnderItsOwnName(t *testing.T) {
	rt, _ := fakeRuntime(t, "ok")
	ms, _ := rt.Models(context.Background())
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID+"="+m.Name)
		if m.Provider != ProviderID || m.Vision != "yes" || len(m.Efforts) == 0 {
			t.Errorf("model %+v: want provider %s, vision yes and its effort levels", m, ProviderID)
		}
	}
	if got := strings.Join(ids, ","); got != "claude-sonnet-5-5=Sonnet 5.5,claude-opus-5-5=Opus 5.5,claude-haiku-5-5=Haiku 5.5" {
		t.Fatalf("models = %s", got)
	}
	if info := rt.Info(); info.Plan != "Claude Max" || info.Version != "2.1.304" {
		t.Fatalf("info = %+v", info)
	}
}

func TestASignedOutCLIIsNotOffered(t *testing.T) {
	t.Setenv("RELO_FAKE_CLAUDE", "signed-out")
	if _, err := New(context.Background(), os.Args[0], nil); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("err = %v, want a not-signed-in error", err)
	}
}

// A turn streams its reply, reports each model call's prompt size, and ends with the reply and the turn's usage.
// The process gets Relo's prompt in place of Claude Code's, the agent's tools, its model and effort, and runs in the
// agent's directory.
func TestATurnRunsWithReloPromptToolsAndModel(t *testing.T) {
	rt, log := fakeRuntime(t, "ok")
	dir := t.TempDir()
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: dir})
	if err := rt.SendTask(context.Background(), sid, task("plain please")); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	var stepEnd runtime.Event
	got := collect(t, evs, func(ev runtime.Event) {
		if ev.Phase == "writing" {
			body.WriteString(ev.Delta)
		}
		if ev.Type == "session.step.ended" {
			stepEnd = ev
		}
		if ev.Session != string(sid) {
			t.Errorf("event %s carries session %q", ev.Type, ev.Session)
		}
	})
	done := got[len(got)-1]
	if !done.Done || done.Error != "" || done.Text != `{"status":"completed","summary":"ok"}` {
		t.Fatalf("turn end = %+v", done)
	}
	if body.String() != done.Text {
		t.Fatalf("streamed reply %q, want %q", body.String(), done.Text)
	}
	if stepEnd.ContextTokens != 1203 || stepEnd.MoreSteps {
		t.Fatalf("step end = %+v, want 1203 prompt tokens and no more steps", stepEnd)
	}
	if u := done.Usage; u == nil || u.Input != 3 || u.CacheRead != 1000 || u.CacheWrite != 200 || u.Output != 30 || u.Reasoning != 20 || u.Context != 1203 {
		t.Fatalf("usage = %+v, want output 30 and reasoning 20 out of 50, context 1203", done.Usage)
	}
	e := startArgs(t, log)
	if e.Dir != dir {
		t.Errorf("ran in %s, want %s", e.Dir, dir)
	}
	if e.System != "SYSTEM-MARKER: you are a Relo worker." {
		t.Errorf("system prompt = %q", e.System)
	}
	want := map[string]string{"--model": "claude-sonnet-5-5", "--effort": "high", "--tools": "Bash,Edit,Glob,Grep,Read,Write",
		"--allowedTools": "Edit,Glob,Grep,Read,Write,mcp__relo__agents", "--disallowedTools": "mcp__relo__art,mcp__relo__canvas,mcp__relo__image,mcp__relo__sprite,mcp__relo__web",
		"--input-format": "stream-json", "--permission-prompt-tool": "stdio"}
	for name, value := range want {
		if got, _ := flag(e.Args, name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	for _, name := range []string{"--safe-mode", "--strict-mcp-config", "--no-session-persistence", "--include-partial-messages"} {
		if !strings.Contains(strings.Join(e.Args, " "), name) {
			t.Errorf("missing %s in %v", name, e.Args)
		}
	}
	if _, ok := flag(e.Args, "--mcp-config"); ok {
		t.Error("an MCP config was passed with no messaging endpoint configured")
	}
}

func TestAReadOnlySessionGetsNoToolThatChangesAnything(t *testing.T) {
	rt, log := fakeRuntime(t, "ok")
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: t.TempDir(), ReadOnly: true})
	if err := rt.SendTask(context.Background(), sid, task("plain")); err != nil {
		t.Fatal(err)
	}
	collect(t, evs, nil)
	args := startArgs(t, log).Args
	if got, _ := flag(args, "--tools"); got != "Glob,Grep,Read" {
		t.Fatalf("--tools = %q, want only the read tools", got)
	}
	primary := runtime.ToolsFor(runtime.RolePrimary, "build", false)
	builtins, allowed, _ := toolFlags(primary, true)
	if len(builtins) != 0 || strings.Join(allowed, ",") != "mcp__relo__agents" {
		t.Fatalf("primary tools = %v allowed %v, want no built-in tool and only the hub", builtins, allowed)
	}
}

// A command that changes something is not pre-approved: the CLI asks, the question becomes Relo's permission event,
// and Relo's answer goes back. Each model call ends with its prompt size and whether tool calls follow.
func TestAPermissionRequestGoesThroughRelo(t *testing.T) {
	for _, reply := range []string{"once", "reject"} {
		t.Run(reply, func(t *testing.T) {
			rt, log := fakeRuntime(t, "ok")
			sid, evs := start(t, rt, runtime.SessionConfig{Directory: t.TempDir()})
			if err := rt.SendTask(context.Background(), sid, task("tool")); err != nil {
				t.Fatal(err)
			}
			var steps []runtime.Event
			var toolDone, toolInput bool
			got := collect(t, evs, func(ev runtime.Event) {
				if ev.Permission != nil {
					if ev.Permission["title"] != "bash" || ev.Permission["pattern"] != "go test ./..." || ev.Permission["tool"] != "shell" {
						t.Errorf("permission = %v", ev.Permission)
					}
					if err := rt.ReplyPermission(context.Background(), ev.Session, ev.Permission["id"].(string), reply); err != nil {
						t.Error(err)
					}
				}
				if ev.Type == "session.step.ended" {
					steps = append(steps, ev)
				}
				if ev.ToolID == "t1" && ev.ToolInput == "go test ./..." && ev.Tool == "shell" {
					toolInput = true
				}
				if ev.ToolDone && ev.ToolID == "t1" {
					toolDone = true
				}
			})
			want := map[string]string{"once": "allow", "reject": "deny"}[reply]
			if done := got[len(got)-1]; done.Text != "done after "+want {
				t.Fatalf("turn end = %+v", done)
			}
			var answered string
			for _, e := range readLog(t, log) {
				if e.Permission != "" {
					answered = e.Permission
				}
			}
			if answered != want {
				t.Fatalf("the CLI was told %q, want %q", answered, want)
			}
			if len(steps) != 2 || !steps[0].MoreSteps || steps[0].ContextTokens != 5002 || steps[1].MoreSteps || steps[1].ContextTokens != 5122 {
				t.Fatalf("steps = %+v", steps)
			}
			if !toolInput || !toolDone {
				t.Fatalf("tool call seen with its command %t, finished %t", toolInput, toolDone)
			}
		})
	}
}

// Cancel interrupts the running call; the session reports it as interrupted, not finished, and takes the next task
// with its conversation intact. That is how Relo recovers an agent stuck on a silent model call.
func TestCancelInterruptsAndTheSessionCarriesOn(t *testing.T) {
	rt, _ := fakeRuntime(t, "ok")
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: t.TempDir()})
	if err := rt.SendTask(context.Background(), sid, task("hang")); err != nil {
		t.Fatal(err)
	}
	got := collect(t, evs, func(ev runtime.Event) {
		if ev.Type == "session.step.started" {
			if err := rt.Cancel(context.Background(), sid); err != nil {
				t.Error(err)
			}
		}
	})
	if end := got[len(got)-1]; !end.Interrupted || end.Done {
		t.Fatalf("after cancel = %+v, want an interruption that is not terminal", end)
	}
	if err := rt.SendTask(context.Background(), sid, task("plain")); err != nil {
		t.Fatal(err)
	}
	if end := collect(t, evs, nil); !end[len(end)-1].Done || end[len(end)-1].Error != "" {
		t.Fatalf("next turn = %+v", end[len(end)-1])
	}
	if err := rt.Cancel(context.Background(), sid); err != nil {
		t.Fatalf("cancel with no turn running: %v", err)
	}
}

func TestACrashEndsTheTurnWithTheCLIsError(t *testing.T) {
	rt, _ := fakeRuntime(t, "ok")
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: t.TempDir()})
	if err := rt.SendTask(context.Background(), sid, task("crash")); err != nil {
		t.Fatal(err)
	}
	got := collect(t, evs, nil)
	if end := got[len(got)-1]; !end.Done || !strings.Contains(end.Error, "boom: the fake CLI fell over") {
		t.Fatalf("turn end = %+v", end)
	}
	if err := rt.SendTask(context.Background(), sid, task("plain")); err == nil {
		t.Fatal("a session whose process died took another task; its conversation is gone")
	}
}

// A peer message reaches the running turn at its next step, the way OpenCode's steering message does. With no turn
// running it is refused, and Relo carries it in the next task instead.
func TestAPeerMessageReachesTheRunningTurn(t *testing.T) {
	rt, _ := fakeRuntime(t, "ok")
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: t.TempDir()})
	if err := rt.DeliverMessage(context.Background(), sid, "p0", "too early"); err == nil {
		t.Fatal("a message was delivered with no turn running")
	}
	if err := rt.SendTask(context.Background(), sid, task("steer")); err != nil {
		t.Fatal(err)
	}
	got := collect(t, evs, func(ev runtime.Event) {
		if ev.Type == "session.step.ended" && ev.MoreSteps {
			if err := rt.DeliverMessage(context.Background(), sid, "p1", "peer says the API is v2"); err != nil {
				t.Error(err)
			}
		}
	})
	if end := got[len(got)-1]; end.Text != "got: peer says the API is v2" {
		t.Fatalf("turn end = %+v", end)
	}
}

// The hub's secret never reaches the CLI: the config file names an SDK-hosted server, only this user can read it, and
// it is gone once the session closes.
func TestTheMessagingSecretStaysOutOfTheCommandLine(t *testing.T) {
	rt, log := fakeRuntime(t, "ok")
	dir := t.TempDir()
	if err := rt.ConfigureMessages(context.Background(), dir, runtime.MessageTools{URL: "http://127.0.0.1:9/mcp", Secret: "s3cret-token"}); err != nil {
		t.Fatal(err)
	}
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: dir})
	if err := rt.SendTask(context.Background(), sid, task("plain")); err != nil {
		t.Fatal(err)
	}
	collect(t, evs, nil)
	e := startArgs(t, log)
	if strings.Contains(strings.Join(e.Args, " "), "s3cret-token") {
		t.Fatal("the secret is on the command line")
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(e.MCP), &cfg); err != nil {
		t.Fatalf("mcp config %q: %v", e.MCP, err)
	}
	// Safe mode drops every MCP server in --mcp-config but SDK-hosted ones, so the hub must be one: an "http" entry
	// left canvas agents without relo_canvas. The hub's address and secret stay in Relo.
	if s := cfg.MCPServers["relo"]; s.Type != "sdk" || s.Name != "relo" || strings.Contains(e.MCP, "s3cret-token") || strings.Contains(e.MCP, "127.0.0.1") {
		t.Fatalf("mcp config = %q", e.MCP)
	}
	if e.MCPMode != "-rw-------" {
		t.Fatalf("mcp config file mode %s, want -rw-------", e.MCPMode)
	}
	file, _ := flag(e.Args, "--mcp-config")
	if err := rt.CloseSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the mcp config outlived the session: %v", err)
	}
	if _, ok := <-evs; ok {
		t.Fatal("the event stream is still open after CloseSession")
	}
}

func TestEnvDropsTheParentSessionsLinks(t *testing.T) {
	got := Env([]string{"PATH=/bin", "CLAUDE_CODE_SESSION_ID=x", "CLAUDE_CODE_MESSAGING_TOKEN=y", "CLAUDE_CODE_USE_BEDROCK=1", "HOME=/h"})
	if strings.Join(got, " ") != "PATH=/bin CLAUDE_CODE_USE_BEDROCK=1 HOME=/h" {
		t.Fatalf("env = %v", got)
	}
}

func TestLookPrefersTheNewestBundledCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	for _, v := range []string{"2.1.99", "2.1.304", "2.0.500"} {
		bin := filepath.Join(home, ".vscode-oss", "extensions", "anthropic.claude-code-"+v+"-linux-x64", "resources", "native-binary", "claude")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Look("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "claude-code-2.1.304-") {
		t.Fatalf("Look = %s, want the 2.1.304 copy", got)
	}
	if _, err := Look(filepath.Join(home, "missing")); err == nil {
		t.Fatal("a configured path that does not exist was accepted")
	}
}

// The translator works on lines recorded from claude 2.1.304.
func TestTranslatorOnRecordedLines(t *testing.T) {
	var tr translator
	clock := time.Unix(0, 0)
	tr.now = func() time.Time { return clock }
	feed := func(line string) []runtime.Event {
		evs, end := tr.feed([]byte(line))
		if end != nil {
			t.Fatalf("unexpected turn end on %s", line)
		}
		return evs
	}
	feed(`{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-haiku-5-5","id":"msg_1","usage":{"input_tokens":2,"cache_creation_input_tokens":3119,"cache_read_input_tokens":0}}}}`)
	if evs := feed(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{},"caller":{"type":"direct"}}}}`); len(evs) != 1 || evs[0].Tool != "shell" || evs[0].ToolID != "toolu_1" {
		t.Fatalf("tool start = %+v", evs)
	}
	delta := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"co"}}}`
	if len(feed(delta)) != 1 || len(feed(delta)) != 0 {
		t.Fatal("streaming tool arguments should give one progress event per half second")
	}
	clock = clock.Add(time.Second)
	if len(feed(delta)) != 1 {
		t.Fatal("no progress event after half a second")
	}
	evs := feed(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":2,"cache_creation_input_tokens":3119,"cache_read_input_tokens":0,"output_tokens":85,"output_tokens_details":{"thinking_tokens":0}}}}`)
	if len(evs) != 1 || evs[0].Type != "session.step.ended" || evs[0].ContextTokens != 3121 || !evs[0].MoreSteps || evs[0].Usage.Output != 85 {
		t.Fatalf("step end = %+v", evs)
	}
	if evs := feed(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"PERM-OK","is_error":false}]},"tool_use_result":{"stdout":"PERM-OK"}}`); len(evs) != 1 || !evs[0].ToolDone {
		t.Fatalf("tool result = %+v", evs)
	}
	if evs := feed(`{"type":"user","message":{"role":"user","content":"a plain prompt echoed back"}}`); len(evs) != 0 {
		t.Fatalf("a plain user message became %+v", evs)
	}
	_, end := tr.feed([]byte(`{"type":"result","subtype":"error_during_execution","duration_ms":6012,"is_error":true,"num_turns":3,"stop_reason":"tool_use","terminal_reason":"aborted_tools","result":null,"errors":["[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=tool_use"],"usage":{"input_tokens":4,"output_tokens":99}}`))
	if end == nil || !end.aborted || end.err == "" || strings.Contains(end.err, "ede_diagnostic") || end.usage.Context != 3121 {
		t.Fatalf("interrupted result = %+v", end)
	}
	_, end = tr.feed([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":null,"usage":{}}`))
	if end == nil || end.aborted || end.err != "Claude Code ended the turn: error max turns" {
		t.Fatalf("max-turns result = %+v", end)
	}
}

// Safe mode keeps no MCP server from --mcp-config but SDK-hosted ones, so the hub is one and Relo relays the CLI's
// JSON-RPC to it with the secret. A request gets the hub's reply, a notification an empty result, and a hub that
// refuses the call becomes a JSON-RPC error rather than a stall.
func TestTheHubIsRelayedForTheCLI(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+" "+string(body))
		mu.Unlock()
		switch {
		case strings.Contains(string(body), `"tools/list"`):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"canvas"}]}}`))
		case !strings.Contains(string(body), `"id"`):
			w.WriteHeader(http.StatusAccepted)
		default:
			http.Error(w, "nope", http.StatusForbidden)
		}
	}))
	defer hub.Close()
	rt, log := fakeRuntime(t, "ok")
	dir := t.TempDir()
	if err := rt.ConfigureMessages(context.Background(), dir, runtime.MessageTools{URL: hub.URL, Secret: "s3cret-token"}); err != nil {
		t.Fatal(err)
	}
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: dir})
	if err := rt.SendTask(context.Background(), sid, task("hub")); err != nil {
		t.Fatal(err)
	}
	got := collect(t, evs, nil)
	if end := got[len(got)-1]; end.Error != "" || end.Text != "hub done" {
		t.Fatalf("turn end = %+v", end)
	}
	var bridged []string
	for _, e := range readLog(t, log) {
		if e.Bridge != "" {
			bridged = append(bridged, e.Bridge)
		}
	}
	want := []string{
		`{"id":1,"jsonrpc":"2.0","result":{"tools":[{"name":"canvas"}]}}`,
		`{"jsonrpc":"2.0","result":{}}`,
		`{"error":{"code":-32603,"message":"Relo's agent hub refused the call (HTTP 403)"},"id":2,"jsonrpc":"2.0"}`,
	}
	if strings.Join(bridged, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bridged replies:\n%s\nwant:\n%s", strings.Join(bridged, "\n"), strings.Join(want, "\n"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || !strings.HasPrefix(seen[0], "Bearer s3cret-token ") {
		t.Fatalf("the hub saw %q, want three calls with the secret", seen)
	}
}

// callableTurn runs one turn on the fake CLI against a real agentmsg hub that serves the canvas tool, with the tool
// policy the orchestrator would send, and returns the tools the model could call, the hub's canvas calls, and the
// fake's log. The fake works the tool list out the way the real CLI does under --safe-mode (see fake_test.go).
func callableTurn(t *testing.T, tools map[string]bool, prompt string) (callable []string, canvasCalls []string, log []fakeLog) {
	t.Helper()
	var mu sync.Mutex
	hub, err := agentmsg.Start(func(context.Context, agentmsg.Request) (any, error) { return map[string]any{}, nil },
		agentmsg.WithCanvas(func(_ context.Context, req agentmsg.CanvasRequest) (any, error) {
			mu.Lock()
			canvasCalls = append(canvasCalls, req.Action)
			mu.Unlock()
			return map[string]any{"artifacts": []any{}}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	rt, path := fakeRuntime(t, "ok")
	dir := t.TempDir()
	if err := rt.ConfigureMessages(context.Background(), dir, runtime.MessageTools{URL: hub.URL, Secret: hub.Secret}); err != nil {
		t.Fatal(err)
	}
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: dir})
	turn := task(prompt)
	turn.Tools = tools
	if err := rt.SendTask(context.Background(), sid, turn); err != nil {
		t.Fatal(err)
	}
	got := collect(t, evs, nil)
	end := got[len(got)-1]
	if end.Error != "" || json.Unmarshal([]byte(end.Text), &callable) != nil {
		t.Fatalf("turn end = %+v, want the list of callable tools", end)
	}
	mu.Lock()
	defer mu.Unlock()
	return callable, canvasCalls, readLog(t, path)
}

func has(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// A Claude Code worker whose prompt offers relo_canvas must be able to call it. Every earlier failure of this kind
// came from the process starting without the tool: the hub was an "http" MCP entry, which --safe-mode drops, so the
// model was told about relo_canvas and had no such tool (and no ToolSearch to look for it). The fake CLI applies
// the same rule, so a config the real CLI would drop leaves the tool out of the list here too.
func TestAClaudeCodeWorkerCanCallCanvasWhenThePromptOffersIt(t *testing.T) {
	tools := runtime.WithCanvas(runtime.ToolsFor("worker", "build", true), true, agent.CanvasActionsForRole(agent.Worker))
	callable, canvasCalls, log := callableTurn(t, tools, "callable canvas")
	if !has(callable, "mcp__relo__canvas") || !has(callable, "mcp__relo__agents") {
		t.Fatalf("callable tools = %v, want mcp__relo__canvas and mcp__relo__agents", callable)
	}
	if strings.Join(canvasCalls, ",") != "list" {
		t.Fatalf("the hub saw canvas calls %v, want one list call relayed from the CLI", canvasCalls)
	}
	// The hub is hosted twice over, as the SDKs do: named in initialize.sdkMcpServers and as an "sdk" config entry.
	var named bool
	for _, e := range log {
		var m struct {
			Request struct {
				Subtype string   `json:"subtype"`
				Servers []string `json:"sdkMcpServers"`
			} `json:"request"`
		}
		if json.Unmarshal([]byte(e.Input), &m) == nil && m.Request.Subtype == "initialize" {
			named = strings.Join(m.Request.Servers, ",") == "relo"
		}
	}
	if !named {
		t.Fatalf("the initialize request does not name the relo SDK server; log: %+v", log)
	}
	var args []string
	for _, e := range log {
		if has(e.Args, "--system-prompt-file") {
			args = e.Args
		}
	}
	if allowed, _ := flag(args, "--allowedTools"); !strings.Contains(allowed, "mcp__relo__canvas") {
		t.Fatalf("--allowedTools = %q, want mcp__relo__canvas", allowed)
	}
	if denied, _ := flag(args, "--disallowedTools"); strings.Contains(denied, "mcp__relo__canvas") {
		t.Fatalf("--disallowedTools = %q denies the canvas tool the prompt offers", denied)
	}
}

// The tool policy still decides: a turn that does not offer canvas gets no canvas tool even though the hub serves it,
// and a read-only role gets the tool the policy gives it.
func TestAClaudeCodeWorkerGetsNoCanvasUnlessOffered(t *testing.T) {
	callable, _, _ := callableTurn(t, runtime.ToolsFor("worker", "build", true), "callable")
	if has(callable, "mcp__relo__canvas") {
		t.Fatalf("callable tools = %v: canvas was not offered", callable)
	}
	if !has(callable, "mcp__relo__agents") {
		t.Fatalf("callable tools = %v, want mcp__relo__agents", callable)
	}
	probe := runtime.WithCanvas(runtime.ToolsFor("probe", "plan", false), true, agent.CanvasActionsForRole(agent.Probe))
	callable, _, _ = callableTurn(t, probe, "callable")
	if !has(callable, "mcp__relo__canvas") {
		t.Fatalf("a read-only role offered canvas lost it: %v", callable)
	}
}
