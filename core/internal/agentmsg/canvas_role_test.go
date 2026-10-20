package agentmsg

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// canvasActionNames is the advertised action list of the relo_canvas tool, in the order the descriptor declares it.
// It is the 8 MCP canvas action names; a change to this list is a change to the tool contract.
var canvasActionNames = []string{"list", "open", "create", "update", "inspect", "capture", "popout", "close"}

// canvasDescriptor returns the relo_canvas entry of the tools/list result.
func canvasDescriptor(t *testing.T, s *Server) map[string]any {
	t.Helper()
	res := rpc(t, s, "tools/list", map[string]any{})
	tools, ok := res["result"].(map[string]any)["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result has no tools: %v", res)
	}
	for _, tool := range tools {
		entry, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if entry["name"] == canvasToolName {
			return entry
		}
	}
	t.Fatalf("tools/list does not advertise %s: %v", canvasToolName, tools)
	return nil
}

// canvasAdvertisedActions reads the action enum from the relo_canvas descriptor.
func canvasAdvertisedActions(t *testing.T, s *Server) []string {
	t.Helper()
	entry := canvasDescriptor(t, s)
	schema, ok := entry["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("canvas descriptor has no inputSchema: %v", entry)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("canvas descriptor has no properties: %v", schema)
	}
	action, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatalf("canvas descriptor has no action property: %v", props)
	}
	raw, ok := action["enum"].([]any)
	if !ok {
		t.Fatalf("canvas action property has no enum: %v", action)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("canvas action enum holds a non-string: %v", v)
		}
		out = append(out, s)
	}
	return out
}

// TestCanvasDescriptorAdvertisesAllEightActions pins the tool contract: the descriptor declares exactly the 8 MCP
// canvas action names, in order. No action is added, renamed or reordered.
func TestCanvasDescriptorAdvertisesAllEightActions(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	got := canvasAdvertisedActions(t, s)
	if !reflect.DeepEqual(got, canvasActionNames) {
		t.Fatalf("canvas descriptor actions = %v, want %v", got, canvasActionNames)
	}
}

// TestCanvasDescriptorIsRoleBlindResidual documents the residual that keeps the descriptor unfiltered per role.
//
// The MCP endpoint is one shared service authenticated by a single server-wide secret (agentmsg.go: Secret,
// authorized), and its RPC router (route) never sees the *http.Request, so tools/list cannot resolve the calling
// agent's role. Every authenticated caller therefore receives the same descriptor, and the primary is advertised
// create and update even though it may not perform them. The refusal happens later, in the core handler that owns
// the capability key (core/internal/orchestrator/canvas.go), which is the enforcement point.
//
// A client-supplied header is not an identity channel either: it is caller-controlled, so honoring it would let any
// caller claim any role. This test pins that the descriptor does not branch on one.
func TestCanvasDescriptorIsRoleBlindResidual(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	primary := canvasAdvertisedActions(t, s)
	builder := canvasAdvertisedActions(t, s)
	if !reflect.DeepEqual(primary, builder) {
		t.Fatalf("descriptor differs between callers: %v vs %v", primary, builder)
	}
	for _, action := range canvasActionNames {
		if !containsAction(primary, action) {
			t.Fatalf("descriptor omits %s: %v", action, primary)
		}
	}
	// A role header changes nothing: the endpoint has no identity to read.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	req, err := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearer(s))
	req.Header.Set("X-Relo-Agent-Role", "primary")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status %d", resp.StatusCode)
	}
	var decoded struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties struct {
						Action struct {
							Enum []string `json:"enum"`
						} `json:"action"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, tool := range decoded.Result.Tools {
		if tool.Name != canvasToolName {
			continue
		}
		if !reflect.DeepEqual(tool.InputSchema.Properties.Action.Enum, primary) {
			t.Fatalf("role header changed the descriptor: %v vs %v", tool.InputSchema.Properties.Action.Enum, primary)
		}
		return
	}
	t.Fatalf("tools/list does not advertise %s", canvasToolName)
}

// TestCanvasTransportForwardsEveryActionToTheCoreHandler shows where the refusal happens. The transport validates
// shape and size and forwards the call; it does not know the caller's role, so it cannot refuse create or update.
// The core handler that owns the capability key applies the role policy and refuses the call there.
func TestCanvasTransportForwardsEveryActionToTheCoreHandler(t *testing.T) {
	var seen []string
	s := startCanvas(t, func(_ context.Context, req CanvasRequest) (any, error) {
		seen = append(seen, req.Action)
		return map[string]any{"action": req.Action}, nil
	})
	cases := []map[string]any{
		{"key": "agent-key", "action": "list"},
		{"key": "agent-key", "action": "open", "id": "abc"},
		{"key": "agent-key", "action": "create", "format": "html", "entry": "index.html", "files": map[string]any{"index.html": "<p/>"}},
		{"key": "agent-key", "action": "update", "id": "abc", "revision": 1, "entry": "index.html", "files": map[string]any{"index.html": "<p/>"}},
		{"key": "agent-key", "action": "inspect", "id": "abc"},
		{"key": "agent-key", "action": "capture", "id": "abc"},
		{"key": "agent-key", "action": "popout", "id": "abc"},
		{"key": "agent-key", "action": "close", "id": "abc"},
	}
	for _, args := range cases {
		res := callCanvasTool(t, s, args)
		if e, ok := res["rpcError"]; ok {
			t.Fatalf("%v: rpc error %v", args["action"], e)
		}
		if isError, _ := res["isError"].(bool); isError {
			t.Fatalf("%v: tool error %v", args["action"], res)
		}
	}
	if !reflect.DeepEqual(seen, canvasActionNames) {
		t.Fatalf("handler saw %v, want %v", seen, canvasActionNames)
	}
}

// TestValidateCanvasAcceptsAllEightActionNames keeps the wire contract intact: a builder request naming any of the 8
// actions is well formed at the transport, and an unknown action is still refused.
func TestValidateCanvasAcceptsAllEightActionNames(t *testing.T) {
	cases := map[string]CanvasRequest{
		"list":    {Key: "agent-key", Action: "list"},
		"open":    {Key: "agent-key", Action: "open", ID: "abc"},
		"create":  {Key: "agent-key", Action: "create", Format: "html", Entry: "index.html", Files: map[string]string{"index.html": "<p/>"}},
		"update":  {Key: "agent-key", Action: "update", ID: "abc", Revision: 1, Entry: "index.html", Files: map[string]string{"index.html": "<p/>"}},
		"inspect": {Key: "agent-key", Action: "inspect", ID: "abc"},
		"capture": {Key: "agent-key", Action: "capture", ID: "abc"},
		"popout":  {Key: "agent-key", Action: "popout", ID: "abc"},
		"close":   {Key: "agent-key", Action: "close", ID: "abc"},
	}
	for _, action := range canvasActionNames {
		req, ok := cases[action]
		if !ok {
			t.Fatalf("no builder request for action %s", action)
		}
		if err := validateCanvas(req); err != nil {
			t.Fatalf("validateCanvas(%s): %v", action, err)
		}
	}
	if err := validateCanvas(CanvasRequest{Key: "agent-key", Action: "status"}); err == nil {
		t.Fatal("validateCanvas accepted an unknown action")
	}
}

func containsAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}
