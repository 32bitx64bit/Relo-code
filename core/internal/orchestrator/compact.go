package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/runtime"
)

// An agent's conversation grows with every model call and every tool result it carries. One ran to 800k tokens under
// a 300k limit: relo.models.contextLimit only sized Relo's own context pack, and OpenCode compacts against the
// model's full window, which can be several times larger. So Relo watches how much each model call held and, once
// that reaches relo.context.compactAt of the window, asks the runtime to summarize the conversation. The turn carries
// on from the summary in the same session, so the agent keeps its edits and its place.

// askTimeout bounds the request itself; the summary is written afterwards, at the next step boundary.
const askTimeout = 10 * time.Second

// refusalsMax is how many times in one turn the runtime may refuse the request before Relo stops asking.
const refusalsMax = 2

// contextWatch is what one turn remembers about the size of its context.
type contextWatch struct {
	compacting bool // a summary is being written, whoever asked for it
	justDone   bool // the summary is finished and the next measurement has not come yet
	givenUp    bool // asking again in this turn is refused or pointless
	refused    int
	last       int // the size at the latest measurement
	before     int // the size when the summary began
}

// watchContext keeps the agent's context size current and compacts it before it outgrows its window. It looks at
// every event but only acts on the ones that end a model step.
func (c *Coord) watchContext(ctx context.Context, a agent.Agent, rt runtime.AgentRuntime, sid runtime.SessionID, ev runtime.Event, st *runState) {
	w := &st.watch
	switch ev.Compaction {
	case "started":
		w.compacting = true
		if w.before == 0 {
			w.before = w.last
		}
	case "ended":
		w.compacting, w.justDone = false, true
	case "failed":
		w.compacting, w.before = false, 0
		c.logWarn("context compaction", fmt.Errorf("agent %s: the runtime could not summarize the conversation", a.ID))
	}
	held := ev.ContextTokens
	if ev.Type == "session.step.ended" && ev.Usage != nil {
		held = ev.Usage.PromptTokens()
	}
	if held <= 0 {
		return
	}
	w.last = held
	settings := c.settings()
	window := settings.Window(a.Model)
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.ContextTokens, x.Window = held, window })
	limit := settings.CompactAfter(a.Model)

	if w.justDone {
		// This is the first call made on the summary, so it says what the summary left.
		w.justDone = false
		before := w.before
		w.before = 0
		if limit > 0 && held >= limit {
			w.givenUp = true
			c.chat("system", fmt.Sprintf("%s: summarizing did not help, the context is still %s of %s tokens (%d%%). It will not be summarized again in this turn.", a.Title, tokensK(held), tokensK(window), held*100/window), a.ID)
			return
		}
		if before > 0 {
			c.chat("system", fmt.Sprintf("%s: context summarized, %s tokens down to %s.", a.Title, tokensK(before), tokensK(held)), a.ID)
		} else {
			c.chat("system", fmt.Sprintf("%s: context summarized, now %s tokens.", a.Title, tokensK(held)), a.ID)
		}
		return
	}
	// A step that ended in plain text is the end of the turn: there is nothing left to carry on from a summary.
	if limit <= 0 || held < limit || !ev.MoreSteps || w.compacting || w.givenUp {
		return
	}
	cp, ok := rt.(runtime.Compactor)
	if !ok {
		return
	}
	w.compacting, w.before = true, held
	cctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	if err := cp.Compact(cctx, sid); err != nil {
		w.compacting, w.before = false, 0
		if w.refused++; w.refused >= refusalsMax {
			w.givenUp = true
		}
		c.logWarn("compact context", fmt.Errorf("agent %s at %d of %d tokens: %w", a.ID, held, window, err))
		return
	}
	c.chat("system", fmt.Sprintf("%s: context reached %s of %s tokens (%d%%). Summarizing the conversation so far; the work carries on from the summary.", a.Title, tokensK(held), tokensK(window), held*100/window), a.ID)
}

// tokensK writes a token count the way the context chip does: 271k, 1200.
func tokensK(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return strconv.Itoa((n+500)/1000) + "k"
}
