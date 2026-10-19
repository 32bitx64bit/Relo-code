package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// canvasCall is one relo_canvas request waiting for the extension's canvas.reply.
type canvasCall struct {
	id      string
	session string
	reply   chan canvasReply
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

// canvasRequest services one relo_canvas call. The key is validated the same way as peerRequest: it must be bound to
// the live session and the agent's live runtime, and the agent must still be active.
func (c *Coord) canvasRequest(ctx context.Context, req agentmsg.CanvasRequest) (any, error) {
	if !c.canvasOn() {
		return nil, errors.New("Canvas is not supported by the connected editor")
	}
	identity, workspace, mode, err := c.canvasIdentity(req.Key)
	if err != nil {
		return nil, err
	}
	// Creating or changing source is a build-mode operation. Read-style actions stay available in every mode.
	if (req.Action == "create" || req.Action == "update") && mode != ModeBuild {
		return nil, fmt.Errorf("canvas %s is available only in build mode", req.Action)
	}
	call := &canvasCall{id: id.New("canvas"), session: identity.session, reply: make(chan canvasReply, 1)}
	if err := c.reserveCanvas(identity.agent, call); err != nil {
		return nil, err
	}
	defer c.releaseCanvas(call.id)

	payload := map[string]any{
		"requestId": call.id,
		"sessionId": identity.session,
		"agentId":   identity.agent,
		"workspace": workspace,
		"request":   canvasWire{Action: req.Action, ID: req.ID, Name: req.Name, Format: req.Format, Entry: req.Entry, Files: req.Files, Assets: req.Assets, Revision: req.Revision, Width: req.Width, Height: req.Height},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("canvas request is not serializable")
	}
	if len(encoded) > maxCanvasEventBytes {
		return nil, fmt.Errorf("canvas request expands beyond the %d byte event limit; split the source across fewer or simpler files", maxCanvasEventBytes)
	}
	c.emit("canvas.requested", payload)

	ticker := time.NewTicker(canvasPoll)
	defer ticker.Stop()
	for {
		select {
		case r := <-call.reply:
			if !c.canvasStillValid(identity) {
				// A late answer after the session or turn changed must not be treated as the artifact result.
				return nil, errors.New("canvas request ended: session changed")
			}
			if r.err != "" {
				return nil, errors.New(r.err)
			}
			return r.result, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("canvas %s timed out or was cancelled", req.Action)
		case <-ticker.C:
			if !c.canvasStillValid(identity) {
				return nil, errors.New("canvas request ended: session changed or agent turn ended")
			}
		}
	}
}

// canvasIdentity resolves a capability key to the session, the workspace and the mode of the live session.
func (c *Coord) canvasIdentity(key string) (peerIdentity, string, Mode, error) {
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
		return peerIdentity{}, "", "", errors.New("invalid or expired agent canvas key")
	}
	c.runtimeMu.Lock()
	live := c.runtimeSessions[identity.agent]
	active := live != nil && live.id == identity.runtime
	c.runtimeMu.Unlock()
	if !active {
		return peerIdentity{}, "", "", errors.New("agent runtime has ended")
	}
	a, ok := c.Agents.Get(identity.agent)
	if !ok || agent.Terminal(a.Status) {
		return peerIdentity{}, "", "", errors.New("agent is no longer active")
	}
	return identity, workspace, mode, nil
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

// CanvasReply delivers the extension's answer to a pending relo_canvas request. Replies for unknown IDs, or for a
// request whose session is no longer current, are rejected and never reach the model.
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
	c.mu.Lock()
	current := c.sess != nil && c.sess.ID == call.session
	c.mu.Unlock()
	if !current {
		return errors.New("stale canvas request: session changed")
	}
	select {
	case call.reply <- canvasReply{result: result, err: errText}:
		return nil
	default:
		return errors.New("canvas request already answered or abandoned")
	}
}
