package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/opencode"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/provider"
	rt "github.com/relocode/relo/internal/runtime"
)

// Optional real-OpenCode orchestration test for peer messaging. It drives Coord.runAgent directly against the
// installed opencode and a fake OpenAI endpoint, so no paid model is used. It proves that:
//   - runAgent puts a private messaging key for the agent into the prompt it sends;
//   - a real relo_agents MCP tool call made by the model reaches Coord.peerRequest and is attributed to the
//     sender that owns the key (the message's From is the sender's agent ID);
//   - the message is retained for a queued target in the coordinator, and is part of that target's next prompt.
// Runs only with RELO_E2E_OPENCODE=1.

const (
	peerE2EAsk     = "CUSTOM-ASK-MARKER-4e8b"
	peerE2EBody    = "PEER-E2E-BODY-5a2c"
	peerE2ESent    = `{"status":"completed","summary":"sent"}`
	peerE2ETimeout = 30 * time.Second
)

var peerE2EKeyRe = regexp.MustCompile(`Your private key is ([0-9a-f]{64})\.`)

// peerE2EModel is the fake OpenAI endpoint. Title-generation requests get a fixed answer. An agent request is
// recorded and its user prompt is inspected: the first agent request whose prompt
// carries a private messaging key is the sender. Its first model call returns the relo_agents tool call with that
// key and the target ID; every later call returns the completion.
type peerE2EModel struct {
	target string // the queued recipient's agent ID, set before the run

	mu      sync.Mutex
	key     string     // private key captured from the sender prompt
	prompts []string   // user prompt of each agent request, in order
	tools   [][]string // tool names offered on each agent request, in order
	titles  int
	sender  int // agent requests made by the sender
}

func (m *peerE2EModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(b, &req)
	if !strings.Contains(string(b), peerE2EAsk) {
		m.mu.Lock()
		m.titles++
		m.mu.Unlock()
		streamChunks(w, map[string]any{"role": "assistant", "content": "Title"}, "stop")
		return
	}
	// The prompt is the last user message; OpenCode reports tool results under another role.
	prompt := ""
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			prompt = fmt.Sprint(msg.Content)
		}
	}
	names := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		names = append(names, tl.Function.Name)
	}

	m.mu.Lock()
	m.prompts = append(m.prompts, prompt)
	m.tools = append(m.tools, names)
	key := ""
	if mm := peerE2EKeyRe.FindStringSubmatch(prompt); mm != nil {
		key = mm[1]
	}
	if m.key == "" && key != "" {
		m.key = key
	}
	fromSender := m.key != "" && key == m.key
	first := false
	if fromSender {
		m.sender++
		first = m.sender == 1
	}
	target := m.target
	m.mu.Unlock()

	if first {
		args, _ := json.Marshal(map[string]string{"key": key, "action": "send", "to": target, "text": peerE2EBody})
		streamChunks(w, toolCallDelta("call_relo_peer", rt.ReloAgentsTool, string(args)), "tool_calls")
		return
	}
	streamChunks(w, map[string]any{"role": "assistant", "content": peerE2ESent}, "stop")
}

func (m *peerE2EModel) snapshot() (prompts []string, tools [][]string, key string, titles int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.prompts...), append([][]string(nil), m.tools...), m.key, m.titles
}

// streamChunks writes one assistant delta, a final chunk with usage, and the SSE terminator.
func streamChunks(w http.ResponseWriter, delta map[string]any, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(d map[string]any, fin any, usage bool) {
		c := map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "claude-fake",
			"choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": fin}}}
		if usage {
			c["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}
		j, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", j)
	}
	chunk(delta, nil, false)
	chunk(map[string]any{}, finish, true)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func toolCallDelta(id, name, args string) map[string]any {
	call := map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
	return map[string]any{"role": "assistant", "tool_calls": []any{call}}
}

func hasToolName(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

// peerE2EFixture starts the real opencode and a coordinator wired to it through the real adapter. The sender and the
// target are Worker agents in the registry; the target is Queued, so its runtime session has not started.
func peerE2EFixture(t *testing.T, model *peerE2EModel) (*Coord, agent.Agent, agent.Agent, context.Context) {
	t.Helper()
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)
	work := t.TempDir()
	ps := []provider.Provider{{ID: "fake", Name: "fake", Kind: provider.KindOpenAI, BaseURL: srv.URL + "/v1", Models: []provider.Model{{ID: "claude-fake", Context: 200000, Output: 8000}}}}
	penv, err := provider.Env(ps, map[string]string{"fake": "k"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), peerE2ETimeout)
	t.Cleanup(cancel)
	proc, err := opencode.Start(ctx, bin, work, append(opencode.Isolation(t.TempDir()), penv...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	c := New(logging.New(nil, logging.Error))
	t.Cleanup(c.CloseMessages)
	c.SetRuntime(proc.Runtime())
	c.CreateSession(work, "peer e2e", ModeBuild)
	sender := c.Agents.Create(agent.Agent{ID: "peer-sender", Role: agent.Worker, Title: "Sender", Workspace: work, Model: "fake/claude-fake", Status: agent.Queued})
	target := c.Agents.Create(agent.Agent{ID: "peer-target", Role: agent.Worker, Title: "Target", Workspace: work, Model: "fake/claude-fake", Status: agent.Queued})
	model.target = target.ID
	return c, sender, target, ctx
}

func TestPeerMessagingThroughRealOpenCodeOrchestration(t *testing.T) {
	model := &peerE2EModel{}
	c, sender, target, ctx := peerE2EFixture(t, model)

	body, err := c.runAgent(ctx, sender, prompt.Parts{}, peerE2EAsk, prompt.ReportSchema)
	if err != nil {
		t.Fatalf("runAgent: %v", err)
	}
	if !strings.Contains(body, "sent") {
		t.Fatalf("sender body = %q", body)
	}

	prompts, tools, key, titles := model.snapshot()
	t.Logf("agent requests: %d; title requests: %d", len(prompts), titles)
	if len(prompts) == 0 {
		t.Fatal("the fake model received no agent request")
	}

	// 1. runAgent grants the private messaging key in the prompt it sends, next to the caller's own ask.
	if key == "" {
		t.Fatalf("no private messaging key in the sender prompt: %q", prompts[0])
	}
	if !strings.Contains(prompts[0], peerE2EAsk) {
		t.Errorf("the custom ask marker did not reach the model: %q", prompts[0])
	}
	if !hasToolName(tools[0], rt.ReloAgentsTool) {
		t.Fatalf("sender was not offered %s: tools = %v", rt.ReloAgentsTool, tools[0])
	}

	// 2. The relo_agents call reached Coord.peerRequest through the MCP hub and was attributed to the sender that
	// owns the key. The key was revoked when runAgent returned, so attribution is checked on the stored message.
	sess := c.Current()
	var queued []PeerMessage
	for _, m := range sess.PeerMessages {
		if m.To == target.ID {
			queued = append(queued, m)
		}
	}
	if len(queued) != 1 {
		t.Fatalf("target has %d messages, want 1; session messages: %+v", len(queued), sess.PeerMessages)
	}
	got := queued[0]
	if got.From != sender.ID {
		t.Errorf("message From = %q, want the sender %q (the key's owner)", got.From, sender.ID)
	}
	if got.Text != peerE2EBody {
		t.Errorf("message text = %q, want %q", got.Text, peerE2EBody)
	}

	// 3. The message is queued for the target in the coordinator, and it is the text the target's next turn will
	// include. Peer delivery is never acknowledged by the sender.
	if got.State != "queued" {
		t.Errorf("message state = %q, want queued (target has no live runtime session)", got.State)
	}
	if turn, ids := c.peerTurnText(target.ID); !strings.Contains(turn, peerE2EBody) || len(ids) != 1 {
		t.Errorf("target's next prompt would carry %q (ids %v), want the queued message", turn, ids)
	}

	before := len(prompts)
	if _, err := c.runAgent(ctx, target, prompt.Parts{}, peerE2EAsk, prompt.ReportSchema); err != nil {
		t.Fatalf("recipient runAgent: %v", err)
	}
	prompts, _, _, _ = model.snapshot()
	if len(prompts) <= before || !strings.Contains(prompts[before], peerE2EBody) {
		t.Fatalf("recipient model did not receive the queued message: %v", prompts[before:])
	}
	if messages := c.Current().PeerMessages; len(messages) != 1 || messages[0].State != "delivered" {
		t.Fatalf("recipient prompt did not update delivery state: %+v", messages)
	}
}
