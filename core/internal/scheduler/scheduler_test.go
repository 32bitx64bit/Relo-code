package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
)

func TestLimits(t *testing.T) {
	s := New(Limits{MaxTotal: 1, MaxTier1: 1, MaxTier2: 1, MaxDepth: 2})
	reg := agent.NewRegistry()
	a := reg.Create(agent.Agent{Role: agent.Worker, Status: agent.Running, Tier: agent.Tier1})
	ok, why := s.Allow(reg, agent.Agent{Role: agent.Worker, Tier: agent.Tier1, Depth: 1})
	if ok {
		t.Fatalf("should block, have %s", a.ID)
	}
	if why == "" {
		t.Fatal("missing reason")
	}
	ok, _ = s.Allow(reg, agent.Agent{Role: agent.Probe, Tier: agent.Tier2, Depth: 3})
	if ok {
		t.Fatal("depth")
	}
}

func TestReserveEnforcesCapsConcurrently(t *testing.T) {
	const cap = 5
	s := New(Limits{MaxTotal: cap, MaxTier1: cap, MaxDepth: 2})
	reg := agent.NewRegistry()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var releases []func()
	accepted := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := agent.Agent{ID: string(rune('a' + i)), Role: agent.Worker, Tier: agent.Tier1, Depth: 1}
			release, ok, _ := s.Reserve(reg, a)
			if ok {
				mu.Lock()
				accepted++
				releases = append(releases, release)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != cap {
		t.Fatalf("accepted %d reservations, want %d", accepted, cap)
	}
	for _, release := range releases {
		release()
	}
}

func TestReserveCountsActiveOnceAndPauseWaits(t *testing.T) {
	s := New(Limits{MaxTotal: 1, MaxTier1: 1, MaxDepth: 2})
	reg := agent.NewRegistry()
	a := reg.Create(agent.Agent{ID: "worker", Role: agent.Worker, Tier: agent.Tier1, Status: agent.Queued, Depth: 1})
	release, ok, why := s.Reserve(reg, a)
	if !ok {
		t.Fatalf("reserve failed: %s", why)
	}
	reg.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Starting })
	second := agent.Agent{ID: "second", Role: agent.Worker, Tier: agent.Tier1, Depth: 1}
	if _, ok, _ := s.Reserve(reg, second); ok {
		t.Fatal("active reservation double-counted as free slot")
	}
	release()
	s.Pause()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wait := make(chan error, 1)
	go func() { wait <- s.Wait(ctx) }()
	time.Sleep(5 * time.Millisecond)
	s.Resume()
	if err := <-wait; err != nil {
		t.Fatal(err)
	}
}
