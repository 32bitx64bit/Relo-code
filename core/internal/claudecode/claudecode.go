// Package claudecode runs Relo agents on the Claude Code CLI (`claude`), using the CLI's own sign-in.
//
// Each Relo session is one CLI process in stream mode: Relo gives it the agent's system prompt in place of Claude
// Code's, only the tools the agent's role may use, no MCP servers but Relo's own hub, and no settings, hooks, skills
// or CLAUDE.md (safe mode). The CLI keeps Claude's signed thinking between tool steps and caches the prompt, which
// an OpenAI-format route cannot do. Relo never reads the CLI's credentials; how it is signed in, and what that sign-in
// may be used for, is the user's account with Anthropic.
package claudecode

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/relocode/relo/internal/runtime"
)

// ProviderID is the provider Relo lists Claude Code's models under: claude-code/claude-sonnet-5-5.
const ProviderID = "claude-code"

// SessionPrefix starts every session id this runtime returns, so a runtime.Mux can route by it.
const SessionPrefix = "claude_"

type Runtime struct {
	bin  string
	env  []string
	info Info
	logf func(format string, args ...any)

	mu        sync.Mutex
	sessions  map[runtime.SessionID]*session
	messaging map[string]runtime.MessageTools // directory -> Relo's hub endpoint
	rateLimit json.RawMessage
}

var (
	_ runtime.AgentRuntime      = (*Runtime)(nil)
	_ runtime.MessageRuntime    = (*Runtime)(nil)
	_ runtime.PermissionReplier = (*Runtime)(nil)
)

// New probes the CLI at bin and returns a runtime for it. logf may be nil.
func New(ctx context.Context, bin string, logf func(format string, args ...any)) (*Runtime, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	env := Env(os.Environ())
	info, err := Probe(ctx, bin, env)
	if err != nil {
		return nil, err
	}
	return &Runtime{bin: bin, env: env, info: info, logf: logf, sessions: map[runtime.SessionID]*session{}, messaging: map[string]runtime.MessageTools{}}, nil
}

// Env is the CLI's environment: Relo's own, without the variables that tie a Claude Code process to the session
// that started it. When Relo itself runs under Claude Code, a child carrying them would report into that session.
func Env(base []string) []string {
	drop := map[string]bool{"CLAUDECODE": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_CHILD_SESSION": true,
		"CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true, "CLAUDE_CODE_ENTRYPOINT": true,
		"CLAUDE_CODE_SESSION_ATTENDED": true, "CLAUDE_CODE_EXECPATH": true, "CLAUDE_CODE_SSE_PORT": true}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			out = append(out, kv)
		}
	}
	return out
}

// Info is what the probe found: the CLI version, the kind of sign-in and the models.
func (r *Runtime) Info() Info { return r.info }

func (r *Runtime) StartSession(_ context.Context, cfg runtime.SessionConfig) (runtime.SessionID, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := runtime.SessionID(SessionPrefix + hex.EncodeToString(b))
	r.mu.Lock()
	r.sessions[id] = newSession(r, id, cfg)
	r.mu.Unlock()
	return id, nil
}

func (r *Runtime) session(id runtime.SessionID) (*session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[id]
	if s == nil {
		return nil, fmt.Errorf("unknown Claude Code session %s", id)
	}
	return s, nil
}

func (r *Runtime) SendTask(ctx context.Context, id runtime.SessionID, task runtime.AgentTask) error {
	s, err := r.session(id)
	if err != nil {
		return err
	}
	return s.sendTask(ctx, task)
}

// Events returns the session's event stream. A session has one stream, closed when the session closes.
func (r *Runtime) Events(_ context.Context, id runtime.SessionID) (<-chan runtime.Event, error) {
	s, err := r.session(id)
	if err != nil {
		return nil, err
	}
	return s.out, nil
}

// Cancel interrupts the session's turn. An unknown or idle session has nothing to cancel.
func (r *Runtime) Cancel(_ context.Context, id runtime.SessionID) error {
	s, err := r.session(id)
	if err != nil {
		return nil
	}
	return s.cancel()
}

func (r *Runtime) CloseSession(_ context.Context, id runtime.SessionID) error {
	r.mu.Lock()
	s := r.sessions[id]
	delete(r.sessions, id)
	r.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.close()
}

func (r *Runtime) Models(context.Context) ([]runtime.Model, error) {
	return append([]runtime.Model(nil), r.info.Models...), nil
}

func (r *Runtime) Providers(ctx context.Context) ([]runtime.Provider, error) {
	ms, _ := r.Models(ctx)
	return []runtime.Provider{{ID: ProviderID, Name: "Claude Code", Models: ms}}, nil
}

func (r *Runtime) Health(context.Context) runtime.Health {
	return runtime.Health{Healthy: true, Kind: ProviderID, Version: r.info.Version}
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	sessions := r.sessions
	r.sessions = map[runtime.SessionID]*session{}
	r.mu.Unlock()
	for _, s := range sessions {
		_ = s.close()
	}
	return nil
}

// ConfigureMessages records Relo's hub endpoint for a directory. A session started there passes it to the CLI as its
// one MCP server, so the agent gets relo_agents (and relo_image or relo_canvas when its role may use them).
func (r *Runtime) ConfigureMessages(_ context.Context, directory string, tools runtime.MessageTools) error {
	r.mu.Lock()
	r.messaging[directory] = tools
	r.mu.Unlock()
	return nil
}

func (r *Runtime) messageTools(directory string) (runtime.MessageTools, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.messaging[directory]
	return t, ok && t.URL != ""
}

func (r *Runtime) DeliverMessage(_ context.Context, id runtime.SessionID, _, text string) error {
	s, err := r.session(id)
	if err != nil {
		return err
	}
	return s.deliver(text)
}

// ReplyPermission answers a permission request the CLI sent: once and always allow, reject refuses.
func (r *Runtime) ReplyPermission(_ context.Context, session, permID, response string) error {
	s, err := r.session(runtime.SessionID(session))
	if err != nil {
		return err
	}
	return s.replyPermission(permID, response)
}

// noteRateLimit keeps the CLI's latest report on the plan's usage windows.
func (r *Runtime) noteRateLimit(line []byte) {
	r.mu.Lock()
	r.rateLimit = append(json.RawMessage(nil), line...)
	r.mu.Unlock()
}

// RateLimit is the latest rate_limit_event the CLI sent, or nil.
func (r *Runtime) RateLimit() json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rateLimit
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
