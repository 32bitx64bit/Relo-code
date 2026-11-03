package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/taskgraph"
)

// The Primary decides how long one command may run; the worker is told, and the watchdog waits that long.
func TestAPrimarySetCommandLimitReachesTheWorkerAndTheWatchdog(t *testing.T) {
	brief := taskBrief(taskgraph.Task{Title: "Run the ladder", CommandMinutes: 45})
	if !strings.Contains(brief, "Command time limit: 45 min") || !strings.Contains(brief, "nohup") {
		t.Fatalf("brief does not carry the limit:\n%s", brief)
	}
	if strings.Contains(taskBrief(taskgraph.Task{Title: "Small fix"}), "Command time limit") {
		t.Fatal("a task with no limit was given one")
	}

	st := &runState{toolWait: 45*time.Minute + 5*time.Minute}
	if got := st.toolLimit(); got != 50*time.Minute {
		t.Fatalf("watchdog waits %v for a 45 minute command, want 50m", got)
	}
	if got := (&runState{}).toolLimit(); got != agentToolWait {
		t.Fatalf("a task with no limit waits %v, want the default %v", got, agentToolWait)
	}
	// A limit shorter than the default never makes the watchdog stricter.
	if got := (&runState{toolWait: 6 * time.Minute}).toolLimit(); got != agentToolWait {
		t.Fatalf("a 1 minute limit shortened the watchdog to %v", got)
	}

	if clampCommandMinutes(-5) != 0 || clampCommandMinutes(90) != 90 || clampCommandMinutes(100000) != 24*60 {
		t.Fatal("command minutes are not kept between 0 and a day")
	}
}

func TestATurnTaskCarriesCommandMinutes(t *testing.T) {
	turn, err := ParseTurn(`{"status":"working","userMessage":"ok","tasks":[{"id":"t1","title":"Long run","role":"worker","description":"d","acceptance":["a"],"priority":1,"deps":[],"difficulty":"medium","commandMinutes":90}]}`, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Tasks) != 1 || turn.Tasks[0].CommandMinutes != 90 {
		t.Fatalf("parsed tasks = %+v", turn.Tasks)
	}
}
