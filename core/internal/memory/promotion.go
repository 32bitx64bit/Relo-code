package memory

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrEvidence means a promotion cited evidence that is missing, stale, unrelated or does not support the claim's
// kind. The message names no payload text.
var ErrEvidence = errors.New("evidence_rejected")

// PromotionRequest is the head decision for one candidate (design §4). Core checks the turn identity, the candidate
// revision, the cited references and the safety of the rationale before it commits. Promotion metadata and the
// promoted state are never taken from the request.
type PromotionRequest struct {
	HeadTurnID   string
	EvidenceRefs []string
	Rationale    string
	// Anchors maps a cited reference to the commit and file state verified at promotion. Optional. Core rejects an
	// anchor for an uncited reference and any file list that differs from what the candidate recorded.
	Anchors map[string]EvidenceAnchor
	// Anchor is the view captured by the caller at promotion, in the observing agent's worktree. Optional: nil keeps
	// the anchor the candidate recorded. Core validates its shape and never derives it from the caller's identity.
	Anchor *Anchor
}

// HeadPromote makes an agent-originated candidate permanent: it becomes verified with source head, the promotion
// metadata, and the revision bumped by one. It needs the candidate's current revision, at least one cited evidence
// reference recorded on the candidate, and a rationale. A candidate that is flagged, expired, supersedes a user
// entry, or cites evidence that does not support it is refused and stays a candidate. Verified entries are never
// evicted: a full quota refuses the promotion. The promoted row keeps its ID.
func (s *Store) HeadPromote(candidateID string, rev int64, req PromotionRequest) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	if err := validateDecisionText(req.HeadTurnID, req.Rationale); err != nil {
		return Entry{}, err
	}
	if len(req.EvidenceRefs) == 0 || len(req.EvidenceRefs) > MaxEvidence {
		return Entry{}, ErrEvidence
	}
	return retry(func() (Entry, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, candidateID)
		if err != nil {
			return Entry{}, err
		}
		if cur.Revision != rev {
			return Entry{}, &ConflictError{Current: cur}
		}
		now := s.now()
		if cur.State != StateCandidate {
			return Entry{}, ErrInvalid
		}
		if cur.Source == SourceUser {
			return Entry{}, ErrUserReview
		}
		if cur.Flagged {
			return Entry{}, ErrUserReview
		}
		if now.Sub(cur.Updated) > CandidateTTL {
			return Entry{}, ErrNotFound
		}
		if err := checkEvidence(cur, req.EvidenceRefs, now); err != nil {
			return Entry{}, err
		}
		next := cur
		if err := stampAnchors(&next, req.EvidenceRefs, req.Anchors); err != nil {
			return Entry{}, err
		}
		next.State = StateVerified
		next.Verified = true
		next.Source = SourceHead
		next.ReviewRequested = false
		next.Revision = rev + 1
		next.Updated = now
		next.Promotion = &Promotion{HeadTurnID: req.HeadTurnID, Rationale: req.Rationale, PromotedAt: now}
		if req.Anchor != nil {
			next.Anchor = req.Anchor
		}
		if err := validate(next); err != nil {
			return Entry{}, err
		}
		// Every check runs before the supersedes target is touched. A refused promotion leaves the target as it was.
		var target Entry
		retiring := false
		if cur.SupersedesID != "" {
			// Supersession: the verified entry this candidate replaces is retired in this commit. A target already
			// gone has nothing left to retire, so the promotion goes ahead. A target whose revision moved since the
			// candidate was proposed is refused: the head decided on a different entry.
			t, err := s.load(tx, cur.SupersedesID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return Entry{}, err
			}
			if err == nil {
				if t.Source == SourceUser {
					return Entry{}, ErrUserReview
				}
				if t.Revision != cur.SupersedesRevision {
					return Entry{}, ErrInvalid
				}
				target, retiring = t, t.State == StateVerified
			}
		}
		// Retiring a verified target frees one slot, so the swap is quota-neutral.
		freed := 0
		if retiring {
			freed = 1
		}
		if err := s.checkQuotaFreeing(tx, freed); err != nil {
			return Entry{}, err
		}
		if retiring {
			if _, err := s.retire(tx, target, target.Revision, now); err != nil {
				return Entry{}, err
			}
		}
		if err := s.write(tx, next, rev); err != nil {
			return Entry{}, err
		}
		return next, dbErr(tx.Commit())
	})
}

// HeadRetire withdraws a verified agent-originated entry on a head decision. It refuses user-provenance entries with
// ErrUserReview, so an agent can never retire a user entry, and a stale revision with *ConflictError. The turn ID and
// rationale are validated with the same rules as a promotion but are not stored: §3 has no field for them yet.
func (s *Store) HeadRetire(id string, rev int64, headTurnID, rationale string) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	if err := validateDecisionText(headTurnID, rationale); err != nil {
		return Entry{}, err
	}
	return retry(func() (Entry, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, id)
		if err != nil {
			return Entry{}, err
		}
		if cur.Revision != rev {
			return Entry{}, &ConflictError{Current: cur}
		}
		if cur.Source == SourceUser {
			return Entry{}, ErrUserReview
		}
		next, err := s.retire(tx, cur, rev, s.now())
		if err != nil {
			return Entry{}, err
		}
		return next, dbErr(tx.Commit())
	})
}

// HeadReject discards a pending agent candidate on the head's decision. It needs the candidate's current revision and
// a rationale. A stale revision returns *ConflictError; a user-provenance row returns ErrUserReview; a row that is not a
// candidate returns ErrInvalid. The row becomes a content-free tombstone, like a user delete.
func (s *Store) HeadReject(id string, rev int64, headTurnID, rationale string) error {
	if s.err != nil {
		return s.err
	}
	if err := validateDecisionText(headTurnID, rationale); err != nil {
		return err
	}
	_, err := retry(func() (struct{}, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return struct{}{}, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, id)
		if err != nil {
			return struct{}{}, err
		}
		if cur.Revision != rev {
			return struct{}{}, &ConflictError{Current: cur}
		}
		if cur.Source == SourceUser {
			return struct{}{}, ErrUserReview
		}
		if cur.State != StateCandidate {
			return struct{}{}, ErrInvalid
		}
		payload, err := json.Marshal(tombstone{SchemaVersion: SchemaVersion, ID: id, Revision: rev + 1, State: StateDeleted})
		if err != nil {
			return struct{}{}, err
		}
		res, err := tx.Exec(`UPDATE memory SET payload=?, revision=?, state=?, updated_at=? WHERE workspace=? AND id=? AND revision=? AND state=?`,
			string(payload), rev+1, StateDeleted, s.now().Unix(), s.ws, id, rev, StateCandidate)
		if err != nil {
			return struct{}{}, dbErr(err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return struct{}{}, s.missing(tx, id, rev)
		}
		return struct{}{}, dbErr(tx.Commit())
	})
	return err
}

// RequestReview asks for head or user review of an agent candidate. It changes no state and does not promote; it
// sets ReviewRequested under the revision check. Only candidates qualify, so a verified or user entry is refused.
func (s *Store) RequestReview(candidateID string, rev int64) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	return retry(func() (Entry, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, candidateID)
		if err != nil {
			return Entry{}, err
		}
		if cur.Revision != rev {
			return Entry{}, &ConflictError{Current: cur}
		}
		if cur.State != StateCandidate || cur.Source == SourceUser {
			return Entry{}, ErrInvalid
		}
		next := cur
		next.ReviewRequested = true
		next.Revision = rev + 1
		next.Updated = s.now()
		if err := validate(next); err != nil {
			return Entry{}, err
		}
		if err := s.write(tx, next, rev); err != nil {
			return Entry{}, err
		}
		return next, dbErr(tx.Commit())
	})
}

// checkEvidence is the promotion gate for evidence. Every cited reference must be recorded on the candidate; at least
// one must support the claim's kind; a command_result must be recent; and when the claim names paths, a cited
// file_observation must cover one of them. Existence and kind are checked here; semantic support is the head's call.
func checkEvidence(c Entry, refs []string, now time.Time) error {
	recorded := make(map[string]Evidence, len(c.Evidence))
	for _, ev := range c.Evidence {
		recorded[ev.Ref] = ev
	}
	var matched []Evidence
	for _, r := range refs {
		ev, ok := recorded[r]
		if !ok {
			return ErrEvidence
		}
		if ev.Type == EvidenceCommandResult && now.Sub(ev.ObservedAt) > CommandEvidenceTTL {
			return ErrEvidence
		}
		matched = append(matched, ev)
	}
	supports := func(pred func(Evidence) bool) bool {
		for _, ev := range matched {
			if pred(ev) {
				return true
			}
		}
		return false
	}
	ok := false
	switch c.Kind {
	case "command":
		ok = c.Command != nil && supports(func(ev Evidence) bool {
			return ev.Type == EvidenceCommandResult && ev.Result == ResultPassed
		})
	case "decision":
		ok = supports(func(ev Evidence) bool {
			return ev.Type == EvidenceUserDecision || ev.Type == EvidenceAcceptedArtifact
		})
	case "fact":
		ok = supports(func(ev Evidence) bool {
			return ev.Type == EvidenceFileObservation || ev.Type == EvidenceAcceptedArtifact ||
				(ev.Type == EvidenceCommandResult && ev.Result == ResultPassed)
		})
	case "pitfall":
		// A failed command or observed file may prove the pitfall. A working alternative needs its own evidence,
		// which this gate does not infer.
		ok = supports(func(ev Evidence) bool {
			return (ev.Type == EvidenceCommandResult && ev.Result == ResultFailed) || ev.Type == EvidenceFileObservation
		})
	}
	if !ok {
		return ErrEvidence
	}
	if len(c.Files) > 0 && (c.Kind == "fact" || c.Kind == "pitfall") {
		covers := false
		for _, ev := range matched {
			for _, f := range ev.Files {
				for _, p := range c.Files {
					if f.Path == p {
						covers = true
					}
				}
			}
		}
		if !covers {
			return ErrEvidence
		}
	}
	return nil
}

// stampAnchors copies the anchors verified at promotion onto the cited references of e. An anchor must name a cited
// reference, carry a valid commit, and list exactly the paths, absence markers and hashes the candidate recorded, so a
// stamp cannot change what was observed. The evidence slice is copied first so the candidate it came from is untouched.
func stampAnchors(e *Entry, refs []string, anchors map[string]EvidenceAnchor) error {
	if len(anchors) == 0 {
		return nil
	}
	cited := make(map[string]bool, len(refs))
	for _, r := range refs {
		cited[r] = true
	}
	for ref, a := range anchors {
		if !cited[ref] || (a.Commit != "" && !commitPattern.MatchString(a.Commit)) {
			return ErrInvalid
		}
	}
	e.Evidence = append([]Evidence(nil), e.Evidence...)
	for i := range e.Evidence {
		ev := &e.Evidence[i]
		a, ok := anchors[ev.Ref]
		if !ok {
			continue
		}
		if len(a.Files) != len(ev.Files) {
			return ErrEvidence
		}
		for j, f := range a.Files {
			if f != ev.Files[j] {
				return ErrEvidence
			}
		}
		if a.Commit != "" {
			ev.Commit = a.Commit
		}
	}
	return nil
}
