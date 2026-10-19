package agentmsg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func canvasEcho(_ context.Context, req CanvasRequest) (any, error) {
	if req.Key != "agent-key" {
		return nil, errors.New("invalid canvas key")
	}
	return map[string]any{"action": req.Action, "id": req.ID, "revision": req.Revision}, nil
}

func startCanvas(t *testing.T, h CanvasHandler) *Server {
	t.Helper()
	s, err := Start(echoHandler, WithCanvas(h))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func toolNames(t *testing.T, s *Server) []string {
	t.Helper()
	res := rpc(t, s, "tools/list", map[string]any{})
	tools := res["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func callCanvasTool(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res := rpc(t, s, "tools/call", map[string]any{"name": canvasToolName, "arguments": args})
	if e, ok := res["error"]; ok {
		return map[string]any{"rpcError": e}
	}
	return res["result"].(map[string]any)
}

func TestDefaultCatalogStaysSingleToolWithoutCanvas(t *testing.T) {
	s := startTest(t, echoHandler)
	names := toolNames(t, s)
	if len(names) != 1 || names[0] != toolName {
		t.Fatalf("default tools = %v, want only %s", names, toolName)
	}
	res := rpc(t, s, "tools/call", map[string]any{"name": canvasToolName, "arguments": map[string]any{"key": "agent-key", "action": "list"}})
	if res["error"] == nil {
		t.Fatalf("relo_canvas must be unknown when canvas is not configured: %v", res)
	}
}

func TestCanvasCatalogAddsSecondToolAndKeepsAgents(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	names := toolNames(t, s)
	if len(names) != 2 || names[0] != toolName || names[1] != canvasToolName {
		t.Fatalf("tools = %v", names)
	}
}

func TestCanvasRoutesThroughHandlerWithKey(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	res := callCanvasTool(t, s, map[string]any{"key": "agent-key", "action": "update", "id": "art-1", "revision": 2, "files": map[string]any{"index.html": "<p>x</p>"}, "entry": "index.html"})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"revision":2`) || !strings.Contains(text, "art-1") {
		t.Fatalf("text = %s", text)
	}
}

func TestCanvasHandlerErrorIsToolError(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	res := callCanvasTool(t, s, map[string]any{"key": "wrong", "action": "list"})
	if res["isError"] != true {
		t.Fatalf("expected tool error, got %v", res)
	}
}

func TestCanvasValidationRejectsBadShape(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	cases := map[string]map[string]any{
		"unknown action":         {"key": "agent-key", "action": "delete"},
		"missing id for open":    {"key": "agent-key", "action": "open"},
		"bad id":                 {"key": "agent-key", "action": "open", "id": "../x"},
		"bad format":             {"key": "agent-key", "action": "create", "format": "python", "entry": "a.py", "files": map[string]any{"a.py": "x"}},
		"create without entry":   {"key": "agent-key", "action": "create", "format": "html", "files": map[string]any{"index.html": "x"}},
		"entry not in files":     {"key": "agent-key", "action": "create", "format": "html", "entry": "b.html", "files": map[string]any{"index.html": "x"}},
		"path traversal":         {"key": "agent-key", "action": "create", "format": "html", "entry": "../x.html", "files": map[string]any{"../x.html": "x"}},
		"source on list":         {"key": "agent-key", "action": "list", "files": map[string]any{"a.html": "x"}},
		"width too large":        {"key": "agent-key", "action": "capture", "id": "a", "width": 99999},
		"unknown field":          {"key": "agent-key", "action": "list", "extra": true},
		"bad base64 asset":       {"key": "agent-key", "action": "update", "id": "a", "assets": map[string]any{"i.png": map[string]any{"mime": "image/png", "data": "!!!"}}},
		"revision negative":      {"key": "agent-key", "action": "update", "id": "a", "revision": -1},
		"too many files":         {"key": "agent-key", "action": "create", "format": "html", "entry": "f0", "files": manyFiles(65)},
		"oversize text per file": {"key": "agent-key", "action": "create", "format": "html", "entry": "a.html", "files": map[string]any{"a.html": strings.Repeat("a", maxCanvasText+1)}},
	}
	for name, args := range cases {
		res := callCanvasTool(t, s, args)
		if res["rpcError"] == nil {
			t.Errorf("%s: expected invalid params, got %v", name, res)
		}
	}
}

func manyFiles(n int) map[string]any {
	files := map[string]any{}
	for i := 0; i < n; i++ {
		files["f"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+itoa(i)] = "x"
	}
	return files
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestCanvasOverallPayloadCapEnforced(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	// Eight files of just under the per-file cap exceed the 4 MiB overall cap.
	chunk := strings.Repeat("a", maxCanvasText)
	files := map[string]any{}
	for i := 0; i < 9; i++ {
		files["f"+itoa(i)+".html"] = chunk
	}
	res := callCanvasTool(t, s, map[string]any{"key": "agent-key", "action": "create", "format": "html", "entry": "f0.html", "files": files})
	if res["rpcError"] == nil {
		t.Fatalf("expected overall payload rejection, got %v", res)
	}
}

func TestCanvasAcceptsLargeBodyWithinCanvasLimitOnly(t *testing.T) {
	s := startCanvas(t, canvasEcho)
	// A canvas call near 3 MiB is accepted even though it is far above the 32 KiB agents limit.
	body := strings.Repeat("a", 3<<20)
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canvas","arguments":{"key":"agent-key","action":"create","format":"html","entry":"a.html","files":{"a.html":"` + body + `"}}}}`
	code, _ := post(t, s, bearer(s), raw)
	if code != http.StatusOK {
		t.Fatalf("large canvas status = %d, want 200", code)
	}
	// A non-canvas call with the same size is still rejected.
	nonCanvas := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agents","arguments":{"key":"agent-key","action":"list","text":"` + body + `"}}}`
	code, _ = post(t, s, bearer(s), nonCanvas)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large agents status = %d, want 413", code)
	}
}

func TestCanvasCaptureMapsScreenshotToImageContent(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	handler := func(_ context.Context, req CanvasRequest) (any, error) {
		return map[string]any{
			"artifact": map[string]any{"id": req.ID, "revision": 3},
			"capture": map[string]any{
				"mime": "image/png", "data": png, "width": 800, "height": 600,
				"visualInspectionAvailable": false,
			},
		}, nil
	}
	s := startCanvas(t, handler)
	res := callCanvasTool(t, s, map[string]any{"key": "agent-key", "action": "capture", "id": "art-1"})
	content := res["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content blocks = %d, want text + image: %v", len(content), content)
	}
	text := content[0].(map[string]any)["text"].(string)
	if strings.Contains(text, png) || strings.Contains(text, `"data"`) {
		t.Fatalf("base64 duplicated in text: %s", text)
	}
	if !strings.Contains(text, "visualInspectionAvailable") || !strings.Contains(text, "imageAttached") {
		t.Fatalf("metadata missing: %s", text)
	}
	img := content[1].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/png" || img["data"] != png {
		t.Fatalf("image block = %v", img)
	}
}

func TestCanvasCaptureWithoutImageStaysTextOnly(t *testing.T) {
	handler := func(_ context.Context, req CanvasRequest) (any, error) {
		return map[string]any{"artifact": map[string]any{"id": req.ID}, "capture": map[string]any{"mime": "image/jpeg", "data": "AAAA"}}, nil
	}
	s := startCanvas(t, handler)
	res := callCanvasTool(t, s, map[string]any{"key": "agent-key", "action": "capture", "id": "a"})
	if n := len(res["content"].([]any)); n != 1 {
		t.Fatalf("non-PNG capture must not attach an image; blocks = %d", n)
	}
}

func TestCanvasHandlerPanicIsToolError(t *testing.T) {
	s := startCanvas(t, func(context.Context, CanvasRequest) (any, error) { panic("boom") })
	res := callCanvasTool(t, s, map[string]any{"key": "agent-key", "action": "list"})
	if res["isError"] != true {
		t.Fatalf("panic must become tool error: %v", res)
	}
}

func TestIsCanvasCallOnlyMatchesCanvasToolCalls(t *testing.T) {
	if !isCanvasCall([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canvas"}}`)) {
		t.Fatal("canvas call not detected")
	}
	if isCanvasCall([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agents"}}`)) {
		t.Fatal("agents call misdetected as canvas")
	}
	if isCanvasCall([]byte(`not json`)) {
		t.Fatal("invalid json misdetected")
	}
}
