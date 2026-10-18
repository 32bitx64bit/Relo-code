package integration

import (
	"sync"

	"github.com/relocode/relo/internal/pathglob"
)

// Policy names paths whose conflicts Analyze resolves without asking anyone. Union paths are text files that
// accumulate entries, such as a session log, where both sides' added lines are kept. TakeLatest paths are generated
// outputs, such as a rebuilt binary, where the agent's newest build wins.
type Policy struct {
	Union      []string
	TakeLatest []string
}

// DefaultPolicy applies until SetPolicy replaces it. Markdown and text files are the usual append-only notes.
func DefaultPolicy() Policy {
	return Policy{Union: []string{"*.md", "*.txt"}}
}

var (
	policyMu sync.RWMutex
	policy   = DefaultPolicy()
)

// SetPolicy replaces the policy used by every later analysis. It is safe to call while analyses run.
func SetPolicy(p Policy) {
	policyMu.Lock()
	defer policyMu.Unlock()
	policy = p
}

// CurrentPolicy returns the policy in effect.
func CurrentPolicy() Policy {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return policy
}

// TakesLatest reports whether conflicts at path are resolved by taking the agent's version.
func TakesLatest(path string) bool {
	return pathglob.Any(CurrentPolicy().TakeLatest, path)
}

// UnionMerges reports whether conflicts at path may keep both sides' added lines.
func UnionMerges(path string) bool {
	return pathglob.Any(CurrentPolicy().Union, path)
}
