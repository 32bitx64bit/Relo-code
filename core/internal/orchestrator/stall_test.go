package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/metrics"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

// stalling builds a coordinator whose only task goes silent on its first turn. route is the routing the coordinator
// has; steps is how many model requests the agent had made before it went quiet.
func stalling(t *testing.T, r *turnsRuntime, route routing.Config, role agent.Role, steps int) (*Coord, agent.Agent, string, error) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	c.Route = route
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Review it", Role: string(role), Workspace: s.Workspace, Model: "mock/dead", Difficulty: "easy"})
	a := c.spawn(task, role)
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Steps = steps })
	body, err := c.runAgent(context.Background(), a, prompt.Parts{}, "go", prompt.ReportSchema)
	return c, a, body, err
}

var hangThenFinish = [][]paced{{{10 * time.Millisecond, thinking}}, {{10 * time.Millisecond, finished}}}

// An agent that made almost no progress and has a backup moves to it at the first silence. Nudging it would wait out
// five more minutes on a route that is not answering (five agents did that in a row, fifteen minutes each).
func TestAnEarlySilentAgentMovesToItsBackupAtOnce(t *testing.T) {
	setLimits(t, 80*time.Millisecond, 2)
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: hangThenFinish}
	route := routing.Config{BackupByDifficulty: map[string]string{"easy": "mock/backup"}}
	c, a, body, err := stalling(t, r, route, agent.Reviewer, 3)
	if err != nil || !strings.Contains(body, "completed") {
		t.Fatalf("body=%q err=%v, want the backup to finish the task", body, err)
	}
	got, _ := c.Agents.Get(a.ID)
	if got.Model != "mock/backup" || got.FallbackFrom != "mock/dead" || got.FallbackClass != "stalled" {
		t.Fatalf("agent = model %q from %q class %q", got.Model, got.FallbackFrom, got.FallbackClass)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A nudge resumes the same session; a move starts a new one and sends the task fresh, without the nudge text.
	if r.starts != 2 || len(r.tasks) != 2 || strings.Contains(r.tasks[1].Prompt, "Relo interrupted it") {
		t.Fatalf("sessions=%d tasks=%d: an early agent with a backup was nudged instead of moved", r.starts, len(r.tasks))
	}
	if !c.recentlyStalled("mock/dead") {
		t.Fatal("the silent model was not remembered")
	}
	said := false
	for _, m := range c.Current().Messages {
		if m.Role == "system" && strings.Contains(m.Text, "mock/dead stopped responding") && strings.Contains(m.Text, "mock/backup") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the user was not told the model changed: %+v", c.Current().Messages)
	}
}

// A reviewer carries no difficulty of its own; its role default picks its backup row.
func TestAReviewerWithNoDifficultyStillHasABackup(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Route = routing.Config{BackupByDifficulty: map[string]string{"easy": "mock/backup"}}
	a := agent.Agent{Role: agent.Reviewer, Model: "mock/dead"}
	if got := c.backupModel(a); got != "mock/backup" {
		t.Fatalf("backupModel = %q, want the easy row", got)
	}
}

// An agent that has real work in its context is nudged first: moving it would throw the work away.
func TestAnAgentWithProgressIsNudgedBeforeItMoves(t *testing.T) {
	setLimits(t, 80*time.Millisecond, 2)
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: hangThenFinish}
	route := routing.Config{BackupByDifficulty: map[string]string{"easy": "mock/backup"}}
	c, a, _, err := stalling(t, r, route, agent.Reviewer, 40)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.Agents.Get(a.ID)
	if got.Model != "mock/dead" || got.FallbackFrom != "" {
		t.Fatalf("agent moved to %q although it had 40 steps of work and a nudge was enough", got.Model)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.starts != 1 || r.cancels != 1 || len(r.tasks) != 2 || !strings.Contains(r.tasks[1].Prompt, "interrupted") {
		t.Fatalf("sessions=%d cancels=%d tasks=%d, want one session nudged once", r.starts, r.cancels, len(r.tasks))
	}
}

// Without a backup the agent behaves as before: it is nudged, then failed.
func TestASilentAgentWithNoBackupIsStillNudged(t *testing.T) {
	setLimits(t, 80*time.Millisecond, 2)
	hangs := []paced{{10 * time.Millisecond, thinking}}
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: [][]paced{hangs, hangs, hangs}}
	_, _, _, err := stalling(t, r, routing.Config{}, agent.Reviewer, 0)
	var stalled *stalledError
	if !errors.As(err, &stalled) || !stalled.model {
		t.Fatalf("err = %v, want a model stall", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 3 {
		t.Fatalf("sent %d tasks, want the first plus two resumptions", len(r.tasks))
	}
}

// With no backup row the ladder's next model is used, but never the Primary's.
func TestStallFailoverUsesTheLadderButNeverThePrimary(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Route = routing.Config{Primary: "mock/p", Tier1: "mock/t1", Tier2: "mock/t2"}
	if got := c.stallFailover(agent.Agent{Role: agent.Reviewer, Model: "mock/t2"}); got != "mock/t1" {
		t.Fatalf("failover from the cheapest model = %q, want the next one up", got)
	}
	if got := c.stallFailover(agent.Agent{Role: agent.Reviewer, Model: "mock/t1"}); got != "" {
		t.Fatalf("failover from the top worker model = %q, want none rather than the Primary's model", got)
	}
	if got := c.stallFailover(agent.Agent{Role: agent.Primary, Model: "mock/p"}); got != "" {
		t.Fatalf("the Primary has no failover, got %q", got)
	}
}

// A tool that outruns its clock is not a model problem; another model would meet the same tool.
func TestAToolThatOutranItsClockDoesNotChangeTheModel(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := &fallbackRuntime{dispatchRuntime: newDispatchRuntime(), id: "main", err: &stalledError{msg: "tool call \"shell\" exceeded 30m0s wall-clock limit"}}
	c.SetRuntime(r)
	c.Route = routing.Config{BackupByDifficulty: map[string]string{"easy": "backup/model"}}
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Reviewer, Model: "main/model", Difficulty: "easy", Workspace: s.Workspace})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{}, "go", nil); err == nil {
		t.Fatal("want the stall to fail the agent")
	}
	if got, _ := c.Agents.Get(a.ID); got.FallbackFrom != "" || len(r.tasks) != 1 {
		t.Fatalf("agent switched models after a tool stall: %+v, %d calls", got, len(r.tasks))
	}
	c.CloseMessages()
}

// New work is not sent to a model that has just gone silent, as long as a backup is configured for it.
func TestNewWorkAvoidsAModelThatJustWentSilent(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Route = routing.Config{BackupByDifficulty: map[string]string{"easy": "mock/backup"}}
	if got := c.avoidStalled(agent.Reviewer, "easy", "mock/dead"); got != "mock/dead" {
		t.Fatalf("a healthy model was swapped: %q", got)
	}
	c.noteStall("mock/dead")
	if got := c.avoidStalled(agent.Reviewer, "easy", "mock/dead"); got != "mock/backup" {
		t.Fatalf("avoidStalled = %q, want the backup", got)
	}
	if got := c.avoidStalled(agent.Reviewer, "easy", "mock/other"); got != "mock/other" {
		t.Fatalf("another model was swapped: %q", got)
	}
	// The backup being dead too leaves the model alone: there is nowhere better to send it.
	c.noteStall("mock/backup")
	if got := c.avoidStalled(agent.Reviewer, "easy", "mock/dead"); got != "mock/dead" {
		t.Fatalf("avoidStalled = %q, want the model kept when the backup is also silent", got)
	}
	c.mu.Lock()
	c.stalledAt["mock/dead"] = time.Now().Add(-stallMemory - time.Second)
	c.mu.Unlock()
	if c.recentlyStalled("mock/dead") {
		t.Fatal("a stall was remembered past stallMemory")
	}
}

// A task retried after its model went silent starts on another model, not on the same one at attempt two.
func TestARetryAfterAModelStallMovesModel(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime())
	c.Route = routing.Config{BackupByDifficulty: map[string]string{"easy": "mock/backup"}}
	c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{ID: "r", Title: "r", Role: string(agent.Reviewer), Difficulty: "easy", Model: "mock/dead", Attempt: 1, MaxAttempts: 3})
	a := c.spawn(task, agent.Reviewer)
	c.retryTaskAfter(task, a, &stalledError{msg: "agent timed out: no activity for 5m0s", model: true})
	got, _ := c.Graph.Get("r")
	if got.Status != taskgraph.Queued || got.Model != "mock/backup" {
		t.Fatalf("task after the stall = %q on %q, want queued on the backup", got.Status, got.Model)
	}
	c.Graph.Update("r", func(x *taskgraph.Task) { x.Model = "mock/dead"; x.Status = taskgraph.Running })
	c.retryTaskAfter(task, a, errors.New("provider returned 500"))
	if got, _ := c.Graph.Get("r"); got.Model != "mock/dead" {
		t.Fatalf("an ordinary failure changed the model to %q", got.Model)
	}
}

// The first retry stays on the model; a second failure of the same kind hands the task to the backup.
func TestATaskMovesToItsBackupAfterTheFirstRetryFails(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime())
	c.Route = routing.Config{BackupByDifficulty: map[string]string{"easy": "mock/backup"}}
	c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Model: "mock/p"})
	apiDown := errors.New("provider returned 502 bad gateway")

	first := c.Graph.Add(taskgraph.Task{ID: "a", Title: "a", Role: string(agent.Reviewer), Difficulty: "easy", Model: "mock/main", Attempt: 1, MaxAttempts: 3})
	c.retryTaskAfter(first, c.spawn(first, agent.Reviewer), apiDown)
	if got, _ := c.Graph.Get("a"); got.Model != "mock/main" {
		t.Fatalf("first failure moved the task to %q, want one retry on the same model", got.Model)
	}

	second := c.Graph.Add(taskgraph.Task{ID: "b", Title: "b", Role: string(agent.Reviewer), Difficulty: "easy", Model: "mock/main", Attempt: 2, MaxAttempts: 3})
	c.retryTaskAfter(second, c.spawn(second, agent.Reviewer), apiDown)
	got, _ := c.Graph.Get("b")
	if got.Model != "mock/backup" || got.Status != taskgraph.Queued {
		t.Fatalf("task after the retry failed = %q on %q, want queued on the backup", got.Status, got.Model)
	}
	if !got.NotBefore.Before(time.Now().Add(10 * time.Second)) {
		t.Fatalf("retry waits until %v, want a few seconds", got.NotBefore)
	}

	// A failure that is not the API's (a bad plan, a failed test) is not a reason to change the model.
	third := c.Graph.Add(taskgraph.Task{ID: "c", Title: "c", Role: string(agent.Reviewer), Difficulty: "easy", Model: "mock/main", Attempt: 2, MaxAttempts: 3})
	c.retryTaskAfter(third, c.spawn(third, agent.Reviewer), errors.New("report said the check failed"))
	if got, _ := c.Graph.Get("c"); got.Model != "mock/main" {
		t.Fatalf("an ordinary failure moved the task to %q", got.Model)
	}
}

var _ = rt.Event{}

func TestCostUnitsWeighCacheWritesAboveReads(t *testing.T) {
	got := costUnits(metrics.Usage{Input: 100, CacheWrite: 800, CacheRead: 1000, Output: 10})
	if want := 100 + 1000 + 100 + 50; got != want {
		t.Fatalf("units = %d, want %d", got, want)
	}
}
