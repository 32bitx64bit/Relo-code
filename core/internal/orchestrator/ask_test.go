package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
)

// readerRuntime plays the code reader: it counts the questions it is sent and answers each with scripted JSON.
type readerRuntime struct {
	*runtime.Mock
	events chan runtime.Event
	reply  func(question string) string
	calls  atomic.Int32
	delay  time.Duration
}

func (r *readerRuntime) StartSession(context.Context, runtime.SessionConfig) (runtime.SessionID, error) {
	r.events = make(chan runtime.Event, 1)
	return "reader", nil
}
func (r *readerRuntime) Events(context.Context, runtime.SessionID) (<-chan runtime.Event, error) {
	return r.events, nil
}
func (r *readerRuntime) SendTask(_ context.Context, _ runtime.SessionID, task runtime.AgentTask) error {
	r.calls.Add(1)
	go func() {
		time.Sleep(r.delay)
		r.events <- runtime.Event{Done: true, JSON: r.reply(task.Prompt)}
	}()
	return nil
}
func (r *readerRuntime) CloseSession(context.Context, runtime.SessionID) error { return nil }

func readerCoord(t *testing.T, r *readerRuntime) (*Coord, agent.Agent, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "engine", "facts.c"), []byte("#include <x.h>\nint ai_facts_military(snap *s, out *o)\n{\n\treturn 0;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Mock = runtime.NewMock()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(r)
	c.CreateSession(dir, "plan it", ModeBuild)
	c.Settings.PrimaryModel, c.Settings.Tier1Model, c.Settings.Tier2Model = "mock/p", "mock/t1", "mock/t2"
	c.Route = routing.Config{Primary: "mock/p", Tier1: "mock/t1", Tier2: "mock/t2"}
	p := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: dir, Model: "mock/p"})
	c.Engine.Bind(p.ID, "", string(agent.Primary), 24000)
	return c, p, dir
}

func readerJSON(answer string, found bool, cites ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"answer": answer, "found": found, "citations": cites})
	return string(b)
}

func TestACitationHoldsOnlyWhenTheFileHasTheLineAndTheSymbol(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.c"), []byte("one\ntwo\nint ai_facts_military(void)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rel    string
		line   int
		symbol string
		want   bool
	}{
		{"a.c", 3, "ai_facts_military", true},
		{"a.c", 1, "", true},
		{"a.c", 90, "", false},
		{"a.c", 3, "ai_facts_naval", false},
		{"missing.c", 1, "", false},
		{"../a.c", 1, "", false},
		{"/etc/passwd", 1, "", false},
		{"a.c", 0, "", false},
	} {
		if got := citationHolds(dir, tc.rel, tc.line, tc.symbol); got != tc.want {
			t.Errorf("citationHolds(%q, %d, %q) = %v, want %v", tc.rel, tc.line, tc.symbol, got, tc.want)
		}
	}
}

// The Primary is told which citations Relo found in the files and which it could not, because a cheap reader invents
// paths and lines.
func TestAnAnswerSaysWhichCitationsHold(t *testing.T) {
	r := &readerRuntime{reply: func(string) string {
		return readerJSON("ai_facts_military takes a snapshot and an out struct.", true,
			map[string]any{"path": "engine/facts.c", "line": 2, "symbol": "ai_facts_military"},
			map[string]any{"path": "engine/invented.c", "line": 7})
	}}
	c, p, _ := readerCoord(t, r)
	got, err := c.ask(context.Background(), p, "What does ai_facts_military take?")
	if err != nil {
		t.Fatal(err)
	}
	text := got.(map[string]any)["answer"].(string)
	for _, want := range []string{"takes a snapshot", "Citations Relo found in the files: engine/facts.c:2 ai_facts_military", "could not find", "engine/invented.c:7"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer lacks %q:\n%s", want, text)
		}
	}
}

// An answer with nothing checkable is marked as a hint, and asking again is free.
func TestARepeatedQuestionIsAnsweredFromTheCacheAndTheBudgetIsPerTurn(t *testing.T) {
	r := &readerRuntime{reply: func(string) string { return readerJSON("Not much to say.", true) }}
	c, p, _ := readerCoord(t, r)
	first, err := c.ask(context.Background(), p, "Where is the tick loop?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.(map[string]any)["answer"].(string), "nothing here was checked") {
		t.Fatalf("an uncited answer was not marked: %v", first)
	}
	if _, err := c.ask(context.Background(), p, "  where is the TICK loop? "); err != nil {
		t.Fatal(err)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("reader ran %d times for one question asked twice, want 1", n)
	}
	for i := 0; i < askMaxPerTurn-1; i++ {
		if _, err := c.ask(context.Background(), p, "another question "+string(rune('a'+i))); err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
	}
	if _, err := c.ask(context.Background(), p, "one question too many"); err == nil || !strings.Contains(err.Error(), "this turn") {
		t.Fatalf("a fifth fresh ask in one turn = %v, want a budget error", err)
	}
	c.newAskTurn()
	if _, err := c.ask(context.Background(), p, "one question too many"); err != nil {
		t.Fatalf("a new turn did not restore the budget: %v", err)
	}
}

func TestOnlyThePrimaryMayAsk(t *testing.T) {
	r := &readerRuntime{reply: func(string) string { return readerJSON("x", true) }}
	c, _, dir := readerCoord(t, r)
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: dir})
	if _, err := c.ask(context.Background(), w, "anything?"); err == nil {
		t.Fatal("a worker was allowed to ask")
	}
	if n := r.calls.Load(); n != 0 {
		t.Fatalf("the reader ran %d times for a refused ask", n)
	}
}

// A slow reader does not hold the tool call open past its window; the same question, asked again, collects the answer.
func TestASlowReaderIsCollectedByAskingAgain(t *testing.T) {
	old := askWait
	askWait = 40 * time.Millisecond
	t.Cleanup(func() { askWait = old })
	r := &readerRuntime{delay: 300 * time.Millisecond, reply: func(string) string {
		return readerJSON("It is in engine/facts.c.", true, map[string]any{"path": "engine/facts.c", "line": 1})
	}}
	c, p, _ := readerCoord(t, r)
	first, err := c.ask(context.Background(), p, "Where are the facts?")
	if err != nil {
		t.Fatal(err)
	}
	if first.(map[string]any)["status"] != "still reading" {
		t.Fatalf("first call = %v, want still reading", first)
	}
	askWait = 2 * time.Second
	second, err := c.ask(context.Background(), p, "Where are the facts?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.(map[string]any)["answer"].(string), "engine/facts.c") {
		t.Fatalf("second call = %v", second)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("reader ran %d times, want 1", n)
	}
}

// What the reader said stays in the Primary's context, so a context rebuild does not make it ask again.
func TestTheAnswerStaysInThePrimarysContext(t *testing.T) {
	r := &readerRuntime{reply: func(string) string {
		return readerJSON("The facts live in engine/facts.c.", true, map[string]any{"path": "engine/facts.c", "line": 1})
	}}
	c, p, _ := readerCoord(t, r)
	if _, err := c.ask(context.Background(), p, "Where do the facts live?"); err != nil {
		t.Fatal(err)
	}
	gen, err := c.Engine.Refresh(p.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range gen.Items {
		if it.Title == "Answers from the code reader" && strings.Contains(it.Content, "Where do the facts live?") && strings.Contains(it.Content, "engine/facts.c") {
			return
		}
	}
	t.Fatalf("no answer in the Primary's context: %+v", gen.Items)
}

// The ask reaches the reader through the messaging tool the Primary already has, bound to the Primary's own key.
func TestTheAskActionAnswersThroughTheMessagingTool(t *testing.T) {
	r := &readerRuntime{reply: func(string) string {
		return readerJSON("Flags are uint32_t.", true, map[string]any{"path": "engine/facts.c", "line": 2, "symbol": "ai_facts_military"})
	}}
	c, p, _ := readerCoord(t, r)
	c.runtimeSessions[p.ID] = &runtimeSession{id: "runtime-primary"}
	key, err := c.bindPeer(p, "runtime-primary")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "ask", Text: "What type are the flags?"})
	if err != nil {
		t.Fatal(err)
	}
	if text := got.(map[string]any)["answer"].(string); !strings.Contains(text, "uint32_t") || !strings.Contains(text, "engine/facts.c:2") {
		t.Fatalf("answer = %q", text)
	}
}
