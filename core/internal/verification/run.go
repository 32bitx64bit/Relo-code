package verification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/id"
)

const outputLimit = 64 * 1024

// Result statuses: passed, failed, or skipped. A command that could not run for reasons that have nothing to do
// with the code under test (missing tool, missing dependencies) is skipped, never failed, so the environment cannot
// make an agent's work look broken.
type Result struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Status  string `json:"status"`
	Output  string `json:"output"`
	Dir     string `json:"dir"`
	Code    int    `json:"exitCode,omitempty"`
	Note    string `json:"note,omitempty"`
}

// CheckLimit is how long one project check may run.
var CheckLimit = 5 * time.Minute

type Runner struct {
	Root string
}

func Detect(root string) []string {
	var cmds []string
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Scripts["test"] != "" {
			cmds = append(cmds, "npm test --silent")
		}
	}
	if exists(root, "go.mod") {
		cmds = append(cmds, "go test ./...")
	}
	if exists(root, "Makefile") || exists(root, "makefile") || exists(root, "GNUmakefile") {
		if makeHasTest(root) {
			cmds = append(cmds, "make test")
		}
	}
	if exists(root, "pyproject.toml") || exists(root, "pytest.ini") || exists(root, "setup.cfg") {
		cmds = append(cmds, "pytest -q")
	}
	if exists(root, "Cargo.toml") {
		cmds = append(cmds, "cargo test")
	}
	return unique(cmds)
}

// Step is one planned verification command. Skip, when set, says why it cannot run here.
type Step struct {
	Command string `json:"command"`
	Skip    string `json:"skip,omitempty"`
}

var toolFor = map[string]string{"npm test --silent": "npm", "go test ./...": "go", "make test": "make", "pytest -q": "pytest", "cargo test": "cargo"}

// Plan turns Detect into runnable steps. A Makefile test target wins over the individual stacks because it is the
// project's own definition of "the tests". Steps whose tool or installed dependencies are missing carry a Skip reason.
func Plan(root string) []Step {
	cmds := Detect(root)
	for _, c := range cmds {
		if c == "make test" {
			cmds = []string{c}
			break
		}
	}
	steps := make([]Step, 0, len(cmds))
	for _, c := range cmds {
		st := Step{Command: c}
		if tool := toolFor[c]; tool != "" {
			if _, err := exec.LookPath(tool); err != nil {
				st.Skip = tool + " is not installed"
			}
		}
		if st.Skip == "" && c == "npm test --silent" && needsNodeModules(root) {
			st.Skip = "dependencies are not installed (no node_modules)"
		}
		steps = append(steps, st)
	}
	return steps
}

func needsNodeModules(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(data, &pkg) != nil || len(pkg.Dependencies)+len(pkg.DevDependencies) == 0 {
		return false
	}
	_, err = os.Stat(filepath.Join(root, "node_modules"))
	return err != nil
}

// envFailure matches the shell's own "tool is missing" messages. They mean the machine cannot run the check, which
// is not evidence about the change under test.
var envFailure = regexp.MustCompile(`(?m)(sh|bash|zsh|dash|make)(: line \d+)?: (\d+: )?[^\n]*: (command )?not found\s*$|executable file not found in \$PATH|npm error (missing script|code ENOENT)`)

// IsEnvFailure reports whether a failed command died because the environment lacks something it needs.
func IsEnvFailure(code int, output string) bool {
	return code == 126 || code == 127 || envFailure.MatchString(output)
}

func (r Runner) Run(ctx context.Context, command, dir string) Result {
	res := Result{ID: id.New("ver"), Command: command, Dir: dir, Status: "running"}
	if dir == "" {
		dir = r.Root
		res.Dir = dir
	}
	if command == "" || dir == "" {
		res.Status = "skipped"
		return res
	}
	parts, err := split(command)
	if err != nil || len(parts) == 0 {
		res.Status = "failed"
		if err != nil {
			res.Output = err.Error()
		}
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, CheckLimit)
	defer cancel()
	cmd := exec.CommandContext(cctx, parts[0], parts[1:]...)
	cmd.Dir = dir
	configureProcessGroup(cmd)
	buf := &limitedBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	err = cmd.Run()
	res.Output = buf.String()
	if err != nil {
		res.Status = "failed"
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.Code = exit.ExitCode()
		} else {
			res.Code = -1
		}
		if ctx.Err() == nil && cctx.Err() == context.DeadlineExceeded {
			// Killed for being slow, not for failing: say so, or the exit code (-1) reads as a crash and the agent
			// is sent to repair code that may be fine.
			res.Note = fmt.Sprintf("did not finish within %s and was stopped", CheckLimit)
		}
		if ctx.Err() == nil && cctx.Err() == nil && IsEnvFailure(res.Code, res.Output) {
			res.Status, res.Note = "skipped", "could not run here: a required tool is missing"
		}
		return res
	}
	res.Status = "passed"
	return res
}

func exists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

func makeHasTest(root string) bool {
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "test:" || strings.HasPrefix(line, "test:") {
				return true
			}
		}
	}
	return false
}

func split(command string) ([]string, error) {
	var args []string
	var word strings.Builder
	quote := rune(0)
	escaped, started := false, false
	for _, r := range command {
		if escaped {
			word.WriteRune(r)
			escaped, started = false, true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote, started = r, true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated escape or quote")
	}
	if started {
		args = append(args, word.String())
	}
	return args, nil
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	const marker = "\n... output truncated ...\n"
	const bodyLimit = outputLimit - len(marker)
	if !b.truncated && len(b.buf)+len(p) <= outputLimit {
		b.buf = append(b.buf, p...)
		return original, nil
	}
	if !b.truncated {
		b.truncated = true
		combined := append(append([]byte(nil), b.buf...), p...)
		head := bodyLimit / 2
		b.buf = append(b.buf[:0], combined[:head]...)
		b.buf = append(b.buf, combined[len(combined)-(bodyLimit-head):]...)
	} else {
		head := bodyLimit / 2
		tailLimit := bodyLimit - head
		if len(p) >= tailLimit {
			b.buf = append(b.buf[:head], p[len(p)-tailLimit:]...)
		} else {
			currentTail := b.buf[head:]
			if excess := len(currentTail) + len(p) - tailLimit; excess > 0 {
				currentTail = currentTail[excess:]
			}
			b.buf = append(b.buf[:head], currentTail...)
			b.buf = append(b.buf, p...)
		}
	}
	return original, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.truncated {
		return string(b.buf)
	}
	const marker = "\n... output truncated ...\n"
	head := (outputLimit - len(marker)) / 2
	return string(b.buf[:head]) + marker + string(b.buf[head:])
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range in {
		if seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}
