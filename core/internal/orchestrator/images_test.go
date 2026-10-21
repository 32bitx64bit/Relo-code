package orchestrator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/vision"
)

// catalog is a model catalog with one model per vision answer.
func catalog() map[string]runtime.Model {
	return map[string]runtime.Model{
		"p/seeing":  {ID: "seeing", Provider: "p", Vision: vision.Yes, VisionSource: vision.SourceConfigured},
		"p/blind":   {ID: "blind", Provider: "p", Vision: vision.No, VisionSource: vision.SourceName},
		"p/unknown": {ID: "unknown", Provider: "p"},
	}
}

func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// imageExposure runs one turn for a role on a model and returns the prompt and the tool policy it carried.
func imageExposure(t *testing.T, role agent.Role, model string) (string, map[string]bool) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	rt := &canvasExposureRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(rt)
	c.UpdateSettings(func(s *Settings) { s.Models, s.ModelsKnown = catalog(), true })
	work := t.TempDir()
	c.CreateSession(work, "match the reference screenshot", ModeBuild)
	a := c.Agents.Create(agent.Agent{ID: "img-" + string(role), Role: role, Workspace: work, Status: agent.Running, Model: model})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{}, "do the work", nil); err != nil {
		t.Fatalf("runAgent(%s): %v", role, err)
	}
	task := rt.last()
	return task.Prompt, task.Tools
}

func TestImageToolFollowsTheModelsVision(t *testing.T) {
	for _, tc := range []struct {
		model   string
		offered bool
		says    string
	}{
		{"p/seeing", true, "relo_image shows you image files"},
		{"p/unknown", true, "Relo does not know whether your model accepts images"},
		{"p/blind", false, "Your model cannot see images"},
	} {
		promptText, tools := imageExposure(t, agent.Worker, tc.model)
		if tools[runtime.ReloImageTool] != tc.offered {
			t.Errorf("%s: relo_image offered = %v, want %v", tc.model, tools[runtime.ReloImageTool], tc.offered)
		}
		if !strings.Contains(promptText, tc.says) {
			t.Errorf("%s: prompt lacks %q:\n%s", tc.model, tc.says, promptText)
		}
	}
	promptText, tools := imageExposure(t, agent.Primary, "p/seeing")
	if tools[runtime.ReloImageTool] || strings.Contains(promptText, "relo_image") || strings.Contains(promptText, "cannot see images") {
		t.Errorf("the Primary was given the image tool or its guidance: %v\n%s", tools, promptText)
	}
}

// newImageCoord binds a worker on model and returns the Coord, the worker's key and the session workspace.
func newImageCoord(t *testing.T, role agent.Role, model string) (*Coord, string, string) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	c.UpdateSettings(func(s *Settings) { s.Models, s.ModelsKnown = catalog(), true })
	work := t.TempDir()
	c.CreateSession(work, "match the reference screenshot", ModeBuild)
	a := c.Agents.Create(agent.Agent{ID: "viewer", Role: role, Status: agent.Running, Model: model})
	c.runtimeSessions[a.ID] = &runtimeSession{id: "runtime-viewer"}
	key, err := c.bindPeer(a, "runtime-viewer")
	if err != nil {
		t.Fatal(err)
	}
	return c, key, work
}

func imageMetadata(t *testing.T, res agentmsg.ImageResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.Text)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestImageRequestShowsWorkspaceFiles(t *testing.T) {
	c, key, work := newImageCoord(t, agent.Worker, "p/seeing")
	writePNG(t, filepath.Join(work, "ref", "market.png"), 3000, 1500)
	writePNG(t, filepath.Join(work, "shots", "market.png"), 400, 200)

	res, err := c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: key, Paths: []string{"shots/market.png", "missing.png", filepath.Join(work, "ref", "market.png")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("got %d images, want the two that exist", len(res.Images))
	}
	for i, img := range res.Images {
		data, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil || img.MIME != "image/png" {
			t.Fatalf("image %d: %s, %v", i, img.MIME, err)
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatalf("image %d does not decode: %v", i, err)
		}
	}
	meta := imageMetadata(t, res)
	entries := meta["images"].([]any)
	shot, missing, ref := entries[0].(map[string]any), entries[1].(map[string]any), entries[2].(map[string]any)
	if shot["index"] != float64(1) || shot["path"] != "shots/market.png" || shot["scale"] != float64(1) {
		t.Errorf("screenshot entry = %v", shot)
	}
	if missing["index"] != nil || !strings.Contains(missing["error"].(string), "no such file") {
		t.Errorf("missing entry = %v", missing)
	}
	if ref["index"] != float64(2) || ref["path"] != "ref/market.png" || ref["sent"].(map[string]any)["width"] != float64(vision.DefaultMaxEdge) || ref["source"].(map[string]any)["width"] != float64(3000) {
		t.Errorf("reference entry = %v", ref)
	}
	if meta["note"] != nil {
		t.Errorf("a model known to see got the unknown-model note: %v", meta["note"])
	}

	res, err = c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: key, Paths: []string{"ref/market.png"}, Region: &agentmsg.ImageRegion{X: 100, Y: 100, Width: 50, Height: 40}})
	if err != nil {
		t.Fatal(err)
	}
	crop := imageMetadata(t, res)["images"].([]any)[0].(map[string]any)
	if c := crop["crop"].(map[string]any); c["x"] != float64(100) || c["width"] != float64(50) || crop["sent"].(map[string]any)["height"] != float64(40) {
		t.Errorf("crop entry = %v", crop)
	}
}

func TestImageRequestRefusals(t *testing.T) {
	c, key, work := newImageCoord(t, agent.Worker, "p/blind")
	writePNG(t, filepath.Join(work, "a.png"), 10, 10)
	if _, err := c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: key, Paths: []string{"a.png"}}); err == nil || !strings.Contains(err.Error(), "cannot see images") {
		t.Fatalf("a text-only model got pixels: %v", err)
	}

	c, key, _ = newImageCoord(t, agent.Primary, "p/seeing")
	if _, err := c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: key, Paths: []string{"a.png"}}); err == nil || !strings.Contains(err.Error(), "Primary") {
		t.Fatalf("the Primary was served: %v", err)
	}

	c, key, work = newImageCoord(t, agent.Worker, "p/unknown")
	writePNG(t, filepath.Join(work, "a.png"), 10, 10)
	if _, err := c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: "not-a-key", Paths: []string{"a.png"}}); err == nil {
		t.Fatal("an unknown key was served")
	}
	outside := filepath.Join(t.TempDir(), "secret.png")
	writePNG(t, outside, 10, 10)
	if _, err := c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: key, Paths: []string{outside}}); err == nil || !strings.Contains(err.Error(), "outside your workspace") {
		t.Fatalf("a file outside the workspace was served: %v", err)
	}
	res, err := c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: key, Paths: []string{"a.png"}})
	if err != nil || len(res.Images) != 1 {
		t.Fatalf("unknown-vision model: %v", err)
	}
	if note, _ := imageMetadata(t, res)["note"].(string); !strings.Contains(note, "does not know") {
		t.Errorf("unknown-vision model was not warned: %q", note)
	}

	// The refused outside path and the served call above already count: a refusal after the key check still costs one.
	for i := 2; i < maxImagePerMinute; i++ {
		if err := c.reserveImage("viewer"); err != nil {
			t.Fatalf("call %d refused early: %v", i+1, err)
		}
	}
	if _, err := c.imageRequest(context.Background(), agentmsg.ImageRequest{Key: key, Paths: []string{"a.png"}}); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("rate limit not applied: %v", err)
	}
}

func TestVisionTasksRouteToAModelThatSees(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.UpdateSettings(func(s *Settings) {
		s.Models, s.ModelsKnown = catalog(), true
		s.PrimaryModel, s.Tier1Model, s.Tier2Model = "p/seeing", "p/blind", "p/unknown"
		s.RoutingMode = routing.ModeSimple
		s.Escalate = true
	})
	if got := c.modelForTask(agent.Worker, routing.Medium, false); got != "p/blind" {
		t.Fatalf("a task without vision moved: %s", got)
	}
	if got := c.modelForTask(agent.Worker, routing.Medium, true); got != "p/seeing" {
		t.Fatalf("a vision task stayed on %s", got)
	}
	if got := c.modelForTask(agent.Worker, routing.Easy, true); got != "p/seeing" {
		t.Fatalf("an easy vision task stayed on %s: unknown does not count as seeing", got)
	}
	model, _ := c.nextAttempt(taskgraph.Task{Attempt: 2, Vision: true}, agent.Agent{Model: "p/seeing"})
	if model != "p/seeing" {
		t.Fatalf("a vision retry escalated away from the only model that sees: %s", model)
	}
}
