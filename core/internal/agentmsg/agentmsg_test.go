package agentmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func startTest(t *testing.T, h Handler) *Server {
	t.Helper()
	s, err := Start(h)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func echoHandler(_ context.Context, req Request) (any, error) {
	if req.Key != "agent-key" {
		return nil, errors.New("invalid key")
	}
	return map[string]any{"action": req.Action, "to": req.To, "text": req.Text}, nil
}

func post(t *testing.T, s *Server, auth string, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func bearer(s *Server) string { return "Bearer " + s.Secret }

func rpc(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	raw, _ := json.Marshal(body)
	code, b := post(t, s, bearer(s), string(raw))
	if code != http.StatusOK {
		t.Fatalf("%s: status %d body %s", method, code, b)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, b)
	}
	return out
}

func TestStartBindsLoopbackWithSecret(t *testing.T) {
	s := startTest(t, echoHandler)
	if !strings.HasPrefix(s.URL, "http://127.0.0.1:") || !strings.HasSuffix(s.URL, "/mcp") {
		t.Fatalf("URL = %q", s.URL)
	}
	if len(s.Secret) != 64 {
		t.Fatalf("secret length = %d, want 64 hex chars", len(s.Secret))
	}
	s2 := startTest(t, echoHandler)
	if s.Secret == s2.Secret {
		t.Fatal("secrets must be unique per server")
	}
}

func TestStartNilHandler(t *testing.T) {
	if _, err := Start(nil); err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestInitializeMirrorsProtocol(t *testing.T) {
	s := startTest(t, echoHandler)
	out := rpc(t, s, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}
	out = rpc(t, s, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	res = out["result"].(map[string]any)
	if res["protocolVersion"] != "2024-11-05" {
		t.Fatalf("unsupported version fallback = %v", res["protocolVersion"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("tools capability missing")
	}
}

func TestPingAndToolsList(t *testing.T) {
	s := startTest(t, echoHandler)
	if out := rpc(t, s, "ping", map[string]any{}); out["error"] != nil || out["result"] == nil {
		t.Fatalf("ping = %v", out)
	}
	out := rpc(t, s, "tools/list", map[string]any{})
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "agents" {
		t.Fatalf("tool name = %v", tool["name"])
	}
	schema := tool["inputSchema"].(map[string]any)
	req := schema["required"].([]any)
	if len(req) != 2 || req[0] != "key" || req[1] != "action" {
		t.Fatalf("required = %v", req)
	}
	action := schema["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]any)
	if len(action) != 3 {
		t.Fatalf("action enum = %v", action)
	}
}

func TestToolCallSuccess(t *testing.T) {
	s := startTest(t, echoHandler)
	out := rpc(t, s, "tools/call", map[string]any{
		"name":      "agents",
		"arguments": map[string]any{"key": "agent-key", "action": "send", "to": "bob", "text": "hi"},
	})
	res := out["result"].(map[string]any)
	if res["isError"] != false {
		t.Fatalf("isError = %v", res["isError"])
	}
	content := res["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" {
		t.Fatalf("content type = %v", content["type"])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content["text"].(string)), &payload); err != nil {
		t.Fatalf("text is not JSON: %v", err)
	}
	if payload["action"] != "send" || payload["to"] != "bob" || payload["text"] != "hi" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestToolCallHandlerErrorIsToolError(t *testing.T) {
	s := startTest(t, echoHandler)
	out := rpc(t, s, "tools/call", map[string]any{
		"name":      "agents",
		"arguments": map[string]any{"key": "wrong", "action": "list"},
	})
	if out["error"] != nil {
		t.Fatalf("handler error must not be a JSON-RPC error: %v", out["error"])
	}
	res := out["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("isError = %v", res["isError"])
	}
	text := res["content"].([]any)[0].(map[string]any)["text"]
	if text != "invalid key" {
		t.Fatalf("text = %v", text)
	}
}

func TestToolCallHandlerPanicIsToolError(t *testing.T) {
	s := startTest(t, func(context.Context, Request) (any, error) { panic("boom") })
	out := rpc(t, s, "tools/call", map[string]any{
		"name":      "agents",
		"arguments": map[string]any{"key": "k", "action": "inbox"},
	})
	if out["result"].(map[string]any)["isError"] != true {
		t.Fatalf("panic not converted to tool error: %v", out)
	}
}

func TestToolCallInvalidParams(t *testing.T) {
	s := startTest(t, echoHandler)
	cases := map[string]map[string]any{
		"forged tool name": {"name": "shell", "arguments": map[string]any{"key": "k", "action": "list"}},
		"missing args":     {"name": "agents"},
		"missing key":      {"name": "agents", "arguments": map[string]any{"action": "list"}},
		"bad action":       {"name": "agents", "arguments": map[string]any{"key": "k", "action": "delete"}},
		"send without to":  {"name": "agents", "arguments": map[string]any{"key": "k", "action": "send"}},
		"unknown field":    {"name": "agents", "arguments": map[string]any{"key": "k", "action": "list", "exec": "rm"}},
		"wrong type":       {"name": "agents", "arguments": map[string]any{"key": 5, "action": "list"}},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			out := rpc(t, s, "tools/call", params)
			e, ok := out["error"].(map[string]any)
			if !ok || e["code"] != float64(codeInvalidParams) {
				t.Fatalf("want invalid params error, got %v", out)
			}
		})
	}
}

func TestProtocolErrors(t *testing.T) {
	s := startTest(t, echoHandler)
	cases := map[string]struct {
		body string
		code float64
	}{
		"parse error":     {`{"jsonrpc":"2.0","id":1,`, codeParseError},
		"batch":           {`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, codeInvalidRequest},
		"not object":      {`"hello"`, codeInvalidRequest},
		"wrong version":   {`{"jsonrpc":"1.0","id":1,"method":"ping"}`, codeInvalidRequest},
		"object id":       {`{"jsonrpc":"2.0","id":{},"method":"ping"}`, codeInvalidRequest},
		"unknown method":  {`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, codeMethodNotFound},
		"initialize bare": {`{"jsonrpc":"2.0","id":1,"method":"initialize"}`, codeInvalidParams},
		"call bare":       {`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, codeInvalidParams},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, b := post(t, s, bearer(s), tc.body)
			if code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			var out map[string]any
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			e, ok := out["error"].(map[string]any)
			if !ok || e["code"] != tc.code {
				t.Fatalf("want code %v, got %s", tc.code, b)
			}
		})
	}
}

func TestNotificationReturns202(t *testing.T) {
	s := startTest(t, echoHandler)
	code, b := post(t, s, bearer(s), `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if len(b) != 0 {
		t.Fatalf("body = %q, want empty", b)
	}
	// A tools/call sent as a notification must not execute the handler.
	called := false
	s2 := startTest(t, func(context.Context, Request) (any, error) { called = true; return nil, nil })
	code, _ = post(t, s2, bearer(s2), `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"agents","arguments":{"key":"k","action":"list"}}}`)
	if code != http.StatusAccepted || called {
		t.Fatalf("notification tools/call: status %d called %v", code, called)
	}
}

func TestAuthRequired(t *testing.T) {
	s := startTest(t, echoHandler)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	cases := map[string]string{
		"missing":      "",
		"wrong secret": "Bearer " + strings.Repeat("0", 64),
		"no scheme":    s.Secret,
		"basic scheme": "Basic " + s.Secret,
		"truncated":    "Bearer " + s.Secret[:10],
		"extra suffix": "Bearer " + s.Secret + "x",
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			code, _ := post(t, s, auth, ping)
			if code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", code)
			}
		})
	}
	// Agent capability keys alone are not global auth.
	code, _ := post(t, s, "Bearer agent-key", ping)
	if code != http.StatusUnauthorized {
		t.Fatalf("capability key accepted as global secret: %d", code)
	}
}

func TestAuthAcceptsLowercaseScheme(t *testing.T) {
	s := startTest(t, echoHandler)
	code, _ := post(t, s, "bearer "+s.Secret, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
}

func TestMethodAndPathRestrictions(t *testing.T) {
	s := startTest(t, echoHandler)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		req, _ := http.NewRequest(m, s.URL, nil)
		req.Header.Set("Authorization", bearer(s))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status %d", m, resp.StatusCode)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS header emitted")
		}
	}
	base := strings.TrimSuffix(s.URL, "/mcp")
	req, _ := http.NewRequest(http.MethodPost, base+"/other", strings.NewReader("{}"))
	req.Header.Set("Authorization", bearer(s))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong path status %d", resp.StatusCode)
	}
}

func TestBrowserOriginRejected(t *testing.T) {
	s := startTest(t, echoHandler)
	req, _ := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", bearer(s))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestContentTypeRequired(t *testing.T) {
	s := startTest(t, echoHandler)
	req, _ := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", bearer(s))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestBodyLimit(t *testing.T) {
	s := startTest(t, echoHandler)
	pad := strings.Repeat("a", maxBodyBytes)
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + pad + `"}}`
	code, _ := post(t, s, bearer(s), big)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", code)
	}
	// Exactly at the limit is accepted.
	prefix := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"`
	suffix := `"}}`
	fill := strings.Repeat("a", maxBodyBytes-len(prefix)-len(suffix))
	exact := prefix + fill + suffix
	if len(exact) != maxBodyBytes {
		t.Fatalf("test construction: len %d", len(exact))
	}
	code, _ = post(t, s, bearer(s), exact)
	if code != http.StatusOK {
		t.Fatalf("at-limit status = %d, want 200", code)
	}
}

func TestShutdown(t *testing.T) {
	s, err := Start(echoHandler)
	if err != nil {
		t.Fatal(err)
	}
	url, secret := s.URL, s.Secret
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+secret)
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("request succeeded after Close")
	}
	// Port must be released.
	ln, err := net.Listen("tcp", strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/mcp"))
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	ln.Close()
}

func TestHandlerReceivesRequestFields(t *testing.T) {
	var got Request
	s := startTest(t, func(_ context.Context, r Request) (any, error) { got = r; return "ok", nil })
	rpc(t, s, "tools/call", map[string]any{
		"name":      "agents",
		"arguments": map[string]any{"key": "k1", "action": "send", "to": "bob", "text": "yo"},
	})
	want := Request{Key: "k1", Action: "send", To: "bob", Text: "yo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSendCarriesPathsAndImportCarriesTheMessage(t *testing.T) {
	var got Request
	s := startTest(t, func(_ context.Context, r Request) (any, error) { got = r; return "ok", nil })
	rpc(t, s, "tools/call", map[string]any{
		"name":      "agents",
		"arguments": map[string]any{"key": "k1", "action": "send", "to": "bob", "text": "api is ready", "paths": []string{"api/api.go", "api/doc.md"}},
	})
	if want := (Request{Key: "k1", Action: "send", To: "bob", Text: "api is ready", Paths: []string{"api/api.go", "api/doc.md"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("send: got %+v want %+v", got, want)
	}
	got = Request{}
	rpc(t, s, "tools/call", map[string]any{
		"name":      "agents",
		"arguments": map[string]any{"key": "k1", "action": "import", "message": "peer_1", "paths": []string{"api/api.go"}},
	})
	if want := (Request{Key: "k1", Action: "import", Message: "peer_1", Paths: []string{"api/api.go"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("import: got %+v want %+v", got, want)
	}
}

func TestPathsAreBoundedAndImportNeedsAMessage(t *testing.T) {
	called := false
	s := startTest(t, func(_ context.Context, r Request) (any, error) { called = true; return "ok", nil })
	many := make([]string, MaxPaths+1)
	for i := range many {
		many[i] = "f.go"
	}
	for name, args := range map[string]map[string]any{
		"too many paths":    {"key": "k", "action": "send", "to": "bob", "text": "x", "paths": many},
		"an empty path":     {"key": "k", "action": "send", "to": "bob", "text": "x", "paths": []string{""}},
		"a very long path":  {"key": "k", "action": "send", "to": "bob", "text": "x", "paths": []string{strings.Repeat("a", maxPathBytes+1)}},
		"a path with a NUL": {"key": "k", "action": "send", "to": "bob", "text": "x", "paths": []string{"a\x00b"}},
		"import no message": {"key": "k", "action": "import"},
		"unknown field":     {"key": "k", "action": "send", "to": "bob", "text": "x", "branch": "main"},
	} {
		resp := rpc(t, s, "tools/call", map[string]any{"name": "agents", "arguments": args})
		if resp["error"] == nil {
			t.Fatalf("%s: accepted: %+v", name, resp)
		}
	}
	if called {
		t.Fatal("the handler ran for an invalid call")
	}
}
