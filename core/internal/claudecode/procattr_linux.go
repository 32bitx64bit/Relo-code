//go:build linux

package claudecode

import "syscall"

// childAttr makes the kernel kill a Claude Code process when Relo Core dies for any reason, so a crash leaves no
// agent running on its own.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
