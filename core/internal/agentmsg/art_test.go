package agentmsg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/art"
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

func TestArtPromptParamHints(t *testing.T) {
	for prompt, want := range map[string]string{"rock --ar 1:1": "params.aspect_ratio", "rock --aspect 1:1": "params.aspect_ratio", "rock --seed 4": "params.seed"} {
		if err := ArtPromptError(prompt); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("hint for %q: %v", prompt, err)
		}
	}
}

func TestArtParameterValidationChecksShapeNotModelValues(t *testing.T) {
	seed := int64(0)
	request := ArtRequest{Key: "k", Action: "generate", Prompt: "p", Params: ArtParams{AspectRatio: "123:456", Resolution: "AbC123", Seed: &seed}}
	if err := validateArtRequest(request); err != nil {
		t.Fatalf("valid shapes refused: %v", err)
	}
	request.Params.Resolution = strings.Repeat("A", 17)
	if err := validateArtRequest(request); err == nil || !strings.Contains(err.Error(), "resolution") {
		t.Fatalf("long resolution accepted: %v", err)
	}
}

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
		"params": map[string]any{"aspect_ratio": "16:9", "resolution": "2K", "seed": 0},
	})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	content := res["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("content = %v", content)
	}
	if got.Key != "agent-key" || got.Action != "generate" || len(got.Refs) != 2 || got.Refs[1].ID != "r_2" ||
		got.Quality != "low" || got.Size != "1536x1024" || got.Background != "transparent" || got.Count != 3 ||
		got.Params.AspectRatio != "16:9" || got.Params.Resolution != "2K" || got.Params.Seed == nil || *got.Params.Seed != 0 {
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
		"invalid aspect ratio":  {"key": "k", "action": "generate", "prompt": "x", "params": map[string]any{"aspect_ratio": "16/9"}},
		"invalid resolution":    {"key": "k", "action": "generate", "prompt": "x", "params": map[string]any{"resolution": "2 K"}},
		"negative seed":         {"key": "k", "action": "generate", "prompt": "x", "params": map[string]any{"seed": -1}},
		"fractional seed":       {"key": "k", "action": "generate", "prompt": "x", "params": map[string]any{"seed": 1.5}},
		"unknown field":         {"key": "k", "action": "status", "extra": 1},
		"key too long":          {"key": strings.Repeat("k", 257), "action": "status"},
		"generate no prompt":    {"key": "k", "action": "generate"},
		"generate blank prompt": {"key": "k", "action": "generate", "prompt": "   "},
		"prompt too long":       {"key": "k", "action": "generate", "prompt": strings.Repeat("x", maxArtPrompt+1)},
		"too many refs":         {"key": "k", "action": "generate", "prompt": "x", "refs": make([]any, MaxArtRefs+1)},
		"ref id empty":          {"key": "k", "action": "generate", "prompt": "x", "refs": []any{""}},
		"ref role unknown":      {"key": "k", "action": "generate", "prompt": "x", "refs": []any{map[string]any{"id": "r", "role": "hat"}}},
		"ref weight too big":    {"key": "k", "action": "generate", "prompt": "x", "refs": []any{map[string]any{"id": "r", "weight": 1.5}}},
		"ref weight negative":   {"key": "k", "action": "generate", "prompt": "x", "refs": []any{map[string]any{"id": "r", "weight": -0.1}}},
		"ref no id":             {"key": "k", "action": "generate", "prompt": "x", "refs": []any{map[string]any{"role": "style"}}},
		"ref unknown field":     {"key": "k", "action": "generate", "prompt": "x", "refs": []any{map[string]any{"id": "r", "stylize": 100}}},
		"two subjects":          {"key": "k", "action": "generate", "prompt": "x", "refs": []any{map[string]any{"id": "a", "role": "subject"}, map[string]any{"id": "b", "role": "subject"}}},
		"prompt sref flag":      {"key": "k", "action": "generate", "prompt": "a rock --sref r_1"},
		"prompt ar flag":        {"key": "k", "action": "generate", "prompt": "a rock\n--ar 16:9"},
		"prompt stylize value":  {"key": "k", "action": "generate", "prompt": "a rock, stylize: 100"},
		"prompt chaos number":   {"key": "k", "action": "generate", "prompt": "a rock chaos 20"},
		"prompt raw flag":       {"key": "k", "action": "generate", "prompt": "a rock --raw"},
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

func TestArtRefsAcceptAnIdOrAnObjectWithRoleAndWeight(t *testing.T) {
	var got ArtRequest
	s := startArt(t, func(_ context.Context, req ArtRequest) (ImageResult, error) {
		got = req
		return ImageResult{Text: map[string]any{"state": "queued"}}, nil
	})
	res := callArtTool(t, s, map[string]any{"key": "k", "action": "generate", "prompt": "a single iron ingot, raw wood handle, no border",
		"refs": []any{"r_1", map[string]any{"id": "r_2", "role": "style", "weight": 0.5}}})
	if res["isError"] != false {
		t.Fatalf("result = %v", res)
	}
	if len(got.Refs) != 2 || got.Refs[0] != (ArtRef{ID: "r_1"}) || got.Refs[1] != (ArtRef{ID: "r_2", Role: "style", Weight: 0.5}) {
		t.Fatalf("refs = %+v", got.Refs)
	}
}

func TestArtPromptErrorRefusesParameterFlagsOnly(t *testing.T) {
	for _, p := range []string{"a rock --sref r_1", "a rock --ar 16:9", "a rock stylize: 100", "a rock chaos 20", "a rock --raw"} {
		if ArtPromptError(p) == nil {
			t.Errorf("%q: no error", p)
		}
	}
	for _, p := range []string{"a single iron ingot, raw wood handle, no border", "a dash -- of salt, wide-angle view"} {
		if err := ArtPromptError(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestArtDescriptorTopicsMatchThePlaybook(t *testing.T) {
	topic := artToolDescriptor()["inputSchema"].(map[string]any)["properties"].(map[string]any)["topic"].(map[string]any)["description"].(string)
	list := strings.TrimPrefix(topic, "playbook: ")
	list = strings.Replace(strings.Replace(list, " (default)", "", 1), " or ", ", ", 1)
	got := strings.Split(list, ", ")
	want := art.PlaybookTopics()
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("description topics %v, playbook topics %v", got, want)
	}
}

func TestArtMalformedRefObjectReturnsTheDecodeHint(t *testing.T) {
	called := false
	s := startArt(t, func(context.Context, ArtRequest) (ImageResult, error) { called = true; return ImageResult{}, nil })
	res := callArtTool(t, s, map[string]any{"key": "k", "action": "generate", "prompt": "a rock",
		"refs": []any{map[string]any{"id": "r_1", "stylize": 100}}})
	e, ok := res["rpcError"].(map[string]any)
	if !ok || !strings.Contains(fmt.Sprint(e["message"]), "refs entries are an id, or {id, role, weight}") {
		t.Fatalf("want the refs decode hint, got %v", res)
	}
	if called {
		t.Fatal("the handler ran for a malformed request")
	}
}

func TestArtPromptWithRefsIsLimitedAfterTheReferenceNotes(t *testing.T) {
	refs := []ArtRef{{ID: "r_1", Role: "style", Weight: 0.5}}
	notes, _ := ArtPromptWithRefs("", refs) // the reference notes alone, with an empty prompt
	suffix := len(notes)
	fits := maxArtPrompt - suffix
	if got, _ := ArtPromptWithRefs(strings.Repeat("x", fits), refs); len(got) != maxArtPrompt {
		t.Fatalf("composed length %d, want %d", len(got), maxArtPrompt)
	}

	called := false
	s := startArt(t, func(context.Context, ArtRequest) (ImageResult, error) { called = true; return ImageResult{}, nil })
	ok := callArtTool(t, s, map[string]any{"key": "k", "action": "generate", "prompt": strings.Repeat("x", fits),
		"refs": []any{map[string]any{"id": "r_1", "role": "style", "weight": 0.5}}})
	if ok["isError"] != false || called != true {
		t.Fatalf("a prompt at the limit with its notes should pass, got %v", ok)
	}

	called = false
	res := callArtTool(t, s, map[string]any{"key": "k", "action": "generate", "prompt": strings.Repeat("x", fits+1),
		"refs": []any{map[string]any{"id": "r_1", "role": "style", "weight": 0.5}}})
	e, isErr := res["rpcError"].(map[string]any)
	if !isErr || !strings.Contains(fmt.Sprint(e["message"]), "the limit is 4000 bytes including them") {
		t.Fatalf("want a length refusal at validation, got %v", res)
	}
	if called {
		t.Fatal("the handler ran for a prompt over the limit")
	}
}
