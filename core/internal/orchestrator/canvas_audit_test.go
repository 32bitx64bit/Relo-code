package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/persistence"
	"github.com/relocode/relo/internal/runtime"
)

// canvasAuditLog collects the audit records the coord produces.
type canvasAuditLog struct {
	mu      sync.Mutex
	records []CanvasAudit
}

func (l *canvasAuditLog) record(rec CanvasAudit) {
	l.mu.Lock()
	l.records = append(l.records, rec)
	l.mu.Unlock()
}

func (l *canvasAuditLog) list() []CanvasAudit {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]CanvasAudit(nil), l.records...)
}

// find returns the first record with the given event name.
func (l *canvasAuditLog) find(event string) (CanvasAudit, bool) {
	for _, rec := range l.list() {
		if rec.Event == event {
			return rec, true
		}
	}
	return CanvasAudit{}, false
}

// A denied canvas call leaves an audit record: the role that asked, the owner, the action and the reason.
func TestCanvasAuditRecordsDeniedPrimaryCall(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Primary)
	log := &canvasAuditLog{}
	c.SetCanvasAudit(log.record)

	_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
	if err == nil || !strings.Contains(err.Error(), "not available to the primary role") {
		t.Fatalf("primary create: err = %v", err)
	}
	rec, ok := log.find("canvas.requested")
	if !ok {
		t.Fatalf("no canvas.requested audit record: %+v", log.list())
	}
	if rec.Role != string(agent.Primary) || rec.Owner != "sender" || rec.Action != "create" {
		t.Fatalf("audit record = %+v", rec)
	}
	if rec.Outcome != "denied" || !strings.Contains(rec.Reason, "not available to the primary role") {
		t.Fatalf("audit record = %+v", rec)
	}
	// The refusal happens before the request reaches the extension, so no canvas event is emitted.
	if n := len(ev.list()); n != 0 {
		t.Fatalf("denied primary call emitted %d canvas events", n)
	}
}

// A builder call leaves a request record, and the answer leaves a reply record.
func TestCanvasAuditRecordsBuilderRequestAndReply(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	log := &canvasAuditLog{}
	c.SetCanvasAudit(log.record)

	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	if err := c.CanvasReply(reqID, map[string]any{"artifact": map[string]any{"id": "art_1"}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	req, ok := log.find("canvas.requested")
	if !ok {
		t.Fatalf("no canvas.requested audit record: %+v", log.list())
	}
	if req.Role != string(agent.Worker) || req.Action != "create" || req.Outcome != "requested" || req.RequestID != reqID {
		t.Fatalf("request audit record = %+v", req)
	}
	rep, ok := log.find("canvas.reply")
	if !ok {
		t.Fatalf("no canvas.reply audit record: %+v", log.list())
	}
	if rep.Role != string(agent.Worker) || rep.Action != "create" || rep.Outcome != "ok" || rep.RequestID != reqID {
		t.Fatalf("reply audit record = %+v", rep)
	}
	// The artifact id of a create is only known from the answer, and only the id is kept.
	if rep.ArtifactID != "art_1" {
		t.Fatalf("reply audit record = %+v", rep)
	}
}

// A failed answer is recorded as an error with its reason.
func TestCanvasAuditRecordsReplyError(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	log := &canvasAuditLog{}
	c.SetCanvasAudit(log.record)

	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "open"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	if err := c.CanvasReply(reqID, nil, "artifact not found"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("failed answer was reported as a success")
	}
	rep, ok := log.find("canvas.reply")
	if !ok {
		t.Fatalf("no canvas.reply audit record: %+v", log.list())
	}
	if rep.Outcome != "error" || !strings.Contains(rep.Reason, "artifact not found") {
		t.Fatalf("reply audit record = %+v", rep)
	}
}

// The audit record carries metadata only: no artifact source, no file contents and no assets.
func TestCanvasAuditRecordCarriesNoArtifactContent(t *testing.T) {
	c, ev, _, key := newCanvasRoleCoord(t, ModeBuild, agent.Worker)
	log := &canvasAuditLog{}
	c.SetCanvasAudit(log.record)

	const source = "<h1>secret landing page</h1>"
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(key, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	// The answer carries the artifact source, exactly as the extension sends it.
	answer := map[string]any{"artifact": map[string]any{"id": "art_1", "files": map[string]string{"index.html": source}}, "source": map[string]any{"files": map[string]string{"index.html": source}}}
	if err := c.CanvasReply(reqID, answer, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, rec := range log.list() {
		raw, err := json.Marshal(rec.Payload())
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if strings.Contains(text, source) || strings.Contains(text, "index.html") {
			t.Fatalf("audit record carries artifact content: %s", text)
		}
		for _, key := range []string{"files", "assets", "source", "entry", "result"} {
			if strings.Contains(text, "\""+key+"\"") {
				t.Fatalf("audit record carries %q: %s", key, text)
			}
		}
	}
}

// newCanvasAuditCoord builds a coord with a primary and a builder agent, each holding a live canvas
// capability key. Every canvas call is audited into the given store, the way the app does it.
func newCanvasAuditCoord(t *testing.T, store *persistence.Store, mode Mode) (*Coord, *canvasEvents, string, string) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	c.CanvasEnabled = true
	c.CreateSession(t.TempDir(), "design a landing page", mode)
	session := c.SessionID()
	c.SetCanvasAudit(func(rec CanvasAudit) {
		if err := store.LogEvent(session, rec.Event, rec.Payload()); err != nil {
			t.Errorf("log canvas audit: %v", err)
		}
	})
	primary := c.Agents.Create(agent.Agent{ID: "primary-agent", Role: agent.Primary, Status: agent.Running})
	c.runtimeSessions[primary.ID] = &runtimeSession{id: "runtime-primary"}
	primaryKey, err := c.bindPeer(primary, "runtime-primary")
	if err != nil {
		t.Fatal(err)
	}
	worker := c.Agents.Create(agent.Agent{ID: "worker-agent", Role: agent.Worker, Status: agent.Running})
	c.runtimeSessions[worker.ID] = &runtimeSession{id: "runtime-worker"}
	workerKey, err := c.bindPeer(worker, "runtime-worker")
	if err != nil {
		t.Fatal(err)
	}
	ev := &canvasEvents{}
	c.SetEmitter(ev.record)
	return c, ev, primaryKey, workerKey
}

// The events table records the canvas calls with metadata only: a denied primary call leaves a denial
// row with its reason, and a builder create leaves a request row without any content.
func TestCanvasAuditWritesMetadataOnlyRowsToTheEventLog(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "relo.db"))
	if err != nil {
		t.Fatal(err)
	}
	c, ev, primaryKey, workerKey := newCanvasAuditCoord(t, store, ModeBuild)
	session := c.SessionID()

	// The Primary may not author: the call is refused and the refusal is recorded.
	if _, err := c.canvasRequest(context.Background(), canvasRoleRequest(primaryKey, "create")); err == nil {
		t.Fatal("primary create was accepted")
	}

	// A builder creates an artifact: the request and its answer are recorded, without any content.
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(context.Background(), canvasRoleRequest(workerKey, "create"))
		done <- err
	}()
	reqID := awaitEvent(t, ev)
	answer := map[string]any{"artifact": map[string]any{"id": "art_1", "files": map[string]string{"index.html": "<h1>hi</h1>"}}}
	if err := c.CanvasReply(reqID, answer, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	rows, err := store.EventRows(session, 20)
	if err != nil {
		t.Fatal(err)
	}
	var denied, requested, replied bool
	for _, row := range rows {
		payload := map[string]any{}
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(row.Payload), "<h1>hi</h1>") || strings.Contains(string(row.Payload), "index.html") {
			t.Fatalf("stored %s row carries artifact content: %s", row.Name, row.Payload)
		}
		switch row.Name {
		case "canvas.requested":
			if payload["role"] == string(agent.Primary) {
				denied = true
				if payload["outcome"] != "denied" {
					t.Fatalf("denied row outcome = %v", payload["outcome"])
				}
				if reason, _ := payload["reason"].(string); !strings.Contains(reason, "not available to the primary role") {
					t.Fatalf("denied row reason = %v", payload["reason"])
				}
				if payload["owner"] != "primary-agent" || payload["action"] != "create" {
					t.Fatalf("denied row = %s", row.Payload)
				}
			}
			if payload["role"] == string(agent.Worker) {
				requested = true
				if payload["outcome"] != "requested" || payload["action"] != "create" || payload["owner"] != "worker-agent" {
					t.Fatalf("requested row = %s", row.Payload)
				}
			}
		case "canvas.reply":
			replied = true
			if payload["role"] != string(agent.Worker) || payload["outcome"] != "ok" {
				t.Fatalf("reply row = %s", row.Payload)
			}
			if payload["artifactId"] != "art_1" {
				t.Fatalf("reply row = %s", row.Payload)
			}
		}
	}
	if !denied || !requested || !replied {
		t.Fatalf("event log rows = %+v", rows)
	}
}

// A denied primary create leaves exactly one row in the event log: the denial. The call never reaches
// the extension, so nothing else is written.
func TestCanvasDeniedPrimaryWritesExactlyOneRow(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "relo.db"))
	if err != nil {
		t.Fatal(err)
	}
	c, ev, primaryKey, _ := newCanvasAuditCoord(t, store, ModeBuild)
	session := c.SessionID()

	if _, err := c.canvasRequest(context.Background(), canvasRoleRequest(primaryKey, "create")); err == nil {
		t.Fatal("primary create was accepted")
	}
	if n := len(ev.list()); n != 0 {
		t.Fatalf("denied primary call emitted %d canvas events", n)
	}
	rows, err := store.EventRows(session, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("event log rows = %+v", rows)
	}
	row := rows[0]
	if row.Name != "canvas.requested" {
		t.Fatalf("stored row = %+v", row)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["outcome"] != "denied" || payload["role"] != string(agent.Primary) || payload["action"] != "create" {
		t.Fatalf("stored row = %s", row.Payload)
	}
	if reason, _ := payload["reason"].(string); !strings.Contains(reason, "not available to the primary role") {
		t.Fatalf("stored row = %s", row.Payload)
	}
}
