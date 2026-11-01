//go:build !linux

package tempdir

// isZombie cannot be read outside Linux, so a process that answers kill 0 counts as alive.
func isZombie(int) bool { return false }
