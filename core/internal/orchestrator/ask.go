package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
)

// The Primary plans without reading code, so a fact about the code reaches it in one of three ways: Relo measures it
// (workspaceFacts, the repository map), a worker reports it, or the Primary asks. An ask is answered by a reader agent
// that does read, and what comes back is short, cited, and checked: Relo opens every file the reader cites and says
// which citations hold. The Primary then builds on the answer and lists it in the task's assumes, so the worker that
// implements it checks it again. A reader that was wrong is caught there, before anything is built on it.
const (
	askMaxPerTurn = 4 // fresh asks in one Primary turn; repeats are cached and free

	readerTimeout  = 4 * time.Minute // how long the reader itself may take
	askQuestionMax = 600
	askRecentKept  = 8
	askRememberMax = 420 // runes of one answer kept in the Primary's context
	citedFileMax   = 4 << 20
)

// askWait is how long one tool call waits for the reader, under the tool server's write timeout. A variable so tests
// can shorten it.
var askWait = 50 * time.Second

type askEntry struct {
	question string
	done     chan struct{}
	result   string // what the Primary is shown; set before done closes
	err      error
}

// askState is the Primary's asks for the current session.
type askState struct {
	mu      sync.Mutex
	session string
	entries map[string]*askEntry
	recent  []string
	turn    int
}

// forSession forgets every ask when the session changes. The caller holds mu.
func (s *askState) forSession(id string) {
	if s.session != id {
		s.session, s.entries, s.recent, s.turn = id, map[string]*askEntry{}, nil, 0
	}
}

// newAskTurn gives the Primary a fresh budget of asks. Called when a Primary turn starts.
func (c *Coord) newAskTurn() {
	c.asks.mu.Lock()
	c.asks.turn = 0
	c.asks.mu.Unlock()
}

// treeKey identifies the tree an answer was read from, so a cached answer is not served once the files changed.
func (c *Coord) treeKey() string {
	if ws := c.workspace(); ws != nil {
		base := ws.Base()
		return base.Commit + "|" + base.Head
	}
	return ""
}

func askKey(question, tree string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(question), " ")) + "\x00" + tree))
	return hex.EncodeToString(sum[:8])
}

// ask is the Primary's relo_agents ask: a question about the code, answered by a reader. It waits up to askWait for
// the answer. A slower reader keeps working and the same question, asked again, collects it.
func (c *Coord) ask(ctx context.Context, asker agent.Agent, question string) (any, error) {
	if asker.Role != agent.Primary {
		return nil, fmt.Errorf("only the Primary asks the reader; message the Primary, or read the files yourself")
	}
	question = strings.TrimSpace(question)
	if question == "" || len(question) > askQuestionMax {
		return nil, fmt.Errorf("ask needs a question of 1 to %d bytes in text", askQuestionMax)
	}
	s := c.Current()
	if s == nil {
		return nil, fmt.Errorf("no session")
	}
	key := askKey(question, c.treeKey())
	c.asks.mu.Lock()
	c.asks.forSession(s.ID)
	e := c.asks.entries[key]
	if e == nil {
		if c.asks.turn >= askMaxPerTurn {
			c.asks.mu.Unlock()
			return nil, fmt.Errorf("you have asked %d questions this turn. Plan with what you have: hand the open ones to the workers in assumes, or delegate a probe", askMaxPerTurn)
		}
		c.asks.turn++
		e = &askEntry{question: question, done: make(chan struct{})}
		c.asks.entries[key] = e
		go c.readFor(s, key, e)
	}
	c.asks.mu.Unlock()

	select {
	case <-e.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(askWait):
		return map[string]any{"status": "still reading", "note": "The reader has not finished. Ask the same question again to collect its answer; that does not count against your asks."}, nil
	}
	if e.err != nil {
		c.asks.mu.Lock()
		if c.asks.entries[key] == e {
			delete(c.asks.entries, key) // a failed read is not an answer; asking again tries again
		}
		c.asks.mu.Unlock()
		return nil, e.err
	}
	return map[string]any{"answer": e.result}, nil
}

// readFor runs the reader for one question and records its answer in the Primary's context.
func (c *Coord) readFor(s *Session, key string, e *askEntry) {
	defer close(e.done)
	c.mu.Lock()
	parent := c.sessionCtx
	c.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, readerTimeout)
	defer cancel()

	settings := c.settings()
	model := c.modelForTask(agent.Probe, routing.Medium, false)
	title, _ := clipText("Reading for the Primary: "+e.question, 80)
	a := c.Agents.Create(agent.Agent{Role: agent.Probe, Title: title, Status: agent.Running, Model: model, Scope: "ask", Workspace: s.Workspace,
		Budget: settings.BudgetFor(model, settings.Budget), Window: settings.Window(model)})
	c.Engine.Bind(a.ID, "", string(agent.Probe), a.Budget)
	c.emit("agent.created", a)

	body, err := c.runAgent(ctx, a, prompt.Parts{Static: prompt.Reader}, e.question, prompt.ReaderSchema)
	c.closeAgentSession(a.ID)
	if cur := c.Current(); err == nil && (cur == nil || cur.ID != s.ID) {
		err = fmt.Errorf("the session changed while it read")
	}
	if err != nil {
		e.err = fmt.Errorf("the reader failed: %v. Delegate a probe task instead", err)
		c.finishReader(a, agent.Failed, "Failed")
		return
	}
	answer, perr := ParseJSON[readerAnswer](body)
	if perr != nil || strings.TrimSpace(answer.Answer) == "" {
		// A reader that answered in prose still answered; it just has nothing Relo can check.
		answer = readerAnswer{Answer: strings.TrimSpace(body), Found: true}
	}
	e.result = c.checkedAnswer(s.Workspace, answer)
	c.finishReader(a, agent.Completed, "Answered")
	c.rememberAnswer(e.question, e.result)
}

type readerAnswer struct {
	Answer    string `json:"answer"`
	Found     bool   `json:"found"`
	Citations []struct {
		Path   string `json:"path"`
		Line   int    `json:"line"`
		Symbol string `json:"symbol"`
	} `json:"citations"`
}

func (c *Coord) finishReader(a agent.Agent, status agent.Status, label string) {
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status, x.Activity = status, label })
	if status == agent.Completed {
		c.emit("agent.completed", c.freshAgent(a))
	} else {
		c.emit("agent.failed", c.freshAgent(a))
	}
}

// checkedAnswer is the answer the Primary reads: the reader's text, then which of its citations Relo found in the
// files. It is the guard against a cheap reader that invents a path or a line.
func (c *Coord) checkedAnswer(root string, r readerAnswer) string {
	text, _ := clipText(strings.TrimSpace(r.Answer), 1400)
	var held, failed []string
	for _, cite := range r.Citations {
		label := fmt.Sprintf("%s:%d", cite.Path, cite.Line)
		if cite.Symbol != "" {
			label += " " + cite.Symbol
		}
		if citationHolds(root, cite.Path, cite.Line, cite.Symbol) {
			held = append(held, label)
		} else {
			failed = append(failed, label)
		}
	}
	var b strings.Builder
	b.WriteString(text)
	if !r.Found {
		b.WriteString("\n\nThe reader did not find what you asked about.")
	}
	switch {
	case len(held) > 0:
		b.WriteString("\n\nCitations Relo found in the files: " + strings.Join(held, "; ") + ".")
	case len(failed) == 0:
		b.WriteString("\n\nNo citations: nothing here was checked. Treat it as a hint, and list it in assumes.")
	}
	if len(failed) > 0 {
		b.WriteString("\nCitations Relo could not find (the reader may be wrong about these): " + strings.Join(failed, "; ") + ".")
	}
	return b.String()
}

// citationHolds reports whether rel is a file in the workspace with at least line lines, and, when symbol is given,
// mentions it. Paths that leave the workspace never hold.
func citationHolds(root, rel string, line int, symbol string) bool {
	if root == "" || rel == "" || line < 1 || filepath.IsAbs(rel) {
		return false
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	full := filepath.Join(root, clean)
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || info.Size() > citedFileMax {
		return false
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return false
	}
	if bytes.Count(data, []byte("\n"))+1 < line {
		return false
	}
	symbol = strings.TrimSpace(symbol)
	return symbol == "" || bytes.Contains(data, []byte(symbol))
}

// rememberAnswer keeps the latest answers in the Primary's context, so a refresh does not make it ask again.
func (c *Coord) rememberAnswer(question, result string) {
	short, _ := clipText(result, askRememberMax)
	c.asks.mu.Lock()
	c.asks.recent = append(c.asks.recent, "Q: "+question+"\nA: "+short)
	if len(c.asks.recent) > askRecentKept {
		c.asks.recent = c.asks.recent[len(c.asks.recent)-askRecentKept:]
	}
	recent := strings.Join(c.asks.recent, "\n\n")
	c.asks.mu.Unlock()
	if p, ok := c.Agents.Primary(); ok {
		c.Engine.SetTaskState(p.ID, "Answers from the code reader", []ctxeng.Item{{Source: ctxeng.SourceWorker, Content: recent + "\n(Relayed, not measured. Hand what a task builds on to its assumes.)", Durability: 0.9}})
	}
}
