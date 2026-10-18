package orchestrator

import (
	"errors"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/workspace"
)

func snapshotNotices(c *Coord, needle string) int {
	n := 0
	for _, m := range c.Current().Messages {
		if m.Role == "system" && strings.Contains(m.Text, needle) {
			n++
		}
	}
	return n
}

func TestSnapshotNoticesSurfaceOncePerCondition(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.CreateSession(dir, "x", ModeBuild)

	c.noticeSnapshot(true, true)
	c.noticeSnapshot(true, true)
	if got := snapshotNotices(c, "cut from HEAD"); got != 1 {
		t.Fatalf("HEAD-fallback notices = %d, want once", got)
	}
	if got := snapshotNotices(c, "more than 2000 untracked files"); got != 1 {
		t.Fatalf("truncation notices = %d, want once", got)
	}

	c.noticeSnapshotFailure(errors.New("tree changed"))
	c.noticeSnapshotFailure(errors.New("tree changed"))
	if got := snapshotNotices(c, "no agent was started"); got != 1 {
		t.Fatalf("fail-closed notices = %d, want once", got)
	}

	// A new session gets its own notice, so an old quiet period does not hide a new problem.
	c.CreateSession(dir, "y", ModeBuild)
	c.noticeSnapshot(true, false)
	if got := snapshotNotices(c, "cut from HEAD"); got != 1 {
		t.Fatalf("new-session fallback notices = %d, want one for the new session", got)
	}
}
