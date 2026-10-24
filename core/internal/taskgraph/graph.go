package taskgraph

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/id"
)

type Status string

const (
	Queued   Status = "queued"
	Blocked  Status = "blocked"
	Running  Status = "running"
	Done     Status = "completed"
	Failed   Status = "failed"
	Canceled Status = "cancelled"
	Review   Status = "review_required"
)

// BlockedOn records why a task is parked waiting on a human answer. A task is
// unresolved while Resolution is empty; clearing Resolution releases it.
type BlockedOn struct {
	Question   string    `json:"question,omitempty"` // the pinned question awaiting an answer
	Family     string    `json:"family,omitempty"`   // family key of the respawn loop this question belongs to
	Resolution string    `json:"resolution,omitempty"`
	BlockedAt  time.Time `json:"blockedAt,omitempty"`
	ResolvedAt time.Time `json:"resolvedAt,omitempty"`
}

// Unresolved reports whether the block is still open. A nil block is not unresolved.
func (b *BlockedOn) Unresolved() bool {
	return b != nil && strings.TrimSpace(b.Resolution) == ""
}

type Task struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Acceptance  []string   `json:"acceptance"`
	Creator     string     `json:"creator"`
	AgentID     string     `json:"agentId,omitempty"`
	Status      Status     `json:"status"`
	Priority    int        `json:"priority"`
	Depends     []string   `json:"dependencies"`
	Paths       []string   `json:"paths,omitempty"` // files this task will edit; used by advisory path leases
	Children    []string   `json:"children"`
	Workspace   string     `json:"workspace"`
	Difficulty  string     `json:"difficulty"`
	Vision      bool       `json:"vision,omitempty"` // the agent must look at images; routing prefers a model that sees
	Attempts    int        `json:"attempts"`
	Model       string     `json:"model"`
	Role        string     `json:"role"`
	Attempt     int        `json:"attempt"`
	Created     time.Time  `json:"createdAt"`
	Started     time.Time  `json:"startedAt,omitempty"`
	Finished    time.Time  `json:"finishedAt,omitempty"`
	Report      any        `json:"report,omitempty"`
	MaxAttempts int        `json:"maxAttempts"`
	Scope       string     `json:"scope,omitempty"`     // declared work area; part of the family key
	BlockedOn   *BlockedOn `json:"blockedOn,omitempty"` // pending question gating completion
}

type Graph struct {
	mu    sync.Mutex
	tasks map[string]*Task
}

func New() *Graph {
	return &Graph{tasks: map[string]*Task{}}
}

func (g *Graph) Add(t Task) Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.ID == "" {
		t.ID = id.New("task")
	}
	if t.Created.IsZero() {
		t.Created = time.Now().UTC()
	}
	if t.Status == "" {
		t.Status = Queued
	}
	if t.MaxAttempts == 0 {
		t.MaxAttempts = 3
	}
	g.tasks[t.ID] = &t
	g.recomputeLocked()
	return *g.tasks[t.ID]
}

func (g *Graph) AddChecked(t Task) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.ID == "" {
		t.ID = id.New("task")
	}
	if _, exists := g.tasks[t.ID]; exists {
		return Task{}, fmt.Errorf("duplicate task id %q", t.ID)
	}
	known := make(map[string]bool, len(g.tasks)+1)
	for id := range g.tasks {
		known[id] = true
	}
	known[t.ID] = true
	for _, dep := range t.Depends {
		if !known[dep] {
			return Task{}, fmt.Errorf("task %q has unknown dependency %q", t.ID, dep)
		}
	}
	candidate := make([]Task, 0, len(g.tasks)+1)
	for _, existing := range g.tasks {
		candidate = append(candidate, *existing)
	}
	candidate = append(candidate, t)
	if err := DetectCycle(candidate); err != nil {
		return Task{}, err
	}
	if t.Created.IsZero() {
		t.Created = time.Now().UTC()
	}
	if t.Status == "" {
		t.Status = Queued
	}
	if t.MaxAttempts == 0 {
		t.MaxAttempts = 3
	}
	g.tasks[t.ID] = &t
	g.recomputeLocked()
	return *g.tasks[t.ID], nil
}

func (g *Graph) SetDependencies(id string, deps []string) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	for _, dep := range deps {
		if _, exists := g.tasks[dep]; !exists {
			return Task{}, fmt.Errorf("task %q has unknown dependency %q", id, dep)
		}
	}
	candidate := make([]Task, 0, len(g.tasks))
	for _, existing := range g.tasks {
		copy := *existing
		if copy.ID == id {
			copy.Depends = deps
		}
		candidate = append(candidate, copy)
	}
	if err := DetectCycle(candidate); err != nil {
		return Task{}, err
	}
	t.Depends = append([]string(nil), deps...)
	g.recomputeLocked()
	return *t, nil
}

func DetectCycle(tasks []Task) error {
	deps := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		deps[t.ID] = t.Depends
	}
	state := make(map[string]uint8, len(tasks))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("dependency cycle includes task %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, dep := range deps[id] {
			if _, exists := deps[dep]; exists {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		state[id] = 2
		return nil
	}
	for id := range deps {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func ValidateDependencies(tasks []Task, known func(id string) bool) error {
	ids := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		ids[t.ID] = true
	}
	var problems []string
	for _, t := range tasks {
		for _, dep := range t.Depends {
			if !ids[dep] && (known == nil || !known(dep)) {
				problems = append(problems, fmt.Sprintf("task %q has unknown dependency %q", t.ID, dep))
			}
		}
	}
	if err := DetectCycle(tasks); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

func (g *Graph) Get(id string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *t, true
}

func (g *Graph) List() []Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Task, 0, len(g.tasks))
	for _, t := range g.tasks {
		out = append(out, *t)
	}
	return out
}

func (g *Graph) Update(id string, fn func(*Task)) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	fn(t)
	g.recomputeLocked()
	return *t, nil
}

func (g *Graph) Runnable() []Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recomputeLocked()
	var out []Task
	for _, t := range g.tasks {
		if t.Status == Queued {
			out = append(out, *t)
		}
	}
	return out
}

func (g *Graph) Cancel(id string) error {
	_, err := g.Update(id, func(t *Task) {
		t.Status = Canceled
		t.Finished = time.Now().UTC()
	})
	return err
}

func (g *Graph) Retry(id string) (Task, error) {
	return g.Update(id, func(t *Task) {
		if t.Attempt >= t.MaxAttempts {
			t.Status = Failed
			return
		}
		t.Status = Queued
		t.Finished = time.Time{}
		t.Started = time.Time{}
	})
}

func (g *Graph) recomputeLocked() {
	for _, t := range g.tasks {
		if t.Status == Done || t.Status == Failed || t.Status == Canceled || t.Status == Running || t.Status == Review {
			continue
		}
		ready := true
		for _, dep := range t.Depends {
			d, ok := g.tasks[dep]
			if !ok || d.Status != Done {
				ready = false
				break
			}
		}
		if ready {
			t.Status = Queued
		} else {
			t.Status = Blocked
		}
	}
}

func (g *Graph) LinkChild(parent, child string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.tasks[parent]
	if !ok {
		return
	}
	for _, c := range p.Children {
		if c == child {
			return
		}
	}
	p.Children = append(p.Children, child)
}
