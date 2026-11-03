package freshness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

type fakeView struct {
	head        string
	headErr     error
	ancestor    bool
	ancestorErr error
	files       map[string][]byte
	readErr     map[string]error
}

func (v *fakeView) Head() (string, error) { return v.head, v.headErr }

func (v *fakeView) IsAncestor(_, _ string) (bool, error) {
	return v.ancestor, v.ancestorErr
}

func (v *fakeView) ReadFile(name string) ([]byte, error) {
	if err := v.readErr[name]; err != nil {
		return nil, err
	}
	data, ok := v.files[name]
	if !ok {
		return nil, errors.New("missing file")
	}
	return data, nil
}

func TestCaptureAndCheckCurrentAndStale(t *testing.T) {
	view := &fakeView{head: "head", ancestor: true, files: map[string][]byte{"a.txt": []byte("observed")}}
	anchor, err := Capture(view, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if anchor.Commit != "head" {
		t.Fatalf("commit = %q, want head", anchor.Commit)
	}
	sum := sha256.Sum256([]byte("observed"))
	if want := hex.EncodeToString(sum[:]); anchor.Files["a.txt"] != want {
		t.Fatalf("hash = %q, want %s", anchor.Files["a.txt"], want)
	}
	view.files["unrelated.txt"] = []byte("changed")
	if got := Check(view, anchor); got != Current {
		t.Fatalf("unchanged evidence = %s, want current", got)
	}
	view.files["a.txt"] = []byte("changed")
	if got := Check(view, anchor); got != Stale {
		t.Fatalf("changed evidence = %s, want stale", got)
	}
}

func TestCheckFailsClosed(t *testing.T) {
	sum := sha256.Sum256([]byte("observed"))
	hash := hex.EncodeToString(sum[:])
	tests := []struct {
		name   string
		view   *fakeView
		anchor Anchor
	}{
		{name: "non-ancestor", view: &fakeView{head: "head"}, anchor: Anchor{Commit: "base"}},
		{name: "missing head", view: &fakeView{headErr: errors.New("missing")}, anchor: Anchor{Commit: "base"}},
		{name: "ancestry failure", view: &fakeView{head: "head", ancestorErr: errors.New("unreachable")}, anchor: Anchor{Commit: "base"}},
		{name: "unreadable file", view: &fakeView{head: "head", ancestor: true, readErr: map[string]error{"a.txt": errors.New("denied")}}, anchor: Anchor{Commit: "base", Files: map[string]string{"a.txt": hash}}},
		{name: "oversized file", view: &fakeView{head: "head", ancestor: true, files: map[string][]byte{"a.txt": make([]byte, maxFileBytes+1)}}, anchor: Anchor{Commit: "base", Files: map[string]string{"a.txt": hash}}},
		{name: "invalid hash", view: &fakeView{head: "head", ancestor: true, files: map[string][]byte{"a.txt": []byte("observed")}}, anchor: Anchor{Commit: "base", Files: map[string]string{"a.txt": "not-a-hash"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Check(test.view, test.anchor); got != Unknown {
				t.Fatalf("Check = %s, want unknown", got)
			}
		})
	}
}

func TestUnknownOverridesChangedHash(t *testing.T) {
	sum := sha256.Sum256([]byte("old"))
	view := &fakeView{
		head: "head", ancestor: true,
		files:   map[string][]byte{"a.txt": []byte("changed")},
		readErr: map[string]error{"z.txt": errors.New("denied")},
	}
	anchor := Anchor{Commit: "base", Files: map[string]string{
		"a.txt": hex.EncodeToString(sum[:]),
		"z.txt": hex.EncodeToString(sum[:]),
	}}
	if got := Check(view, anchor); got != Unknown {
		t.Fatalf("Check = %s, want unknown", got)
	}
}

func TestCaptureRejectsInvalidPathsAndOversizedFiles(t *testing.T) {
	for _, name := range []string{"", "/absolute", "../escape", "a/../b", ".git/config", ".relo/state", `a\\b`} {
		view := &fakeView{head: "head", files: map[string][]byte{name: []byte("data")}}
		if _, err := Capture(view, []string{name}); err == nil {
			t.Errorf("Capture(%q) succeeded, want error", name)
		}
	}
	view := &fakeView{head: "head", files: map[string][]byte{"big.bin": make([]byte, maxFileBytes+1)}}
	if _, err := Capture(view, []string{"big.bin"}); err == nil {
		t.Fatal("Capture oversized file succeeded, want error")
	}
}
