package art

import (
	"context"
	"os"
	"testing"
)

// TestLiveImages makes two real, cheap calls against an image API: one 1024x1024 low-quality generation with a
// transparent background, then an edit that uses it as a reference. It costs a cent or two and never runs by default.
//
//	RELO_E2E_IMAGES=1 RELO_E2E_IMAGES_KEY=sk-... go test ./internal/art -run TestLiveImages -v
//
// RELO_E2E_IMAGES_BASE_URL (default https://api.openai.com/v1) and RELO_E2E_IMAGES_MODEL (default DefaultModel) pick
// the endpoint and model. It reports what the endpoint said instead of failing on a refused transparent background,
// because whether a model allows one is exactly what this is for finding out.
func TestLiveImages(t *testing.T) {
	if os.Getenv("RELO_E2E_IMAGES") != "1" {
		t.Skip("set RELO_E2E_IMAGES=1 (and RELO_E2E_IMAGES_KEY) to spend a few cents on a real image endpoint")
	}
	key := os.Getenv("RELO_E2E_IMAGES_KEY")
	if key == "" {
		t.Fatal("RELO_E2E_IMAGES_KEY is empty")
	}
	base := os.Getenv("RELO_E2E_IMAGES_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := os.Getenv("RELO_E2E_IMAGES_MODEL")
	if model == "" {
		model = DefaultModel
	}
	ep := Endpoint{BaseURL: base, Key: key}
	ctx := context.Background()

	call := imageCall{Model: model, Prompt: "A single iron ingot, hammered dark metal, three-quarter view, centred, whole object in frame, game icon style, no shadow.",
		Size: "1024x1024", Quality: "low", Background: "transparent", N: 1}
	reply, err := callImages(ctx, newClient(), ep, call)
	if err != nil {
		t.Logf("transparent background was refused or the call failed: %v", err)
		call.Background = "opaque"
		if reply, err = callImages(ctx, newClient(), ep, call); err != nil {
			t.Fatalf("generation failed: %v", err)
		}
	}
	got, err := prepare(reply.Images[0], maxCandidateEdge)
	if err != nil {
		t.Fatalf("the picture could not be read: %v", err)
	}
	t.Logf("generation: background=%s, %dx%d, usage reported=%v %+v, cost $%.4f (estimate $%.4f)", call.Background, got.Width, got.Height, reply.HasUsage, reply.Usage,
		CostUSD(model, reply.Usage), EstimateUSD(model, "low", "1024x1024", 1, 0, len(call.Prompt)))

	edit := imageCall{Model: model, Prompt: "Image 1 is an iron ingot. Draw the same ingot in gold, keeping the shape, view and style.", Size: "1024x1024", Quality: "low", N: 1, Refs: [][]byte{got.PNG}}
	reply, err = callImages(ctx, newClient(), ep, edit)
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	t.Logf("edit: usage reported=%v %+v, cost $%.4f (estimate $%.4f)", reply.HasUsage, reply.Usage, CostUSD(model, reply.Usage), EstimateUSD(model, "low", "1024x1024", 1, 1, len(edit.Prompt)))
}
