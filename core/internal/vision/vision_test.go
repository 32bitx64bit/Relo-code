package vision

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuess(t *testing.T) {
	for id, want := range map[string]string{
		"claude-opus-5-5":            Yes,
		"cc/claude-sonnet-5-5":       Yes,
		"claude-3-5-haiku-latest":    Yes,
		"anthropic/claude-fable-5-1": Yes,
		"claude-2.1":                 No,
		"claude-instant-1.2":         No,
		"gpt-4o-mini":                Yes,
		"gpt-4.1":                    Yes,
		"gpt-5-codex":                Yes,
		"cx/gpt-6-sol[1m]":           Yes,
		"openai/o3":                  Yes,
		"o4-mini":                    Yes,
		"o3-mini":                    No,
		"o1-mini":                    No,
		"gemini-2.5-pro":             Yes,
		"qwen2.5-vl-72b-instruct":    Yes,
		"qwen3-omni-flash":           Yes,
		"glm-4.5v":                   Yes,
		"deepseek-vl2":               Yes,
		"deepseek-ocr":               Yes,
		"llama-3.2-90b-vision":       Yes,
		"deepseek-v4.1-flash":        No,
		"deepseek-reasoner":          No,
		"gpt-oss-120b":               No,
		"qwen3-coder-plus":           No,
		"codestral-latest":           No,
		"minimax-m3.1-flash:free":    Unknown,
		"mimo-v2-flash":              Unknown,
		"gpt-4":                      Unknown,
		"glm-4.6":                    Unknown,
		"kimi-k2":                    Unknown,
		"":                           Unknown,
	} {
		if got := Guess(id); got != want {
			t.Errorf("Guess(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestResolveOrder(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		configured     *bool
		catalog, id    string
		status, source string
	}{
		{&no, Yes, "claude-opus-5-5", No, SourceConfigured},
		{&yes, No, "deepseek-chat", Yes, SourceConfigured},
		{nil, No, "claude-opus-5-5", No, SourceCatalog},
		{nil, "", "claude-opus-5-5", Yes, SourceName},
		{nil, "", "my-model", Unknown, ""},
	}
	for _, c := range cases {
		status, source := Resolve(c.configured, c.catalog, c.id)
		if status != c.status || source != c.source {
			t.Errorf("Resolve(%v, %q, %q) = %q/%q, want %q/%q", c.configured, c.catalog, c.id, status, source, c.status, c.source)
		}
	}
	if FromInputs([]string{"text"}) != No || FromInputs([]string{"text", "Image"}) != Yes || FromInputs(nil) != Unknown {
		t.Error("FromInputs misreads modality lists")
	}
}

// screenshot draws a w×h opaque image: a white background, a red block in the top-left quarter, and a one-pixel black
// grid every 8 pixels, the kind of fine detail a naive shrink turns into noise.
func screenshot(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if x < w/2 && y < h/2 {
				c = color.RGBA{220, 20, 20, 255}
			}
			if x%8 == 0 || y%8 == 0 {
				c = color.RGBA{0, 0, 0, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeSent(t *testing.T, p Picture) image.Image {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(p.Data))
	if err != nil {
		t.Fatalf("sent image does not decode: %v", err)
	}
	if "image/"+format != p.MIME {
		t.Fatalf("MIME %s but data is %s", p.MIME, format)
	}
	if b := img.Bounds(); b.Dx() != p.Width || b.Dy() != p.Height {
		t.Fatalf("data is %dx%d, metadata says %dx%d", b.Dx(), b.Dy(), p.Width, p.Height)
	}
	return img
}

func TestPrepareSendsSmallPNGUnchanged(t *testing.T) {
	data := encodePNG(t, screenshot(640, 360))
	p, err := Prepare(bytes.NewReader(data), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Original || !bytes.Equal(p.Data, data) || p.Scale != 1 || p.Width != 640 || p.Height != 360 || p.MIME != "image/png" {
		t.Fatalf("small PNG was not passed through: %+v", p.summary())
	}
}

func TestPrepareShrinksToMaxEdge(t *testing.T) {
	data := encodePNG(t, screenshot(3840, 2160))
	p, err := Prepare(bytes.NewReader(data), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != DefaultMaxEdge || p.Height != 882 || p.SourceWidth != 3840 || p.SourceHeight != 2160 || p.Original {
		t.Fatalf("4K screenshot prepared as %+v", p.summary())
	}
	if p.Scale < 2.44 || p.Scale > 2.46 {
		t.Fatalf("scale = %v, want about 2.449", p.Scale)
	}
	img := decodeSent(t, p)
	// The red quarter must still be red after the shrink, away from the grid lines' influence.
	r, g, b, _ := img.At(p.Width/4, p.Height/4).RGBA()
	if r>>8 < 120 || g>>8 > 120 || b>>8 > 120 {
		t.Errorf("top-left quarter lost its colour: %d,%d,%d", r>>8, g>>8, b>>8)
	}
	if p.ApproxTokens() < 1800 || p.ApproxTokens() > 1900 {
		t.Errorf("ApproxTokens = %d for %dx%d", p.ApproxTokens(), p.Width, p.Height)
	}

	big, err := Prepare(bytes.NewReader(data), Options{MaxEdge: MaxMaxEdge})
	if err != nil {
		t.Fatal(err)
	}
	if big.Width != MaxMaxEdge {
		t.Fatalf("maxEdge %d gave width %d", MaxMaxEdge, big.Width)
	}
	if _, err := Prepare(bytes.NewReader(data), Options{MaxEdge: 5000}); err == nil {
		t.Fatal("maxEdge above the limit was accepted")
	}
}

func TestPrepareCropsInSourcePixels(t *testing.T) {
	data := encodePNG(t, screenshot(1000, 800))
	p, err := Prepare(bytes.NewReader(data), Options{Region: &Region{X: 900, Y: 700, Width: 400, Height: 400}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Crop == nil || *p.Crop != (Region{X: 900, Y: 700, Width: 100, Height: 100}) {
		t.Fatalf("crop past the edge was not clamped: %+v", p.Crop)
	}
	if p.Width != 100 || p.Height != 100 || p.Scale != 1 {
		t.Fatalf("cropped picture %+v", p.summary())
	}
	img := decodeSent(t, p)
	// Source pixel (905, 705) is white: not in the red quarter and not on the grid; (904, 704) is a grid line.
	if r, g, b, _ := img.At(5, 5).RGBA(); r>>8 != 255 || g>>8 != 255 || b>>8 != 255 {
		t.Errorf("crop shows the wrong pixels at (5,5): %d,%d,%d", r>>8, g>>8, b>>8)
	}
	if r, _, _, _ := img.At(4, 4).RGBA(); r>>8 != 0 {
		t.Errorf("crop is off by some pixels: (4,4) should be on the grid")
	}
	if _, err := Prepare(bytes.NewReader(data), Options{Region: &Region{X: 2000, Y: 0, Width: 10, Height: 10}}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("region outside the image: %v", err)
	}
	if _, err := Prepare(bytes.NewReader(data), Options{Region: &Region{Width: 0, Height: 10}}); err == nil {
		t.Fatal("zero-width region was accepted")
	}
}

func TestPrepareReadsOtherFormats(t *testing.T) {
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, screenshot(320, 200), nil); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(bytes.NewReader(jbuf.Bytes()), Options{})
	if err != nil || p.Format != "jpeg" || !p.Original || p.MIME != "image/jpeg" {
		t.Fatalf("JPEG: %+v, %v", p.summary(), err)
	}

	pal := color.Palette{color.White, color.Black}
	frame := func() *image.Paletted { return image.NewPaletted(image.Rect(0, 0, 40, 30), pal) }
	var gbuf bytes.Buffer
	if err := gif.EncodeAll(&gbuf, &gif.GIF{Image: []*image.Paletted{frame(), frame()}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	p, err = Prepare(bytes.NewReader(gbuf.Bytes()), Options{})
	if err != nil || p.Format != "gif" || !p.Animated || p.MIME != "image/png" {
		t.Fatalf("animated GIF: %+v, %v", p.summary(), err)
	}
	decodeSent(t, p)

	webp, err := os.ReadFile(filepath.Join("testdata", "gopher.webp"))
	if err != nil {
		t.Fatal(err)
	}
	p, err = Prepare(bytes.NewReader(webp), Options{})
	if err != nil || p.Format != "webp" || p.MIME != "image/png" {
		t.Fatalf("WebP: %+v, %v", p.summary(), err)
	}
	decodeSent(t, p)
}

func TestPrepareRefusesWhatIsNotAnImage(t *testing.T) {
	if _, err := Prepare(strings.NewReader(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`), Options{}); err == nil || !strings.Contains(err.Error(), "SVG") {
		t.Fatalf("SVG: %v", err)
	}
	if _, err := Prepare(strings.NewReader("package main\n"), Options{}); err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Fatalf("source file: %v", err)
	}
	// A PNG header that claims 10000x10000 is refused from the header alone.
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 10000, 10000))); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(bytes.NewReader(buf.Bytes()), Options{}); err == nil || !strings.Contains(err.Error(), "megapixel") {
		t.Fatalf("100 MP image: %v", err)
	}
}

// noise fills a w×h opaque image with values that do not compress.
func noise(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for i := range img.Pix {
		seed = seed*1664525 + 1013904223
		img.Pix[i] = byte(seed >> 24)
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	return img
}

func TestPrepareFallsBackToJPEGForPhotos(t *testing.T) {
	// Noise does not compress, so as PNG it overflows MaxImageBytes at the largest edge.
	p, err := Prepare(bytes.NewReader(encodePNG(t, noise(2576, 1600))), Options{MaxEdge: MaxMaxEdge})
	if err != nil {
		t.Fatal(err)
	}
	if p.MIME != "image/jpeg" || len(p.Data) > MaxImageBytes {
		t.Fatalf("noise image sent as %s, %d bytes", p.MIME, len(p.Data))
	}
	decodeSent(t, p)

	// When even the lowest JPEG quality is too big, the image shrinks until it fits, and the bounds say so.
	mime, data, sent, err := encodeWithin(noise(1000, 800), 100<<10)
	if err != nil {
		t.Fatal(err)
	}
	if wantH := float64(sent.Dx()) * 0.8; mime != "image/jpeg" || len(data) > 100<<10 || sent.Dx() >= 1000 || float64(sent.Dy()) < wantH-1 || float64(sent.Dy()) > wantH+1 {
		t.Fatalf("shrink to fit: %s, %d bytes, %v", mime, len(data), sent)
	}
}

func TestOpenStaysInsideTheRoots(t *testing.T) {
	work := t.TempDir()
	session := t.TempDir()
	outside := t.TempDir()
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(work, "shots", "a.png"))
	write(filepath.Join(session, "ref", "b.png"))
	write(filepath.Join(outside, "secret.png"))
	if err := os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(work, "link.png")); err != nil {
		t.Fatal(err)
	}
	roots := []string{work, session}

	open := func(name string) (string, error) {
		f, rel, err := Open(roots, name)
		if f != nil {
			f.Close()
		}
		return rel, err
	}
	if rel, err := open("shots/a.png"); err != nil || rel != "shots/a.png" {
		t.Fatalf("relative path in the agent workspace: %q, %v", rel, err)
	}
	if rel, err := open("ref/b.png"); err != nil || rel != "ref/b.png" {
		t.Fatalf("relative path in the session workspace: %q, %v", rel, err)
	}
	if rel, err := open(filepath.Join(session, "ref", "b.png")); err != nil || rel != "ref/b.png" {
		t.Fatalf("absolute path in the session workspace: %q, %v", rel, err)
	}
	for _, name := range []string{filepath.Join(outside, "secret.png"), "../" + filepath.Base(outside) + "/secret.png", "link.png", "missing.png", "shots", ""} {
		if _, err := open(name); err == nil {
			t.Errorf("Open(%q) succeeded; it must stay inside the workspace and open only files", name)
		}
	}
}

func (p Picture) summary() map[string]any {
	return map[string]any{"mime": p.MIME, "format": p.Format, "src": [2]int{p.SourceWidth, p.SourceHeight}, "sent": [2]int{p.Width, p.Height}, "scale": p.Scale, "crop": p.Crop, "animated": p.Animated, "original": p.Original, "bytes": len(p.Data)}
}
