package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
)

// setSnapshotClock moves the notice clock to *now for the rest of the test.
func setSnapshotClock(t *testing.T, now *time.Time) {
	t.Helper()
	old := snapshotNoticeClock
	snapshotNoticeClock = func() time.Time { return *now }
	t.Cleanup(func() { snapshotNoticeClock = old })
}

func TestSnapshotFallbackRepeatsAtMostOncePerMinute(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	setSnapshotClock(t, &now)
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "x", ModeBuild)

	c.noticeSnapshot(true, false)
	now = now.Add(time.Second)
	c.noticeSnapshot(true, false)
	if got := snapshotNotices(c, "cut from HEAD"); got != 1 {
		t.Fatalf("fallback notices within 60s = %d, want 1", got)
	}

	now = now.Add(60 * time.Second) // 61 s after the first post
	c.noticeSnapshot(true, false)
	if got := snapshotNotices(c, "cut from HEAD"); got != 2 {
		t.Fatalf("fallback notices after 61s = %d, want 2", got)
	}
	if got := snapshotNotices(c, "including work you already accepted"); got != 2 {
		t.Fatalf("fallback text lacks the accepted-work warning (%d matches)", got)
	}
}

func TestSnapshotFailureRepeatsAtMostOncePerMinute(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	setSnapshotClock(t, &now)
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "x", ModeBuild)

	c.noticeSnapshotFailure(errors.New("tree changed"))
	now = now.Add(30 * time.Second)
	c.noticeSnapshotFailure(errors.New("tree changed"))
	if got := snapshotNotices(c, "no agent was started"); got != 1 {
		t.Fatalf("fail-closed notices within 60s = %d, want 1", got)
	}

	now = now.Add(31 * time.Second) // 61 s after the first post
	c.noticeSnapshotFailure(errors.New("tree changed"))
	if got := snapshotNotices(c, "no agent was started"); got != 2 {
		t.Fatalf("fail-closed notices after 61s = %d, want 2", got)
	}
}

func TestSnapshotTruncationStaysOncePerSession(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	setSnapshotClock(t, &now)
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "x", ModeBuild)

	c.noticeSnapshot(false, true)
	now = now.Add(10 * time.Minute)
	c.noticeSnapshot(false, true)
	if got := snapshotNotices(c, "more than 2000 untracked files"); got != 1 {
		t.Fatalf("truncation notices = %d, want once", got)
	}
}

func TestSnapshotNoticesAreIndependentPerSession(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	setSnapshotClock(t, &now)
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "first", ModeBuild)
	c.noticeSnapshot(true, false)

	// A second session is not silenced by the first session's notice posted a moment ago.
	c.CreateSession(t.TempDir(), "second", ModeBuild)
	now = now.Add(time.Second)
	c.noticeSnapshot(true, false)
	if got := snapshotNotices(c, "cut from HEAD"); got != 1 {
		t.Fatalf("second session fallback notices = %d, want 1", got)
	}
}
