package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/logging"
	rt "github.com/relocode/relo/internal/runtime"
)

// compactorRuntime plays a script like scriptRuntime and can also summarize the conversation on request.
type compactorRuntime struct {
	scriptRuntime
	cmu  sync.Mutex
	asks int
	err  error
}

func (r *compactorRuntime) Compact(context.Context, rt.SessionID) error {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	r.asks++
	return r.err
}

func (r *compactorRuntime) asked() int {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	return r.asks
}

// A 300k limit on a model that could hold a million: the situation that ran to 800k.
func newCompacting(t *testing.T, askErr error, tune func(*Settings), script ...rt.Event) (*Coord, *compactorRuntime, *captured) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	r := &compactorRuntime{scriptRuntime: scriptRuntime{dispatchRuntime: *newDispatchRuntime(), script: append(script, finishedTurn)}, err: askErr}
	c.SetRuntime(r)
	k := &captured{}
	c.SetEmitter(k.emit)
	c.UpdateSettings(func(s *Settings) {
		s.PrimaryModel = "p/big"
		s.ContextLimit = 300_000
		s.Models = map[string]rt.Model{"p/big": {ID: "big", Provider: "p", Context: 1_000_000}}
		if tune != nil {
			tune(s)
		}
	})
	return c, r, k
}

var finishedTurn = rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: turnJSON}

func stepEnd(held int, more bool) rt.Event {
	return rt.Event{Type: "session.step.ended", ContextTokens: held, MoreSteps: more}
}

func compaction(phase string) rt.Event {
	p := "model"
	if phase == "started" {
		p = "compacting"
	}
	return rt.Event{Type: "session.compaction." + phase, Phase: p, Compaction: phase}
}

func run(t *testing.T, c *Coord) {
	t.Helper()
	c.CreateSession(t.TempDir(), "", ModeBuild)
	if err := c.Submit(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
}

func systemNotes(k *captured) string {
	var out []string
	for _, m := range k.named("chat.message") {
		if chat := m.(Chat); chat.Role == "system" {
			out = append(out, chat.Text)
		}
	}
	return strings.Join(out, "\n")
}

func TestContextAtNinetyPercentOfTheLimitIsSummarizedOnce(t *testing.T) {
	c, r, k := newCompacting(t, nil, nil,
		stepEnd(120_000, true),
		stepEnd(271_000, true), // 90% of 300k is 270k
		stepEnd(274_000, true), // still waiting for the summary to start: no second request
		compaction("started"),
		compaction("ended"),
		stepEnd(41_000, true),
	)
	run(t, c)
	if got := r.asked(); got != 1 {
		t.Fatalf("summaries asked for = %d, want 1", got)
	}
	p := primaryOf(c)
	if p.ContextTokens != 41_000 || p.Window != 300_000 {
		t.Errorf("the chip shows %d / %d, want 41000 / 300000: it follows each model call, not just the end of the turn", p.ContextTokens, p.Window)
	}
	notes := systemNotes(k)
	if !strings.Contains(notes, "reached 271k of 300k tokens (90%)") || !strings.Contains(notes, "271k tokens down to 41k") {
		t.Errorf("the chat did not say what happened:\n%s", notes)
	}
}

// "Reaches 90%" includes 90%.
func TestExactlyTheShareCounts(t *testing.T) {
	c, r, _ := newCompacting(t, nil, nil, stepEnd(270_000, true))
	run(t, c)
	if got := r.asked(); got != 1 {
		t.Fatalf("asked for %d summaries at exactly 270000 of 300000", got)
	}
}

func TestBelowTheShareNothingIsSummarizedButTheSizeIsShown(t *testing.T) {
	c, r, _ := newCompacting(t, nil, nil, stepEnd(269_000, true))
	run(t, c)
	if got := r.asked(); got != 0 {
		t.Fatalf("asked for %d summaries at 89.7%%", got)
	}
	if p := primaryOf(c); p.ContextTokens != 269_000 {
		t.Errorf("context = %d, want the live 269000", p.ContextTokens)
	}
}

// The last step of a turn ends in plain text: the turn is over, and a summary asked for now would run after it.
func TestTheLastStepOfATurnIsNotSummarized(t *testing.T) {
	c, r, _ := newCompacting(t, nil, nil, stepEnd(290_000, false))
	run(t, c)
	if got := r.asked(); got != 0 {
		t.Fatalf("asked for %d summaries on the step that ended the turn", got)
	}
}

func TestCompactionCanBeSwitchedOff(t *testing.T) {
	c, r, _ := newCompacting(t, nil, func(s *Settings) { s.CompactAt = 0 }, stepEnd(299_000, true))
	run(t, c)
	if got := r.asked(); got != 0 {
		t.Fatalf("asked for %d summaries with relo.context.compactAt = 0", got)
	}
}

func TestTheShareIsConfigurable(t *testing.T) {
	c, r, _ := newCompacting(t, nil, func(s *Settings) { s.CompactAt = 0.5 }, stepEnd(149_000, true), stepEnd(150_000, true))
	run(t, c)
	if got := r.asked(); got != 1 {
		t.Fatalf("asked for %d summaries, want 1 (at 150k = 50%% of 300k)", got)
	}
}

// With no limit set and no model maximum there is no window to be 90% of, so nothing can be judged too big.
func TestAnUnknownWindowIsNeverSummarized(t *testing.T) {
	c, r, _ := newCompacting(t, nil, func(s *Settings) { s.ContextLimit = 0; s.Models = nil }, stepEnd(900_000, true))
	run(t, c)
	if got := r.asked(); got != 0 {
		t.Fatalf("asked for %d summaries without a known window", got)
	}
}

// A smaller model keeps its own maximum even under a bigger limit.
func TestTheModelsOwnSmallerWindowWins(t *testing.T) {
	c, r, _ := newCompacting(t, nil, func(s *Settings) {
		s.Models = map[string]rt.Model{"p/big": {ID: "big", Provider: "p", Context: 200_000}}
	}, stepEnd(179_000, true), stepEnd(181_000, true))
	run(t, c)
	if got := r.asked(); got != 1 {
		t.Fatalf("asked for %d summaries, want 1 (at 180k = 90%% of the model's 200k)", got)
	}
}

// If the summary still leaves the context over the line, asking again would burn a model call every step.
func TestASummaryThatDoesNotHelpIsNotRepeated(t *testing.T) {
	c, r, k := newCompacting(t, nil, nil,
		stepEnd(280_000, true),
		compaction("started"),
		compaction("ended"),
		stepEnd(285_000, true),
		stepEnd(290_000, true),
		stepEnd(295_000, true),
	)
	run(t, c)
	if got := r.asked(); got != 1 {
		t.Fatalf("asked for %d summaries, want 1", got)
	}
	if notes := systemNotes(k); !strings.Contains(notes, "did not help") {
		t.Errorf("nobody was told that the summary did not help:\n%s", notes)
	}
}

// OpenCode compacts by itself near the end of the model's window. While it is doing so Relo does not pile a
// second request on top.
func TestRelosRequestWaitsForTheRuntimesOwnSummary(t *testing.T) {
	c, r, k := newCompacting(t, nil, nil,
		compaction("started"),
		stepEnd(280_000, true),
		compaction("ended"),
		stepEnd(30_000, true),
	)
	run(t, c)
	if got := r.asked(); got != 0 {
		t.Fatalf("asked for %d summaries while the runtime was already writing one", got)
	}
	if notes := systemNotes(k); !strings.Contains(notes, "context summarized") {
		t.Errorf("a summary the runtime began was not reported:\n%s", notes)
	}
}

func TestARefusedRequestIsTriedTwiceThenLeftAlone(t *testing.T) {
	c, r, k := newCompacting(t, errors.New("409 a compaction is already queued"), nil,
		stepEnd(280_000, true), stepEnd(285_000, true), stepEnd(290_000, true), stepEnd(295_000, true))
	run(t, c)
	if got := r.asked(); got != refusalsMax {
		t.Fatalf("asked %d times, want %d", got, refusalsMax)
	}
	if notes := systemNotes(k); strings.Contains(notes, "Summarizing") {
		t.Errorf("the chat said a summary was under way when every request was refused:\n%s", notes)
	}
	if p := primaryOf(c); p.Status != "completed" {
		t.Errorf("the turn did not finish after the refusals: %s", p.Status)
	}
}

// A runtime without the capability (the mock) just carries on.
func TestARuntimeThatCannotCompactCarriesOn(t *testing.T) {
	c, k := newScripted(stepEnd(290_000, true), finishedTurn)
	c.UpdateSettings(func(s *Settings) {
		s.PrimaryModel = "p/big"
		s.ContextLimit = 300_000
		s.Models = map[string]rt.Model{"p/big": {ID: "big", Provider: "p", Context: 1_000_000}}
	})
	run(t, c)
	if notes := systemNotes(k); strings.Contains(notes, "Summarizing") {
		t.Errorf("claimed to summarize on a runtime that cannot:\n%s", notes)
	}
	if p := primaryOf(c); p.ContextTokens != 290_000 {
		t.Errorf("context = %d, want 290000", p.ContextTokens)
	}
}
