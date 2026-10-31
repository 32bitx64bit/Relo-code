package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/prompt"
	rt "github.com/relocode/relo/internal/runtime"
)

func loopEvent(id, name string, args map[string]any) rt.Event {
	return rt.Event{Type: "message.part.updated", PartType: "tool", Tool: name, ToolID: id,
		Raw: map[string]any{"part": map[string]any{"state": map[string]any{"status": "running", "input": args}}}}
}

func setLoopDetection(enabled bool) func(*Coord) {
	return func(c *Coord) {
		c.UpdateSettings(func(s *Settings) { s.LoopDetection = enabled })
	}
}

// hasChat reports whether a chat line containing text was shown to the user.
func hasChat(c *Coord, text string) bool {
	for _, m := range c.sess.Messages {
		if strings.Contains(m.Text, text) {
			return true
		}
	}
	return false
}

func TestLoopDetectorWindowAndReset(t *testing.T) {
	var d loopDetector
	call := func(i int, name string, args map[string]any) int {
		return d.observe(loopEvent(fmt.Sprint(i), name, args))
	}
	a := map[string]any{"path": "a", "offset": 1}
	for i := 0; i < 2; i++ {
		if got := call(i, "read", a); got != 0 {
			t.Fatalf("early hit %d", got)
		}
	}
	for i := 2; i < 9; i++ {
		if got := call(i, "read", map[string]any{"path": fmt.Sprint(i)}); got != 0 {
			t.Fatalf("distinct call hit %d", got)
		}
	}
	if got := call(9, "read", a); got != 0 {
		t.Fatalf("expired calls hit %d", got)
	}
	if got := call(10, "read", map[string]any{"offset": 1, "path": "a"}); got != 0 {
		t.Fatalf("early hit %d", got)
	}
	if got := call(11, "read", a); got != 1 {
		t.Fatalf("first hit = %d", got)
	}
	if got := d.observe(loopEvent("11", "read", a)); got != 0 {
		t.Fatalf("update counted twice: %d", got)
	}
	if got := call(12, "read", a); got != 0 {
		t.Fatalf("window not reset: %d", got)
	}
	if got := call(13, "read", a); got != 0 {
		t.Fatalf("window not reset: %d", got)
	}
	if got := call(14, "read", a); got != 2 {
		t.Fatalf("second hit = %d", got)
	}
}

func TestLoopDetectorRawArgumentsAndSplitEvents(t *testing.T) {
	var d loopDetector
	for i := 0; i < 3; i++ {
		id := fmt.Sprint(i)
		d.observe(rt.Event{Type: "session.tool.input.started", PartType: "tool", Tool: "read", ToolID: id})
		got := d.observe(rt.Event{Type: "session.tool.called", PartType: "tool", ToolID: id,
			Raw: map[string]any{"input": map[string]any{"path": "same", "offset": i}}})
		if got != 0 {
			t.Fatalf("different arguments hit %d", got)
		}
	}
	for i := 3; i < 6; i++ {
		id := fmt.Sprint(i)
		d.observe(rt.Event{Type: "session.tool.input.started", PartType: "tool", Tool: "read", ToolID: id})
		got := d.observe(rt.Event{Type: "session.tool.called", PartType: "tool", ToolID: id,
			Raw: map[string]any{"input": map[string]any{"path": "same", "offset": 99}}})
		want := 0
		if i == 5 {
			want = 1
		}
		if got != want {
			t.Fatalf("call %d hit %d, want %d", i, got, want)
		}
	}
}

func TestLoopFirstHitSteersOnceAndContinues(t *testing.T) {
	setLimits(t, time.Second, 0)
	first := []paced{}
	for i := 0; i < 3; i++ {
		first = append(first, paced{ev: loopEvent(fmt.Sprint(i), "read", map[string]any{"path": "a"})})
	}
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: [][]paced{first, {{ev: finished}}}}
	c, _, body, err := runTurns(t, r, setLoopDetection(true))
	if err != nil || body != finished.JSON {
		t.Fatalf("body=%q err=%v", body, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancels != 1 || len(r.tasks) != 2 || r.tasks[1].Prompt != loopNudge {
		t.Fatalf("cancels=%d tasks=%#v", r.cancels, r.tasks)
	}
	if !hasChat(c, "identical tool calls") {
		t.Fatalf("steering was not shown to the user: %#v", c.sess.Messages)
	}
}

func TestLoopDetectionInterruptsOnceAndReturnsPartialOutput(t *testing.T) {
	setLimits(t, time.Second, 0)
	first := []paced{{ev: rt.Event{Phase: "writing", Delta: "partial"}}}
	second := []paced{}
	for i := 0; i < 3; i++ {
		first = append(first, paced{ev: loopEvent(fmt.Sprintf("a%d", i), "read", map[string]any{"path": "a"})})
		second = append(second, paced{ev: loopEvent(fmt.Sprintf("b%d", i), "read", map[string]any{"path": "a"})})
	}
	r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: [][]paced{first, second}}
	c, a, body, err := runTurns(t, r, setLoopDetection(true))
	if err != nil || body != "partial" {
		t.Fatalf("second loop hit must yield the partial body: body=%q err=%v", body, err)
	}
	// The partial body is the task report, not an error that drops it.
	rep, err := c.workerReport(context.Background(), a, prompt.Parts{}, body)
	if err != nil || !strings.Contains(rep.Summary, "partial") {
		t.Fatalf("report summary=%q err=%v", rep.Summary, err)
	}
	if !hasChat(c, "kept repeating identical tool calls") {
		t.Fatalf("abort was not shown to the user: %#v", c.sess.Messages)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 2 || r.tasks[1].Prompt != loopNudge {
		t.Fatalf("tasks = %#v", r.tasks)
	}
}

// Repeating a call is legitimate sometimes, so detection is opt-in: the default and an explicit false both leave
// the run alone, however many identical calls arrive.
func TestLoopDetectionIsOffUnlessEnabled(t *testing.T) {
	cases := []struct {
		name    string
		options []func(*Coord)
	}{
		{name: "default"},
		{name: "explicitly disabled", options: []func(*Coord){setLoopDetection(false)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setLimits(t, time.Second, 0)
			script := []paced{}
			for i := 0; i < 6; i++ {
				script = append(script, paced{ev: loopEvent(fmt.Sprintf("a%d", i), "read", map[string]any{"path": "a"})})
			}
			script = append(script, paced{ev: finished})
			r := &turnsRuntime{dispatchRuntime: newDispatchRuntime(), confirm: true, turns: [][]paced{script}}
			c, _, body, err := runTurns(t, r, tc.options...)
			if err != nil || body != finished.JSON {
				t.Fatalf("body=%q err=%v", body, err)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.cancels != 0 || len(r.tasks) != 1 {
				t.Fatalf("cancels=%d tasks=%d, want no interrupt and one task", r.cancels, len(r.tasks))
			}
			if hasChat(c, "identical tool calls") || hasChat(c, "kept repeating") {
				t.Fatalf("loop detection warned while disabled: %#v", c.sess.Messages)
			}
		})
	}
}

// Claude Code sends a tool call's start with no arguments, then its complete input on the assistant message.
func TestLoopDetectorCountsClaudeCodeToolUseInput(t *testing.T) {
	var d loopDetector
	input := map[string]any{"file_path": "a.go"}
	for i := 0; i < 3; i++ {
		id := fmt.Sprint(i)
		if got := d.observe(rt.Event{Type: "session.tool", Phase: "tool", Tool: "read", ToolID: id}); got != 0 {
			t.Fatalf("start of call %d hit %d", i, got)
		}
		got := d.observe(rt.Event{Type: "session.tool", Phase: "tool", Tool: "read", ToolID: id, Raw: map[string]any{"input": input}})
		want := 0
		if i == 2 {
			want = 1
		}
		if got != want {
			t.Fatalf("call %d hit %d, want %d", i, got, want)
		}
	}
}
