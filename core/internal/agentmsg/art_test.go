package agentmsg

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func startArt(t *testing.T, h ArtHandler, opts ...Option) *Server {
	t.Helper()
	s, err := Start(echoHandler, append(opts, WithArt(h))...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func callArtTool(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res := rpc(t, s, "tools/call", map[string]any{"name": artToolName, "arguments": args})
	if e, ok := res["error"]; ok {
		return map[string]any{"rpcError": e}
	}
	return res["result"].(map[string]any)
}

func nopArt(context.Context, ArtRequest) (ImageResult, error) { return ImageResult{}, nil }

func TestArtToolIsListedOnlyWhenRegistered(t *testing.T) {
	if names := toolNames(t, startTest(t, echoHandler)); strings.Contains(strings.Join(names, ","), artToolName) {
		t.Fatalf("art tool listed without WithArt: %v", names)
	}
	s := startArt(t, nopArt, WithSprite(nopSprite))
	if names := toolNames(t, s); strings.Join(names, ",") != "agents,sprite,art" {
		t.Fatalf("tools = %v", names)
	}
}

func TestArtToolPassesTheRequestAndReturnsTextThenPictures(t *testing.T) {
	var got ArtRequest
	s := startArt(t, func(_ context.Context, req ArtRequest) (ImageResult, error) {
		got = req
		return ImageResult{Text: map[string]any{"state": "ready"}, Images: []Image{{MIME: "image/png", Data: tinyPNG}}}, nil
	})
	res := callArtTool(t, s, map[string]any{
		"key": "agent-key", "action": "generate",
		"prompt":  "a single iron ingot, flat solid magenta background",
		"refs":    []any{"r_1", "r_2"},
		"quality": "low", "size": "1536x1024", "background": "transparent", "count": 3,
	})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	content := res["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("content = %v", content)
	}
	if got.Key != "agent-key" || got.Action != "generate" || len(got.Refs) != 2 || got.Refs[1] != "r_2" ||
		got.Quality != "low" || got.Size != "1536x1024" || got.Background != "transparent" || got.Count != 3 {
		t.Fatalf("request = %+v", got)
	}

	callArtTool(t, s, map[string]any{"key": "k", "action": "wait", "job": "j3", "waitSeconds": 30})
	if got.Action != "wait" || got.Job != "j3" || got.WaitSeconds != 30 {
		t.Fatalf("request = %+v", got)
	}
	callArtTool(t, s, map[string]any{"key": "k", "action": "refs", "sources": []any{
		map[string]any{"path": "art/ref/iron.png"}, map[string]any{"job": "j2", "index": 3}}})
	if len(got.Sources) != 2 || got.Sources[0].Path != "art/ref/iron.png" || got.Sources[1].Job != "j2" || got.Sources[1].Index != 3 {
		t.Fatalf("request = %+v", got)
	}
}

func TestArtToolRejectsMalformedRequests(t *testing.T) {
	called := false
	s := startArt(t, func(context.Context, ArtRequest) (ImageResult, error) { called = true; return ImageResult{}, nil })
	cases := map[string]map[string]any{
		"no key":                {"action": "status"},
		"no action":             {"key": "k"},
		"unknown action":        {"key": "k", "action": "import"},
		"retired action vary":   {"key": "k", "action": "vary", "job": "j1", "index": 1},
		"retired action budget": {"key": "k", "action": "budget"},
		"retired field params":  {"key": "k", "action": "generate", "prompt": "x", "params": map[string]any{"aspect": "1:1"}},
		"unknown field":         {"key": "k", "action": "status", "extra": 1},
		"key too long":          {"key": strings.Repeat("k", 257), "action": "status"},
		"generate no prompt":    {"key": "k", "action": "generate"},
		"generate blank prompt": {"key": "k", "action": "generate", "prompt": "   "},
		"prompt too long":       {"key": "k", "action": "generate", "prompt": strings.Repeat("x", maxArtPrompt+1)},
		"too many refs":         {"key": "k", "action": "generate", "prompt": "x", "refs": make([]any, MaxArtRefs+1)},
		"ref id empty":          {"key": "k", "action": "generate", "prompt": "x", "refs": []any{""}},
		"ref is an object":      {"key": "k", "action": "generate", "prompt": "x", "refs": []any{map[string]any{"id": "r", "role": "style"}}},
		"count too many":        {"key": "k", "action": "generate", "prompt": "x", "count": 5},
		"count negative":        {"key": "k", "action": "generate", "prompt": "x", "count": -1},
		"quality not in list":   {"key": "k", "action": "generate", "prompt": "x", "quality": strings.Repeat("q", 21)},
		"wait no job":           {"key": "k", "action": "wait"},
		"wait too long":         {"key": "k", "action": "wait", "job": "j1", "waitSeconds": artMaxWait + 1},
		"cancel no job":         {"key": "k", "action": "cancel"},
		"refs no sources":       {"key": "k", "action": "refs"},
		"refs empty source":     {"key": "k", "action": "refs", "sources": []any{map[string]any{}}},
		"refs path and job":     {"key": "k", "action": "refs", "sources": []any{map[string]any{"path": "a.png", "job": "j1", "index": 1}}},
		"refs job no index":     {"key": "k", "action": "refs", "sources": []any{map[string]any{"job": "j1"}}},
		"refs too many":         {"key": "k", "action": "refs", "sources": make([]any, MaxArtSources+1)},
		"refs bad path":         {"key": "k", "action": "refs", "sources": []any{map[string]any{"path": strings.Repeat("a", maxPathBytes+1)}}},
		"limit too big":         {"key": "k", "action": "jobs", "limit": maxArtJobs + 1},
		"topic too long":        {"key": "k", "action": "playbook", "topic": strings.Repeat("t", 41)},
		"waitSeconds a float":   {"key": "k", "action": "wait", "job": "j1", "waitSeconds": 1.5},
	}
	for name, args := range cases {
		res := callArtTool(t, s, args)
		if _, ok := res["rpcError"]; !ok {
			t.Errorf("%s: want an RPC error, got %v", name, res)
		}
	}
	if called {
		t.Fatal("the handler ran for a request that failed validation")
	}
}

func TestArtToolAcceptsEveryActionWithItsRequiredFields(t *testing.T) {
	var seen []string
	s := startArt(t, func(_ context.Context, r ArtRequest) (ImageResult, error) {
		seen = append(seen, r.Action)
		return ImageResult{}, nil
	})
	valid := []map[string]any{
		{"key": "k", "action": "status"},
		{"key": "k", "action": "playbook"},
		{"key": "k", "action": "playbook", "topic": "refs"},
		{"key": "k", "action": "refs", "sources": []any{map[string]any{"path": "a.png"}}},
		{"key": "k", "action": "generate", "prompt": "an ingot"},
		{"key": "k", "action": "wait", "job": "j1"},
		{"key": "k", "action": "generate", "prompt": "an ingot", "refs": []any{"r_1"}, "quality": "high", "size": "auto", "background": "opaque", "count": 1},
		{"key": "k", "action": "cancel", "job": "j1"},
		{"key": "k", "action": "jobs", "limit": 5},
	}
	for _, args := range valid {
		res := callArtTool(t, s, args)
		if res["isError"] != false {
			t.Errorf("%v: %v", args, res)
		}
	}
	if len(seen) != len(valid) {
		t.Fatalf("handler saw %v", seen)
	}
	if len(ArtActions) != 7 {
		t.Fatalf("actions %v", ArtActions)
	}
}

func TestArtToolReportsHandlerFailuresAsToolErrors(t *testing.T) {
	for name, h := range map[string]ArtHandler{
		"error": func(context.Context, ArtRequest) (ImageResult, error) {
			return ImageResult{}, errors.New("not enough budget for this job")
		},
		"panic": func(context.Context, ArtRequest) (ImageResult, error) { panic("boom") },
	} {
		s := startArt(t, h)
		res := callArtTool(t, s, map[string]any{"key": "k", "action": "status"})
		if res["isError"] != true {
			t.Fatalf("%s: result = %v", name, res)
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if name == "error" && !strings.Contains(text, "budget") || name == "panic" && text != "art handler failed" {
			t.Fatalf("%s: text = %q", name, text)
		}
	}
}

func TestArtDescriptorListsEveryAction(t *testing.T) {
	d := artToolDescriptor()
	props := d["inputSchema"].(map[string]any)["properties"].(map[string]any)
	enum := props["action"].(map[string]any)["enum"].([]string)
	if strings.Join(enum, ",") != strings.Join(ArtActions, ",") {
		t.Fatalf("enum = %v", enum)
	}
	for _, field := range []string{"sources", "prompt", "refs", "quality", "size", "background", "count", "job", "waitSeconds", "limit", "topic"} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema lacks %s", field)
		}
	}
	if d["inputSchema"].(map[string]any)["additionalProperties"] != false {
		t.Fatal("schema allows unknown fields")
	}
	if desc := d["description"].(string); !strings.Contains(desc, "costs real money") || strings.Contains(desc, "Midjourney") {
		t.Fatal("the description does not warn that jobs cost money, or still names another generator")
	}
}
