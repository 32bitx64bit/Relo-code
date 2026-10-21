//go:build !linux

package claudecode

import "syscall"

// childAttr has no parent-death signal outside Linux; CloseSession and the core's shutdown path clean up there.
func childAttr() *syscall.SysProcAttr { return nil }
