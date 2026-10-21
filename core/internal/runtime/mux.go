package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Route is one extra backend behind a Mux: the runtime that serves a provider's models.
type Route struct {
	Provider string // provider id whose sessions this backend runs
	Prefix   string // every session id the backend returns starts with it
	Runtime  AgentRuntime
}

// Mux puts more than one runtime behind the single AgentRuntime the orchestrator holds. A session is started on the
// backend that serves its provider, and its id carries that backend's prefix, so every later call for it reaches the
// same backend. Sessions of any other provider go to the base runtime (OpenCode). The optional abilities the
// orchestrator finds by type assertion are forwarded to whichever backend owns the session.
type Mux struct {
	base   AgentRuntime
	routes []Route

	mu   sync.Mutex
	dirs map[string]AgentRuntime // directory -> backend of the latest session started there
}

var (
	_ AgentRuntime      = (*Mux)(nil)
	_ Compactor         = (*Mux)(nil)
	_ MessageRuntime    = (*Mux)(nil)
	_ PermissionReplier = (*Mux)(nil)
)

// ErrUnsupported is returned when a session's backend lacks an optional ability.
var ErrUnsupported = errors.New("the session's runtime does not support this")

func NewMux(base AgentRuntime, routes ...Route) *Mux {
	return &Mux{base: base, routes: routes, dirs: map[string]AgentRuntime{}}
}

func (m *Mux) owner(session SessionID) AgentRuntime {
	for _, r := range m.routes {
		if strings.HasPrefix(string(session), r.Prefix) {
			return r.Runtime
		}
	}
	return m.base
}

func (m *Mux) StartSession(ctx context.Context, cfg SessionConfig) (SessionID, error) {
	rt, prefix := m.base, ""
	for _, r := range m.routes {
		if cfg.Provider != "" && cfg.Provider == r.Provider {
			rt, prefix = r.Runtime, r.Prefix
			break
		}
	}
	sid, err := rt.StartSession(ctx, cfg)
	if err != nil {
		return "", err
	}
	if prefix != "" && !strings.HasPrefix(string(sid), prefix) {
		_ = rt.CloseSession(ctx, sid)
		return "", fmt.Errorf("runtime for %s returned session %q without its prefix %q", cfg.Provider, sid, prefix)
	}
	if prefix == "" && m.owner(sid) != m.base {
		_ = rt.CloseSession(ctx, sid)
		return "", fmt.Errorf("session %q collides with another backend's prefix", sid)
	}
	if cfg.Directory != "" {
		m.mu.Lock()
		m.dirs[cfg.Directory] = rt
		m.mu.Unlock()
	}
	return sid, nil
}

func (m *Mux) SendTask(ctx context.Context, session SessionID, task AgentTask) error {
	return m.owner(session).SendTask(ctx, session, task)
}

func (m *Mux) Events(ctx context.Context, session SessionID) (<-chan Event, error) {
	return m.owner(session).Events(ctx, session)
}

func (m *Mux) Cancel(ctx context.Context, session SessionID) error {
	return m.owner(session).Cancel(ctx, session)
}

func (m *Mux) CloseSession(ctx context.Context, session SessionID) error {
	return m.owner(session).CloseSession(ctx, session)
}

// Models lists every backend's models. A route that cannot list its models is left out rather than hiding the base
// runtime's catalog; the base runtime's error is returned.
func (m *Mux) Models(ctx context.Context) ([]Model, error) {
	out, err := m.base.Models(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, x := range out {
		seen[x.Provider+"/"+x.ID] = true
	}
	for _, r := range m.routes {
		ms, err := r.Runtime.Models(ctx)
		if err != nil {
			continue
		}
		for _, x := range ms {
			if !seen[x.Provider+"/"+x.ID] {
				seen[x.Provider+"/"+x.ID] = true
				out = append(out, x)
			}
		}
	}
	return out, nil
}

func (m *Mux) Providers(ctx context.Context) ([]Provider, error) {
	out, err := m.base.Providers(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range out {
		seen[p.ID] = true
	}
	for _, r := range m.routes {
		ps, err := r.Runtime.Providers(ctx)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if !seen[p.ID] {
				seen[p.ID] = true
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// Health is the base runtime's: it is the one Relo cannot run without.
func (m *Mux) Health(ctx context.Context) Health { return m.base.Health(ctx) }

func (m *Mux) Close() error {
	errs := []error{m.base.Close()}
	for _, r := range m.routes {
		errs = append(errs, r.Runtime.Close())
	}
	return errors.Join(errs...)
}

func (m *Mux) Compact(ctx context.Context, session SessionID) error {
	cp, ok := m.owner(session).(Compactor)
	if !ok {
		return ErrUnsupported
	}
	return cp.Compact(ctx, session)
}

// ConfigureMessages registers the messaging endpoint with the backend of the latest session started in the
// directory, which is the session about to use it.
func (m *Mux) ConfigureMessages(ctx context.Context, directory string, tools MessageTools) error {
	m.mu.Lock()
	rt := m.dirs[directory]
	m.mu.Unlock()
	if rt == nil {
		rt = m.base
	}
	mr, ok := rt.(MessageRuntime)
	if !ok {
		return ErrUnsupported
	}
	return mr.ConfigureMessages(ctx, directory, tools)
}

func (m *Mux) DeliverMessage(ctx context.Context, session SessionID, id, text string) error {
	mr, ok := m.owner(session).(MessageRuntime)
	if !ok {
		return ErrUnsupported
	}
	return mr.DeliverMessage(ctx, session, id, text)
}

// ReplyPermission answers a permission request on the backend that asked it.
func (m *Mux) ReplyPermission(ctx context.Context, session, permID, response string) error {
	pr, ok := m.owner(SessionID(session)).(PermissionReplier)
	if !ok {
		return ErrUnsupported
	}
	return pr.ReplyPermission(ctx, session, permID, response)
}
