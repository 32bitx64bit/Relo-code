// Package cdp speaks the Chrome DevTools Protocol over --remote-debugging-pipe.
//
// Chromium reads NUL-terminated JSON commands from fd 3 and writes NUL-terminated
// JSON responses and events to fd 4. No TCP port is opened, so nothing listens on
// the network and no DevTools endpoint is discoverable by other local processes.
package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by calls made on, or pending on, a closed Conn.
var ErrClosed = errors.New("cdp: connection closed")

// Error is a protocol error returned by the browser for a command.
type Error struct {
	Code    int
	Message string
	Data    string
}

func (e *Error) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp: %s (%d): %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("cdp: %s (%d)", e.Message, e.Code)
}

// Event is a protocol event. SessionID is empty for browser-level events.
type Event struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}

type reply struct {
	result json.RawMessage
	err    error
}

// Conn is one CDP pipe connection. It is safe for concurrent use.
type Conn struct {
	w  io.WriteCloser // parent end of fd 3
	r  io.ReadCloser  // parent end of fd 4
	wm sync.Mutex     // serialises writes

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan reply
	subs    map[*Subscription]struct{}
	closed  bool

	done chan struct{} // closed when the read loop exits
}

// NewConn starts the read loop over r and writes commands to w.
func NewConn(w io.WriteCloser, r io.ReadCloser) *Conn {
	c := &Conn{
		w:       w,
		r:       r,
		pending: make(map[int64]chan reply),
		subs:    make(map[*Subscription]struct{}),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Done is closed once the read loop has exited (browser gone or Close called).
func (c *Conn) Done() <-chan struct{} { return c.done }

// Call sends a command and decodes its result into result (may be nil).
// sessionID is empty for browser-level commands.
func (c *Conn) Call(ctx context.Context, sessionID, method string, params, result any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("cdp: marshal %s params: %w", method, err)
		}
		raw = b
	}
	id := c.nextID.Add(1)
	ch := make(chan reply, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	msg := message{ID: id, Method: method, SessionID: sessionID, Params: raw}
	if err := c.write(msg); err != nil {
		c.drop(id)
		return err
	}

	select {
	case rp := <-ch:
		if rp.err != nil {
			return rp.err
		}
		if result != nil && len(rp.result) > 0 {
			if err := json.Unmarshal(rp.result, result); err != nil {
				return fmt.Errorf("cdp: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.drop(id)
		return ctx.Err()
	}
}

func (c *Conn) write(m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("cdp: marshal %s: %w", m.Method, err)
	}
	b = append(b, 0)
	c.wm.Lock()
	defer c.wm.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return fmt.Errorf("cdp: write %s: %w", m.Method, err)
	}
	return nil
}

func (c *Conn) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) readLoop() {
	defer close(c.done)
	br := bufio.NewReaderSize(c.r, 64<<10)
	for {
		frame, err := br.ReadBytes(0)
		if len(frame) > 1 {
			var m message
			if jerr := json.Unmarshal(frame[:len(frame)-1], &m); jerr == nil {
				c.dispatch(m)
			}
		}
		if err != nil {
			break
		}
	}
	c.shutdown()
}

func (c *Conn) dispatch(m message) {
	if m.ID != 0 {
		c.mu.Lock()
		ch, ok := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ok {
			rp := reply{result: m.Result}
			if m.Error != nil {
				rp.err = m.Error
			}
			ch <- rp
		}
		return
	}
	if m.Method == "" {
		return
	}
	ev := Event{SessionID: m.SessionID, Method: m.Method, Params: m.Params}
	c.mu.Lock()
	for s := range c.subs {
		if s.sessionID == ev.SessionID {
			s.push(ev)
		}
	}
	c.mu.Unlock()
}

// Close ends the connection. Closing the write end makes Chromium exit cleanly.
func (c *Conn) Close() error {
	c.shutdown()
	err := c.w.Close()
	_ = c.r.Close()
	return err
}

func (c *Conn) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[int64]chan reply)
	subs := c.subs
	c.subs = make(map[*Subscription]struct{})
	c.mu.Unlock()

	for _, ch := range pending {
		ch <- reply{err: ErrClosed}
	}
	for s := range subs {
		s.close()
	}
}

// Subscription receives events for one session ("" for browser-level events).
// Events are queued without bound so the read loop never blocks on a slow reader;
// call Unsubscribe when done.
type Subscription struct {
	C <-chan Event

	c         *Conn
	sessionID string
	out       chan Event
	mu        sync.Mutex
	queue     []Event
	wake      chan struct{}
	stop      chan struct{}
	stopOnce  sync.Once
}

// Subscribe returns a subscription to events for sessionID.
func (c *Conn) Subscribe(sessionID string) *Subscription {
	s := &Subscription{
		c:         c,
		sessionID: sessionID,
		out:       make(chan Event),
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
	}
	s.C = s.out
	go s.pump()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		s.close()
		return s
	}
	c.subs[s] = struct{}{}
	c.mu.Unlock()
	return s
}

// Unsubscribe stops delivery and releases the subscription's goroutine.
func (s *Subscription) Unsubscribe() {
	s.c.mu.Lock()
	delete(s.c.subs, s)
	s.c.mu.Unlock()
	s.close()
}

func (s *Subscription) push(ev Event) {
	s.mu.Lock()
	s.queue = append(s.queue, ev)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Subscription) close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *Subscription) pump() {
	defer close(s.out)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.stop:
				return
			}
		}
		ev := s.queue[0]
		s.queue[0] = Event{}
		s.queue = s.queue[1:]
		s.mu.Unlock()
		select {
		case s.out <- ev:
		case <-s.stop:
			return
		}
	}
}
