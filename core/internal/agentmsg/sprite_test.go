package agentmsg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/sprite"
)

func startSprite(t *testing.T, h SpriteHandler, opts ...Option) *Server {
	t.Helper()
	s, err := Start(echoHandler, append(opts, WithSprite(h))...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func callSpriteTool(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res := rpc(t, s, "tools/call", map[string]any{"name": spriteToolName, "arguments": args})
	if e, ok := res["error"]; ok {
		return map[string]any{"rpcError": e}
	}
	return res["result"].(map[string]any)
}

func nopSprite(context.Context, SpriteRequest) (ImageResult, error) { return ImageResult{}, nil }

func TestSpriteToolIsListedOnlyWhenRegistered(t *testing.T) {
	if names := toolNames(t, startTest(t, echoHandler)); strings.Contains(strings.Join(names, ","), spriteToolName) {
		t.Fatalf("sprite tool listed without WithSprite: %v", names)
	}
	s := startSprite(t, nopSprite, WithCanvas(canvasEcho), WithImages(func(context.Context, ImageRequest) (ImageResult, error) { return ImageResult{}, nil }))
	if names := toolNames(t, s); strings.Join(names, ",") != "agents,canvas,image,sprite" {
		t.Fatalf("tools = %v", names)
	}
}

func TestSpriteToolPassesTheRequestAndReturnsTextThenPreview(t *testing.T) {
	var got SpriteRequest
	s := startSprite(t, func(_ context.Context, req SpriteRequest) (ImageResult, error) {
		got = req
		return ImageResult{Text: map[string]any{"frames": 1}, Images: []Image{{MIME: "image/png", Data: tinyPNG}}}, nil
	})
	res := callSpriteTool(t, s, map[string]any{
		"key":   "agent-key",
		"input": map[string]any{"path": "art/raw/1.png"},
		"ops": []any{
			map[string]any{"op": "matte", "color": "#ff00ff", "tolerance": 30, "holes": true},
			map[string]any{"op": "fit", "size": []int{128, 128}, "margin": 4, "anchor": "bottom"},
			map[string]any{"op": "check", "rules": map[string]any{"width": 128, "alpha": true}, "strict": true},
		},
		"output":  "Textures/iron.png",
		"preview": true,
	})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	content := res["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("content = %v", content)
	}
	if got.Key != "agent-key" || got.Input.Path != "art/raw/1.png" || got.Output != "Textures/iron.png" || !got.Preview || len(got.Ops) != 3 {
		t.Fatalf("request = %+v", got)
	}
	matte, fit, check := got.Ops[0], got.Ops[1], got.Ops[2]
	if matte.Color != "#ff00ff" || *matte.Tolerance != 30 || !matte.Holes || fit.Size[0] != 128 || fit.Margin != 4 || fit.Anchor != "bottom" || check.Rules.Width != 128 || !check.Rules.Alpha || !check.Strict {
		t.Fatalf("ops = %+v", got.Ops)
	}
}

func TestSpriteToolRejectsMalformedRequests(t *testing.T) {
	called := false
	s := startSprite(t, func(context.Context, SpriteRequest) (ImageResult, error) { called = true; return ImageResult{}, nil })
	valid := func() map[string]any {
		return map[string]any{"key": "k", "input": map[string]any{"path": "a.png"}}
	}
	cases := map[string]func(map[string]any){
		"no key":         func(a map[string]any) { delete(a, "key") },
		"no input":       func(a map[string]any) { delete(a, "input") },
		"empty path":     func(a map[string]any) { a["input"] = map[string]any{"path": ""} },
		"unknown field":  func(a map[string]any) { a["extra"] = 1 },
		"unknown input":  func(a map[string]any) { a["input"] = map[string]any{"path": "a.png", "job": "j"} },
		"unknown op":     func(a map[string]any) { a["ops"] = []any{map[string]any{"op": "sparkle"}} },
		"unknown op key": func(a map[string]any) { a["ops"] = []any{map[string]any{"op": "trim", "bogus": 1}} },
		"unknown rule": func(a map[string]any) {
			a["ops"] = []any{map[string]any{"op": "check", "rules": map[string]any{"bogus": 1}}}
		},
		"output not png":   func(a map[string]any) { a["output"] = "out.jpg" },
		"too many ops":     func(a map[string]any) { a["ops"] = make([]any, sprite.MaxOps+1) },
		"key too long":     func(a map[string]any) { a["key"] = strings.Repeat("k", 257) },
		"op wrong type":    func(a map[string]any) { a["ops"] = []any{map[string]any{"op": "fit", "size": "big"}} },
		"preview a string": func(a map[string]any) { a["preview"] = "yes" },
	}
	for name, mutate := range cases {
		args := valid()
		mutate(args)
		res := callSpriteTool(t, s, args)
		if _, ok := res["rpcError"]; !ok {
			t.Errorf("%s: want an RPC error, got %v", name, res)
		}
	}
	if called {
		t.Fatal("the handler ran for a request that failed validation")
	}
}

func TestSpriteToolReportsHandlerFailuresAsToolErrors(t *testing.T) {
	for name, h := range map[string]SpriteHandler{
		"error": func(context.Context, SpriteRequest) (ImageResult, error) {
			return ImageResult{}, errors.New("step 2 (fit): frame 1 has nothing visible to fit")
		},
		"panic": func(context.Context, SpriteRequest) (ImageResult, error) { panic("boom") },
	} {
		s := startSprite(t, h)
		res := callSpriteTool(t, s, map[string]any{"key": "k", "input": map[string]any{"path": "a.png"}})
		if res["isError"] != true {
			t.Fatalf("%s: result = %v", name, res)
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if name == "error" && !strings.Contains(text, "nothing visible") || name == "panic" && text != "sprite handler failed" {
			t.Fatalf("%s: text = %q", name, text)
		}
	}
}

func TestSpriteDescriptorNamesEveryOp(t *testing.T) {
	d := spriteToolDescriptor()
	schema := d["inputSchema"].(map[string]any)["properties"].(map[string]any)
	items := schema["ops"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	enum := items["op"].(map[string]any)["enum"].([]string)
	if strings.Join(enum, ",") != strings.Join(sprite.OpNames(), ",") {
		t.Fatalf("enum = %v", enum)
	}
	if _, ok := items["rules"]; !ok {
		t.Fatal("rules missing from the op schema")
	}
}
