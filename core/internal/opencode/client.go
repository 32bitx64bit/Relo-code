package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/runtime"
)

const serverUser = "opencode"

type subscription struct {
	ch     chan runtime.Event
	stop   chan struct{}
	once   sync.Once
	queue  []runtime.Event
	notify chan struct{}
}

type Client struct {
	Base     string
	HTTP     *http.Client
	Username string
	Password string

	mu        sync.Mutex
	subs      map[string][]*subscription
	cancel    context.CancelFunc
	models    map[string]string
	efforts   map[string]string // per-session effort (variant) last posted with the model
	instr     map[string]string
	locations map[string]string
	agents    map[string]string
	timers    map[string]*time.Timer
	turnDone  map[string]bool // terminal event already emitted for the session's current turn
	lastEvent string

	oversizeLogged bool
}

func NewClient(base string) *Client {
	return NewClientWithAuth(base, serverUser, os.Getenv("OPENCODE_SERVER_PASSWORD"))
}

func NewClientWithAuth(base, username, password string) *Client {
	return &Client{
		Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second},
		Username: username, Password: password, subs: map[string][]*subscription{},
		models: map[string]string{}, efforts: map[string]string{}, instr: map[string]string{}, locations: map[string]string{}, agents: map[string]string{}, timers: map[string]*time.Timer{},
		turnDone: map[string]bool{},
	}
}

func (c *Client) request(ctx context.Context, method, path string, body any, dest any, stream bool) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(mustJSON(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, r)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		c.mu.Lock()
		lastEvent := c.lastEvent
		c.mu.Unlock()
		if lastEvent != "" {
			req.Header.Set("Last-Event-ID", lastEvent)
		}
	}
	hc := c.HTTP
	if stream {
		copy := *hc
		copy.Timeout = 0
		hc = &copy
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 && !(res.StatusCode == http.StatusNoContent) {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("opencode authentication failed (HTTP 401): %s", strings.TrimSpace(string(b)))
		}
		return nil, &httpError{status: res.StatusCode, msg: fmt.Sprintf("opencode %s %s: %s %s", method, path, res.Status, b)}
	}
	if dest != nil {
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
		if len(b) > 0 {
			if err := json.Unmarshal(b, dest); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if !stream {
		res.Body.Close()
		return nil, nil
	}
	return res, nil
}

func (c *Client) Health(ctx context.Context) runtime.Health {
	var h struct {
		Version string `json:"version"`
	}
	if _, err := c.request(ctx, http.MethodGet, "/api/info", nil, &h, false); err != nil {
		return runtime.Health{Kind: "opencode", Error: err.Error()}
	}
	return runtime.Health{Healthy: true, Version: h.Version, Kind: "opencode"}
}

func (c *Client) StartSession(ctx context.Context, cfg runtime.SessionConfig) (runtime.SessionID, error) {
	body := map[string]any{"title": cfg.Title}
	if cfg.Parent != "" {
		body["parentID"] = cfg.Parent
	}
	if cfg.Directory != "" {
		body["location"] = map[string]string{"directory": cfg.Directory}
	}
	if cfg.ReadOnly {
		body["permissions"] = []map[string]string{{"action": "*", "resource": "*", "effect": "allow"}, {"action": "edit", "resource": "*", "effect": "deny"}, {"action": "write", "resource": "*", "effect": "deny"}, {"action": "bash", "resource": "*", "effect": "deny"}, {"action": "shell", "resource": "*", "effect": "deny"}}
	}
	var payload struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if _, err := c.request(ctx, http.MethodPost, "/api/session", body, &payload, false); err != nil {
		return "", err
	}
	if cfg.Parent != "" && cfg.Directory != "" {
		if err := c.post(ctx, "/api/session/"+url.PathEscape(payload.Data.ID)+"/move", map[string]any{"directory": cfg.Directory}, nil); err != nil {
			return "", err
		}
	}
	c.mu.Lock()
	if cfg.Directory != "" {
		c.locations[payload.Data.ID] = cfg.Directory
	}
	c.mu.Unlock()
	return runtime.SessionID(payload.Data.ID), nil
}

func (c *Client) SendTask(ctx context.Context, session runtime.SessionID, task runtime.AgentTask) error {
	sid := string(session)
	c.stopTurnTimer(sid) // a timer left by a previous turn must not cancel this one
	c.mu.Lock()
	delete(c.turnDone, sid) // new turn: its terminal event is not yet claimed
	dir := task.Directory
	if dir == "" {
		dir = c.locations[sid]
	}
	prevModel, prevSystem, prevEffort := c.models[sid], c.instr[sid], c.efforts[sid]
	c.mu.Unlock()
	if dir != "" && dir != c.sessionDirectory(sid) {
		if err := c.post(ctx, "/api/session/"+url.PathEscape(sid)+"/move", map[string]any{"directory": dir}, nil); err != nil {
			return err
		}
		c.mu.Lock()
		c.locations[sid] = dir
		c.mu.Unlock()
	}
	model := ""
	if task.Provider != "" && task.Model != "" {
		model = task.Provider + "/" + task.Model
		// Re-post when the model or the effort changed. An empty effort re-posts without a
		// variant, which resets a previously chosen effort back to the model default.
		if model != prevModel || task.Effort != prevEffort {
			ref := map[string]string{"providerID": task.Provider, "id": task.Model}
			if task.Effort != "" {
				ref["variant"] = task.Effort
			}
			if err := c.post(ctx, "/api/session/"+url.PathEscape(sid)+"/model", map[string]any{"model": ref}, nil); err != nil {
				return err
			}
			c.mu.Lock()
			c.models[sid] = model
			c.efforts[sid] = task.Effort
			c.mu.Unlock()
		}
	}
	if task.System != "" && prevSystem == "" {
		var entries struct {
			Data []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"data"`
		}
		path := "/api/experimental/session/" + url.PathEscape(sid) + "/instructions/entries"
		if err := c.get(ctx, path, &entries); err != nil {
			return err
		}
		for _, entry := range entries.Data {
			if entry.Key == "relo-system" && entry.Value == task.System {
				prevSystem = hash(task.System)
				break
			}
		}
		if prevSystem != "" {
			c.mu.Lock()
			c.instr[sid] = prevSystem
			c.mu.Unlock()
		}
	}
	if task.System != "" && hash(task.System) != prevSystem {
		path := "/api/experimental/session/" + url.PathEscape(sid) + "/instructions/entries/relo-system"
		if _, err := c.request(ctx, http.MethodPut, path, map[string]string{"value": task.System}, nil, false); err != nil {
			return err
		}
		c.mu.Lock()
		c.instr[sid] = hash(task.System)
		c.mu.Unlock()
	}
	// Every turn uses the build agent. OpenCode's plan agent adds a "Plan mode"
	// persona and reminder that leaks into user chat. Read-only is enforced by
	// setTools deny rules on the "edit" permission instead.
	c.mu.Lock()
	selectAgent := c.agents[sid] != "build"
	c.mu.Unlock()
	if selectAgent {
		if err := c.selectAgent(ctx, sid, "build"); err != nil {
			return err
		}
		c.mu.Lock()
		c.agents[sid] = "build"
		c.mu.Unlock()
	}
	if err := c.setTools(ctx, sid, task.Tools); err != nil {
		return err
	}
	body := map[string]any{"text": task.Prompt}
	c.startTurnTimer(sid, task.TimeoutSec)
	if _, err := c.request(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sid)+"/prompt", body, nil, false); err != nil {
		c.stopTurnTimer(sid)
		return err
	}
	return nil
}

func (c *Client) startTurnTimer(session string, timeoutSec int) {
	if timeoutSec <= 0 {
		return
	}
	c.mu.Lock()
	if old := c.timers[session]; old != nil {
		old.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(time.Duration(timeoutSec)*time.Second, func() {
		c.mu.Lock()
		if c.timers[session] != timer || !c.claimTurn(session) {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Cancel(ctx, runtime.SessionID(session)); err != nil {
			c.broadcast(session, runtime.Event{Type: "session.error", Session: session, Done: true, Error: fmt.Sprintf("agent timed out after %ds; interrupt failed: %v", timeoutSec, err)})
			return
		}
		c.broadcast(session, runtime.Event{Type: "session.error", Session: session, Done: true, Error: fmt.Sprintf("agent timed out after %ds", timeoutSec)})
	})
	c.timers[session] = timer
	c.mu.Unlock()
}

func (c *Client) stopTurnTimer(session string) {
	c.mu.Lock()
	if timer := c.timers[session]; timer != nil {
		timer.Stop()
		delete(c.timers, session)
	}
	c.mu.Unlock()
}

// claimTurn marks the session's current turn as terminated and stops its timer.
// It returns false if a terminal event was already emitted for this turn.
// Caller must hold c.mu.
func (c *Client) claimTurn(session string) bool {
	if c.turnDone[session] {
		return false
	}
	c.turnDone[session] = true
	if timer := c.timers[session]; timer != nil {
		timer.Stop()
		delete(c.timers, session)
	}
	return true
}

// globalEventType reports events that intentionally carry no session.
func globalEventType(t string) bool {
	return strings.HasPrefix(t, "backend.")
}

func (c *Client) sessionDirectory(session string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.locations[session]
}

func (c *Client) selectAgent(ctx context.Context, session, agent string) error {
	return c.post(ctx, "/api/session/"+url.PathEscape(session)+"/agent", map[string]string{"agent": agent}, nil)
}

func (c *Client) setTools(ctx context.Context, session string, tools map[string]bool) error {
	if len(tools) == 0 {
		return nil
	}
	rules := []map[string]string{{"action": "*", "resource": "*", "effect": "allow"}}
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic request body
	denied := map[string]bool{}
	deny := func(action string) {
		if !denied[action] {
			denied[action] = true
			rules = append(rules, map[string]string{"action": action, "resource": "*", "effect": "deny"})
		}
	}
	for _, name := range names {
		if tools[name] {
			continue
		}
		deny(name)
		switch name {
		case "write", "edit":
			deny("edit") // OpenCode gates the write and edit tools under the "edit" permission
		case "bash", "shell":
			// The command tool is called "shell" in OpenCode v2 and "bash" in v1. Denying only one name leaves the
			// other open, which let a toolless Primary run commands.
			deny("bash")
			deny("shell")
		}
	}
	return c.patch(ctx, "/api/session/"+url.PathEscape(session), map[string]any{"permissions": rules})
}

func (c *Client) Events(ctx context.Context, session runtime.SessionID) (<-chan runtime.Event, error) {
	s := &subscription{ch: make(chan runtime.Event, 64), stop: make(chan struct{}), notify: make(chan struct{}, 1)}
	c.mu.Lock()
	c.subs[string(session)] = append(c.subs[string(session)], s)
	go c.deliver(s)
	if c.cancel == nil {
		cctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		go c.stream(cctx)
	}
	c.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-s.stop:
		}
		c.unsub(string(session), s)
	}()
	return s.ch, nil
}

// Cancel interrupts the session's current work. A 404 means the session is
// already closed or finished, so there is nothing to cancel and it is not an error.
func (c *Client) Cancel(ctx context.Context, session runtime.SessionID) error {
	err := c.post(ctx, "/api/session/"+url.PathEscape(string(session))+"/interrupt", map[string]any{}, nil)
	return ignoreNotFound("interrupt", string(session), err)
}

// Compact asks OpenCode to summarize the session's history. It is admitted at once and, steered, runs at the next
// step boundary of a turn in progress; the turn carries on from the summary. Progress arrives as compaction events.
func (c *Client) Compact(ctx context.Context, session runtime.SessionID) error {
	return c.post(ctx, "/api/session/"+url.PathEscape(string(session))+"/compact", map[string]any{"delivery": "steer"}, nil)
}

func (c *Client) CloseSession(ctx context.Context, session runtime.SessionID) error {
	sid := string(session)
	c.mu.Lock()
	if timer := c.timers[sid]; timer != nil {
		timer.Stop()
		delete(c.timers, sid)
	}
	for _, sub := range c.subs[sid] {
		sub.once.Do(func() { close(sub.stop) })
	}
	delete(c.subs, sid)
	if len(c.subs) == 0 && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.mu.Unlock()
	err := c.delete(ctx, "/api/session/"+url.PathEscape(sid))
	c.mu.Lock()
	delete(c.models, sid)
	delete(c.efforts, sid)
	delete(c.instr, sid)
	delete(c.locations, sid)
	delete(c.agents, sid)
	c.mu.Unlock()
	return ignoreNotFound("close", sid, err) // already gone: idempotent success
}

// httpError keeps the HTTP status so callers can branch on it.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func ignoreNotFound(op, sid string, err error) error {
	var he *httpError
	if err != nil && errors.As(err, &he) && he.status == http.StatusNotFound {
		debugf("opencode: %s %s: session not found; treating as done", op, sid)
		return nil
	}
	return err
}

// debugf logs only when RELO_OPENCODE_DEBUG is set.
func debugf(format string, args ...any) {
	if os.Getenv("RELO_OPENCODE_DEBUG") != "" {
		log.Printf(format, args...)
	}
}

// Models lists enabled models from OpenCode's catalog, with context/output limits and effort
// variants parsed, then cross-filled from other providers serving the same model ID.
func (c *Client) Models(ctx context.Context) ([]runtime.Model, error) {
	var payload struct {
		Data []struct {
			ID         string `json:"modelID"`
			Name       string `json:"name"`
			ProviderID string `json:"providerID"`
			Enabled    bool   `json:"enabled"`
			Family     string `json:"family"`
			Limit      struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Variants []struct {
				ID string `json:"id"`
			} `json:"variants"`
		} `json:"data"`
	}
	if _, err := c.request(ctx, http.MethodGet, "/api/model", nil, &payload, false); err != nil {
		return nil, err
	}
	var out []runtime.Model
	families := map[string]string{}
	for _, m := range payload.Data {
		if !m.Enabled {
			continue
		}
		var efforts []string
		for _, v := range m.Variants {
			if v.ID != "" && v.ID != "none" {
				efforts = append(efforts, v.ID)
			}
		}
		out = append(out, runtime.Model{ID: m.ID, Provider: m.ProviderID, Name: m.Name, Context: m.Limit.Context, Output: m.Limit.Output, Efforts: efforts})
		if m.Family != "" {
			families[m.ProviderID+"/"+m.ID] = m.Family
		}
	}
	return CrossFill(out, families), nil
}

// CrossFill fills EMPTY limit/effort fields of a model from another provider's entry for the same
// model ID (exact modelID match), so a custom proxy serving "claude-opus-4" inherits the built-in
// Anthropic limits. Only models missing Context, Output or Efforts are touched, and only zero/empty
// fields are overwritten. A donor must share the model's family: both families must be known and equal
// (families is keyed "provider/modelID"; models without a family never receive a fill). When several
// donors qualify the first in list order wins, so the result is deterministic. Entries that already
// have every field are returned unchanged. models is not mutated.
func CrossFill(models []runtime.Model, families map[string]string) []runtime.Model {
	out := make([]runtime.Model, len(models))
	copy(out, models)
	for i := range out {
		m := &out[i]
		if m.Context > 0 && m.Output > 0 && len(m.Efforts) > 0 {
			continue
		}
		fam := families[m.Provider+"/"+m.ID]
		if fam == "" {
			continue
		}
		for _, d := range models {
			if d.ID != m.ID || d.Provider == m.Provider || families[d.Provider+"/"+d.ID] != fam {
				continue
			}
			if m.Context == 0 {
				m.Context = d.Context
			}
			if m.Output == 0 {
				m.Output = d.Output
			}
			if len(m.Efforts) == 0 {
				m.Efforts = append([]string(nil), d.Efforts...)
			}
			if m.Context > 0 && m.Output > 0 && len(m.Efforts) > 0 {
				break
			}
		}
	}
	return out
}

func (c *Client) Providers(ctx context.Context) ([]runtime.Provider, error) {
	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if _, err := c.request(ctx, http.MethodGet, "/api/provider", nil, &payload, false); err != nil {
		return nil, err
	}
	models, err := c.Models(ctx)
	if err != nil {
		return nil, err
	}
	byProvider := map[string][]runtime.Model{}
	for _, model := range models {
		byProvider[model.Provider] = append(byProvider[model.Provider], model)
	}
	var out []runtime.Provider
	for _, p := range payload.Data {
		out = append(out, runtime.Provider{ID: p.ID, Name: p.Name, Models: byProvider[p.ID]})
	}
	return out, nil
}

func (c *Client) ReplyPermission(ctx context.Context, session, permID, response string) error {
	decision := map[string]string{"once": "once", "always": "always", "reject": "reject"}[response]
	if decision == "" {
		return fmt.Errorf("invalid permission response %q", response)
	}
	path := "/api/session/" + url.PathEscape(session) + "/permission/" + url.PathEscape(permID) + "/reply"
	return c.post(ctx, path, map[string]string{"decision": decision}, nil)
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	for _, subs := range c.subs {
		for _, s := range subs {
			s.once.Do(func() { close(s.stop) })
		}
	}
	for sid, timer := range c.timers {
		timer.Stop()
		delete(c.timers, sid)
	}
	c.subs = map[string][]*subscription{}
	c.mu.Unlock()
	return nil
}

func (c *Client) stream(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.once(ctx)
		if ctx.Err() != nil {
			return
		}
		c.broadcast("", runtime.Event{Type: "backend.disconnected", Error: errString(err)})
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) once(ctx context.Context) error {
	res, err := c.request(ctx, http.MethodGet, "/api/event", nil, nil, true)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	br := bufio.NewReaderSize(res.Body, 64*1024)
	var data []string
	skipFrame := false // current frame contained an oversize line; drop it up to its blank terminator
	for {
		raw, oversize, rerr := readSSELine(br)
		line := strings.TrimRight(string(raw), "\r\n")
		switch {
		case oversize:
			c.skipOversize(raw)
			data = nil
			skipFrame = true
		case skipFrame:
			if line == "" {
				skipFrame = false
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case strings.HasPrefix(line, "id:"):
			c.mu.Lock()
			c.lastEvent = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			c.mu.Unlock()
		case line == "" && len(data) > 0:
			c.dispatch(ctx, strings.Join(data, "\n"))
			data = nil
		}
		if rerr != nil {
			if rerr == io.EOF {
				if len(data) > 0 && !skipFrame {
					c.dispatch(ctx, strings.Join(data, "\n"))
				}
				return nil
			}
			return rerr
		}
	}
}

const (
	sseMaxLine    = 8 * 1024 * 1024 // longest accepted SSE line
	ssePrefixKeep = 4096            // bytes kept from an oversize line to recover its envelope id
)

var sseEnvelopeID = regexp.MustCompile(`^data:\s*\{\s*"id"\s*:\s*"([^"]+)"`)

// readSSELine returns one line without its terminator. Lines over sseMaxLine
// are not returned in full: oversize is true and line holds only a prefix,
// the rest is consumed and discarded so the reader keeps moving.
func readSSELine(br *bufio.Reader) (line []byte, oversize bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		if !oversize && len(line)+len(chunk) > sseMaxLine {
			oversize = true
			line = line[:min(len(line), ssePrefixKeep)]
		}
		if !oversize {
			line = append(line, chunk...)
		} else if len(line) < ssePrefixKeep {
			n := min(len(chunk), ssePrefixKeep-len(line))
			line = append(line, chunk[:n]...)
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		return line, oversize, rerr
	}
}

// skipOversize logs once and advances lastEvent from the envelope id in the
// oversize line's prefix, so a reconnect does not replay the bad event.
func (c *Client) skipOversize(prefix []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := sseEnvelopeID.FindSubmatch(prefix); m != nil {
		c.lastEvent = string(m[1])
	}
	if !c.oversizeLogged {
		c.oversizeLogged = true
		log.Printf("opencode: SSE line exceeds %d bytes; discarding event and continuing", sseMaxLine)
	}
}

func (c *Client) dispatch(ctx context.Context, raw string) {
	var envelope struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Data       json.RawMessage `json:"data"`
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil || envelope.Type == "" {
		return
	}
	if envelope.ID != "" {
		c.mu.Lock()
		c.lastEvent = envelope.ID
		c.mu.Unlock()
	}
	var props map[string]any
	if len(envelope.Data) > 0 {
		_ = json.Unmarshal(envelope.Data, &props)
	}
	if len(props) == 0 && len(envelope.Properties) > 0 {
		_ = json.Unmarshal(envelope.Properties, &props)
	}
	if nested, ok := props["properties"].(map[string]any); ok {
		props = nested
	}
	ev := runtime.Event{Type: envelope.Type, Raw: props}
	props = eventProperties(envelope.Type, props)
	if sid, ok := props["sessionID"].(string); ok {
		ev.Session = sid
	}
	if info, ok := props["info"].(map[string]any); ok && ev.Session == "" {
		ev.Session, _ = info["id"].(string)
	}
	if part, ok := props["part"].(map[string]any); ok {
		ev.PartType, _ = part["type"].(string)
		ev.Text, _ = part["text"].(string)
		if ev.Session == "" {
			ev.Session, _ = part["sessionID"].(string)
		}
	}
	if delta, ok := props["delta"].(string); ok {
		ev.Delta = delta
	}
	if msg, ok := props["message"].(map[string]any); ok {
		if ev.Session == "" {
			ev.Session, _ = msg["sessionID"].(string)
		}
		setUsage(&ev, msg["tokens"])
	}
	setUsage(&ev, props["tokens"])
	switch envelope.Type {
	case "message.part.updated":
		if part, ok := props["part"].(map[string]any); ok {
			switch part["type"] {
			case "text":
				ev.Delta, _ = props["delta"].(string)
				ev.PartType, ev.Phase = "text", "writing"
			case "reasoning":
				ev.Delta, ev.Text, ev.PartType, ev.Phase = "", "", "reasoning", "reasoning"
			case "tool":
				ev.Delta, ev.Text, ev.PartType, ev.Phase = "", "", "tool", "tool"
				ev.Tool, _ = part["tool"].(string)
				ev.ToolID, _ = part["callID"].(string)
				if st, ok := part["state"].(map[string]any); ok {
					ev.ToolInput = ToolInputSummary(st["input"])
					if s, _ := st["status"].(string); s == "completed" || s == "error" {
						ev.ToolDone, ev.Phase = true, "model"
					}
				}
			}
		}
	case "session.step.started":
		ev.Phase = "model"
	case "session.reasoning.started":
		ev.Phase = "reasoning"
	case "session.reasoning.delta":
		ev.Phase, ev.Delta = "reasoning", "" // reasoning is progress, never reply text
	case "session.tool.input.started":
		ev.Phase, ev.PartType = "tool", "tool"
		ev.Tool, _ = props["name"].(string)
		ev.ToolID, _ = props["id"].(string)
	case "session.tool.input.delta":
		ev.Phase, ev.Delta = "tool", "" // streamed tool arguments are not reply text
	case "session.tool.called":
		ev.Phase, ev.PartType = "tool", "tool"
		ev.ToolID, _ = props["id"].(string)
		ev.ToolInput = ToolInputSummary(props["input"])
	case "session.tool.success", "session.tool.failed":
		ev.Phase, ev.ToolDone = "model", true
		ev.ToolID, _ = props["id"].(string)
	case "session.execution.interrupted":
		ev.Interrupted = true
	case "session.step.ended":
		// This usage is the one provider call that just ended; the session.usage.updated events around it add up.
		if u := ev.Usage; u != nil {
			ev.ContextTokens = u.Input + u.Output + u.CacheRead + u.CacheWrite
		}
		ev.MoreSteps = props["finish"] == "tool-calls"
	case "session.compaction.started":
		ev.Phase, ev.Compaction = "compacting", "started"
	case "session.compaction.delta":
		ev.Phase, ev.Delta = "compacting", ""
	case "session.compaction.ended":
		ev.Phase, ev.Compaction = "model", "ended"
	case "session.compaction.failed":
		ev.Phase, ev.Compaction = "model", "failed"
	case "session.retry.scheduled":
		ev.Phase = "retry"
		if n, ok := props["attempt"].(float64); ok {
			ev.Attempt = int(n)
		}
	case "session.text.started":
		ev.Phase = "writing"
	case "session.text.delta":
		ev.PartType, ev.Phase = "text", "writing"
		if ev.Delta == "" {
			ev.Delta, _ = props["delta"].(string)
		}
		if text, ok := props["text"].(string); ok && ev.Text == "" {
			ev.Text = text
		}
	case "message.updated":
		if info, ok := props["info"].(map[string]any); ok {
			if ev.Session == "" {
				ev.Session, _ = info["sessionID"].(string)
			}
			setUsage(&ev, info["tokens"])
		}
	case "session.idle", "session.status", "session.execution.succeeded":
		status := props["status"]
		if statusMap, ok := status.(map[string]any); ok {
			status = statusMap["type"]
		}
		if envelope.Type == "session.idle" || envelope.Type == "session.execution.succeeded" || status == "idle" {
			ev.Idle = true
			ev.Done = true
		}
	case "session.error", "session.execution.failed":
		ev.Done = true
		if detail, ok := props["error"].(map[string]any); ok {
			ev.Error, _ = detail["message"].(string)
			if ev.Error == "" {
				ev.Error, _ = detail["detail"].(string)
			}
		}
		if ev.Error == "" && props["message"] != nil {
			ev.Error = fmt.Sprint(props["message"])
		}
		if ev.Error == "" && props["error"] != nil {
			ev.Error = fmt.Sprint(props["error"])
		}
		if ev.Error == "" {
			ev.Error = envelope.Type
		}
	case "permission.asked", "permission.updated":
		ev.Permission = props
		if ev.Session == "" {
			ev.Session, _ = props["sessionID"].(string)
		}
		if _, ok := ev.Permission["id"]; !ok {
			if request, ok := props["request"].(map[string]any); ok {
				ev.Permission = request
			}
		}
		if action, ok := ev.Permission["action"]; ok {
			ev.Permission["title"] = action
		}
		if resources, ok := ev.Permission["resources"].([]any); ok {
			ev.Permission["pattern"] = strings.Join(anyStrings(resources), ", ")
		}
	}
	if ev.Session == "" && !globalEventType(envelope.Type) {
		return // session-less events must not reach every session's subscribers
	}
	if ev.Done {
		c.mu.Lock()
		claimed := c.claimTurn(ev.Session)
		c.mu.Unlock()
		if !claimed {
			return // duplicate terminal for this turn (e.g. idle plus execution.succeeded)
		}
		if ev.Idle {
			c.completeEvent(ctx, &ev, props)
		}
	}
	c.broadcast(ev.Session, ev)
}

// setUsage replaces ev.Usage only when raw carries usage, so a nil or empty
// tokens payload never clobbers usage already read from this event.
func setUsage(ev *runtime.Event, raw any) {
	if u := parseUsage(raw); u != nil {
		ev.Usage = u
	}
}

func eventProperties(eventType string, props map[string]any) map[string]any {
	if eventType != "session.idle" {
		return props
	}
	if id, ok := props["sessionID"].(string); ok {
		return map[string]any{"sessionID": id}
	}
	return props
}

func (c *Client) completeEvent(ctx context.Context, ev *runtime.Event, props map[string]any) {
	if info, ok := props["info"].(map[string]any); ok {
		if ev.Session == "" {
			ev.Session, _ = info["id"].(string)
		}
		setUsage(ev, info["tokens"])
	}
	var payload struct {
		Data []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			Type      string `json:"type"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Tokens json.RawMessage `json:"tokens"`
		} `json:"data"`
	}
	// Bounded so the SSE reader cannot stall; ctx is the stream lifetime, so Close aborts it.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.request(cctx, http.MethodGet, "/api/session/"+url.PathEscape(ev.Session)+"/message?limit=100&order=desc", nil, &payload, false); err == nil {
		var total runtime.Usage
		found := false
		var finalParts []string
		for i := 0; i < len(payload.Data); i++ {
			msg := payload.Data[i]
			if msg.Type == "user" && found {
				break
			}
			if msg.Type != "assistant" {
				continue
			}
			found = true
			var text strings.Builder
			for _, part := range msg.Content {
				if part.Type == "text" {
					text.WriteString(part.Text)
				}
			}
			finalParts = append(finalParts, text.String())
			if usage := parseUsageJSON(msg.Tokens); usage != nil {
				if total.Context == 0 {
					// Newest first: the first message with usage is the last provider call.
					total.Context = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
				}
				total.Input += usage.Input
				total.Output += usage.Output
				total.Reasoning += usage.Reasoning
				total.CacheRead += usage.CacheRead
				total.CacheWrite += usage.CacheWrite
			}
		}
		if found {
			// Oldest first. A turn with tool steps has a text per call (narration, then the answer); keep them
			// as separate paragraphs rather than running one into the next.
			var texts []string
			for i := len(finalParts) - 1; i >= 0; i-- {
				if strings.TrimSpace(finalParts[i]) != "" {
					texts = append(texts, finalParts[i])
				}
			}
			ev.Text += strings.Join(texts, "\n\n")
			ev.JSON = jsonObject(ev.Text)
			ev.Usage = &total
			return
		}
	}
	if ev.Text != "" {
		ev.JSON = jsonObject(ev.Text)
	}
}

func (c *Client) broadcast(session string, ev runtime.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sid, subs := range c.subs {
		if session != "" && sid != session {
			continue
		}
		for _, s := range subs {
			s.queue = append(s.queue, ev)
			select {
			case s.notify <- struct{}{}:
			default:
			}
		}
	}
}

func (c *Client) deliver(s *subscription) {
	defer close(s.ch)
	for {
		select {
		case <-s.stop:
			return
		case <-s.notify:
		}
		for {
			c.mu.Lock()
			if len(s.queue) == 0 {
				c.mu.Unlock()
				break
			}
			ev := s.queue[0]
			s.queue[0] = runtime.Event{}
			s.queue = s.queue[1:]
			c.mu.Unlock()
			select {
			case <-s.stop:
				return
			case s.ch <- ev:
			}
		}
	}
}

func (c *Client) unsub(session string, s *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.subs[session]
	for i, sub := range list {
		if sub == s {
			c.subs[session] = append(list[:i], list[i+1:]...)
			if len(c.subs[session]) == 0 {
				delete(c.subs, session)
			}
			s.once.Do(func() { close(s.stop) })
			if len(c.subs) == 0 && c.cancel != nil {
				c.cancel()
				c.cancel = nil
			}
			return
		}
	}
}

func (c *Client) get(ctx context.Context, path string, dest any) error {
	_, err := c.request(ctx, http.MethodGet, path, nil, dest, false)
	return err
}

func (c *Client) post(ctx context.Context, path string, body any, dest any) error {
	_, err := c.request(ctx, http.MethodPost, path, body, dest, false)
	return err
}

func (c *Client) patch(ctx context.Context, path string, body any) error {
	_, err := c.request(ctx, http.MethodPatch, path, body, nil, false)
	return err
}

func (c *Client) delete(ctx context.Context, path string) error {
	_, err := c.request(ctx, http.MethodDelete, path, nil, nil, false)
	return err
}

func parseUsage(raw any) *runtime.Usage {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return parseUsageJSON(b)
}

func anyStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func parseUsageJSON(b []byte) *runtime.Usage {
	var u struct {
		Input     float64 `json:"input"`
		Output    float64 `json:"output"`
		Reasoning float64 `json:"reasoning"`
		Cache     struct {
			Read  float64 `json:"read"`
			Write float64 `json:"write"`
		} `json:"cache"`
	}
	if len(b) == 0 || json.Unmarshal(b, &u) != nil {
		return nil
	}
	out := &runtime.Usage{Input: int(u.Input), Output: int(u.Output), Reasoning: int(u.Reasoning), CacheRead: int(u.Cache.Read), CacheWrite: int(u.Cache.Write)}
	if *out == (runtime.Usage{}) {
		return nil // null or all-zero payload carries no usage
	}
	return out
}

func jsonObject(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```json") && strings.HasSuffix(trimmed, "```") {
		trimmed = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "```json"), "```"))
	}
	var obj map[string]any
	if len(trimmed) == 0 || json.Unmarshal([]byte(trimmed), &obj) != nil || obj == nil {
		return ""
	}
	return trimmed
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func errString(err error) string {
	if err == nil {
		return "disconnected"
	}
	return err.Error()
}

// ToolInputSummary is the one line that says what a tool call is doing: its command, or the path or pattern it works
// on. It is for a status line, so it is cut short and never holds a newline.
func ToolInputSummary(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"command", "filePath", "path", "pattern", "query", "url"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			line := strings.Join(strings.Fields(v), " ")
			if r := []rune(line); len(r) > 140 {
				line = string(r[:139]) + "…"
			}
			return line
		}
	}
	return ""
}
