package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Shapes taken from a real OpenCode 2.0.29 run. session.usage.updated carries the running total of the turn, so
// only the step's own event says how much the latest provider call held.
func TestAStepEndReportsWhatTheCallHeld(t *testing.T) {
	c := NewClient("http://localhost")
	evs := dispatchCollect(t, c, "ses_s",
		`{"id":"e1","type":"session.step.ended","data":{"sessionID":"ses_s","finish":"tool-calls","tokens":{"cache":{"read":30000,"write":2000},"input":8000,"output":500,"reasoning":0}}}`,
		`{"id":"e2","type":"session.usage.updated","data":{"sessionID":"ses_s","tokens":{"cache":{"read":90000,"write":2000},"input":24000,"output":1500,"reasoning":0}}}`,
		`{"id":"e3","type":"session.step.ended","data":{"sessionID":"ses_s","finish":"stop","tokens":{"cache":{"read":0,"write":0},"input":1000,"output":50,"reasoning":0}}}`,
	)
	if len(evs) != 3 {
		t.Fatalf("got %d events: %+v", len(evs), evs)
	}
	if got := evs[0].ContextTokens; got != 8000+30000+2000 {
		t.Errorf("first step held %d tokens, want 40000 prompt tokens (output excluded)", got)
	}
	if !evs[0].MoreSteps {
		t.Error("a step that ended in tool calls was not reported as going on")
	}
	if evs[1].ContextTokens != 0 {
		t.Errorf("the running total was taken for the size of one call: %d", evs[1].ContextTokens)
	}
	if evs[2].ContextTokens != 1000 || evs[2].MoreSteps {
		t.Errorf("a step that ended in plain text = %+v, want 1000 prompt tokens and the end of the turn", evs[2])
	}
}

func TestCompactionEventsAreNamed(t *testing.T) {
	c := NewClient("http://localhost")
	evs := dispatchCollect(t, c, "ses_k",
		`{"id":"e1","type":"session.compaction.started","data":{"sessionID":"ses_k","reason":"auto"}}`,
		`{"id":"e2","type":"session.compaction.delta","data":{"sessionID":"ses_k","text":"## Objective"}}`,
		`{"id":"e3","type":"session.compaction.ended","data":{"sessionID":"ses_k","reason":"auto","text":"## Objective","tokens":{"input":3000,"output":50}}}`,
		`{"id":"e4","type":"session.compaction.failed","data":{"sessionID":"ses_k","reason":"auto"}}`,
	)
	if len(evs) != 4 {
		t.Fatalf("got %d events", len(evs))
	}
	want := []struct{ phase, compaction string }{{"compacting", "started"}, {"compacting", ""}, {"model", "ended"}, {"model", "failed"}}
	for i, w := range want {
		if evs[i].Phase != w.phase || evs[i].Compaction != w.compaction {
			t.Errorf("event %d = phase %q compaction %q, want %q %q", i, evs[i].Phase, evs[i].Compaction, w.phase, w.compaction)
		}
	}
	if evs[1].Delta != "" {
		t.Error("the summary text streamed as reply text")
	}
	for _, ev := range evs {
		if ev.Done || ev.ContextTokens != 0 {
			t.Errorf("compaction event %q ended the turn or claimed a context size: %+v", ev.Type, ev)
		}
	}
}

// A steered request runs at the next step boundary of the turn that is going, instead of queueing behind it.
func TestCompactAsksOpenCodeToSummarizeSteered(t *testing.T) {
	var path, method string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"data":{"id":"msg_1","type":"compaction","delivery":"steer"}}`))
	}))
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.Compact(context.Background(), "ses_c"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/session/ses_c/compact" {
		t.Errorf("request = %s %s", method, path)
	}
	if body["delivery"] != "steer" {
		t.Errorf("body = %v, want delivery steer", body)
	}
}

func TestCompactReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"a compaction is already queued"}`, http.StatusConflict)
	}))
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.Compact(context.Background(), "ses_c"); err == nil {
		t.Fatal("a refused request was reported as success")
	}
}
