package sprite

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"testing"
)

var (
	magenta = color.NRGBA{R: 255, G: 0, B: 255, A: 255}
	orange  = color.NRGBA{R: 230, G: 120, B: 20, A: 255}
)

// noise is a small deterministic generator, so fixtures are the same on every run.
type noise uint32

func (n *noise) next(amp int) int {
	*n ^= *n << 13
	*n ^= *n >> 17
	*n ^= *n << 5
	if amp == 0 {
		return 0
	}
	return int(uint32(*n)%uint32(2*amp+1)) - amp
}

func blend(fg, bg color.NRGBA, c float64) color.NRGBA {
	mix := func(a, b uint8) uint8 { return uint8(math.Round(float64(a)*c + float64(b)*(1-c))) }
	return color.NRGBA{R: mix(fg.R, bg.R), G: mix(fg.G, bg.G), B: mix(fg.B, bg.B), A: 255}
}

// ringOnKey draws an annulus of fill on a flat key background, with the edges blended into the key the way a
// generator blends them, and a little noise on the flat background. inner 0 makes a solid disc.
func ringOnKey(size int, inner, outer float64, key, fill color.NRGBA, amp int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	n := noise(2463534242)
	cx := float64(size) / 2
	const ss = 4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			hit := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					d := math.Hypot(float64(x)+(float64(sx)+0.5)/ss-cx, float64(y)+(float64(sy)+0.5)/ss-cx)
					if d >= inner && d <= outer {
						hit++
					}
				}
			}
			c := float64(hit) / (ss * ss)
			px := blend(fill, key, c)
			if hit == 0 {
				px = color.NRGBA{R: uint8(clampInt(int(key.R)+n.next(amp), 0, 255)), G: uint8(clampInt(int(key.G)+n.next(amp), 0, 255)), B: uint8(clampInt(int(key.B)+n.next(amp), 0, 255)), A: 255}
			}
			img.SetNRGBA(x, y, px)
		}
	}
	return img
}

func disc(size int, key, fill color.NRGBA) *image.NRGBA {
	return ringOnKey(size, 0, float64(size)*0.35, key, fill, 4)
}

func run(t *testing.T, in *image.NRGBA, ops ...Op) ([]*image.NRGBA, *Report) {
	t.Helper()
	frames, rep, err := Run(in, ops, Env{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return frames, rep
}

func alphaAt(img *image.NRGBA, x, y int) uint8 { return img.NRGBAAt(x, y).A }

func TestMatteKeyRemovesBackgroundWithoutFringe(t *testing.T) {
	in := disc(256, magenta, orange)
	frames, rep := run(t, in, Op{Op: "matte"}, Op{Op: "check", Rules: &Rules{Alpha: true}})
	out := frames[0]
	if a := alphaAt(out, 2, 2); a != 0 {
		t.Fatalf("corner alpha = %d, want 0", a)
	}
	if a := alphaAt(out, 128, 128); a != 255 {
		t.Fatalf("centre alpha = %d, want 255", a)
	}
	// No visible pixel may keep the colour of the background it was cut from, and the soft edge must have been
	// repainted from inside the sprite.
	soft, worst := 0, 0.0
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			p := out.NRGBAAt(x, y)
			if p.A == 0 {
				continue
			}
			if d := math.Sqrt(float64(dist2([]uint8{p.R, p.G, p.B, p.A}, orange))); d > worst {
				worst = d
			}
			if p.A < 255 {
				soft++
			}
		}
	}
	if soft == 0 {
		t.Fatal("the edge is hard; feather should leave soft pixels")
	}
	if worst > 12 {
		t.Fatalf("a visible pixel is %.0f away from the sprite colour; the key colour leaked into the edge", worst)
	}
	if rep.Failed {
		t.Fatalf("checks failed: %+v", rep.Checks)
	}
	found := false
	for _, c := range rep.Checks {
		found = found || c.Rule == "keyResidue" && c.OK
	}
	if !found {
		t.Fatalf("a matte should add a passing keyResidue check: %+v", rep.Checks)
	}
}

func TestMatteDetectsTheKeyColour(t *testing.T) {
	green := color.NRGBA{R: 0, G: 255, B: 0, A: 255}
	_, rep := run(t, disc(128, green, orange), Op{Op: "matte"})
	if !strings.Contains(rep.Steps[0].Note, "#00ff00") && !strings.Contains(rep.Steps[0].Note, "#01ff01") && !strings.Contains(rep.Steps[0].Note, "#00fe") {
		// The detected colour is the mean of the noisy border, so it is near, not equal to, the key.
		if !strings.Contains(rep.Steps[0].Note, "auto") {
			t.Fatalf("matte note = %q", rep.Steps[0].Note)
		}
	}
}

func TestMatteKeepsEnclosedKeyColourUnlessHolesAreAsked(t *testing.T) {
	ring := ringOnKey(256, 40, 90, magenta, orange, 0)
	kept, _ := run(t, ring, Op{Op: "matte", Shrink: ptr(0), Feather: ptr(0)})
	if a := alphaAt(kept[0], 128, 128); a != 255 {
		t.Fatalf("the middle of the ring has alpha %d; by default only background touching the border is removed", a)
	}
	cleared, _ := run(t, ring, Op{Op: "matte", Holes: true, Shrink: ptr(0), Feather: ptr(0)})
	if a := alphaAt(cleared[0], 128, 128); a != 0 {
		t.Fatalf("the middle of the ring has alpha %d; holes should clear it", a)
	}
	if a := alphaAt(cleared[0], 128+65, 128); a != 255 {
		t.Fatalf("the ring itself has alpha %d, want 255", a)
	}
}

func TestMatteNamedColourAndTolerance(t *testing.T) {
	in := disc(128, magenta, orange)
	frames, _ := run(t, in, Op{Op: "matte", Color: "#ff00ff", Tolerance: ptr(60)})
	if a := alphaAt(frames[0], 1, 1); a != 0 {
		t.Fatalf("corner alpha = %d", a)
	}
	_, _, err := Run(in, []Op{{Op: "matte", Color: "pink"}}, Env{})
	if err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("err = %v, want a colour error", err)
	}
	_, _, err = Run(in, []Op{{Op: "matte", Tolerance: ptr(999)}}, Env{})
	if err == nil || !strings.Contains(err.Error(), "tolerance") {
		t.Fatalf("err = %v, want a tolerance error", err)
	}
}

func TestMatteAutoFailsWithoutFlatBorder(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 2), G: uint8(y * 2), B: uint8(x + y), A: 255})
		}
	}
	_, _, err := Run(img, []Op{{Op: "matte"}}, Env{})
	if err == nil || !strings.Contains(err.Error(), "no flat background colour") {
		t.Fatalf("err = %v", err)
	}
}

func TestMatteLeavesAnAlreadyTransparentPictureAlone(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 20; y < 40; y++ {
		for x := 20; x < 40; x++ {
			img.SetNRGBA(x, y, orange)
		}
	}
	frames, rep := run(t, img, Op{Op: "matte"})
	if !bytes.Equal(frames[0].Pix, img.Pix) {
		t.Fatal("pixels changed")
	}
	if !strings.Contains(rep.Steps[0].Note, "already has a transparent border") {
		t.Fatalf("note = %q", rep.Steps[0].Note)
	}
}

func TestMatteMask(t *testing.T) {
	in := disc(64, magenta, orange)
	mask := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			v := uint8(0)
			if x >= 16 && x < 48 && y >= 16 && y < 48 {
				v = 255
			}
			mask.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
	}
	data, err := EncodePNG(mask)
	if err != nil {
		t.Fatal(err)
	}
	env := Env{ReadFile: func(name string) ([]byte, error) { return data, nil }}
	frames, _, err := Run(in, []Op{{Op: "matte", Mode: "mask", Mask: "m.png", Shrink: ptr(0), Feather: ptr(0)}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if alphaAt(frames[0], 32, 32) != 255 || alphaAt(frames[0], 5, 5) != 0 || alphaAt(frames[0], 50, 32) != 0 {
		t.Fatal("mask not applied")
	}
	if _, _, err := Run(in, []Op{{Op: "matte", Mode: "mask"}}, env); err == nil {
		t.Fatal("a mask path is required")
	}
}

func TestResizeLeavesNoDarkHalo(t *testing.T) {
	// Generators and exporters often leave transparent pixels black. Averaging those in without weighting by alpha
	// darkens every edge when the picture is shrunk.
	img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			d := math.Hypot(float64(x)-128, float64(y)-128)
			a := clampInt(int(math.Round((90-d)*255/3)), 0, 255)
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{R: orange.R, G: orange.G, B: orange.B, A: uint8(a)})
			}
		}
	}
	for _, filter := range []string{"lanczos", "catmullrom", "area", "bilinear"} {
		frames, _ := run(t, img, Op{Op: "resize", Size: []int{32, 32}, Filter: filter})
		out := frames[0]
		if out.Rect.Dx() != 32 || out.Rect.Dy() != 32 {
			t.Fatalf("%s: size = %v", filter, out.Rect)
		}
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				p := out.NRGBAAt(x, y)
				// Below about 16/255 coverage an 8-bit pixel cannot hold a meaningful colour, and cannot be seen.
				if p.A < 16 {
					continue
				}
				// A premultiplied 8-bit channel is off by up to half a step, which un-premultiplied is 128/alpha steps.
				allowed := 4 + 255/float64(p.A)
				if d := math.Sqrt(float64(dist2([]uint8{p.R, p.G, p.B, p.A}, orange))); d > allowed {
					t.Fatalf("%s: pixel %d,%d (alpha %d) is %.0f from the sprite colour: a dark halo", filter, x, y, p.A, d)
				}
			}
		}
	}
}

func TestResizeRefusesAChangedAspectRatio(t *testing.T) {
	in := image.NewNRGBA(image.Rect(0, 0, 100, 50))
	if _, _, err := Run(in, []Op{{Op: "resize", Size: []int{40, 40}}}, Env{}); err == nil || !strings.Contains(err.Error(), "aspect ratio") {
		t.Fatalf("err = %v", err)
	}
	frames, _ := run(t, in, Op{Op: "resize", Size: []int{40, 0}})
	if frames[0].Rect.Dx() != 40 || frames[0].Rect.Dy() != 20 {
		t.Fatalf("size = %v", frames[0].Rect)
	}
	frames, _ = run(t, in, Op{Op: "resize", Size: []int{40, 40}, Stretch: true})
	if frames[0].Rect.Dx() != 40 || frames[0].Rect.Dy() != 40 {
		t.Fatalf("stretched size = %v", frames[0].Rect)
	}
	frames, _ = run(t, in, Op{Op: "resize", Scale: 0.5})
	if frames[0].Rect.Dx() != 50 || frames[0].Rect.Dy() != 25 {
		t.Fatalf("scaled size = %v", frames[0].Rect)
	}
	frames, _ = run(t, in, Op{Op: "resize", MaxEdge: 20})
	if frames[0].Rect.Dx() != 20 || frames[0].Rect.Dy() != 10 {
		t.Fatalf("maxEdge size = %v", frames[0].Rect)
	}
	if _, _, err := Run(in, []Op{{Op: "resize"}}, Env{}); err == nil {
		t.Fatal("resize with nothing to do should fail")
	}
	if _, _, err := Run(in, []Op{{Op: "resize", Scale: 2, MaxEdge: 10}}, Env{}); err == nil {
		t.Fatal("resize with two modes should fail")
	}
}

func square(w, h, x, y, sw, sh int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for j := y; j < y+sh; j++ {
		for i := x; i < x+sw; i++ {
			img.SetNRGBA(i, j, c)
		}
	}
	return img
}

func TestTrim(t *testing.T) {
	img := square(100, 100, 30, 40, 10, 20, orange)
	frames, _ := run(t, img, Op{Op: "trim"})
	if frames[0].Rect.Dx() != 10 || frames[0].Rect.Dy() != 20 {
		t.Fatalf("trimmed = %v", frames[0].Rect)
	}
	frames, _ = run(t, img, Op{Op: "trim", Padding: 3})
	if frames[0].Rect.Dx() != 16 || frames[0].Rect.Dy() != 26 || alphaAt(frames[0], 3, 3) != 255 || alphaAt(frames[0], 2, 2) != 0 {
		t.Fatalf("padded = %v", frames[0].Rect)
	}
	if _, _, err := Run(image.NewNRGBA(image.Rect(0, 0, 8, 8)), []Op{{Op: "trim"}}, Env{}); err == nil || !strings.Contains(err.Error(), "fully transparent") {
		t.Fatalf("err = %v", err)
	}
}

func TestFitPlacesTheSpriteInTheBox(t *testing.T) {
	img := square(100, 100, 30, 40, 10, 20, orange)
	frames, _ := run(t, img, Op{Op: "fit", Size: []int{64, 64}, Margin: 4, Anchor: "bottom"})
	out := frames[0]
	if out.Rect.Dx() != 64 || out.Rect.Dy() != 64 {
		t.Fatalf("size = %v", out.Rect)
	}
	ink, ok := bounds(out, alphaVisible)
	if !ok {
		t.Fatal("empty")
	}
	// Smaller than the box, so it is not enlarged: 10x20, resting on the bottom margin and centred.
	if ink.Dx() != 10 || ink.Dy() != 20 || ink.Max.Y != 60 || ink.Min.X != 27 {
		t.Fatalf("ink = %v", ink)
	}
	frames, _ = run(t, img, Op{Op: "fit", Size: []int{64, 64}, Margin: 4, Upscale: true})
	ink, _ = bounds(frames[0], alphaVisible)
	if ink.Dy() != 56 || ink.Min.Y != 4 || ink.Max.Y != 60 {
		t.Fatalf("upscaled ink = %v", ink)
	}
	big := square(400, 400, 0, 0, 400, 200, orange)
	frames, _ = run(t, big, Op{Op: "fit", Size: []int{64, 64}, Margin: 2, Anchor: "top-left"})
	ink, _ = bounds(frames[0], alphaVisible)
	if ink.Min.X != 2 || ink.Min.Y != 2 || ink.Dx() != 60 || ink.Dy() != 30 {
		t.Fatalf("shrunk ink = %v", ink)
	}
	for _, bad := range []Op{{Op: "fit"}, {Op: "fit", Size: []int{10, 10}, Margin: 5}, {Op: "fit", Size: []int{10, 10}, Anchor: "sideways"}, {Op: "fit", Size: []int{10, 10}, Filter: "magic"}} {
		if _, _, err := Run(img, []Op{bad}, Env{}); err == nil {
			t.Fatalf("%+v should fail", bad)
		}
	}
}

func TestFitCommonKeepsRelativeSize(t *testing.T) {
	sheet := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	for y := 10; y < 90; y++ {
		for x := 10; x < 90; x++ {
			sheet.SetNRGBA(x, y, orange)
		}
	}
	for y := 40; y < 90; y++ {
		for x := 120; x < 170; x++ {
			sheet.SetNRGBA(x, y, orange)
		}
	}
	frames, _ := run(t, sheet, Op{Op: "slice"}, Op{Op: "fit", Size: []int{40, 40}, Common: true})
	if len(frames) != 2 {
		t.Fatalf("frames = %d", len(frames))
	}
	a, _ := bounds(frames[0], alphaVisible)
	b, _ := bounds(frames[1], alphaVisible)
	// 80 and 50 pixel squares share the scale of the larger: 40/80 = 0.5, so 40 and 25.
	if a.Dx() != 40 || b.Dx() != 25 {
		t.Fatalf("widths = %d, %d; want 40 and 25", a.Dx(), b.Dx())
	}
}

func TestOutline(t *testing.T) {
	img := square(16, 16, 6, 6, 4, 4, orange)
	frames, rep := run(t, img, Op{Op: "outline", Width: 1})
	out := frames[0]
	if p := out.NRGBAAt(5, 7); p.A != 255 || p.R != 0 || p.G != 0 || p.B != 0 {
		t.Fatalf("pixel beside the sprite = %+v, want opaque black", p)
	}
	if p := out.NRGBAAt(4, 7); p.A != 0 {
		t.Fatalf("pixel two away = %+v, want clear", p)
	}
	if p := out.NRGBAAt(7, 7); p != orange {
		t.Fatalf("sprite pixel = %+v, changed", p)
	}
	if p := out.NRGBAAt(5, 5); p.A == 0 || p.A == 255 {
		t.Fatalf("corner pixel alpha = %d, want a soft corner", p.A)
	}
	if strings.Contains(rep.Steps[0].Note, "cut off") {
		t.Fatalf("note = %q", rep.Steps[0].Note)
	}
	edge := square(16, 16, 0, 6, 4, 4, orange)
	_, rep = run(t, edge, Op{Op: "outline", Width: 2, Color: "#ff0000"})
	if !strings.Contains(rep.Steps[0].Note, "cut off") {
		t.Fatalf("a sprite at the edge should warn: %q", rep.Steps[0].Note)
	}
	for _, bad := range []Op{{Op: "outline", Width: 99}, {Op: "outline", Color: "zzz"}} {
		if _, _, err := Run(img, []Op{bad}, Env{}); err == nil {
			t.Fatalf("%+v should fail", bad)
		}
	}
}

func TestMirrorAndRotate(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	// a b c
	// d e f
	cols := []color.NRGBA{{R: 1, A: 255}, {R: 2, A: 255}, {R: 3, A: 255}, {R: 4, A: 255}, {R: 5, A: 255}, {R: 6, A: 255}}
	for i, c := range cols {
		img.SetNRGBA(i%3, i/3, c)
	}
	red := func(f *image.NRGBA) []uint8 {
		var out []uint8
		for y := 0; y < f.Rect.Dy(); y++ {
			for x := 0; x < f.Rect.Dx(); x++ {
				out = append(out, f.NRGBAAt(x, y).R)
			}
		}
		return out
	}
	cases := []struct {
		op   Op
		w, h int
		want []uint8
	}{
		{Op{Op: "mirror"}, 3, 2, []uint8{3, 2, 1, 6, 5, 4}},
		{Op{Op: "mirror", Axis: "v"}, 3, 2, []uint8{4, 5, 6, 1, 2, 3}},
		{Op{Op: "rotate", Degrees: 90}, 2, 3, []uint8{4, 1, 5, 2, 6, 3}},
		{Op{Op: "rotate", Degrees: 180}, 3, 2, []uint8{6, 5, 4, 3, 2, 1}},
		{Op{Op: "rotate", Degrees: 270}, 2, 3, []uint8{3, 6, 2, 5, 1, 4}},
	}
	for _, c := range cases {
		frames, _ := run(t, img, c.op)
		if frames[0].Rect.Dx() != c.w || frames[0].Rect.Dy() != c.h || !bytes.Equal(red(frames[0]), c.want) {
			t.Fatalf("%+v: %dx%d %v, want %dx%d %v", c.op, frames[0].Rect.Dx(), frames[0].Rect.Dy(), red(frames[0]), c.w, c.h, c.want)
		}
	}
	for _, bad := range []Op{{Op: "mirror", Axis: "x"}, {Op: "rotate", Degrees: 45}} {
		if _, _, err := Run(img, []Op{bad}, Env{}); err == nil {
			t.Fatalf("%+v should fail", bad)
		}
	}
}

func TestSlice(t *testing.T) {
	sheet := image.NewNRGBA(image.Rect(0, 0, 80, 40))
	for _, r := range []image.Rectangle{image.Rect(4, 4, 20, 20), image.Rect(44, 6, 60, 30), image.Rect(24, 24, 28, 28)} {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				sheet.SetNRGBA(x, y, orange)
			}
		}
	}
	frames, rep := run(t, sheet, Op{Op: "slice"})
	if len(frames) != 2 || frames[0].Rect.Dx() != 16 || frames[1].Rect.Dx() != 16 || frames[1].Rect.Dy() != 24 {
		t.Fatalf("frames = %d (%v) %s", len(frames), rep.Frames, rep.Steps[0].Note)
	}
	frames, _ = run(t, sheet, Op{Op: "slice", MinArea: 1})
	if len(frames) != 3 {
		t.Fatalf("with minArea 1 got %d frames, want 3 (the speck included)", len(frames))
	}
	frames, _ = run(t, sheet, Op{Op: "slice", Gap: 40})
	if len(frames) != 1 {
		t.Fatalf("a wide gap should join everything, got %d", len(frames))
	}
	frames, _ = run(t, sheet, Op{Op: "slice", Cols: 2, Rows: 1})
	if len(frames) != 2 || frames[0].Rect.Dx() != 40 || frames[0].Rect.Dy() != 40 {
		t.Fatalf("grid = %d frames, first %v", len(frames), frames[0].Rect)
	}
	if _, _, err := Run(sheet, []Op{{Op: "slice", Cols: 2, Rows: 1}, {Op: "slice"}}, Env{}); err == nil {
		t.Fatal("slicing twice should fail")
	}
	if _, _, err := Run(image.NewNRGBA(image.Rect(0, 0, 8, 8)), []Op{{Op: "slice"}}, Env{}); err == nil {
		t.Fatal("slicing an empty picture should fail")
	}
}

func TestSliceClearsInkThatBelongsToANeighbour(t *testing.T) {
	// Two L-shaped pieces whose bounding boxes overlap: each frame must hold only its own piece.
	img := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	set := func(x, y int) { img.SetNRGBA(x, y, orange) }
	for i := 0; i < 20; i++ {
		set(i, 0)
		set(0, i)
	}
	for i := 5; i < 30; i++ {
		set(i, 29)
		set(29, i)
	}
	// The second piece starts at 5,5 and runs along the bottom and right; the first is the top and left.
	frames, _ := run(t, img, Op{Op: "slice", MinArea: 5})
	if len(frames) != 2 {
		t.Fatalf("frames = %d", len(frames))
	}
}

func TestSheet(t *testing.T) {
	a := square(16, 16, 2, 2, 12, 12, orange)
	b := square(24, 12, 0, 0, 24, 12, color.NRGBA{R: 10, G: 200, B: 10, A: 255})
	sheet, note, err := RenderSheet([]*image.NRGBA{a, b}, SheetOptions{Scales: []int{1, 3}, Labels: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The widest row is the second picture: pad + 24 + pad + 72 + pad.
	if want := sheetPad + 24 + sheetPad + 72 + sheetPad; sheet.Rect.Dx() != want {
		t.Fatalf("width = %d, want %d", sheet.Rect.Dx(), want)
	}
	if !strings.Contains(note, "x1, x3") {
		t.Fatalf("note = %q", note)
	}
	// The first cell is at the pad, below the label; its top-left pixel is checker, and the sprite is on top.
	if p := sheet.NRGBAAt(sheetPad, sheetPad+sheetLabel); p != checkerLight {
		t.Fatalf("checker pixel = %+v", p)
	}
	if p := sheet.NRGBAAt(sheetPad+8, sheetPad+sheetLabel+8); p != orange {
		t.Fatalf("sprite pixel = %+v", p)
	}
	// At x3 each source pixel is a 3x3 block.
	x3 := sheetPad + 16 + sheetPad
	if p := sheet.NRGBAAt(x3+6+2, sheetPad+sheetLabel+6+2); p != orange {
		t.Fatalf("magnified sprite pixel = %+v", p)
	}
	if p := sheet.NRGBAAt(x3+5, sheetPad+sheetLabel+5); p == orange {
		t.Fatalf("magnified pixel before the sprite should be background: %+v", p)
	}
	solid, _, err := RenderSheet([]*image.NRGBA{a}, SheetOptions{Background: "#3a6b35", Scales: []int{1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := solid.NRGBAAt(sheetPad, sheetPad); p != (color.NRGBA{R: 0x3a, G: 0x6b, B: 0x35, A: 255}) {
		t.Fatalf("solid background pixel = %+v", p)
	}
	for _, bad := range []SheetOptions{{Scales: []int{0}}, {Scales: []int{9}}, {Scales: []int{1, 2, 3, 4, 5}}, {Background: "#12"}, {Background: "pic.png"}} {
		if _, _, err := RenderSheet([]*image.NRGBA{a}, bad, nil); err == nil {
			t.Fatalf("%+v should fail", bad)
		}
	}
	if _, _, err := RenderSheet(nil, SheetOptions{}, nil); err == nil {
		t.Fatal("an empty sheet should fail")
	}
}

func TestSheetStaysWithinTheSizeLimit(t *testing.T) {
	big := image.NewNRGBA(image.Rect(0, 0, 1500, 1500))
	sheet, note, err := RenderSheet([]*image.NRGBA{big}, SheetOptions{Scales: []int{1, 3}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sheet.Rect.Dx() > MaxSide || sheet.Rect.Dy() > MaxSide || !strings.Contains(note, "dropped") {
		t.Fatalf("sheet %v, note %q", sheet.Rect, note)
	}
}

func TestSheetTilesAPictureBackground(t *testing.T) {
	tile := square(2, 2, 0, 0, 1, 1, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	data, _ := EncodePNG(tile)
	read := func(string) ([]byte, error) { return data, nil }
	sheet, _, err := RenderSheet([]*image.NRGBA{image.NewNRGBA(image.Rect(0, 0, 4, 4))}, SheetOptions{Background: "ground.png", Scales: []int{1}}, read)
	if err != nil {
		t.Fatal(err)
	}
	if p := sheet.NRGBAAt(sheetPad+2, sheetPad); p.R != 1 || p.G != 2 || p.B != 3 {
		t.Fatalf("tiled pixel = %+v", p)
	}
}

func TestCheck(t *testing.T) {
	img := square(64, 64, 10, 10, 40, 40, orange)
	rules := &Rules{Width: 64, Height: 64, Alpha: true, Margin: ptr(8), MaxBytes: 100_000, NamePattern: `^[a-z_]+\.png$`}
	frames, rep, err := Run(img, []Op{{Op: "check", Rules: rules}}, Env{Output: "Textures/iron_ore.png"})
	if err != nil || rep.Failed || len(frames) != 1 {
		t.Fatalf("err %v failed %v: %+v", err, rep.Failed, rep.Checks)
	}
	rules.Margin = ptr(11)
	rules.Width = 128
	_, rep, _ = Run(img, []Op{{Op: "check", Rules: rules}}, Env{Output: "Textures/IronOre.png"})
	failed := map[string]bool{}
	for _, c := range rep.Checks {
		if !c.OK {
			failed[c.Rule] = true
		}
	}
	if !rep.Failed || !failed["width"] || !failed["margin"] || !failed["namePattern"] || failed["height"] || failed["alpha"] {
		t.Fatalf("failed = %v", failed)
	}
	_, rep, _ = Run(img, []Op{{Op: "check", Rules: rules, Strict: true}, {Op: "resize", Scale: 0.5}}, Env{})
	if rep.Aborted == "" || len(rep.Steps) != 1 {
		t.Fatalf("a strict failure should stop the run: aborted %q, %d steps", rep.Aborted, len(rep.Steps))
	}
	if _, _, err := Run(img, []Op{{Op: "check"}}, Env{}); err == nil {
		t.Fatal("check without rules should fail")
	}
	if _, _, err := Run(img, []Op{{Op: "check", Rules: &Rules{NamePattern: "("}}}, Env{}); err == nil {
		t.Fatal("a bad pattern should fail")
	}
}

func TestCheckFlagsKeyColourLeftInTheSprite(t *testing.T) {
	// The eye of the ring is the key colour and is not connected to the border, so matte keeps it, and the check
	// reports that a lot of the sprite still looks like the background.
	ring := ringOnKey(256, 20, 100, magenta, orange, 0)
	_, rep := run(t, ring, Op{Op: "matte", Color: "#ff00ff"}, Op{Op: "check", Rules: &Rules{}})
	var res *Check
	for i := range rep.Checks {
		if rep.Checks[i].Rule == "keyResidue" {
			res = &rep.Checks[i]
		}
	}
	if res == nil || res.OK {
		t.Fatalf("keyResidue = %+v, want a failure", res)
	}
}

func TestRunRejectsBadPipelines(t *testing.T) {
	img := square(8, 8, 0, 0, 8, 8, orange)
	if _, _, err := Run(img, []Op{{Op: "sparkle"}}, Env{}); err == nil || !strings.Contains(err.Error(), "unknown op") || !strings.Contains(err.Error(), "matte") {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := Run(img, make([]Op, MaxOps+1), Env{}); err == nil {
		t.Fatal("too many steps should fail")
	}
	_, _, err := Run(img, []Op{{Op: "inspect"}, {Op: "resize", Scale: 1000}}, Env{})
	if err == nil || !strings.Contains(err.Error(), "step 2 (resize)") {
		t.Fatalf("err = %v, want the failing step named", err)
	}
}

func TestInspectDescribesThePicture(t *testing.T) {
	img := square(20, 10, 5, 2, 10, 4, orange)
	_, rep := run(t, img, Op{Op: "inspect"})
	f := rep.Steps[0].Frames[0]
	if f.Width != 20 || f.Height != 10 || f.Opaque != 0.2 || f.Colors != 1 || f.Ink == nil || *f.Ink != (Rect{X: 5, Y: 2, Width: 10, Height: 4}) {
		t.Fatalf("info = %+v ink %+v", f, f.Ink)
	}
}

func TestEncodeIsDeterministicAndDropsHiddenColour(t *testing.T) {
	img := square(16, 16, 4, 4, 8, 8, orange)
	img.SetNRGBA(0, 0, color.NRGBA{R: 9, G: 9, B: 9, A: 0})
	a, err := EncodePNG(img)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := EncodePNG(img)
	if !bytes.Equal(a, b) {
		t.Fatal("two encodes differ")
	}
	decoded, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, bl, al := decoded.At(0, 0).RGBA(); r|g|bl|al != 0 {
		t.Fatalf("a transparent pixel kept colour: %v %v %v %v", r, g, bl, al)
	}
	if img.NRGBAAt(0, 0).R != 9 {
		t.Fatal("EncodePNG changed its input")
	}
	back, format, err := Decode(a)
	if err != nil || format != "png" || !bytes.Equal(back.Pix[4*16*4+4*4:4*16*4+4*4+4], []uint8{230, 120, 20, 255}) {
		t.Fatalf("round trip: %v %s", err, format)
	}
}

func TestDecodeRefusesBadAndHugeInput(t *testing.T) {
	if _, _, err := Decode([]byte("not an image")); err == nil {
		t.Fatal("garbage should fail")
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 5000, 5000))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "megapixel") {
		t.Fatalf("err = %v", err)
	}
}

func TestWholePipelineMakesAGameReadySprite(t *testing.T) {
	in := disc(1024, magenta, orange)
	rules := &Rules{Width: 128, Height: 128, Alpha: true, Margin: ptr(2), MaxBytes: 64 << 10}
	run1 := func() ([]byte, *Report) {
		frames, rep := run(t, in,
			Op{Op: "matte"},
			Op{Op: "fit", Size: []int{128, 128}, Margin: 4},
			Op{Op: "outline", Width: 1},
			Op{Op: "check", Rules: rules, Strict: true})
		data, err := EncodePNG(frames[0])
		if err != nil {
			t.Fatal(err)
		}
		return data, rep
	}
	a, rep := run1()
	if rep.Failed || rep.Aborted != "" {
		t.Fatalf("checks: %+v", rep.Checks)
	}
	b, _ := run1()
	if !bytes.Equal(a, b) {
		t.Fatal("the same pipeline produced different bytes")
	}
	if rep.Frames[0].Ink == nil || rep.Frames[0].Ink.Width < 100 {
		t.Fatalf("ink = %+v", rep.Frames[0].Ink)
	}
}

func ptr[T any](v T) *T { return &v }
