package ipc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/relocode/relo/internal/id"
)

const Version = 1

type Kind string

const (
	KindReq Kind = "req"
	KindRes Kind = "res"
	KindErr Kind = "err"
	KindEvt Kind = "evt"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type Frame struct {
	V      int             `json:"v"`
	Kind   Kind            `json:"kind"`
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result any             `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

type Handler func(method string, params json.RawMessage) (any, error)

type Server struct {
	in      io.Reader
	out     io.Writer
	handler Handler
	mu      sync.Mutex
	closed  atomic.Bool
	running atomic.Bool
	queue   *frameQueue
}

func New(in io.Reader, out io.Writer, h Handler) *Server {
	return &Server{in: in, out: out, handler: h}
}

func (s *Server) Serve() error {
	if !s.running.CompareAndSwap(false, true) {
		return fmt.Errorf("IPC server already serving")
	}
	q := &frameQueue{out: s.out, limit: 128}
	q.cond = sync.NewCond(&q.mu)
	s.mu.Lock()
	s.queue = q
	s.mu.Unlock()
	writerDone := make(chan error, 1)
	go func() { writerDone <- q.run() }()
	var handlers sync.WaitGroup
	sem := make(chan struct{}, 32)
	sc := bufio.NewScanner(s.in)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			_ = s.write(Frame{V: Version, Kind: KindErr, Error: &Error{Code: "invalid_frame", Message: "frame must be valid JSON"}})
			continue
		}
		if f.V != Version {
			s.reject(f.ID, "invalid_version", fmt.Sprintf("unsupported protocol version %d", f.V))
			continue
		}
		if f.Kind != KindReq || f.ID == "" || f.Method == "" {
			s.reject(f.ID, "invalid_request", "request requires kind=req, id, and method")
			continue
		}
		if len(f.Params) > 0 && bytes.TrimSpace(f.Params)[0] != '{' {
			s.reject(f.ID, "invalid_params", "params must be an object")
			continue
		}
		if f.Method == "shutdown" {
			handlers.Wait()
			s.handle(f)
			break
		}
		sem <- struct{}{}
		handlers.Add(1)
		go func(f Frame) {
			defer handlers.Done()
			defer func() { <-sem }()
			s.handle(f)
		}(f)
	}
	shuttingDown := s.closed.Load()
	s.closed.Store(true)
	handlers.Wait()
	q.close()
	writeErr := <-writerDone
	if sc.Err() != nil && !shuttingDown {
		return sc.Err()
	}
	return writeErr
}

func (s *Server) Shutdown() {
	s.closed.Store(true)
	if closer, ok := s.in.(io.Closer); ok {
		_ = closer.Close()
	}
}

func (s *Server) handle(f Frame) {
	// A bug in one request must not take the whole core down: every running agent and the OpenCode child would go
	// with it, and the extension would restart into the same bug. Report it to the caller instead.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic in %s: %v\n%s\n", f.Method, r, debug.Stack())
			_ = s.write(Frame{V: Version, Kind: KindErr, ID: f.ID, Error: &Error{Code: "internal", Message: fmt.Sprintf("internal error in %s: %v", f.Method, r)}})
		}
	}()
	res, err := s.handler(f.Method, f.Params)
	if err != nil {
		_ = s.write(Frame{V: Version, Kind: KindErr, ID: f.ID, Error: &Error{Code: codeOf(err), Message: err.Error()}})
		return
	}
	_ = s.write(Frame{V: Version, Kind: KindRes, ID: f.ID, Result: res})
}

func (s *Server) Emit(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.write(Frame{V: Version, Kind: KindEvt, ID: id.New("evt"), Method: method, Params: b})
}

func (s *Server) write(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	s.mu.Lock()
	q := s.queue
	s.mu.Unlock()
	if q != nil {
		qf := queuedFrame{data: append(b, '\n'), kind: f.Kind, method: f.Method}
		if f.Kind == KindEvt {
			qf.key, _ = coalesceKey(f.Method, f.Params)
		}
		return q.enqueue(qf)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return io.ErrClosedPipe
	}
	_, err = s.out.Write(append(b, '\n'))
	if err != nil {
		return err
	}
	if flusher, ok := s.out.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return err
		}
	}
	if syncer, ok := s.out.(interface{ Sync() error }); ok {
		if err := syncer.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) reject(id, code, message string) {
	_ = s.write(Frame{V: Version, Kind: KindErr, ID: id, Error: &Error{Code: code, Message: message}})
}

// lossyEvents are state snapshots or streams that a newer frame supersedes, so they are
// the first candidates for eviction when the queue is full.
var lossyEvents = map[string]bool{
	"chat.delta":            true,
	"agent.updated":         true,
	"agent.activity":        true,
	"usage.updated":         true,
	"context.created":       true,
	"context.refreshed":     true,
	"context.compressed":    true,
	"context.replaced":      true,
	"context.item.promoted": true,
	"context.item.dropped":  true,
	"log":                   true,
}

// coalesceKey returns a key identifying events whose newest payload fully replaces any
// queued, not-yet-sent event with the same key. Only these event names coalesce.
func coalesceKey(name string, payload json.RawMessage) (string, bool) {
	field := map[string]string{"chat.delta": "agentId", "agent.updated": "id", "usage.updated": "agentId"}[name]
	if field == "" || len(payload) == 0 {
		return "", false
	}
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return "", false
	}
	v, ok := m[field].(string)
	if !ok || v == "" {
		return "", false
	}
	return name + "\x00" + v, true
}

type queuedFrame struct {
	data   []byte
	kind   Kind
	method string
	key    string // coalesceKey result; empty when the frame does not coalesce
}

type frameQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	frames []queuedFrame
	limit  int
	closed bool
	err    error
	out    io.Writer
}

// evictionIndex picks the event frame to drop when the queue is full: oldest lossy event
// first, then oldest event that is not chat.message, then oldest event. Responses/errors
// are never candidates. Returns -1 when no event frame is queued.
func (q *frameQueue) evictionIndex() int {
	for i, old := range q.frames {
		if old.kind == KindEvt && lossyEvents[old.method] {
			return i
		}
	}
	for i, old := range q.frames {
		if old.kind == KindEvt && old.method != "chat.message" {
			return i
		}
	}
	for i, old := range q.frames {
		if old.kind == KindEvt {
			return i
		}
	}
	return -1
}

// enqueue appends f. A coalescing frame replaces the queued frame with the same key by
// removing it and appending f at the tail (latest state keeps queue order). When full,
// event frames evict per evictionIndex; request/response frames wait for space.
func (q *frameQueue) enqueue(f queuedFrame) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return io.ErrClosedPipe
	}
	if q.err != nil {
		return q.err
	}
	if f.key != "" {
		for i, old := range q.frames {
			if old.key == f.key {
				q.frames = append(q.frames[:i], q.frames[i+1:]...)
				break
			}
		}
	}
	for len(q.frames) >= q.limit && !q.closed && q.err == nil {
		if f.kind == KindEvt {
			i := q.evictionIndex()
			if i < 0 {
				return fmt.Errorf("IPC event queue full")
			}
			q.frames = append(q.frames[:i], q.frames[i+1:]...)
			break
		}
		q.cond.Wait()
	}
	if q.closed {
		return io.ErrClosedPipe
	}
	if q.err != nil {
		return q.err
	}
	q.frames = append(q.frames, f)
	q.cond.Signal()
	return nil
}

func (q *frameQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *frameQueue) run() error {
	bw := bufio.NewWriter(q.out)
	for {
		q.mu.Lock()
		for len(q.frames) == 0 && !q.closed && q.err == nil {
			q.cond.Wait()
		}
		if q.err != nil {
			err := q.err
			q.mu.Unlock()
			return err
		}
		if len(q.frames) == 0 && q.closed {
			q.mu.Unlock()
			return bw.Flush()
		}
		f := q.frames[0]
		q.frames[0] = queuedFrame{}
		q.frames = q.frames[1:]
		q.cond.Broadcast()
		q.mu.Unlock()
		if _, err := bw.Write(f.data); err != nil {
			q.fail(err)
			return err
		}
		if err := bw.Flush(); err != nil {
			q.fail(err)
			return err
		}
	}
}

func (q *frameQueue) fail(err error) {
	q.mu.Lock()
	q.err = err
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

type coded interface{ Code() string }

func codeOf(err error) string {
	if c, ok := err.(coded); ok {
		return c.Code()
	}
	return "error"
}

type Fail struct {
	C string
	M string
}

func (e Fail) Error() string { return e.M }
func (e Fail) Code() string  { return e.C }

func Decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	err := json.Unmarshal(raw, &v)
	return v, err
}

func MustParams(raw json.RawMessage) map[string]any {
	v, _ := Decode[map[string]any](raw)
	if v == nil {
		return map[string]any{}
	}
	return v
}

func Str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return fmt.Sprint(nilish(m[k]))
}

func nilish(v any) any {
	if v == nil {
		return ""
	}
	return v
}
