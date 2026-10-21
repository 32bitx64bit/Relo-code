package vws

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// hashObject computes the object id of content as Git would, without storing it.
func (r *repo) hashObject(typ string, data []byte) string {
	header := typ + " " + strconv.Itoa(len(data)) + "\x00"
	if r.algo == "sha256" {
		h := sha256.New()
		h.Write([]byte(header))
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New()
	h.Write([]byte(header))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// writeObject stores one loose object and returns its id. An object that is already loose is left alone. It is
// written beside its final name and renamed into place, so a reader never sees half an object, and it is synced
// first when the engine is durable, so a revision published after it can rely on it.
//
// Trees and commits are written here directly: they are tiny, there can be many per revision, and spawning git
// for each would cost more than the work. Blobs that come from a file go through hashBlob so Git's own content
// filters stay in charge of them.
func (r *repo) writeObject(typ string, data []byte) (string, error) {
	oid := r.hashObject(typ, data)
	dst := filepath.Join(r.objects, oid[:2], oid[2:])
	if _, err := os.Stat(dst); err == nil {
		return oid, nil
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "tmp_obj_")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	zw := zlib.NewWriter(tmp)
	if _, err := fmt.Fprintf(zw, "%s %d\x00", typ, len(data)); err != nil {
		return fail(err)
	}
	if _, err := zw.Write(data); err != nil {
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if r.fsync {
		if err := tmp.Sync(); err != nil {
			return fail(err)
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if st, serr := os.Stat(name); serr == nil {
		r.objectCount.Add(1)
		r.byteCount.Add(st.Size())
	}
	_ = os.Chmod(name, 0o444)
	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		if _, serr := os.Stat(dst); serr == nil {
			return oid, nil // another writer stored the same object first
		}
		return "", err
	}
	return oid, nil
}

// hashBlob stores file content as a blob through Git, so core.autocrlf, attributes and clean filters apply exactly
// as they would to `git add` of a file at path.
func (r *repo) hashBlob(path string, data []byte) (string, error) {
	out, err := r.gitIn(r.root, nil, data, "hash-object", "-w", "--stdin", "--path="+path)
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(out))
	if !r.isOID(oid) {
		return "", fmt.Errorf("vws: git hash-object returned %q", oid)
	}
	return oid, nil
}

// readBlob returns a blob's content, or an error naming what it was instead.
func (r *repo) readBlob(oid string) ([]byte, error) {
	typ, data, err := r.readObject(oid)
	if err != nil {
		return nil, err
	}
	if typ != "blob" {
		return nil, fmt.Errorf("vws: object %s is a %s, not a blob", oid, typ)
	}
	return data, nil
}

// commitBody renders a commit object. The author and committer are fixed so identical work hashes identically.
func commitBody(tree string, parents []string, subject, body string, ts int64) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		if p != "" {
			fmt.Fprintf(&b, "parent %s\n", p)
		}
	}
	stamp := fmt.Sprintf("Relo <relo@localhost> %d +0000", ts)
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s\n", stamp, stamp, subject)
	if body != "" {
		fmt.Fprintf(&b, "\n%s\n", body)
	}
	return b.Bytes()
}

// writeCommit stores a commit.
func (r *repo) writeCommit(tree string, parents []string, subject, body string, ts int64) (string, error) {
	return r.writeObject("commit", commitBody(tree, parents, subject, body, ts))
}

// commitInfo is the part of a commit the engine reads back.
type commitInfo struct {
	Tree    string
	Parents []string
	Message string
}

func (r *repo) readCommit(oid string) (commitInfo, error) {
	typ, data, err := r.readObject(oid)
	if err != nil {
		return commitInfo{}, err
	}
	if typ != "commit" {
		return commitInfo{}, fmt.Errorf("vws: object %s is a %s, not a commit", oid, typ)
	}
	head, msg, _ := strings.Cut(string(data), "\n\n")
	var ci commitInfo
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			ci.Tree = strings.TrimPrefix(line, "tree ")
		case strings.HasPrefix(line, "parent "):
			ci.Parents = append(ci.Parents, strings.TrimPrefix(line, "parent "))
		}
	}
	ci.Message = msg
	if !r.isOID(ci.Tree) {
		return commitInfo{}, fmt.Errorf("vws: commit %s has no tree", oid)
	}
	return ci, nil
}
