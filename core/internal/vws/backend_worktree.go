package vws

import (
	"context"
	"os"
	"strings"
	"time"
)

// WorktreeMaterializer lays a revision out as a detached Git worktree. It is the conservative backend: every tool
// that works in a worktree today works here, and the directory is a real repository checkout as OpenCode expects.
// Cheaper backends can replace it later without changing the engine.
type WorktreeMaterializer struct {
	// Prepare runs after the checkout, for per-directory setup such as linking installed dependencies.
	Prepare func(dir string)
}

func (m *WorktreeMaterializer) Name() string { return "worktree" }

func (m *WorktreeMaterializer) Materialize(ctx context.Context, g Git, req MaterializeRequest) error {
	root := gitRoot(g, req.Dir)
	if _, err := g.Run(root, nil, nil, "worktree", "add", "--detach", "--force", req.Dir, req.Commit); err != nil {
		return err
	}
	if m.Prepare != nil {
		m.Prepare(req.Dir)
	}
	return nil
}

// Scan stages everything in the directory in the worktree's own index and compares that with the revision it was
// cut from. The index is the worktree's private one: the user's is never involved, and ignore rules apply exactly
// as they do to the worktrees Relo has always used.
func (m *WorktreeMaterializer) Scan(ctx context.Context, g Git, req MaterializeRequest) ([]Captured, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A file that changes while it is read (a running dev server, a watcher) fails the add; it is read again.
		if _, err = g.Run(req.Dir, nil, nil, "add", "-A"); err == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	out, err := g.Run(req.Dir, nil, nil, "diff-index", "--cached", "--raw", "--no-renames", "--no-abbrev", "-z", req.Commit)
	if err != nil {
		return nil, err
	}
	zero := strings.Repeat("0", len(req.Commit))
	var captured []Captured
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(fields); {
		meta := fields[i]
		if !strings.HasPrefix(meta, ":") {
			i++
			continue
		}
		parts := strings.Fields(meta[1:])
		if len(parts) >= 5 {
			newMode := parseMode(parts[1])
			c := Captured{Path: fields[i+1], Mode: newMode, OID: parts[3]}
			if parts[4][0] == 'D' || parts[3] == zero {
				c = Captured{Path: fields[i+1], Deleted: true}
			}
			captured = append(captured, c)
		}
		i += 2
	}
	return captured, nil
}

func (m *WorktreeMaterializer) Release(ctx context.Context, g Git, dir string) error {
	root := gitRoot(g, dir)
	if _, err := os.Lstat(dir); err != nil {
		_, _ = g.Run(root, nil, nil, "worktree", "prune")
		return nil
	}
	if _, err := g.Run(root, nil, nil, "worktree", "remove", "--force", dir); err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			return err
		}
	}
	_, _ = g.Run(root, nil, nil, "worktree", "prune")
	return nil
}

func parseMode(s string) uint32 {
	var mode uint32
	for _, c := range s {
		mode = mode<<3 | uint32(c-'0')
	}
	return mode
}

// gitRoot returns a directory git can be run in before dir exists: the repository the engine opened. The
// materializer is only handed the directory, so the engine's root is found through the Git adapter.
func gitRoot(g Git, dir string) string {
	if a, ok := g.(gitAdapter); ok {
		return a.r.root
	}
	return dir
}
