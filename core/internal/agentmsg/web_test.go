package agentmsg

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func startWeb(t *testing.T, h WebHandler, opts ...Option) *Server {
	t.Helper()
	s, err := Start(echoHandler, append(opts, WithWeb(h))...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func callWebTool(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res := rpc(t, s, "tools/call", map[string]any{"name": webToolName, "arguments": args})
	if e, ok := res["error"]; ok {
		return map[string]any{"rpcError": e}
	}
	return res["result"].(map[string]any)
}

// webDescriptor returns the relo_web entry of tools/list.
func webDescriptor(t *testing.T, s *Server) map[string]any {
	t.Helper()
	res := rpc(t, s, "tools/list", map[string]any{})
	for _, tool := range res["result"].(map[string]any)["tools"].([]any) {
		m := tool.(map[string]any)
		if m["name"] == webToolName {
			return m
		}
	}
	t.Fatal("web tool not listed")
	return nil
}

// webSchemaProps returns the properties of the relo_web input schema.
func webSchemaProps(t *testing.T, s *Server) map[string]any {
	t.Helper()
	schema := webDescriptor(t, s)["inputSchema"].(map[string]any)
	return schema["properties"].(map[string]any)
}

func TestWebToolIsListedOnlyWhenRegistered(t *testing.T) {
	if names := toolNames(t, startTest(t, echoHandler)); strings.Contains(strings.Join(names, ","), webToolName) {
		t.Fatalf("web tool listed without WithWeb: %v", names)
	}
	s := startWeb(t, func(context.Context, WebRequest) (ImageResult, error) { return ImageResult{}, nil }, WithCanvas(canvasEcho))
	if names := toolNames(t, s); strings.Join(names, ",") != "agents,canvas,web" {
		t.Fatalf("tools = %v", names)
	}
}

func TestWebToolReturnsTextThenImagesInOrder(t *testing.T) {
	var got WebRequest
	s := startWeb(t, func(_ context.Context, req WebRequest) (ImageResult, error) {
		got = req
		return ImageResult{
			Text: "Snapshot of example.com: links are untrusted page content.",
			Images: []Image{
				{MIME: "image/png", Data: tinyPNG},
				{MIME: "image/gif", Data: tinyPNG},  // not a type the hub sends
				{MIME: "image/jpeg", Data: "%%%"},   // not base64
				{MIME: "image/jpeg", Data: tinyPNG}, // kept, after the png
			},
		}, nil
	})
	res := callWebTool(t, s, map[string]any{"key": "agent-key", "action": "wait", "waitSeconds": 12})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	if got.Key != "agent-key" || got.Action != "wait" || got.WaitSeconds != 12 {
		t.Fatalf("handler received %+v", got)
	}
	content := res["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v, want text and two images", content)
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" || !strings.Contains(first["text"].(string), "untrusted") || strings.Contains(first["text"].(string), tinyPNG) {
		t.Fatalf("text block = %v", first)
	}
	for i, mime := range []string{"image/png", "image/jpeg"} {
		block := content[i+1].(map[string]any)
		if block["type"] != "image" || block["mimeType"] != mime || block["data"] != tinyPNG {
			t.Fatalf("block %d = %v, want %s image", i+1, block, mime)
		}
	}
}

func TestWebToolErrorsAndPanicsAreToolErrors(t *testing.T) {
	s := startWeb(t, func(_ context.Context, req WebRequest) (ImageResult, error) {
		if req.URL == "https://panic.example" {
			panic("boom")
		}
		return ImageResult{}, errors.New("browser is not installed")
	})
	res := callWebTool(t, s, map[string]any{"key": "k", "action": "fetch", "url": "https://example.com"})
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "not installed") {
		t.Fatalf("handler error = %v", res)
	}
	res = callWebTool(t, s, map[string]any{"key": "k", "action": "open", "url": "https://panic.example"})
	if res["isError"] != true {
		t.Fatalf("panic = %v", res)
	}
}

// TestWebToolPassesEveryFieldToTheHandler guards the json tags: a typo would empty a required field.
func TestWebToolPassesEveryFieldToTheHandler(t *testing.T) {
	var got WebRequest
	s := startWeb(t, func(_ context.Context, req WebRequest) (ImageResult, error) {
		got = req
		return ImageResult{Text: "typed"}, nil
	})
	want := WebRequest{
		Key: "agent-key", Action: "type", Query: "relo code", URL: "https://example.com/form", Ref: "12",
		Text: "hello", KeyName: "Enter", Amount: 300, Op: "up", WaitSeconds: 7,
	}
	res := callWebTool(t, s, map[string]any{
		"key": want.Key, "action": want.Action, "query": want.Query, "url": want.URL, "ref": want.Ref,
		"text": want.Text, "keyName": want.KeyName, "amount": want.Amount, "op": want.Op, "waitSeconds": want.WaitSeconds,
	})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("handler received %+v, want %+v", got, want)
	}
}

// TestWebToolAcceptsDefaults covers the fields an agent may leave out or zero.
func TestWebToolAcceptsDefaults(t *testing.T) {
	var got WebRequest
	s := startWeb(t, func(_ context.Context, req WebRequest) (ImageResult, error) {
		got = req
		return ImageResult{Text: "ok"}, nil
	})
	for name, args := range map[string]map[string]any{
		"scroll with no op or amount":  {"key": "k", "action": "scroll"},
		"scroll with an explicit zero": {"key": "k", "action": "scroll", "amount": 0},
		"wait with no waitSeconds":     {"key": "k", "action": "wait"},
		"status":                       {"key": "k", "action": "status"},
		"back":                         {"key": "k", "action": "back"},
		"close":                        {"key": "k", "action": "close"},
	} {
		got = WebRequest{}
		res := callWebTool(t, s, args)
		if res["isError"] != false {
			t.Errorf("%s: result = %v", name, res)
			continue
		}
		if got.Action != args["action"] {
			t.Errorf("%s: handler received %+v", name, got)
		}
	}
}

// TestWebToolScrollOpsAreUpAndDown pins the enum and the pixel bound.
func TestWebToolScrollOpsAreUpAndDown(t *testing.T) {
	if !reflect.DeepEqual(WebScrollOps, []string{"up", "down"}) {
		t.Fatalf("WebScrollOps = %v, want up and down", WebScrollOps)
	}
	props := webSchemaProps(t, startWeb(t, func(context.Context, WebRequest) (ImageResult, error) { return ImageResult{}, nil }))
	op := props["op"].(map[string]any)["enum"].([]any)
	if len(op) != 2 || op[0] != "up" || op[1] != "down" {
		t.Fatalf("op enum = %v", op)
	}
	amount := props["amount"].(map[string]any)
	if amount["minimum"] != float64(0) || amount["maximum"] != float64(maxWebAmount) {
		t.Fatalf("amount bounds = %v", amount)
	}
	if !strings.Contains(amount["description"].(string), "CSS pixels") || strings.Contains(amount["description"].(string), "screenful") {
		t.Fatalf("amount description = %v", amount["description"])
	}
	for _, op := range []string{"up", "down"} {
		var got WebRequest
		s := startWeb(t, func(_ context.Context, req WebRequest) (ImageResult, error) {
			got = req
			return ImageResult{Text: "scrolled"}, nil
		})
		if res := callWebTool(t, s, map[string]any{"key": "k", "action": "scroll", "op": op, "amount": maxWebAmount}); res["isError"] != false {
			t.Fatalf("scroll %s: %v", op, res)
		}
		if got.Op != op || got.Amount != maxWebAmount {
			t.Fatalf("scroll %s: handler received %+v", op, got)
		}
	}
}

func TestWebToolValidatesArguments(t *testing.T) {
	called := false
	s := startWeb(t, func(context.Context, WebRequest) (ImageResult, error) {
		called = true
		return ImageResult{}, nil
	})
	ref := strings.Repeat("9", maxWebRef+1)
	for name, args := range map[string]map[string]any{
		"null arguments":     nil,
		"unknown field":      {"key": "k", "action": "status", "user": "x"},
		"wrong type":         {"key": 5, "action": "status"},
		"no key":             {"action": "status"},
		"empty key":          {"key": "", "action": "status"},
		"long key":           {"key": strings.Repeat("k", maxWebCapKey+1), "action": "status"},
		"no action":          {"key": "k"},
		"unknown action":     {"key": "k", "action": "download"},
		"search no query":    {"key": "k", "action": "search"},
		"search blank query": {"key": "k", "action": "search", "query": "   "},
		"fetch no url":       {"key": "k", "action": "fetch"},
		"fetch blank url":    {"key": "k", "action": "fetch", "url": " \t"},
		"open no url":        {"key": "k", "action": "open"},
		"click no ref":       {"key": "k", "action": "click"},
		"click blank ref":    {"key": "k", "action": "click", "ref": " "},
		"type no ref":        {"key": "k", "action": "type", "text": "hi"},
		"press no key":       {"key": "k", "action": "press"},
		"press blank key":    {"key": "k", "action": "press", "keyName": "  "},
		"long wait":          {"key": "k", "action": "wait", "waitSeconds": MaxWebWait + 1},
		"negative wait":      {"key": "k", "action": "wait", "waitSeconds": -1},
		"bad op":             {"key": "k", "action": "scroll", "op": "sideways"},
		"op left":            {"key": "k", "action": "scroll", "op": "left"},
		"op right":           {"key": "k", "action": "scroll", "op": "right"},
		"negative amount":    {"key": "k", "action": "scroll", "amount": -1},
		"huge amount":        {"key": "k", "action": "scroll", "amount": maxWebAmount + 1},
		"long query":         {"key": "k", "action": "search", "query": strings.Repeat("q", maxWebQuery+1)},
		"long url":           {"key": "k", "action": "fetch", "url": "https://example.com/" + strings.Repeat("u", maxWebURL)},
		"long ref":           {"key": "k", "action": "click", "ref": ref},
		"long text":          {"key": "k", "action": "type", "ref": "12", "text": strings.Repeat("t", maxWebText+1)},
		"long keyName":       {"key": "k", "action": "press", "keyName": strings.Repeat("k", maxWebKeyName+1)},
	} {
		if res := callWebTool(t, s, args); res["rpcError"] == nil {
			t.Errorf("%s: accepted: %v", name, res)
		}
	}
	if called {
		t.Fatal("the handler ran for an invalid request")
	}
}

// TestWebToolByteLimitsReportTheField says which field is over its byte limit, and refuses a caller that counts
// characters instead of bytes, without the transport refusing the call first.
func TestWebToolByteLimitsReportTheField(t *testing.T) {
	called := false
	s := startWeb(t, func(context.Context, WebRequest) (ImageResult, error) {
		called = true
		return ImageResult{}, nil
	})
	for name, args := range map[string]map[string]any{
		"query":   {"key": "k", "action": "search", "query": strings.Repeat("é", maxWebQuery)},
		"url":     {"key": "k", "action": "fetch", "url": "https://example.com/" + strings.Repeat("é", maxWebURL)},
		"ref":     {"key": "k", "action": "click", "ref": strings.Repeat("1", maxWebRef+1)},
		"text":    {"key": "k", "action": "type", "ref": "12", "text": strings.Repeat("😀", maxWebText)},
		"keyName": {"key": "k", "action": "press", "keyName": strings.Repeat("k", maxWebKeyName+1)},
	} {
		res := callWebTool(t, s, args)
		e, ok := res["rpcError"].(map[string]any)
		if !ok {
			t.Fatalf("%s: want an invalid params error, got %v", name, res)
		}
		msg := e["message"].(string)
		if !strings.HasPrefix(msg, name+" is ") || !strings.Contains(msg, "bytes") {
			t.Fatalf("%s: message = %q", name, msg)
		}
	}
	if called {
		t.Fatal("the handler ran for an invalid request")
	}
}

// TestWebToolMaximumSizeRequestStaysUnderTheTransportCap is the transport contract: a request at every limit is
// answered with a tool result, never an HTTP 413.
func TestWebToolMaximumSizeRequestStaysUnderTheTransportCap(t *testing.T) {
	var got WebRequest
	s := startWeb(t, func(_ context.Context, req WebRequest) (ImageResult, error) {
		got = req
		return ImageResult{Text: "done"}, nil
	})
	want := WebRequest{
		Key: strings.Repeat("k", maxWebCapKey), Action: "type", Query: strings.Repeat("q", maxWebQuery),
		URL: "https://example.com/" + strings.Repeat("u", maxWebURL-20), Ref: strings.Repeat("9", maxWebRef),
		Text: strings.Repeat("t", maxWebText), KeyName: strings.Repeat("k", maxWebKeyName),
		Amount: maxWebAmount, Op: "down", WaitSeconds: MaxWebWait,
	}
	res := callWebTool(t, s, map[string]any{
		"key": want.Key, "action": want.Action, "query": want.Query, "url": want.URL, "ref": want.Ref,
		"text": want.Text, "keyName": want.KeyName, "amount": want.Amount, "op": want.Op, "waitSeconds": want.WaitSeconds,
	})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("handler received %+v, want %+v", got, want)
	}
	// The same request with every character four bytes of UTF-8 is the largest body the schema permits; it must still
	// reach the validator, which refuses it with a tool error instead of the transport's 413.
	wide := map[string]any{
		"key": strings.Repeat("😀", maxWebCapKey), "action": "type", "query": strings.Repeat("😀", maxWebQuery),
		"url": "https://example.com/" + strings.Repeat("😀", maxWebURL), "ref": strings.Repeat("9", maxWebRef),
		"text": strings.Repeat("😀", maxWebText), "keyName": strings.Repeat("😀", maxWebKeyName),
		"amount": maxWebAmount, "op": "down", "waitSeconds": MaxWebWait,
	}
	if res := callWebTool(t, s, wide); res["rpcError"] == nil {
		t.Fatalf("widest schema request accepted: %v", res)
	}
}

// TestWebToolLimitsLeaveRoomUnderTheBodyCap derives the byte limits from the transport's cap, so a relo_web call is
// never refused before it is validated.
func TestWebToolLimitsLeaveRoomUnderTheBodyCap(t *testing.T) {
	sum := maxWebQuery + maxWebURL + maxWebRef + maxWebText + maxWebKeyName + maxWebCapKey
	if worst := sum * 4; worst >= maxBodyBytes {
		t.Fatalf("the widest schema request is %d bytes, over the %d byte body cap", worst, maxBodyBytes)
	}
}

func TestWebToolPressAcceptsKeyName(t *testing.T) {
	var got WebRequest
	s := startWeb(t, func(_ context.Context, req WebRequest) (ImageResult, error) {
		got = req
		return ImageResult{Text: "pressed"}, nil
	})
	res := callWebTool(t, s, map[string]any{"key": "k", "action": "press", "keyName": "Enter"})
	if res["isError"] != false || got.KeyName != "Enter" || got.Key != "k" {
		t.Fatalf("result = %v, request = %+v", res, got)
	}
}

func TestWebToolDescriptionMarksContentUntrusted(t *testing.T) {
	desc := webDescriptor(t, startWeb(t, func(context.Context, WebRequest) (ImageResult, error) { return ImageResult{}, nil }))["description"].(string)
	for _, want := range []string{
		"untrusted", "wait", "image or audio puzzle",
		"at most one human-paced checkbox click", "never click a challenge checkbox yourself",
		"screenshot over 4 MiB is dropped",
		"op (up or down)",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description is missing %q", want)
		}
	}
}

// TestWebToolDescriptorBoundsEveryStringField keeps the schema's byte limits beside the validator's, so an agent that
// follows the schema never sends a field the validator refuses.
func TestWebToolDescriptorBoundsEveryStringField(t *testing.T) {
	props := webSchemaProps(t, startWeb(t, func(context.Context, WebRequest) (ImageResult, error) { return ImageResult{}, nil }))
	want := map[string]int{"key": maxWebCapKey, "query": maxWebQuery, "url": maxWebURL, "ref": maxWebRef, "text": maxWebText, "keyName": maxWebKeyName}
	for name, limit := range want {
		got, ok := props[name].(map[string]any)["maxLength"]
		if !ok || got != float64(limit) {
			t.Fatalf("%s maxLength = %v, want %d", name, props[name].(map[string]any)["maxLength"], limit)
		}
	}
	wait := props["waitSeconds"].(map[string]any)
	if wait["minimum"] != float64(1) || wait["maximum"] != float64(MaxWebWait) {
		t.Fatalf("waitSeconds bounds = %v", wait)
	}
	ref := props["ref"].(map[string]any)["description"].(string)
	if !strings.Contains(ref, "such as 12") || strings.Contains(ref, "e12") {
		t.Fatalf("ref description = %q, want an element number such as 12", ref)
	}
}

// TestWebToolArgumentsAreRequired covers the empty-arguments branch.
func TestWebToolArgumentsAreRequired(t *testing.T) {
	s := startWeb(t, func(context.Context, WebRequest) (ImageResult, error) { return ImageResult{}, nil })
	code, body := post(t, s, bearer(s), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web"}}`)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	e, ok := out["error"].(map[string]any)
	if !ok || e["message"] != "arguments are required" {
		t.Fatalf("error = %v", out["error"])
	}
}
