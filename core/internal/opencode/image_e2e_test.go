package opencode

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/vision"
)

// relo_image against the real opencode and a fake model. A worker calls the tool, Relo's hub prepares a real file from
// the workspace with internal/vision, and the pixels must reach the model's next request as an image part. A model the
// provider config declares text only must not receive them, whatever a tool returns. It runs only when
// RELO_E2E_OPENCODE=1.

type imageTurn struct {
	requests []agentRequest
	sent     string // base64 of the picture the hub returned
	calls    []agentmsg.ImageRequest
}

func runImageTurn(t *testing.T, served provider.Model) imageTurn {
	t.Helper()
	model := newFakeChat("IMAGE-PROMPT", func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamChunks(w, toolCallDelta("call_image", runtime.ReloImageTool, `{"key":"image-key","paths":["shots/screen.png"]}`), "tool_calls")
			return
		}
		streamChunks(w, map[string]any{"role": "assistant", "content": `{"status":"completed","summary":"looked"}`}, "stop")
	})
	ctx, rt, work := startMessagingE2EWith(t, model, served)
	raw, err := base64.StdEncoding.DecodeString(testPNG(3000, 1500))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "shots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "shots", "screen.png"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var turn imageTurn
	hub, err := agentmsg.Start(func(context.Context, agentmsg.Request) (any, error) { return nil, nil }, agentmsg.WithImages(func(_ context.Context, req agentmsg.ImageRequest) (agentmsg.ImageResult, error) {
		turn.calls = append(turn.calls, req)
		f, rel, err := vision.Open([]string{work}, req.Paths[0])
		if err != nil {
			return agentmsg.ImageResult{}, err
		}
		defer f.Close()
		pic, err := vision.Prepare(f, vision.Options{})
		if err != nil {
			return agentmsg.ImageResult{}, err
		}
		turn.sent = base64.StdEncoding.EncodeToString(pic.Data)
		return agentmsg.ImageResult{
			Text:   map[string]any{"images": []any{map[string]any{"index": 1, "path": rel, "sent": map[string]int{"width": pic.Width, "height": pic.Height}}}},
			Images: []agentmsg.Image{{MIME: pic.MIME, Data: turn.sent}},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	if err := rt.(runtime.MessageRuntime).ConfigureMessages(ctx, work, runtime.MessageTools{URL: hub.URL, Secret: hub.Secret}); err != nil {
		t.Fatal(err)
	}
	sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: "image", Directory: work})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.CloseSession(context.Background(), sid) }()
	events, err := rt.Events(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	log := watchEvents(events, model)
	tools := runtime.WithImages(runtime.ToolsFor("worker", "build", true), "worker", true)
	if err := rt.SendTask(ctx, sid, runtime.AgentTask{Role: "worker", Model: served.ID, Provider: "fake", Prompt: "IMAGE-PROMPT", Tools: tools, Directory: work}); err != nil {
		t.Fatal(err)
	}
	if result := log.waitDone(t, turnWait); result.Error != "" || !strings.Contains(result.JSON, "looked") {
		t.Fatalf("image turn ended badly: %+v; model: %s", result, model.describe())
	}
	turn.requests = model.agentRequests()
	if len(turn.requests) != 2 || !hasTool(turn.requests[0].tools, runtime.ReloImageTool) {
		t.Fatalf("relo_image was not offered, or the turn took another path: %s", model.describe())
	}
	if len(turn.calls) != 1 || turn.calls[0].Key != "image-key" || turn.sent == "" {
		t.Fatalf("the hub handler saw %+v", turn.calls)
	}
	return turn
}

func TestReloImageDeliversPixelsToTheModel(t *testing.T) {
	// "claude-fake" matches no family the name guess knows, so no modalities are declared and OpenCode's default holds.
	turn := runImageTurn(t, provider.Model{ID: "claude-fake", Context: 200000, Output: 8000})
	if !hasImagePart(turn.requests[1].body, "image/png", turn.sent) {
		t.Fatalf("the prepared picture did not reach the model as an image part: %.800s", turn.requests[1].body)
	}
	if !strings.Contains(turn.requests[1].body, "shots/screen.png") {
		t.Error("the image metadata did not reach the model")
	}
}

func TestTextOnlyModelNeverReceivesPixels(t *testing.T) {
	// The orchestrator refuses relo_image for a text-only model before this point. OpenCode's own check is the backstop
	// for every other path to an image (its read tool included), and it holds only because BuildConfig declares the
	// model's modalities.
	no := false
	turn := runImageTurn(t, provider.Model{ID: "claude-fake", Context: 200000, Output: 8000, Vision: &no})
	body := turn.requests[1].body
	if strings.Contains(body, "data:image/png;base64,") || strings.Contains(body, turn.sent[:64]) {
		t.Fatalf("a model declared text only received image data: %.800s", body)
	}
	// OpenCode 2.0.29 puts its own note where the image would have been.
	if !strings.Contains(body, "does not support image input") {
		i := max(0, strings.LastIndex(body, `"role":"tool"`))
		t.Errorf("the text-only model was not told the image was withheld: %.400s", body[i:])
	}
}
