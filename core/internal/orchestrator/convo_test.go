package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/runtime"
)

func TestSplitBlocksCutsAtHeadingsButNotInsideCode(t *testing.T) {
	text := "# Session-stable context\n\n### Objective [summary]\nBuild X\n\n### Notes [worker]\nSee:\n```\n### not a heading\n```\nDone.\n"
	got := splitBlocks("stable", text)
	var heads []string
	for _, b := range got {
		heads = append(heads, b.head)
	}
	want := []string{"# Session-stable context", "### Objective [summary]", "### Notes [worker]"}
	if strings.Join(heads, "|") != strings.Join(want, "|") {
		t.Fatalf("heads = %q, want %q", heads, want)
	}
	if !strings.Contains(got[2].text, "### not a heading") {
		t.Fatalf("a heading inside a code fence split the block: %q", got[2].text)
	}
	// The same heading twice stays two blocks.
	twice := splitBlocks("body", "### Report [worker]\none\n### Report [worker]\ntwo\n")
	if len(twice) != 2 || twice[0].key == twice[1].key {
		t.Fatalf("repeated heading collapsed: %+v", twice)
	}
}

// What a conversation has been sent, plus the delta, is the whole current context: the delta loses nothing.
func TestTheDeltaCarriesEverythingThatChanged(t *testing.T) {
	prev := contextBlocks("# Session-stable context\n\n### Objective [summary]\nBuild X\n", "## report\n### Alpha [worker]\nalpha v1\n### Beta [worker]\nbeta v1\n")
	cur := contextBlocks("# Session-stable context\n\n### Objective [summary]\nBuild Y\n", "## report\n### Alpha [worker]\nalpha v1\n### Gamma [worker]\ngamma v1\n")
	changed, gone := diffBlocks(blockMap(prev), cur)

	// Replaying the delta over what was sent must give exactly the current blocks.
	have := blockMap(prev)
	for _, b := range changed {
		have[b.key] = b
	}
	for _, b := range cur {
		if have[b.key].text != b.text {
			t.Errorf("block %q after the delta = %q, want %q", b.key, have[b.key].text, b.text)
		}
	}
	text := contextDelta(changed, gone)
	for _, want := range []string{"Build Y", "gamma v1", "No longer in your context: Beta [worker]"} {
		if !strings.Contains(text, want) {
			t.Errorf("delta lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"alpha v1", "Build X"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("delta repeats %q:\n%s", unwanted, text)
		}
	}
	if none := contextDelta(nil, nil); !strings.Contains(none, "Nothing in your context changed") {
		t.Errorf("empty delta = %q", none)
	}
}

func TestAConversationIsStartedOverOnlyWhenContinuingCostsMore(t *testing.T) {
	now := time.Now()
	convo := func(tokens int, age time.Duration, turns int) *primaryConvo {
		return &primaryConvo{static: "s", turns: turns, tokens: tokens, first: 30000, firstEst: 26000, sentAt: now.Add(-age)}
	}
	// fresh = 26000 estimate + 4000 overhead = 30000
	for _, tc := range []struct {
		name string
		cv   *primaryConvo
		want bool
	}{
		{"no conversation", nil, true},
		{"warm and small", convo(34000, time.Minute, 5), false},
		{"warm and grown to twice a fresh one plus slack", convo(70000, time.Minute, 5), true},
		{"warm and a little over", convo(55000, time.Minute, 5), false},
		{"cold and bigger than a fresh one", convo(40000, 8*time.Minute, 5), true},
		{"cold but no bigger than a fresh one", convo(29000, 8*time.Minute, 5), false},
		{"too many turns", convo(34000, time.Minute, primaryMaxTurns), true},
		{"not sent anything yet", convo(0, time.Hour, 0), false},
	} {
		if got := tc.cv.shouldStartOver("s", 26000, now); got != tc.want {
			t.Errorf("%s: shouldStartOver = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !convo(34000, time.Minute, 5).shouldStartOver("another prompt", 26000, now) {
		t.Error("a changed system prompt did not start the conversation over")
	}
}

// convoRuntime keeps every session and task so a test can see what each turn was sent, and can play a dead process.
type convoRuntime struct {
	*runtime.Mock
	mu       sync.Mutex
	starts   int
	closes   int
	tasks    []runtime.AgentTask
	sessions []runtime.SessionID
	chans    map[runtime.SessionID]chan runtime.Event
	failSend func(task int) bool
	context  int
}

func newConvoRuntime() *convoRuntime {
	return &convoRuntime{Mock: runtime.NewMock(), chans: map[runtime.SessionID]chan runtime.Event{}, context: 40000}
}

func (r *convoRuntime) StartSession(context.Context, runtime.SessionConfig) (runtime.SessionID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	id := runtime.SessionID(fmt.Sprintf("conv-%d", r.starts))
	r.chans[id] = make(chan runtime.Event, 16)
	return id, nil
}
func (r *convoRuntime) Events(_ context.Context, id runtime.SessionID) (<-chan runtime.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chans[id], nil
}
func (r *convoRuntime) SendTask(_ context.Context, id runtime.SessionID, task runtime.AgentTask) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, task)
	r.sessions = append(r.sessions, id)
	if r.failSend != nil && r.failSend(len(r.tasks)) {
		return errors.New("the Claude Code process for this session has ended; the agent needs a new session")
	}
	r.chans[id] <- runtime.Event{Done: true, JSON: `{"status":"completed","userMessage":"ok","tasks":[]}`, Usage: &runtime.Usage{Input: 10, Output: 10, Context: r.context}}
	return nil
}
func (r *convoRuntime) CloseSession(context.Context, runtime.SessionID) error {
	r.mu.Lock()
	r.closes++
	r.mu.Unlock()
	return nil
}
func (r *convoRuntime) count() (starts, closes, tasks int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts, r.closes, len(r.tasks)
}
func (r *convoRuntime) task(i int) runtime.AgentTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[i]
}

func convoCoord(t *testing.T) (*Coord, *convoRuntime, agent.Agent) {
	t.Helper()
	r := newConvoRuntime()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "plan it", ModeBuild)
	p := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	c.Engine.Bind(p.ID, "", string(agent.Primary), 24000)
	return c, r, p
}

func convoParts(static, objective string, reports []string, facts string) prompt.Parts {
	stable := "# Session-stable context\n\n### Objective [summary]\n" + objective + "\n\n"
	body := "## report\n"
	for _, r := range reports {
		// A real report runs to a few hundred bytes at least; the delta's fixed header must not outweigh a block.
		body += "### " + r + " [worker]\nThe full report of " + r + " with every detail the worker wrote.\n" + strings.Repeat("It changed a file, ran the checks and found nothing else to say.\n", 12)
	}
	extra := "## Workspace facts\n" + facts + "\n"
	return prompt.Parts{Static: static, Stable: stable, Body: body, Extra: extra, Dynamic: body + "\n" + extra}
}

func primaryTurnOf(t *testing.T, c *Coord, p agent.Agent, parts prompt.Parts, ask string) {
	t.Helper()
	if _, err := c.runAgent(context.Background(), p, parts, ask, prompt.PrimarySchemaFor(false)); err != nil {
		t.Fatal(err)
	}
}

// The second turn goes to the same conversation and carries only what changed. The whole context went in the first.
func TestThePrimaryContinuesOneConversationAndSendsOnlyWhatChanged(t *testing.T) {
	c, r, p := convoCoord(t)
	earlier := []string{"Alpha", "Delta", "Epsilon", "Zeta", "Eta"}
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", earlier, "- clean"), "Plan.")
	secondParts := convoParts("STATIC", "Build X", append(append([]string{}, earlier...), "Beta"), "- one file waiting")
	primaryTurnOf(t, c, p, secondParts, "Handle the message.")

	if starts, closes, tasks := r.count(); starts != 1 || closes != 0 || tasks != 2 {
		t.Fatalf("starts=%d closes=%d tasks=%d, want one conversation, still open, two turns", starts, closes, tasks)
	}
	first, second := r.task(0), r.task(1)
	if strings.Contains(first.System, "Objective") || !strings.Contains(first.System, "STATIC") {
		t.Fatalf("the system prompt holds context that changes between turns, or lost the role prompt:\n%s", first.System)
	}
	for _, want := range []string{"Build X", "Alpha", "- clean"} {
		if !strings.Contains(first.Prompt, want) {
			t.Errorf("the first message lacks %q", want)
		}
	}
	for _, want := range []string{"Context changes since your last turn", "Beta", "- one file waiting", "Handle the message."} {
		if !strings.Contains(second.Prompt, want) {
			t.Errorf("the second message lacks %q:\n%s", want, second.Prompt)
		}
	}
	for _, unwanted := range []string{"Build X", "full report of Alpha", "full report of Eta"} {
		if strings.Contains(second.Prompt, unwanted) {
			t.Errorf("the second message repeats %q:\n%s", unwanted, second.Prompt)
		}
	}
	whole := len(secondParts.Stable) + len(secondParts.Dynamic)
	if len(second.Prompt)*3 >= whole {
		t.Errorf("the continuing message (%d bytes) is not far smaller than the context it stands for (%d)", len(second.Prompt), whole)
	}
}

func TestAnItemTheEngineDroppedIsNamedAsGone(t *testing.T) {
	c, r, p := convoCoord(t)
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha", "Beta"}, "- clean"), "Plan.")
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Beta"}, "- clean"), "Go on.")
	if got := r.task(1).Prompt; !strings.Contains(got, "No longer in your context: Alpha [worker]") {
		t.Fatalf("the dropped report was not named:\n%s", got)
	}
}

// A repair turn re-sends the same context. It must cost a line, not a second copy.
func TestARepairTurnSendsNoContextAgain(t *testing.T) {
	c, r, p := convoCoord(t)
	parts := convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean")
	primaryTurnOf(t, c, p, parts, "Plan.")
	primaryTurnOf(t, c, p, parts, "Previous output failed validation: x. Return corrected JSON only.")
	got := r.task(1).Prompt
	if !strings.Contains(got, "Nothing in your context changed") || strings.Contains(got, "full report of Alpha") {
		t.Fatalf("the repair turn:\n%s", got)
	}
}

func TestTheConversationStartsOverWhenItsPromptOrSizeCallsForIt(t *testing.T) {
	c, r, p := convoCoord(t)
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.")

	// A changed role prompt cannot be applied to a running process.
	primaryTurnOf(t, c, p, convoParts("OTHER STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.")
	if starts, closes, _ := r.count(); starts != 2 || closes != 1 {
		t.Fatalf("after a changed system prompt: starts=%d closes=%d, want 2 and 1", starts, closes)
	}
	if !strings.Contains(r.task(1).Prompt, "full report of Alpha") {
		t.Fatalf("the new conversation did not get the whole context:\n%s", r.task(1).Prompt)
	}

	// Grown far past a fresh context.
	r.mu.Lock()
	r.context = 400000
	r.mu.Unlock()
	primaryTurnOf(t, c, p, convoParts("OTHER STATIC", "Build X", []string{"Alpha"}, "- clean"), "Go on.")
	primaryTurnOf(t, c, p, convoParts("OTHER STATIC", "Build X", []string{"Alpha"}, "- clean"), "Go on.")
	if starts, _, _ := r.count(); starts != 3 {
		t.Fatalf("after the conversation outgrew a fresh context: starts=%d, want 3", starts)
	}
}

func TestAColdConversationBiggerThanAFreshOneStartsOver(t *testing.T) {
	c, r, p := convoCoord(t)
	r.context = 90000
	parts := convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean")
	primaryTurnOf(t, c, p, parts, "Plan.")
	c.runtimeMu.Lock()
	cv := c.runtimeSessions[p.ID].convo
	cv.first, cv.tokens = 3000, 90000 // the first request was small, so a fresh one is small too
	cv.sentAt = time.Now().Add(-10 * time.Minute)
	c.runtimeMu.Unlock()
	primaryTurnOf(t, c, p, parts, "Go on.")
	if starts, _, _ := r.count(); starts != 2 {
		t.Fatalf("a cold, grown conversation continued: starts=%d, want 2", starts)
	}
}

// If the process behind the conversation died while it sat idle, the turn starts a new one with the whole context.
func TestADeadConversationIsReplacedWithoutLosingTheTurn(t *testing.T) {
	c, r, p := convoCoord(t)
	r.failSend = func(task int) bool { return task == 2 }
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.")
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha", "Beta"}, "- changed"), "Go on.")
	starts, closes, tasks := r.count()
	if starts != 2 || closes < 1 || tasks != 3 {
		t.Fatalf("starts=%d closes=%d tasks=%d, want a second conversation after the dead one was closed", starts, closes, tasks)
	}
	if got := r.task(2).Prompt; !strings.Contains(got, "full report of Alpha") || !strings.Contains(got, "Beta") {
		t.Fatalf("the replacement conversation did not get the whole context:\n%s", got)
	}
}

func TestAnIdleConversationIsClosedAndABusyOneIsNot(t *testing.T) {
	old := primaryIdleCloseAfter
	primaryIdleCloseAfter = 40 * time.Millisecond
	t.Cleanup(func() { primaryIdleCloseAfter = old })
	c, r, p := convoCoord(t)
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, closes, _ := r.count(); closes == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle conversation was never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.runtimeMu.Lock()
	left := len(c.runtimeSessions)
	c.runtimeMu.Unlock()
	if left != 0 {
		t.Fatalf("%d sessions still tracked after the idle close", left)
	}
}

// Asking for a clean slate closes the conversation, so the next turn sends the rebuilt context whole.
func TestRefreshSelfStartsANewConversation(t *testing.T) {
	c, r, p := convoCoord(t)
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.")
	c.honorRefreshSelf(p.ID, Turn{RefreshSelf: true})
	if _, closes, _ := r.count(); closes != 1 {
		t.Fatalf("closes=%d after refreshSelf, want 1", closes)
	}
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.")
	if starts, _, _ := r.count(); starts != 2 {
		t.Fatalf("starts=%d, want a new conversation", starts)
	}
}

// Every other agent keeps the one-generation session it always had.
func TestAWorkersSessionStillEndsWithItsGeneration(t *testing.T) {
	c, r, _ := convoCoord(t)
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: c.Current().Workspace, Model: "mock/w"})
	c.Engine.Bind(w.ID, "", string(agent.Worker), 12000)
	parts := convoParts("STATIC", "Do it", []string{"Alpha"}, "- clean")
	if _, err := c.runAgent(context.Background(), w, parts, "Go.", prompt.ReportSchema); err != nil {
		t.Fatal(err)
	}
	c.runtimeMu.Lock()
	cv := c.runtimeSessions[w.ID].convo
	c.runtimeMu.Unlock()
	if cv != nil {
		t.Fatal("a worker was given a continuing conversation")
	}
	if got := r.task(0).System; !strings.Contains(got, "Objective") {
		t.Fatalf("a worker's system prompt lost its stable context:\n%s", got)
	}
}

// A conversation whose very first send failed is not kept: the next turn starts a working one.
func TestAFailedFirstSendDoesNotLeaveADeadConversation(t *testing.T) {
	c, r, p := convoCoord(t)
	r.failSend = func(task int) bool { return task == 1 }
	if _, err := c.runAgent(context.Background(), p, convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.", prompt.PrimarySchemaFor(false)); err == nil {
		t.Fatal("the failed send was not reported")
	}
	c.runtimeMu.Lock()
	left := len(c.runtimeSessions)
	c.runtimeMu.Unlock()
	if left != 0 {
		t.Fatalf("%d sessions kept after a failed first send", left)
	}
	primaryTurnOf(t, c, p, convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean"), "Plan.")
	if starts, _, tasks := r.count(); starts != 2 || tasks != 2 {
		t.Fatalf("starts=%d tasks=%d, want a second conversation that worked", starts, tasks)
	}
}

// The output contract is part of the system prompt a running process keeps, so a changed schema starts a new conversation.
func TestAChangedOutputContractStartsANewConversation(t *testing.T) {
	c, r, p := convoCoord(t)
	parts := convoParts("STATIC", "Build X", []string{"Alpha"}, "- clean")
	if _, err := c.runAgent(context.Background(), p, parts, "Plan.", prompt.PrimarySchemaFor(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.runAgent(context.Background(), p, parts, "Plan.", prompt.PrimarySchemaFor(true)); err != nil {
		t.Fatal(err)
	}
	if starts, closes, _ := r.count(); starts != 2 || closes != 1 {
		t.Fatalf("starts=%d closes=%d, want the old conversation closed and a new one started", starts, closes)
	}
}
