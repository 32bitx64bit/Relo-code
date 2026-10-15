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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

// Everything OpenCode puts in a request is paid for again in every call of every agent. Left alone, it sent a
// 25 KB system prompt (its "Code Mode" catalog of tools) and ten tool descriptions to a worker that uses six tools.
// This runs the real OpenCode against a fake model endpoint and checks what actually goes out. It needs the
// opencode binary, so it only runs when RELO_E2E_OPENCODE=1.
func TestAgentRequestsCarryOnlyTheToolsTheyUse(t *testing.T) {
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			mu.Lock()
			bodies = append(bodies, b)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(v map[string]any) { j, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", j) }
		chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": `{"status":"completed","summary":"ok"}`}}}})
		chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// RELO_E2E_DIR points the agents at a real project, where OpenCode adds what it finds there (instruction files,
	// the directory); RELO_E2E_DUMP is a folder to write each role's request body to, for measuring.
	work, realDir := os.Getenv("RELO_E2E_DIR"), true
	if work == "" {
		work, realDir = t.TempDir(), false
	}
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

	for _, tc := range []struct {
		role     string
		mutating bool
		allowed  []string
	}{
		{"worker", true, []string{"edit", "glob", "grep", "read", "shell", "write"}},
		{"probe", false, []string{"glob", "grep", "read", "shell"}},
	} {
		mu.Lock()
		bodies = nil
		mu.Unlock()
		sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: tc.role, Directory: work, ReadOnly: !tc.mutating})
		if err != nil {
			t.Fatal(err)
		}
		evs, err := rt.Events(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		task := runtime.AgentTask{AgentID: "a", Role: tc.role, Model: "claude-fake", Provider: "fake", System: "SYSTEM", Prompt: "PROMPT-MARKER", Tools: runtime.ToolsFor(tc.role, "build", tc.mutating), Directory: work}
		if err := rt.SendTask(ctx, sid, task); err != nil {
			t.Fatal(err)
		}
		for ev := range evs {
			if ev.Done {
				break
			}
		}
		_ = rt.CloseSession(ctx, sid)
		mu.Lock()
		var main []byte
		for _, b := range bodies {
			if strings.Contains(string(b), "PROMPT-MARKER") {
				main = b
			}
		}
		mu.Unlock()
		if main == nil {
			t.Fatalf("%s: the model was never called", tc.role)
		}
		if dump := os.Getenv("RELO_E2E_DUMP"); dump != "" {
			if err := os.WriteFile(filepath.Join(dump, tc.role+".json"), main, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var req struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(main, &req); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tl := range req.Tools {
			got = append(got, tl.Function.Name)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(tc.allowed, ",") {
			t.Errorf("%s: tools sent to the model = %v, want %v", tc.role, got, tc.allowed)
		}
		if !realDir && len(main) > 12_000 {
			t.Errorf("%s: an empty request is %d bytes; OpenCode's own text should be a few KB", tc.role, len(main))
		}
	}
}
