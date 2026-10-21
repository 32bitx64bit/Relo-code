package vws

import (
	"errors"
	"fmt"
	"io/fs"
)

var (
	// ErrStaleRevision means a write named an expected revision and the workspace has moved on. Nothing was written.
	ErrStaleRevision = errors.New("vws: the workspace changed since the revision this write was based on")
	// ErrReadOnly means the workspace belongs to a read-only role.
	ErrReadOnly = errors.New("vws: the workspace is read-only")
	// ErrProtected means the path is Git or Relo metadata.
	ErrProtected = errors.New("vws: protected metadata path")
	// ErrInvalidPath means the path is empty, absolute, contains traversal or a forbidden character.
	ErrInvalidPath = errors.New("vws: invalid workspace path")
	// ErrSymlink means the operation would follow or replace a symbolic link.
	ErrSymlink = errors.New("vws: refusing to follow or replace a symlink")
	// ErrNotDir means a path component that should be a directory is a file.
	ErrNotDir = errors.New("vws: not a directory")
	// ErrIsDir means the path is a directory where a file was expected.
	ErrIsDir = errors.New("vws: is a directory")
	// ErrNotRegular means the path is not a regular file (a link or a submodule).
	ErrNotRegular = errors.New("vws: not a regular file")
	// ErrExists means a workspace for the agent already exists.
	ErrExists = errors.New("vws: workspace already exists")
	// ErrMaterialized means the agent's tools are running in a directory, so that directory is the authority and the
	// overlay cannot be edited directly until the execution is released or captured.
	ErrMaterialized = errors.New("vws: the workspace is materialized; its directory is authoritative")
	// ErrDisposed means the workspace was disposed.
	ErrDisposed = errors.New("vws: the workspace was disposed")
	// ErrTooLarge means a file is larger than the engine accepts.
	ErrTooLarge = errors.New("vws: file is too large")
	// ErrUnsupportedRepo means the repository converts file content on the way in or out (line endings, filters), so a
	// view of stored blobs would not match a checkout. Callers fall back to worktrees.
	ErrUnsupportedRepo = errors.New("vws: repository rewrites file content; use worktrees")
)

// notExist is an error that satisfies errors.Is(err, fs.ErrNotExist).
func notExist(path string) error { return fmt.Errorf("%w: %s", fs.ErrNotExist, path) }

// IsNotExist reports whether err says a path or workspace is absent.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// PathError ties a sentinel to the path it concerns.
type PathError struct {
	Path string
	Err  error
}

func (e *PathError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }
func (e *PathError) Unwrap() error { return e.Err }

func pathErr(path string, err error) error { return &PathError{Path: path, Err: err} }
