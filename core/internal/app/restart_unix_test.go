//go:build unix

package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets this test binary stand in for `opencode serve`: when RELO_FAKE_OPENCODE_DIR is set and the
// first argument is "serve", it records its PID in that directory and serves just enough of the API.
func TestMain(m *testing.M) {
	if dir := os.Getenv("RELO_FAKE_OPENCODE_DIR"); dir != "" && len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(fakeOpenCode(dir, os.Args[2:]))
	}
	os.Exit(m.Run())
}

func fakeOpenCode(dir string, args []string) int {
	port := ""
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			port = args[i+1]
		}
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), nil, 0o600); err != nil {
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return 3
	}
	// Record what the child was started with, so tests can assert isolation without a real OpenCode.
	var seen []string
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "OPENCODE_DISABLE_PROJECT_CONFIG", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT"} {
		seen = append(seen, k+"="+os.Getenv(k))
	}
	_ = os.WriteFile(filepath.Join(dir, "env-"+strconv.Itoa(os.Getpid())), []byte(strings.Join(seen, "\n")), 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"version":"fake"}`) })
	mux.HandleFunc("/api/model", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"data":[]}`) })
	_ = http.Serve(ln, mux) // SIGINT from Process.Stop ends the process
	return 0
}

func fakePIDs(t *testing.T, dir string) []int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(pid int) bool {
	for i := 0; i < 100; i++ {
		if !alive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Every start path must retire the previous OpenCode child: a repeated start, a restart, a fallback to the
// mock runtime, and Close.
func TestStartRuntimeNeverOrphansPreviousChild(t *testing.T) {
	core, _ := newMockApp(t)
	pids := t.TempDir()
	t.Setenv("RELO_FAKE_OPENCODE_DIR", pids)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	core.Cfg.Runtime = "opencode"
	core.Cfg.OCPath = self
	ctx := context.Background()

	if _, err := core.startRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	first := fakePIDs(t, pids)
	if len(first) != 1 || !alive(first[0]) {
		t.Fatalf("first child not running: %v", first)
	}

	// Calling startRuntime directly (the path that used to leak) replaces the child.
	if _, err := core.startRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	if !waitDead(first[0]) {
		t.Fatalf("child %d still alive after startRuntime", first[0])
	}
	var second int
	for _, p := range fakePIDs(t, pids) {
		if p != first[0] {
			second = p
		}
	}
	if second == 0 || !alive(second) {
		t.Fatalf("second child not running")
	}

	// Falling back to the mock runtime also retires the child.
	core.Cfg.OCPath = filepath.Join(pids, "missing-binary")
	t.Setenv("PATH", pids) // keep Look from finding a real opencode on PATH or in the home-dir guesses
	t.Setenv("HOME", pids)
	if _, err := core.startRuntime(ctx); err == nil {
		t.Fatal("want fallback error")
	}
	if !waitDead(second) {
		t.Fatalf("child %d still alive after fallback", second)
	}

	// Close stops a live child too.
	core.Cfg.OCPath = self
	if _, err := core.startRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	var third int
	for _, p := range fakePIDs(t, pids) {
		if p != first[0] && p != second {
			third = p
		}
	}
	if third == 0 || !alive(third) {
		t.Fatal("third child not running")
	}
	core.Close()
	if !waitDead(third) {
		t.Fatalf("child %d still alive after Close", third)
	}
}

// The child must run in Relo's private state dir with only Relo's providers enabled, whatever the parent
// shell has set for the user's own OpenCode.
func TestStartRuntimeIsolatesChildFromUserOpenCode(t *testing.T) {
	core, _ := newMockApp(t)
	pids := t.TempDir()
	t.Setenv("RELO_FAKE_OPENCODE_DIR", pids)
	t.Setenv("OPENCODE_CONFIG", "/home/user/their-opencode.json")
	t.Setenv("OPENCODE_CONFIG_DIR", "/home/user/their-dir")
	t.Setenv("XDG_CONFIG_HOME", "/home/user/.config")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	core.Cfg.Runtime, core.Cfg.OCPath, core.Cfg.StateDir = "opencode", self, state
	core.applyProviders(map[string]any{"providers": []any{map[string]any{"id": "acme", "kind": "openai", "baseURL": "https://a.test/v1", "models": []any{map[string]any{"id": "m", "context": 64000}}}}})
	if _, err := core.startRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	pidList := fakePIDs(t, pids)
	if len(pidList) != 1 {
		t.Fatalf("children: %v", pidList)
	}
	raw, err := os.ReadFile(filepath.Join(pids, "env-"+strconv.Itoa(pidList[0])))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, _ := strings.Cut(line, "=")
		env[k] = v
	}
	if env["XDG_CONFIG_HOME"] != filepath.Join(state, "config") || env["XDG_DATA_HOME"] != filepath.Join(state, "data") || env["XDG_CACHE_HOME"] != filepath.Join(state, "cache") || env["XDG_STATE_HOME"] != filepath.Join(state, "state") {
		t.Fatalf("child not isolated: %v", env)
	}
	if env["OPENCODE_CONFIG"] != "" || env["OPENCODE_CONFIG_DIR"] != "" {
		t.Fatalf("user's OpenCode config pointers leaked into the child: %v", env)
	}
	if env["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" {
		t.Fatalf("workspace opencode.json must be ignored: %v", env)
	}
	if !strings.Contains(env["OPENCODE_CONFIG_CONTENT"], `"enabled_providers":["acme"]`) {
		t.Fatalf("only Relo providers may be enabled: %s", env["OPENCODE_CONFIG_CONTENT"])
	}

	// Dev escape hatch: RELO_SHARE_OPENCODE=1 gives back the user's own setup and the full catalog.
	t.Setenv("RELO_SHARE_OPENCODE", "1")
	if _, err := core.startRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	var shared string
	for _, p := range fakePIDs(t, pids) {
		if p != pidList[0] {
			b, _ := os.ReadFile(filepath.Join(pids, "env-"+strconv.Itoa(p)))
			shared = string(b)
		}
	}
	if strings.Contains(shared, "enabled_providers") || !strings.Contains(shared, "XDG_CONFIG_HOME=/home/user/.config") || !strings.Contains(shared, "OPENCODE_CONFIG=/home/user/their-opencode.json") {
		t.Fatalf("escape hatch did not restore the user's environment:\n%s", shared)
	}
}
