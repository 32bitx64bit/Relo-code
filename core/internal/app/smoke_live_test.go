package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
	relort "github.com/relocode/relo/internal/runtime"
)

// TestLiveSmoke drives the real OpenCode backend with the free model.
// Gated by RELO_LIVE_SMOKE=1. RELO_SMOKE_PRESET=auto|off (default auto).
// Hard cap: RELO_SMOKE_MAX_TURNS (default 4) live agent starts; exceeding aborts.
// Artifacts: /tmp/kilo/smoke/<ts>-<preset>/{events.jsonl,summary.md}.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("RELO_LIVE_SMOKE") != "1" {
		t.Skip("set RELO_LIVE_SMOKE=1 to run live OpenCode smoke test")
	}
	preset := os.Getenv("RELO_SMOKE_PRESET")
	if preset == "" {
		preset = "auto"
	}
	maxTurns := 4
	if v := os.Getenv("RELO_SMOKE_MAX_TURNS"); v != "" {
		fmt.Sscanf(v, "%d", &maxTurns)
	}
	const model = "opencode/cat-jam-free"
	// The free model is an OpenCode built-in, which Relo hides unless sharing is on.
	t.Setenv("RELO_SHARE_OPENCODE", "1")

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.txt"), []byte("smoke workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join("/tmp/kilo/smoke", time.Now().UTC().Format("20060102T150405Z")+"-"+preset)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evf, err := os.Create(filepath.Join(outDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer evf.Close()

	var mu sync.Mutex
	counts := map[string]int{}
	turns := 0
	var cacheLast map[string]any
	abort := make(chan string, 1)

	var logMu sync.Mutex
	var logLines []string
	logw := writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
			if strings.Contains(l, `"level":"warn"`) || strings.Contains(l, `"level":"error"`) {
				logLines = append(logLines, l)
			}
		}
		return len(p), nil
	})
	log := logging.New(logw, logging.Warn)

	baseGor := runtime.NumGoroutine()
	a, err := New(Config{Workspace: ws, Runtime: "opencode", LogLevel: "warn"}, log)
	if err != nil {
		t.Fatal(err)
	}
	a.Coord.SetEmitter(func(name string, payload any) {
		b, _ := json.Marshal(payload)
		mu.Lock()
		defer mu.Unlock()
		counts[name]++
		line, _ := json.Marshal(map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano), "event": name, "payload": json.RawMessage(b)})
		evf.Write(append(line, '\n'))
		if name == "cache.updated" {
			if m, ok := payload.(map[string]any); ok {
				cacheLast = m
			}
		}
	})

	init := map[string]any{
		"primaryModel": model, "tier1Model": model, "tier2Model": model,
		"efficiency": map[string]any{"preset": preset},
		"logLevel":   "warn",
	}
	raw, _ := json.Marshal(init)
	res, err := a.Handle("initialize", raw)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if m, ok := res.(map[string]any); ok {
		b, _ := json.Marshal(m["backend"])
		t.Logf("backend: %s", b)
	}
	// Count every live SendTask (= one model turn) and enforce the hard cap.
	if rt := a.Coord.RuntimeSnapshot(); rt != nil {
		a.Coord.SetRuntime(&countingRT{AgentRuntime: rt, max: maxTurns, onOver: func(n int) {
			select {
			case abort <- fmt.Sprintf("live turn %d refused: cap %d", n, maxTurns):
			default:
			}
		}, next: func() int {
			mu.Lock()
			defer mu.Unlock()
			turns++
			return turns
		}})
	}
	if models, err := a.Handle("models.list", nil); err != nil {
		t.Logf("models.list error: %v", err)
	} else {
		t.Logf("models.list: %d entries", lenOf(models))
	}

	objective := "Create a file hello.txt containing the word hi, then report done."
	pb, _ := json.Marshal(map[string]any{"text": objective})
	submitDone := make(chan error, 1)
	go func() {
		_, err := a.Handle("prompt.submit", pb)
		submitDone <- err
	}()

	deadline := time.After(4 * time.Minute)
	var reason string
	select {
	case reason = <-abort:
		t.Errorf("ABORT: %s", reason)
		a.Coord.StopAll(context.Background())
	case <-submitDone:
		// Handle returned fast; the actual run happens in a goroutine. Wait on runWG via StopAll side-effect is unsafe, poll instead.
		reason = "submit-returned"
	case <-deadline:
		reason = "timeout 4m"
	}
	// Poll until no agent is running or deadline.
	for reason != "timeout 4m" && !strings.HasPrefix(reason, "agent starts") {
		mu.Lock()
		running := 0
		for _, ag := range a.Coord.Agents.List() {
			if ag.Status == "running" || ag.Status == "starting" || ag.Status == "queued" {
				running++
			}
		}
		mu.Unlock()
		if running == 0 && counts["agent.completed"]+counts["agent.failed"] > 0 {
			break
		}
		select {
		case <-deadline:
			reason = "timeout 4m"
		case r := <-abort:
			t.Errorf("ABORT: %s", r)
			a.Coord.StopAll(context.Background())
			reason = r
		case <-time.After(2 * time.Second):
		}
	}

	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
		t.Logf("Close returned cleanly")
	case <-time.After(5 * time.Second):
		t.Errorf("Close did not return within 5s")
	}
	time.Sleep(500 * time.Millisecond)
	endGor := runtime.NumGoroutine()
	if endGor > baseGor+5 {
		t.Logf("WARN goroutine delta %d (base=%d end=%d)", endGor-baseGor, baseGor, endGor)
	} else {
		t.Logf("goroutines base=%d end=%d", baseGor, endGor)
	}

	mu.Lock()
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Live smoke summary (preset=%s)\n\nstop reason=%q turns=%d cap=%d\n\n## Event counts\n", preset, reason, turns, maxTurns)
	for _, k := range keys {
		fmt.Fprintf(&sb, "- %s: %d\n", k, counts[k])
	}
	cb, _ := json.MarshalIndent(cacheLast, "", "  ")
	fmt.Fprintf(&sb, "\n## Last cache.updated\n```json\n%s\n```\n", cb)
	mu.Unlock()

	logMu.Lock()
	fmt.Fprintf(&sb, "\n## Warn/error log lines (%d)\n", len(logLines))
	for _, l := range logLines {
		if len(l) > 400 {
			l = l[:400]
		}
		fmt.Fprintf(&sb, "- %s\n", l)
	}
	logMu.Unlock()
	if err := os.WriteFile(filepath.Join(outDir, "summary.md"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("artifacts: %s", outDir)
}

// countingRT wraps the real runtime; every SendTask is one live model turn.
// Calls beyond max are refused before reaching OpenCode (hard cap).
type countingRT struct {
	relort.AgentRuntime
	max    int
	next   func() int
	onOver func(n int)
}

func (c *countingRT) SendTask(ctx context.Context, sid relort.SessionID, task relort.AgentTask) error {
	n := c.next()
	if n > c.max {
		c.onOver(n)
		return fmt.Errorf("smoke: live turn cap %d reached", c.max)
	}
	return c.AgentRuntime.SendTask(ctx, sid, task)
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func lenOf(v any) int {
	b, _ := json.Marshal(v)
	var arr []any
	_ = json.Unmarshal(b, &arr)
	return len(arr)
}
