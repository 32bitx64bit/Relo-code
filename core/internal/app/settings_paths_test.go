package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/logging"
)

// pathSettingsRepo is a git workspace with one commit, so the core can cut an agent tree from it.
func pathSettingsRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "relo@test")
	gitRun(t, dir, "config", "user.name", "Relo")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "init")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestApplySettingsPathLists(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	t.Cleanup(func() { integration.SetPolicy(integration.DefaultPolicy()) })

	core.applySettings(map[string]any{
		"unionPaths":      []any{" *.log ", 42, "", "   ", strings.Repeat("u", 257), "docs/**/*.md"},
		"takeLatestPaths": []any{"**/Assemblies/*.dll"},
	})
	want := integration.Policy{
		Union:      []string{"*.log", "docs/**/*.md"},
		TakeLatest: []string{"**/Assemblies/*.dll"},
	}
	if got := integration.CurrentPolicy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %+v, want %+v", got, want)
	}

	// Sending only unionPaths leaves TakeLatest alone; an empty array clears Union.
	core.applySettings(map[string]any{"unionPaths": []any{}})
	if got := integration.CurrentPolicy(); len(got.Union) != 0 || !reflect.DeepEqual(got.TakeLatest, want.TakeLatest) {
		t.Fatalf("policy after unionPaths only = %+v", got)
	}

	// Entries past the cap are dropped, and a value that is not an array leaves the policy alone.
	many := make([]any, 0, 150)
	for i := 0; i < 150; i++ {
		many = append(many, "p")
	}
	core.applySettings(map[string]any{"takeLatestPaths": many, "unionPaths": "*.md"})
	got := integration.CurrentPolicy()
	if len(got.TakeLatest) != maxPathEntries || len(got.Union) != 0 {
		t.Fatalf("policy with cap = %d take-latest, %d union", len(got.TakeLatest), len(got.Union))
	}
}

func TestApplySettingsIgnorePathsHidesBuildOutput(t *testing.T) {
	dir := pathSettingsRepo(t)
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, ".relo", "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatal(err)
	}
	tree, err := mountTree(core, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"obj/x.dll", "src/x.go"} {
		if err := os.MkdirAll(filepath.Join(tree.Path, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tree.Path, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	assertChanged := func(label, want string) {
		t.Helper()
		changed, err := core.Coord.WS.Changed("agent-1")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(changed, ","); got != want {
			t.Fatalf("%s: Changed = %q, want %q", label, got, want)
		}
	}
	assertChanged("before settings", "obj/x.dll,src/x.go")

	core.applySettings(map[string]any{"ignorePaths": []any{"**/obj/**", "  ", 7}})
	assertChanged("after ignorePaths", "src/x.go")
}
