package agentmsg

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// tinyPNG is any short payload; the hub checks MIME and size, not pixels.
var tinyPNG = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nnot really"))

func startImages(t *testing.T, h ImageHandler, opts ...Option) *Server {
	t.Helper()
	s, err := Start(echoHandler, append(opts, WithImages(h))...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func callImageTool(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res := rpc(t, s, "tools/call", map[string]any{"name": imageToolName, "arguments": args})
	if e, ok := res["error"]; ok {
		return map[string]any{"rpcError": e}
	}
	return res["result"].(map[string]any)
}

func TestImageToolIsListedOnlyWhenRegistered(t *testing.T) {
	if names := toolNames(t, startTest(t, echoHandler)); strings.Contains(strings.Join(names, ","), imageToolName) {
		t.Fatalf("image tool listed without WithImages: %v", names)
	}
	s := startImages(t, func(context.Context, ImageRequest) (ImageResult, error) { return ImageResult{}, nil }, WithCanvas(canvasEcho))
	if names := toolNames(t, s); strings.Join(names, ",") != "agents,canvas,image" {
		t.Fatalf("tools = %v", names)
	}
}

func TestImageToolReturnsTextThenImagesInOrder(t *testing.T) {
	var got ImageRequest
	s := startImages(t, func(_ context.Context, req ImageRequest) (ImageResult, error) {
		got = req
		return ImageResult{
			Text: map[string]any{"images": []string{"a.png", "b.jpg"}},
			Images: []Image{
				{MIME: "image/png", Data: tinyPNG},
				{MIME: "image/gif", Data: tinyPNG},  // not a type the hub sends
				{MIME: "image/jpeg", Data: "%%%"},   // not base64
				{MIME: "image/jpeg", Data: tinyPNG}, // kept, after the png
			},
		}, nil
	})
	res := callImageTool(t, s, map[string]any{"key": "agent-key", "paths": []string{"a.png", "b.jpg"}, "region": map[string]any{"x": 0, "y": 10, "width": 20, "height": 30}, "maxEdge": 800})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	if got.Key != "agent-key" || strings.Join(got.Paths, ",") != "a.png,b.jpg" || got.Region == nil || *got.Region != (ImageRegion{Y: 10, Width: 20, Height: 30}) || got.MaxEdge != 800 {
		t.Fatalf("handler received %+v", got)
	}
	content := res["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v, want text and two images", content)
	}
	if first := content[0].(map[string]any); first["type"] != "text" || !strings.Contains(first["text"].(string), "b.jpg") || strings.Contains(first["text"].(string), tinyPNG) {
		t.Fatalf("text block = %v", first)
	}
	for i, mime := range []string{"image/png", "image/jpeg"} {
		block := content[i+1].(map[string]any)
		if block["type"] != "image" || block["mimeType"] != mime || block["data"] != tinyPNG {
			t.Fatalf("block %d = %v, want %s image", i+1, block, mime)
		}
	}
}

func TestImageToolErrorsAndPanicsAreToolErrors(t *testing.T) {
	s := startImages(t, func(_ context.Context, req ImageRequest) (ImageResult, error) {
		if req.Paths[0] == "panic.png" {
			panic("boom")
		}
		return ImageResult{}, errors.New("your model cannot see images")
	})
	res := callImageTool(t, s, map[string]any{"key": "k", "paths": []string{"x.png"}})
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "cannot see") {
		t.Fatalf("handler error = %v", res)
	}
	res = callImageTool(t, s, map[string]any{"key": "k", "paths": []string{"panic.png"}})
	if res["isError"] != true {
		t.Fatalf("panic = %v", res)
	}
}

func TestImageToolValidatesArguments(t *testing.T) {
	called := false
	s := startImages(t, func(context.Context, ImageRequest) (ImageResult, error) {
		called = true
		return ImageResult{}, nil
	})
	for name, args := range map[string]map[string]any{
		"no key":        {"paths": []string{"a.png"}},
		"no paths":      {"key": "k", "paths": []string{}},
		"five paths":    {"key": "k", "paths": []string{"1", "2", "3", "4", "5"}},
		"empty path":    {"key": "k", "paths": []string{""}},
		"bad region":    {"key": "k", "paths": []string{"a.png"}, "region": map[string]any{"x": -1, "y": 0, "width": 5, "height": 5}},
		"empty region":  {"key": "k", "paths": []string{"a.png"}, "region": map[string]any{"x": 0, "y": 0, "width": 0, "height": 5}},
		"small edge":    {"key": "k", "paths": []string{"a.png"}, "maxEdge": 10},
		"huge edge":     {"key": "k", "paths": []string{"a.png"}, "maxEdge": 9000},
		"unknown field": {"key": "k", "paths": []string{"a.png"}, "path": "a.png"},
	} {
		if res := callImageTool(t, s, args); res["rpcError"] == nil {
			t.Errorf("%s: accepted: %v", name, res)
		}
	}
	if called {
		t.Fatal("the handler ran for an invalid request")
	}
}
