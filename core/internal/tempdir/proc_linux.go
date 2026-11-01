//go:build linux

package tempdir

import (
	"os"
	"strconv"
	"strings"
)

// isZombie reads the process state from /proc; without it, a process that answers kill 0 counts as alive.
func isZombie(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state is the field after the parenthesised command name, which may itself hold spaces and brackets.
	stat := string(raw)
	if i := strings.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) {
		return stat[i+2] == 'Z' || stat[i+2] == 'X'
	}
	return false
}
