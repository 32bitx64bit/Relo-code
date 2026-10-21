package vws

import (
	"bytes"
	"container/list"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Git tree entry modes.
const (
	modeTree    uint32 = 0o040000
	modeFile    uint32 = 0o100644
	modeExec    uint32 = 0o100755
	modeSymlink uint32 = 0o120000
	modeGitlink uint32 = 0o160000
)

// treeEntry is one name in a tree object.
type treeEntry struct {
	Name string
	Mode uint32
	OID  string
}

func (e treeEntry) isTree() bool { return e.Mode == modeTree }

// isFile reports a regular file (either permission).
func (e treeEntry) isFile() bool { return e.Mode == modeFile || e.Mode == modeExec }

// normalMode folds the many historical file modes Git accepts into the four it writes.
func normalMode(mode uint32) uint32 {
	switch mode & 0o170000 {
	case 0o040000:
		return modeTree
	case 0o120000:
		return modeSymlink
	case 0o160000:
		return modeGitlink
	}
	if mode&0o111 != 0 {
		return modeExec
	}
	return modeFile
}

// tree is a parsed tree object. It is shared between readers through the cache and never modified.
type tree struct {
	entries []treeEntry
	index   map[string]int
}

func newTree(entries []treeEntry) *tree {
	t := &tree{entries: entries}
	if len(entries) > 16 {
		t.index = make(map[string]int, len(entries))
		for i, e := range entries {
			t.index[e.Name] = i
		}
	}
	return t
}

func (t *tree) find(name string) (treeEntry, int, bool) {
	if t == nil {
		return treeEntry{}, -1, false
	}
	if t.index != nil {
		i, ok := t.index[name]
		if !ok {
			return treeEntry{}, -1, false
		}
		return t.entries[i], i, true
	}
	for i, e := range t.entries {
		if e.Name == name {
			return e, i, true
		}
	}
	return treeEntry{}, -1, false
}

// parseTree reads a tree object's body. rawLen is the width of an object id in the binary form.
func parseTree(data []byte, rawLen int) ([]treeEntry, error) {
	var entries []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("vws: malformed tree entry")
		}
		mode, err := strconv.ParseUint(string(data[:sp]), 8, 32)
		if err != nil {
			return nil, fmt.Errorf("vws: malformed tree mode: %w", err)
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+1+rawLen {
			return nil, fmt.Errorf("vws: truncated tree entry")
		}
		entries = append(entries, treeEntry{Name: string(data[:nul]), Mode: uint32(mode), OID: hex.EncodeToString(data[nul+1 : nul+1+rawLen])})
		data = data[nul+1+rawLen:]
	}
	return entries, nil
}

// sortKey orders entries the way Git does: by name, with a directory compared as if its name ended in a slash.
func sortKey(e treeEntry) string {
	if e.isTree() {
		return e.Name + "/"
	}
	return e.Name
}

func sortEntries(entries []treeEntry) {
	sort.Slice(entries, func(i, j int) bool { return sortKey(entries[i]) < sortKey(entries[j]) })
}

func serializeTree(entries []treeEntry) ([]byte, error) {
	var b bytes.Buffer
	for _, e := range entries {
		raw, err := hex.DecodeString(e.OID)
		if err != nil {
			return nil, fmt.Errorf("vws: bad object id %q: %w", e.OID, err)
		}
		b.WriteString(strconv.FormatUint(uint64(e.Mode), 8))
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		b.Write(raw)
	}
	return b.Bytes(), nil
}

// treeCache holds parsed trees by object id. Trees are immutable, so one cache is safe to share between every
// workspace on the repository: all agents read the same base trees and parse each of them once.
type treeCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

type cacheItem struct {
	oid string
	t   *tree
}

func newTreeCache(max int) *treeCache {
	return &treeCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *treeCache) get(oid string) (*tree, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[oid]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheItem).t, true
}

func (c *treeCache) put(oid string, t *tree) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[oid]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.items[oid] = c.order.PushFront(&cacheItem{oid: oid, t: t})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*cacheItem).oid)
	}
}

// loadTree reads and parses a tree. An empty id is the empty tree.
func (r *repo) loadTree(oid string) (*tree, error) {
	if oid == "" {
		return newTree(nil), nil
	}
	if t, ok := r.trees.get(oid); ok {
		return t, nil
	}
	typ, data, err := r.readObject(oid)
	if err != nil {
		return nil, err
	}
	if typ != "tree" {
		return nil, fmt.Errorf("vws: object %s is a %s, not a tree", oid, typ)
	}
	entries, err := parseTree(data, r.rawLen)
	if err != nil {
		return nil, fmt.Errorf("vws: tree %s: %w", oid, err)
	}
	t := newTree(entries)
	r.trees.put(oid, t)
	return t, nil
}

// writeTree stores a tree from entries in any order.
func (r *repo) writeTree(entries []treeEntry) (string, error) {
	sorted := append([]treeEntry(nil), entries...)
	sortEntries(sorted)
	body, err := serializeTree(sorted)
	if err != nil {
		return "", err
	}
	oid, err := r.writeObject("tree", body)
	if err != nil {
		return "", err
	}
	r.trees.put(oid, newTree(sorted))
	return oid, nil
}

func (r *repo) emptyTree() (string, error) { return r.writeTree(nil) }

// lookup finds the entry at parts below root. found is false when any component is absent. A component that is a
// file is ErrNotDir and one that is a link is ErrSymlink: a path is never resolved through either.
func (r *repo) lookup(root string, parts []string) (treeEntry, bool, error) {
	cur := root
	for i, name := range parts {
		t, err := r.loadTree(cur)
		if err != nil {
			return treeEntry{}, false, err
		}
		e, _, ok := t.find(name)
		if !ok {
			return treeEntry{}, false, nil
		}
		if i == len(parts)-1 {
			return e, true, nil
		}
		switch {
		case e.isTree():
			cur = e.OID
		case e.Mode == modeSymlink:
			return treeEntry{}, false, pathErr(join(parts[:i+1]), ErrSymlink)
		default:
			return treeEntry{}, false, pathErr(join(parts[:i+1]), ErrNotDir)
		}
	}
	return treeEntry{Mode: modeTree, OID: cur}, true, nil
}

// change is one edit to a tree. A nil leaf deletes the path, and a delete of a directory removes everything below
// it. A leaf replaces whatever was there.
type change struct {
	parts []string
	leaf  *treeEntry
}

// applyChanges rewrites root with changes and returns the new root. Only trees along the changed paths are
// written; every other subtree is shared with the old one. The changes describe a final state, so a file replaced
// by a directory (or the reverse) is expressed by the delete and the add together.
func (r *repo) applyChanges(root string, changes []change) (string, error) {
	oid, _, err := r.rewrite(root, changes)
	if err != nil {
		return "", err
	}
	if oid == "" {
		return r.emptyTree()
	}
	return oid, nil
}

// rewrite returns the new id of a tree, or "" with empty set when nothing is left in it.
func (r *repo) rewrite(treeOID string, changes []change) (oid string, empty bool, err error) {
	t, err := r.loadTree(treeOID)
	if err != nil {
		return "", false, err
	}
	type group struct {
		self   *change
		deeper []change
	}
	groups := map[string]*group{}
	var names []string
	for _, c := range changes {
		if len(c.parts) == 0 {
			return "", false, fmt.Errorf("vws: change without a path")
		}
		name := c.parts[0]
		g := groups[name]
		if g == nil {
			g = &group{}
			groups[name] = g
			names = append(names, name)
		}
		if len(c.parts) == 1 {
			cc := c
			g.self = &cc
		} else {
			g.deeper = append(g.deeper, change{parts: c.parts[1:], leaf: c.leaf})
		}
	}
	entries := append([]treeEntry(nil), t.entries...)
	remove := map[string]bool{}
	set := map[string]treeEntry{}
	for _, name := range names {
		g := groups[name]
		existing, _, exists := t.find(name)
		switch {
		case g.self != nil && g.self.leaf != nil: // the path becomes this entry
			if len(g.deeper) > 0 {
				// Anything below it can only be a delete, from a directory this entry replaces.
				for _, d := range g.deeper {
					if d.leaf != nil {
						return "", false, pathErr(name, ErrIsDir)
					}
				}
			}
			e := *g.self.leaf
			e.Name = name
			set[name] = e
		case g.self != nil: // the path is deleted, and may come back as a directory
			delete(set, name)
			if len(g.deeper) == 0 {
				if exists {
					remove[name] = true
				}
				continue
			}
			base := ""
			if exists && existing.isTree() {
				base = existing.OID
			}
			child, childEmpty, err := r.rewrite(base, g.deeper)
			if err != nil {
				return "", false, err
			}
			if childEmpty {
				if exists {
					remove[name] = true
				}
				continue
			}
			set[name] = treeEntry{Name: name, Mode: modeTree, OID: child}
		default: // only changes below it
			base := ""
			if exists {
				switch {
				case existing.isTree():
					base = existing.OID
				case existing.Mode == modeSymlink:
					return "", false, pathErr(name, ErrSymlink)
				default:
					return "", false, pathErr(name, ErrNotDir)
				}
			}
			child, childEmpty, err := r.rewrite(base, g.deeper)
			if err != nil {
				return "", false, err
			}
			if childEmpty {
				if exists {
					remove[name] = true
				}
				continue
			}
			set[name] = treeEntry{Name: name, Mode: modeTree, OID: child}
		}
	}
	out := entries[:0:0]
	for _, e := range entries {
		if remove[e.Name] {
			continue
		}
		if repl, ok := set[e.Name]; ok {
			out = append(out, repl)
			delete(set, e.Name)
			continue
		}
		out = append(out, e)
	}
	for _, e := range set {
		out = append(out, e)
	}
	if len(out) == 0 {
		return "", true, nil
	}
	oid, err = r.writeTree(out)
	return oid, false, err
}

// walk calls fn for every entry below root in path order, descending into directories. fn returning false stops
// the walk of that subtree.
func (r *repo) walk(root, prefix string, fn func(path string, e treeEntry) (descend bool, err error)) error {
	t, err := r.loadTree(root)
	if err != nil {
		return err
	}
	entries := append([]treeEntry(nil), t.entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for _, e := range entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		descend, err := fn(p, e)
		if err != nil {
			return err
		}
		if descend && e.isTree() {
			if err := r.walk(e.OID, p, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func kindOf(mode uint32) string {
	switch mode {
	case modeTree:
		return "dir"
	case modeSymlink:
		return "symlink"
	case modeGitlink:
		return "submodule"
	}
	return "file"
}

func describeMode(mode uint32) string {
	return strings.TrimLeft(strconv.FormatUint(uint64(mode), 8), "0")
}
