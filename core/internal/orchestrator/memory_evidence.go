package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/memory/freshness"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
)

var (
	// errNotPresented refuses a decision on a candidate the Primary was not shown in its latest turn, or was shown at
	// another revision. It also refuses a replay, because a listing is spent by the decisions that cite it.
	errNotPresented = errors.New("memory candidate was not presented to the primary at this revision")
	// errEvidenceUnresolved refuses evidence that no durable local record backs.
	errEvidenceUnresolved = errors.New("evidence does not resolve to a durable local record")
	// errEvidenceStale refuses evidence that expired or whose file bytes changed since observation.
	errEvidenceStale = errors.New("evidence is stale")
)

// fileEvidenceTTL bounds how long a file_observation can justify a promotion. Design §5 expires command and environment
// facts after 30 days but sets no time limit for file facts, which are otherwise governed by their hash. The command
// bound is reused here as a deliberate, documented choice until the design names one.
const fileEvidenceTTL = memory.CommandEvidenceTTL

// maxEvidenceFileBytes bounds the file a file_observation may hash. A larger file is refused, never read.
const maxEvidenceFileBytes = 1 << 20

// setPresentedMemory records the candidates listed to the Primary in its latest turn, with their revisions.
func (c *Coord) setPresentedMemory(presented map[string]int64) {
	c.memoryMu.Lock()
	defer c.memoryMu.Unlock()
	c.memoryPresented = presented
}

// takePresentedMemory returns the listing the Primary was shown and clears it, so each listing is spent by one turn.
func (c *Coord) takePresentedMemory() map[string]int64 {
	c.memoryMu.Lock()
	defer c.memoryMu.Unlock()
	presented := c.memoryPresented
	c.memoryPresented = nil
	return presented
}

// dropPresentedMemory forgets the listing without a decision. A failed or repaired Primary turn and a peer-guidance turn
// call it: none of them applies decisions on the listing it was built on.
func (c *Coord) dropPresentedMemory() {
	c.setPresentedMemory(nil)
}

// relistMemory puts the pending candidates back into the Primary's context before a context refresh, so the refreshed
// generation carries the listing and the next turn may decide on exactly what it shows.
func (c *Coord) relistMemory(primaryID string) {
	if !c.settings().Memory {
		return
	}
	if items := c.pendingMemoryItems(); len(items) > 0 {
		c.Engine.Ingest(primaryID, items)
	}
}

// decideMemory applies one head decision on a candidate or a listed verified agent entry. Promote, reject and retire
// must cite an id that was presented at this revision. Promote also resolves every evidence ref against durable local
// records before the head gate. Reject needs no evidence. It returns the entry the store committed, so callers report
// the stored revision rather than an assumed one.
func (c *Coord) decideMemory(m MemoryAction, presented map[string]int64, turnID string) (memory.Entry, error) {
	switch m.Op {
	case "promote":
		if err := checkPresented(presented, m); err != nil {
			return memory.Entry{}, err
		}
		cand, ok := c.Memory.Get(m.ID)
		if !ok {
			return memory.Entry{}, memory.ErrNotFound
		}
		anchors, err := c.resolveEvidence(cand, m.Evidence, time.Now().UTC())
		if err != nil {
			return memory.Entry{}, err
		}
		req := memory.PromotionRequest{HeadTurnID: turnID, EvidenceRefs: m.Evidence, Rationale: m.Rationale, Anchors: anchors, Anchor: c.promotionAnchor(cand, m.Evidence)}
		return c.Memory.HeadPromote(m.ID, m.Revision, req)
	case "reject":
		if err := checkPresented(presented, m); err != nil {
			return memory.Entry{}, err
		}
		return memory.Entry{}, c.Memory.HeadReject(m.ID, m.Revision, turnID, m.Rationale)
	case "retire":
		if err := checkPresented(presented, m); err != nil {
			return memory.Entry{}, err
		}
		return c.Memory.HeadRetire(m.ID, m.Revision, turnID, m.Rationale)
	}
	return memory.Entry{}, fmt.Errorf("unknown memory op %q", m.Op)
}

func checkPresented(presented map[string]int64, m MemoryAction) error {
	rev, ok := presented[m.ID]
	if !ok || rev != m.Revision {
		return errNotPresented
	}
	return nil
}

// resolveEvidence checks every cited reference against durable local records: the session's check records for command
// results, the observing agent's worktree for file observations, and the answered task for user decisions. Any ref that
// resolves to nothing, or to something stale, refuses the whole promotion. The store is not touched on refusal. It
// returns the anchors a verified file observation stamps: the worktree's commit and the files it just hashed.
func (c *Coord) resolveEvidence(cand memory.Entry, refs []string, now time.Time) (map[string]memory.EvidenceAnchor, error) {
	s := c.Current()
	if s == nil {
		return nil, errEvidenceUnresolved
	}
	recorded := make(map[string]memory.Evidence, len(cand.Evidence))
	for _, ev := range cand.Evidence {
		recorded[ev.Ref] = ev
	}
	anchors := make(map[string]memory.EvidenceAnchor)
	for _, r := range refs {
		ev, ok := recorded[r]
		if !ok || ev.SessionID != s.ID {
			return nil, errEvidenceUnresolved
		}
		var err error
		var files []memory.EvidenceFile
		switch ev.Type {
		case memory.EvidenceCommandResult:
			err = c.resolveCommand(s, ev, now)
		case memory.EvidenceFileObservation:
			files, err = c.resolveFile(ev, now)
		case memory.EvidenceUserDecision:
			err = c.resolveUserDecision(ev)
		default:
			// accepted_artifact has no durable record resolver yet, so it fails closed.
			err = errEvidenceUnresolved
		}
		if err != nil {
			return nil, err
		}
		// Every kind is anchored to the observing agent's HEAD so freshness can judge it. Only file observations carry
		// file hashes; the other kinds have an empty file list.
		anchors[r] = memory.EvidenceAnchor{Commit: c.worktreeHead(ev.AgentID), Files: files}
	}
	return anchors, nil
}

// worktreeHead is the commit checked out in the agent's worktree, or empty when it cannot be read. An empty commit is
// stamped as nothing, so the reference keeps whatever commit it recorded and freshness reads unknown without one.
func (c *Coord) worktreeHead(agentID string) string {
	a, ok := c.Agents.Get(agentID)
	if !ok || a.Workspace == "" {
		return ""
	}
	head, err := newWorktreeView(a.Workspace).Head()
	if err != nil {
		return ""
	}
	return head
}

// recallView is the workspace view agentID judges memory anchors in: its worktree, read through git. It is nil when the
// agent has no git workspace, so nothing is judged current there.
func (c *Coord) recallView(agentID string) freshness.View {
	a, ok := c.Agents.Get(agentID)
	if !ok || a.Workspace == "" {
		return nil
	}
	v, err := freshness.NewGitView(a.Workspace)
	if err != nil {
		return nil
	}
	return v
}

// captureAnchor records the commit and file hashes of paths in agentID's view. It returns nil when memory is off, the
// view is unavailable, or a path cannot be read: an entry without an anchor is never recalled.
func (c *Coord) captureAnchor(agentID string, paths []string) *memory.Anchor {
	if !c.settings().Memory {
		return nil
	}
	view := c.recallView(agentID)
	if view == nil {
		return nil
	}
	a, err := freshness.Capture(view, paths)
	if err != nil {
		return nil
	}
	return &memory.Anchor{Commit: a.Commit, Files: a.Files}
}

// promotionAnchor captures the candidate's anchor again at promotion, in the view of the agent that observed a file
// cited by refs. It returns nil when no file observation is cited, which keeps the anchor the candidate recorded.
func (c *Coord) promotionAnchor(cand memory.Entry, refs []string) *memory.Anchor {
	recorded := make(map[string]memory.Evidence, len(cand.Evidence))
	for _, ev := range cand.Evidence {
		recorded[ev.Ref] = ev
	}
	for _, r := range refs {
		if ev, ok := recorded[r]; ok && ev.Type == memory.EvidenceFileObservation {
			return c.captureAnchor(ev.AgentID, cand.Files)
		}
	}
	return nil
}

// checkOwner reports which agent ran the check record with this ID.
func (c *Coord) checkOwner(resultID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	owner, ok := c.checkOwners[resultID]
	return owner, ok && owner != ""
}

// checkRecord returns the session's check record with this ID, when agentID is the agent that ran it.
func (c *Coord) checkRecord(s *Session, resultID, agentID string) (verification.Result, bool) {
	if owner, ok := c.checkOwner(resultID); !ok || owner != agentID {
		return verification.Result{}, false
	}
	for _, v := range s.Verify {
		if v.ID == resultID {
			return v, true
		}
	}
	return verification.Result{}, false
}

// latestCheck returns the agent's most recent passed or failed check record in s: the only check a candidate may cite.
func (c *Coord) latestCheck(agentID string, s *Session) (verification.Result, bool) {
	for i := len(s.Verify) - 1; i >= 0; i-- {
		v := s.Verify[i]
		if v.Status != "passed" && v.Status != "failed" {
			continue
		}
		if owner, ok := c.checkOwner(v.ID); ok && owner == agentID {
			return v, true
		}
	}
	return verification.Result{}, false
}

// resolveCommand backs a command_result with the one check record it cites. The record must be the session's, run by
// the agent the evidence names, for the same command, with the outcome the evidence claims.
func (c *Coord) resolveCommand(s *Session, ev memory.Evidence, now time.Time) error {
	if ev.AgentID == "" || ev.Command == "" {
		return errEvidenceUnresolved
	}
	if _, ok := c.Agents.Get(ev.AgentID); !ok {
		return errEvidenceUnresolved
	}
	var want string
	switch ev.Result {
	case memory.ResultPassed:
		want = "passed"
	case memory.ResultFailed:
		want = "failed"
	default:
		return errEvidenceUnresolved
	}
	rec, ok := c.checkRecord(s, ev.Ref, ev.AgentID)
	if !ok || rec.Command != ev.Command || rec.Status != want {
		return errEvidenceUnresolved
	}
	if now.Sub(ev.ObservedAt) > memory.CommandEvidenceTTL {
		return errEvidenceStale
	}
	return nil
}

// resolveFile re-hashes every file the observation cites, in the worktree of the agent that observed it. A path that
// fails the memory path rules is unresolved; a missing, non-regular, symlinked, oversized or changed file is stale or
// refused.
func (c *Coord) resolveFile(ev memory.Evidence, now time.Time) ([]memory.EvidenceFile, error) {
	if len(ev.Files) == 0 || ev.AgentID == "" {
		return nil, errEvidenceUnresolved
	}
	a, ok := c.Agents.Get(ev.AgentID)
	if !ok || a.Workspace == "" {
		return nil, errEvidenceUnresolved
	}
	if now.Sub(ev.ObservedAt) > fileEvidenceTTL {
		return nil, errEvidenceStale
	}
	for _, f := range ev.Files {
		if !memory.ValidEvidencePath(f.Path) {
			return nil, errEvidenceUnresolved
		}
		if f.Absent {
			// A verified absence still holds only while the path stays missing.
			if f.SHA256 != "" {
				return nil, errEvidenceUnresolved
			}
			if _, err := hashWorktreeFile(a.Workspace, f.Path); !errors.Is(err, memory.ErrFileAbsent) {
				return nil, errEvidenceStale
			}
			continue
		}
		if f.SHA256 == "" {
			return nil, errEvidenceUnresolved
		}
		if err := checkWorktreeFile(a.Workspace, f); err != nil {
			return nil, err
		}
	}
	// The recorded files are the ones that were just checked, so they are the anchor's file list unchanged.
	return ev.Files, nil
}

// checkWorktreeFile checks one cited file under root: a regular file that stays inside root, within the size bound,
// whose bytes still hash to the recorded SHA-256. An oversized file is unresolved; every other failure is stale.
func checkWorktreeFile(root string, f memory.EvidenceFile) error {
	got, err := hashWorktreeFile(root, f.Path)
	if errors.Is(err, errTooLarge) {
		return errEvidenceUnresolved
	}
	if err != nil || got != f.SHA256 {
		return errEvidenceStale
	}
	return nil
}

// resolveUserDecision backs a user_decision with the task whose question the user answered. An answer from the
// Primary or a delegated answer does not justify a decision.
func (c *Coord) resolveUserDecision(ev memory.Evidence) error {
	t, ok := c.Graph.Get(ev.Ref)
	if !ok || t.BlockedOn.Unresolved() || t.BlockedOn.AnsweredBy != taskgraph.AnsweredByUser || strings.TrimSpace(t.BlockedOn.Answer) == "" {
		return errEvidenceUnresolved
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
