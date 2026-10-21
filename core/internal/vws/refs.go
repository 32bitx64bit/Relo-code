package vws

import (
	"errors"
	"fmt"
	"strings"
)

// Ref layout. Everything the engine needs to survive a restart or a garbage collection hangs off these names, none
// of which sit in the user's own branch namespace except the agent's branch.
const refPrefix = "refs/relo/vws/"

func baseRef(id string) string    { return refPrefix + id + "/base" }
func overlayRef(id string) string { return refPrefix + id + "/overlay" }
func execRef(id string) string    { return refPrefix + id + "/exec" }
func snapRef(id string, rev uint64) string {
	return fmt.Sprintf("%s%s/snap/%d", refPrefix, id, rev)
}
func branchRef(branch string) string { return "refs/heads/" + branch }

// validAgentID mirrors the worktree manager's rule, since the id becomes a ref name and a directory name.
func validAgentID(id string) bool {
	if id == "" || strings.Contains(id, "..") || strings.HasSuffix(id, ".") || strings.HasSuffix(strings.ToLower(id), ".lock") {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// refOp is one change in an atomic ref transaction. An empty old means "whatever is there"; the zero id means
// "must not exist". An empty new deletes the ref.
type refOp struct {
	ref string
	new string
	old string
}

// ErrRefConflict says a ref was not where the caller expected it, so no ref in the transaction moved.
var ErrRefConflict = errors.New("vws: a ref moved while the update was being made")

// updateRefs applies every op in one transaction: either all refs move or none do.
func (r *repo) updateRefs(ops ...refOp) error {
	var b strings.Builder
	for _, op := range ops {
		switch {
		case op.new == "":
			if op.old != "" {
				fmt.Fprintf(&b, "delete %s %s\n", op.ref, op.old)
			} else {
				fmt.Fprintf(&b, "delete %s\n", op.ref)
			}
		case op.old == r.zero():
			fmt.Fprintf(&b, "create %s %s\n", op.ref, op.new)
		case op.old != "":
			fmt.Fprintf(&b, "update %s %s %s\n", op.ref, op.new, op.old)
		default:
			fmt.Fprintf(&b, "update %s %s\n", op.ref, op.new)
		}
	}
	_, err := r.gitIn(r.root, nil, []byte(b.String()), "update-ref", "--stdin")
	if err != nil {
		var ge *gitError
		if errors.As(err, &ge) && (strings.Contains(ge.stderr, "reference already exists") ||
			strings.Contains(ge.stderr, "but expected") || strings.Contains(ge.stderr, "cannot lock ref")) {
			return fmt.Errorf("%w: %v", ErrRefConflict, err)
		}
		return err
	}
	return nil
}

// resolveRef returns the object a ref points at, or "" when the ref does not exist.
func (r *repo) resolveRef(ref string) (string, error) {
	out, err := r.run(r.root, nil, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		var ge *gitError
		if errors.As(err, &ge) && ge.stderr == "" {
			return "", nil // --quiet --verify: a missing ref exits 1 with nothing on stderr
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// listRefs returns ref name to object id for every ref under prefix.
func (r *repo) listRefs(prefix string) (map[string]string, error) {
	out, err := r.git("for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		oid, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			refs[name] = oid
		}
	}
	return refs, nil
}
