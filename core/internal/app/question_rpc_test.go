package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

// newQuestionTestApp builds an App with no runtime, so the answer's follow-up Submit fails fast and touches nothing.
func newQuestionTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Close() })
	core.Coord.CreateSession(dir, "question", orchestrator.ModeBuild)
	return core
}

func TestQuestionAnswerRecordsUserAnswerOnBlockedTask(t *testing.T) {
	core := newQuestionTestApp(t)
	core.Coord.Graph.Add(taskgraph.Task{ID: "q1", Title: "q1", Status: taskgraph.Asking,
		BlockedOn: &taskgraph.BlockedOn{Question: "which?", BlockedAt: time.Now().UTC()}})
	notes := collectSystemNotes(core)
	if _, err := core.Handle("question.answer", json.RawMessage(`{"taskId":"q1","answer":"use the new one"}`)); err != nil {
		t.Fatal(err)
	}
	got, ok := core.Coord.Graph.Get("q1")
	if !ok || got.Status != taskgraph.Queued || got.BlockedOn == nil {
		t.Fatalf("task after answer = %+v", got)
	}
	if got.BlockedOn.AnsweredBy != taskgraph.AnsweredByUser || got.BlockedOn.Resolution != "use the new one" {
		t.Fatalf("answer provenance = %+v", got.BlockedOn)
	}
	// The mock runtime has no session, so the follow-up Submit fails. AnswerQuestion also notes the answer,
	// so expect that one plus exactly one failed-resume notice naming the task.
	waitFor(t, func() bool { return failedResumeNotes(notes()) > 0 })
	// Give a duplicate notice a chance to appear before counting.
	time.Sleep(50 * time.Millisecond)
	var resume []string
	for _, n := range notes() {
		if strings.Contains(n, "did not start") {
			resume = append(resume, n)
		}
	}
	if len(resume) != 1 {
		t.Fatalf("notices = %q, want exactly one failed-resume notice", notes())
	}
	for _, want := range []string{"q1", "recorded", "did not start", "next message"} {
		if !strings.Contains(resume[0], want) {
			t.Fatalf("notice %q does not contain %q", resume[0], want)
		}
	}
}

// collectSystemNotes returns the system chat notices emitted from now on. It is safe to read while the
// async question.answer resume runs.
func collectSystemNotes(core *App) func() []string {
	var mu sync.Mutex
	var out []string
	core.Coord.SetEmitter(func(name string, payload any) {
		if name != "chat.message" {
			return
		}
		if m, ok := payload.(orchestrator.Chat); ok && m.Role == "system" {
			mu.Lock()
			out = append(out, m.Text)
			mu.Unlock()
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), out...)
	}
}

// failedResumeNotes counts the system notices that report a failed question.answer resume.
func failedResumeNotes(notes []string) int {
	n := 0
	for _, text := range notes {
		if strings.Contains(text, "did not start") {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

func TestQuestionAnswerRejectsUnknownOrMissingTask(t *testing.T) {
	core := newQuestionTestApp(t)
	if _, err := core.Handle("question.answer", json.RawMessage(`{"taskId":"nope","answer":"x"}`)); err == nil {
		t.Fatal("unknown taskId accepted")
	}
	if _, err := core.Handle("question.answer", json.RawMessage(`{"answer":"x"}`)); err == nil {
		t.Fatal("missing taskId accepted")
	}
}

func TestQuestionAnswerSuccessfulResumeEmitsNoNotice(t *testing.T) {
	core := newQuestionTestApp(t)
	core.Coord.SetRuntime(runtime.NewMock())
	core.Coord.Graph.Add(taskgraph.Task{ID: "q2", Title: "q2", Status: taskgraph.Asking,
		BlockedOn: &taskgraph.BlockedOn{Question: "which?", BlockedAt: time.Now().UTC()}})
	notes := collectSystemNotes(core)
	if _, err := core.Handle("question.answer", json.RawMessage(`{"taskId":"q2","answer":"the old one"}`)); err != nil {
		t.Fatal(err)
	}
	// The resume emits the user's own message; give any erroneous notice the same chance to appear.
	waitFor(t, func() bool {
		for _, m := range core.Coord.Current().Messages {
			if m.Role == "user" && strings.Contains(m.Text, "q2") {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	if n := failedResumeNotes(notes()); n != 0 {
		t.Fatalf("successful resume emitted %d failed-resume notices: %q", n, notes())
	}
}

func TestQuestionDelegateTogglesSessionFlag(t *testing.T) {
	core := newQuestionTestApp(t)
	if _, err := core.Handle("question.delegate", json.RawMessage(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	if !core.Coord.Current().DelegateAnswers {
		t.Fatal("delegate not enabled")
	}
	if _, err := core.Handle("question.delegate", json.RawMessage(`{"enabled":false}`)); err != nil {
		t.Fatal(err)
	}
	if core.Coord.Current().DelegateAnswers {
		t.Fatal("delegate not disabled")
	}
	if _, err := core.Handle("question.delegate", json.RawMessage(`{"enabled":"yes"}`)); err == nil {
		t.Fatal("non-boolean enabled accepted")
	}
}
