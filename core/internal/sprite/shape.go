package sprite

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"sort"
)

const (
	maxOutline = 6
	maxGap     = 64
	// defaultMinArea is the smallest piece slice components keeps, so stray specks are not returned as sprites.
	defaultMinArea = 64
)

// trim crops each picture to the bounds of its visible pixels, plus padding.
func (s *state) trim(op Op) error {
	threshold := optInt(op.Threshold, alphaVisible)
	if threshold < 0 || threshold > 254 {
		return errors.New("threshold must be between 0 and 254")
	}
	if op.Padding < 0 || op.Padding > MaxSide/4 {
		return fmt.Errorf("padding must be between 0 and %d", MaxSide/4)
	}
	for i, f := range s.frames {
		r, ok := bounds(f, threshold)
		if !ok {
			return fmt.Errorf("frame %d is fully transparent; run matte first, or lower threshold", i+1)
		}
		c := crop(f, r)
		if op.Padding > 0 {
			padded := image.NewNRGBA(image.Rect(0, 0, c.Rect.Dx()+2*op.Padding, c.Rect.Dy()+2*op.Padding))
			place(padded, c, op.Padding, op.Padding)
			c = padded
		}
		s.frames[i] = c
	}
	s.note("trim", fmt.Sprintf("threshold %d, padding %d", threshold, op.Padding))
	return nil
}

// outline draws a line of the given width around the visible pixels, behind them. It never grows the picture: leave
// room for the line with the margin of fit, or it is cut off at the edge.
func (s *state) outline(op Op) error {
	width := op.Width
	if width == 0 {
		width = 1
	}
	if width < 1 || width > maxOutline {
		return fmt.Errorf("width must be between 1 and %d", maxOutline)
	}
	c := color.NRGBA{A: 255}
	if op.Color != "" {
		var err error
		if c, err = parseColor(op.Color); err != nil {
			return err
		}
	}
	// Each offset within reach of the line carries the coverage a source pixel at that distance gives it.
	type offset struct {
		dx, dy int
		cover  float32
	}
	var offsets []offset
	for dy := -width - 1; dy <= width+1; dy++ {
		for dx := -width - 1; dx <= width+1; dx++ {
			if cover := math.Min(1, math.Max(0, float64(width)+1-math.Hypot(float64(dx), float64(dy)))); cover > 0 {
				offsets = append(offsets, offset{dx, dy, float32(cover)})
			}
		}
	}
	clipped := false
	for fi, f := range s.frames {
		w, h := f.Rect.Dx(), f.Rect.Dy()
		out := image.NewNRGBA(f.Rect)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				i := (y*w + x) * 4
				var reach float32
				for _, o := range offsets {
					nx, ny := x+o.dx, y+o.dy
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					if v := float32(f.Pix[(ny*w+nx)*4+3]) / 255 * o.cover; v > reach {
						reach = v
					}
				}
				sa := float32(f.Pix[i+3]) / 255
				la := reach * float32(c.A) / 255
				oa := sa + la*(1-sa)
				if oa <= 0 {
					continue
				}
				for k, lc := range [3]uint8{c.R, c.G, c.B} {
					v := (float32(f.Pix[i+k])*sa + float32(lc)*la*(1-sa)) / oa
					out.Pix[i+k] = uint8(clampInt(round(float64(v)), 0, 255))
				}
				out.Pix[i+3] = uint8(clampInt(round(float64(oa*255)), 0, 255))
			}
		}
		if r, ok := bounds(f, alphaVisible); ok && (r.Min.X < width || r.Min.Y < width || r.Max.X > w-width || r.Max.Y > h-width) {
			clipped = true
		}
		s.frames[fi] = out
	}
	note := fmt.Sprintf("%d px %s", width, hex(c))
	if clipped {
		note += "; the sprite is within the line width of the edge, so the line is cut off there: leave a margin"
	}
	s.note("outline", note)
	return nil
}

func (s *state) mirror(op Op) error {
	axis := op.Axis
	if axis == "" {
		axis = "h"
	}
	if axis != "h" && axis != "v" {
		return fmt.Errorf("axis %q is not h (left to right) or v (top to bottom)", axis)
	}
	for i, f := range s.frames {
		w, h := f.Rect.Dx(), f.Rect.Dy()
		out := image.NewNRGBA(f.Rect)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				sx, sy := x, y
				if axis == "h" {
					sx = w - 1 - x
				} else {
					sy = h - 1 - y
				}
				copy(out.Pix[(y*w+x)*4:(y*w+x)*4+4], f.Pix[(sy*w+sx)*4:(sy*w+sx)*4+4])
			}
		}
		s.frames[i] = out
	}
	s.note("mirror", "axis "+axis)
	return nil
}

func (s *state) rotate(op Op) error {
	if op.Degrees != 90 && op.Degrees != 180 && op.Degrees != 270 {
		return errors.New("degrees must be 90, 180 or 270 (clockwise)")
	}
	for i, f := range s.frames {
		w, h := f.Rect.Dx(), f.Rect.Dy()
		var out *image.NRGBA
		if op.Degrees == 180 {
			out = image.NewNRGBA(f.Rect)
		} else {
			out = image.NewNRGBA(image.Rect(0, 0, h, w))
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				var dx, dy, dw int
				switch op.Degrees {
				case 90:
					dx, dy, dw = h-1-y, x, h
				case 180:
					dx, dy, dw = w-1-x, h-1-y, w
				default:
					dx, dy, dw = y, w-1-x, h
				}
				copy(out.Pix[(dy*dw+dx)*4:(dy*dw+dx)*4+4], f.Pix[(y*w+x)*4:(y*w+x)*4+4])
			}
		}
		s.frames[i] = out
	}
	s.note("rotate", fmt.Sprintf("%d degrees clockwise", op.Degrees))
	return nil
}

// slice cuts one picture into several: the cells of a grid, or each separate sprite on a transparent background.
func (s *state) slice(op Op) error {
	if len(s.frames) != 1 {
		return fmt.Errorf("slice cuts a single picture, and there are already %d", len(s.frames))
	}
	src := s.frames[0]
	mode := op.Mode
	if mode == "" {
		mode = "components"
		if op.Cols > 0 || op.Rows > 0 {
			mode = "grid"
		}
	}
	switch mode {
	case "grid":
		return s.sliceGrid(src, op)
	case "components":
		return s.sliceComponents(src, op)
	default:
		return fmt.Errorf("slice mode %q is not one of grid, components", mode)
	}
}

func (s *state) sliceGrid(src *image.NRGBA, op Op) error {
	cols, rows := max(1, op.Cols), max(1, op.Rows)
	if cols*rows > MaxFrames {
		return fmt.Errorf("a %dx%d grid makes %d pictures; the most is %d", cols, rows, cols*rows, MaxFrames)
	}
	cw, ch := src.Rect.Dx()/cols, src.Rect.Dy()/rows
	if cw < 1 || ch < 1 {
		return fmt.Errorf("a %dx%d grid does not fit in a %dx%d picture", cols, rows, src.Rect.Dx(), src.Rect.Dy())
	}
	frames := make([]*image.NRGBA, 0, cols*rows)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			frames = append(frames, crop(src, image.Rect(c*cw, r*ch, (c+1)*cw, (r+1)*ch)))
		}
	}
	s.frames = frames
	note := fmt.Sprintf("%dx%d grid of %dx%d cells", cols, rows, cw, ch)
	if src.Rect.Dx()%cols != 0 || src.Rect.Dy()%rows != 0 {
		note += "; the picture does not divide evenly, so the right and bottom edges were left out"
	}
	s.note("slice", note)
	return nil
}

func (s *state) sliceComponents(src *image.NRGBA, op Op) error {
	gap := op.Gap
	if gap < 0 || gap > maxGap {
		return fmt.Errorf("gap must be between 0 and %d", maxGap)
	}
	minArea := op.MinArea
	if minArea <= 0 {
		minArea = defaultMinArea
	}
	w, h := src.Rect.Dx(), src.Rect.Dy()
	ink := make([]bool, w*h)
	hasInk := false
	for i := range ink {
		if src.Pix[i*4+3] > alphaVisible {
			ink[i], hasInk = true, true
		}
	}
	if !hasInk {
		return errors.New("the picture is fully transparent; run matte first")
	}
	// Dilating by the gap joins pieces that are close, such as a sprite and its detached shadow, into one.
	joined := dilate(ink, w, h, gap)
	label := make([]int32, w*h)
	var found []piece
	var stack []int32
	for i := range joined {
		if !joined[i] || label[i] != 0 {
			continue
		}
		id := int32(len(found) + 1)
		label[i] = id
		stack = append(stack[:0], int32(i))
		var r image.Rectangle
		pixels := 0
		for len(stack) > 0 {
			p := int(stack[len(stack)-1])
			stack = stack[:len(stack)-1]
			x, y := p%w, p/w
			if ink[p] {
				pixels++
				r = r.Union(image.Rect(x, y, x+1, y+1))
			}
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					if q := ny*w + nx; joined[q] && label[q] == 0 {
						label[q] = id
						stack = append(stack, int32(q))
					}
				}
			}
		}
		found = append(found, piece{id: id, rect: r, pixels: pixels})
	}
	var keep []piece
	for _, p := range found {
		if p.pixels >= minArea && !p.rect.Empty() {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return fmt.Errorf("no piece has %d or more pixels; lower minArea", minArea)
	}
	if len(keep) > MaxFrames {
		return fmt.Errorf("found %d pieces; the most is %d. Raise gap to join pieces that belong together, or minArea to drop specks", len(keep), MaxFrames)
	}
	sortPieces(keep)
	frames := make([]*image.NRGBA, 0, len(keep))
	for _, p := range keep {
		id, r := p.id, p.rect
		f := crop(src, r)
		// Ink that belongs to a neighbouring piece but falls inside this piece's rectangle is cleared.
		for y := 0; y < r.Dy(); y++ {
			for x := 0; x < r.Dx(); x++ {
				q := (r.Min.Y+y)*w + r.Min.X + x
				if ink[q] && label[q] != id {
					f.Pix[(y*r.Dx()+x)*4+3] = 0
				}
			}
		}
		frames = append(frames, f)
	}
	s.frames = frames
	s.note("slice", fmt.Sprintf("%d pieces (gap %d, minArea %d)", len(frames), gap, minArea))
	return nil
}

// piece is one separate sprite found by slice.
type piece struct {
	id     int32
	rect   image.Rectangle
	pixels int
}

// sortPieces puts pieces into reading order: rows from top to bottom, each row from left to right. A piece joins the
// current row when its vertical centre is within half the row's first piece's height of that piece's centre.
func sortPieces(ps []piece) {
	centre := func(p piece) int { return p.rect.Min.Y + p.rect.Dy()/2 }
	sort.SliceStable(ps, func(i, j int) bool { return centre(ps[i]) < centre(ps[j]) })
	for start := 0; start < len(ps); {
		end := start + 1
		for end < len(ps) && centre(ps[end])-centre(ps[start]) <= max(1, ps[start].rect.Dy()/2) {
			end++
		}
		row := ps[start:end]
		sort.SliceStable(row, func(i, j int) bool { return row[i].rect.Min.X < row[j].rect.Min.X })
		start = end
	}
}

// dilate grows a mask by r pixels in every direction, with a square window.
func dilate(mask []bool, w, h, r int) []bool {
	if r == 0 {
		return append([]bool(nil), mask...)
	}
	tmp := make([]bool, len(mask))
	for y := 0; y < h; y++ {
		last := -1 << 30
		for x := 0; x < w; x++ {
			if mask[y*w+x] {
				last = x
			}
			tmp[y*w+x] = x-last <= r
		}
		last = 1 << 30
		for x := w - 1; x >= 0; x-- {
			if mask[y*w+x] {
				last = x
			}
			if last-x <= r {
				tmp[y*w+x] = true
			}
		}
	}
	out := make([]bool, len(mask))
	for x := 0; x < w; x++ {
		last := -1 << 30
		for y := 0; y < h; y++ {
			if tmp[y*w+x] {
				last = y
			}
			out[y*w+x] = y-last <= r
		}
		last = 1 << 30
		for y := h - 1; y >= 0; y-- {
			if tmp[y*w+x] {
				last = y
			}
			if last-y <= r {
				out[y*w+x] = true
			}
		}
	}
	return out
}
