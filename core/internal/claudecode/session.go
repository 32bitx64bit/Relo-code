package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/runtime"
)

// A session is one `claude` process in stream mode. It starts with the first task, because the process takes the
// model, effort, system prompt and tools as flags, and it keeps the conversation for every later task of the session:
// a follow-up prompt carries only what is new, the way Relo sends it to OpenCode.
type session struct {
	id       runtime.SessionID
	rt       *Runtime
	dir      string
	readOnly bool

	mu        sync.Mutex
	proc      *process
	ended     bool   // the process has exited; the conversation is gone
	key       string // the process's flags, fixed for its life
	turn      bool   // a Relo turn is running
	interrupt bool   // Cancel asked for an interrupt; the turn's result confirms it
	timedOut  string // the turn timer fired; the result reports it
	timer     *time.Timer
	turns     int // counts the session's turns, so a late timer can tell its turn has ended
	perms     map[string]json.RawMessage // open permission requests: request id -> the tool input
	seq       int
	tr        translator
	files     []string

	// Events go through an unbounded queue, so a slow reader never stalls the CLI's output.
	queue  []runtime.Event
	signal chan struct{}
	out    chan runtime.Event
	stop   chan struct{}
	closed bool
	pumped chan struct{}
}

type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	done   chan struct{} // closed when the output ends and the process has been waited for
	stderr *tail
	exit   error
}

func (p *process) write(v any) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return writeLine(p.stdin, v)
}

func newSession(rt *Runtime, id runtime.SessionID, cfg runtime.SessionConfig) *session {
	s := &session{id: id, rt: rt, dir: cfg.Directory, readOnly: cfg.ReadOnly, perms: map[string]json.RawMessage{},
		signal: make(chan struct{}, 1), out: make(chan runtime.Event, 64), stop: make(chan struct{}), pumped: make(chan struct{})}
	go s.pump()
	return s
}

func (s *session) pump() {
	defer close(s.pumped)
	for {
		s.mu.Lock()
		batch := s.queue
		s.queue = nil
		s.mu.Unlock()
		for _, ev := range batch {
			select {
			case s.out <- ev:
			case <-s.stop:
				return
			}
		}
		select {
		case <-s.signal:
		case <-s.stop:
			return
		}
	}
}

// emitLocked queues an event for the reader. Caller holds mu.
func (s *session) emitLocked(ev runtime.Event) {
	if s.closed {
		return
	}
	ev.Session = string(s.id)
	s.queue = append(s.queue, ev)
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *session) emit(ev runtime.Event) {
	s.mu.Lock()
	s.emitLocked(ev)
	s.mu.Unlock()
}

func (s *session) sendTask(ctx context.Context, task runtime.AgentTask) error {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return errors.New("Claude Code session is closed")
	case s.ended:
		s.mu.Unlock()
		return errors.New("the Claude Code process for this session has ended; the agent needs a new session")
	case s.turn:
		s.mu.Unlock()
		return errors.New("a turn is already running in this Claude Code session")
	}
	if task.Directory != "" && s.proc == nil {
		s.dir = task.Directory
	}
	args, key, err := s.flags(task)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if s.proc == nil {
		if err := s.startLocked(args); err != nil {
			s.mu.Unlock()
			return err
		}
		s.key = key
	} else if key != s.key {
		// The flags cannot change on a running process. Relo starts a new session when an agent's model or context
		// generation changes, so this is a setting edited mid-run; it applies from the agent's next session.
		s.rt.logf("claude code session %s keeps its flags; the changed model, effort, prompt or tools apply from its next session", s.id)
	}
	s.turn, s.interrupt, s.timedOut = true, false, ""
	s.turns++
	s.tr.startTurn()
	if task.TimeoutSec > 0 {
		turn, limit := s.turns, task.TimeoutSec
		s.timer = time.AfterFunc(time.Duration(limit)*time.Second, func() { s.expire(turn, limit) })
	}
	p := s.proc
	s.mu.Unlock()
	// Written without the lock: a large prompt can fill the pipe, and the output reader needs the lock meanwhile.
	if err := p.write(userMessage(task.Prompt)); err != nil {
		s.mu.Lock()
		s.turn = false
		s.stopTimerLocked()
		s.mu.Unlock()
		return fmt.Errorf("send the task to Claude Code: %w", err)
	}
	return nil
}

// flags builds the process's command line from a task. The system prompt and the messaging config go in files that
// only this user can read: the messaging config holds the hub's secret, which must not show in a process list.
func (s *session) flags(task runtime.AgentTask) ([]string, string, error) {
	builtins, allowed, denied := toolFlags(task.Tools, s.readOnly)
	key := strings.Join([]string{task.Model, task.Effort, hash(task.System), strings.Join(builtins, ","), strings.Join(allowed, ","), strings.Join(denied, ",")}, "\x00")
	if s.proc != nil {
		return nil, key, nil
	}
	if task.Model == "" {
		return nil, "", errors.New("Claude Code needs a model for the session")
	}
	system, err := s.tempFile("system-*.txt", []byte(task.System))
	if err != nil {
		return nil, "", err
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--model", task.Model, "--system-prompt-file", system, "--tools", strings.Join(builtins, ","),
		"--permission-prompt-tool", "stdio", "--safe-mode", "--strict-mcp-config", "--no-session-persistence"}
	if task.Effort != "" {
		args = append(args, "--effort", task.Effort)
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	if len(denied) > 0 {
		args = append(args, "--disallowedTools", strings.Join(denied, ","))
	}
	if tools, ok := s.rt.messageTools(s.dir); ok {
		config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{mcpServer: map[string]any{
			"type": "http", "url": tools.URL, "headers": map[string]string{"Authorization": "Bearer " + tools.Secret},
		}}})
		file, err := s.tempFile("mcp-*.json", config)
		if err != nil {
			return nil, "", err
		}
		args = append(args, "--mcp-config", file)
	}
	return args, key, nil
}

func (s *session) tempFile(pattern string, content []byte) (string, error) {
	f, err := os.CreateTemp("", "relo-claude-"+pattern)
	if err != nil {
		return "", err
	}
	s.files = append(s.files, f.Name())
	if _, err := f.Write(content); err != nil {
		f.Close()
		return "", err
	}
	return f.Name(), f.Close()
}

// startLocked starts the CLI. The initialize request goes first; the CLI reads its input in order, so the task
// written after it needs no wait.
func (s *session) startLocked(args []string) error {
	cmd := exec.Command(s.rt.bin, args...)
	cmd.Dir = s.dir
	cmd.Env = s.rt.env
	cmd.SysProcAttr = childAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	p := &process{cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: &tail{max: 4096}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Claude Code: %w", err)
	}
	s.proc = p
	go s.read(p, stdout)
	if err := p.write(controlRequest("relo_init", map[string]any{"subtype": "initialize", "hooks": nil})); err != nil {
		return fmt.Errorf("start Claude Code: %w", err)
	}
	return nil
}

func (s *session) read(p *process, stdout io.Reader) {
	lines := newLineReader(stdout)
	for {
		line, err := lines.next()
		if err != nil {
			break
		}
		s.handle(p, line)
	}
	p.exit = p.cmd.Wait()
	close(p.done)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
	if s.turn && !s.closed {
		s.turn = false
		s.stopTimerLocked()
		msg := "Claude Code exited"
		if p.exit != nil {
			msg += " (" + p.exit.Error() + ")"
		}
		if t := strings.TrimSpace(p.stderr.String()); t != "" {
			msg += ": " + t
		}
		s.emitLocked(runtime.Event{Type: "session.execution.failed", Done: true, Error: msg})
	}
}

func (s *session) handle(p *process, line []byte) {
	var head struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if json.Unmarshal(line, &head) != nil {
		return
	}
	switch head.Type {
	case "control_request":
		s.control(p, head.RequestID, head.Request)
		return
	case "control_response", "rate_limit_event", "keep_alive":
		if head.Type == "rate_limit_event" {
			s.rt.noteRateLimit(line)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	evs, end := s.tr.feed(line)
	if !s.turn {
		// A peer message that reached the CLI just as a turn ended starts a turn nobody waits for. Its output is not
		// part of any Relo turn; the next task joins it, and its result ends that task's turn.
		return
	}
	for _, ev := range evs {
		s.emitLocked(ev)
	}
	if end != nil {
		s.finishLocked(end)
	}
}

func (s *session) finishLocked(end *turnEnd) {
	s.turn = false
	s.stopTimerLocked()
	clear(s.perms)
	interrupted, timedOut := s.interrupt, s.timedOut
	s.interrupt, s.timedOut = false, ""
	usage := end.usage
	switch {
	case timedOut != "":
		s.emitLocked(runtime.Event{Type: "session.error", Done: true, Error: timedOut, Usage: &usage})
	case interrupted:
		// Not terminal: the conversation is intact and the next task carries on from it.
		s.emitLocked(runtime.Event{Type: "session.execution.interrupted", Interrupted: true})
	case end.err != "":
		s.emitLocked(runtime.Event{Type: "session.execution.failed", Done: true, Error: end.err, Usage: &usage})
	default:
		s.emitLocked(runtime.Event{Type: "session.execution.succeeded", Done: true, Text: end.text, Usage: &usage})
	}
}

// control answers a request from the CLI. A permission request becomes Relo's permission event; anything else (hook
// callbacks, SDK-hosted MCP servers) is refused, since Relo registers none.
func (s *session) control(p *process, id string, raw json.RawMessage) {
	var req struct {
		Subtype string          `json:"subtype"`
		Tool    string          `json:"tool_name"`
		Input   json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.Subtype != "can_use_tool" {
		_ = p.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "not supported by Relo"}})
		return
	}
	s.mu.Lock()
	if !s.turn {
		s.mu.Unlock()
		_ = p.write(permissionReply(id, nil, false))
		return
	}
	s.perms[id] = req.Input
	var input map[string]any
	_ = json.Unmarshal(req.Input, &input)
	perm := map[string]any{"id": id, "title": permissionAction(req.Tool), "pattern": describe(req.Tool, input), "tool": reloTool(req.Tool)}
	if path, _ := input["file_path"].(string); path != "" {
		perm["path"] = path
	}
	if req.Tool == "Bash" {
		perm["pattern"], _ = input["command"].(string)
	}
	s.emitLocked(runtime.Event{Type: "permission.asked", Phase: "tool", Permission: perm})
	s.mu.Unlock()
}

// permissionAction is the permission's title in Relo's terms, which permission.Classify reads.
func permissionAction(tool string) string {
	switch tool {
	case "Bash":
		return "bash"
	case "Edit", "Write", "NotebookEdit":
		return "edit"
	case "WebFetch", "WebSearch":
		return "webfetch"
	}
	return reloTool(tool)
}

func permissionReply(id string, input json.RawMessage, allow bool) map[string]any {
	answer := map[string]any{"behavior": "deny", "message": "Relo's permission policy refused this. Do not retry it; report it if the task needs it."}
	if allow {
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		answer = map[string]any{"behavior": "allow", "updatedInput": input}
	}
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": answer}}
}

func (s *session) replyPermission(permID, response string) error {
	s.mu.Lock()
	input, ok := s.perms[permID]
	delete(s.perms, permID)
	p := s.proc
	s.mu.Unlock()
	if !ok || p == nil {
		return fmt.Errorf("no open permission request %s", permID)
	}
	return p.write(permissionReply(permID, input, response == "once" || response == "always"))
}

// cancel interrupts the running turn. The CLI stops the model call or the tool and ends the turn; that result is
// reported as interrupted, and the session takes the next task as usual.
func (s *session) cancel() error {
	s.mu.Lock()
	if !s.turn || s.proc == nil || s.ended {
		s.mu.Unlock()
		return nil
	}
	s.interrupt = true
	p := s.interruptRequestLocked()
	s.mu.Unlock()
	return p()
}

// interruptRequestLocked returns the write that asks the CLI to interrupt, to run once the lock is released.
func (s *session) interruptRequestLocked() func() error {
	s.seq++
	p, req := s.proc, controlRequest(fmt.Sprintf("relo_interrupt_%d", s.seq), map[string]any{"subtype": "interrupt"})
	return func() error { return p.write(req) }
}

// expire ends a turn that ran past its time limit. When the CLI does not answer the interrupt, the process goes.
func (s *session) expire(turn, limit int) {
	s.mu.Lock()
	if s.turns != turn || !s.turn {
		s.mu.Unlock()
		return
	}
	s.timedOut = fmt.Sprintf("agent timed out after %ds", limit)
	p, interrupt := s.proc, s.interruptRequestLocked()
	s.mu.Unlock()
	if interrupt() == nil {
		select {
		case <-p.done:
			return
		case <-time.After(10 * time.Second):
		}
		s.mu.Lock()
		stillOpen := s.turn && s.timedOut != ""
		s.mu.Unlock()
		if !stillOpen {
			return
		}
	}
	_ = p.cmd.Process.Kill()
}

func (s *session) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// deliver puts a peer message into the running turn. The CLI reads it at the next step, the way OpenCode's steering
// message is read. With no turn running it is left for Relo to carry in the next task.
func (s *session) deliver(text string) error {
	s.mu.Lock()
	if !s.turn || s.proc == nil || s.ended {
		s.mu.Unlock()
		return errors.New("no Claude Code turn is running; the message waits for the next task")
	}
	p := s.proc
	s.mu.Unlock()
	return p.write(userMessage(text))
}

func (s *session) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.turn = false
	s.stopTimerLocked()
	p := s.proc
	files := s.files
	s.files = nil
	s.mu.Unlock()
	close(s.stop)
	<-s.pumped
	if p != nil {
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
	for _, f := range files {
		_ = os.Remove(f)
	}
	close(s.out)
	return nil
}

// toolFlags turns Relo's tool policy into Claude Code's flags: the built-in tools the model is offered, the ones that
// run without asking, and the hub tools it must not see. Bash is offered but not pre-approved: Claude Code runs
// read-only commands on its own and asks about any other, and that question goes to Relo's permission policy.
func toolFlags(policy map[string]bool, readOnly bool) (builtins, allowed, denied []string) {
	seen := map[string]bool{}
	for name, on := range policy {
		tool, ok := claudeTools[name]
		if !on || !ok || seen[tool] {
			continue
		}
		if readOnly && (tool == "Edit" || tool == "Write" || tool == "Bash") {
			continue
		}
		seen[tool] = true
		builtins = append(builtins, tool)
		if tool != "Bash" {
			allowed = append(allowed, tool)
		}
	}
	for relo, claude := range reloMCPTools {
		if policy[relo] {
			allowed = append(allowed, claude)
		} else {
			denied = append(denied, claude)
		}
	}
	sort.Strings(builtins)
	sort.Strings(allowed)
	sort.Strings(denied)
	return builtins, allowed, denied
}

// tail keeps the last bytes written to it: the CLI's stderr, for the error when it exits.
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
