//go:build !unix

package tempdir

// ProcessAlive cannot probe other processes outside unix, so no root is ever stale by marker: only age removes
// untracked one-shot files there.
func ProcessAlive(pid int) bool { return pid > 0 }
