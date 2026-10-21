package vws

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// repo is the Git repository the engine keeps objects and refs in. It never reads or writes the user's index,
// HEAD, branches or checkout.
type repo struct {
	root      string // directory git commands run in
	commonDir string // shared git directory: objects and refs live here even when root is a linked worktree
	objects   string // objects directory
	algo      string // sha1 or sha256
	hexLen    int    // length of an object id in hex
	rawLen    int    // length of an object id in a tree entry
	fsync     bool   // fsync objects before a ref publishes them

	check *catFile // cat-file --batch-check: type and size
	read  *catFile // cat-file --batch: content

	trees *treeCache

	spawnCount  atomic.Int64 // git children started for commands (not the long-lived cat-file helpers)
	objectCount atomic.Int64 // loose objects written
	byteCount   atomic.Int64 // bytes of loose objects written
}

func openRepo(root string, fsync bool) (*repo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	r := &repo{root: abs, fsync: fsync, trees: newTreeCache(4096)}
	common, err := r.git("rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("vws: %s is not a git repository: %w", root, err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(abs, common)
	}
	r.commonDir = filepath.Clean(common)
	r.objects = filepath.Join(r.commonDir, "objects")
	if dir, derr := r.git("rev-parse", "--git-path", "objects"); derr == nil && dir != "" {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(abs, dir)
		}
		r.objects = filepath.Clean(dir)
	}
	r.algo = "sha1"
	if out, ferr := r.git("rev-parse", "--show-object-format"); ferr == nil && strings.TrimSpace(out) == "sha256" {
		r.algo = "sha256"
	}
	if r.algo == "sha256" {
		r.hexLen, r.rawLen = 64, 32
	} else {
		r.hexLen, r.rawLen = 40, 20
	}
	r.check = &catFile{repo: r, mode: "--batch-check"}
	r.read = &catFile{repo: r, mode: "--batch"}
	return r, nil
}

func (r *repo) close() {
	r.check.stop()
	r.read.stop()
}

func (r *repo) zero() string { return strings.Repeat("0", r.hexLen) }

func (r *repo) isOID(s string) bool {
	if len(s) != r.hexLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// gitEnv is the environment of every git child: no pager or prompt, and no optional lock on the user's index.
func gitEnv(extra ...string) []string {
	env := append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat",
		"GIT_AUTHOR_NAME=Relo", "GIT_AUTHOR_EMAIL=relo@localhost", "GIT_COMMITTER_NAME=Relo", "GIT_COMMITTER_EMAIL=relo@localhost")
	return append(env, extra...)
}

// GitProbe, when non-nil, is called before every git command the engine starts. Tests and benchmarks use it to see
// which commands a workflow costs.
var GitProbe func(args []string)

func (r *repo) run(dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	r.spawnCount.Add(1)
	if GitProbe != nil {
		GitProbe(args)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &gitError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}
func (e *gitError) Unwrap() error { return e.err }

// git runs a command in the repository root and returns its trimmed standard output.
func (r *repo) git(args ...string) (string, error) {
	out, err := r.run(r.root, nil, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// gitRaw is git with the output untrimmed, for NUL-separated records.
func (r *repo) gitRaw(args ...string) ([]byte, error) { return r.run(r.root, nil, nil, args...) }

func (r *repo) gitIn(dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	if stdin == nil {
		stdin = []byte{}
	}
	return r.run(dir, env, stdin, args...)
}

// catFile is one long-lived `git cat-file` child. Starting git per object costs more than the lookup, so a read of
// a deep path or a listing reuses a single process. Requests are serialised; the child is restarted if it dies.
type catFile struct {
	repo *repo
	mode string

	mu  sync.Mutex
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func (c *catFile) start() error {
	cmd := exec.Command("git", "cat-file", c.mode)
	cmd.Dir = c.repo.root
	cmd.Env = gitEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd, c.in, c.out = cmd, in, bufio.NewReaderSize(out, 64*1024)
	return nil
}

func (c *catFile) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

func (c *catFile) stopLocked() {
	if c.cmd == nil {
		return
	}
	_ = c.in.Close()
	_ = c.cmd.Wait()
	c.cmd, c.in, c.out = nil, nil, nil
}

// objectInfo is what a batch-check answers.
type objectInfo struct {
	typ  string
	size int64
}

var errMissing = errors.New("object missing")

// request sends one object name and reads the header line; wantBody reads the content that follows it.
func (c *catFile) request(oid string, wantBody bool) (objectInfo, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if c.cmd == nil {
			if err := c.start(); err != nil {
				return objectInfo{}, nil, err
			}
		}
		info, body, err := c.exchange(oid, wantBody)
		if err == nil || errors.Is(err, errMissing) {
			return info, body, err
		}
		lastErr = err
		c.stopLocked() // the child died or the stream is out of step: start a fresh one
	}
	return objectInfo{}, nil, lastErr
}

func (c *catFile) exchange(oid string, wantBody bool) (objectInfo, []byte, error) {
	if _, err := io.WriteString(c.in, oid+"\n"); err != nil {
		return objectInfo{}, nil, err
	}
	line, err := c.out.ReadString('\n')
	if err != nil {
		return objectInfo{}, nil, err
	}
	fields := strings.Fields(line)
	if len(fields) == 2 && fields[1] == "missing" {
		return objectInfo{}, nil, fmt.Errorf("%w: %s", errMissing, oid)
	}
	if len(fields) != 3 {
		return objectInfo{}, nil, fmt.Errorf("vws: unexpected cat-file reply %q", strings.TrimSpace(line))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return objectInfo{}, nil, err
	}
	info := objectInfo{typ: fields[1], size: size}
	if !wantBody {
		return info, nil, nil
	}
	body := make([]byte, size+1) // the content, then a newline git adds after it
	if _, err := io.ReadFull(c.out, body); err != nil {
		return objectInfo{}, nil, err
	}
	return info, body[:size], nil
}

// stat returns an object's type and size without reading it.
func (r *repo) stat(oid string) (objectInfo, error) {
	info, _, err := r.check.request(oid, false)
	return info, err
}

// readObject returns an object's type and content.
func (r *repo) readObject(oid string) (string, []byte, error) {
	info, body, err := r.read.request(oid, true)
	return info.typ, body, err
}

// hasObject reports whether the object database holds oid.
func (r *repo) hasObject(oid string) bool {
	_, err := r.stat(oid)
	return err == nil
}
