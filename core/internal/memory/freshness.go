package memory

import (
	"errors"
)

// Freshness is whether an entry's evidence still holds in one view (design §5). Only FreshCurrent may be recalled.
type Freshness string

// Freshness states. Unknown is never current: a missing anchor, unreadable input or dirty view fails closed.
const (
	FreshCurrent Freshness = "current"
	FreshStale   Freshness = "stale"
	FreshUnknown Freshness = "unknown"
)

// FreshReason is a stable code naming why an entry is not current. It holds no payload text. Empty when current.
type FreshReason string

// Freshness reasons.
const (
	FreshNoEvidence        FreshReason = "no_evidence"
	FreshNoAnchor          FreshReason = "no_anchor_commit"
	FreshInvalidAnchor     FreshReason = "invalid_anchor"
	FreshCommitMissing     FreshReason = "commit_missing"
	FreshNotAncestor       FreshReason = "commit_not_ancestor"
	FreshFileMissing       FreshReason = "file_missing"
	FreshFileChanged       FreshReason = "file_hash_changed"
	FreshFileUnreadable    FreshReason = "file_unreadable"
	FreshAbsenceContradict FreshReason = "absence_contradicted"
	FreshDirtyView         FreshReason = "dirty_view"
	FreshViewError         FreshReason = "view_error"
)

// ErrFileAbsent is wrapped by View.FileSHA256 when the path does not exist in the view. Any other error means the file
// could not be read, which is unknown rather than stale.
var ErrFileAbsent = errors.New("file absent")

// View is one recipient's workspace or worktree snapshot, including uncommitted on-disk bytes. Each method reads the
// view as it is now; nothing is cached across calls. Implementations never follow a link out of the view.
type View interface {
	// HasCommit reports whether the commit object exists in the view's repository.
	HasCommit(commit string) (bool, error)
	// IsAncestor reports whether commit is the view's current commit or one of its ancestors.
	IsAncestor(commit string) (bool, error)
	// FileSHA256 hashes the workspace-relative path as the view holds it now. A missing path wraps ErrFileAbsent.
	FileSHA256(path string) (string, error)
	// Dirty reports uncommitted changes under paths, or anywhere in the view when paths is empty.
	Dirty(paths []string) (bool, error)
}

// CheckFreshness evaluates every evidence anchor of e against view v. A stale anchor makes the entry stale at once.
// Otherwise any unknown anchor, or a dirty view, makes it unknown. It is current only when every anchor matches.
// The first unknown reason is returned. Freshness never changes e and never writes to the store.
func CheckFreshness(e Entry, v View) (Freshness, FreshReason) {
	if len(e.Evidence) == 0 {
		return FreshUnknown, FreshNoEvidence
	}
	state, reason := FreshCurrent, FreshReason("")
	var paths []string
	for _, ev := range e.Evidence {
		for _, f := range ev.Files {
			paths = append(paths, f.Path)
		}
		s, r := checkAnchor(ev, v)
		if s == FreshStale {
			return s, r
		}
		if s == FreshUnknown && state == FreshCurrent {
			state, reason = s, r
		}
	}
	dirty, err := v.Dirty(paths)
	if err != nil {
		if state == FreshCurrent {
			return FreshUnknown, FreshViewError
		}
		return state, reason
	}
	if dirty && state == FreshCurrent {
		return FreshUnknown, FreshDirtyView
	}
	return state, reason
}

// checkAnchor checks one evidence record: its commit must exist and be the view's commit or an ancestor, and every
// file must hash to its recorded SHA-256, or be absent when the record carries an absence marker.
func checkAnchor(ev Evidence, v View) (Freshness, FreshReason) {
	if ev.Commit == "" {
		return FreshUnknown, FreshNoAnchor
	}
	if !commitPattern.MatchString(ev.Commit) {
		return FreshUnknown, FreshInvalidAnchor
	}
	has, err := v.HasCommit(ev.Commit)
	if err != nil {
		return FreshUnknown, FreshViewError
	}
	if !has {
		return FreshUnknown, FreshCommitMissing
	}
	anc, err := v.IsAncestor(ev.Commit)
	if err != nil {
		return FreshUnknown, FreshViewError
	}
	if !anc {
		return FreshStale, FreshNotAncestor
	}
	for _, f := range ev.Files {
		if !ValidEvidencePath(f.Path) {
			return FreshUnknown, FreshInvalidAnchor
		}
		got, err := v.FileSHA256(f.Path)
		if f.Absent {
			switch {
			case errors.Is(err, ErrFileAbsent):
				continue
			case err == nil:
				return FreshStale, FreshAbsenceContradict
			default:
				return FreshUnknown, FreshFileUnreadable
			}
		}
		if errors.Is(err, ErrFileAbsent) {
			return FreshStale, FreshFileMissing
		}
		if err != nil {
			return FreshUnknown, FreshFileUnreadable
		}
		if !sha256Pattern.MatchString(f.SHA256) {
			return FreshUnknown, FreshInvalidAnchor
		}
		if got != f.SHA256 {
			return FreshStale, FreshFileChanged
		}
	}
	return FreshCurrent, ""
}
