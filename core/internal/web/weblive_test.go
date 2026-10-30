//go:build weblive

package web

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestWebLiveSearch is deliberately outside make test. It uses the real
// provider and network only when both the build tag and explicit opt-in exist.
func TestWebLiveSearch(t *testing.T) {
	if os.Getenv("RELO_WEB_LIVE") != "1" {
		t.Skip("set RELO_WEB_LIVE=1 with -tags=weblive for live network access")
	}
	engine, err := New(Settings{Mode: ModeHidden, StateDir: t.TempDir()}, Hooks{Log: func(msg string) { t.Log(msg) }})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	running, err := engine.Do(context.Background(), "live-smoke", Request{Action: ActionSearch, Query: "Relo Code"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		result, err := engine.Wait(context.Background(), "live-smoke", running.Op, MaxWait)
		if err != nil {
			t.Fatal(err)
		}
		if result.State == StateRunning {
			continue
		}
		if result.State != StateDone || len(result.Results) == 0 {
			t.Fatalf("live search = %+v", result)
		}
		return
	}
	t.Fatal("live search still running after bounded waits", time.Duration(3)*MaxWait)
}
