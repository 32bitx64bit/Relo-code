package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/persistence"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
)

// seedCommand records a check run by agentID for command, with the given result ID and status.
func seedCommand(c *Coord, agentID, resultID, command, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess.Verify = append(c.sess.Verify, verification.Result{ID: resultID, Command: command, Status: status})
	if c.checkOwners == nil {
		c.checkOwners = map[string]string{}
	}
	c.checkOwners[resultID] = agentID
}

// proposeCited proposes a passed command_result candidate that cites ev, as a worker's completion would.
func proposeCited(t *testing.T, m *memory.Store, s *Session, title string, ev memory.Evidence) memory.Entry {
	t.Helper()
	ev.SessionID, ev.Type, ev.Result, ev.ObservedAt = s.ID, memory.EvidenceCommandResult, memory.ResultPassed, time.Now().UTC()
	e, err := m.ProposeCandidate(memory.Entry{Kind: "fact", Title: title, Body: "Checked by the run.", Evidence: []memory.Evidence{ev}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// promoteCited presents the listing and makes the head's promote decision on e, citing its first evidence ref.
func promoteCited(c *Coord, head agent.Agent, e memory.Entry) {
	presentPending(c)
	_ = c.applyMemoryActions(head.ID, []MemoryAction{{Op: "promote", ID: e.ID, Revision: e.Revision, Evidence: []string{e.Evidence[0].Ref}, Rationale: "Checked by the run."}})
}

func isVerified(m *memory.Store, id string) bool {
	got, ok := m.Get(id)
	return ok && got.State == memory.StateVerified
}

// A candidate's command_result promotes only on its own agent's check record for the same command.
func TestCandidateCheckBoundToOwnAgentAndCommand(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "binding", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	other := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	const testCmd = "go test ./internal/memory"

	seedCommand(c, other.ID, "ver-other", testCmd, "passed")
	fromOther := proposeCited(t, m, s, "Other agent's check", memory.Evidence{AgentID: w.ID, Ref: "ver-other", Command: testCmd})
	promoteCited(c, head, fromOther)
	if isVerified(m, fromOther.ID) {
		t.Fatal("candidate promoted on another agent's passed check")
	}

	seedCommand(c, w.ID, "ver-vet", "go vet ./...", "passed")
	otherCmd := proposeCited(t, m, s, "Other command's check", memory.Evidence{AgentID: w.ID, Ref: "ver-vet", Command: testCmd})
	promoteCited(c, head, otherCmd)
	if isVerified(m, otherCmd.ID) {
		t.Fatal("candidate promoted on another command's passed check")
	}

	seedCommand(c, w.ID, "ver-own", testCmd, "passed")
	own := proposeCited(t, m, s, "Own check", memory.Evidence{AgentID: w.ID, Ref: "ver-own", Command: testCmd})
	promoteCited(c, head, own)
	if !isVerified(m, own.ID) {
		t.Fatal("candidate citing its own agent's check result did not promote")
	}
}

// The presented listing is single-use and dies with a failed turn, a peer-guidance turn or a later turn; a refresh
// re-lists the candidates so the next decision is presented again.
func TestPresentedMemoryLifecycle(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "lifecycle", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	cand := candidateWithCheck(t, c, m, "ver-life")
	promote := []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision, Evidence: []string{"ver-life"}, Rationale: "Checked by the run."}}

	// A failed turn drops the listing it was built on.
	presentPending(c)
	c.dropPresentedMemory()
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatal(err)
	}
	if isVerified(m, cand.ID) {
		t.Fatal("decision applied from a listing dropped by a failed turn")
	}

	// A peer-guidance turn drops it too.
	presentPending(c)
	c.dropPresentedMemory()
	_ = c.applyMemoryActions(head.ID, promote)
	if isVerified(m, cand.ID) {
		t.Fatal("decision applied in a peer-guidance turn")
	}

	// A later turn spends the listing even when it decides nothing, so a replay in the turn after is refused.
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, nil); err != nil {
		t.Fatal(err)
	}
	_ = c.applyMemoryActions(head.ID, promote)
	if isVerified(m, cand.ID) {
		t.Fatal("decision applied from a listing a later turn already spent")
	}

	// refresh_context re-lists the candidates, so the next turn may decide on them.
	c.honorRefreshSelf(head.ID, Turn{RefreshSelf: true})
	if err := c.applyMemoryActions(head.ID, promote); err != nil {
		t.Fatal(err)
	}
	if !isVerified(m, cand.ID) {
		t.Fatal("decision refused after refresh_context re-listed the candidate")
	}
}

// Head retire acts only on a verified agent entry listed at its revision; user entries are never retired by the head.
func TestHeadRetireGatedOnPresentedSet(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "retire", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	cand := candidateWithCheck(t, c, m, "ver-retire-gate")
	promoteCited(c, head, cand)
	if !isVerified(m, cand.ID) {
		t.Fatal("setup: candidate did not promote")
	}
	verified, _ := m.Get(cand.ID)
	retire := func(rev int64) []MemoryAction {
		return []MemoryAction{{Op: "retire", ID: cand.ID, Revision: rev, Rationale: "Superseded by a newer run."}}
	}

	// Not listed: refused.
	if err := c.applyMemoryActions(head.ID, retire(verified.Revision)); err != nil {
		t.Fatal(err)
	}
	if !isVerified(m, cand.ID) {
		t.Fatal("unpresented verified entry was retired")
	}

	// Listed at another revision: refused.
	presentPending(c)
	_ = c.applyMemoryActions(head.ID, retire(verified.Revision+1))
	if !isVerified(m, cand.ID) {
		t.Fatal("retire at a wrong revision was applied")
	}

	// Listed at its revision: retired.
	presentPending(c)
	var retired []map[string]any
	c.Emit = func(name string, payload any) {
		if m, ok := payload.(map[string]any); ok && name == "memory.updated" {
			retired = append(retired, m)
		}
	}
	if err := c.applyMemoryActions(head.ID, retire(verified.Revision)); err != nil {
		t.Fatal(err)
	}
	// The store keeps the base revision on a retirement; the event must carry the revision the row now holds.
	if got, _ := m.Get(cand.ID); got.State != memory.StateRetired {
		t.Fatalf("listed verified entry state = %s, want retired", got.State)
	}
	stored, _ := m.Get(cand.ID)
	if len(retired) != 1 || retired[0]["revision"] != stored.Revision || stored.Revision != verified.Revision+1 || retired[0]["change"] != "retire" {
		t.Fatalf("memory.updated = %v, want stored revision %d", retired, verified.Revision+1)
	}

	// A user entry is never listed, so the head cannot retire it.
	user, err := m.UserPromote(memory.Entry{Kind: "fact", Title: "User note", Body: "Owned by the user."})
	if err != nil {
		t.Fatal(err)
	}
	presentPending(c)
	_ = c.applyMemoryActions(head.ID, []MemoryAction{{Op: "retire", ID: user.ID, Revision: user.Revision, Rationale: "Stale."}})
	if got, _ := m.Get(user.ID); got.State != memory.StateVerified {
		t.Fatalf("head retired a user entry: state = %s", got.State)
	}
}

// A decision candidate promotes on a question the user answered; any presented candidate can be rejected without evidence.
func TestDecisionEvidenceAndRejectWithoutEvidence(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "decision", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	answered := c.Graph.Add(taskgraph.Task{Title: "Keep the old API?", BlockedOn: &taskgraph.BlockedOn{Question: "Keep the old API?",
		Answer: "Keep it.", AnsweredBy: taskgraph.AnsweredByUser, Resolution: "Keep it."}})
	decision, err := m.ProposeCandidate(memory.Entry{Kind: "decision", Title: "Keep the old API", Body: "The user chose to keep it.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceUserDecision, SessionID: s.ID, Ref: answered.ID, ObservedAt: time.Now().UTC(), Result: memory.ResultObserved}}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, decision)
	if !isVerified(m, decision.ID) {
		t.Fatal("decision candidate did not promote on a user-answered question")
	}

	unanswered := c.Graph.Add(taskgraph.Task{Title: "Rename?", BlockedOn: &taskgraph.BlockedOn{Question: "Rename?"}})
	guess, err := m.ProposeCandidate(memory.Entry{Kind: "decision", Title: "Rename the package", Body: "Guessed without an answer.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceUserDecision, SessionID: s.ID, Ref: unanswered.ID, ObservedAt: time.Now().UTC(), Result: memory.ResultObserved}}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, guess)
	if isVerified(m, guess.ID) {
		t.Fatal("decision promoted on an unanswered question")
	}

	// Reject needs no evidence: a candidate with none is discarded.
	bare, err := m.ProposeCandidate(memory.Entry{Kind: "fact", Title: "Stale note", Body: "No longer true."})
	if err != nil {
		t.Fatal(err)
	}
	presentPending(c)
	if err := c.applyMemoryActions(head.ID, []MemoryAction{{Op: "reject", ID: bare.ID, Revision: bare.Revision, Rationale: "Not needed."}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(bare.ID); ok {
		t.Fatal("presented candidate was not rejected without evidence")
	}
}

// A decision candidate promotes only on a question the user answered: an answer the Primary or a delegate gave, or a
// head-quoted answer, never justifies one. The head's own answeredBy claim does not make its answer the user's.
func TestDecisionEvidenceRequiresUserAnswer(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "decision-source", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})

	// The Primary answers the question itself, quoting its own text. applyResolvedBlocks records it as primary: the
	// head's answeredBy claim in its turn JSON is never trusted.
	quoted := c.Graph.Add(taskgraph.Task{Title: "Keep the old API?", Status: taskgraph.Asking,
		BlockedOn: &taskgraph.BlockedOn{Question: "Keep the old API?", BlockedAt: time.Now().UTC()}})
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: quoted.ID, Answer: "Keep it.", AnsweredBy: taskgraph.AnsweredByUser}}, "keep it, use the old API")
	if got, _ := c.Graph.Get(quoted.ID); got.BlockedOn.AnsweredBy == taskgraph.AnsweredByUser {
		t.Fatalf("head's own answer was recorded as the user's: %+v", got.BlockedOn)
	}
	selfDecision, err := m.ProposeCandidate(memory.Entry{Kind: "decision", Title: "Keep the old API", Body: "The head decided to keep it.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceUserDecision, SessionID: s.ID, Ref: quoted.ID, ObservedAt: time.Now().UTC(), Result: memory.ResultObserved}}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, selfDecision)
	if isVerified(m, selfDecision.ID) {
		t.Fatal("decision promoted on the primary's own answer")
	}

	// An answer the primary backs with durable evidence is recorded as primary, and still does not justify a decision.
	c.applyObjective(head.ID, "Use contract B for the second stage")
	cited := c.Graph.Add(taskgraph.Task{Title: "Which contract for stage two?", Status: taskgraph.Asking,
		BlockedOn: &taskgraph.BlockedOn{Question: "Which contract for stage two?", BlockedAt: time.Now().UTC()}})
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: cited.ID, Answer: "Use contract B.", Cite: "Use contract B"}})
	if got, _ := c.Graph.Get(cited.ID); got.BlockedOn.Unresolved() || got.BlockedOn.AnsweredBy != taskgraph.AnsweredByPrimary {
		t.Fatalf("cited primary answer was not recorded as primary: %+v", got.BlockedOn)
	}
	citedDecision, err := m.ProposeCandidate(memory.Entry{Kind: "decision", Title: "Use contract B", Body: "The head chose contract B.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceUserDecision, SessionID: s.ID, Ref: cited.ID, ObservedAt: time.Now().UTC(), Result: memory.ResultObserved}}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, citedDecision)
	if isVerified(m, citedDecision.ID) {
		t.Fatal("decision promoted on an evidence-backed primary answer")
	}

	// A delegated answer does not justify a decision either.
	c.SetDelegateAnswers(true)
	delegated := c.Graph.Add(taskgraph.Task{Title: "Which contract?", Status: taskgraph.Asking,
		BlockedOn: &taskgraph.BlockedOn{Question: "Which contract?", BlockedAt: time.Now().UTC()}})
	if err := c.recordQuestionAnswer(delegated.ID, "Use contract A", taskgraph.AnsweredByDelegated, ""); err != nil {
		t.Fatal(err)
	}
	delegatedDecision, err := m.ProposeCandidate(memory.Entry{Kind: "decision", Title: "Use contract A", Body: "A delegate chose contract A.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceUserDecision, SessionID: s.ID, Ref: delegated.ID, ObservedAt: time.Now().UTC(), Result: memory.ResultObserved}}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, delegatedDecision)
	if isVerified(m, delegatedDecision.ID) {
		t.Fatal("decision promoted on a delegated answer")
	}

	// The user's own answer still promotes.
	user := c.Graph.Add(taskgraph.Task{Title: "Rename?", Status: taskgraph.Asking,
		BlockedOn: &taskgraph.BlockedOn{Question: "Rename?", BlockedAt: time.Now().UTC()}})
	if err := c.AnswerQuestion(user.ID, "Do not rename."); err != nil {
		t.Fatal(err)
	}
	userDecision, err := m.ProposeCandidate(memory.Entry{Kind: "decision", Title: "Do not rename", Body: "The user chose not to rename.",
		Evidence: []memory.Evidence{{Type: memory.EvidenceUserDecision, SessionID: s.ID, Ref: user.ID, ObservedAt: time.Now().UTC(), Result: memory.ResultObserved}}})
	if err != nil {
		t.Fatal(err)
	}
	promoteCited(c, head, userDecision)
	if !isVerified(m, userDecision.ID) {
		t.Fatal("decision candidate did not promote on the user's answer")
	}
}

// A failed pending listing leaves no presented set, so a decision in that turn fails closed instead of reusing the
// previous turn's listing.
func TestPendingLoadErrorClearsPresented(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "load-error", ModeBuild)
	c.Settings.Memory = true
	p, err := persistence.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := memory.Open(p.DB(), s.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	c.Memory = m
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	cand := candidateWithCheck(t, c, m, "ver-load")

	// A listing is presented, then the load fails before the head decides.
	presentPending(c)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if items := c.pendingMemoryItems(); len(items) != 0 {
		t.Fatalf("a failed load listed candidates: %d items", len(items))
	}
	_ = c.applyMemoryActions(head.ID, []MemoryAction{{Op: "promote", ID: cand.ID, Revision: cand.Revision, Evidence: []string{"ver-load"}, Rationale: "Checked by the run."}})
	if presented := c.takePresentedMemory(); len(presented) != 0 {
		t.Fatalf("presented set survived a failed load: %v", presented)
	}
	if isVerified(m, cand.ID) {
		t.Fatal("decision applied from a listing built before the failed load")
	}
}

// File evidence resolves against the worktree of the agent that observed it, and refuses paths that break the memory
// path rules and files over the size bound.
func TestFileEvidenceWorktreeAndBounds(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "files", ModeBuild)
	worktree := t.TempDir()
	observer := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: worktree})
	if err := os.MkdirAll(filepath.Join(worktree, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "notes", "a.txt"), []byte("observed"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := hashFile(filepath.Join(worktree, "notes", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ev := func(path, hash string) memory.Evidence {
		return memory.Evidence{Type: memory.EvidenceFileObservation, SessionID: s.ID, AgentID: observer.ID, Ref: "ev-file",
			ObservedAt: time.Now().UTC(), Result: memory.ResultObserved, Files: []memory.EvidenceFile{{Path: path, SHA256: hash}}}
	}
	now := time.Now().UTC()

	// The file exists only in the observer's worktree, not the session workspace, and still resolves.
	if _, err := c.resolveFile(ev("notes/a.txt", sum), now); err != nil {
		t.Fatalf("worktree-only file evidence: %v", err)
	}
	if _, err := c.resolveFile(ev("notes/a.txt", strings.Repeat("0", 64)), now); err != errEvidenceStale {
		t.Fatalf("changed bytes: err = %v, want stale", err)
	}
	if _, err := c.resolveFile(ev("../outside.txt", sum), now); err != errEvidenceUnresolved {
		t.Fatalf("escaping path: err = %v, want unresolved", err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.resolveFile(ev("linked/secret.txt", sum), now); err != errEvidenceStale {
		t.Fatalf("file reached through a symlinked directory: err = %v, want stale", err)
	}

	big := make([]byte, maxEvidenceFileBytes+1)
	if err := os.WriteFile(filepath.Join(worktree, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	bigSum, err := hashFile(filepath.Join(worktree, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.resolveFile(ev("big.bin", bigSum), now); err != errEvidenceUnresolved {
		t.Fatalf("oversized file: err = %v, want unresolved", err)
	}
}

// The Primary prompt documents memoryActions, and a turn carrying them with tasks [] parses and applies.
func TestPrimaryTurnWithMemoryActionsAndNoTasksApplies(t *testing.T) {
	if !strings.Contains(prompt.Primary, "memoryActions") {
		t.Fatal("primary prompt does not document memoryActions")
	}
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "turn", ModeBuild)
	c.Settings.Memory = true
	m := openTestMemory(t, c, s.Workspace)
	head := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	cand := candidateWithCheck(t, c, m, "ver-turn")
	presentPending(c)

	body := fmt.Sprintf(`{"status":"completed","userMessage":"Rejected the note.","tasks":[],"memoryActions":[{"op":"reject","id":%q,"revision":%d,"rationale":"Not needed."}]}`, cand.ID, cand.Revision)
	turn, err := ParseTurn(body, c.knownTask)
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}
	if len(turn.Tasks) != 0 || len(turn.MemoryActions) != 1 {
		t.Fatalf("turn tasks=%d memoryActions=%d, want 0 and 1", len(turn.Tasks), len(turn.MemoryActions))
	}
	if err := c.applyMemoryActions(head.ID, turn.MemoryActions); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(cand.ID); ok {
		t.Fatal("memoryActions in a tasks [] turn did not apply")
	}
}

// A verified file observation yields an anchor carrying the worktree's commit and the files it hashed, which core then
// stamps; an absent marker that still holds yields the marker itself.
func TestFileEvidenceAnchorCarriesWorktreeCommit(t *testing.T) {
	r := initRepo(t)
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "anchors", ModeBuild)
	observer := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: r.dir})
	sum, err := hashFile(filepath.Join(r.dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ev := memory.Evidence{Type: memory.EvidenceFileObservation, SessionID: s.ID, AgentID: observer.ID, Ref: "ev-anchor",
		ObservedAt: now, Result: memory.ResultObserved, Files: []memory.EvidenceFile{
			{Path: "a.txt", SHA256: sum},
			{Path: "missing.txt", Absent: true},
		}}
	anchors, err := c.resolveEvidence(memory.Entry{Evidence: []memory.Evidence{ev}}, []string{"ev-anchor"}, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	a := anchors["ev-anchor"]
	if a.Commit != r.head || len(a.Files) != 2 || a.Files[0].SHA256 != sum || !a.Files[1].Absent {
		t.Fatalf("anchor = %+v, want head %s with recorded files", a, r.head)
	}
}
