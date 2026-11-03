package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/persistence"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
)

// openTestMemory attaches a real catalog-backed memory store to c, as app.New does.
func openTestMemory(t *testing.T, c *Coord, workspace string) *memory.Store {
	t.Helper()
	p, err := persistence.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	m, _, err := memory.Open(p.DB(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	c.Memory = m
	return m
}

// seedCheck records a check result in the current session, run by agentID, as a worker's verification run does. The
// result keeps resultID, the ref a candidate cites.
func seedCheck(c *Coord, agentID, resultID, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess.Verify = append(c.sess.Verify, verification.Result{ID: resultID, Command: "go test ./internal/memory", Status: status})
	if c.checkOwners == nil {
		c.checkOwners = map[string]string{}
	}
	c.checkOwners[resultID] = agentID
}

// presentPending builds the Primary's pending listing, which records what the next memoryActions may cite.
func presentPending(c *Coord) {
	_ = c.pendingMemoryItems()
}

// candidateWithCheck proposes a candidate whose command_result is backed by a passed check and the worker that ran it,
// both durable in the session.
func candidateWithCheck(t *testing.T, c *Coord, m *memory.Store, ref string) memory.Entry {
	t.Helper()
	s := c.Current()
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	seedCheck(c, w.ID, ref, "passed")
	e, err := m.ProposeCandidate(memory.Entry{Kind: "fact", Title: "Tests live beside sources", Body: "Unit tests sit next to the package they cover.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceCommandResult, SessionID: s.ID, AgentID: w.ID, Ref: ref, Command: "go test ./internal/memory", ObservedAt: time.Now().UTC(), Result: memory.ResultPassed}}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A worker completion becomes a candidate with core-observed evidence and never a verified entry.
func TestWorkerCompletionProposesCandidateWithEvidence(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "memory", ModeBuild)
	c.Settings.Memory = true
	c.Settings.AutoVerify = false
	c.Settings.AutoReview = false
	m := openTestMemory(t, c, s.Workspace)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	task := c.Graph.Add(taskgraph.Task{ID: "memory-worker", Role: string(agent.Worker), Workspace: s.Workspace, Attempt: 1})
	// The worker's own check record is what the candidate cites; the outcome alone is not evidence.
	seedCheck(c, w.ID, "ver-worker", "passed")

	c.acceptReport(task, w, RawReport{Status: "completed", Summary: "Added the parser tests."}, verifyOutcome{ran: true, passed: true})

	entries, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want one candidate", len(entries))
	}
	got := entries[0]
	if got.State != memory.StateCandidate || got.Source != memory.SourceAgent || got.Verified {
		t.Fatalf("worker completion stored state=%s source=%s verified=%v, want candidate/agent/false", got.State, got.Source, got.Verified)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Type != memory.EvidenceCommandResult || got.Evidence[0].Result != memory.ResultPassed {
		t.Fatalf("evidence = %+v, want one passed command_result", got.Evidence)
	}
}

func TestWorkerCompletionOffWhenMemoryDisabled(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "memory off", ModeBuild)
	c.Settings.Memory = false
	c.Settings.AutoVerify = false
	c.Settings.AutoReview = false
	m := openTestMemory(t, c, s.Workspace)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	task := c.Graph.Add(taskgraph.Task{ID: "memory-off", Role: string(agent.Worker), Workspace: s.Workspace, Attempt: 1})

	c.acceptReport(task, w, RawReport{Status: "completed", Summary: "Done."}, verifyOutcome{ran: true, passed: true})

	if entries, _ := m.Load(); len(entries) != 0 {
		t.Fatalf("entries = %d with Settings.Memory off, want 0", len(entries))
	}
}

// Only the Primary decides candidates; a worker's decision is refused and changes nothing.
func TestOnlyPrimaryDecidesMemoryCandidates(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "head gate", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	worker := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	cand := candidateWithCheck(t, c, m, "ev-check")

	promote := []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision, Evidence: []string{"ev-check"}, Rationale: "Checked by the test run."}}
	if err := c.applyMemoryActions(worker.ID, promote); err == nil {
		t.Fatal("worker promote was accepted, want refusal")
	}
	if got, _ := m.Get(cand.ID); got.State != memory.StateCandidate || got.Verified {
		t.Fatalf("worker promote changed the candidate: state=%s verified=%v", got.State, got.Verified)
	}

	presentPending(c)
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatalf("primary promote: %v", err)
	}
	if got, _ := m.Get(cand.ID); got.State != memory.StateVerified || got.Source != memory.SourceHead || !got.Verified {
		t.Fatalf("primary promote stored state=%s source=%s verified=%v", got.State, got.Source, got.Verified)
	}
}

func TestPrimaryRejectsAndRetiresThroughHeadGate(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "reject retire", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	worker := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})

	rejected := candidateWithCheck(t, c, m, "ev-reject")
	if err := c.applyMemoryActions(worker.ID, []MemoryAction{{Op: "reject", ID: rejected.ID, Revision: rejected.Revision, Rationale: "Not needed."}}); err == nil {
		t.Fatal("worker reject was accepted, want refusal")
	}
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, []MemoryAction{{Op: "reject", ID: rejected.ID, Revision: rejected.Revision, Rationale: "Not needed."}}); err != nil {
		t.Fatalf("primary reject: %v", err)
	}
	if _, ok := m.Get(rejected.ID); ok {
		t.Fatal("rejected candidate still readable")
	}

	kept := candidateWithCheck(t, c, m, "ev-retire")
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, []MemoryAction{{Op: "promote", ID: kept.ID, Revision: kept.Revision, Evidence: []string{"ev-retire"}, Rationale: "Verified by the run."}}); err != nil {
		t.Fatalf("primary promote: %v", err)
	}
	promoted, _ := m.Get(kept.ID)
	if err := c.applyMemoryActions(worker.ID, []MemoryAction{{Op: "retire", ID: kept.ID, Revision: promoted.Revision, Rationale: "Stale."}}); err == nil {
		t.Fatal("worker retire was accepted, want refusal")
	}
	// The verified entry is listed for retire, so the head's retire is presented at its revision.
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, []MemoryAction{{Op: "retire", ID: kept.ID, Revision: promoted.Revision, Rationale: "Superseded by a newer run."}}); err != nil {
		t.Fatalf("primary retire: %v", err)
	}
	if got, _ := m.Get(kept.ID); got.State != memory.StateRetired {
		t.Fatalf("state after retire = %s, want retired", got.State)
	}
}

// The head sees pending candidates by id and revision, never their bodies.
func TestPendingCandidatesListedWithoutBodies(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "listing", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	cand := candidateWithCheck(t, c, m, "ev-list")

	items := c.pendingMemoryItems()
	if len(items) != 1 {
		t.Fatalf("pending items = %+v, want one listing", items)
	}
	text := items[0].Content
	if !strings.Contains(text, cand.ID) || !strings.Contains(text, "ev-list") || strings.Contains(text, "Unit tests sit next") {
		t.Fatalf("listing = %q, want id and evidence ref without body", text)
	}
}

func TestParseTurnRejectsUnknownMemoryOp(t *testing.T) {
	body := `{"status":"completed","userMessage":"ok","tasks":[],"memoryActions":[{"op":"publish","id":"mem_x","revision":1,"rationale":"no"}]}`
	if _, err := ParseTurn(body, func(string) bool { return false }); err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("ParseTurn err = %v, want unknown op", err)
	}
}

// The head's review queue is bounded; the overflow is counted, not listed.
func TestPendingListingIsBounded(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "bounded", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	for i := 0; i < maxPendingMemoryListed+5; i++ {
		candidateWithCheck(t, c, m, "ev-bound")
	}

	items := c.pendingMemoryItems()
	if len(items) != 1 {
		t.Fatalf("pending items = %d, want one listing", len(items))
	}
	text := items[0].Content
	if n := strings.Count(text, "\n- id "); n != maxPendingMemoryListed {
		t.Fatalf("listed %d candidates, want %d", n, maxPendingMemoryListed)
	}
	if !strings.Contains(text, "5 more pending candidates are not listed") {
		t.Fatalf("listing = %q, want overflow count", text)
	}
}

// A decision replayed with a spent revision fails closed: the entry keeps its state and revision.
func TestReplayedHeadDecisionFailsClosed(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "replay", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	cand := candidateWithCheck(t, c, m, "ev-replay")

	promote := []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision, Evidence: []string{"ev-replay"}, Rationale: "Checked by the run."}}
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Get(cand.ID)
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Get(cand.ID)
	if after.Revision != before.Revision || after.State != memory.StateVerified || after.Source != memory.SourceHead {
		t.Fatalf("replayed promote changed entry: before rev=%d state=%s, after rev=%d state=%s", before.Revision, before.State, after.Revision, after.State)
	}
}

// A candidate the Primary was never shown is refused, and so is one shown at another revision. Nothing changes.
func TestUnpresentedCandidateDecisionRefused(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "unpresented", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	cand := candidateWithCheck(t, c, m, "ev-unpresented")
	promote := []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision, Evidence: []string{"ev-unpresented"}, Rationale: "Checked by the run."}}
	reject := []MemoryAction{{Op: "reject", ID: cand.ID, Revision: cand.Revision, Rationale: "Not needed."}}

	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatal(err)
	}
	if err := c.applyMemoryActions(head.ID, reject); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(cand.ID); got.State != memory.StateCandidate || got.Revision != cand.Revision {
		t.Fatalf("refused decisions changed the candidate: state=%s rev=%d", got.State, got.Revision)
	}

	presentPending(c)
	stale := []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision + 1, Evidence: []string{"ev-unpresented"}, Rationale: "Checked by the run."}}
	if err := c.applyMemoryActions(head.ID, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(cand.ID); got.State != memory.StateCandidate || got.Revision != cand.Revision {
		t.Fatalf("stale-revision decision changed the candidate: state=%s rev=%d", got.State, got.Revision)
	}

	presentPending(c)
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatalf("presented promote: %v", err)
	}
	if got, _ := m.Get(cand.ID); got.State != memory.StateVerified {
		t.Fatalf("presented promote state = %s, want verified", got.State)
	}
}

// Overflow candidates are counted but not listed, so the head cannot decide them.
func TestOverflowCandidateCannotBeDecided(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "overflow", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	for i := 0; i < maxPendingMemoryListed+1; i++ {
		candidateWithCheck(t, c, m, "ev-overflow")
	}
	items := c.pendingMemoryItems()
	text := items[0].Content
	entries, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	var hidden memory.Entry
	found := false
	for _, e := range entries {
		if e.State == memory.StateCandidate && !strings.Contains(text, "- id "+e.ID+" ") {
			hidden, found = e, true
		}
	}
	if !found {
		t.Fatal("no candidate was left out of the listing")
	}
	hiddenPromote := []MemoryAction{{Op: "promote", ID: hidden.ID, Revision: hidden.Revision, Evidence: []string{"ev-overflow"}, Rationale: "Checked by the run."}}
	if err := c.applyMemoryActions(head.ID, hiddenPromote); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(hidden.ID); got.State != memory.StateCandidate {
		t.Fatalf("overflow candidate state = %s, want candidate", got.State)
	}
}

// A promotion whose evidence has no durable local record is refused and the store is unchanged.
func TestPromotionRefusesUnresolvedEvidence(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "unresolved", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	worker := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	// No check record exists in the session for this command result.
	cand, err := m.ProposeCandidate(memory.Entry{Kind: "fact", Title: "Orphan check", Body: "Checks run from the module root.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceCommandResult, SessionID: s.ID, AgentID: worker.ID, Ref: "ev-orphan", Command: "go test ./internal/memory", ObservedAt: time.Now().UTC(), Result: memory.ResultPassed}}})
	if err != nil {
		t.Fatal(err)
	}
	promote := []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision, Evidence: []string{"ev-orphan"}, Rationale: "Checked by the run."}}
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(cand.ID); got.State != memory.StateCandidate || got.Revision != cand.Revision {
		t.Fatalf("unresolved promotion changed the candidate: state=%s rev=%d", got.State, got.Revision)
	}

	// A cited ref the candidate never recorded is unresolved too.
	seedCheck(c, worker.ID, "ev-never-recorded", "passed")
	presentPending(c)
	unknown := []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision, Evidence: []string{"ev-never-recorded"}, Rationale: "Checked by the run."}}
	if err := c.applyMemoryActions(head.ID, unknown); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(cand.ID); got.State != memory.StateCandidate || got.Revision != cand.Revision {
		t.Fatalf("unrecorded-ref promotion changed the candidate: state=%s rev=%d", got.State, got.Revision)
	}

	// With the check record present, the same candidate promotes: resolution is what was missing.
	seedCheck(c, worker.ID, "ev-orphan", "passed")
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatalf("promotion with a durable check record: %v", err)
	}
}

// A file_observation is refused once its bytes changed or it expired, and accepted while both hold.
func TestFileObservationStaleOrExpiredRefused(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "file evidence", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	const path = "notes/layout.txt"
	full := filepath.Join(s.Workspace, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("layout v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := hashFile(full)
	if err != nil {
		t.Fatal(err)
	}
	propose := func(ref string, observed time.Time) memory.Entry {
		t.Helper()
		e, err := m.ProposeCandidate(memory.Entry{Kind: "fact", Title: "Layout notes " + ref, Body: "The layout notes sit in notes.", Files: []string{path},
			Evidence: []memory.Evidence{{Type: memory.EvidenceFileObservation, Ref: ref, ObservedAt: observed, Result: memory.ResultObserved,
				Files: []memory.EvidenceFile{{Path: path, SHA256: sum}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	promote := func(e memory.Entry) error {
		presentPending(c)
		return c.applyMemoryActions(head.ID, []MemoryAction{{Op: "promote", ID: e.ID, Revision: e.Revision, Evidence: []string{e.Evidence[0].Ref}, Rationale: "Read on disk."}})
	}

	if err := promote(propose("ev-file-current", time.Now().UTC())); err != nil {
		t.Fatalf("current file observation: %v", err)
	}

	if err := os.WriteFile(full, []byte("layout v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := propose("ev-file-changed", time.Now().UTC())
	if err := promote(changed); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(changed.ID); got.State != memory.StateCandidate {
		t.Fatalf("changed-file promotion state = %s, want candidate", got.State)
	}

	if err := os.WriteFile(full, []byte("layout v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	expired := propose("ev-file-expired", time.Now().UTC().Add(-(fileEvidenceTTL + time.Hour)))
	if err := promote(expired); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(expired.ID); got.State != memory.StateCandidate {
		t.Fatalf("expired-observation promotion state = %s, want candidate", got.State)
	}
}
