package scheduler

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

func TestLeasesKeepOverlappingPathsApart(t *testing.T) {
	s := New(Limits{PathLeases: true})
	release, ok, why := s.AcquirePaths("first", []string{"core/a.go"})
	if !ok {
		t.Fatalf("first acquire failed: %s", why)
	}
	if _, ok, why := s.AcquirePaths("second", []string{"core/a.go"}); ok {
		t.Fatal("two tasks with the same path acquired at once")
	} else if why != WhyPaths {
		t.Fatalf("why = %q, want %q", why, WhyPaths)
	}
	// All-or-nothing: a refused request must not keep the paths that were free.
	if _, ok, _ := s.AcquirePaths("partial", []string{"core/a.go", "core/b.go"}); ok {
		t.Fatal("a request with one held path was accepted")
	}
	releaseB, ok, _ := s.AcquirePaths("other", []string{"core/b.go"})
	if !ok {
		t.Fatal("a refused request leaked the path that was free")
	}
	releaseB()
	// A declared directory overlaps a file below it.
	if _, ok, _ := s.AcquirePaths("third", []string{"core"}); ok {
		t.Fatal("a directory and a file below it acquired at once")
	}
	release()
	if _, ok, why := s.AcquirePaths("second", []string{"core/a.go"}); !ok {
		t.Fatalf("path stayed leased after release: %s", why)
	}
}

func TestLeasesAllowDisjointPathsTogether(t *testing.T) {
	s := New(Limits{PathLeases: true})
	releaseA, ok, why := s.AcquirePaths("a", []string{"a.txt"})
	if !ok {
		t.Fatalf("a: %s", why)
	}
	defer releaseA()
	releaseB, ok, why := s.AcquirePaths("b", []string{"b.txt"})
	if !ok {
		t.Fatalf("disjoint paths must run together: %s", why)
	}
	defer releaseB()
}

func TestLeasesOffByDefaultAndNoPathsUnchanged(t *testing.T) {
	off := New(Limits{})
	releaseA, ok, _ := off.AcquirePaths("a", []string{"same.txt"})
	if !ok {
		t.Fatal("a lease must not be enforced while the flag is off")
	}
	if _, ok, _ := off.AcquirePaths("b", []string{"same.txt"}); !ok {
		t.Fatal("flag off changed scheduling for a declared path")
	}
	if got := off.PriorHolders([]string{"same.txt"}); len(got) != 0 {
		t.Fatalf("flag off reported soft dependencies: %+v", got)
	}
	releaseA()

	on := New(Limits{PathLeases: true})
	releaseC, ok, why := on.AcquirePaths("c", nil)
	if !ok {
		t.Fatalf("no paths: %s", why)
	}
	releaseD, ok, why := on.AcquirePaths("d", nil)
	if !ok {
		t.Fatalf("a task that declares no paths must not wait: %s", why)
	}
	releaseC()
	releaseD()
	if got := on.PriorHolders(nil); len(got) != 0 {
		t.Fatalf("no paths produced soft dependencies: %+v", got)
	}
}

func TestLeasePriorHoldersKeepTheFinishedTask(t *testing.T) {
	s := New(Limits{PathLeases: true})
	release, ok, why := s.AcquirePaths("a", []string{"core/a.go", "core/b.go"})
	if !ok {
		t.Fatalf("a: %s", why)
	}
	held := s.PriorHolders([]string{"core/a.go"})
	if len(held) != 1 || held[0].TaskID != "a" || len(held[0].Paths) != 1 || held[0].Paths[0] != "core/a.go" {
		t.Fatalf("while held = %+v, want a on core/a.go", held)
	}
	release()
	after := s.PriorHolders([]string{"core/a.go"})
	if len(after) != 1 || after[0].TaskID != "a" {
		t.Fatalf("after release = %+v, want the finished holder", after)
	}
	if got := s.PriorHolders([]string{"unrelated.go"}); len(got) != 0 {
		t.Fatalf("disjoint path reported a holder: %+v", got)
	}
}

func TestLeaseWaitMedianReportsHeldDelay(t *testing.T) {
	s := New(Limits{PathLeases: true})
	release, _, _ := s.AcquirePaths("a", []string{"a.txt"})
	if s.LeaseWaitMedian() != 0 {
		t.Fatal("no wait should report zero")
	}
	if _, ok, _ := s.AcquirePaths("b", []string{"a.txt"}); ok {
		t.Fatal("b should have been refused")
	}
	time.Sleep(2 * time.Millisecond)
	release()
	releaseB, ok, _ := s.AcquirePaths("b", []string{"a.txt"})
	if !ok {
		t.Fatal("b should acquire after a releases")
	}
	releaseB()
	if got := s.LeaseWaitMedian(); got < 2*time.Millisecond {
		t.Fatalf("median wait = %s, want at least 2ms", got)
	}
}

// Tasks are admitted oldest first, as the dispatcher does, and a running task releases when it finishes. Under
// random finishes every task must eventually run, with no two overlapping tasks running at once.
func TestLeasePropertyNoDeadlockOrStarvation(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	paths := []string{"a.txt", "b.txt", "c.txt", "dir"}
	for trial := 0; trial < 100; trial++ {
		s := New(Limits{PathLeases: true})
		type task struct {
			id, path string
		}
		tasks := make([]task, 24)
		for i := range tasks {
			tasks[i] = task{id: "t" + string(rune('a'+i)), path: paths[rng.Intn(len(paths))]}
		}
		running := map[string]func(){}
		done := map[string]bool{}
		for step := 0; step < 10000 && len(done) < len(tasks); step++ {
			for _, tk := range tasks {
				if running[tk.id] != nil || done[tk.id] {
					continue
				}
				if release, ok, _ := s.AcquirePaths(tk.id, []string{tk.path}); ok {
					running[tk.id] = release
				}
			}
			// No two running tasks may hold overlapping paths, and no two may declare the same path.
			var active []string
			for id, release := range running {
				if release == nil {
					continue
				}
				active = append(active, id)
			}
			sort.Strings(active)
			for i := 0; i < len(active); i++ {
				for j := i + 1; j < len(active); j++ {
					for _, a := range tasks {
						if a.id != active[i] {
							continue
						}
						for _, b := range tasks {
							if b.id == active[j] && pathsOverlap(a.path, b.path) {
								t.Fatalf("trial %d: %s and %s ran together on %s", trial, a.id, b.id, a.path)
							}
						}
					}
				}
			}
			if len(running) == 0 {
				if len(done) < len(tasks) {
					t.Fatalf("trial %d: no task could run with %d left", trial, len(tasks)-len(done))
				}
				break
			}
			active = active[:0]
			for id := range running {
				active = append(active, id)
			}
			victim := active[rng.Intn(len(active))]
			running[victim]()
			delete(running, victim)
			done[victim] = true
		}
		if len(done) != len(tasks) {
			t.Fatalf("trial %d: only %d of %d tasks ran (starvation)", trial, len(done), len(tasks))
		}
	}
}
