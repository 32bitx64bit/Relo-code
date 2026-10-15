package ipc

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
)

func testQueue(limit int) *frameQueue {
	q := &frameQueue{limit: limit, out: io.Discard}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func evtFrame(method, payload string) queuedFrame {
	key, _ := coalesceKey(method, json.RawMessage(payload))
	return queuedFrame{data: []byte(method + " " + payload), kind: KindEvt, method: method, key: key}
}

func resFrame(id string) queuedFrame {
	return queuedFrame{data: []byte("res " + id), kind: KindRes}
}

func queued(q *frameQueue) []string {
	var out []string
	for _, f := range q.frames {
		out = append(out, string(f.data))
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustEnqueue(t *testing.T, q *frameQueue, f queuedFrame) {
	t.Helper()
	if err := q.enqueue(f); err != nil {
		t.Fatalf("enqueue %s: %v", f.method, err)
	}
}

func TestQueueNeverDropsChatMessageWhileLossyPresent(t *testing.T) {
	q := testQueue(4)
	mustEnqueue(t, q, evtFrame("chat.message", `{"id":"m1"}`))
	mustEnqueue(t, q, evtFrame("log", `{"n":1}`))
	mustEnqueue(t, q, evtFrame("chat.message", `{"id":"m2"}`))
	mustEnqueue(t, q, evtFrame("log", `{"n":2}`))
	mustEnqueue(t, q, evtFrame("log", `{"n":3}`)) // evicts log n=1
	mustEnqueue(t, q, evtFrame("log", `{"n":4}`)) // evicts log n=2
	want := []string{`chat.message {"id":"m1"}`, `chat.message {"id":"m2"}`, `log {"n":3}`, `log {"n":4}`}
	if got := queued(q); !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestQueueEvictsNonLossyBeforeChatMessage(t *testing.T) {
	q := testQueue(3)
	mustEnqueue(t, q, evtFrame("chat.message", `{"id":"m1"}`))
	mustEnqueue(t, q, evtFrame("agent.spawned", `{"id":"x"}`))
	mustEnqueue(t, q, evtFrame("chat.message", `{"id":"m2"}`))
	mustEnqueue(t, q, evtFrame("chat.message", `{"id":"m3"}`)) // evicts agent.spawned, the only non-chat.message event
	want := []string{`chat.message {"id":"m1"}`, `chat.message {"id":"m2"}`, `chat.message {"id":"m3"}`}
	if got := queued(q); !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestQueueNeverEvictsResponsesWhenLossyPresent(t *testing.T) {
	q := testQueue(3)
	mustEnqueue(t, q, resFrame("1"))
	mustEnqueue(t, q, evtFrame("chat.delta", `{"agentId":"a","text":"x"}`))
	mustEnqueue(t, q, evtFrame("log", `{"n":1}`))
	mustEnqueue(t, q, evtFrame("log", `{"n":2}`)) // evicts the lossy log, never the res
	// chat.delta is the oldest lossy frame, so it is evicted first; the res stays.
	want := []string{"res 1", `log {"n":1}`, `log {"n":2}`}
	if got := queued(q); !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestQueueCoalescesChatDeltaByAgentID(t *testing.T) {
	q := testQueue(10)
	mustEnqueue(t, q, evtFrame("chat.delta", `{"agentId":"a","text":"1"}`))
	mustEnqueue(t, q, evtFrame("chat.delta", `{"agentId":"b","text":"1"}`))
	mustEnqueue(t, q, evtFrame("chat.delta", `{"agentId":"a","text":"12"}`))
	// Replaced frame moves to the tail: a's newest delta follows b's.
	want := []string{`chat.delta {"agentId":"b","text":"1"}`, `chat.delta {"agentId":"a","text":"12"}`}
	if got := queued(q); !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestQueueCoalescesAgentUpdatedAndUsageUpdated(t *testing.T) {
	q := testQueue(10)
	mustEnqueue(t, q, evtFrame("agent.updated", `{"id":"a","status":"running"}`))
	mustEnqueue(t, q, evtFrame("agent.updated", `{"id":"a","status":"done"}`))
	mustEnqueue(t, q, evtFrame("usage.updated", `{"agentId":"a","input":1}`))
	mustEnqueue(t, q, evtFrame("usage.updated", `{"agentId":"a","input":2}`))
	want := []string{`agent.updated {"id":"a","status":"done"}`, `usage.updated {"agentId":"a","input":2}`}
	if got := queued(q); !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCoalesceKey(t *testing.T) {
	cases := []struct {
		name, payload string
		ok            bool
	}{
		{"chat.delta", `{"agentId":"a"}`, true},
		{"agent.updated", `{"id":"a"}`, true},
		{"usage.updated", `{"agentId":"a"}`, true},
		{"chat.delta", `{"text":"no agent"}`, false},
		{"chat.message", `{"agentId":"a"}`, false},
		{"chat.delta", ``, false},
	}
	for _, c := range cases {
		_, ok := coalesceKey(c.name, json.RawMessage(c.payload))
		if ok != c.ok {
			t.Errorf("coalesceKey(%s, %s) ok=%v want %v", c.name, c.payload, ok, c.ok)
		}
	}
}

func TestQueuePreservesOrderOfNonLossyFrames(t *testing.T) {
	q := testQueue(10)
	mustEnqueue(t, q, resFrame("1"))
	mustEnqueue(t, q, evtFrame("chat.message", `{"id":"m1"}`))
	mustEnqueue(t, q, evtFrame("agent.spawned", `{"id":"s1"}`))
	mustEnqueue(t, q, resFrame("2"))
	mustEnqueue(t, q, evtFrame("chat.message", `{"id":"m2"}`))
	want := []string{"res 1", `chat.message {"id":"m1"}`, `agent.spawned {"id":"s1"}`, "res 2", `chat.message {"id":"m2"}`}
	if got := queued(q); !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
