package orchestrator

import (
	"fmt"
	"time"

	"github.com/relocode/relo/internal/taskgraph"
)

// familyFailureCap is how many failed or canceled dispatches one family of work may spend across task ids and
// reworded titles before Primary's next re-plan of it is refused and the family is blocked with one question. It
// matches the per-id budget: the first run plus taskgraph MaxAttempts (default 3) retries is 4 dispatches
// (taskgraph/graph.go:312-314). The per-id budget alone let the 'SPT stage 2' loop run 16 ids of one attempt each
// (session sess_b0abba4abea841d1d19d, 16 failed worker tasks, 2026-10-23 14:48:06Z-15:11:54Z).
const familyFailureCap = 4

// familySpend counts the failed or canceled dispatches of the work candidate names: failed or canceled tasks, and
// tasks still parked on a question, that are Related to it. Only spend after the family's last answered question
// counts, so an answer reopens the family with a fresh budget. It returns the count and the most recent such task,
// which carries the family's question.
func familySpend(graph []taskgraph.Task, candidate taskgraph.Task) (int, taskgraph.Task) {
	var since time.Time
	for _, t := range graph {
		if t.BlockedOn != nil && t.BlockedOn.AnsweredAt.After(since) && taskgraph.Related(t, candidate) {
			since = t.BlockedOn.AnsweredAt
		}
	}
	var last taskgraph.Task
	var lastAt time.Time
	n := 0
	for _, t := range graph {
		spent := t.Status == taskgraph.Failed || t.Status == taskgraph.Canceled || (t.Status == taskgraph.Asking && t.BlockedOn.Unresolved())
		if !spent || !taskgraph.Related(t, candidate) {
			continue
		}
		at := t.Finished
		if at.IsZero() {
			at = t.Created
		}
		if !since.IsZero() && !at.After(since) {
			continue
		}
		n++
		if last.ID == "" || at.After(lastAt) {
			last, lastAt = t, at
		}
	}
	return n, last
}

// blockFamilyOnBudget parks the family on one question for the user or Primary: keep retrying or stop. It sets the
// question on last, the family's most recent dispatch, and marks it Asking so the refusal rules see it as open.
// Answering the question releases last (recordQuestionAnswer) and starts a new budget for the family.
func (c *Coord) blockFamilyOnBudget(last taskgraph.Task, n int) {
	question := fmt.Sprintf("%d failed or canceled dispatches of %q across task ids and reworded titles. Stop this work, or answer with a different approach to try once.", n, last.Title)
	updated, err := c.Graph.Update(last.ID, func(x *taskgraph.Task) {
		x.Status = taskgraph.Asking
		x.BlockedOn = &taskgraph.BlockedOn{
			Question:  question,
			Family:    taskgraph.FamilyKey(*x),
			Options:   []taskgraph.Option{{Label: "Stop this work", Detail: "Keep the family blocked and fix the root cause first", Recommended: true}, {Label: "Retry with a different approach", Detail: "Describe the change in the answer; the next dispatch carries it"}},
			BlockedAt: time.Now().UTC(),
		}
	})
	if err != nil {
		c.logWarn("block family", err)
		return
	}
	c.emit("task.updated", updated)
	c.chat("system", fmt.Sprintf("%q is blocked, needs a decision: %s", updated.Title, question), "")
	c.surfaceBlocks()
}
