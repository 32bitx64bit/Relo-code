package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Session is a flattened target session multiplexed over the browser connection.
type Session struct {
	conn *Conn
	id   string
}

// Attach creates a flattened session for targetID (Target.attachToTarget).
// Flattened sessions carry sessionId on every message over the single pipe.
func (c *Conn) Attach(ctx context.Context, targetID string) (*Session, error) {
	var out struct {
		SessionID string `json:"sessionId"`
	}
	params := map[string]any{"targetId": targetID, "flatten": true}
	if err := c.Call(ctx, "", "Target.attachToTarget", params, &out); err != nil {
		return nil, err
	}
	if out.SessionID == "" {
		return nil, fmt.Errorf("cdp: attach %s returned no sessionId", targetID)
	}
	return &Session{conn: c, id: out.SessionID}, nil
}

// ID returns the flattened session identifier.
func (s *Session) ID() string { return s.id }

// Call sends a command scoped to this session.
func (s *Session) Call(ctx context.Context, method string, params, result any) error {
	return s.conn.Call(ctx, s.id, method, params, result)
}

// Subscribe returns a subscription to this session's events. Call it before the
// command that triggers the event so nothing is missed.
func (s *Session) Subscribe() *Subscription {
	return s.conn.Subscribe(s.id)
}

// WaitEvent blocks until an event named method arrives on sub or ctx ends.
// Pass a subscription created before the triggering command.
func WaitEvent(ctx context.Context, sub *Subscription, method string) (json.RawMessage, error) {
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return nil, ErrClosed
			}
			if ev.Method == method {
				return ev.Params, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Detach closes the flattened session on the browser side.
func (s *Session) Detach(ctx context.Context) error {
	return s.conn.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": s.id}, nil)
}

// Evaluate runs a read-only expression in the page and returns its string value.
// Runtime.evaluate works without enabling the Runtime domain.
func (s *Session) Evaluate(ctx context.Context, expr string) (string, error) {
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	err := s.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &out)
	if err != nil {
		return "", err
	}
	if len(out.ExceptionDetails) > 0 {
		return "", errors.New("cdp: evaluate threw: " + string(out.ExceptionDetails))
	}
	return out.Result.Value, nil
}
