package memory

import (
	"fmt"
	"strings"
	"testing"
)

// fakeView is a scripted View. Files absent from files are missing; names in unreadable fail with a non-absent error.
type fakeView struct {
	commits    map[string]bool
	ancestors  map[string]bool
	files      map[string]string
	unreadable map[string]bool
	dirty      bool
	err        error
}

func (v fakeView) HasCommit(c string) (bool, error) {
	return v.commits[c], v.err
}

func (v fakeView) IsAncestor(c string) (bool, error) {
	return v.ancestors[c], v.err
}

func (v fakeView) FileSHA256(p string) (string, error) {
	if v.unreadable[p] {
		return "", fmt.Errorf("permission denied: %s", p)
	}
	h, ok := v.files[p]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrFileAbsent, p)
	}
	return h, nil
}

func (v fakeView) Dirty(paths []string) (bool, error) {
	return v.dirty, nil
}

const (
	anchorCommit = "abcdef1234567"
	hashA        = "1111111111111111111111111111111111111111111111111111111111111111"
	hashB        = "2222222222222222222222222222222222222222222222222222222222222222"
)

// anchoredView is a clean view where the anchor commit is an ancestor and a.go and b.go hold their recorded hashes.
func anchoredView() fakeView {
	return fakeView{
		commits:   map[string]bool{anchorCommit: true},
		ancestors: map[string]bool{anchorCommit: true},
		files:     map[string]string{"a.go": hashA, "b.go": hashB},
	}
}

func anchoredEntry() Entry {
	return Entry{Evidence: []Evidence{{
		Type: EvidenceFileObservation, Ref: "ev-1", ObservedAt: phase2Now, Commit: anchorCommit,
		Files: []EvidenceFile{{Path: "a.go", SHA256: hashA}, {Path: "b.go", SHA256: hashB}},
	}}}
}

func TestFreshnessCurrentWhenEveryAnchorMatches(t *testing.T) {
	s, r := CheckFreshness(anchoredEntry(), anchoredView())
	if s != FreshCurrent || r != "" {
		t.Fatalf("got (%s, %q), want (current, empty)", s, r)
	}
}

func TestFreshnessMissingAnchorIsUnknownNeverCurrent(t *testing.T) {
	noCommit := anchoredEntry()
	noCommit.Evidence[0].Commit = ""
	if s, r := CheckFreshness(noCommit, anchoredView()); s != FreshUnknown || r != FreshNoAnchor {
		t.Fatalf("no anchor commit: got (%s, %q)", s, r)
	}
	if s, r := CheckFreshness(Entry{}, anchoredView()); s != FreshUnknown || r != FreshNoEvidence {
		t.Fatalf("no evidence: got (%s, %q)", s, r)
	}

	// The commit object is missing from this view's repository.
	v := anchoredView()
	v.commits = map[string]bool{}
	if s, r := CheckFreshness(anchoredEntry(), v); s != FreshUnknown || r != FreshCommitMissing {
		t.Fatalf("missing commit: got (%s, %q)", s, r)
	}

	// Git itself failed to answer: unknown, not stale and not current.
	v = anchoredView()
	v.err = fmt.Errorf("git: exec failed")
	if s, r := CheckFreshness(anchoredEntry(), v); s != FreshUnknown || r != FreshViewError {
		t.Fatalf("view error: got (%s, %q)", s, r)
	}
}

func TestFreshnessUnreadableFileIsUnknown(t *testing.T) {
	v := anchoredView()
	v.unreadable = map[string]bool{"b.go": true}
	if s, r := CheckFreshness(anchoredEntry(), v); s != FreshUnknown || r != FreshFileUnreadable {
		t.Fatalf("unreadable file: got (%s, %q)", s, r)
	}
}

func TestFreshnessDirtyViewIsUnknownNeverCurrent(t *testing.T) {
	v := anchoredView()
	v.dirty = true
	if s, r := CheckFreshness(anchoredEntry(), v); s != FreshUnknown || r != FreshDirtyView {
		t.Fatalf("dirty view: got (%s, %q)", s, r)
	}
}

func TestFreshnessChangedHashIsStale(t *testing.T) {
	v := anchoredView()
	v.files["a.go"] = strings.Repeat("3", 64)
	if s, r := CheckFreshness(anchoredEntry(), v); s != FreshStale || r != FreshFileChanged {
		t.Fatalf("changed hash: got (%s, %q)", s, r)
	}
}

func TestFreshnessMissingFileIsStale(t *testing.T) {
	v := anchoredView()
	delete(v.files, "a.go")
	if s, r := CheckFreshness(anchoredEntry(), v); s != FreshStale || r != FreshFileMissing {
		t.Fatalf("missing file: got (%s, %q)", s, r)
	}
}

func TestFreshnessNonAncestorCommitIsStale(t *testing.T) {
	// Rebased or diverged: the commit exists but is not reachable from this view's HEAD.
	v := anchoredView()
	v.ancestors = map[string]bool{}
	if s, r := CheckFreshness(anchoredEntry(), v); s != FreshStale || r != FreshNotAncestor {
		t.Fatalf("non-ancestor: got (%s, %q)", s, r)
	}
}

func TestFreshnessStaleBeatsUnknown(t *testing.T) {
	// A stale file is a definite change; an unknown second anchor must not hide it.
	e := anchoredEntry()
	e.Evidence = append(e.Evidence, Evidence{Type: EvidenceFileObservation, Ref: "ev-2", ObservedAt: phase2Now})
	v := anchoredView()
	v.files["a.go"] = strings.Repeat("3", 64)
	if s, r := CheckFreshness(e, v); s != FreshStale || r != FreshFileChanged {
		t.Fatalf("stale with unknown sibling: got (%s, %q)", s, r)
	}
}

func TestFreshnessAbsenceMarker(t *testing.T) {
	e := Entry{Evidence: []Evidence{{
		Type: EvidenceFileObservation, Ref: "ev-absent", ObservedAt: phase2Now, Commit: anchorCommit,
		Files: []EvidenceFile{{Path: "gone.go", Absent: true}},
	}}}
	v := anchoredView()
	if s, r := CheckFreshness(e, v); s != FreshCurrent || r != "" {
		t.Fatalf("still absent: got (%s, %q)", s, r)
	}
	// Existence later invalidates the absence claim.
	v.files["gone.go"] = hashA
	if s, r := CheckFreshness(e, v); s != FreshStale || r != FreshAbsenceContradict {
		t.Fatalf("file appeared: got (%s, %q)", s, r)
	}
}

func TestFreshnessRejectsInvalidAnchorsAsUnknown(t *testing.T) {
	bad := anchoredEntry()
	bad.Evidence[0].Commit = "not-a-commit"
	if s, r := CheckFreshness(bad, anchoredView()); s != FreshUnknown || r != FreshInvalidAnchor {
		t.Fatalf("malformed commit: got (%s, %q)", s, r)
	}
	escaping := anchoredEntry()
	escaping.Evidence[0].Files[0].Path = "../a.go"
	if s, r := CheckFreshness(escaping, anchoredView()); s != FreshUnknown || r != FreshInvalidAnchor {
		t.Fatalf("escaping path: got (%s, %q)", s, r)
	}
}

func TestFreshnessIsPerViewNotGlobal(t *testing.T) {
	// The same entry is current in one view and stale in a parallel branch's view; neither changes the entry.
	e := anchoredEntry()
	branch := anchoredView()
	branch.files["a.go"] = strings.Repeat("4", 64)
	main := anchoredView()
	if s, _ := CheckFreshness(e, main); s != FreshCurrent {
		t.Fatalf("main view: got %s, want current", s)
	}
	if s, _ := CheckFreshness(e, branch); s != FreshStale {
		t.Fatalf("branch view: got %s, want stale", s)
	}
	if e.Evidence[0].Files[0].SHA256 != hashA {
		t.Fatal("freshness changed the entry's recorded hash")
	}
}
