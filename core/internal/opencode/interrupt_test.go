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

	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

// Relo gets an agent out of a hung tool call by interrupting the session and sending it another task. That only
// works if OpenCode says nothing while the call hangs (so Relo has to watch the clock), confirms the interrupt with
// an event that is not a terminal one, and takes a new task on the same session afterwards. This checks all three
// against the real opencode, with a fake model that asks for a command that never ends. It runs only when
// RELO_E2E_OPENCODE=1.
func TestAnInterruptedSessionTakesAnotherTask(t *testing.T) {
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(v map[string]any) { j, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", j) }
		choice := func(delta map[string]any, finish any) []any {
			return []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
		}
		usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		if n == 1 {
			call := map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "shell", "arguments": `{"command":"sleep 600","timeout":0,"description":"hang"}`}}
			chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake", "choices": choice(map[string]any{"role": "assistant", "tool_calls": []any{call}}, nil)})
			chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake", "choices": choice(map[string]any{}, "tool_calls"), "usage": usage})
		} else {
			chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake", "choices": choice(map[string]any{"role": "assistant", "content": `{"status":"completed","summary":"resumed"}`}, nil)})
			chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake", "choices": choice(map[string]any{}, "stop"), "usage": usage})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	work := t.TempDir()
	ps := []provider.Provider{{ID: "fake", Name: "fake", Kind: provider.KindOpenAI, BaseURL: srv.URL + "/v1", Models: []provider.Model{{ID: "claude-fake", Context: 200000, Output: 8000}}}}
	penv, err := provider.Env(ps, map[string]string{"fake": "k"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	proc, err := Start(ctx, bin, work, append(Isolation(t.TempDir()), penv...))
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Stop()
	rt := proc.Runtime()
	sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: "w", Directory: work})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.CloseSession(context.Background(), sid) }()
	evs, err := rt.Events(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	task := runtime.AgentTask{AgentID: "a", Role: "worker", Model: "claude-fake", Provider: "fake", System: "SYSTEM", Prompt: "run it", Tools: runtime.ToolsFor("worker", "build", true), Directory: work}
	if err := rt.SendTask(ctx, sid, task); err != nil {
		t.Fatal(err)
	}

	// The command is running: the stream must go quiet without ending, and the call must be one Relo can see.
	sawCall := false
	quiet := time.After(4 * time.Second)
wait:
	for {
		select {
		case ev := <-evs:
			if ev.Tool == "shell" {
				sawCall = true
			}
			if ev.Done {
				t.Fatalf("the turn ended while its command was still running: %+v", ev)
			}
		case <-quiet:
			break wait
		}
	}
	if !sawCall {
		t.Fatal("no event named the shell call, so a stuck call could not be told from a stuck model")
	}

	if err := rt.Cancel(ctx, sid); err != nil {
		t.Fatal(err)
	}
	confirmed, failedCall := false, false
	deadline := time.After(15 * time.Second)
	for !confirmed {
		select {
		case ev := <-evs:
			failedCall = failedCall || (ev.ToolDone && ev.ToolID == "call_1")
			if ev.Done {
				t.Fatalf("an interrupt produced a terminal event, which would end the turn that resumes the agent: %+v", ev)
			}
			confirmed = ev.Interrupted
		case <-deadline:
			t.Fatal("the interrupt was never confirmed")
		}
	}
	if !failedCall {
		t.Error("the interrupted call was never reported as finished, so Relo would still think it was running")
	}

	task.Prompt = "carry on"
	if err := rt.SendTask(ctx, sid, task); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(30 * time.Second)
	for {
		select {
		case ev := <-evs:
			if ev.Done {
				if ev.Error != "" || !strings.Contains(ev.JSON, "resumed") {
					t.Fatalf("the resumed turn ended badly: %+v", ev)
				}
				return
			}
		case <-deadline:
			t.Fatal("the resumed turn never finished")
		}
	}
}
