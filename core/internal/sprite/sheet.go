package sprite

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"sort"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	sheetPad    = 8
	sheetLabel  = 14
	maxScale    = 8
	maxScales   = 4
	checkerSize = 8
)

var (
	pageColor     = color.NRGBA{R: 40, G: 40, B: 40, A: 255}
	checkerLight  = color.NRGBA{R: 204, G: 204, B: 204, A: 255}
	checkerDark   = color.NRGBA{R: 160, G: 160, B: 160, A: 255}
	defaultScales = []int{1, 3}
)

// SheetOptions says how to lay out a contact sheet.
type SheetOptions struct {
	// Background is checker (the default), a colour such as #3a6b35, or the path of a picture to tile.
	Background string
	// Scales are the magnifications each sprite is shown at, left to right. Magnified copies use nearest-neighbour, so
	// every pixel stays visible.
	Scales []int
	Labels bool
}

func (s *state) sheet(op Op) error {
	labels := op.Labels == nil || *op.Labels
	img, note, err := RenderSheet(s.frames, SheetOptions{Background: op.Background, Scales: op.Scales, Labels: labels}, s.env.ReadFile)
	if err != nil {
		return err
	}
	s.frames = []*image.NRGBA{img}
	s.note("sheet", note)
	return nil
}

// RenderSheet lays the pictures out one row each on a background, at the requested scales side by side. It is how an
// agent judges a sprite at the size the game draws it, and up close, in one look.
func RenderSheet(frames []*image.NRGBA, opts SheetOptions, readFile func(string) ([]byte, error)) (*image.NRGBA, string, error) {
	if len(frames) == 0 {
		return nil, "", errors.New("sheet has nothing to show")
	}
	scales, err := cleanScales(opts.Scales)
	if err != nil {
		return nil, "", err
	}
	background, err := backgroundFor(opts.Background, readFile)
	if err != nil {
		return nil, "", err
	}
	labelH := 0
	if opts.Labels {
		labelH = sheetLabel
	}
	layout := func(scales []int) (width, height int, rowH []int) {
		y := sheetPad
		for _, f := range frames {
			w, h := f.Rect.Dx(), f.Rect.Dy()
			rowW, tallest := sheetPad, 0
			for _, sc := range scales {
				rowW += w*sc + sheetPad
				tallest = max(tallest, h*sc)
			}
			width = max(width, rowW)
			rowH = append(rowH, tallest)
			y += labelH + tallest + sheetPad
		}
		return width, y, rowH
	}
	dropped := false
	var width, height int
	var rowH []int
	for {
		width, height, rowH = layout(scales)
		if width <= MaxSide && height <= MaxSide {
			break
		}
		if len(scales) == 1 {
			break
		}
		scales, dropped = scales[:len(scales)-1], true
	}

	sheet := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(sheet, sheet.Bounds(), &image.Uniform{C: pageColor}, image.Point{}, draw.Src)
	y := sheetPad
	for i, f := range frames {
		w, h := f.Rect.Dx(), f.Rect.Dy()
		if opts.Labels {
			text := fmt.Sprintf("#%d  %dx%d  at", i+1, w, h)
			for _, sc := range scales {
				text += fmt.Sprintf(" x%d", sc)
			}
			drawText(sheet, sheetPad, y, text)
		}
		x := sheetPad
		for _, sc := range scales {
			cw, ch := w*sc, h*sc
			place(sheet, background(cw, ch), x, y+labelH)
			shown := f
			if sc > 1 {
				shown = magnify(f, sc)
			}
			draw.Draw(sheet, image.Rect(x, y+labelH, x+cw, y+labelH+ch), shown, image.Point{}, draw.Over)
			x += cw + sheetPad
		}
		y += labelH + rowH[i] + sheetPad
	}
	note := fmt.Sprintf("%d pictures at x%s on %s, %dx%d", len(frames), joinInts(scales, ", x"), orDefault(opts.Background, "checker"), width, height)
	if dropped {
		note += "; the larger scales were dropped to stay within the size limit"
	}
	if width > MaxSide || height > MaxSide {
		sc := float64(MaxSide) / float64(max(width, height))
		sheet = resample(sheet, max(1, round(float64(width)*sc)), max(1, round(float64(height)*sc)), area, false)
		note += fmt.Sprintf("; shrunk to %dx%d to stay within the size limit", sheet.Rect.Dx(), sheet.Rect.Dy())
	}
	return sheet, note, nil
}

func cleanScales(in []int) ([]int, error) {
	if len(in) == 0 {
		return append([]int(nil), defaultScales...), nil
	}
	seen := map[int]bool{}
	var out []int
	for _, sc := range in {
		if sc < 1 || sc > maxScale {
			return nil, fmt.Errorf("a scale must be between 1 and %d", maxScale)
		}
		if !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}
	if len(out) > maxScales {
		return nil, fmt.Errorf("at most %d scales", maxScales)
	}
	sort.Ints(out)
	return out, nil
}

// backgroundFor returns a function that makes an opaque background of the requested size.
func backgroundFor(spec string, readFile func(string) ([]byte, error)) (func(w, h int) *image.NRGBA, error) {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "" || strings.EqualFold(spec, "checker"):
		return func(w, h int) *image.NRGBA {
			img := image.NewNRGBA(image.Rect(0, 0, w, h))
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					c := checkerLight
					if (x/checkerSize+y/checkerSize)%2 == 1 {
						c = checkerDark
					}
					copy(img.Pix[(y*w+x)*4:], []uint8{c.R, c.G, c.B, 255})
				}
			}
			return img
		}, nil
	case strings.HasPrefix(spec, "#"):
		c, err := parseColor(spec)
		if err != nil {
			return nil, err
		}
		c.A = 255
		return func(w, h int) *image.NRGBA {
			img := image.NewNRGBA(image.Rect(0, 0, w, h))
			draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
			return img
		}, nil
	default:
		if readFile == nil {
			return nil, errors.New("picture backgrounds are not available here")
		}
		data, err := readFile(spec)
		if err != nil {
			return nil, err
		}
		tile, _, err := Decode(data)
		if err != nil {
			return nil, fmt.Errorf("background: %w", err)
		}
		return func(w, h int) *image.NRGBA {
			img := image.NewNRGBA(image.Rect(0, 0, w, h))
			tw, th := tile.Rect.Dx(), tile.Rect.Dy()
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					j := ((y%th)*tw + x%tw) * 4
					copy(img.Pix[(y*w+x)*4:(y*w+x)*4+4], tile.Pix[j:j+3])
					img.Pix[(y*w+x)*4+3] = 255
				}
			}
			return img
		}, nil
	}
}

// magnify enlarges by a whole factor without blending, so each source pixel becomes a clear block.
func magnify(img *image.NRGBA, factor int) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w*factor, h*factor))
	for y := 0; y < h*factor; y++ {
		for x := 0; x < w*factor; x++ {
			copy(out.Pix[(y*w*factor+x)*4:(y*w*factor+x)*4+4], img.Pix[((y/factor)*w+x/factor)*4:((y/factor)*w+x/factor)*4+4])
		}
	}
	return out
}

func drawText(dst *image.NRGBA, x, y int, text string) {
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(color.White), Face: basicfont.Face7x13, Dot: fixed.P(x, y+11)}
	d.DrawString(text)
}

func joinInts(v []int, sep string) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, sep)
}
