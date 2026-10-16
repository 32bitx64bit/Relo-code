package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/opencode"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/provider"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

const fakeSummary = `## Objective
- explore

## Requirements
- (none)

## Decisions
- (none)

## Work State
### Completed
- (none)

### Active
- globbing

### Blocked
- (none)

## Next Move
1. keep going

## Relevant Files
- (none)

## Important Context
- (none)`

// fakeModel is an OpenAI-compatible endpoint whose "model" calls a tool for toolSteps calls and then answers. Each
// call reports a prompt of 1000 tokens per message in the request, so the conversation's size is visible to Relo
// exactly as a real provider would report it, and a summary makes it smaller.
// askLog notes in the model's trace when Relo asks for a summary.
type askLog struct {
	rt.AgentRuntime
	model *fakeModel
}

func (l askLog) Compact(ctx context.Context, s rt.SessionID) error {
	l.model.mu.Lock()
	l.model.trace = append(l.model.trace, "ASK")
	l.model.mu.Unlock()
	return l.AgentRuntime.(rt.Compactor).Compact(ctx, s)
}

type fakeModel struct {
	mu        sync.Mutex
	toolSteps int
	steps     int
	summaries int
	biggest   int // the largest prompt reported to a call that was not a summary
	trace     []string
}

func (m *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(b, &req)
	last := ""
	if n := len(req.Messages); n > 0 {
		last = fmt.Sprint(req.Messages[n-1].Content)
	}
	summary := strings.Contains(last, "summarize the conversation") || strings.Contains(last, "did not fill in the required summary")
	held := 1000 * len(req.Messages)
	m.mu.Lock()
	var n int
	if summary {
		m.summaries++
		held = 3000
		m.trace = append(m.trace, "SUMMARY")
	} else {
		m.trace = append(m.trace, fmt.Sprint(held/1000, "k"))
		m.steps++
		n = m.steps
		m.biggest = max(m.biggest, held)
	}
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(v map[string]any) { j, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", j) }
	choice := func(delta map[string]any, finish any) map[string]any {
		c := map[string]any{"index": 0, "delta": delta}
		if finish != nil {
			c["finish_reason"] = finish
		}
		return map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "m", "choices": []any{c}}
	}
	usage := map[string]any{"prompt_tokens": held, "completion_tokens": 50, "total_tokens": held + 50}
	withUsage := func(c map[string]any) map[string]any { c["usage"] = usage; return c }
	switch {
	case summary:
		chunk(choice(map[string]any{"role": "assistant", "content": fakeSummary}, nil))
		chunk(withUsage(choice(map[string]any{}, "stop")))
	case n <= m.toolSteps:
		call := map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", n), "type": "function", "function": map[string]any{"name": "glob", "arguments": `{"pattern":"*"}`}}
		chunk(choice(map[string]any{"role": "assistant", "tool_calls": []any{call}}, nil))
		chunk(withUsage(choice(map[string]any{}, "tool_calls")))
	default:
		chunk(choice(map[string]any{"role": "assistant", "content": `{"status":"completed","summary":"ok"}`}, nil))
		chunk(withUsage(choice(map[string]any{}, "stop")))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// runExploration drives one architect through 20 tool calls against the real OpenCode. The Relo limit is 20k tokens
// and the model's own window is a million, which is how a 300k limit ended up at 800k.
func runExploration(t *testing.T, compactAt float64) (*fakeModel, agent.Agent, *captured) {
	t.Helper()
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	model := &fakeModel{toolSteps: 20}
	srv := httptest.NewServer(model)
	defer srv.Close()

	ps := []provider.Provider{{ID: "fake", Name: "fake", Kind: provider.KindOpenAI, BaseURL: srv.URL + "/v1", Models: []provider.Model{{ID: "m", Context: 1_000_000, Output: 8000}}}}
	penv, err := provider.Env(ps, map[string]string{"fake": "k"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	work := t.TempDir()
	proc, err := opencode.Start(ctx, bin, work, append(opencode.Isolation(t.TempDir()), penv...))
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Stop()

	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(askLog{proc.Runtime(), model})
	k := &captured{}
	c.SetEmitter(k.emit)
	c.UpdateSettings(func(s *Settings) {
		s.ContextLimit = 20_000
		s.CompactAt = compactAt
		s.Models = map[string]rt.Model{"fake/m": {ID: "m", Provider: "fake", Context: 1_000_000, Output: 8000}}
	})
	s := c.CreateSession(work, "q", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "fake/m"})
	task := c.Graph.Add(taskgraph.Task{Title: "Explore", Role: string(agent.Architect), Workspace: s.Workspace, Model: "fake/m", Difficulty: "hard"})
	a := c.spawn(task, agent.Architect)
	body, err := c.runAgent(ctx, a, prompt.Parts{}, "explore", prompt.ReportSchema)
	if err != nil {
		t.Fatalf("the exploration failed: %v", err)
	}
	if !strings.Contains(body, "completed") {
		t.Fatalf("body = %q", body)
	}
	got, _ := c.Agents.Get(a.ID)
	return model, got, k
}

// The control: the behaviour the bug report describes. Nothing stops the conversation growing past the limit.
func TestWithoutCompactionTheContextRunsPastTheLimit(t *testing.T) {
	model, _, _ := runExploration(t, 0)
	if model.summaries != 0 {
		t.Fatalf("%d summaries with compaction off", model.summaries)
	}
	if model.biggest <= 20_000*2 {
		t.Fatalf("the largest call held %d tokens; the control is meant to show growth well past the 20k limit", model.biggest)
	}
}

func TestTheRealOpenCodeIsSummarizedAtNinetyPercentOfTheLimit(t *testing.T) {
	model, a, k := runExploration(t, 0.9)
	t.Logf("calls: %s", strings.Join(model.trace, " "))
	if model.summaries < 1 {
		t.Fatal("OpenCode was never asked to summarize")
	}
	// The request is steered: the summary is written at a step boundary, and the end of the step that crossed the line
	// reaches Relo after the next call has begun. So one or two calls land after the line, never a runaway. A step here
	// is a tenth of the window (a real one is a few percent), and without compaction the same run reaches 40k.
	if limit := 20_000 + 3*2_000; model.biggest > limit {
		t.Errorf("the largest call held %d tokens, over the %d that the limit and two late steps allow", model.biggest, limit)
	}
	waiting, late := false, 0
	for _, e := range model.trace {
		switch {
		case e == "ASK":
			waiting, late = true, 0
		case e == "SUMMARY":
			waiting = false
		case waiting:
			if late++; late > 3 {
				t.Fatalf("more than three calls between a request and its summary: %s", strings.Join(model.trace, " "))
			}
		}
	}
	notes := systemNotes(k)
	if !strings.Contains(notes, "Summarizing the conversation") || !strings.Contains(notes, "context summarized") {
		t.Errorf("the chat did not report the summary:\n%s", notes)
	}
	if a.Window != 20_000 || a.ContextTokens <= 0 || a.ContextTokens > 20_000 {
		t.Errorf("the chip shows %d / %d, want a size under the 20000 window", a.ContextTokens, a.Window)
	}
}
