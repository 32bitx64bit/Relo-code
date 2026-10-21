package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// routed is a second backend: it hands out ids with its prefix and records what reached it.
type routed struct {
	prefix string
	id     string // the next session id; prefix+"1" when empty
	calls  []string
	models []Model
	failed bool
}

func (r *routed) note(call string, session SessionID) {
	r.calls = append(r.calls, call+" "+string(session))
}
func (r *routed) StartSession(context.Context, SessionConfig) (SessionID, error) {
	id := r.id
	if id == "" {
		id = r.prefix + "1"
	}
	r.note("start", SessionID(id))
	return SessionID(id), nil
}
func (r *routed) SendTask(_ context.Context, s SessionID, _ AgentTask) error {
	r.note("send", s)
	return nil
}
func (r *routed) Events(_ context.Context, s SessionID) (<-chan Event, error) {
	r.note("events", s)
	return make(chan Event), nil
}
func (r *routed) Cancel(_ context.Context, s SessionID) error       { r.note("cancel", s); return nil }
func (r *routed) CloseSession(_ context.Context, s SessionID) error { r.note("close", s); return nil }
func (r *routed) Models(context.Context) ([]Model, error) {
	if r.failed {
		return nil, errors.New("not signed in")
	}
	return r.models, nil
}
func (r *routed) Providers(context.Context) ([]Provider, error) {
	if r.failed {
		return nil, errors.New("not signed in")
	}
	return []Provider{{ID: "claude-code", Name: "Claude Code", Models: r.models}}, nil
}
func (r *routed) Health(context.Context) Health { return Health{Healthy: false, Kind: "routed"} }
func (r *routed) Close() error                  { r.calls = append(r.calls, "close-all"); return nil }
func (r *routed) ConfigureMessages(_ context.Context, dir string, _ MessageTools) error {
	r.calls = append(r.calls, "messages "+dir)
	return nil
}
func (r *routed) DeliverMessage(_ context.Context, s SessionID, _, _ string) error {
	r.note("deliver", s)
	return nil
}
func (r *routed) ReplyPermission(_ context.Context, s, _, reply string) error {
	r.note("permission "+reply, SessionID(s))
	return nil
}

func TestMuxRoutesASessionToTheBackendOfItsProvider(t *testing.T) {
	ctx := context.Background()
	base := NewMock()
	cc := &routed{prefix: "claude_", models: []Model{{ID: "claude-sonnet-5-5", Provider: "claude-code"}}}
	m := NewMux(base, Route{Provider: "claude-code", Prefix: "claude_", Runtime: cc})

	sid, err := m.StartSession(ctx, SessionConfig{Provider: "claude-code", Directory: "/w/a"})
	if err != nil || sid != "claude_1" {
		t.Fatalf("start = %q, %v", sid, err)
	}
	_ = m.SendTask(ctx, sid, AgentTask{})
	_, _ = m.Events(ctx, sid)
	_ = m.Cancel(ctx, sid)
	_ = m.ConfigureMessages(ctx, "/w/a", MessageTools{URL: "http://hub"})
	_ = m.DeliverMessage(ctx, sid, "p1", "hi")
	_ = m.ReplyPermission(ctx, string(sid), "perm-1", "once")
	_ = m.CloseSession(ctx, sid)
	want := "start claude_1,send claude_1,events claude_1,cancel claude_1,messages /w/a,deliver claude_1,permission once claude_1,close claude_1"
	if got := strings.Join(cc.calls, ","); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
	if err := m.Compact(ctx, sid); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("compact on a backend that cannot = %v", err)
	}

	other, err := m.StartSession(ctx, SessionConfig{Provider: "openrouter", Directory: "/w/b"})
	if err != nil || strings.HasPrefix(string(other), "claude_") {
		t.Fatalf("an openrouter session = %q, %v; want one from the base runtime", other, err)
	}
	if err := m.SendTask(ctx, other, AgentTask{Prompt: "x"}); err != nil {
		t.Fatalf("send to the base runtime: %v", err)
	}
	if len(cc.calls) != len(strings.Split(want, ",")) {
		t.Fatalf("the base runtime's session reached the routed backend: %v", cc.calls)
	}
	// The directory's latest session is the base runtime's, and the mock has no messaging.
	if err := m.ConfigureMessages(ctx, "/w/b", MessageTools{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("messages for a base session's directory = %v, want the base runtime's answer", err)
	}
}

func TestMuxRefusesASessionIdItCouldNotRouteBack(t *testing.T) {
	ctx := context.Background()
	cc := &routed{prefix: "claude_", id: "oc_123"}
	m := NewMux(NewMock(), Route{Provider: "claude-code", Prefix: "claude_", Runtime: cc})
	if _, err := m.StartSession(ctx, SessionConfig{Provider: "claude-code"}); err == nil {
		t.Fatal("a session without the route's prefix was accepted; later calls would reach the base runtime")
	}
	if cc.calls[len(cc.calls)-1] != "close oc_123" {
		t.Fatalf("the stray session was left open: %v", cc.calls)
	}
}

func TestMuxListsEveryBackendsModels(t *testing.T) {
	ctx := context.Background()
	cc := &routed{prefix: "claude_", models: []Model{{ID: "claude-sonnet-5-5", Provider: "claude-code"}}}
	m := NewMux(NewMock(), Route{Provider: "claude-code", Prefix: "claude_", Runtime: cc})
	ms, err := m.Models(ctx)
	if err != nil || len(ms) != 4 || ms[3].Provider != "claude-code" {
		t.Fatalf("models = %+v, %v", ms, err)
	}
	ps, _ := m.Providers(ctx)
	if len(ps) == 0 || ps[len(ps)-1].ID != "claude-code" {
		t.Fatalf("providers = %+v", ps)
	}
	if h := m.Health(ctx); !h.Healthy {
		t.Fatalf("health = %+v, want the base runtime's", h)
	}

	cc.failed = true
	if ms, err := m.Models(ctx); err != nil || len(ms) != 3 {
		t.Fatalf("with the routed backend failing: %+v, %v; want the base catalog alone", ms, err)
	}
	_ = m.Close()
	if cc.calls[len(cc.calls)-1] != "close-all" {
		t.Fatal("Close did not reach the routed backend")
	}
}
