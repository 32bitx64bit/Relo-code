package vws

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// gitT runs git in dir and fails the test on error.
func gitT(t testing.TB, dir string, args ...string) string {
	t.Helper()
	return gitEnvT(t, dir, nil, args...)
}

func gitEnvT(t testing.TB, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// newRepo makes a repository whose single commit holds files. Executable files are marked with a "*" suffix on
// the name. The user's global git config is ignored so a developer's settings cannot change the result.
func newRepo(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.email", "t@t")
	gitT(t, dir, "config", "user.name", "T")
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "*") {
			name, mode = strings.TrimSuffix(name, "*"), 0o755
		}
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func openEngine(t testing.TB, dir string, opts ...func(*Options)) *Engine {
	t.Helper()
	o := Options{}
	for _, f := range opts {
		f(&o)
	}
	e, err := Open(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func newWS(t testing.TB, e *Engine, id string) *Workspace {
	t.Helper()
	w, err := e.CreateVirtualWorkspace(id, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func mustRead(t testing.TB, w *Workspace, path string) string {
	t.Helper()
	data, err := w.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func mustWrite(t testing.TB, w *Workspace, path, content string) {
	t.Helper()
	if err := w.WriteFile(path, []byte(content)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustNotExist(t testing.TB, w *Workspace, path string) {
	t.Helper()
	if _, err := w.ReadFile(path); !IsNotExist(err) {
		t.Fatalf("read %s: want not-exist, got %v", path, err)
	}
}

// userState captures everything about the user's checkout the engine must never change.
type userState struct{ head, branch, status, index, refs string }

func captureUser(t testing.TB, dir string) userState {
	t.Helper()
	idx, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	return userState{
		head:   gitT(t, dir, "rev-parse", "HEAD"),
		branch: gitT(t, dir, "symbolic-ref", "HEAD"),
		status: gitT(t, dir, "status", "--porcelain", "--untracked-files=all", "--", ".", ":(exclude).relo"),
		index:  string(idx),
		refs:   gitT(t, dir, "for-each-ref", "refs/heads/main", "refs/tags"), // the agents' own branches are refs/heads/relo/*
	}
}

func (a userState) equal(b userState) bool { return a == b }

func sortedPaths(changes []FileChange) []string {
	var out []string
	for _, c := range changes {
		out = append(out, c.Kind[:1]+":"+c.Path)
	}
	sort.Strings(out)
	return out
}

func samePaths(t testing.TB, got []FileChange, want ...string) {
	t.Helper()
	sort.Strings(want)
	g := sortedPaths(got)
	if strings.Join(g, ",") != strings.Join(want, ",") {
		t.Fatalf("changes = %v, want %v", g, want)
	}
}
