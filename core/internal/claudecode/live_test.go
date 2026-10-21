package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/runtime"
)

// TestLiveClaudeCode drives the installed CLI on its own sign-in with Haiku at low effort: a reply, a command that
// needs Relo's permission, a cancel during a long command, and a question that needs the earlier turns. Gated by
// RELO_E2E_CLAUDE=1; it uses a few thousand tokens of the signed-in plan.
func TestLiveClaudeCode(t *testing.T) {
	if os.Getenv("RELO_E2E_CLAUDE") != "1" {
		t.Skip("set RELO_E2E_CLAUDE=1 to run against the installed Claude Code CLI")
	}
	bin, err := Look(os.Getenv("RELO_CLAUDE_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rt, err := New(ctx, bin, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	t.Logf("claude %s, %s, %d models", rt.Info().Version, rt.Info().Plan, len(rt.Info().Models))
	dir := t.TempDir()
	sid, err := rt.StartSession(ctx, runtime.SessionConfig{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := rt.Events(ctx, sid)
	tk := func(prompt string) runtime.AgentTask {
		return runtime.AgentTask{AgentID: "live", Role: "worker", Provider: ProviderID, Model: "claude-haiku-5-5", Effort: "low", TimeoutSec: 120,
			System: "You are a Relo worker agent in a scratch directory. Do exactly what the task says, nothing more. End every turn with one line of JSON: {\"status\":\"completed\",\"summary\":\"...\"}.",
			Prompt: prompt, Tools: runtime.ToolsFor("worker", "build", true)}
	}
	var total runtime.Usage
	turn := func(prompt string, on func(runtime.Event)) runtime.Event {
		t.Helper()
		if err := rt.SendTask(ctx, sid, tk(prompt)); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(2 * time.Minute)
		for {
			select {
			case ev := <-evs:
				if on != nil {
					on(ev)
				}
				if ev.Done || ev.Interrupted {
					if ev.Usage != nil {
						total.Input += ev.Usage.Input
						total.CacheRead += ev.Usage.CacheRead
						total.CacheWrite += ev.Usage.CacheWrite
						total.Output += ev.Usage.Output
						total.Reasoning += ev.Usage.Reasoning
					}
					return ev
				}
			case <-deadline:
				t.Fatalf("turn %q did not end", prompt)
			}
		}
	}

	end := turn("Reply with the JSON line only.", nil)
	if end.Error != "" || !strings.Contains(end.Text, `"completed"`) {
		t.Fatalf("reply turn = %+v", end)
	}

	asked := 0
	end = turn("Run this shell command: mkdir made-by-relo && echo MADE", func(ev runtime.Event) {
		if ev.Permission != nil {
			asked++
			t.Logf("permission: %v", ev.Permission)
			_ = rt.ReplyPermission(ctx, ev.Session, ev.Permission["id"].(string), "once")
		}
	})
	if end.Error != "" || asked == 0 {
		t.Fatalf("command turn = %+v, permission asked %d times", end, asked)
	}
	if st, err := os.Stat(filepath.Join(dir, "made-by-relo")); err != nil || !st.IsDir() {
		t.Fatalf("the command did not run in the session's directory: %v", err)
	}

	end = turn(`Run this shell command and wait for it: python3 -c "import time; time.sleep(60)"`, func(ev runtime.Event) {
		if ev.Permission != nil {
			_ = rt.ReplyPermission(ctx, ev.Session, ev.Permission["id"].(string), "once")
			go func() {
				time.Sleep(3 * time.Second)
				_ = rt.Cancel(ctx, sid)
			}()
		}
	})
	if !end.Interrupted {
		t.Fatalf("cancel during the command = %+v, want an interruption", end)
	}

	end = turn("What is the name of the directory you created earlier? Put just the name in the summary.", nil)
	if end.Error != "" || !strings.Contains(end.Text, "made-by-relo") {
		t.Fatalf("the session lost its conversation: %+v", end)
	}
	t.Logf("usage over four turns: input %d, cache read %d, cache write %d, output %d, thinking %d",
		total.Input, total.CacheRead, total.CacheWrite, total.Output, total.Reasoning)
}
