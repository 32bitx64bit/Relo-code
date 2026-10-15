//go:build linux

package opencode

import "syscall"

// childAttr makes the kernel kill the OpenCode server when Relo Core dies for any reason, including SIGKILL and
// crashes. Without it every core crash leaves a ~100 MB server behind, running for hours.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
