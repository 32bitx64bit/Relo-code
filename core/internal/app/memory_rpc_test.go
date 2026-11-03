package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/memory"
)

func newMemoryApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	log := logging.New(&bytes.Buffer{}, logging.Error)
	a, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Coord.CreateSession(dir, "memory rpc", "build")
	return a
}

func rpc(t *testing.T, a *App, method string, params map[string]any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return a.Handle(method, raw)
}

// User RPCs write directly with user provenance; caller-supplied source and state are ignored.
func TestMemoryUpdateDeleteStayUserDirect(t *testing.T) {
	a := newMemoryApp(t)
	seed, err := a.Coord.Memory.UserPromote(memory.Entry{Kind: "fact", Title: "Table tests", Body: "Prefer table-driven tests."})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rpc(t, a, "memory.update", map[string]any{"id": seed.ID, "title": "Table-driven tests"}); err == nil {
		t.Fatal("memory.update without revision accepted, want invalid_params")
	}
	out, err := rpc(t, a, "memory.update", map[string]any{"id": seed.ID, "revision": seed.Revision, "title": "Table-driven tests",
		"source": "agent", "state": "candidate"})
	if err != nil {
		t.Fatalf("memory.update: %v", err)
	}
	got := out.(memory.Entry)
	if got.Source != memory.SourceUser || got.State != memory.StateVerified || got.Revision != seed.Revision+1 || got.Title != "Table-driven tests" {
		t.Fatalf("update = source %s state %s revision %d title %q, want user/verified/%d", got.Source, got.State, got.Revision, got.Title, seed.Revision+1)
	}
	if _, err := rpc(t, a, "memory.update", map[string]any{"id": seed.ID, "revision": seed.Revision, "title": "stale"}); err == nil {
		t.Fatal("stale memory.update accepted")
	}

	if _, err := rpc(t, a, "memory.delete", map[string]any{"id": seed.ID, "revision": got.Revision}); err != nil {
		t.Fatalf("memory.delete: %v", err)
	}
	items, err := a.Coord.Memory.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range items {
		if e.ID == seed.ID {
			t.Fatalf("deleted entry still listed: %+v", e)
		}
	}
}

// context.promote from the user path stores a verified user entry directly, not a candidate.
func TestContextPromoteIsUserDirect(t *testing.T) {
	a := newMemoryApp(t)
	s := a.Coord.Current()
	primary := a.Coord.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	a.Coord.Engine.Ingest(primary.ID, []ctxeng.Item{{Kind: ctxeng.KindArchitecture, Title: "Retry policy", Content: "Retries use exponential backoff.", Source: ctxeng.SourceUser, Durability: 1}})
	var itemID string
	for _, it := range a.Coord.Engine.SnapshotPool(primary.ID) {
		if it.Title == "Retry policy" {
			itemID = it.ID
		}
	}
	if itemID == "" {
		t.Fatal("ingested item not in pool")
	}
	if _, err := rpc(t, a, "context.promote", map[string]any{"agentId": primary.ID, "itemId": itemID}); err != nil {
		t.Fatalf("context.promote: %v", err)
	}
	items, _ := a.Coord.Memory.Load()
	if len(items) != 1 || items[0].Source != memory.SourceUser || items[0].State != memory.StateVerified || !items[0].Verified {
		t.Fatalf("promoted entries = %+v, want one verified user entry", items)
	}
}

// No client RPC proposes or decides memory: agent writes exist only as orchestrator agent actions.
func TestNoClientRPCAcceptsAgentMemoryWrites(t *testing.T) {
	a := newMemoryApp(t)
	for _, method := range []string{"memory.propose", "memory.promote", "memory.retire", "memory.reject", "memory.candidate"} {
		if _, err := rpc(t, a, method, map[string]any{"id": "mem_x", "revision": 1}); err == nil {
			t.Fatalf("%s accepted, want unknown method", method)
		}
	}
}

// memory.delete needs an integer revision: anything else is invalid_params and leaves the entry unchanged. The
// memory.updated event reports the revision the store committed, not an assumed Revision+1.
func TestMemoryDeleteRequiresIntegerRevisionAndReportsStoredRevision(t *testing.T) {
	a := newMemoryApp(t)
	seed, err := a.Coord.Memory.UserPromote(memory.Entry{Kind: "fact", Title: "Keep", Body: "Keep this entry."})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]any{
		{"id": seed.ID},
		{"id": seed.ID, "revision": "1"},
		{"id": seed.ID, "revision": 1.5},
		{"id": seed.ID, "revision": 0},
	} {
		if _, err := rpc(t, a, "memory.delete", bad); err == nil {
			t.Fatalf("memory.delete %v accepted, want invalid_params", bad)
		}
	}
	if got, ok := a.Coord.Memory.Get(seed.ID); !ok || got.Revision != seed.Revision || got.State != memory.StateVerified {
		t.Fatalf("entry after refused deletes = %+v, %v; want unchanged at revision %d", got, ok, seed.Revision)
	}

	var events []map[string]any
	a.Coord.Emit = func(name string, payload any) {
		if m, ok := payload.(map[string]any); ok && name == "memory.updated" {
			events = append(events, m)
		}
	}
	if _, err := rpc(t, a, "memory.delete", map[string]any{"id": seed.ID, "revision": seed.Revision}); err != nil {
		t.Fatalf("memory.delete: %v", err)
	}
	if len(events) != 1 || events[0]["revision"] != seed.Revision+1 || events[0]["change"] != "deleted" {
		t.Fatalf("memory.updated events = %v, want one deleted event at stored revision %d", events, seed.Revision+1)
	}
}

// memory.get, memory.retire and memory.update (the user promote path) are user RPCs: each needs the stored revision,
// core stamps user provenance whatever the caller sends, and memory.updated carries the stored revision.
func TestMemoryUserRPCsGetRetireAndStoredRevision(t *testing.T) {
	a := newMemoryApp(t)
	seed, err := a.Coord.Memory.UserPromote(memory.Entry{Kind: "fact", Title: "Retire me", Body: "Retire this entry."})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rpc(t, a, "memory.get", map[string]any{"id": seed.ID})
	if err != nil {
		t.Fatalf("memory.get: %v", err)
	}
	if e := got.(memory.Entry); e.ID != seed.ID || e.Revision != seed.Revision || e.Source != memory.SourceUser || e.State != memory.StateVerified {
		t.Fatalf("memory.get = %+v, want stored user entry", e)
	}
	if _, err := rpc(t, a, "memory.get", map[string]any{"id": "mem_missing"}); err == nil {
		t.Fatal("memory.get on unknown id accepted, want not_found")
	}

	edited, err := rpc(t, a, "memory.update", map[string]any{"id": seed.ID, "revision": seed.Revision, "title": "Promoted", "source": "agent", "state": "candidate"})
	if err != nil {
		t.Fatalf("memory.update: %v", err)
	}
	cur := edited.(memory.Entry)
	if cur.Source != memory.SourceUser || cur.State != memory.StateVerified || cur.Revision != seed.Revision+1 {
		t.Fatalf("memory.update = %+v, want direct user verified write", cur)
	}
	for _, bad := range []map[string]any{{"id": seed.ID}, {"id": seed.ID, "revision": "1"}, {"id": seed.ID, "revision": 1.5}} {
		if _, err := rpc(t, a, "memory.retire", bad); err == nil {
			t.Fatalf("memory.retire %v accepted, want invalid_params", bad)
		}
	}
	if _, err := rpc(t, a, "memory.retire", map[string]any{"id": seed.ID, "revision": seed.Revision}); err == nil {
		t.Fatal("memory.retire with stale revision accepted, want conflict")
	}

	var events []map[string]any
	a.Coord.Emit = func(name string, payload any) {
		if m, ok := payload.(map[string]any); ok && name == "memory.updated" {
			events = append(events, m)
		}
	}
	retired, err := rpc(t, a, "memory.retire", map[string]any{"id": seed.ID, "revision": cur.Revision})
	if err != nil {
		t.Fatalf("memory.retire: %v", err)
	}
	r := retired.(memory.Entry)
	if r.State != memory.StateRetired || r.Revision != cur.Revision+1 || r.RetiredBy != memory.SourceUser {
		t.Fatalf("memory.retire = %+v, want retired user entry", r)
	}
	if len(events) != 1 || events[0]["revision"] != r.Revision || events[0]["change"] != "retired" {
		t.Fatalf("memory.updated events = %v, want one retired event at stored revision %d", events, r.Revision)
	}
}

// memory.update applies the nested patch the extension sends: title and body are persisted, core keeps user
// provenance, and memory.updated reports the revision the store committed.
func TestMemoryUpdateAppliesPatchAndReportsStoredRevision(t *testing.T) {
	a := newMemoryApp(t)
	seed, err := a.Coord.Memory.UserPromote(memory.Entry{Kind: "fact", Title: "Old title", Body: "Old body."})
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	a.Coord.Emit = func(name string, payload any) {
		if m, ok := payload.(map[string]any); ok && name == "memory.updated" {
			events = append(events, m)
		}
	}
	out, err := rpc(t, a, "memory.update", map[string]any{
		"id": seed.ID, "revision": seed.Revision,
		"patch": map[string]any{"title": "New title", "body": "New body.", "source": "agent", "state": "candidate"},
	})
	if err != nil {
		t.Fatalf("memory.update: %v", err)
	}
	got := out.(memory.Entry)
	if got.Title != "New title" || got.Body != "New body." {
		t.Fatalf("memory.update = title %q body %q, want the patched text", got.Title, got.Body)
	}
	if got.Source != memory.SourceUser || got.State != memory.StateVerified || got.Revision != seed.Revision+1 {
		t.Fatalf("memory.update = source %s state %s revision %d, want user/verified/%d", got.Source, got.State, got.Revision, seed.Revision+1)
	}
	cur, ok := a.Coord.Memory.Get(seed.ID)
	if !ok || cur.Title != "New title" || cur.Body != "New body." || cur.Revision != got.Revision {
		t.Fatalf("stored entry = %+v, %v; want the patched text at revision %d", cur, ok, got.Revision)
	}
	if len(events) != 1 || events[0]["revision"] != got.Revision || events[0]["change"] != "updated" {
		t.Fatalf("memory.updated events = %v, want one updated event at stored revision %d", events, got.Revision)
	}

	if _, err := rpc(t, a, "memory.update", map[string]any{
		"id": seed.ID, "revision": seed.Revision, "patch": map[string]any{"title": "Stale"},
	}); err == nil {
		t.Fatal("memory.update with stale revision accepted, want conflict")
	}
	if _, err := rpc(t, a, "memory.update", map[string]any{
		"id": seed.ID, "revision": got.Revision, "patch": map[string]any{"body": "password: hunter2"},
	}); err == nil {
		t.Fatal("memory.update with a secret in the patch accepted, want unsafe_content")
	}
	cur, _ = a.Coord.Memory.Get(seed.ID)
	if cur.Body != "New body." || cur.Revision != got.Revision {
		t.Fatalf("entry after refused writes = %+v, want unchanged at revision %d", cur, got.Revision)
	}
}

// An empty patch, or one that would not change the stored text, is refused instead of bumping the revision.
func TestMemoryUpdateRejectsEmptyAndNoopPatch(t *testing.T) {
	a := newMemoryApp(t)
	seed, err := a.Coord.Memory.UserPromote(memory.Entry{Kind: "fact", Title: "Same title", Body: "Same body."})
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []map[string]any{
		{"id": seed.ID, "revision": seed.Revision},
		{"id": seed.ID, "revision": seed.Revision, "patch": map[string]any{}},
		{"id": seed.ID, "revision": seed.Revision, "patch": map[string]any{"title": seed.Title}},
		{"id": seed.ID, "revision": seed.Revision, "patch": map[string]any{"title": seed.Title, "body": seed.Body}},
	} {
		if _, err := rpc(t, a, "memory.update", params); err == nil {
			t.Fatalf("memory.update %v accepted, want invalid_params", params)
		}
	}
	cur, ok := a.Coord.Memory.Get(seed.ID)
	if !ok || cur.Revision != seed.Revision || cur.Title != seed.Title || cur.Body != seed.Body {
		t.Fatalf("entry after refused patches = %+v, %v; want unchanged at revision %d", cur, ok, seed.Revision)
	}
}

// memory.list feeds the snapshot's array shape and carries state, revision, source and stale reason per entry.
func TestMemoryListCarriesReviewFields(t *testing.T) {
	a := newMemoryApp(t)
	seed, err := a.Coord.Memory.UserPromote(memory.Entry{Kind: "fact", Title: "Listed", Body: "Listed entry."})
	if err != nil {
		t.Fatal(err)
	}
	out, err := rpc(t, a, "memory.list", map[string]any{})
	if err != nil {
		t.Fatalf("memory.list: %v", err)
	}
	items, ok := out.([]memory.Entry)
	if !ok || len(items) != 1 || items[0].ID != seed.ID {
		t.Fatalf("memory.list = %T %+v, want a one-entry array", out, out)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"state", "revision", "source"} {
		if _, ok := rows[0][field]; !ok {
			t.Fatalf("listed entry missing %q: %s", field, raw)
		}
	}
	if _, ok := rows[0]["staleReason"]; ok {
		t.Fatalf("fresh entry carries staleReason: %s", raw)
	}
	snap := a.Coord.Snapshot()
	if m, ok := snap["memory"].([]memory.Entry); !ok || len(m) != 1 {
		t.Fatalf("snapshot memory = %T %+v, want the array shape", snap["memory"], snap["memory"])
	}
}
