package agent

import (
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/id"
)

type Role string

const (
	Primary     Role = "primary"
	Head        Role = "head"
	Worker      Role = "worker"
	Probe       Role = "probe"
	Reviewer    Role = "reviewer"
	Architect   Role = "architect"
	Debug       Role = "debug"
	Test        Role = "test"
	Integration Role = "integration"
)

type Tier string

const (
	TierPrimary Tier = "primary"
	Tier1       Tier = "tier1"
	Tier2       Tier = "tier2"
)

type Status string

const (
	Queued      Status = "queued"
	Starting    Status = "starting"
	Running     Status = "running"
	Waiting     Status = "waiting"
	Blocked     Status = "blocked"
	Completed   Status = "completed"
	Failed      Status = "failed"
	Cancelled   Status = "cancelled"
	ReviewReq   Status = "review_required"
	Interrupted Status = "interrupted"
)

type Agent struct {
	ID            string    `json:"id"`
	ParentID      string    `json:"parentId,omitempty"`
	Role          Role      `json:"role"`
	Tier          Tier      `json:"tier"`
	Title         string    `json:"title"`
	TaskID        string    `json:"taskId,omitempty"`
	Difficulty    string    `json:"difficulty"`
	Attempt       int       `json:"attempt"`
	EscalatedFrom string    `json:"escalatedFrom,omitempty"`
	Status        Status    `json:"status"`
	Model         string    `json:"model"`
	Scope         string    `json:"scope"`
	Workspace     string    `json:"workspace"`
	Worktree      string    `json:"worktree,omitempty"`
	Generation    int       `json:"generation"`
	Budget        int       `json:"budget"`
	UsedTokens    int       `json:"usedTokens"`    // tokens spent so far, summed over every model call
	ContextTokens int       `json:"contextTokens"` // what the latest model call held: how full the window is now
	Window        int       `json:"window"`        // context window planned for Model; 0 when unknown
	InputTokens   int       `json:"inputTokens"`
	OutputTokens  int       `json:"outputTokens"`
	Steps         int       `json:"steps"`      // model requests made, one per provider call
	CacheRead     int       `json:"cacheRead"`  // cached input tokens read, summed over every model call
	CacheWrite    int       `json:"cacheWrite"` // input tokens written to the cache, summed over every model call
	Cost          float64   `json:"cost,omitempty"`
	Activity      string    `json:"activity"`
	Phase         string    `json:"phase"`               // what it is doing now: context, model, reasoning, writing, tool, verifying, waiting; "" when idle or finished
	PhaseAt       int64     `json:"phaseAt,omitempty"`   // unix ms the current phase began
	StartedAt     int64     `json:"startedAt,omitempty"` // unix ms the current run began
	EndedAt       int64     `json:"endedAt,omitempty"`   // unix ms it reached a terminal status; 0 while running
	Tools         int       `json:"tools,omitempty"`     // tool calls made in the current run
	Rev           int64     `json:"rev"`                 // bumped on every change; receivers drop copies older than what they hold
	Files         []string  `json:"files,omitempty"`
	RuntimeID     string    `json:"runtimeId,omitempty"`
	Created       time.Time `json:"createdAt"`
	Depth         int       `json:"depth"`
}

type Report struct {
	Status   Status   `json:"status"`
	Summary  string   `json:"summary"`
	Findings []string `json:"findings"`
	Files    []string `json:"filesChanged"`
	Verify   []Verify `json:"verification"`
	Risks    []string `json:"remainingRisks"`
	Next     []string `json:"recommendedNextActions"`
	Cause    string   `json:"rootCause,omitempty"`
	// Handoff is what tasks that build on this one need, kept whole when the summary is clipped for the Primary.
	Handoff string `json:"handoff,omitempty"`
	// Question is the decision a blocked agent needs.
	Question string `json:"question,omitempty"`
	Raw      string `json:"raw,omitempty"`
}

type Verify struct {
	Command string `json:"command"`
	Status  string `json:"status"`
	Output  string `json:"output,omitempty"`
}

type Registry struct {
	mu      sync.Mutex
	agents  map[string]*Agent
	reports map[string]Report
	// OnChange, when set, receives a copy of the agent after every Update. It runs outside the registry lock.
	OnChange func(Agent)
}

// Terminal reports whether s is a final status for a run.
func Terminal(s Status) bool {
	switch s {
	case Completed, Failed, Cancelled, Interrupted, ReviewReq:
		return true
	}
	return false
}

// stamp keeps the clocks consistent for every writer: the run clock starts when the agent first runs and stops at a
// terminal status (which also clears the phase), and the phase clock restarts whenever the phase changes.
func (a *Agent) stamp(prevPhase string, now int64) {
	if Terminal(a.Status) {
		a.Phase = ""
		if a.EndedAt == 0 {
			a.EndedAt = now
		}
	} else {
		a.EndedAt = 0
	}
	if (a.Status == Running || a.Status == Starting) && a.StartedAt == 0 {
		a.StartedAt = now
	}
	if a.Phase != prevPhase || a.PhaseAt == 0 {
		a.PhaseAt = now
	}
}

func NewRegistry() *Registry {
	return &Registry{agents: map[string]*Agent{}, reports: map[string]Report{}}
}

func (r *Registry) Create(a Agent) Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.ID == "" {
		a.ID = id.New("agent")
	}
	if a.Created.IsZero() {
		a.Created = time.Now().UTC()
	}
	if a.Status == "" {
		a.Status = Queued
	}
	if a.Tier == "" {
		a.Tier = TierFor(a.Role)
	}
	a.stamp(a.Phase, time.Now().UnixMilli())
	a.Rev++
	if a.ParentID != "" {
		if p, ok := r.agents[a.ParentID]; ok {
			a.Depth = p.Depth + 1
		}
	}
	r.agents[a.ID] = &a
	return a
}

func (r *Registry) Get(id string) (Agent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok {
		return Agent{}, false
	}
	return *a, true
}

func (r *Registry) List() []Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, *a)
	}
	return out
}

func (r *Registry) Children(parent string) []Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Agent
	for _, a := range r.agents {
		if a.ParentID == parent {
			out = append(out, *a)
		}
	}
	return out
}

func (r *Registry) Update(id string, fn func(*Agent)) (Agent, bool) {
	r.mu.Lock()
	a, ok := r.agents[id]
	if !ok {
		r.mu.Unlock()
		return Agent{}, false
	}
	prev, wasDone := a.Phase, Terminal(a.Status)
	fn(a)
	if wasDone && !Terminal(a.Status) { // a finished agent running again is a new run: restart its clocks
		a.StartedAt, a.EndedAt, a.Tools = 0, 0, 0
	}
	a.stamp(prev, time.Now().UnixMilli())
	a.Rev++
	out, hook := *a, r.OnChange
	r.mu.Unlock()
	if hook != nil {
		hook(out)
	}
	return out, true
}

func (r *Registry) SetReport(id string, rep Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports[id] = rep
}

func (r *Registry) Report(id string) (Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep, ok := r.reports[id]
	return rep, ok
}

func (r *Registry) CountActive() (total, t1, t2 int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.agents {
		if a.Status == Running || a.Status == Starting || a.Status == Waiting {
			total++
			switch a.Tier {
			case Tier1:
				t1++
			case Tier2:
				t2++
			}
		}
	}
	return
}

func (r *Registry) Primary() (Agent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.agents {
		if a.Role == Primary && a.Status != Cancelled {
			return *a, true
		}
	}
	return Agent{}, false
}

func TierFor(role Role) Tier {
	switch role {
	case Primary:
		return TierPrimary
	case Probe:
		return Tier2
	default:
		return Tier1
	}
}

func DefaultDifficulty(role Role) string {
	switch role {
	case Probe, Test, Reviewer:
		return "easy"
	case Worker, Integration, Debug:
		return "medium"
	case Architect, Head:
		return "hard"
	case Primary:
		return "expert"
	default:
		return ""
	}
}

func ParseRole(s string) (Role, bool) {
	r := Role(strings.ToLower(strings.TrimSpace(s)))
	switch r {
	case Primary, Head, Worker, Probe, Reviewer, Architect, Debug, Test, Integration:
		return r, true
	default:
		return "", false
	}
}

func Mutating(role Role) bool {
	switch role {
	case Probe, Reviewer, Primary, Head:
		return false
	default:
		return true
	}
}
