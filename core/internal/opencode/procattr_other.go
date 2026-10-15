//go:build !linux

package opencode

import "syscall"

// childAttr has no parent-death signal outside Linux; Process.Stop and the core's shutdown path clean up there.
func childAttr() *syscall.SysProcAttr { return nil }
