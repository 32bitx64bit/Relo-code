package ipc

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader(`{"v":1,"kind":"req","id":"1","method":"ping","params":{"x":1}}` + "\n")
	srv := New(in, &out, func(method string, params json.RawMessage) (any, error) {
		if method != "ping" {
			t.Fatalf("method %s", method)
		}
		return map[string]any{"ok": true}, nil
	})
	if err := srv.Serve(); err != nil {
		t.Fatal(err)
	}
	var f Frame
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &f); err != nil {
		t.Fatal(err, out.String())
	}
	if f.Kind != KindRes || f.ID != "1" {
		t.Fatalf("%+v", f)
	}
}

func TestConcurrentRequestsAndShutdownDrain(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader(strings.Join([]string{
		`{"v":1,"kind":"req","id":"slow","method":"slow","params":{}}`,
		`{"v":1,"kind":"req","id":"fast","method":"fast","params":{}}`,
		`{"v":1,"kind":"req","id":"stop","method":"shutdown","params":{}}`,
	}, "\n") + "\n")
	var once sync.Once
	srv := New(in, &out, func(method string, _ json.RawMessage) (any, error) {
		if method == "slow" {
			time.Sleep(20 * time.Millisecond)
		}
		once.Do(func() {})
		return method, nil
	})
	if err := srv.Serve(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 drained responses, got %d: %s", len(lines), out.String())
	}
	ids := map[string]bool{}
	for _, line := range lines {
		var f Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal(err)
		}
		ids[f.ID] = true
	}
	for _, id := range []string{"slow", "fast", "stop"} {
		if !ids[id] {
			t.Fatalf("missing response for %s: %s", id, out.String())
		}
	}
}

func TestInvalidFrameReturnsErrorAndContinues(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("not json\n" + `{"v":9,"kind":"req","id":"bad","method":"ping","params":{}}` + "\n" + `{"v":1,"kind":"req","id":"ok","method":"ping","params":{}}` + "\n")
	srv := New(in, &out, func(string, json.RawMessage) (any, error) { return true, nil })
	if err := srv.Serve(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), `"kind":"err"`) != 2 || !strings.Contains(out.String(), `"id":"ok"`) {
		t.Fatalf("bad frame handling: %s", out.String())
	}
}

func TestEmit(t *testing.T) {
	var out bytes.Buffer
	srv := New(strings.NewReader(""), &out, nil)
	if err := srv.Emit("agent.started", map[string]any{"id": "a1"}); err != nil {
		t.Fatal(err)
	}
	var f Frame
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &f); err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindEvt || f.Method != "agent.started" {
		t.Fatalf("%+v", f)
	}
}

// A handler bug must come back as an error frame; the process, and the requests after it, carry on.
func TestHandlerPanicBecomesErrorFrameAndServerSurvives(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader(strings.Join([]string{
		`{"v":1,"kind":"req","id":"bad","method":"boom","params":{}}`,
		`{"v":1,"kind":"req","id":"stop","method":"shutdown","params":{}}`,
	}, "\n") + "\n")
	srv := New(in, &out, func(method string, _ json.RawMessage) (any, error) {
		if method == "boom" {
			var m map[string]int
			m["x"] = 1 // assignment to entry in nil map, the crash that took the core down in production
		}
		return map[string]any{"ok": true}, nil
	})
	if err := srv.Serve(); err != nil {
		t.Fatal(err)
	}
	frames := map[string]Frame{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal(err, line)
		}
		frames[f.ID] = f
	}
	if f := frames["bad"]; f.Kind != KindErr || f.Error == nil || f.Error.Code != "internal" || !strings.Contains(f.Error.Message, "nil map") {
		t.Fatalf("panic reply = %+v", f)
	}
	if f := frames["stop"]; f.Kind != KindRes {
		t.Fatalf("server did not answer after the panic: %+v", f)
	}
}
