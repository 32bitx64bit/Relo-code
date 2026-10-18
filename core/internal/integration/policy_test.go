package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// usePolicy installs p for one test and restores the default afterwards.
func usePolicy(t *testing.T, p Policy) {
	t.Helper()
	SetPolicy(p)
	t.Cleanup(func() { SetPolicy(DefaultPolicy()) })
}

func TestPolicyDefaultsAndMatching(t *testing.T) {
	if got := CurrentPolicy(); len(got.Union) != 2 || got.TakeLatest != nil {
		t.Fatalf("default policy = %+v", got)
	}
	usePolicy(t, Policy{Union: []string{"notes/*.md"}, TakeLatest: []string{"Mod/Assemblies/*.dll"}})
	if !UnionMerges("notes/a.md") || UnionMerges("README.md") {
		t.Fatal("union policy did not match as configured")
	}
	if !TakesLatest("Mod/Assemblies/Mod.dll") || TakesLatest("Mod/Mod.dll") {
		t.Fatal("take-latest policy did not match as configured")
	}
}

// Each rule resolves a file that would otherwise be a conflict. The workspace is written only when Action says so.
func TestResolutionRulesSettleConflicts(t *testing.T) {
	binA := []byte{0, 1, 2}
	binUser := []byte{0, 9, 9}
	binAgent := []byte{0, 5, 5, 7}
	cases := []struct {
		name       string
		policy     Policy
		path       string
		ours       []byte // nil means absent
		theirs     []byte // nil means absent
		base       []byte
		hadBase    bool
		wantAction string
		wantRes    string
		wantOurs   []byte // workspace bytes after Merge
		wantRemove bool
		check      func(t *testing.T, merged []byte)
	}{
		{
			name:       "union appends from both sides",
			policy:     DefaultPolicy(),
			path:       "RELO_SESSION.md",
			ours:       []byte("# Log\n\n## User\nu\n"),
			theirs:     []byte("# Log\n\n## Agent\na\n"),
			base:       []byte("# Log\n"),
			hadBase:    true,
			wantAction: "merged",
			wantRes:    "union",
			check: func(t *testing.T, merged []byte) {
				for _, want := range []string{"## User", "u\n", "## Agent", "a\n"} {
					if !bytes.Contains(merged, []byte(want)) {
						t.Fatalf("union result lacks %q: %q", want, merged)
					}
				}
				if bytes.Contains(merged, []byte("<<<<<<<")) {
					t.Fatalf("union result has markers: %q", merged)
				}
			},
		},
		{
			name:       "whitespace-only add_add keeps the workspace file",
			policy:     DefaultPolicy(),
			path:       "src/app.py",
			ours:       []byte("a = 1\r\nb = 2  \n\n\n"),
			theirs:     []byte("a = 1\nb = 2\n"),
			hadBase:    false,
			wantAction: "unchanged",
			wantRes:    "whitespace",
			wantOurs:   []byte("a = 1\r\nb = 2  \n\n\n"),
		},
		{
			name:       "take-latest replaces a conflicting binary",
			policy:     Policy{TakeLatest: []string{"Mod/Assemblies/*.dll"}},
			path:       "Mod/Assemblies/Mod.dll",
			ours:       binUser,
			theirs:     binAgent,
			base:       binA,
			hadBase:    true,
			wantAction: "updated",
			wantRes:    "take-latest",
			wantOurs:   binAgent,
		},
		{
			name:       "take-latest creates the file when the workspace lacks it",
			policy:     Policy{TakeLatest: []string{"Mod/Assemblies/*.dll"}},
			path:       "Mod/Assemblies/Mod.dll",
			ours:       nil,
			theirs:     binAgent,
			base:       binA,
			hadBase:    true,
			wantAction: "created",
			wantRes:    "take-latest",
			wantOurs:   binAgent,
		},
		{
			name:       "take-latest honours an agent deletion",
			policy:     Policy{TakeLatest: []string{"Mod/Assemblies/*.dll"}},
			path:       "Mod/Assemblies/Mod.dll",
			ours:       binUser,
			theirs:     nil,
			base:       binA,
			hadBase:    true,
			wantAction: "deleted",
			wantRes:    "take-latest",
			wantRemove: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usePolicy(t, c.policy)
			root, wt := t.TempDir(), t.TempDir()
			if c.ours != nil {
				writeBytes(t, root, c.path, c.ours)
			}
			if c.theirs != nil {
				writeBytes(t, wt, c.path, c.theirs)
			}
			a, err := Analyze(root, wt, c.path, c.base, c.hadBase)
			if err != nil {
				t.Fatal(err)
			}
			if a.Kind != "" || a.Action != c.wantAction || a.Resolved != c.wantRes {
				t.Fatalf("analysis = %+v, want action %q resolved %q", a, c.wantAction, c.wantRes)
			}
			if reason, err := Check(root, wt, c.path, c.base, c.hadBase); err != nil || reason != "" {
				t.Fatalf("Check = %q, %v; want no conflict", reason, err)
			}
			out, err := Merge(root, wt, c.path, c.base, c.hadBase)
			if err != nil || out.Action != c.wantAction {
				t.Fatalf("Merge = %+v, %v", out, err)
			}
			got, exists, err := readTarget(root, c.path)
			if err != nil {
				t.Fatal(err)
			}
			if c.wantRemove {
				if exists {
					t.Fatalf("workspace file should be removed")
				}
				return
			}
			if c.wantOurs != nil && (!exists || !bytes.Equal(got, c.wantOurs)) {
				t.Fatalf("workspace = %q, want %q", got, c.wantOurs)
			}
			if c.check != nil {
				c.check(t, got)
			}
		})
	}
}

// Each rule refuses what it does not cover. The conflict kind is what keeps the file out of the LLM path and
// away from an automatic write, so these must stay conflicts.
func TestResolutionRulesDoNotCoverOtherCases(t *testing.T) {
	cases := []struct {
		name     string
		policy   Policy
		path     string
		ours     string
		theirs   string
		base     string
		hadBase  bool
		wantKind string
	}{
		{
			name:     "union refused when ours changed a base line",
			policy:   DefaultPolicy(),
			path:     "RELO_TASKS.md",
			ours:     "a\nb\nC\n",
			theirs:   "a\nb\nc\nagent\n",
			base:     "a\nb\nc\n",
			hadBase:  true,
			wantKind: KindOverlap,
		},
		{
			name:     "union refused when ours deleted a base line",
			policy:   DefaultPolicy(),
			path:     "RELO_TASKS.md",
			ours:     "a\nb\n",
			theirs:   "a\nb\nc\nagent\n",
			base:     "a\nb\nc\n",
			hadBase:  true,
			wantKind: KindOverlap,
		},
		{
			name:     "union refused when theirs changed a base line",
			policy:   DefaultPolicy(),
			path:     "notes.txt",
			ours:     "a\nb\nc\nuser\n",
			theirs:   "a\nB\n",
			base:     "a\nb\nc\n",
			hadBase:  true,
			wantKind: KindOverlap,
		},
		{
			name:     "union not applied to go files",
			policy:   DefaultPolicy(),
			path:     "main.go",
			ours:     "one\ntwo\nours\n",
			theirs:   "one\ntwo\ntheirs\n",
			base:     "one\ntwo\n",
			hadBase:  true,
			wantKind: KindOverlap,
		},
		{
			name:     "whitespace rule stops at interior whitespace",
			policy:   DefaultPolicy(),
			path:     "config.yaml",
			ours:     "x:  1\n",
			theirs:   "x: 1\n",
			hadBase:  false,
			wantKind: KindAddAdd,
		},
		{
			name:     "whitespace rule stops at different content",
			policy:   DefaultPolicy(),
			path:     "a.txt",
			ours:     "one\n",
			theirs:   "two\n",
			hadBase:  false,
			wantKind: KindAddAdd,
		},
		{
			name:     "take-latest not applied when the path is not in the policy",
			policy:   Policy{TakeLatest: []string{"Other/*.dll"}},
			path:     "Mod/Assemblies/Mod.dll",
			ours:     "\x00user",
			theirs:   "\x00agent",
			base:     "\x00orig",
			hadBase:  true,
			wantKind: KindBinary,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usePolicy(t, c.policy)
			root, wt := t.TempDir(), t.TempDir()
			write(t, root, c.path, c.ours)
			write(t, wt, c.path, c.theirs)
			var base []byte
			if c.hadBase {
				base = []byte(c.base)
			}
			a, err := Analyze(root, wt, c.path, base, c.hadBase)
			if err != nil {
				t.Fatal(err)
			}
			if a.Kind != c.wantKind || a.Resolved != "" {
				t.Fatalf("analysis = %+v, want kind %q unresolved", a, c.wantKind)
			}
		})
	}
}

// Protected and symlink paths stay conflicts even when the take-latest policy matches them.
func TestTakeLatestNeverOverridesProtectedOrSymlinkPaths(t *testing.T) {
	usePolicy(t, Policy{TakeLatest: []string{"**"}})

	root, wt := t.TempDir(), t.TempDir()
	write(t, root, ".relo/x", "user\n")
	write(t, wt, ".relo/x", "agent\n")
	a, err := Analyze(root, wt, ".relo/x", []byte("orig\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindProtected || a.Resolved != "" {
		t.Fatalf("protected path: %+v", a)
	}

	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, wt = t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(wt, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	a, err = Analyze(root, wt, "link.txt", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindSymlink || a.Resolved != "" {
		t.Fatalf("symlink path: %+v", a)
	}
}

// The whitespace rule must not fire on a file that merely looks similar: the canonical comparison trims only the
// line ends and trailing blank lines.
func TestSameIgnoringTrailingSpace(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a\nb\n", "a\r\nb", true},
		{"a \t\n\n\n", "a\n", true},
		{"a\n  b\n", "a\nb\n", false},
		{"a\x00", "a\x00", false},
	}
	for _, c := range cases {
		if got := sameIgnoringTrailingSpace([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("sameIgnoringTrailingSpace(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func writeBytes(t *testing.T, dir, rel string, body []byte) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
