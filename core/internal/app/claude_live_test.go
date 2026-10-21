package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/claudecode"
	"github.com/relocode/relo/internal/logging"
)

// TestLiveClaudeCodeRun runs one objective end to end with every role on Claude Code (Haiku): OpenCode starts as the
// base runtime, the Mux sends the agents' sessions to the CLI, and the agents get the hub's tools over MCP. Gated by
// RELO_E2E_CLAUDE=1; it needs opencode and a signed-in claude, and uses some of the plan.
func TestLiveClaudeCodeRun(t *testing.T) {
	if os.Getenv("RELO_E2E_CLAUDE") != "1" {
		t.Skip("set RELO_E2E_CLAUDE=1 to run against the installed Claude Code CLI")
	}
	const model = claudecode.ProviderID + "/claude-haiku-5-5"
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.txt"), []byte("live workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	var failures, warns, chat []string
	logw := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
			if strings.Contains(l, `"level":"warn"`) || strings.Contains(l, `"level":"error"`) {
				warns = append(warns, l)
			}
		}
		return len(p), nil
	})
	a, err := New(Config{Workspace: ws, Runtime: "opencode", LogLevel: "warn"}, logging.New(logw, logging.Warn))
	if err != nil {
		t.Fatal(err)
	}
	a.Coord.SetEmitter(func(name string, payload any) {
		mu.Lock()
		defer mu.Unlock()
		counts[name]++
		if name == "chat.message" {
			b, _ := json.Marshal(payload)
			chat = append(chat, string(b))
		}
		if name == "agent.failed" {
			b, _ := json.Marshal(payload)
			failures = append(failures, string(b))
		}
	})
	init, _ := json.Marshal(map[string]any{"primaryModel": model, "tier1Model": model, "tier2Model": model,
		"claudeCode": map[string]any{"enabled": true, "path": os.Getenv("RELO_CLAUDE_PATH")}, "logLevel": "warn"})
	res, err := a.Handle("initialize", init)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	backend, _ := json.Marshal(res.(map[string]any)["backend"])
	t.Logf("backend: %s", backend)
	if st := a.claudeCodeStatus(); st["ready"] != true {
		t.Fatalf("Claude Code is not ready: %v", st)
	}

	pb, _ := json.Marshal(map[string]any{"text": "Create a file hello.txt containing the word hi, then report done."})
	if _, err := a.Handle("prompt.submit", pb); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		running := 0
		for _, ag := range a.Coord.Agents.List() {
			if ag.Status == "running" || ag.Status == "starting" || ag.Status == "queued" || ag.Status == "waiting" {
				running++
			}
		}
		mu.Lock()
		ended := counts["agent.completed"] + counts["agent.failed"]
		started := counts["agent.started"]
		mu.Unlock()
		if running == 0 && ended > 0 {
			break
		}
		if started > 6 {
			a.Coord.StopAll(context.Background())
			t.Fatalf("more than 6 agent starts for a one-file objective")
		}
	}
	var agents []string
	for _, ag := range a.Coord.Agents.List() {
		agents = append(agents, string(ag.Role)+":"+string(ag.Status)+":"+ag.Model)
	}
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Error("Close did not return within 10s")
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("agents: %v", agents)
	t.Logf("events: completed %d, failed %d, permission %d, chat %d", counts["agent.completed"], counts["agent.failed"], counts["permission.asked"], counts["chat.message"])
	for _, c := range chat {
		if len(c) > 600 {
			c = c[:600]
		}
		t.Logf("chat: %s", c)
	}
	for _, w := range warns {
		if len(w) > 400 {
			w = w[:400]
		}
		t.Logf("log: %s", w)
	}
	if counts["agent.completed"] == 0 || len(failures) > 0 {
		t.Fatalf("the run did not complete: %v", failures)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "hello.txt")); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("hello.txt = %q, %v", b, err)
	}
}
