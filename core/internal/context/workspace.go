package ctxeng

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Cognitive state (what an agent knows), repository state (what its files are) and execution state (what its tools
// have done) are three different things, linked only by stable revision identifiers. These types are the link from the
// first to the second: a generation names the repository state it was built against, and a file-derived item names
// the bytes it was cut from. The engine never touches Git; callers supply the identifiers and a way to look files up.

// WorkspaceContextRef names the repository state a generation was built against.
type WorkspaceContextRef struct {
	AgentID    string `json:"agentId"`
	BaseCommit string `json:"baseCommit"`
	// OverlayRevision counts the published states of the agent's workspace; zero for a worktree, which has none.
	OverlayRevision uint64 `json:"overlayRevision"`
	// EffectiveTreeOID is the Git tree of every file the agent sees at that revision.
	EffectiveTreeOID string `json:"effectiveTreeOid"`
}

// Derivation records what a file-derived item was cut from: whose workspace, at which revision, and the Git blob id
// of each file at that point. A later look at the same files says whether the item still describes them.
type Derivation struct {
	AgentID  string `json:"agentId"`
	Revision uint64 `json:"revision"`
	// Hashes maps each path to the Git blob id of its content; empty means the file did not exist.
	Hashes map[string]string `json:"hashes"`
	// Imported is set only by IngestImported: the item came from another agent's workspace because someone chose to
	// bring it, never because it was reachable.
	Imported bool `json:"imported,omitempty"`
}

// BlobHash is the Git blob id of data, so a hash taken here equals the object id Git gives the same bytes.
func BlobHash(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// FileLookup returns the blob id of a path in the workspace being checked, and whether the file exists.
type FileLookup func(path string) (blob string, exists bool, err error)

// StaleItem is a file-derived item whose files no longer match what it was cut from.
type StaleItem struct {
	ID    string   `json:"id"`
	Title string   `json:"title,omitempty"`
	Paths []string `json:"paths"`
}

// Revalidation is what checking an agent's file-derived items against its workspace found.
type Revalidation struct {
	Checked   int         `json:"checked"`
	Stale     []StaleItem `json:"stale,omitempty"`
	Unchecked int         `json:"unchecked,omitempty"` // items whose files could not be read; left as they were
}

// BindWorkspace sets the repository state the agent's next generations are built against. A context refresh calls it
// with the current revision, so a refreshed generation describes the files the agent has now and never an older copy.
func (e *Engine) BindWorkspace(agentID string, ref WorkspaceContextRef) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := ref
	e.ensure(agentID).Workspace = &cp
}

// WorkspaceRef returns the repository state the agent's context is bound to.
func (e *Engine) WorkspaceRef(agentID string) (WorkspaceContextRef, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	if st.Workspace == nil {
		return WorkspaceContextRef{}, false
	}
	return *st.Workspace, true
}

// foreign reports whether an item was cut from another agent's workspace without anyone choosing to bring it. Such
// an item would make the agent reason about code its own files do not contain.
func foreign(it Item, agentID string) bool {
	return it.From != nil && it.From.AgentID != "" && it.From.AgentID != agentID && !it.From.Imported
}

// IngestImported adds items that came from another agent's workspace because they were chosen: an explicit import,
// a ChangeReference the user or Primary followed. They are marked as imported, and so are never mistaken for code the
// agent can reach on its own. It returns the number added.
func (e *Engine) IngestImported(agentID string, items []Item) int {
	marked := make([]Item, 0, len(items))
	for _, it := range items {
		if it.From != nil {
			d := *it.From
			d.Imported = true
			it.From = &d
		}
		marked = append(marked, it)
	}
	e.Ingest(agentID, marked)
	return len(marked)
}

// Revalidate compares every file-derived item of the agent with the files as they are now. An item whose files
// differ from the bytes it was cut from is marked stale; one whose files match again is marked fresh. The generation
// already built is left alone, so the prompt prefix stays byte-identical until the next boundary build, which drops
// stale code and flags anything else that cites a changed file.
func (e *Engine) Revalidate(agentID string, lookup FileLookup) Revalidation {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	var res Revalidation
	for i := range st.Pool {
		it := &st.Pool[i]
		if it.From == nil || len(it.From.Hashes) == 0 {
			continue
		}
		res.Checked++
		var changed []string
		unknown := false
		for path, want := range it.From.Hashes {
			blob, exists, err := lookup(path)
			if err != nil {
				unknown = true
				break
			}
			if !exists {
				blob = ""
			}
			if blob != want {
				changed = append(changed, path)
			}
		}
		if unknown {
			res.Unchecked++
			continue
		}
		if len(changed) == 0 {
			it.Stale = ""
			continue
		}
		sort.Strings(changed)
		it.Stale = staleReason(changed, it.From.Revision)
		res.Stale = append(res.Stale, StaleItem{ID: it.ID, Title: it.Title, Paths: changed})
	}
	sort.Slice(res.Stale, func(i, j int) bool { return res.Stale[i].ID < res.Stale[j].ID })
	return res
}

func staleReason(paths []string, rev uint64) string {
	return strings.Join(paths, ", ") + " changed after revision " + strconv.FormatUint(rev, 10)
}

// staleIDs lists the pool items currently marked stale, with the reason.
func staleIDs(pool []Item) map[string]string {
	var out map[string]string
	for _, it := range pool {
		if it.Stale == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[it.ID] = it.Stale
	}
	return out
}

// applyFreshness is the boundary build's answer to stale items. Code that no longer matches the files is dropped, as
// it would only mislead. Anything else that cites a changed file stays, flagged, because what it concluded may still
// hold and the agent decides.
func applyFreshness(items []Item, stale map[string]string) ([]Item, []Dropped) {
	out := make([]Item, 0, len(items))
	var dropped []Dropped
	for _, it := range items {
		reason, isStale := stale[it.ID]
		switch {
		case !isStale:
			it.Stale = "" // an earlier generation's copy may carry a flag the files have since cleared
			out = append(out, it)
		case it.Class == ClassCode:
			it.Stale = reason
			dropped = append(dropped, Dropped{Item: it, Reason: "stale_file"})
		default:
			it.Stale = reason
			out = append(out, it)
		}
	}
	return out, dropped
}

// staleInGeneration reports whether the newest generation still shows an item the pool now knows to be stale.
func staleInGeneration(st *state) bool {
	if len(st.Generations) == 0 {
		return false
	}
	stale := staleIDs(st.Pool)
	if len(stale) == 0 {
		return false
	}
	for _, it := range st.Generations[len(st.Generations)-1].Items {
		if _, ok := stale[it.ID]; ok {
			return true
		}
	}
	return false
}
