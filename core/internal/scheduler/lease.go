package scheduler

import (
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Path leases are advisory: they only serialize tasks that declare overlapping paths. A task that declares no paths
// takes no lease and behaves exactly as before, so the feature is off unless a task opts in.
//
// A task waits until the earlier task finishes, not until its changes are accepted. When it is admitted it is cut
// with the earlier task's pending patches already applied (see orchestrator stack.go), so the same artifact covers a
// declared dependency and a lease overlap: a soft dependency.

// WhyPaths is the status a task waits on while another task holds an overlapping path.
const WhyPaths = "path leased by another task"

// SoftDep names an earlier task that held a path this task declares, and the paths they overlapped on.
type SoftDep struct {
	TaskID string
	Paths  []string
}

type leaseHolder struct {
	taskID string
	seq    uint64
}

// leasePath normalizes a declared path and rejects values that cannot name a file in the workspace.
func leasePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	if p == "." || p == ".." || path.IsAbs(p) || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

// leasePaths normalizes, deduplicates and sorts declared paths so acquisition has one total order.
func leasePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		p := leasePath(raw)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// pathsOverlap treats a declared directory and a file below it as overlapping.
func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func (s *Scheduler) leaseEnabled() bool { return s.Limits.PathLeases }

// PriorHolders lists earlier tasks that held an overlapping path. Both the current holder and the most recent
// finished holder are reported, earliest first, so an agent admitted now is cut with their pending work.
func (s *Scheduler) PriorHolders(paths []string) []SoftDep {
	wanted := leasePaths(paths)
	if len(wanted) == 0 || !s.leaseEnabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.priorHoldersLocked(wanted)
}

func (s *Scheduler) priorHoldersLocked(wanted []string) []SoftDep {
	byTask := map[string]*SoftDep{}
	seqs := map[string]uint64{}
	consider := func(heldPath string, h leaseHolder) {
		if h.taskID == "" {
			return
		}
		for _, p := range wanted {
			if !pathsOverlap(p, heldPath) {
				continue
			}
			dep := byTask[h.taskID]
			if dep == nil {
				dep = &SoftDep{TaskID: h.taskID}
				byTask[h.taskID] = dep
			}
			if !containsString(dep.Paths, heldPath) {
				dep.Paths = append(dep.Paths, heldPath)
			}
			// A retried task has both a stale finished entry and its current one; order by the current run.
			if h.seq > seqs[h.taskID] {
				seqs[h.taskID] = h.seq
			}
			return
		}
	}
	for p, h := range s.leases {
		consider(p, h)
	}
	for p, h := range s.lastLease {
		consider(p, h)
	}
	ids := make([]string, 0, len(byTask))
	for id := range byTask {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool { return seqs[ids[i]] < seqs[ids[j]] })
	out := make([]SoftDep, 0, len(ids))
	for _, id := range ids {
		out = append(out, *byTask[id])
	}
	return out
}

// AcquirePaths takes every requested path or none, in sorted path order. It returns a release func that is safe to
// call more than once.
func (s *Scheduler) AcquirePaths(taskID string, paths []string) (release func(), ok bool, why string) {
	wanted := leasePaths(paths)
	if len(wanted) == 0 || !s.leaseEnabled() {
		return func() {}, true, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range wanted {
		for held, h := range s.leases {
			if h.taskID == taskID {
				continue
			}
			if pathsOverlap(p, held) {
				if _, started := s.leaseWait[taskID]; !started {
					s.leaseWait[taskID] = time.Now()
				}
				return func() {}, false, WhyPaths
			}
		}
	}
	s.seq++
	h := leaseHolder{taskID: taskID, seq: s.seq}
	for _, p := range wanted {
		s.leases[p] = h
	}
	s.heldPaths[taskID] = append(s.heldPaths[taskID], wanted...)
	if started, waiting := s.leaseWait[taskID]; waiting {
		s.leaseDelays = append(s.leaseDelays, time.Since(started))
		delete(s.leaseWait, taskID)
	}
	var once sync.Once
	return func() {
		once.Do(func() { s.ReleasePaths(taskID) })
	}, true, ""
}

// ReleasePaths releases every path held by a task and remembers it as the last holder so the next overlapping task
// can stack its pending work.
func (s *Scheduler) ReleasePaths(taskID string) {
	if taskID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.heldPaths[taskID] {
		if h, ok := s.leases[p]; ok && h.taskID == taskID {
			delete(s.leases, p)
			s.lastLease[p] = h
		}
	}
	delete(s.heldPaths, taskID)
}

// LeaseWaitMedian reports the median delay between a task first being refused a path and acquiring it. Zero means
// no task waited on a lease. The design keeps the feature off when this exceeds the median agent run time.
func (s *Scheduler) LeaseWaitMedian() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.leaseDelays) == 0 {
		return 0
	}
	delays := append([]time.Duration(nil), s.leaseDelays...)
	sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
	return delays[len(delays)/2]
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
