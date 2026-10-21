package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

const (
	thinkMarker = "THINK-MARKER-step-one"
	sigMarker   = "SIG-MARKER-step-one"
)

// Claude reached through an OpenAI-compatible router lost its thinking between tool steps: OpenAI's format has no
// field for the thinking's signature, so the router replaced it with a placeholder and Claude re-derived its whole
// plan, 40-60k output tokens, on every step. A model declared kind anthropic is spoken to through Anthropic's
// format at the same base URL, and its signed thinking goes back on the next step. This runs the real OpenCode
// against a fake endpoint that answers both formats. It needs the opencode binary: RELO_E2E_OPENCODE=1.
func TestClaudeThinkingSurvivesToolStepsOnlyInAnthropicFormat(t *testing.T) {
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	type seen struct {
		path, apiKey string
		followUp     []byte // the request that carries the tool result: what the model is shown of step one
	}
	var mu sync.Mutex
	calls := map[string]*seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &req)
		anthropic := strings.HasSuffix(r.URL.Path, "/messages")
		if !anthropic && !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		followUp := strings.Contains(string(b), `"tool_result"`) || strings.Contains(string(b), `"role":"tool"`)
		mu.Lock()
		c := calls[req.Model]
		if c == nil {
			c = &seen{path: r.URL.Path, apiKey: r.Header.Get("x-api-key")}
			calls[req.Model] = c
		}
		if followUp && c.followUp == nil {
			c.followUp = b
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if anthropic {
			writeAnthropicStep(w, req.Model, followUp)
		} else {
			writeOpenAIStep(w, req.Model, followUp)
		}
	}))
	defer srv.Close()

	yes := true
	models := []provider.Model{
		{ID: "claude-native", Kind: provider.KindAnthropic, Reasoning: true, Context: 200000, Output: 8000},
		{ID: "claude-native-unflagged", Kind: provider.KindAnthropic, Context: 200000, Output: 8000, Vision: &yes},
		{ID: "claude-shim", Reasoning: true, Context: 200000, Output: 8000},
	}
	ps := []provider.Provider{{ID: "fake", Name: "fake", Kind: provider.KindOpenAI, BaseURL: srv.URL + "/v1", Models: models}}
	penv, err := provider.Env(ps, map[string]string{"fake": "k"}, false)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(work+"/note.txt", []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	proc, err := Start(ctx, bin, work, append(Isolation(t.TempDir()), penv...))
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Stop()
	rt := proc.Runtime()
	for _, m := range models {
		sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: m.ID, Directory: work, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		evs, err := rt.Events(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		task := runtime.AgentTask{AgentID: m.ID, Role: "probe", Model: m.ID, Provider: "fake", System: "SYSTEM", Prompt: "List the text files.", Tools: runtime.ToolsFor("probe", "build", false), Directory: work}
		if err := rt.SendTask(ctx, sid, task); err != nil {
			t.Fatal(err)
		}
		for ev := range evs {
			if ev.Done {
				break
			}
		}
		_ = rt.CloseSession(ctx, sid)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range models {
		c := calls[m.ID]
		if c == nil || c.followUp == nil {
			t.Fatalf("%s: the model was never called back with the tool result (%+v)", m.ID, c)
		}
		thinking, signature := returnedThinking(c.followUp)
		t.Logf("%s: %s, thinking sent back %q, signature %q", m.ID, c.path, thinking, signature)
		if m.Kind != provider.KindAnthropic {
			// The OpenAI format can carry the text at most, never the signature, so the router cannot pass it on.
			if signature != "" {
				t.Errorf("%s: OpenAI-format request carried a thinking signature %q; the per-model kind may be unnecessary now", m.ID, signature)
			}
			continue
		}
		if !strings.HasSuffix(c.path, "/v1/messages") || c.apiKey != "k" {
			t.Errorf("%s: called %s with x-api-key %q, want /v1/messages with the provider's key", m.ID, c.path, c.apiKey)
		}
		if thinking != thinkMarker || signature != sigMarker {
			t.Errorf("%s: step two was sent thinking %q signed %q, want step one's %q signed %q", m.ID, thinking, signature, thinkMarker, sigMarker)
		}
	}
}

// returnedThinking finds step one's thinking in a follow-up request: an Anthropic thinking block, or the reasoning
// text an OpenAI-format assistant message may carry.
func returnedThinking(body []byte) (thinking, signature string) {
	var req struct {
		Messages []struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		if t := m.ReasoningContent + m.Reasoning; t != "" {
			thinking = t
		}
		var blocks []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "thinking" {
					thinking, signature = b.Thinking, b.Signature
				}
			}
		}
	}
	return thinking, signature
}

// writeAnthropicStep answers in Anthropic's stream format: step one thinks (signed) and calls glob, step two ends.
func writeAnthropicStep(w http.ResponseWriter, model string, followUp bool) {
	event := func(name string, v map[string]any) {
		j, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, j)
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 1}}})
	stop := "end_turn"
	if followUp {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "done"}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	} else {
		stop = "tool_use"
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": thinkMarker}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": sigMarker}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "glob", "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"pattern":"*.txt"}`}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
	}
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 20}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

// writeOpenAIStep is the same exchange in OpenAI's chat stream format, with the thinking as reasoning_content.
func writeOpenAIStep(w http.ResponseWriter, model string, followUp bool) {
	chunk := func(delta map[string]any, finish any) {
		j, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\n", j)
	}
	if followUp {
		chunk(map[string]any{"role": "assistant", "content": "done"}, nil)
		chunk(map[string]any{}, "stop")
	} else {
		chunk(map[string]any{"role": "assistant", "reasoning_content": thinkMarker}, nil)
		chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "glob", "arguments": `{"pattern":"*.txt"}`}}}}, nil)
		chunk(map[string]any{}, "tool_calls")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// On an OpenAI-compatible route the router fills its own default effort (9router: xhigh) whenever a request names
// none, so a model's chosen effort must reach the wire as reasoning_effort. OpenCode drops a "none" variant, so
// thinking cannot be switched off this way, only lowered. Needs RELO_E2E_OPENCODE=1.
func TestChosenEffortReachesAnOpenAIFormatRequest(t *testing.T) {
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	var mu sync.Mutex
	efforts := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		model, _ := req["model"].(string)
		mu.Lock()
		if _, seen := efforts[model]; !seen && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			efforts[model] = req["reasoning_effort"]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		writeOpenAIStep(w, model, true)
	}))
	defer srv.Close()
	levels := []string{"low", "medium", "high"}
	ps := []provider.Provider{{ID: "fake", Kind: provider.KindOpenAI, BaseURL: srv.URL + "/v1", Models: []provider.Model{
		{ID: "claude-low", Reasoning: true, Efforts: levels}, {ID: "claude-default", Reasoning: true, Efforts: levels}}}}
	penv, err := provider.Env(ps, map[string]string{"fake": "k"}, false)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	proc, err := Start(ctx, bin, work, append(Isolation(t.TempDir()), penv...))
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Stop()
	rt := proc.Runtime()
	want := map[string]string{"claude-low": "low", "claude-default": ""}
	for model, effort := range want {
		sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: model, Directory: work, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		evs, err := rt.Events(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		task := runtime.AgentTask{AgentID: model, Role: "probe", Model: model, Provider: "fake", Effort: effort, System: "SYSTEM", Prompt: "hi", Tools: runtime.ToolsFor("probe", "build", false), Directory: work}
		if err := rt.SendTask(ctx, sid, task); err != nil {
			t.Fatal(err)
		}
		for ev := range evs {
			if ev.Done {
				break
			}
		}
		_ = rt.CloseSession(ctx, sid)
	}
	mu.Lock()
	defer mu.Unlock()
	for model, effort := range want {
		t.Logf("%s: chose %q, request carried reasoning_effort %v", model, effort, efforts[model])
		if effort == "" && efforts[model] != nil || effort != "" && efforts[model] != effort {
			t.Errorf("%s: request carried reasoning_effort %v, want %q (none when unset)", model, efforts[model], effort)
		}
	}
}
