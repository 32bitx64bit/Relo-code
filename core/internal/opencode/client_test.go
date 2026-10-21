package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/runtime"
)

func TestV2ClientSessionTaskAndEvents(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	prompts := make(chan int, 4)
	promptCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"version": "2.0.27", "pid": 12, "urls": []string{}, "paths": map[string]any{"tmp": "/tmp"}})
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ses_1"}})
	})
	mux.HandleFunc("/api/model", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"location": map[string]string{"directory": "/"}, "data": []any{map[string]any{"id": "openai/gpt", "modelID": "gpt", "providerID": "openai", "name": "GPT", "enabled": true}}})
	})
	mux.HandleFunc("/api/provider", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"location": map[string]string{"directory": "/"}, "data": []any{map[string]any{"id": "openai", "name": "OpenAI"}}})
	})
	mux.HandleFunc("/api/session/ses_1/model", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, "model:"+body["model"].(map[string]any)["id"].(string))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/experimental/session/ses_1/instructions/entries", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	})
	mux.HandleFunc("/api/experimental/session/ses_1/instructions/entries/relo-system", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, "system:"+body["value"].(string))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/session/ses_1", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/session/ses_1/agent", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/session/ses_1/interrupt", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("true"))
	})
	mux.HandleFunc("/api/session/ses_1/prompt", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["text"] != "reply exactly OK" || len(body) != 1 {
			t.Errorf("prompt: %v", body)
		}
		mu.Lock()
		promptCount++
		requests = append(requests, "prompt")
		count := promptCount
		mu.Unlock()
		prompts <- count
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "msg_user"}})
	})
	mux.HandleFunc("/api/session/ses_1/message", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		mu.Lock()
		turn := promptCount
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "msg_a", "sessionID": "ses_1", "type": "assistant", "content": []any{map[string]any{"type": "text", "text": "{\"answer\":\"OK\"}"}}, "tokens": map[string]any{"input": 50 + turn, "output": 3, "reasoning": 0, "cache": map[string]any{"read": 40 * turn, "write": 2}}}}, "cursor": map[string]any{}})
	})
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for {
			select {
			case turn := <-prompts:
				_, _ = fmt.Fprintf(w, "id: evt_%d\ndata: {\"id\":\"evt_%d\",\"type\":\"message.part.updated\",\"data\":{\"sessionID\":\"ses_1\",\"part\":{\"type\":\"text\",\"text\":\"{\\\"answer\\\":\\\"OK\\\"}\"},\"delta\":\"{\\\"answer\\\":\\\"OK\\\"}\"}}\n\n", turn*2-1, turn*2-1)
				fl.Flush()
				_, _ = fmt.Fprintf(w, "id: evt_%d\ndata: {\"id\":\"evt_%d\",\"type\":\"session.idle\",\"data\":{\"sessionID\":\"ses_1\"}}\n\n", turn*2, turn*2)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if h := c.Health(context.Background()); !h.Healthy || h.Version != "2.0.27" {
		t.Fatalf("health: %+v", h)
	}
	id, err := c.StartSession(context.Background(), runtime.SessionConfig{Title: "test", Directory: "/work/tree"})
	if err != nil || id != "ses_1" {
		t.Fatal(err, id)
	}
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].Provider != "openai" || models[0].ID != "gpt" {
		t.Fatalf("models: %+v %v", models, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, err := c.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	task := runtime.AgentTask{Prompt: "reply exactly OK", System: "stable system", Provider: "openai", Model: "gpt", Directory: "/work/tree", Tools: map[string]bool{"read": true, "write": true, "edit": true}}
	for turn := 1; turn <= 3; turn++ {
		if turn == 3 {
			task.System = "updated stable system"
		}
		if err := c.SendTask(ctx, id, task); err != nil {
			t.Fatal(err)
		}
		var terminal runtime.Event
		deadline := time.After(time.Second)
		for !terminal.Done {
			select {
			case ev := <-events:
				if ev.Done {
					terminal = ev
				}
			case <-deadline:
				t.Fatalf("turn %d no terminal event", turn)
			}
		}
		if terminal.Session != "ses_1" || terminal.JSON != `{"answer":"OK"}` || terminal.Text != terminal.JSON {
			t.Fatalf("turn %d terminal: %+v", turn, terminal)
		}
		if terminal.Usage == nil || terminal.Usage.Input != 50+turn || terminal.Usage.CacheRead != 40*turn {
			t.Fatalf("turn %d usage: %+v", turn, terminal.Usage)
		}
	}
	if err := c.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Count(strings.Join(requests, ","), "model:gpt") != 1 || strings.Count(strings.Join(requests, ","), "system:stable system") != 1 || strings.Count(strings.Join(requests, ","), "system:updated stable system") != 1 || strings.Count(strings.Join(requests, ","), "prompt") != 3 {
		t.Fatalf("updates repeated: %v", requests)
	}
}

func TestV2AuthenticationRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"_tag":"UnauthorizedError"}`))
			return
		}
		_, _ = w.Write([]byte(`{"version":"2.0.27"}`))
	}))
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	if h := c.Health(context.Background()); !h.Healthy {
		t.Fatalf("health: %+v", h)
	}
	bad := NewClientWithAuth(srv.URL, "opencode", "wrong")
	if h := bad.Health(context.Background()); h.Error == "" || !strings.Contains(h.Error, "HTTP 401") {
		t.Fatalf("rejected password error: %+v", h)
	}
}

func TestTurnTimeoutInterruptsAndReportsError(t *testing.T) {
	interrupted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/ses_timeout/interrupt" {
			http.NotFound(w, r)
			return
		}
		interrupted <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	s := &subscription{ch: make(chan runtime.Event, 2), stop: make(chan struct{}), notify: make(chan struct{}, 1)}
	c.mu.Lock()
	c.subs["ses_timeout"] = []*subscription{s}
	c.mu.Unlock()
	go c.deliver(s)
	c.startTurnTimer("ses_timeout", 1)
	select {
	case <-interrupted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout did not interrupt session")
	}
	select {
	case ev := <-s.ch:
		if !strings.Contains(ev.Error, "timed out after 1s") {
			t.Fatalf("timeout event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout error event missing")
	}
}

func TestTerminalEventsNeverDropped(t *testing.T) {
	c := NewClient("http://localhost")
	s := &subscription{ch: make(chan runtime.Event, 1), stop: make(chan struct{}), notify: make(chan struct{}, 1)}
	s.ch <- runtime.Event{Type: "progress"}
	c.mu.Lock()
	c.subs["ses"] = []*subscription{s}
	c.mu.Unlock()
	go c.deliver(s)
	c.broadcast("ses", runtime.Event{Done: true, Type: "session.idle"})
	if got := <-s.ch; got.Done {
		t.Fatalf("buffered event unexpectedly replaced: %+v", got)
	}
	select {
	case got := <-s.ch:
		if !got.Done {
			t.Fatalf("terminal event missing: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event dropped")
	}
}

func TestReconnectResumesAfterLastEventID(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	resumed := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		request := requests
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if request == 1 {
			_, _ = w.Write([]byte("id: evt_1\ndata: {\"id\":\"evt_1\",\"type\":\"message.part.updated\",\"data\":{\"sessionID\":\"ses_resume\",\"part\":{\"type\":\"text\",\"text\":\"OK\"},\"delta\":\"OK\"}}\n\n"))
			return
		}
		select { // later reconnects must not block srv.Close
		case resumed <- r.Header.Get("Last-Event-ID"):
		default:
		}
		_, _ = w.Write([]byte("data: {\"id\":\"evt_2\",\"type\":\"session.idle\",\"data\":{\"sessionID\":\"ses_resume\"}}\n\n"))
	}))
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	events, err := c.Events(ctx, "ses_resume")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-events:
			if ev.Done {
				select {
				case id := <-resumed:
					if id != "evt_1" {
						t.Fatalf("Last-Event-ID = %q", id)
					}
				case <-time.After(time.Second):
					t.Fatal("stream did not reconnect")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("terminal event lost after reconnect")
		}
	}
}

func TestJSONObjectAndUsageParsing(t *testing.T) {
	if got := jsonObject("```json\n{\"ok\":true}\n```"); got != `{"ok":true}` {
		t.Fatalf("json object: %q", got)
	}
	if got := jsonObject("[1,2]"); got != "" {
		t.Fatalf("array accepted: %q", got)
	}
	usage := parseUsage(map[string]any{"input": float64(4), "output": float64(3), "reasoning": float64(2), "cache": map[string]any{"read": float64(8), "write": float64(1)}})
	if usage == nil || *usage != (runtime.Usage{Input: 4, Output: 3, Reasoning: 2, CacheRead: 8, CacheWrite: 1}) {
		t.Fatalf("usage: %+v", usage)
	}
}

func authenticated(w http.ResponseWriter, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if ok && user == "opencode" && pass == "secret" {
		return true
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"_tag":"UnauthorizedError"}`))
	return false
}

func TestV2OpenCode2027TextDeltaAndExecutionSucceeded(t *testing.T) {
	prompts := make(chan int, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "2.0.27", "pid": 12, "urls": []string{}, "paths": map[string]any{"tmp": "/tmp"}})
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ses_1"}})
	})
	mux.HandleFunc("/api/session/ses_1/agent", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/session/ses_1/prompt", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["text"] != "reply exactly OK" {
			t.Errorf("prompt body: %v", body)
		}
		prompts <- 1
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "msg_user"}})
	})
	mux.HandleFunc("/api/session/ses_1/message", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		// Newest first: the final answer, an earlier tool step, then the user message that started the turn.
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "msg_a", "sessionID": "ses_1", "type": "assistant", "content": []any{map[string]any{"type": "text", "text": "{\"answer\":\"OK\"}"}}, "tokens": map[string]any{"input": 50, "output": 3, "reasoning": 0, "cache": map[string]any{"read": 40, "write": 2}}},
			map[string]any{"id": "msg_step", "sessionID": "ses_1", "type": "assistant", "content": []any{}, "tokens": map[string]any{"input": 30, "output": 5, "reasoning": 0, "cache": map[string]any{"read": 10, "write": 0}}},
			map[string]any{"id": "msg_user", "sessionID": "ses_1", "type": "user"},
		}})
	})
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		select {
		case <-prompts:
			_, _ = fmt.Fprintf(w, "id: evt_1\ndata: {\"id\":\"evt_1\",\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"ses_1\",\"delta\":\"{\\\"answer\\\":\\\"OK\\\"}\"}}\n\n")
			fl.Flush()
			_, _ = fmt.Fprintf(w, "id: evt_2\ndata: {\"id\":\"evt_2\",\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses_1\"}}\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	id, err := c.StartSession(ctx, runtime.SessionConfig{Title: "test", Directory: "/work"})
	if err != nil || id != "ses_1" {
		t.Fatal(err, id)
	}
	events, err := c.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendTask(ctx, id, runtime.AgentTask{Prompt: "reply exactly OK"}); err != nil {
		t.Fatal(err)
	}
	var terminal runtime.Event
	select {
	case ev := <-events:
		for !ev.Done {
			select {
			case next := <-events:
				ev = next
			case <-ctx.Done():
				t.Fatal("no terminal event")
			}
		}
		terminal = ev
	case <-ctx.Done():
		t.Fatal("no event received")
	}
	if terminal.Session != "ses_1" || terminal.Text != `{"answer":"OK"}` || terminal.JSON != `{"answer":"OK"}` {
		t.Fatalf("terminal: %+v", terminal)
	}
	// Calls add up, but Context is only the last call's prompt, including cached input, excluding output.
	if terminal.Usage == nil || terminal.Usage.Input != 80 || terminal.Usage.Output != 8 || terminal.Usage.Context != 50+40+2 {
		t.Fatalf("usage: %+v", terminal.Usage)
	}
}

// sseServer serves one SSE body per connection; later connections record Last-Event-ID.
func sseServer(t *testing.T, bodies ...string) (*httptest.Server, chan string) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	resumed := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" {
			http.NotFound(w, r) // completion lookups must not consume SSE connections
			return
		}
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if i > 0 {
			select {
			case resumed <- r.Header.Get("Last-Event-ID"):
			default:
			}
		}
		if i < len(bodies) {
			_, _ = w.Write([]byte(bodies[i]))
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, resumed
}

func TestOversizeSSELineDoesNotReplayForever(t *testing.T) {
	big := strings.Repeat("x", sseMaxLine+10)
	body := "id: evt_1\ndata: {\"id\":\"evt_1\",\"type\":\"progress\",\"data\":{\"sessionID\":\"ses_big\",\"delta\":\"" + big + "\"}}\n\n" +
		"id: evt_2\ndata: {\"id\":\"evt_2\",\"type\":\"session.idle\",\"data\":{\"sessionID\":\"ses_big\"}}\n\n"
	srv, _ := sseServer(t, body)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	events, err := c.Events(ctx, "ses_big")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if !ev.Done {
			t.Fatalf("expected terminal after oversize line, got %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("stream stalled on oversize line")
	}
	c.mu.Lock()
	last := c.lastEvent
	c.mu.Unlock()
	if last != "evt_2" {
		t.Fatalf("lastEvent = %q, want evt_2", last)
	}
}

func TestOversizeSSELineAdvancesLastEventID(t *testing.T) {
	big := strings.Repeat("y", sseMaxLine+10)
	body := "id: evt_9\ndata: {\"id\":\"evt_9\",\"type\":\"progress\",\"data\":{\"delta\":\"" + big + "\"}}\n\n"
	srv, resumed := sseServer(t, body, "")
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := c.Events(ctx, "ses_x"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-resumed:
		if id != "evt_9" {
			t.Fatalf("Last-Event-ID after oversize = %q, want evt_9", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reconnect after oversize line")
	}
}

func TestSessionlessEventNotFannedOut(t *testing.T) {
	c := NewClient("http://localhost")
	a := &subscription{ch: make(chan runtime.Event, 4), stop: make(chan struct{}), notify: make(chan struct{}, 1)}
	b := &subscription{ch: make(chan runtime.Event, 4), stop: make(chan struct{}), notify: make(chan struct{}, 1)}
	c.mu.Lock()
	c.subs["ses_a"] = []*subscription{a}
	c.subs["ses_b"] = []*subscription{b}
	c.mu.Unlock()
	go c.deliver(a)
	go c.deliver(b)
	c.dispatch(context.Background(), `{"id":"evt_s","type":"session.idle","data":{}}`)
	select {
	case ev := <-a.ch:
		t.Fatalf("session-less idle delivered to ses_a: %+v", ev)
	case ev := <-b.ch:
		t.Fatalf("session-less idle delivered to ses_b: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	c.dispatch(context.Background(), `{"id":"evt_g","type":"backend.disconnected","data":{}}`)
	select {
	case ev := <-a.ch:
		if ev.Type != "backend.disconnected" {
			t.Fatalf("unexpected: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("global backend event not delivered")
	}
}

func dispatchCollect(t *testing.T, c *Client, session string, raws ...string) []runtime.Event {
	t.Helper()
	s := &subscription{ch: make(chan runtime.Event, 16), stop: make(chan struct{}), notify: make(chan struct{}, 1)}
	c.mu.Lock()
	c.subs[session] = []*subscription{s}
	c.mu.Unlock()
	go c.deliver(s)
	defer s.once.Do(func() { close(s.stop) })
	for _, raw := range raws {
		c.dispatch(context.Background(), raw)
	}
	var out []runtime.Event
	for {
		select {
		case ev := <-s.ch:
			out = append(out, ev)
		case <-time.After(100 * time.Millisecond):
			return out
		}
	}
}

func doneEvents(evs []runtime.Event) []runtime.Event {
	var out []runtime.Event
	for _, ev := range evs {
		if ev.Done {
			out = append(out, ev)
		}
	}
	return out
}

func TestFailedExecutionEmitsSingleDoneWithError(t *testing.T) {
	c := NewClient("http://localhost")
	evs := dispatchCollect(t, c, "ses_f",
		`{"id":"e1","type":"session.execution.failed","data":{"sessionID":"ses_f","error":{"message":"boom"}}}`)
	done := doneEvents(evs)
	if len(done) != 1 || done[0].Error != "boom" {
		t.Fatalf("done events = %+v", evs)
	}
}

func TestSucceededThenIdleEmitsSingleDone(t *testing.T) {
	c := NewClient("http://localhost")
	evs := dispatchCollect(t, c, "ses_d",
		`{"id":"e1","type":"session.execution.succeeded","data":{"sessionID":"ses_d"}}`,
		`{"id":"e2","type":"session.idle","data":{"sessionID":"ses_d"}}`)
	if done := doneEvents(evs); len(done) != 1 {
		t.Fatalf("want exactly one Done, got %d: %+v", len(done), evs)
	}
}

func TestStaleTimerDoesNotCancelNextTurn(t *testing.T) {
	var mu sync.Mutex
	interrupts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/ses_t/interrupt", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		interrupts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("true"))
	})
	mux.HandleFunc("/api/session/ses_t/agent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/session/ses_t/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	// turn 1 times out later; it errors before its timer fires, leaving no timer behind
	c.startTurnTimer("ses_t", 1)
	dispatchCollect(t, c, "ses_t", `{"id":"e1","type":"session.error","data":{"sessionID":"ses_t","message":"x"}}`)
	c.mu.Lock()
	if c.timers["ses_t"] != nil {
		c.mu.Unlock()
		t.Fatal("timer survived terminal event")
	}
	c.timers["ses_t"] = time.AfterFunc(time.Hour, func() {}) // simulate a stale leftover timer
	c.mu.Unlock()
	// turn 2 has no timeout; SendTask must clear the stale timer so it cannot interrupt turn 2
	if err := c.SendTask(context.Background(), "ses_t", runtime.AgentTask{Prompt: "next"}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	if c.timers["ses_t"] != nil {
		c.mu.Unlock()
		t.Fatal("SendTask left stale timer")
	}
	c.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if interrupts != 0 {
		t.Fatalf("stale timer interrupted newer turn: %d", interrupts)
	}
}

func TestNilUsageDoesNotOverwrite(t *testing.T) {
	c := NewClient("http://localhost")
	if u := parseUsage(nil); u != nil {
		t.Fatalf("parseUsage(nil) = %+v", u)
	}
	if u := parseUsage(map[string]any{"input": float64(0)}); u != nil {
		t.Fatalf("zero usage not nil: %+v", u)
	}
	evs := dispatchCollect(t, c, "ses_u",
		`{"id":"u1","type":"message.updated","data":{"info":{"sessionID":"ses_u","tokens":{"input":7,"output":2}}}}`,
		`{"id":"u2","type":"message.updated","data":{"info":{"sessionID":"ses_u","tokens":null}}}`)
	var last *runtime.Usage
	for _, ev := range evs {
		if ev.Usage != nil {
			last = ev.Usage
		}
	}
	if last == nil || last.Input != 7 || last.Output != 2 {
		t.Fatalf("usage clobbered: %+v", evs)
	}
}

func TestCloseDuringCompleteEventReturnsPromptly(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // stall the message lookup; only client Close can unblock the reader
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.dispatch(ctx, `{"id":"i1","type":"session.idle","data":{"sessionID":"ses_c"}}`)
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch still blocked after Close")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
}

// effortServer records every model POST body and serves SendTask's other endpoints.
func effortServer(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var posted []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/ses_e/model", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posted = append(posted, body)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/session/ses_e/agent", func(w http.ResponseWriter, r *http.Request) {
		if authenticated(w, r) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/session/ses_e/prompt", func(w http.ResponseWriter, r *http.Request) {
		if authenticated(w, r) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &posted
}

func TestSendTaskPostsEffortVariantAndResets(t *testing.T) {
	srv, posted := effortServer(t)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	ctx := context.Background()
	task := runtime.AgentTask{Provider: "openai", Model: "gpt", Prompt: "hi"}

	// same model, effort "high": must re-post because the effort changed from "".
	task.Effort = "high"
	if err := c.SendTask(ctx, "ses_e", task); err != nil {
		t.Fatal(err)
	}
	// same model and effort again: no re-post.
	if err := c.SendTask(ctx, "ses_e", task); err != nil {
		t.Fatal(err)
	}
	// effort cleared: re-post without variant to reset.
	task.Effort = ""
	if err := c.SendTask(ctx, "ses_e", task); err != nil {
		t.Fatal(err)
	}
	if len(*posted) != 2 {
		t.Fatalf("model posts = %d, want 2: %v", len(*posted), *posted)
	}
	first := (*posted)[0]["model"].(map[string]any)
	if first["variant"] != "high" || first["id"] != "gpt" || first["providerID"] != "openai" {
		t.Fatalf("first post = %v, want variant high", first)
	}
	reset := (*posted)[1]["model"].(map[string]any)
	if _, ok := reset["variant"]; ok {
		t.Fatalf("reset post still has variant: %v", reset)
	}
}

func TestModelsParsesLimitsVariantsAndFamily(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/model", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"modelID": "gem", "providerID": "google", "name": "Gem", "enabled": true, "family": "gemini",
				"limit":        map[string]any{"context": 1048576, "output": 65536},
				"variants":     []any{map[string]any{"id": "none"}, map[string]any{"id": "low"}, map[string]any{"id": ""}, map[string]any{"id": "high"}},
				"capabilities": map[string]any{"input": []any{"text", "image", "pdf"}, "output": []any{"text"}}},
			map[string]any{"modelID": "blind", "providerID": "google", "enabled": true, "capabilities": map[string]any{"input": []any{"text"}}},
			map[string]any{"modelID": "silent", "providerID": "google", "enabled": true},
			map[string]any{"modelID": "disabled", "providerID": "google", "enabled": false},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v, want only enabled", models)
	}
	m := models[0]
	if m.Context != 1048576 || m.Output != 65536 || fmt.Sprint(m.Efforts) != "[low high]" {
		t.Fatalf("parsed model = %+v", m)
	}
	if got := fmt.Sprint(m.Vision, "/", m.VisionSource, " ", models[1].Vision, "/", models[1].VisionSource, " ", models[2].Vision, "/", models[2].VisionSource); got != "yes/catalog no/catalog /" {
		t.Fatalf("capabilities.input read as %s", got)
	}
}

func TestModelsCrossFillsCustomProxyFromBuiltin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/model", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"modelID": "claude-opus-4", "providerID": "anthropic", "name": "Opus", "enabled": true, "family": "claude",
				"limit": map[string]any{"context": 200000, "output": 32000}, "variants": []any{map[string]any{"id": "high"}}},
			map[string]any{"modelID": "claude-opus-4", "providerID": "relo-proxy", "name": "Proxy", "enabled": true, "family": "claude",
				"limit": map[string]any{"context": 0, "output": 8000}},
			map[string]any{"modelID": "claude-opus-4", "providerID": "unrelated", "name": "Other", "enabled": true,
				"limit": map[string]any{"context": 0, "output": 0}},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byProv := map[string]runtime.Model{}
	for _, m := range models {
		byProv[m.Provider] = m
	}
	proxy := byProv["relo-proxy"]
	if proxy.Context != 200000 || proxy.Output != 8000 || fmt.Sprint(proxy.Efforts) != "[high]" {
		t.Fatalf("proxy = %+v, want context/efforts from anthropic and own output", proxy)
	}
	if other := byProv["unrelated"]; other.Context != 0 || len(other.Efforts) != 0 {
		t.Fatalf("family-less model was filled: %+v", other)
	}
	if b := byProv["anthropic"]; b.Context != 200000 || b.Output != 32000 {
		t.Fatalf("builtin changed: %+v", b)
	}
}

func TestCrossFillOnlyFillsEmptyFields(t *testing.T) {
	models := []runtime.Model{
		{ID: "m", Provider: "p", Context: 10, Output: 0},
		{ID: "m", Provider: "builtin", Context: 999, Output: 555, Efforts: []string{"low"}},
		{ID: "m", Provider: "other", Context: 777, Output: 444, Efforts: []string{"max"}},
	}
	fam := map[string]string{"p/m": "f", "builtin/m": "f", "other/m": "f"}
	out := CrossFill(models, fam)
	if out[0].Context != 10 || out[0].Output != 555 || fmt.Sprint(out[0].Efforts) != "[low]" {
		t.Fatalf("filled = %+v, want Context kept 10, Output/Efforts from first donor", out[0])
	}
	if models[0].Output != 0 || models[0].Efforts != nil {
		t.Fatalf("input mutated: %+v", models[0])
	}
	// no family on the model: untouched.
	if got := CrossFill(models, map[string]string{"builtin/m": "f"}); got[0].Output != 0 {
		t.Fatalf("family-less model filled: %+v", got[0])
	}
}

// An interrupt is confirmed by session.execution.interrupted, which is not a terminal event: the session carries
// on, and a stalled agent is resumed on it. Making it terminal would end the resumed turn before it began.
func TestAnInterruptIsConfirmedButIsNotTheEndOfTheTurn(t *testing.T) {
	c := NewClient("http://localhost")
	evs := dispatchCollect(t, c, "ses_i",
		`{"id":"e1","type":"session.tool.failed","data":{"sessionID":"ses_i","id":"call_1"}}`,
		`{"id":"e2","type":"session.execution.interrupted","data":{"sessionID":"ses_i"}}`,
	)
	if len(evs) != 2 {
		t.Fatalf("got %d events: %+v", len(evs), evs)
	}
	if !evs[0].ToolDone || evs[0].ToolID != "call_1" {
		t.Errorf("the failed call was not reported as finished: %+v", evs[0])
	}
	if !evs[1].Interrupted || evs[1].Done || evs[1].Idle || evs[1].Error != "" {
		t.Errorf("interrupt event = %+v, want Interrupted only", evs[1])
	}
}

func TestAToolCallsCommandReachesTheStatusLine(t *testing.T) {
	c := NewClient("http://localhost")
	evs := dispatchCollect(t, c, "ses_c",
		`{"id":"e1","type":"session.tool.input.started","data":{"sessionID":"ses_c","id":"call_1","name":"shell"}}`,
		`{"id":"e2","type":"session.tool.called","data":{"sessionID":"ses_c","id":"call_1","input":{"command":"ctest -j8 --timeout 300\n  --output-on-failure","description":"run the suite"}}}`,
		`{"id":"e3","type":"message.part.updated","data":{"sessionID":"ses_c","part":{"type":"tool","tool":"read","callID":"call_2","state":{"status":"running","input":{"filePath":"engine/src/a.c","offset":10}}}}}`,
	)
	if len(evs) != 3 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[1].ToolInput != "ctest -j8 --timeout 300 --output-on-failure" || evs[1].ToolID != "call_1" {
		t.Errorf("shell call = %+v", evs[1])
	}
	if evs[2].ToolInput != "engine/src/a.c" || evs[2].Tool != "read" {
		t.Errorf("read call = %+v", evs[2])
	}
	if long := ToolInputSummary(map[string]any{"command": strings.Repeat("x", 500)}); len([]rune(long)) > 140 {
		t.Errorf("a %d-character command was not cut", len([]rune(long)))
	}
	if ToolInputSummary(nil) != "" || ToolInputSummary(map[string]any{"other": "x"}) != "" {
		t.Error("an input with nothing to show produced text")
	}
}
