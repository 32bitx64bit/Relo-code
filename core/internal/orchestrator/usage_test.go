package orchestrator

import (
	"context"
	"testing"

	"github.com/relocode/relo/internal/agent"
	rt "github.com/relocode/relo/internal/runtime"
)

// Input/Output add up over every provider call of a turn, so a turn with several tool steps reports far more
// than the window ever held. The context figure must come from the last call only.
func TestContextFillIsLastCallNotRunningTotal(t *testing.T) {
	turn := func(total rt.Usage) rt.Event {
		return rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: turnJSON, Usage: &total}
	}
	c, _ := newScripted(turn(rt.Usage{Input: 90_000, Output: 3_000, Context: 11_000}))
	c.UpdateSettings(func(s *Settings) {
		s.PrimaryModel = "p/big"
		s.ContextLimit = 300_000
		s.Models = map[string]rt.Model{"p/big": {ID: "big", Provider: "p", Context: 1_000_000}}
	})
	c.CreateSession(t.TempDir(), "", ModeBuild)
	if err := c.Submit(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	p, _ := c.Agents.Primary()
	if p.ContextTokens != 11_000 || p.Window != 300_000 {
		t.Fatalf("context = %d / %d, want 11000 / 300000", p.ContextTokens, p.Window)
	}
	if p.UsedTokens != 93_000 {
		t.Fatalf("spent = %d, want the 93000 the provider billed", p.UsedTokens)
	}
	if p.Status != agent.Completed {
		t.Fatalf("primary = %s", p.Status)
	}
}

func TestContextFallsBackToUsageSumWhenRuntimeOmitsIt(t *testing.T) {
	c, _ := newScripted(rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: turnJSON, Usage: &rt.Usage{Input: 500, Output: 40, CacheRead: 8_000, CacheWrite: 100}})
	c.UpdateSettings(func(s *Settings) { s.PrimaryModel = "p/blind" })
	c.CreateSession(t.TempDir(), "", ModeBuild)
	if err := c.Submit(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	p, _ := c.Agents.Primary()
	if p.ContextTokens != 8_600 {
		t.Fatalf("context = %d, want input + cached = 8600 (output excluded)", p.ContextTokens)
	}
	if p.Window != 0 {
		t.Fatalf("window = %d, want 0 (unknown) with no cap and no model limit", p.Window)
	}
}

func TestTerminalTotalsDoNotOverwriteLatestToolStepContext(t *testing.T) {
	for _, terminalContext := range []int{0, 115_000} {
		c, _ := newScripted(
			rt.Event{Type: "session.step.ended", ContextTokens: 90_000, MoreSteps: true},
			rt.Event{Type: "session.step.ended", ContextTokens: 110_000},
			rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: turnJSON,
				Usage: &rt.Usage{Input: 20_000, CacheRead: 180_000, Output: 5_000, Context: terminalContext}},
		)
		c.CreateSession(t.TempDir(), "", ModeBuild)
		if err := c.Submit(context.Background(), "hi", nil); err != nil {
			t.Fatal(err)
		}
		want := terminalContext
		if want == 0 {
			want = 110_000
		}
		p, _ := c.Agents.Primary()
		if p.ContextTokens != want {
			t.Fatalf("context = %d, want latest request %d, not cumulative usage", p.ContextTokens, want)
		}
		if got := c.CacheSnapshot().Session.Usage; got.Input != 20_000 || got.CacheRead != 180_000 || got.Output != 5_000 {
			t.Fatalf("turn usage = %+v, want cumulative billing unchanged", got)
		}
	}
}

// The terminal event repeats the whole turn's text. It used to be appended to the streamed deltas, so every reply
// that streamed showed up twice in the chat.
func TestReplyIsNotDuplicatedByTheTerminalEvent(t *testing.T) {
	const full = "Let me look.\n\nHere is the answer."
	c, _ := newScripted(
		rt.Event{Type: "session.text.delta", Phase: "writing", Delta: "Let me look."},
		rt.Event{Type: "session.text.delta", Phase: "writing", Delta: "Here is the answer."},
		rt.Event{Type: "session.idle", Done: true, Idle: true, Text: full},
	)
	c.CreateSession(t.TempDir(), "", ModeBuild)
	if err := c.Submit(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	var replies []string
	for _, m := range c.Current().Messages {
		if m.Role == "assistant" {
			replies = append(replies, m.Text)
		}
	}
	if len(replies) != 1 || replies[0] != full {
		t.Fatalf("assistant messages = %q, want the text once, as one reply with a paragraph break", replies)
	}
}
