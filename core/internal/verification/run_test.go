package verification

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSplitQuotedArgs(t *testing.T) {
	got, err := split(`printf "%s" "hello world" 'single quote'`)
	if err != nil || len(got) != 4 || got[2] != "hello world" || got[3] != "single quote" {
		t.Fatalf("split: %#v %v", got, err)
	}
	if _, err := split(`printf "unterminated`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestDetectListsOnlyConfiguredTests(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{
		"package.json": `{"scripts":{"build":"tsc"}}`,
		"go.mod":       "module example.test\n",
		"Makefile":     "build:\n\ttrue\n",
		"Cargo.toml":   "[package]\nname='demo'\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := Detect(root)
	if len(got) != 2 || got[0] != "go test ./..." || got[1] != "cargo test" {
		t.Fatalf("unexpected candidates: %v", got)
	}
}

func TestRunCapsOutputAndUsesGivenDir(t *testing.T) {
	dir := t.TempDir()
	res := (Runner{}).Run(context.Background(), `head -c 100000 /dev/zero`, dir)
	if res.Status != "passed" || len(res.Output) > outputLimit || !strings.Contains(res.Output, "output truncated") {
		t.Fatalf("output cap failed: status=%s len=%d", res.Status, len(res.Output))
	}
}

func TestRunFailsForMissingWorkdir(t *testing.T) {
	res := (Runner{}).Run(context.Background(), "true", filepath.Join(t.TempDir(), "missing"))
	if res.Status != "failed" {
		t.Fatalf("missing workdir accepted: %+v", res)
	}
}

func TestRunCancellationKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- (Runner{}).Run(ctx, `sh -c "sleep 30 & wait"`, t.TempDir())
	}()
	timer := time.NewTimer(100 * time.Millisecond)
	select {
	case res := <-done:
		timer.Stop()
		t.Fatalf("command exited before cancellation: %+v", res)
	case <-timer.C:
	}
	cancel()
	select {
	case res := <-done:
		if res.Status != "failed" {
			t.Fatalf("cancelled command status = %s", res.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process group survived cancellation")
	}
}

func TestPlanPrefersMakeTestAndSkipsMissingDependencies(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{
		"package.json": `{"scripts":{"test":"tsx --test"},"devDependencies":{"tsx":"1"}}`,
		"Makefile":     "test:\n\ttrue\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	steps := Plan(root)
	if len(steps) != 1 || steps[0].Command != "make test" {
		t.Fatalf("a Makefile test target should be the only step: %+v", steps)
	}
	if err := os.Remove(filepath.Join(root, "Makefile")); err != nil {
		t.Fatal(err)
	}
	steps = Plan(root)
	if len(steps) != 1 || steps[0].Command != "npm test --silent" || steps[0].Skip == "" {
		t.Fatalf("npm test without node_modules must be skipped: %+v", steps)
	}
	if err := os.Mkdir(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if steps = Plan(root); len(steps) != 1 || steps[0].Skip != "" {
		t.Fatalf("npm test with node_modules should run: %+v", steps)
	}
}

func TestEnvFailureIsSkippedNotFailed(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	dir := t.TempDir()
	res := (Runner{}).Run(context.Background(), `sh -c "relo-no-such-tool --flag"`, dir)
	if res.Status != "skipped" || res.Note == "" {
		t.Fatalf("missing tool should skip: %+v", res)
	}
	res = (Runner{}).Run(context.Background(), `sh -c "echo FAIL: boom; exit 3"`, dir)
	if res.Status != "failed" || res.Code != 3 {
		t.Fatalf("a real failure must stay failed: %+v", res)
	}
	for _, out := range []string{"sh: line 1: tsx: command not found", "sh: 1: tsx: not found", `exec: "go": executable file not found in $PATH`} {
		if !IsEnvFailure(1, out) {
			t.Errorf("not recognised as environment failure: %q", out)
		}
	}
	if IsEnvFailure(1, "--- FAIL: TestUser (0.00s)\n    user.go:4: user: not found in db") {
		t.Error("test output mentioning not found must not count as environment failure")
	}
}

// A check killed for being slow is not a check that crashed: the note must say so.
func TestSlowCheckIsReportedAsStoppedForTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sleep")
	}
	old := CheckLimit
	CheckLimit = 150 * time.Millisecond
	defer func() { CheckLimit = old }()
	res := Runner{Root: t.TempDir()}.Run(context.Background(), "sleep 5", "")
	if res.Status != "failed" || !strings.Contains(res.Note, "did not finish within") {
		t.Fatalf("result = %+v", res)
	}
}

// Scoped narrows each stack's check to the packages or files the change touched. A command with no narrower form, and
// a change outside the stack, keep the command the project defines.
func TestScopedNarrowsToTheChangedPackagesAndFiles(t *testing.T) {
	steps := Scoped(t.TempDir(), []Step{{Command: "go test ./..."}, {Command: "pytest -q"}, {Command: "make test"}},
		[]string{"core/internal/prompt/prompt.go", "core/internal/style/style.go", "tests/test_api.py", "README.md"})
	if len(steps) != 3 {
		t.Fatalf("steps = %+v", steps)
	}
	if got := steps[0].Command; got != "go test ./core/internal/prompt ./core/internal/style" {
		t.Errorf("go step = %q; it must name the touched packages, not ./...", got)
	}
	if got := steps[1].Command; got != "pytest -q tests/test_api.py" {
		t.Errorf("pytest step = %q; it must name the touched file", got)
	}
	if got := steps[2].Command; got != "make test" {
		t.Errorf("make step = %q; a Makefile target has no narrower form", got)
	}
	for _, steps := range [][]Step{
		Scoped(t.TempDir(), []Step{{Command: "go test ./..."}}, nil),
		Scoped(t.TempDir(), []Step{{Command: "go test ./..."}}, []string{"README.md"}),
		Scoped(t.TempDir(), []Step{{Command: "go test ./..."}}, []string{"../elsewhere/x.go"}),
	} {
		if len(steps) != 1 || steps[0].Command != "go test ./..." {
			t.Fatalf("unscopable change narrowed the check: %+v", steps)
		}
	}
}

// A step Plan skipped for a missing tool must stay skipped after narrowing: the scoped command never becomes runnable.
func TestScopedKeepsSkippedStepsSkipped(t *testing.T) {
	steps := Scoped(t.TempDir(), []Step{
		{Command: "go test ./...", Skip: "go is not installed"},
		{Command: "pytest -q", Skip: "pytest is not installed"},
		{Command: "make test", Skip: "make is not installed"},
	}, []string{"core/internal/prompt/prompt.go", "tests/test_api.py"})
	if len(steps) != 3 {
		t.Fatalf("steps = %+v", steps)
	}
	if steps[0].Command != "go test ./core/internal/prompt" || steps[0].Skip != "go is not installed" {
		t.Errorf("skipped go step = %+v; it must narrow but keep its skip", steps[0])
	}
	if steps[1].Command != "pytest -q tests/test_api.py" || steps[1].Skip != "pytest is not installed" {
		t.Errorf("skipped pytest step = %+v; it must narrow but keep its skip", steps[1])
	}
	if steps[2].Command != "make test" || steps[2].Skip != "make is not installed" {
		t.Errorf("skipped make step = %+v; it has no narrower form and must keep its skip", steps[2])
	}
}

// A narrowed step whose tool is no longer on PATH must be skipped, not run as if the tool existed.
func TestScopedSkipsNarrowedStepWhenToolIsMissing(t *testing.T) {
	t.Setenv("PATH", "")
	steps := Scoped(t.TempDir(), []Step{{Command: "go test ./..."}}, []string{"core/internal/prompt/prompt.go"})
	if len(steps) != 1 || steps[0].Command != "go test ./core/internal/prompt" || steps[0].Skip == "" {
		t.Fatalf("narrowed step without its tool = %+v; it must stay skipped", steps)
	}
}
