package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

// fakeBrowser answers NUL-delimited commands on the other end of two io.Pipes.
type fakeBrowser struct {
	t *testing.T
	// reads commands from the client; writes frames back to it.
	in  *io.PipeReader
	out *io.PipeWriter
}

func newFakePair(t *testing.T) (*Conn, *fakeBrowser) {
	t.Helper()
	cmdR, cmdW := io.Pipe()   // client -> browser
	respR, respW := io.Pipe() // browser -> client
	fb := &fakeBrowser{t: t, in: cmdR, out: respW}
	go fb.serve()
	c := NewConn(cmdW, respR)
	t.Cleanup(func() {
		_ = c.Close()
		_ = cmdR.Close()
		_ = respW.Close()
	})
	return c, fb
}

func (fb *fakeBrowser) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fb.t.Error(err)
		return
	}
	_, _ = fb.out.Write(append(b, 0))
}

func (fb *fakeBrowser) serve() {
	br := bufio.NewReader(fb.in)
	for {
		frame, err := br.ReadBytes(0)
		if err != nil {
			return
		}
		var cmd struct {
			ID        int64           `json:"id"`
			Method    string          `json:"method"`
			SessionID string          `json:"sessionId"`
			Params    json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame[:len(frame)-1], &cmd); err != nil {
			fb.t.Errorf("bad frame %q: %v", frame, err)
			return
		}
		switch cmd.Method {
		case "Target.attachToTarget":
			fb.send(map[string]any{"id": cmd.ID, "result": map[string]any{"sessionId": "S1"}})
		case "Echo":
			fb.send(map[string]any{"id": cmd.ID, "result": map[string]any{"echo": cmd.Params}})
		case "Fail":
			fb.send(map[string]any{"id": cmd.ID, "error": map[string]any{"code": -32000, "message": "boom"}})
		case "Ping":
			// Session event first, then the reply: the subscriber must see it.
			fb.send(map[string]any{"method": "Page.loadEventFired", "sessionId": cmd.SessionID, "params": map[string]any{"n": 1}})
			fb.send(map[string]any{"method": "Target.targetCreated", "params": map[string]any{}})
			fb.send(map[string]any{"id": cmd.ID, "result": map[string]any{}})
		case "Hang":
			// never answers
		}
	}
}

func TestCallRoundTripAndSession(t *testing.T) {
	c, _ := newFakePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := c.Attach(ctx, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "S1" {
		t.Fatalf("session id = %q", s.ID())
	}
	var out struct {
		Echo struct {
			X int `json:"x"`
		} `json:"echo"`
	}
	if err := s.Call(ctx, "Echo", map[string]any{"x": 7}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Echo.X != 7 {
		t.Fatalf("echo x = %d, want 7", out.Echo.X)
	}
}

func TestProtocolErrorIsReturned(t *testing.T) {
	c, _ := newFakePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Call(ctx, "", "Fail", nil, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Message != "boom" || pe.Code != -32000 {
		t.Fatalf("err = %v, want *Error boom", err)
	}
}

func TestEventsRouteBySession(t *testing.T) {
	c, _ := newFakePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := c.Attach(ctx, "T1")
	if err != nil {
		t.Fatal(err)
	}
	sessSub := s.Subscribe()
	defer sessSub.Unsubscribe()
	browserSub := c.Subscribe("")
	defer browserSub.Unsubscribe()

	if err := s.Call(ctx, "Ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := WaitEvent(ctx, sessSub, "Page.loadEventFired"); err != nil {
		t.Fatalf("session event: %v", err)
	}
	ev, err := WaitEvent(ctx, browserSub, "Target.targetCreated")
	if err != nil || ev == nil {
		t.Fatalf("browser event: %v", err)
	}
	// The session-scoped event must not leak into the browser-level subscription.
	select {
	case e := <-browserSub.C:
		if e.Method == "Page.loadEventFired" {
			t.Fatalf("session event leaked to browser subscription")
		}
	default:
	}
}

func TestCallHonoursContext(t *testing.T) {
	c, _ := newFakePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "", "Hang", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestCloseFailsPendingCalls(t *testing.T) {
	c, _ := newFakePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- c.Call(ctx, "", "Hang", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	_ = c.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call not released by Close")
	}
	if err := c.Call(ctx, "", "Echo", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close err = %v", err)
	}
}
