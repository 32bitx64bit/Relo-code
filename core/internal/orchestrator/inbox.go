package orchestrator

import (
	"context"
	"strings"

	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/style"
)

// A user message sent while a request is running cannot interrupt the turn in flight: it waits in the session's
// inbox and is handed to the head agent at its next step. The inbox is keyed by session ID, so a message queued
// on one session is never delivered to another.

// queuedNote is one waiting user message.
type queuedNote struct {
	id   string
	text string
	refs []string
}

// enqueueLocked records a user message for the head's next step and returns it for announcing. Caller holds mu.
func (c *Coord) enqueueLocked(text string, refs []string) Chat {
	c.addChatLocked(Chat{Role: "user", Text: text, Queued: true})
	m := c.sess.Messages[len(c.sess.Messages)-1]
	sid := c.sess.ID
	c.inbox[sid] = append(c.inbox[sid], queuedNote{id: m.ID, text: text, refs: refs})
	return m
}

// takeQueued drains this session's waiting messages for one head step. Each delivered message loses its queued
// mark and is re-announced so the UI updates it. Another session's inbox is never touched.
func (c *Coord) takeQueued(sid string) (string, []string, bool) {
	c.mu.Lock()
	notes := c.inbox[sid]
	delete(c.inbox, sid)
	var lines, refs, ids []string
	for _, n := range notes {
		lines = append(lines, n.text)
		refs = append(refs, n.refs...)
		ids = append(ids, n.id)
	}
	updated := c.clearQueuedLocked(ids, false)
	c.mu.Unlock()
	for _, m := range updated {
		c.emit("chat.message", m)
	}
	return strings.Join(lines, "\n\n"), refs, len(notes) > 0
}

// queuedPending reports whether this session still has messages waiting for the head.
func (c *Coord) queuedPending(sid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inbox[sid]) > 0
}

// clearQueuedLocked clears the queued mark on the given messages and returns them. Caller holds mu.
func (c *Coord) clearQueuedLocked(ids []string, dropped bool) []Chat {
	if c.sess == nil || len(ids) == 0 {
		return nil
	}
	wanted := make(map[string]bool, len(ids))
	for _, want := range ids {
		wanted[want] = true
	}
	var updated []Chat
	for i := range c.sess.Messages {
		m := &c.sess.Messages[i]
		if m.Queued && wanted[m.ID] {
			m.Queued = false
			m.Dropped = dropped
			updated = append(updated, *m)
		}
	}
	return updated
}

// dropQueued empties every inbox, clears the queued marks and reports how many messages were dropped.
func (c *Coord) dropQueued() int {
	c.mu.Lock()
	var ids []string
	n := 0
	for sid, notes := range c.inbox {
		n += len(notes)
		delete(c.inbox, sid)
		if c.sess == nil || sid != c.sess.ID {
			continue
		}
		for _, note := range notes {
			ids = append(ids, note.id)
		}
	}
	updated := c.clearQueuedLocked(ids, true)
	c.mu.Unlock()
	for _, m := range updated {
		c.emit("chat.message", m)
	}
	return n
}

// clearQueuedMarks drops the queued mark from a session being installed. Queued is live state: nothing runs
// once a session is opened or restored, so no message there can still be waiting.
func clearQueuedMarks(s *Session) {
	if s == nil {
		return
	}
	for i := range s.Messages {
		if s.Messages[i].Queued {
			s.Messages[i].Dropped = true
		}
		s.Messages[i].Queued = false
	}
}

// wakeHead nudges a head that is waiting on its sub agents to step now.
func (c *Coord) wakeHead() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ingestGuidance puts delivered user guidance into the head's context, so a later context refresh keeps it.
func (c *Coord) ingestGuidance(primaryID, text string, refs []string) {
	c.ingestGuidanceWithSession(primaryID, text, refs, c.Current())
}

func (c *Coord) ingestGuidanceWithSession(primaryID, text string, refs []string, s *Session) {
	if s != nil {
		c.syncPrimaryConversation(primaryID, text, s.Messages)
	}
	// Retire the append-only title used by older generations; the transcript now supplies these instructions.
	c.Engine.SetTaskState(primaryID, "User guidance sent while Relo was working", nil)
	var items []ctxeng.Item
	for _, ref := range refs {
		items = append(items, ctxeng.Item{Class: ctxeng.ClassCode, Source: ctxeng.SourceRepo, Content: ref, Durability: 0.6})
	}
	c.Engine.Ingest(primaryID, items)
}

// stepQueued gives the head one turn on guidance that arrived while it waited on its sub agents. The sub agents
// are not cancelled and keep running: only the head steps.
func (c *Coord) stepQueued(ctx context.Context) {
	text, refs, ok := c.takeQueued(c.SessionID())
	if text == "" {
		return
	}
	p, ok := c.Agents.Primary()
	if !ok {
		return
	}
	c.ingestGuidance(p.ID, text, refs)
	settings := c.settings()
	gen, err := c.Engine.Refresh(p.ID, "user guidance arrived")
	if err != nil {
		c.logWarn("refresh primary for guidance", err)
		return
	}
	c.emit("context.compressed", map[string]any{"agentId": p.ID, "generation": gen.Number})
	resolved := style.Resolve(settings.Efficiency, p.Role, p.Difficulty, p.Attempt, settings.Budget)
	extra := c.workspaceFacts() + c.constraintsNote() + "\nThe user sent this while you were working. Fold it into the plan now, say briefly how you are handling it, and keep the sub agents going:\n" + text
	parts := prompt.ComposePartsWith(p.Role, *gen, extra, resolved.Profile.Fragment(p.Role))
	turn, err := c.primaryTurn(ctx, p, parts, "Incorporate the user's guidance and return the structured turn.")
	c.closeAgentSession(p.ID)
	if err != nil {
		c.logWarn("queued guidance turn", err)
		c.reportFailure(p, "Primary could not take your queued message", err)
		return
	}
	if turn.UserMessage != "" {
		c.chat("assistant", turn.UserMessage, p.ID)
	}
	c.applyTurn(p, turn)
	c.primaryWaits()
}
