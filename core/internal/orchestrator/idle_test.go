package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

// pacedRuntime plays a script of runtime events with a delay before each one, the way a model working for a while
// does. A script that ends without a Done event stays silent forever.
type pacedRuntime struct {
	*dispatchRuntime
	script []paced
}

type paced struct {
	after time.Duration
	ev    rt.Event
}

func (r pacedRuntime) SendTask(_ context.Context, _ rt.SessionID, task rt.AgentTask) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()
	go func() {
		for _, step := range r.script {
			time.Sleep(step.after)
			r.events <- step.ev
		}
	}()
	return nil
}

var (
	thinking = rt.Event{Type: "message.part.updated", PartType: "reasoning", Phase: "reasoning"}
	toolRuns = rt.Event{Type: "message.part.updated", PartType: "tool", Phase: "tool", Tool: "shell", ToolID: "t1"}
	toolCmd  = rt.Event{Type: "session.tool.called", PartType: "tool", Phase: "tool", ToolID: "t1", ToolInput: "ctest -j8 --timeout 300"}
	toolEnds = rt.Event{Type: "message.part.updated", PartType: "tool", Phase: "model", ToolID: "t1", ToolDone: true}
	finished = rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: `{"status":"completed","summary":"ok"}`}
)

// setLimits shortens the clock for one test. recoveries is how many times a silent turn may be interrupted.
func setLimits(t *testing.T, idle time.Duration, recoveries int) {
	t.Helper()
	oldIdle, oldRec, oldWait := agentIdleLimit, agentRecoveries, interruptWait
	agentIdleLimit, agentRecoveries, interruptWait = idle, recoveries, 150*time.Millisecond
	t.Cleanup(func() { agentIdleLimit, agentRecoveries, interruptWait = oldIdle, oldRec, oldWait })
}

func runPaced(t *testing.T, idle time.Duration, script []paced) (string, error, time.Duration) {
	t.Helper()
	setLimits(t, idle, 0)

	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(pacedRuntime{newDispatchRuntime(), script})
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Design", Role: string(agent.Architect), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "hard"})
	a := c.spawn(task, agent.Architect)
	start := time.Now()
	body, err := c.runAgent(context.Background(), a, prompt.Parts{}, "go", prompt.ReportSchema)
	return body, err, time.Since(start)
}

func steps(n int, gap time.Duration, ev rt.Event) []paced {
	out := make([]paced, n)
	for i := range out {
		out[i] = paced{gap, ev}
	}
	return out
}

// The run that prompted this: still reading code, cut off by a clock that only knew how long it had been going.
func TestAnAgentThatKeepsWorkingIsNotTimedOutHoweverLongItTakes(t *testing.T) {
	script := append(steps(20, 25*time.Millisecond, thinking), paced{25 * time.Millisecond, finished})
	body, err, took := runPaced(t, 100*time.Millisecond, script)
	if err != nil {
		t.Fatalf("an agent that never went quiet for 100ms was stopped after %s: %v", took, err)
	}
	if !strings.Contains(body, "completed") {
		t.Fatalf("body = %q", body)
	}
	if took < 400*time.Millisecond {
		t.Fatalf("the script did not run for longer than the idle limit (%s), so this proves nothing", took)
	}
}

func TestAnAgentThatGoesQuietIsStopped(t *testing.T) {
	_, err, took := runPaced(t, 80*time.Millisecond, steps(2, 10*time.Millisecond, thinking))
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "no activity") {
		t.Fatalf("err = %v", err)
	}
	if took > 600*time.Millisecond {
		t.Fatalf("took %s to notice that nothing was happening", took)
	}
}

// A build or a test suite is silent while it runs. That is the tool working: the clock is off until it ends, and
// back on after.
func TestAnAgentWaitingOnAToolCallIsNotTimedOut(t *testing.T) {
	script := []paced{{10 * time.Millisecond, toolRuns}, {250 * time.Millisecond, toolEnds}, {10 * time.Millisecond, finished}}
	if _, err, took := runPaced(t, 80*time.Millisecond, script); err != nil {
		t.Fatalf("an agent whose tool call ran for 250ms, three times the limit, was stopped after %s: %v", took, err)
	}
	// Once the call has ended the limit applies again.
	script = []paced{{10 * time.Millisecond, toolRuns}, {10 * time.Millisecond, toolEnds}}
	if _, err, _ := runPaced(t, 80*time.Millisecond, script); err == nil {
		t.Fatal("an agent that went quiet after its tool call finished was never stopped")
	}
}

// A shell command that runs for nine minutes, or an hour, is not a reason to stop the agent. Only the user can.
func TestAToolCallThatNeverReturnsIsNeverTimedOut(t *testing.T) {
	setLimits(t, 30*time.Millisecond, 1)
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: [][]paced{{{10 * time.Millisecond, toolRuns}, {10 * time.Millisecond, toolCmd}}}}
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Run the suite", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "hard"})
	a := c.spawn(task, agent.Worker)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.runAgent(ctx, a, prompt.Parts{}, "go", prompt.ReportSchema); done <- err }()

	select {
	case err := <-done:
		t.Fatalf("the agent was stopped while its command was still running (ten times the limit had not passed): %v", err)
	case <-time.After(300 * time.Millisecond): // ten times the idle limit, and four times its interrupt wait
	}
	r.mu.Lock()
	cancels := r.cancels
	r.mu.Unlock()
	if cancels != 0 {
		t.Fatalf("the running command was interrupted %d times", cancels)
	}
	cancel() // Stop
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not end the wait for the command")
	}
}

// A model step starts only when the last step's tool calls have ended. If the event that said so was lost, the
// agent must not be left looking as if it were still in a tool call, with its clock off for good.
func TestALostToolEndEventDoesNotSwitchTheClockOffForGood(t *testing.T) {
	setLimits(t, 60*time.Millisecond, 0)
	stepStarts := rt.Event{Type: "session.step.started", Phase: "model"}
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), turns: [][]paced{{{10 * time.Millisecond, toolRuns}, {10 * time.Millisecond, stepStarts}}}} // toolEnds never arrives
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Run", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "hard"})
	a := c.spawn(task, agent.Worker)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // a failure is a hang, so bound it
	defer cancel()
	_, err := c.runAgent(ctx, a, prompt.Parts{}, "go", prompt.ReportSchema)
	var stalled *stalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("err = %v, want the silent agent to be stopped by its clock", err)
	}
}

// turnsRuntime plays one script per task it is sent, the way a session does across a stalled turn and the turn that
// resumes it. confirm says whether the runtime answers an interrupt, as OpenCode does.
type turnsRuntime struct {
	*dispatchRuntime
	turns   [][]paced
	confirm bool
	cancels int
}

var (
	toolFails   = rt.Event{Type: "session.tool.failed", Phase: "model", ToolID: "t1", ToolDone: true}
	interrupted = rt.Event{Type: "session.execution.interrupted", Interrupted: true}
)

func (r *turnsRuntime) SendTask(_ context.Context, _ rt.SessionID, task rt.AgentTask) error {
	r.mu.Lock()
	n := len(r.tasks)
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()
	if n < len(r.turns) {
		script := r.turns[n]
		go func() {
			for _, step := range script {
				time.Sleep(step.after)
				r.events <- step.ev
			}
		}()
	}
	return nil
}

func (r *turnsRuntime) Cancel(context.Context, rt.SessionID) error {
	r.mu.Lock()
	r.cancels++
	r.mu.Unlock()
	if r.confirm {
		r.events <- toolFails
		r.events <- interrupted
	}
	return nil
}

func runTurns(t *testing.T, r *turnsRuntime) (*Coord, agent.Agent, string, error) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Run the suite", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "hard"})
	a := c.spawn(task, agent.Worker)
	body, err := c.runAgent(context.Background(), a, prompt.Parts{}, "go", prompt.ReportSchema)
	return c, a, body, err
}

// The model goes quiet with no tool running: the call is interrupted and the agent, which keeps its context and its
// edits, is told and carries on.
func TestASilentModelCallIsInterruptedAndTheAgentIsToldToCarryOn(t *testing.T) {
	setLimits(t, 100*time.Millisecond, 1)
	r := &turnsRuntime{
		dispatchRuntime: newDispatchRuntime(),
		confirm:         true,
		turns: [][]paced{
			{{10 * time.Millisecond, thinking}}, // then nothing, ever
			{{10 * time.Millisecond, finished}},
		},
	}
	c, _, body, err := runTurns(t, r)
	if err != nil {
		t.Fatalf("a silent model call failed the agent instead of being interrupted: %v", err)
	}
	if !strings.Contains(body, "completed") {
		t.Fatalf("body = %q", body)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancels != 1 || len(r.tasks) != 2 || r.starts != 1 {
		t.Fatalf("cancels=%d tasks=%d sessions=%d, want the one session interrupted once and sent one more task", r.cancels, len(r.tasks), r.starts)
	}
	nudge := r.tasks[1].Prompt
	for _, want := range []string{"model call", "interrupted", "same format"} {
		if !strings.Contains(nudge, want) {
			t.Errorf("the agent was not told %q:\n%s", want, nudge)
		}
	}
	said := false
	for _, m := range c.Current().Messages {
		if m.Role == "system" && strings.Contains(m.Text, "the model did not respond for") && strings.Contains(m.Text, "interrupted") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the user was not told what happened: %+v", c.Current().Messages)
	}
}

// Two nudges are enough: an agent that keeps going silent is stopped.
func TestAnAgentThatKeepsGoingSilentIsStoppedAfterTheRecoveriesAreSpent(t *testing.T) {
	setLimits(t, 80*time.Millisecond, 2)
	hangs := []paced{{10 * time.Millisecond, thinking}}
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: [][]paced{hangs, hangs, hangs}}
	_, _, _, err := runTurns(t, r)
	var stalled *stalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("err = %v, want a stalled agent", err)
	}
	for _, want := range []string{"timed out", "no activity for 80ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not say %q", err, want)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 3 {
		t.Fatalf("sent %d tasks, want the first plus two resumptions", len(r.tasks))
	}
}

// If the runtime never confirms the interrupt there is nothing safe to resume: fail rather than wait for ever, and
// report the silence that started it, not the short wait for the confirmation.
func TestAnInterruptTheRuntimeNeverConfirmsFailsInsteadOfHanging(t *testing.T) {
	setLimits(t, 80*time.Millisecond, 1)
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: false, turns: [][]paced{{{10 * time.Millisecond, thinking}}}}
	start := time.Now()
	_, _, _, err := runTurns(t, r)
	if err == nil || !strings.Contains(err.Error(), "no activity for 80ms") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s", took)
	}
}

// What the user could see of the nine minutes was a status line that never changed.
func TestTheStatusLineSaysHowLongAnAgentHasBeenQuiet(t *testing.T) {
	setLimits(t, 5*time.Second, 0)
	oldShown, oldTick := silenceShown, silenceTick
	silenceShown, silenceTick = 80*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { silenceShown, silenceTick = oldShown, oldTick })
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), turns: [][]paced{{{10 * time.Millisecond, toolRuns}, {10 * time.Millisecond, toolCmd}, {300 * time.Millisecond, toolCmd}, {300 * time.Millisecond, toolEnds}, {10 * time.Millisecond, finished}}}}
	// The second toolCmd repeats what the call is doing: it does not change the label by itself, so only taking the
	// note off the status line can make it disappear.

	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Run", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "hard"})
	a := c.spawn(task, agent.Worker)
	done := make(chan error, 1)
	go func() {
		_, err := c.runAgent(context.Background(), a, prompt.Parts{}, "go", prompt.ReportSchema)
		done <- err
	}()

	sawQuiet, sawCleared := false, false
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			break loop
		case <-deadline:
			t.Fatal("agent never finished")
		case <-time.After(5 * time.Millisecond):
			got, _ := c.Agents.Get(a.ID)
			switch {
			case strings.HasPrefix(got.Activity, "Quiet for ") && strings.HasSuffix(got.Activity, "shell: ctest -j8 --timeout 300"):
				sawQuiet = true
			case sawQuiet && got.Activity == "shell: ctest -j8 --timeout 300":
				sawCleared = true
			}
		}
	}
	if !sawQuiet {
		t.Error("the status line never said how long the agent had been quiet, and on what")
	}
	if !sawCleared {
		t.Error("the quiet note stayed on the status line after the agent spoke again")
	}
}
