//go:build unix

package tempdir

import (
	"errors"
	"syscall"
)

// ProcessAlive reports whether pid is a running process. A killed process stays a zombie until its parent reaps
// it, so a zombie is not alive for temp cleanup: its markers mean nothing.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		// EPERM: the process runs, owned by someone else. Without /proc the best answer is yes.
		return !isZombie(pid)
	}
	return false
}
