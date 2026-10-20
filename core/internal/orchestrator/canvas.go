package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/id"
)

const (
	// maxCanvasConcurrent bounds in-flight canvas requests across the whole Coord. Each one holds an HTTP request
	// open while the extension renders, so the bound also protects the loopback server.
	maxCanvasConcurrent = 8
	// maxCanvasPerMinute bounds how often one agent may call relo_canvas. Reads and writes count alike.
	maxCanvasPerMinute = 30
	maxCanvasErrText   = 2048
	// maxCanvasEventBytes leaves headroom under the extension IPC scanner's 16 MiB line cap. JSON escaping can
	// expand source text, so the limit is applied to the encoded event, not only to the decoded payload.
	maxCanvasEventBytes = 12 << 20
	// canvasPoll re-checks that the session, runtime and key are still live while a request waits.
	canvasPoll = 250 * time.Millisecond
	// maxCanvasAuditReason bounds the refusal text kept in an audit record. The reason names the rule that
	// refused the call; it is not a place for artifact content.
	maxCanvasAuditReason = 512
)

// CanvasAudit is a metadata-only record of one canvas call. It names who asked the extension for what, and
// why a call was refused, so a later claim about who used canvas can be checked against the event log.
// It never carries artifact source, file contents, assets or a tool result.
type CanvasAudit struct {
	Event      string // "canvas.requested" or "canvas.reply"
	Action     string // the canvas action the agent asked for
	Role       string // the role of the calling agent
	Owner      string // the agent id that owns the request
	ArtifactID string // the artifact the call names, when it names one
	RequestID  string // the core request id
	Mode       string // the session mode the call was made in
	Outcome    string // requested, denied, refused, ok, error, rejected or abandoned
	Reason     string // why the call was refused or failed
}

// Payload returns the row stored in the event log. It holds the metadata of one canvas call and nothing
// else: no artifact source, no file contents, no assets and no tool result.
func (a CanvasAudit) Payload() map[string]any {
	payload := map[string]any{
		"event":   a.Event,
		"action":  a.Action,
		"role":    a.Role,
		"owner":   a.Owner,
		"outcome": a.Outcome,
	}
	if a.ArtifactID != "" {
		payload["artifactId"] = a.ArtifactID
	}
	if a.RequestID != "" {
		payload["requestId"] = a.RequestID
	}
	if a.Mode != "" {
		payload["mode"] = a.Mode
	}
	if a.Reason != "" {
		payload["reason"] = a.Reason
	}
	return payload
}

// canvasAuditReason bounds the refusal text kept in an audit record.
func canvasAuditReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > maxCanvasAuditReason {
		return reason[:maxCanvasAuditReason]
	}
	return reason
}

// canvasResultArtifactID reads the artifact id out of an extension answer. The answer may carry the whole
// artifact, source included, so only the id is taken from it.
func canvasResultArtifactID(result any) string {
	m, ok := result.(map[string]any)
	if !ok {
		return ""
	}
	if artifact, ok := m["artifact"].(map[string]any); ok {
		if id, ok := artifact["id"].(string); ok {
			return id
		}
	}
	if id, ok := m["id"].(string); ok {
		return id
	}
	return ""
}

// canvasCall is one relo_canvas request waiting for the extension's canvas.reply. It records the owner of the
// request, so the answer can be checked against the agent that asked for it.
type canvasCall struct {
	id      string
	session string
	// agent is the owner of the request: the agent whose capability key made the call.
	agent string
	// role is the owner's role when the request was made. A reply is refused when the role no longer matches.
	role agent.Role
	// action is the canvas action the owner asked for. A reply is refused when the role may no longer perform it.
	action string
	// artifact is the artifact the request names, when it names one. It is metadata for the audit trail.
	artifact string
	// mode is the session mode the request was made in. It is metadata for the audit trail.
	mode Mode
	// identity is the capability binding of the owner, used to re-check the turn that made the request.
	identity peerIdentity
	reply    chan canvasReply
}

// audit builds the metadata-only record of a canvas call from the pending call.
func (call *canvasCall) audit(event, outcome, reason string) CanvasAudit {
	return CanvasAudit{
		Event:      event,
		Action:     call.action,
		Role:       string(call.role),
		Owner:      call.agent,
		ArtifactID: call.artifact,
		RequestID:  call.id,
		Mode:       string(call.mode),
		Outcome:    outcome,
		Reason:     canvasAuditReason(reason),
	}
}

type canvasReply struct {
	result any
	err    string
}

// canvasWire is what the extension receives. It deliberately has no key field: the capability never leaves core.
type canvasWire struct {
	Action   string                    `json:"action"`
	ID       string                    `json:"id,omitempty"`
	Name     string                    `json:"name,omitempty"`
	Format   string                    `json:"format,omitempty"`
	Entry    string                    `json:"entry,omitempty"`
	Files    map[string]string         `json:"files,omitempty"`
	Assets   map[string]agentmsg.Asset `json:"assets,omitempty"`
	Revision int                       `json:"revision,omitempty"`
	Width    int                       `json:"width,omitempty"`
	Height   int                       `json:"height,omitempty"`
}

// SetCanvasEnabled records whether the connected extension supports Canvas. A change restarts the messaging server
// on its next use, so the relo_canvas catalog entry matches the flag.
func (c *Coord) SetCanvasEnabled(on bool) {
	c.messageMu.Lock()
	if c.CanvasEnabled == on {
		c.messageMu.Unlock()
		return
	}
	c.CanvasEnabled = on
	s := c.messageServer
	c.messageServer = nil
	c.messageMu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// canvasOn reports whether relo_canvas is offered to agents.
func (c *Coord) canvasOn() bool {
	c.messageMu.Lock()
	defer c.messageMu.Unlock()
	return c.CanvasEnabled
}

// SetCanvasAudit installs the audit sink for canvas calls. The sink receives one metadata-only record for
// every canvas request and every canvas reply, so a later claim about who used canvas can be checked
// against the event log. The records never carry artifact source.
func (c *Coord) SetCanvasAudit(fn func(CanvasAudit)) {
	c.canvasAuditMu.Lock()
	c.canvasAuditFn = fn
	c.canvasAuditMu.Unlock()
}

// CanvasAuditInstalled reports whether an audit sink is registered for canvas calls.
func (c *Coord) CanvasAuditInstalled() bool {
	c.canvasAuditMu.RLock()
	defer c.canvasAuditMu.RUnlock()
	return c.canvasAuditFn != nil
}

// auditCanvas hands one metadata-only record to the audit sink, if one is installed.
func (c *Coord) auditCanvas(rec CanvasAudit) {
	c.canvasAuditMu.RLock()
	fn := c.canvasAuditFn
	c.canvasAuditMu.RUnlock()
	if fn == nil {
		return
	}
	fn(rec)
}

// canvasRequest services one relo_canvas call. The key is validated the same way as peerRequest: it must be bound to
// the live session and the agent's live runtime, and the agent must still be active.
func (c *Coord) canvasRequest(ctx context.Context, req agentmsg.CanvasRequest) (any, error) {
	if !c.canvasOn() {
		return nil, errors.New("Canvas is not supported by the connected editor")
	}
	identity, workspace, mode, role, err := c.canvasIdentity(req.Key)
	if err != nil {
		return nil, err
	}
	// The audit record names the caller: the role, the owner and the action. It never names the content.
	audit := CanvasAudit{
		Event:      "canvas.requested",
		Action:     req.Action,
		Role:       string(role),
		Owner:      identity.agent,
		ArtifactID: req.ID,
		Mode:       string(mode),
	}
	// Canvas authoring belongs to builder roles. Every other role, the Primary included, may only inspect, so a
	// denied action is refused here, before the request reaches the extension and before any artifact is written.
	if !agent.CanvasActionAllowed(role, req.Action) {
		refusal := fmt.Errorf("canvas %s is not available to the %s role; canvas authoring is reserved for builder agents", req.Action, role)
		audit.Outcome = "denied"
		audit.Reason = canvasAuditReason(refusal.Error())
		c.auditCanvas(audit)
		return nil, refusal
	}
	// Creating or changing source is a build-mode operation. Read-style actions stay available in every mode.
	if (req.Action == "create" || req.Action == "update") && mode != ModeBuild {
		refusal := fmt.Errorf("canvas %s is available only in build mode", req.Action)
		audit.Outcome = "denied"
		audit.Reason = canvasAuditReason(refusal.Error())
		c.auditCanvas(audit)
		return nil, refusal
	}
	call := &canvasCall{
		id:       id.New("canvas"),
		session:  identity.session,
		agent:    identity.agent,
		role:     role,
		action:   req.Action,
		artifact: req.ID,
		mode:     mode,
		identity: identity,
		reply:    make(chan canvasReply, 1),
	}
	if err := c.reserveCanvas(identity.agent, call); err != nil {
		audit.Outcome = "refused"
		audit.Reason = canvasAuditReason(err.Error())
		c.auditCanvas(audit)
		return nil, err
	}
	defer c.releaseCanvas(call.id)

	// The envelope names the owner and the role so the extension can scope artifacts to the agent that made them.
	// owner is the agent that issued the request; for a create it is the owner of the new artifact. readUnowned
	// tells the extension whether the role may read an artifact that carries no owner: only an authoring role may.
	payload := map[string]any{
		"requestId":   call.id,
		"sessionId":   identity.session,
		"agentId":     identity.agent,
		"role":        string(role),
		"owner":       identity.agent,
		"readUnowned": agent.CanvasReadUnownedRole(role),
		"workspace":   workspace,
		"request":     canvasWire{Action: req.Action, ID: req.ID, Name: req.Name, Format: req.Format, Entry: req.Entry, Files: req.Files, Assets: req.Assets, Revision: req.Revision, Width: req.Width, Height: req.Height},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("canvas request is not serializable")
	}
	if len(encoded) > maxCanvasEventBytes {
		return nil, fmt.Errorf("canvas request expands beyond the %d byte event limit; split the source across fewer or simpler files", maxCanvasEventBytes)
	}
	c.emit("canvas.requested", payload)
	// The request reached the extension: record who asked for what, so the event log shows the call.
	audit.RequestID = call.id
	audit.Outcome = "requested"
	c.auditCanvas(audit)

	ticker := time.NewTicker(canvasPoll)
	defer ticker.Stop()
	for {
		select {
		case r := <-call.reply:
			if !c.canvasStillValid(identity) {
				// A late answer after the session or turn changed must not be treated as the artifact result.
				// The reply itself is already recorded by CanvasReply.
				return nil, errors.New("canvas request ended: session changed")
			}
			if r.err != "" {
				return nil, errors.New(r.err)
			}
			return r.result, nil
		case <-ctx.Done():
			c.auditCanvas(call.audit("canvas.reply", "abandoned", "canvas request timed out or was cancelled"))
			return nil, fmt.Errorf("canvas %s timed out or was cancelled", req.Action)
		case <-ticker.C:
			if !c.canvasStillValid(identity) {
				c.auditCanvas(call.audit("canvas.reply", "abandoned", "canvas request ended: session changed or agent turn ended"))
				return nil, errors.New("canvas request ended: session changed or agent turn ended")
			}
		}
	}
}

// canvasIdentity resolves a capability key to the session, the workspace, the mode and the role of the live session.
func (c *Coord) canvasIdentity(key string) (peerIdentity, string, Mode, agent.Role, error) {
	c.mu.Lock()
	identity, ok := c.peerKeys[key]
	valid := ok && c.sess != nil && c.sess.ID == identity.session
	var workspace string
	var mode Mode
	if valid {
		workspace, mode = c.sess.Workspace, c.sess.Mode
	}
	c.mu.Unlock()
	if !valid {
		return peerIdentity{}, "", "", "", errors.New("invalid or expired agent canvas key")
	}
	c.runtimeMu.Lock()
	live := c.runtimeSessions[identity.agent]
	active := live != nil && live.id == identity.runtime
	c.runtimeMu.Unlock()
	if !active {
		return peerIdentity{}, "", "", "", errors.New("agent runtime has ended")
	}
	a, ok := c.Agents.Get(identity.agent)
	if !ok || agent.Terminal(a.Status) {
		return peerIdentity{}, "", "", "", errors.New("agent is no longer active")
	}
	return identity, workspace, mode, a.Role, nil
}

// canvasStillValid reports whether the session, the runtime binding and the key are all still the ones that issued
// the request. Turn end revokes the key, so a waiting call is released without a reply.
func (c *Coord) canvasStillValid(identity peerIdentity) bool {
	c.mu.Lock()
	sessionOK := c.sess != nil && c.sess.ID == identity.session
	_, keyOK := c.peerKeys[c.keyFor(identity)]
	c.mu.Unlock()
	if !sessionOK || !keyOK {
		return false
	}
	c.runtimeMu.Lock()
	live := c.runtimeSessions[identity.agent]
	active := live != nil && live.id == identity.runtime
	c.runtimeMu.Unlock()
	return active
}

// keyFor finds the capability key bound to identity. It must be called with c.mu held.
func (c *Coord) keyFor(identity peerIdentity) string {
	for key, bound := range c.peerKeys {
		if bound == identity {
			return key
		}
	}
	return ""
}

// reserveCanvas admits a request under the concurrency and per-agent rate limits and records it as pending.
func (c *Coord) reserveCanvas(agentID string, call *canvasCall) error {
	c.canvasMu.Lock()
	defer c.canvasMu.Unlock()
	if len(c.canvasPending) >= maxCanvasConcurrent {
		return errors.New("too many concurrent canvas requests; wait for one to finish")
	}
	now := time.Now()
	recent := c.canvasRate[agentID][:0]
	for _, at := range c.canvasRate[agentID] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= maxCanvasPerMinute {
		c.canvasRate[agentID] = recent
		return errors.New("canvas rate limit reached; consolidate your changes")
	}
	c.canvasRate[agentID] = append(recent, now)
	c.canvasPending[call.id] = call
	return nil
}

func (c *Coord) releaseCanvas(requestID string) {
	c.canvasMu.Lock()
	delete(c.canvasPending, requestID)
	c.canvasMu.Unlock()
}

// CanvasReply delivers the extension's answer to a pending relo_canvas request. The answer is accepted only for a
// request whose owner is still active and still allowed to perform the action it asked for, so a late answer, a
// hijacked request id, or a role that changed in the meantime never reaches the model.
func (c *Coord) CanvasReply(requestID string, result any, errText string) error {
	if requestID == "" {
		return errors.New("canvas reply requires requestId")
	}
	if len(errText) > maxCanvasErrText {
		errText = errText[:maxCanvasErrText]
	}
	c.canvasMu.Lock()
	call := c.canvasPending[requestID]
	if call != nil {
		delete(c.canvasPending, requestID)
	}
	c.canvasMu.Unlock()
	if call == nil {
		return errors.New("unknown or stale canvas request")
	}
	if err := c.canvasReplyAllowed(call); err != nil {
		// The answer is refused. The waiting agent is told why instead of waiting out the request timeout.
		c.auditCanvas(call.audit("canvas.reply", "rejected", err.Error()))
		select {
		case call.reply <- canvasReply{err: err.Error()}:
		default:
		}
		return err
	}
	c.mu.Lock()
	current := c.sess != nil && c.sess.ID == call.session
	c.mu.Unlock()
	if !current {
		c.auditCanvas(call.audit("canvas.reply", "rejected", "stale canvas request: session changed"))
		return errors.New("stale canvas request: session changed")
	}
	select {
	case call.reply <- canvasReply{result: result, err: errText}:
		// The answer is recorded by its metadata: the result itself is never kept.
		outcome, reason := "ok", ""
		if errText != "" {
			outcome, reason = "error", errText
		}
		rec := call.audit("canvas.reply", outcome, reason)
		if rec.ArtifactID == "" {
			rec.ArtifactID = canvasResultArtifactID(result)
		}
		c.auditCanvas(rec)
		return nil
	default:
		c.auditCanvas(call.audit("canvas.reply", "abandoned", "canvas request already answered or abandoned"))
		return errors.New("canvas request already answered or abandoned")
	}
}

// canvasReplyAllowed re-checks the owner of a pending request before its answer is delivered. The owner must still
// exist under the id that made the request, still be active, still hold the role it made the request with, and
// still be allowed to perform the action: a turn that ended, an owner id that no longer matches, or a role that
// changed must not receive an artifact result.
func (c *Coord) canvasReplyAllowed(call *canvasCall) error {
	if call.agent == "" {
		return errors.New("canvas reply rejected: the request has no owner")
	}
	a, ok := c.Agents.Get(call.agent)
	if !ok {
		return errors.New("canvas reply rejected: the requesting agent no longer exists")
	}
	// The owner id is the identity of the request. A record that no longer carries it is not the agent that asked.
	if a.ID != call.agent {
		return fmt.Errorf("canvas reply rejected: the request owner %q no longer matches the agent record %q", call.agent, a.ID)
	}
	if agent.Terminal(a.Status) {
		return errors.New("canvas reply rejected: the requesting agent is no longer active")
	}
	if a.Role != call.role {
		return errors.New("canvas reply rejected: the requesting agent changed role")
	}
	if !agent.CanvasActionAllowed(a.Role, call.action) {
		return fmt.Errorf("canvas reply rejected: the %s role may no longer %s a canvas artifact", a.Role, call.action)
	}
	if !c.canvasStillValid(call.identity) {
		return errors.New("canvas reply rejected: the requesting agent's turn has ended")
	}
	return nil
}
