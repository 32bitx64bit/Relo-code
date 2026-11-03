package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The Primary used to start a new conversation every turn: its session was closed after each one and its whole
// curated context (about 26k tokens) was sent again as one message, so the provider wrote all of it to its cache
// every time. In a Pixels at War run that was 77% of the Primary's cost.
//
// Now one conversation continues across turns. The first message carries the whole context; every later one carries
// what changed since the last, and the provider reads the rest from cache at a tenth of the price. The curated
// context is still what the Primary reasons from: the engine decides what is in it each turn, and this file only
// decides how much of it has to be sent again.
//
// A conversation is started over (the old one closed, the whole context sent) when continuing it would cost more:
// it has grown well past a fresh context, its cache has gone cold and rewriting it would cost more than a fresh one,
// the Primary's prompt or model changed, or the Primary asked for a clean slate.
const (
	// primaryWarm is how long after its last request a conversation's cache is still warm. Providers keep a prefix for
	// five minutes after its last use.
	primaryWarm = 4 * time.Minute
	// primaryIdleClose closes the Claude process of a conversation nobody has used for this long.
	primaryIdleClose = 10 * time.Minute
	// primaryMaxTurns bounds how long one conversation runs, whatever its size.
	primaryMaxTurns = 60
	// primaryGrowth is how many times the size of a fresh context a conversation may reach, plus primaryGrowthSlack
	// tokens, before it is started over.
	primaryGrowth      = 2.0
	primaryGrowthSlack = 6000
	// droppedListMax bounds the names listed as no longer in the context.
	droppedListMax = 30
)

// primaryConvo is what Relo remembers of the Primary's live conversation: what it has been sent and how big it is.
// Guarded by runtimeMu, like the session that holds it.
type primaryConvo struct {
	blocks   map[string]block // context block key -> what was last sent
	static   string           // hash of the system prompt the process was started with
	turns    int
	sentAt   time.Time // when the conversation was last used
	busy     bool      // a turn is running
	tokens   int       // the prompt size of the latest request, which is the conversation's size
	first    int       // the prompt size of the first request
	firstEst int       // Relo's estimate of the first message, to tell the runtime's own overhead from it
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// estimateTokens is a rough count for deciding sizes, not for billing.
func estimateTokens(s string) int { return len(s) / 4 }

// overhead is what the runtime adds to every request beyond Relo's own text: its tool definitions and system frame.
func (cv *primaryConvo) overhead() int {
	return max(0, cv.first-cv.firstEst)
}

// shouldStartOver reports whether continuing the conversation would cost more than a fresh one holding fullTokens of
// context, or is not possible at all.
func (cv *primaryConvo) shouldStartOver(static string, fullTokens int, now time.Time) bool {
	if cv == nil {
		return true
	}
	if cv.turns == 0 {
		return false
	}
	if cv.static != static || cv.turns >= primaryMaxTurns {
		return true
	}
	fresh := fullTokens + cv.overhead()
	if cv.tokens <= 0 {
		return false
	}
	if float64(cv.tokens) > primaryGrowth*float64(fresh)+primaryGrowthSlack {
		return true
	}
	// A cold cache is rewritten whole whichever way the turn goes; a conversation bigger than a fresh context costs
	// more to rewrite than the fresh context does.
	if now.Sub(cv.sentAt) > primaryWarm && float64(cv.tokens) > 1.05*float64(fresh)+500 {
		return true
	}
	return false
}

// block is one piece of rendered context: an item with its heading, or a section heading.
type block struct {
	key  string // section, heading and occurrence, so the same heading twice stays two blocks
	head string // the heading line, for naming a block that went away
	text string
}

// splitBlocks cuts rendered context into blocks at its markdown headings, leaving headings inside code fences alone.
// section keeps the stable and dynamic halves apart, since the same heading may appear in both.
func splitBlocks(section, text string) []block {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var blocks []block
	seen := map[string]int{}
	var head string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		h := head
		if h == "" {
			h = "(start)"
		}
		n := seen[h]
		seen[h]++
		blocks = append(blocks, block{key: fmt.Sprintf("%s|%s|%d", section, h, n), head: h, text: b.String()})
		b.Reset()
	}
	inFence := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "#") {
			if rest := strings.TrimLeft(line, "#"); strings.HasPrefix(rest, " ") && len(line)-len(rest) <= 3 {
				flush()
				head = strings.TrimSpace(line)
			}
		}
		b.WriteString(line)
	}
	flush()
	return blocks
}

// contextBlocks is the item-rendered part of a Primary prompt as blocks, stable half first.
func contextBlocks(stable, body string) []block {
	return append(splitBlocks("stable", stable), splitBlocks("body", body)...)
}

// blockMap indexes blocks by key.
func blockMap(blocks []block) map[string]block {
	m := make(map[string]block, len(blocks))
	for _, b := range blocks {
		m[b.key] = b
	}
	return m
}

// diffBlocks reports what a conversation that has been sent prev must be sent to hold cur: the blocks that are new or
// changed, and the headings of the items that are gone. Section headings (one or two #) are structure, not items.
func diffBlocks(prev map[string]block, cur []block) (changed []block, gone []string) {
	now := make(map[string]bool, len(cur))
	heads := make(map[string]bool, len(cur))
	for _, b := range cur {
		now[b.key] = true
		heads[b.head] = true
		if old, ok := prev[b.key]; !ok || old.text != b.text {
			changed = append(changed, b)
		}
	}
	keys := make([]string, 0, len(prev))
	for k := range prev {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	for _, k := range keys {
		head := prev[k].head
		if now[k] || heads[head] || !strings.HasPrefix(head, "### ") || seen[head] {
			continue
		}
		seen[head] = true
		gone = append(gone, strings.TrimSpace(strings.TrimLeft(head, "#")))
	}
	return changed, gone
}

// contextDelta renders what changed as the message that continues the conversation.
func contextDelta(changed []block, gone []string) string {
	var b strings.Builder
	if len(changed) == 0 && len(gone) == 0 {
		b.WriteString("## Context\nNothing in your context changed since your last turn.\n")
		return b.String()
	}
	b.WriteString("## Context changes since your last turn\nOnly what changed is below; everything else you were given earlier still stands. A block here replaces the earlier block with the same heading. The workspace facts and notes after it are current and outrank anything said earlier.\n\n")
	for _, c := range changed {
		b.WriteString(strings.TrimRight(c.text, "\n"))
		b.WriteString("\n\n")
	}
	if len(gone) > 0 {
		shown := gone
		if len(shown) > droppedListMax {
			shown = shown[:droppedListMax]
		}
		b.WriteString("No longer in your context: " + strings.Join(shown, "; "))
		if len(gone) > len(shown) {
			b.WriteString(fmt.Sprintf("; and %d more", len(gone)-len(shown)))
		}
		b.WriteString(".\n")
	}
	return b.String()
}
