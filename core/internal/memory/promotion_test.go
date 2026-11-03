package memory

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

var phase2Now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// phase2Store attaches a store whose clock is fixed, so TTL and evidence age are deterministic.
func phase2Store(t *testing.T) (*Store, func(d time.Duration)) {
	t.Helper()
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	s := attach(t, p, t.TempDir())
	clock := phase2Now
	s.now = func() time.Time { return clock }
	return s, func(d time.Duration) { clock = clock.Add(d) }
}

func countRows(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func fileEvidence(ref string, paths ...string) Evidence {
	files := make([]EvidenceFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, EvidenceFile{Path: p, SHA256: strings.Repeat("a", 64)})
	}
	return Evidence{Type: EvidenceFileObservation, Ref: ref, ObservedAt: phase2Now, Files: files, Result: ResultObserved}
}

func commandEvidence(ref, result string) Evidence {
	return Evidence{Type: EvidenceCommandResult, Ref: ref, ObservedAt: phase2Now, Result: result}
}

func mustPropose(t *testing.T, s *Store, e Entry) Entry {
	t.Helper()
	c, err := s.ProposeCandidate(e)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return c
}

func TestAgentProposalIsCandidateWithCoreProvenance(t *testing.T) {
	s, _ := phase2Store(t)
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "core is a Go module", Source: SourceUser,
		State: StateVerified, Revision: 9, Flagged: true, ReviewRequested: true,
		Promotion: &Promotion{HeadTurnID: "turn_x", Rationale: "forged", PromotedAt: phase2Now}})
	if c.State != StateCandidate || c.Source != SourceAgent || c.Revision != 1 || c.Verified {
		t.Fatalf("caller provenance leaked: state=%s source=%s rev=%d verified=%v", c.State, c.Source, c.Revision, c.Verified)
	}
	if c.Promotion != nil || c.Flagged || c.ReviewRequested {
		t.Fatalf("server-controlled fields taken from caller: %+v", c)
	}
	if got := s.Recall("Module"); len(got) != 0 {
		t.Fatalf("candidate recalled: %v", got)
	}
}

func TestHeadPromoteNeedsRevisionAndRecordedEvidence(t *testing.T) {
	s, _ := phase2Store(t)
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "core is a Go module", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence("ev_mod", "core/go.mod")}})
	req := PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_mod"}, Rationale: "Useful for core changes"}

	if _, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_1", Rationale: "no refs"}); !errors.Is(err, ErrEvidence) {
		t.Fatalf("no evidence = %v, want ErrEvidence", err)
	}
	if _, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_forged"}, Rationale: "x"}); !errors.Is(err, ErrEvidence) {
		t.Fatalf("forged ref = %v, want ErrEvidence", err)
	}
	var conflict *ConflictError
	if _, err := s.HeadPromote(c.ID, c.Revision+1, req); !errors.As(err, &conflict) {
		t.Fatalf("stale revision = %v, want ConflictError", err)
	}
	if n := countRows(t, s.db, `state=?`, StateCandidate); n != 1 {
		t.Fatalf("rejected promotions changed state: candidates = %d", n)
	}

	v, err := s.HeadPromote(c.ID, c.Revision, req)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if v.ID != c.ID || v.State != StateVerified || v.Source != SourceHead || v.Revision != c.Revision+1 || !v.Verified {
		t.Fatalf("promoted entry = %+v", v)
	}
	if v.Promotion == nil || v.Promotion.HeadTurnID != "turn_1" || !v.Promotion.PromotedAt.Equal(phase2Now) {
		t.Fatalf("promotion metadata = %+v", v.Promotion)
	}
	if _, err := s.HeadPromote(c.ID, v.Revision, req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("replayed promotion of a verified entry = %v, want ErrInvalid", err)
	}
}

func TestHeadPromoteChecksEvidenceKindAndRelevance(t *testing.T) {
	s, _ := phase2Store(t)
	req := func(refs ...string) PromotionRequest {
		return PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: refs, Rationale: "supported by evidence"}
	}

	pitfall := mustPropose(t, s, Entry{Kind: "pitfall", Title: "Race", Body: "tests race on cleanup",
		Evidence: []Evidence{commandEvidence("ev_fail", ResultFailed), commandEvidence("ev_pass", ResultPassed)}})
	if _, err := s.HeadPromote(pitfall.ID, pitfall.Revision, req("ev_pass")); !errors.Is(err, ErrEvidence) {
		t.Fatalf("passing command proving a pitfall = %v, want ErrEvidence", err)
	}
	if _, err := s.HeadPromote(pitfall.ID, pitfall.Revision, req("ev_fail")); err != nil {
		t.Fatalf("failed command proving pitfall: %v", err)
	}

	cmd := mustPropose(t, s, Entry{Kind: "command", Title: "Run core tests", Body: "Run go test from core.",
		Command:  &Command{Argv: []string{"go", "test", "./..."}, Cwd: "core"},
		Evidence: []Evidence{commandEvidence("ev_cmd", ResultPassed)}})
	if _, err := s.HeadPromote(cmd.ID, cmd.Revision, req("ev_cmd")); err != nil {
		t.Fatalf("command fact with passing result: %v", err)
	}

	noCmd := mustPropose(t, s, Entry{Kind: "command", Title: "No argv", Body: "No command recorded.",
		Evidence: []Evidence{commandEvidence("ev_cmd2", ResultPassed)}})
	if _, err := s.HeadPromote(noCmd.ID, noCmd.Revision, req("ev_cmd2")); !errors.Is(err, ErrEvidence) {
		t.Fatalf("command kind without argv = %v, want ErrEvidence", err)
	}

	unrelated := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "core is a Go module", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence("ev_other", "docs/readme.md")}})
	if _, err := s.HeadPromote(unrelated.ID, unrelated.Revision, req("ev_other")); !errors.Is(err, ErrEvidence) {
		t.Fatalf("evidence on other file = %v, want ErrEvidence", err)
	}

	stale := mustPropose(t, s, Entry{Kind: "command", Title: "Old run", Body: "Run once.",
		Command: &Command{Argv: []string{"go", "vet"}, Cwd: "core"},
		Evidence: []Evidence{{Type: EvidenceCommandResult, Ref: "ev_old", Result: ResultPassed,
			ObservedAt: phase2Now.Add(-CommandEvidenceTTL - time.Hour)}}})
	if _, err := s.HeadPromote(stale.ID, stale.Revision, req("ev_old")); !errors.Is(err, ErrEvidence) {
		t.Fatalf("stale command evidence = %v, want ErrEvidence", err)
	}
}

func TestCandidateExpiresBeforePromotion(t *testing.T) {
	s, advance := phase2Store(t)
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Old", Body: "old observation",
		Evidence: []Evidence{fileEvidence("ev_old", "core/go.mod")}, Files: []string{"core/go.mod"}})
	advance(CandidateTTL + time.Second)
	if _, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_old"}, Rationale: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired candidate = %v, want ErrNotFound", err)
	}
}

func TestAgentCannotOverwriteOrDowngradeUserEntry(t *testing.T) {
	s, _ := phase2Store(t)
	u, err := s.UserPromote(Entry{Kind: "decision", Title: "Use SQLite", Body: "The user chose SQLite for the catalog."})
	if err != nil {
		t.Fatal(err)
	}

	contradiction := mustPropose(t, s, Entry{Kind: "decision", Title: "Use Postgres", Body: "Agent says Postgres.", SupersedesID: u.ID,
		Evidence: []Evidence{{Type: EvidenceUserDecision, Ref: "ev_msg", ObservedAt: phase2Now, Result: ResultObserved}}})
	if !contradiction.Flagged || contradiction.State != StateCandidate {
		t.Fatalf("contradiction = %+v, want flagged candidate", contradiction)
	}
	if _, err := s.HeadPromote(contradiction.ID, contradiction.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_msg"}, Rationale: "x"}); !errors.Is(err, ErrUserReview) {
		t.Fatalf("head promotion of contradiction = %v, want ErrUserReview", err)
	}
	if _, err := s.RequestReview(u.ID, u.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatalf("review request on user entry = %v, want ErrInvalid", err)
	}

	got, ok := s.Get(u.ID)
	if !ok || got.Revision != u.Revision || got.Source != SourceUser || got.Body != u.Body {
		t.Fatalf("user entry changed by agent path: %+v", got)
	}
	if _, err := s.UserUpdate(u.ID, u.Revision, Patch{Body: strPtr("The user chose SQLite.")}); err != nil {
		t.Fatalf("user edit after contradiction: %v", err)
	}
}

func TestSupersedesLinksReplacementWithoutTouchingOldEntry(t *testing.T) {
	s, _ := phase2Store(t)
	old := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "old",
		Evidence: []Evidence{fileEvidence("ev_old", "core/go.mod")}, Files: []string{"core/go.mod"}})
	old, err := s.HeadPromote(old.ID, old.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_old"}, Rationale: "first"})
	if err != nil {
		t.Fatal(err)
	}

	repl := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "replacement", SupersedesID: old.ID,
		Files: []string{"core/go.mod"}, Evidence: []Evidence{fileEvidence("ev_new", "core/go.mod")}})
	if repl.Flagged || repl.SupersedesID != old.ID {
		t.Fatalf("agent replacement = %+v", repl)
	}
	if _, err := s.ProposeCandidate(Entry{Kind: "fact", Title: "Missing", Body: "x", SupersedesID: "mem_missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("supersede unknown entry = %v, want ErrNotFound", err)
	}
	if got, _ := s.Get(old.ID); got.Revision != old.Revision || got.State != StateVerified || got.Body != "old" {
		t.Fatalf("old entry changed before replacement promoted: %+v", got)
	}
}

func TestRequestReviewIsCandidateOnlyAndRevisionChecked(t *testing.T) {
	s, _ := phase2Store(t)
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Note", Body: "a note"})
	var conflict *ConflictError
	if _, err := s.RequestReview(c.ID, c.Revision+1); !errors.As(err, &conflict) {
		t.Fatalf("stale review request = %v, want ConflictError", err)
	}
	r, err := s.RequestReview(c.ID, c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ReviewRequested || r.Revision != c.Revision+1 || r.State != StateCandidate {
		t.Fatalf("review request = %+v", r)
	}
	if _, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev"}, Rationale: "x"}); !errors.As(err, &conflict) {
		t.Fatalf("promotion with pre-review revision = %v, want ConflictError", err)
	}
}

func TestSecretScanRunsOnEveryAgentPath(t *testing.T) {
	s, _ := phase2Store(t)
	bad := []Entry{
		{Kind: "command", Title: "Run", Body: "x", Command: &Command{Argv: []string{"deploy", "--token=abc"}, Cwd: "."},
			Evidence: []Evidence{commandEvidence("ev_a", ResultPassed)}},
		{Kind: "command", Title: "Run", Body: "x", Command: &Command{Argv: []string{"go", "test"}, Cwd: ".", Environment: "API_KEY=1"},
			Evidence: []Evidence{commandEvidence("ev_b", ResultPassed)}},
		{Kind: "fact", Title: "Owner", Body: "x", Files: []string{"core/go.mod"},
			Evidence: []Evidence{{Type: EvidenceFileObservation, Ref: "ev_c", ObservedAt: phase2Now, Result: ResultObserved,
				Files: []EvidenceFile{{Path: "core/go.mod", SHA256: strings.Repeat("b", 64)}}}}},
	}
	for i, e := range bad[:2] {
		if _, err := s.ProposeCandidate(e); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("case %d = %v, want ErrUnsafe", i, err)
		}
	}
	if _, err := s.ProposeCandidate(bad[2]); err != nil {
		t.Fatalf("clean evidence rejected: %v", err)
	}
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "core is a Go module", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence("ev_mod", "core/go.mod")}})
	if _, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_mod"}, Rationale: "password: hunter2"}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("rationale with secret = %v, want ErrUnsafe", err)
	}
	if n := countRows(t, s.db, `state=?`, StateVerified); n != 0 {
		t.Fatalf("unsafe promotion stored: verified = %d", n)
	}
}

func TestEvidenceShapeIsValidatedOnProposal(t *testing.T) {
	s, _ := phase2Store(t)
	cases := map[string]Entry{
		"file without hash": {Kind: "fact", Title: "T", Body: "b", Evidence: []Evidence{{Type: EvidenceFileObservation, Ref: "ev_1",
			ObservedAt: phase2Now, Result: ResultObserved, Files: []EvidenceFile{{Path: "core/go.mod"}}}}},
		"command observed": {Kind: "command", Title: "T", Body: "b", Command: &Command{Argv: []string{"go"}, Cwd: "."},
			Evidence: []Evidence{commandEvidence("ev_2", ResultObserved)}},
		"escaping path": {Kind: "fact", Title: "T", Body: "b", Evidence: []Evidence{{Type: EvidenceFileObservation, Ref: "ev_3",
			ObservedAt: phase2Now, Result: ResultObserved, Files: []EvidenceFile{{Path: "../etc/passwd", SHA256: strings.Repeat("c", 64)}}}}},
		"unknown type": {Kind: "fact", Title: "T", Body: "b", Evidence: []Evidence{{Type: "transcript", Ref: "ev_4", ObservedAt: phase2Now, Result: ResultObserved}}},
		"no timestamp": {Kind: "fact", Title: "T", Body: "b", Evidence: []Evidence{{Type: EvidenceAcceptedArtifact, Ref: "ev_5", Result: ResultObserved}}},
	}
	for name, e := range cases {
		if _, err := s.ProposeCandidate(e); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if n := countRows(t, s.db, `1=1`); n != 0 {
		t.Fatalf("rejected proposals stored %d rows", n)
	}
}

func TestHeadPromoteRefusedAtVerifiedQuotaKeepsCandidate(t *testing.T) {
	s, _ := phase2Store(t)
	s.maxVerified = 1
	if _, err := s.UserPromote(Entry{Kind: "fact", Title: "Kept", Body: "kept"}); err != nil {
		t.Fatal(err)
	}
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "core is a Go module", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence("ev_mod", "core/go.mod")}})
	if _, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_mod"}, Rationale: "x"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("promotion over quota = %v, want ErrQuota", err)
	}
	if n := countRows(t, s.db, `state=?`, StateCandidate); n != 1 {
		t.Fatalf("candidate lost on quota refusal: %d", n)
	}
}

func TestHeadPromoteSupersedesRetiresOldVerifiedEntry(t *testing.T) {
	s, _ := phase2Store(t)
	old := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "old", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence("ev_old", "core/go.mod")}})
	old, err := s.HeadPromote(old.ID, old.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_old"}, Rationale: "first"})
	if err != nil {
		t.Fatal(err)
	}
	repl := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "replacement", SupersedesID: old.ID,
		Files: []string{"core/go.mod"}, Evidence: []Evidence{fileEvidence("ev_new", "core/go.mod")}})
	if _, err := s.HeadPromote(repl.ID, repl.Revision, PromotionRequest{HeadTurnID: "turn_2", EvidenceRefs: []string{"ev_new"}, Rationale: "newer"}); err != nil {
		t.Fatalf("promote replacement: %v", err)
	}
	got, ok := s.Get(old.ID)
	if !ok || got.State != StateRetired || got.Verified || got.Revision != old.Revision+1 || got.Body != "old" {
		t.Fatalf("superseded entry = %+v, want retired with revision+1", got)
	}
	if n := countRows(t, s.db, `state=?`, StateVerified); n != 1 {
		t.Fatalf("verified rows = %d, want only the replacement", n)
	}
}

func TestSupersedingMissingTargetStillPromotes(t *testing.T) {
	s, _ := phase2Store(t)
	old := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "old", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence("ev_old", "core/go.mod")}})
	repl := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "replacement", SupersedesID: old.ID,
		Files: []string{"core/go.mod"}, Evidence: []Evidence{fileEvidence("ev_new", "core/go.mod")}})
	if err := s.UserDelete(old.ID, old.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HeadPromote(repl.ID, repl.Revision, PromotionRequest{HeadTurnID: "turn_2", EvidenceRefs: []string{"ev_new"}, Rationale: "newer"}); err != nil {
		t.Fatalf("promote after target deleted: %v", err)
	}
}

func TestRetireAndReinstateRules(t *testing.T) {
	s, _ := phase2Store(t)
	u, err := s.UserPromote(Entry{Kind: "decision", Title: "Use SQLite", Body: "The user chose SQLite."})
	if err != nil {
		t.Fatal(err)
	}
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "core is a Go module", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence("ev_mod", "core/go.mod")}})
	agent, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_1", EvidenceRefs: []string{"ev_mod"}, Rationale: "useful"})
	if err != nil {
		t.Fatal(err)
	}

	var conflict *ConflictError
	if _, err := s.HeadRetire(u.ID, u.Revision, "turn_1", "x"); !errors.Is(err, ErrUserReview) {
		t.Fatalf("head retire of user entry = %v, want ErrUserReview", err)
	}
	if _, err := s.UserRetire(agent.ID, agent.Revision+1); !errors.As(err, &conflict) {
		t.Fatalf("stale retire = %v, want ConflictError", err)
	}
	retired, err := s.UserRetire(agent.ID, agent.Revision)
	if err != nil || retired.State != StateRetired || retired.Source != SourceHead || retired.Revision != agent.Revision+1 {
		t.Fatalf("user retire = %+v, %v", retired, err)
	}
	if _, err := s.UserRetire(agent.ID, retired.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retire of retired entry = %v, want ErrInvalid", err)
	}
	if _, err := s.UserUpdate(agent.ID, retired.Revision, Patch{Body: strPtr("edited")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reinstating agent entry by user edit = %v, want ErrInvalid", err)
	}

	ur, err := s.UserRetire(u.ID, u.Revision)
	if err != nil || ur.State != StateRetired || ur.Source != SourceUser {
		t.Fatalf("user retire of user entry = %+v, %v", ur, err)
	}
	back, err := s.UserUpdate(u.ID, ur.Revision, Patch{Body: strPtr("The user chose SQLite again.")})
	if err != nil || back.State != StateVerified || back.Source != SourceUser {
		t.Fatalf("user reinstatement = %+v, %v", back, err)
	}
}

// promotedVerified proposes and head-promotes one agent candidate, returning the verified row.
func promotedVerified(t *testing.T, s *Store, title, ref string, supersedes Entry) Entry {
	t.Helper()
	e := Entry{Kind: "fact", Title: title, Body: title + " body", Files: []string{"core/go.mod"},
		Evidence: []Evidence{fileEvidence(ref, "core/go.mod")}}
	if supersedes.ID != "" {
		e.SupersedesID = supersedes.ID
	}
	c := mustPropose(t, s, e)
	v, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_" + ref, EvidenceRefs: []string{ref}, Rationale: "kept"})
	if err != nil {
		t.Fatalf("promote %s: %v", ref, err)
	}
	return v
}

func TestRejectedReplacementLeavesTargetUnchanged(t *testing.T) {
	s, _ := phase2Store(t)
	old := promotedVerified(t, s, "Old", "ev_old", Entry{})
	repl := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "replacement", SupersedesID: old.ID,
		Files: []string{"core/go.mod"}, Evidence: []Evidence{fileEvidence("ev_new", "core/go.mod")}})
	// The cited reference is not on the candidate, so evidence is refused. The target must not be retired first.
	if _, err := s.HeadPromote(repl.ID, repl.Revision, PromotionRequest{HeadTurnID: "turn_2", EvidenceRefs: []string{"ev_missing"}, Rationale: "x"}); !errors.Is(err, ErrEvidence) {
		t.Fatalf("promotion with missing evidence = %v, want ErrEvidence", err)
	}
	got, ok := s.Get(old.ID)
	if !ok || got.State != StateVerified || got.Revision != old.Revision || got.Body != old.Body {
		t.Fatalf("target after rejected promotion = %+v, want verified at revision %d", got, old.Revision)
	}
}

func TestChangedSupersedesTargetIsRefused(t *testing.T) {
	s, _ := phase2Store(t)
	old := promotedVerified(t, s, "Old", "ev_old", Entry{})
	repl := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "replacement", SupersedesID: old.ID,
		Files: []string{"core/go.mod"}, Evidence: []Evidence{fileEvidence("ev_new", "core/go.mod")}})
	// A user retire changes the target's revision and state while leaving its content in place.
	edited, err := s.UserRetire(old.ID, old.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HeadPromote(repl.ID, repl.Revision, PromotionRequest{HeadTurnID: "turn_2", EvidenceRefs: []string{"ev_new"}, Rationale: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("promotion after target changed = %v, want ErrInvalid", err)
	}
	got, _ := s.Get(old.ID)
	if got.State != StateRetired || got.Revision != edited.Revision || got.Body != edited.Body {
		t.Fatalf("changed target = %+v, want untouched at revision %d", got, edited.Revision)
	}
}

func TestReplacementPromotionSucceedsAtMaxVerified(t *testing.T) {
	s, _ := phase2Store(t)
	old := promotedVerified(t, s, "Old", "ev_old", Entry{})
	s.maxVerified = 1 // the quota is full with the target alone; the swap frees the slot it needs
	repl := mustPropose(t, s, Entry{Kind: "fact", Title: "Module", Body: "replacement", SupersedesID: old.ID,
		Files: []string{"core/go.mod"}, Evidence: []Evidence{fileEvidence("ev_new", "core/go.mod")}})
	if _, err := s.HeadPromote(repl.ID, repl.Revision, PromotionRequest{HeadTurnID: "turn_2", EvidenceRefs: []string{"ev_new"}, Rationale: "x"}); err != nil {
		t.Fatalf("replacement at MaxVerified = %v, want success", err)
	}
	if n := countRows(t, s.db, `state=?`, StateVerified); n != 1 {
		t.Fatalf("verified rows = %d, want 1", n)
	}
}

func TestUserRetireRecordsUserActorAndKeepsSource(t *testing.T) {
	s, _ := phase2Store(t)
	agent := promotedVerified(t, s, "Module", "ev_mod", Entry{})
	retired, err := s.UserRetire(agent.ID, agent.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if retired.RetiredBy != SourceUser || retired.Source != SourceHead {
		t.Fatalf("user retire = retiredBy %q source %q, want user and head", retired.RetiredBy, retired.Source)
	}
	if got, ok := s.Get(agent.ID); !ok || got.RetiredBy != SourceUser {
		t.Fatalf("retired row read back = %+v, %v", got, ok)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT payload FROM memory WHERE id=?`, agent.ID).Scan(&stored); err != nil || !strings.Contains(stored, `"retiredBy":"user"`) {
		t.Fatalf("stored retire payload lacks user actor: %v", err)
	}
}

// Promotion stamps the verified commit onto the cited observation and refuses anchors that disagree with the record.
func TestPromotionStampsAnchorsAndRefusesMismatch(t *testing.T) {
	s, _ := phase2Store(t)
	const commit = "abcdef1234567"
	cand := mustPropose(t, s, Entry{Kind: "fact", Title: "Anchored fact", Body: "Observed in a.go.", Files: []string{"a.go"},
		Evidence: []Evidence{fileEvidence("ev-a", "a.go")}})
	req := PromotionRequest{HeadTurnID: "turn-1", EvidenceRefs: []string{"ev-a"}, Rationale: "Observed.",
		Anchors: map[string]EvidenceAnchor{"ev-a": {Commit: commit, Files: cand.Evidence[0].Files}}}
	got, err := s.HeadPromote(cand.ID, cand.Revision, req)
	if err != nil {
		t.Fatalf("anchored promotion: %v", err)
	}
	if got.Evidence[0].Commit != commit || got.Evidence[0].Files[0].SHA256 != cand.Evidence[0].Files[0].SHA256 {
		t.Fatalf("stamped evidence = %+v, want commit %s and recorded hash", got.Evidence[0], commit)
	}

	other := mustPropose(t, s, Entry{Kind: "fact", Title: "Mismatched fact", Body: "Observed in a.go.", Files: []string{"a.go"},
		Evidence: []Evidence{fileEvidence("ev-b", "a.go")}})
	forged := EvidenceAnchor{Commit: commit, Files: []EvidenceFile{{Path: "a.go", SHA256: strings.Repeat("b", 64)}}}
	req.EvidenceRefs = []string{"ev-b"}
	req.Anchors = map[string]EvidenceAnchor{"ev-b": forged}
	if _, err := s.HeadPromote(other.ID, other.Revision, req); err != ErrEvidence {
		t.Fatalf("forged hash: err = %v, want ErrEvidence", err)
	}
	req.Anchors = map[string]EvidenceAnchor{"ev-a": {Commit: commit}}
	if _, err := s.HeadPromote(other.ID, other.Revision, req); err != ErrInvalid {
		t.Fatalf("anchor for uncited ref: err = %v, want ErrInvalid", err)
	}
	req.Anchors = map[string]EvidenceAnchor{"ev-b": {Commit: "not-hex"}}
	if _, err := s.HeadPromote(other.ID, other.Revision, req); err != ErrInvalid {
		t.Fatalf("malformed commit: err = %v, want ErrInvalid", err)
	}
	if c, ok := s.Get(other.ID); !ok || c.State != StateCandidate {
		t.Fatalf("refused promotion changed the candidate: %+v", c)
	}
}
