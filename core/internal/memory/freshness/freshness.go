// Package freshness captures and checks view-bound evidence anchors.
package freshness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

const maxFileBytes = 1 << 20

// Status describes whether an anchor is valid for the current view.
type Status string

const (
	Current Status = "current"
	Stale   Status = "stale"
	Unknown Status = "unknown"
)

// Anchor pins evidence to a commit and the SHA-256 of each observed file.
type Anchor struct {
	Commit string            `json:"commit"`
	Files  map[string]string `json:"files"`
}

// View exposes the commit and file bytes visible to one agent. ReadFile must
// include uncommitted changes in that view rather than reading only HEAD.
type View interface {
	Head() (string, error)
	IsAncestor(ancestor, head string) (bool, error)
	ReadFile(path string) ([]byte, error)
}

// Capture records the current commit and file hashes from view.
func Capture(view View, paths []string) (Anchor, error) {
	if view == nil {
		return Anchor{}, errors.New("freshness: nil view")
	}
	head, err := view.Head()
	if err != nil {
		return Anchor{}, fmt.Errorf("freshness: resolve head: %w", err)
	}
	if head == "" {
		return Anchor{}, errors.New("freshness: empty head commit")
	}
	anchor := Anchor{Commit: head, Files: make(map[string]string, len(paths))}
	ordered := append([]string(nil), paths...)
	sort.Strings(ordered)
	for _, name := range ordered {
		if !validPath(name) {
			return Anchor{}, fmt.Errorf("freshness: invalid path %q", name)
		}
		data, err := view.ReadFile(name)
		if err != nil {
			return Anchor{}, fmt.Errorf("freshness: read %q: %w", name, err)
		}
		if len(data) > maxFileBytes {
			return Anchor{}, fmt.Errorf("freshness: %q exceeds %d bytes", name, maxFileBytes)
		}
		sum := sha256.Sum256(data)
		anchor.Files[name] = hex.EncodeToString(sum[:])
	}
	return anchor, nil
}

// Check compares anchor with view. Unknown takes precedence over Stale: every
// cited file must be readable and bounded before changed bytes can stale it.
func Check(view View, anchor Anchor) Status {
	if view == nil || anchor.Commit == "" {
		return Unknown
	}
	head, err := view.Head()
	if err != nil || head == "" {
		return Unknown
	}
	ancestor, err := view.IsAncestor(anchor.Commit, head)
	if err != nil || !ancestor {
		return Unknown
	}

	paths := make([]string, 0, len(anchor.Files))
	for name := range anchor.Files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	stale := false
	for _, name := range paths {
		want := anchor.Files[name]
		if !validPath(name) || !validSHA256(want) {
			return Unknown
		}
		data, err := view.ReadFile(name)
		if err != nil || len(data) > maxFileBytes {
			return Unknown
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, want) {
			stale = true
		}
	}
	if stale {
		return Stale
	}
	return Current
}

func validPath(name string) bool {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, `\:`) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" || part == ".relo" {
			return false
		}
	}
	return true
}

func validSHA256(sum string) bool {
	if len(sum) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(sum)
	return err == nil
}
