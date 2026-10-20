package app

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
)

// The app stores one metadata-only row per canvas call in the event log: a denied primary call leaves
// a denial row with its reason, and no artifact content reaches the log.
func TestCanvasAuditRowsAreMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	core.Coord.CreateSession(dir, "design a landing page", orchestrator.ModeBuild)
	session := core.Coord.SessionID()
	if session == "" {
		t.Fatal("no session")
	}
	if !core.Coord.CanvasAuditInstalled() {
		t.Fatal("the app did not install the canvas audit sink")
	}

	core.logCanvasAudit(orchestrator.CanvasAudit{
		Event:      "canvas.requested",
		Action:     "create",
		Role:       "primary",
		Owner:      "primary-agent",
		ArtifactID: "art_1",
		RequestID:  "canvas_1",
		Mode:       "build",
		Outcome:    "denied",
		Reason:     "canvas create is not available to the primary role; canvas authoring is reserved for builder agents",
	})
	core.logCanvasAudit(orchestrator.CanvasAudit{
		Event:     "canvas.reply",
		Action:    "create",
		Role:      "worker",
		Owner:     "worker-agent",
		RequestID: "canvas_2",
		Mode:      "build",
		Outcome:   "ok",
	})

	rows, err := core.Store.EventRows(session, 10)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, row := range rows {
		payload := map[string]any{}
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		byName[row.Name] = payload
	}
	denied, ok := byName["canvas.requested"]
	if !ok {
		t.Fatalf("no canvas.requested row: %+v", rows)
	}
	if denied["role"] != "primary" || denied["owner"] != "primary-agent" || denied["action"] != "create" {
		t.Fatalf("canvas.requested row = %v", denied)
	}
	if denied["outcome"] != "denied" {
		t.Fatalf("canvas.requested row = %v", denied)
	}
	if reason, _ := denied["reason"].(string); !strings.Contains(reason, "not available to the primary role") {
		t.Fatalf("canvas.requested row = %v", denied)
	}
	if denied["artifactId"] != "art_1" || denied["requestId"] != "canvas_1" {
		t.Fatalf("canvas.requested row = %v", denied)
	}
	replied, ok := byName["canvas.reply"]
	if !ok {
		t.Fatalf("no canvas.reply row: %+v", rows)
	}
	if replied["role"] != "worker" || replied["owner"] != "worker-agent" || replied["action"] != "create" || replied["outcome"] != "ok" {
		t.Fatalf("canvas.reply row = %v", replied)
	}
	// The stored rows carry metadata only: no source, no file names, no assets.
	for _, row := range rows {
		if row.Name != "canvas.requested" && row.Name != "canvas.reply" {
			continue
		}
		text := string(row.Payload)
		for _, key := range []string{"files", "assets", "source", "entry", "result"} {
			if strings.Contains(text, "\""+key+"\"") {
				t.Fatalf("%s row carries %q: %s", row.Name, key, text)
			}
		}
	}
}

// The raw canvas payloads carry artifact source, files and assets. The app's own emitter must not write
// them to the event log: the audit sink is the only writer of canvas rows.
func TestCanvasRawPayloadNeverReachesTheEventLog(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	// AttachIPC installs the app's emitter, the one that persists events.
	core.AttachIPC(ipc.New(strings.NewReader(""), io.Discard, core.Handle))
	core.Coord.CreateSession(dir, "design a landing page", orchestrator.ModeBuild)
	session := core.Coord.SessionID()
	if session == "" {
		t.Fatal("no session")
	}

	// The raw payload the extension receives carries the artifact source, its files and its assets.
	raw := map[string]any{
		"requestId": "canvas_1",
		"role":      "worker",
		"owner":     "worker-agent",
		"request": map[string]any{
			"action": "create",
			"files":  map[string]any{"index.html": "<h1>secret landing page</h1>"},
			"assets": []any{map[string]any{"path": "logo.png", "data": "AAAA"}},
		},
	}
	core.Coord.Notify("canvas.requested", raw)
	core.Coord.Notify("canvas.reply", raw)

	// The audit sink writes the one row that stands for those calls.
	core.logCanvasAudit(orchestrator.CanvasAudit{
		Event:     "canvas.requested",
		Action:    "create",
		Role:      "worker",
		Owner:     "worker-agent",
		RequestID: "canvas_1",
		Mode:      "build",
		Outcome:   "requested",
	})

	rows, err := core.Store.EventRows(session, 20)
	if err != nil {
		t.Fatal(err)
	}
	var canvasRows int
	var name, text string
	for _, row := range rows {
		if row.Name != "canvas.requested" && row.Name != "canvas.reply" {
			continue
		}
		canvasRows++
		name, text = row.Name, string(row.Payload)
	}
	if canvasRows != 1 {
		t.Fatalf("canvas rows = %d, want 1: %+v", canvasRows, rows)
	}
	if name != "canvas.requested" {
		t.Fatalf("canvas row = %s %s", name, text)
	}
	for _, needle := range []string{"secret landing page", "index.html", "logo.png", "files", "assets"} {
		if strings.Contains(text, needle) {
			t.Fatalf("the stored canvas row carries %q: %s", needle, text)
		}
	}
}
