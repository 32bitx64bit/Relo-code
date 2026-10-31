package verification

import (
	"context"
	"sync"
)

// BaselineCache remembers whether a check command already fails on a pristine base checkout. One session owns one
// cache. Each (baseline dir, command) pair runs at most once for the life of the cache, timeouts included, and
// concurrent callers for the same pair wait on the single in-flight run instead of starting their own.
//
// A started run always finishes, detached from the caller that started it, and its outcome is cached; cancelling
// that caller does not delete the entry or leave an unfinished result behind.
//
// The baseline dir is one checkout per base commit (Workspace.Baseline names it after the short commit), so a new
// base commit is a new key and gets a fresh run.
type BaselineCache struct {
	mu   sync.Mutex
	runs map[string]*baselineRun
}

type baselineRun struct {
	done   chan struct{} // closed when the run has finished
	failed bool          // valid only after done is closed
}

// NewBaselineCache returns an empty cache for one session.
func NewBaselineCache() *BaselineCache {
	return &BaselineCache{runs: map[string]*baselineRun{}}
}

// FailsOnBase reports whether command fails on dir without the agent's change. The shared run is detached from the
// caller that starts it, so one agent being cancelled does not abort the check for the others; Runner.Run still
// stops it at CheckLimit.
//
// A caller that is cancelled while waiting always reports false: the outcome is unknown to that agent, so its
// failure counts as the agent's rather than being blamed on the base. The run itself keeps going and its real
// result is cached for later callers, so an unfinished run is never reported as a result.
func (b *BaselineCache) FailsOnBase(ctx context.Context, runner Runner, dir, command string) bool {
	fails, _ := b.BaselineOutcome(ctx, runner, dir, command)
	return fails
}

// BaselineOutcome reports whether command fails on dir without the agent's change, and false in the second result
// when the outcome is unknown to this caller: the caller was cancelled while it waited. The second result is true
// once a run finished, even if it was started by another caller that was later cancelled, so a cancelled leader does
// not stop its waiters from getting the real outcome.
func (b *BaselineCache) BaselineOutcome(ctx context.Context, runner Runner, dir, command string) (fails, known bool) {
	key := dir + "\x00" + command
	b.mu.Lock()
	if run, ok := b.runs[key]; ok {
		b.mu.Unlock()
		select {
		case <-run.done:
			return run.failed, true
		case <-ctx.Done():
			return false, false
		}
	}
	run := &baselineRun{done: make(chan struct{})}
	b.runs[key] = run
	b.mu.Unlock()

	// WithoutCancel keeps the run alive when the caller that started it is cancelled, while Runner.Run adds
	// CheckLimit so a slow check still ends as a failed result with a timeout note: cached as failed.
	res := runner.Run(context.WithoutCancel(ctx), command, dir)
	run.failed = res.Status == "failed"
	close(run.done)
	if ctx.Err() != nil {
		return false, false
	}
	return run.failed, true
}
