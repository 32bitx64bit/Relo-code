package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/metrics"
	"github.com/relocode/relo/internal/orchestrator"
)

func TestInitializeHandleReturns(t *testing.T) {
	dir := t.TempDir()
	log := logging.New(&bytes.Buffer{}, logging.Error)
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	done := make(chan error, 1)
	go func() {
		_, err := core.Handle("initialize", json.RawMessage(`{"runtime":"mock","workspace":`+jq(dir)+`}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initialize handle hung")
	}
}

func TestInitializeHandleWithIPC(t *testing.T) {
	dir := t.TempDir()
	log := logging.New(&bytes.Buffer{}, logging.Error)
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	var out bytes.Buffer
	srv := ipc.New(strings.NewReader(""), &out, core.Handle)
	core.AttachIPC(srv)
	done := make(chan error, 1)
	go func() {
		_, err := core.Handle("initialize", json.RawMessage(`{"runtime":"mock"}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("initialize+ipc hung, out=%s", out.String())
	}
	if !strings.Contains(out.String(), "backend.status") {
		t.Fatalf("missing events: %s", out.String())
	}
}

func TestInitializeMockIPC(t *testing.T) {
	dir := t.TempDir()
	log := logging.New(&bytes.Buffer{}, logging.Error)
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join([]string{
		`{"v":1,"kind":"req","id":"1","method":"initialize","params":{"workspace":` + jq(dir) + `,"runtime":"mock"}}`,
		`{"v":1,"kind":"req","id":"2","method":"backend.status","params":{}}`,
	}, "\n") + "\n")
	srv := ipc.New(in, &out, core.Handle)
	core.AttachIPC(srv)
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ipc serve hung: " + out.String())
	}
	if !strings.Contains(out.String(), `"kind":"res"`) {
		t.Fatalf("no response: %s", out.String())
	}
}

func TestApplySettingsIncludesRoutingAndEfficiency(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	core.applySettings(map[string]any{
		"escalate": false, "reportMaxChars": float64(640),
		"byDifficulty": map[string]any{"hard": "provider/model"},
		"efficiency":   map[string]any{"preset": "auto", "planLean": true},
	})
	settings := core.Coord.Settings
	if settings.Escalate || !settings.EscalateSet || settings.ReportMaxChars != 640 || settings.ByDifficulty["hard"] != "provider/model" {
		t.Fatalf("settings not applied: %+v", settings)
	}
	if core.Efficiency["preset"] != "auto" || core.Efficiency["planLean"] != true {
		t.Fatalf("efficiency settings not retained: %#v", core.Efficiency)
	}
}

func TestCacheGetAndRestore(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	session := core.Coord.CreateSession(dir, "", "build")
	core.Coord.Cache.Add("agent-1", 2, "model", metrics.Usage{Input: 10, CacheRead: 40, CacheWrite: 5})
	core.AttachIPC(ipc.New(strings.NewReader(""), &bytes.Buffer{}, core.Handle))
	core.Coord.Notify("cache.updated", cachePayload(core.Coord.Cache.Snapshot()))
	got, err := core.Handle("cache.get", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := got.(map[string]any)
	row := snapshot["session"].(map[string]any)
	if row["hitRate"] != 40.0/55.0 {
		t.Fatalf("cache.get = %#v, %v", got, err)
	}
	core.Coord.Cache = &metrics.CacheStats{}
	core.restoreSession(session.ID)
	if got := core.Coord.Cache.Snapshot(); len(got.Agents) != 1 || got.Session.HitRate != 40.0/55.0 {
		t.Fatalf("restored cache snapshot = %+v", got)
	}
}

func jq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestDirectModeChangeAcceptRejectContract(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	core.Coord.CreateSession(dir, "", "build")
	core.Coord.Restore(withChange(core.Coord.Current(), "chg-direct", "applied"))
	if _, err := core.acceptChange("chg-direct"); err != nil {
		t.Fatalf("accept applied change should be a no-op, got %v", err)
	}
	_, err = core.rejectChange("chg-direct")
	if err == nil || !strings.Contains(err.Error(), "cannot be reverted") {
		t.Fatalf("reject applied change error = %v", err)
	}
}

func withChange(s *orchestrator.Session, id, status string) *orchestrator.Session {
	s.Changes = append(s.Changes, orchestrator.Change{ID: id, AgentID: "a", Path: "out.txt", Status: status})
	return s
}

// A restored session has nil Usage (omitempty); a stored per-agent usage record used to panic on write.
func TestRestoreSessionWithAgentUsageRecordDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	session := core.Coord.CreateSession(dir, "", "build")
	for _, rec := range []any{map[string]any{"agentId": "a1", "input": 3}, map[string]any{"input": 1}} {
		if err := core.Store.PutUsage(session.ID, rec); err != nil {
			t.Fatal(err)
		}
	}
	stored := *session
	stored.Usage = nil
	core.Coord.Restore(&stored)
	core.restoreSession(session.ID)
	got := core.Coord.Current()
	if got.Usage == nil {
		t.Fatal("restored session Usage map is nil")
	}
	if _, ok := got.Usage["a1"]; !ok {
		t.Fatalf("restored usage missing agent record: %+v", got.Usage)
	}
}
