package art

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discPNG(t *testing.T, w, h int, bg color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := x-w/2, y-h/2
			if dx*dx+dy*dy < (w/3)*(w/3) {
				img.SetNRGBA(x, y, color.NRGBA{220, 120, 20, 255})
			} else {
				img.SetNRGBA(x, y, bg)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAddRefStoresOnceKeepsAlphaAndDropsMetadata(t *testing.T) {
	s, _ := newStore(t)
	data := discPNG(t, 60, 60, color.NRGBA{0, 0, 0, 0})
	a, err := s.AddRef(data)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddRef(data)
	if err != nil || a.ID != b.ID || !strings.HasPrefix(a.ID, "r_") {
		t.Fatalf("same picture, ids %q and %q (%v)", a.ID, b.ID, err)
	}
	stored, err := os.ReadFile(filepath.Join(s.RefsDir(), a.ID+".png"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(stored))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, al := img.At(0, 0).RGBA(); al != 0 {
		t.Fatalf("a transparent corner should stay transparent, alpha is %d", al>>8)
	}
	if info, ok := s.RefInfoFor(a.ID); !ok || info.Width != 60 {
		t.Fatalf("info = %+v %v", info, ok)
	}
	if s.HasRef("../r_x") || s.HasRef("r_missing") || s.HasRef("") {
		t.Fatal("HasRef accepted an id it should not")
	}
	if _, err := s.AddRef([]byte("not a picture")); err == nil {
		t.Fatal("stored something that is not a picture")
	}
}

func TestAddRefShrinksLargePictures(t *testing.T) {
	s, _ := newStore(t)
	info, err := s.AddRef(discPNG(t, 3000, 1000, color.NRGBA{10, 10, 10, 255}))
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != maxRefEdge || info.Height != 512 {
		t.Fatalf("stored %dx%d, want %dx512", info.Width, info.Height, maxRefEdge)
	}
}

func TestAttachNumbersSkipsDuplicatesAndCapsTheCount(t *testing.T) {
	s, _ := newStore(t)
	job, _ := s.Create(Job{})
	a, b := discPNG(t, 40, 40, color.NRGBA{255, 0, 255, 255}), discPNG(t, 48, 48, color.NRGBA{255, 0, 255, 255})
	var jpegBuf bytes.Buffer
	jpeg.Encode(&jpegBuf, image.NewRGBA(image.Rect(0, 0, 30, 30)), nil)

	added, err := s.attach(job, [][]byte{a, b, a, jpegBuf.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 3 || added[0].Index != 1 || added[2].Index != 3 || added[2].File != "3.png" {
		t.Fatalf("added = %+v", added)
	}
	for _, c := range added {
		if _, err := os.Stat(filepath.Join(job.Dir, c.File)); err != nil {
			t.Fatal(err)
		}
	}
	job.Candidates = added
	again, err := s.attach(job, [][]byte{a, b})
	if err != nil || len(again) != 0 {
		t.Fatalf("re-attaching the same pictures added %d (%v)", len(again), err)
	}
	more, err := s.attach(job, [][]byte{discPNG(t, 52, 52, color.NRGBA{1, 2, 3, 255})})
	if err != nil || more[0].Index != 4 {
		t.Fatalf("the next number should follow the highest, got %+v (%v)", more, err)
	}
	if _, err := s.attach(job, [][]byte{[]byte("junk")}); err == nil {
		t.Fatal("junk was attached without an error")
	}
	full := job
	full.Candidates = make([]Candidate, MaxCandidates)
	if _, err := s.attach(full, [][]byte{discPNG(t, 70, 70, color.NRGBA{9, 9, 9, 255})}); err == nil {
		t.Fatal("a full job took another picture")
	}
}

func TestCandidateSheetLaysOutAndNumbers(t *testing.T) {
	s, _ := newStore(t)
	job, _ := s.Create(Job{})
	added, _ := s.attach(job, [][]byte{discPNG(t, 700, 700, color.NRGBA{255, 0, 255, 255}), discPNG(t, 500, 300, color.NRGBA{255, 0, 255, 255}), discPNG(t, 64, 64, color.NRGBA{255, 0, 255, 255})})
	job.Candidates = added
	data, err := CandidateSheet(job)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 2*512+3*8 || img.Bounds().Dy() != 2*512+3*8 {
		t.Fatalf("sheet is %v", img.Bounds())
	}
	// The gutter between cells is checked, not a flat dark fill.
	if p := color.NRGBAModel.Convert(img.At(512+4, 4)).(color.NRGBA); p != checkerLight && p != checkerDark {
		t.Fatalf("gutter pixel = %+v, want a checker colour", p)
	}
	if _, err := CandidateSheet(Job{ID: "j9"}); err == nil {
		t.Fatal("made a sheet of nothing")
	}
}

func TestCandidateSheetShowsAlphaOnChecker(t *testing.T) {
	s, _ := newStore(t)
	job, _ := s.Create(Job{})
	added, err := s.attach(job, [][]byte{discPNG(t, 300, 300, color.NRGBA{0, 0, 0, 0})})
	if err != nil {
		t.Fatal(err)
	}
	job.Candidates = added
	data, err := CandidateSheet(job)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 1024+2*8 || img.Bounds().Dy() != 1024+2*8 {
		t.Fatalf("sheet is %v", img.Bounds())
	}
	// The picture is centred in a 1024 cell from (8,8): its transparent top-left is at (8+362, 8+362).
	x, y := 8+362, 8+362
	first := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	if first != checkerLight && first != checkerDark {
		t.Fatalf("transparent pixel = %+v, want a light checker colour", first)
	}
	// A neighbouring square has the other colour, so alpha reads as a checker rather than a flat fill.
	next := color.NRGBAModel.Convert(img.At(x+checkerSize, y)).(color.NRGBA)
	if next == first {
		t.Fatalf("adjacent checker squares share %+v", first)
	}
	// Nothing in the sheet is left transparent.
	if a := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA).A; a != 255 {
		t.Fatalf("alpha under the sprite = %d", a)
	}
}
