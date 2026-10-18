package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/relocode/relo/internal/integration"
)

// Journal retention: an entry is kept while it is younger than 14 days and among the newest 200.
const (
	journalRetention = 14 * 24 * time.Hour
	journalMax       = 200
	changeMax        = 200
	journalPrefix    = "relo-journal\n"
	journalRefPrefix = "refs/relo/journal/"
	changeRefPrefix  = "refs/relo/changes/"
)

// JournalWrite is one path touched by a write. Post is the exact content Relo is about to write; PostExists is
// false for a delete. The pre-image and its mode are read here, before the write.
type JournalWrite struct {
	Path       string `json:"path"`
	Post       []byte `json:"-"`
	PostExists bool   `json:"postExists"`
}

// JournalFile is one path in a journal entry: the blob before and the blob Relo wrote. "-" means the path did
// not exist on that side.
type JournalFile struct {
	Path string `json:"path"`
	Pre  string `json:"pre"`
	Post string `json:"post"`
	Mode uint32 `json:"mode,omitempty"`
}

type journalRecord struct {
	ChangeIDs []string      `json:"changeIds"`
	Files     []JournalFile `json:"files"`
	At        int64         `json:"at"`
}

// UndoResult is what an undo restored.
type UndoResult struct {
	Entry    string   `json:"entry"`
	Restored []string `json:"restored"`
}

// JournalChange stores the pre-image of every touched path in a commit under refs/relo/journal/<unixnano> before
// the caller writes. It returns the entry commit. Nothing is written to the workspace.
func (m *Manager) JournalChange(changeID string, writes []JournalWrite) (string, error) {
	if !isGit(m.root) {
		return "", fmt.Errorf("not a git repository")
	}
	m.journalMu.Lock()
	defer m.journalMu.Unlock()
	rec := journalRecord{At: time.Now().UnixNano()}
	if changeID != "" {
		rec.ChangeIDs = []string{changeID}
	}
	dir, err := os.MkdirTemp("", "relo-journal-*")
	if err != nil {
		return "", err
	}
	indexPath := filepath.Join(dir, "index")
	defer os.RemoveAll(dir)
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath,
		"GIT_AUTHOR_NAME=Relo", "GIT_AUTHOR_EMAIL=relo@localhost",
		"GIT_COMMITTER_NAME=Relo", "GIT_COMMITTER_EMAIL=relo@localhost")
	entries := 0
	for _, w := range writes {
		if err := validateRelative(w.Path); err != nil {
			return "", err
		}
		full := filepath.Join(m.root, filepath.FromSlash(w.Path))
		pre, preErr := os.ReadFile(full)
		preExists := preErr == nil
		if preErr != nil && !os.IsNotExist(preErr) {
			return "", preErr
		}
		file := JournalFile{Path: w.Path, Pre: "-", Post: integration.BlobID(w.Post, w.PostExists)}
		if preExists {
			file.Pre = integration.BlobID(pre, true)
			if st, serr := os.Stat(full); serr == nil {
				file.Mode = uint32(st.Mode().Perm())
			}
			sha, herr := gitStdin(m.root, string(pre), "hash-object", "-w", "--stdin")
			if herr != nil {
				return "", herr
			}
			if _, uerr := gitEnv(m.root, env, "update-index", "--add", "--cacheinfo", "100644", sha, w.Path); uerr != nil {
				return "", uerr
			}
			entries++
		}
		rec.Files = append(rec.Files, file)
	}
	tree := "4b825dc642cb6eb9a060e54bf8d69288fbee4904" // the empty tree
	if entries > 0 {
		tree, err = gitEnv(m.root, env, "write-tree")
		if err != nil {
			return "", err
		}
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	commit, err := gitEnv(m.root, env, "commit-tree", tree, "-m", journalPrefix+string(payload))
	if err != nil {
		return "", err
	}
	ref := fmt.Sprintf("%s%d", journalRefPrefix, rec.At)
	if _, err := git(m.root, "update-ref", ref, commit); err != nil {
		return "", err
	}
	return commit, nil
}

// JournalEntryFor returns the newest entry that mentions changeID.
func (m *Manager) JournalEntryFor(changeID string) (journalRecord, string, error) {
	refs, err := m.journalRefs()
	if err != nil {
		return journalRecord{}, "", err
	}
	for i := len(refs) - 1; i >= 0; i-- {
		rec, _, err := m.journalRecordAt(refs[i])
		if err != nil {
			continue
		}
		for _, id := range rec.ChangeIDs {
			if id == changeID {
				return rec, refs[i], nil
			}
		}
	}
	return journalRecord{}, "", fmt.Errorf("no journal entry for change %s", changeID)
}

func (m *Manager) journalRecordAt(ref string) (journalRecord, string, error) {
	commit, err := git(m.root, "rev-parse", ref)
	if err != nil {
		return journalRecord{}, "", err
	}
	msg, err := gitRaw(m.root, "log", "-1", "--format=%B", commit)
	if err != nil {
		return journalRecord{}, "", err
	}
	idx := strings.Index(msg, journalPrefix)
	if idx < 0 {
		return journalRecord{}, commit, fmt.Errorf("journal entry %s has no payload", ref)
	}
	var rec journalRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(msg[idx+len(journalPrefix):])), &rec); err != nil {
		return journalRecord{}, commit, err
	}
	return rec, commit, nil
}

// UndoChange restores the pre-image recorded for changeID. It restores every path only when each path still holds
// the post-image Relo wrote; otherwise it refuses and changes nothing.
func (m *Manager) UndoChange(changeID string) (UndoResult, error) {
	if !isGit(m.root) {
		return UndoResult{}, fmt.Errorf("not a git repository")
	}
	m.journalMu.Lock()
	defer m.journalMu.Unlock()
	rec, ref, err := m.JournalEntryFor(changeID)
	if err != nil {
		return UndoResult{}, err
	}
	type restore struct {
		file JournalFile
		data []byte
	}
	var restores []restore
	for _, f := range rec.Files {
		current, exists, err := readWorkspaceFile(m.root, f.Path)
		if err != nil {
			return UndoResult{}, err
		}
		if integration.BlobID(current, exists) != f.Post {
			return UndoResult{}, fmt.Errorf("%s changed after Relo wrote it, so the undo was refused and nothing was restored", f.Path)
		}
		r := restore{file: f}
		if f.Pre != "-" {
			out, err := gitRaw(m.root, "cat-file", "-p", f.Pre)
			if err != nil {
				return UndoResult{}, err
			}
			r.data = []byte(out)
		}
		restores = append(restores, r)
	}
	res := UndoResult{Entry: ref}
	for _, r := range restores {
		full := filepath.Join(m.root, filepath.FromSlash(r.file.Path))
		if r.file.Pre == "-" {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return res, err
			}
		} else if err := writeRestored(full, r.data, os.FileMode(r.file.Mode)); err != nil {
			return res, err
		}
		res.Restored = append(res.Restored, r.file.Path)
	}
	return res, nil
}

// KeepChangeBlob keeps a rejected or superseded agent file as refs/relo/changes/<id> so its bytes remain
// recoverable after the worktree is released.
func (m *Manager) KeepChangeBlob(changeID, worktree, rel string) error {
	if !isGit(m.root) || changeID == "" || rel == "" {
		return nil
	}
	m.journalMu.Lock()
	defer m.journalMu.Unlock()
	if err := validateRelative(rel); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	sha, err := gitStdin(m.root, string(data), "hash-object", "-w", "--stdin")
	if err != nil {
		return err
	}
	name := changeRefPrefix + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + changeRefID(changeID)
	_, err = git(m.root, "update-ref", name, sha)
	return err
}

func changeRefID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func readWorkspaceFile(root, rel string) ([]byte, bool, error) {
	if err := validateRelative(rel); err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err == nil {
		return data, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, err
}

func writeRestored(dst string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".relo-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func (m *Manager) journalRefs() ([]string, error) {
	out, err := gitRaw(m.root, "for-each-ref", "--format=%(refname)", journalRefPrefix)
	if err != nil {
		return nil, err
	}
	refs := strings.Fields(out)
	sort.Slice(refs, func(i, j int) bool {
		return refSuffix(refs[i]) < refSuffix(refs[j])
	})
	return refs, nil
}

func refSuffix(ref string) string { return ref[strings.LastIndex(ref, "/")+1:] }

// pruneJournal keeps the newest 200 entries that are younger than 14 days. Older refs are deleted; their commits
// become unreachable and Git may collect them.
func (m *Manager) pruneJournal() {
	m.journalMu.Lock()
	defer m.journalMu.Unlock()
	refs, err := m.journalRefs()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-journalRetention).UnixNano()
	var keep []string
	for _, ref := range refs {
		ts, err := strconv.ParseInt(refSuffix(ref), 10, 64)
		if err != nil || ts < cutoff {
			_, _ = git(m.root, "update-ref", "-d", ref)
			continue
		}
		keep = append(keep, ref)
	}
	if len(keep) > journalMax {
		for _, ref := range keep[:len(keep)-journalMax] {
			_, _ = git(m.root, "update-ref", "-d", ref)
		}
	}
}

// pruneChangeRefs deletes refs/relo/changes entries older than journalRetention, then keeps the newest changeMax.
// The write time is the nanosecond prefix of the ref name; refs without one are kept by the count cap only.
func (m *Manager) pruneChangeRefs() {
	m.journalMu.Lock()
	defer m.journalMu.Unlock()
	out, err := gitRaw(m.root, "for-each-ref", "--format=%(refname)", changeRefPrefix)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-journalRetention).UnixNano()
	var refs []string
	for _, ref := range strings.Fields(out) {
		if ts, ok := changeRefStamp(ref); ok && ts < cutoff {
			_, _ = git(m.root, "update-ref", "-d", ref)
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) <= changeMax {
		return
	}
	sort.Strings(refs)
	for _, ref := range refs[:len(refs)-changeMax] {
		_, _ = git(m.root, "update-ref", "-d", ref)
	}
}

func changeRefStamp(ref string) (int64, bool) {
	name := strings.TrimPrefix(ref, changeRefPrefix)
	i := strings.IndexByte(name, '-')
	if i <= 0 {
		return 0, false
	}
	ts, err := strconv.ParseInt(name[:i], 10, 64)
	return ts, err == nil
}
