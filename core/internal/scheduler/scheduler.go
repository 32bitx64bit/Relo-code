package scheduler

import (
	"context"
	"sync"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/taskgraph"
)

type Limits struct {
	MaxTotal int
	MaxTier1 int
	MaxTier2 int
	MaxDepth int
	MaxRetry int
}

func (l Limits) WithDefaults() Limits {
	if l.MaxTotal == 0 {
		l.MaxTotal = 6
	}
	if l.MaxTier1 == 0 {
		l.MaxTier1 = 3
	}
	if l.MaxTier2 == 0 {
		l.MaxTier2 = 4
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = 3
	}
	if l.MaxRetry == 0 {
		l.MaxRetry = 3
	}
	return l
}

// Why an agent is not admitted. A full limit passes when another agent finishes; the others do not.
const (
	WhyPaused = "paused"
	WhyDepth  = "max hierarchy depth"
	WhyTotal  = "max concurrent agents"
	WhyTier1  = "max tier1 agents"
	WhyTier2  = "max tier2 agents"
)

// Full reports whether why is a limit that opens up as soon as another agent finishes, so waiting for it is useful.
func Full(why string) bool { return why == WhyTotal || why == WhyTier1 || why == WhyTier2 }

type Scheduler struct {
	Limits   Limits
	mu       sync.Mutex
	paused   bool
	reserved map[string]agent.Agent
	resumed  chan struct{}
}

func New(l Limits) *Scheduler {
	resumed := make(chan struct{})
	close(resumed)
	return &Scheduler{Limits: l.WithDefaults(), reserved: map[string]agent.Agent{}, resumed: resumed}
}

func (s *Scheduler) Pause() {
	s.mu.Lock()
	if !s.paused {
		s.paused = true
		s.resumed = make(chan struct{})
	}
	s.mu.Unlock()
}
func (s *Scheduler) Resume() {
	s.mu.Lock()
	if s.paused {
		s.paused = false
		close(s.resumed)
	}
	s.mu.Unlock()
}
func (s *Scheduler) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

func (s *Scheduler) Allow(reg *agent.Registry, a agent.Agent) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowLocked(reg, a)
}

// CanReserve reports whether current limits permit another agent of this tier.
func (s *Scheduler) CanReserve(reg *agent.Registry, a agent.Agent) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowLocked(reg, a)
}

// Reserve atomically admits an agent, including agents not yet marked active.
// Release after the registry reflects the agent's active or terminal status.
func (s *Scheduler) Reserve(reg *agent.Registry, a agent.Agent) (release func(), ok bool, why string) {
	s.mu.Lock()
	if ok, why = s.allowLocked(reg, a); !ok {
		s.mu.Unlock()
		return nil, false, why
	}
	s.reserved[a.ID] = a
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.reserved, a.ID)
			s.mu.Unlock()
		})
	}, true, ""
}

func (s *Scheduler) Unreserve(id string) {
	s.mu.Lock()
	delete(s.reserved, id)
	s.mu.Unlock()
}

func (s *Scheduler) allowLocked(reg *agent.Registry, a agent.Agent) (bool, string) {
	if s.paused {
		return false, WhyPaused
	}
	if a.Depth >= s.Limits.MaxDepth {
		return false, WhyDepth
	}
	total, t1, t2 := reg.CountActive()
	active := map[string]bool{}
	for _, registered := range reg.List() {
		if registered.Status == agent.Running || registered.Status == agent.Starting || registered.Status == agent.Waiting {
			active[registered.ID] = true
		}
	}
	for _, reserved := range s.reserved {
		if reserved.ID == a.ID {
			continue
		}
		if active[reserved.ID] {
			continue
		}
		total++
		switch reserved.Tier {
		case agent.Tier1:
			t1++
		case agent.Tier2:
			t2++
		}
	}
	if total >= s.Limits.MaxTotal {
		return false, WhyTotal
	}
	if a.Tier == agent.Tier1 && t1 >= s.Limits.MaxTier1 {
		return false, WhyTier1
	}
	if a.Tier == agent.Tier2 && t2 >= s.Limits.MaxTier2 {
		return false, WhyTier2
	}
	return true, ""
}

// Wait blocks until dispatch resumes or ctx is cancelled.
func (s *Scheduler) Wait(ctx context.Context) error {
	s.mu.Lock()
	resumed := s.resumed
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-resumed:
		return nil
	}
}

func (s *Scheduler) Next(g *taskgraph.Graph, n int) []taskgraph.Task {
	if s.Paused() {
		return nil
	}
	run := g.Runnable()
	if n > 0 && len(run) > n {
		run = run[:n]
	}
	return run
}

func (s *Scheduler) Run(ctx context.Context, fn func(context.Context) error) error {
	if s.Paused() {
		return nil
	}
	return fn(ctx)
}
