//go:build linux

package opencode

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Helper process: starts a long sleep the way Start starts OpenCode, prints its pid, and waits to be killed.
func TestHelperParentProcess(t *testing.T) {
	if os.Getenv("RELO_TEST_PARENT") != "1" {
		t.Skip("helper for TestChildDiesWithParent")
	}
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = childAttr()
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString(strconv.Itoa(cmd.Process.Pid) + "\n")
	time.Sleep(time.Minute)
}

// The OpenCode server must not outlive a crashed or killed core.
func TestChildDiesWithParent(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestHelperParentProcess$")
	parent.Env = append(os.Environ(), "RELO_TEST_PARENT=1")
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("helper printed no pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("bad pid %q", line)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("child not running before the kill: %v", err)
	}
	_ = parent.Process.Kill()
	_ = parent.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// A killed child lingers as a zombie until its (now init) parent reaps it; both mean "not running".
		if data, rerr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); rerr != nil || strings.Contains(string(data), ") Z ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child %d survived its parent", pid)
}
