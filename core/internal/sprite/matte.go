package sprite

import (
	"errors"
	"fmt"
	"image"
	"image/color"
)

const (
	defaultTolerance = 48
	defaultShrink    = 1
	defaultFeather   = 1
	maxTolerance     = 200
	maxShrink        = 8
	maxFeather       = 4
	// minBorderShare is the fraction of the border a background colour must cover to be trusted when it is detected
	// rather than named.
	minBorderShare = 0.6
	// defaultHolesMin is the smallest enclosed region, in pixels, that holes removes.
	defaultHolesMin = 64
)

// matte turns a flat-colour or masked background into transparency on every frame.
//
// A generator cannot output alpha, so the usual sprite workflow is to ask for a flat background colour that the
// sprite does not contain. The key mode removes that colour: it flood-fills from the border so colour inside the
// sprite survives, pulls the edge in by shrink pixels to cut the blended fringe, softens it by feather pixels, and
// then repaints every soft edge pixel with the colour of the nearest solid pixel, so no trace of the background
// colour is left to show up as a halo when the sprite is later shrunk or laid on another background.
func (s *state) matte(op Op) error {
	tol := optInt(op.Tolerance, defaultTolerance)
	shrink := optInt(op.Shrink, defaultShrink)
	feather := optInt(op.Feather, defaultFeather)
	if tol < 0 || tol > maxTolerance {
		return fmt.Errorf("tolerance must be between 0 and %d", maxTolerance)
	}
	if shrink < 0 || shrink > maxShrink {
		return fmt.Errorf("shrink must be between 0 and %d", maxShrink)
	}
	if feather < 0 || feather > maxFeather {
		return fmt.Errorf("feather must be between 0 and %d", maxFeather)
	}
	mode := op.Mode
	if mode == "" {
		mode = "key"
	}
	switch mode {
	case "key":
		return s.matteKey(op, tol, shrink, feather)
	case "mask":
		return s.matteMask(op, shrink, feather)
	default:
		return fmt.Errorf("matte mode %q is not one of key, mask", mode)
	}
}

func (s *state) matteKey(op Op, tol, shrink, feather int) error {
	auto := op.Color == "" || op.Color == "auto"
	var named color.NRGBA
	if !auto {
		c, err := parseColor(op.Color)
		if err != nil {
			return err
		}
		named = c
	}
	contiguous := op.Contiguous == nil || *op.Contiguous
	holesMin := op.HolesMinArea
	if holesMin <= 0 {
		holesMin = defaultHolesMin
	}
	var notes []string
	for i, f := range s.frames {
		key, how := named, "named"
		if auto {
			c, share, transparent := detectKey(f)
			if transparent {
				notes = append(notes, fmt.Sprintf("frame %d already has a transparent border, left alone", i+1))
				continue
			}
			if share < minBorderShare {
				return fmt.Errorf("no flat background colour along the border of frame %d (the best match covers %d%%); name the colour you asked the generator for with color, or use mode mask", i+1, round(share*100))
			}
			key, how = c, fmt.Sprintf("auto, %d%% of the border", round(share*100))
		}
		fg, removed := keyMask(f, key, tol, contiguous, op.Holes, holesMin)
		if removed == 0 {
			notes = append(notes, fmt.Sprintf("frame %d: nothing matched %s within tolerance %d", i+1, hex(key), tol))
			continue
		}
		finishMatte(f, fg, shrink, feather)
		s.key = &key
		notes = append(notes, fmt.Sprintf("frame %d: key %s (%s) removed %d%% of pixels", i+1, hex(key), how, round(100*float64(removed)/float64(len(fg)))))
	}
	s.note("matte", fmt.Sprintf("tolerance %d, shrink %d, feather %d; %s", tol, shrink, feather, joinNotes(notes)))
	return nil
}

func (s *state) matteMask(op Op, shrink, feather int) error {
	if op.Mask == "" {
		return errors.New("mode mask needs mask: the path of a picture that is white where the sprite is and black where it is not")
	}
	if s.env.ReadFile == nil {
		return errors.New("masks are not available here")
	}
	data, err := s.env.ReadFile(op.Mask)
	if err != nil {
		return err
	}
	mask, _, err := Decode(data)
	if err != nil {
		return fmt.Errorf("mask: %w", err)
	}
	for i, f := range s.frames {
		if f.Rect.Dx() != mask.Rect.Dx() || f.Rect.Dy() != mask.Rect.Dy() {
			return fmt.Errorf("mask is %dx%d but frame %d is %dx%d; they must match", mask.Rect.Dx(), mask.Rect.Dy(), i+1, f.Rect.Dx(), f.Rect.Dy())
		}
		fg := make([]bool, f.Rect.Dx()*f.Rect.Dy())
		for p := range fg {
			px := mask.Pix[p*4 : p*4+4]
			luma := (299*int(px[0]) + 587*int(px[1]) + 114*int(px[2])) / 1000
			fg[p] = luma*int(px[3])/255 >= alphaOpaque
		}
		finishMatte(f, fg, shrink, feather)
	}
	s.note("matte", fmt.Sprintf("mask %s, shrink %d, feather %d", op.Mask, shrink, feather))
	return nil
}

// detectKey finds the flat colour along the border. It returns the colour, the share of the border within the default
// tolerance of it, and whether the border is already transparent.
func detectKey(img *image.NRGBA) (color.NRGBA, float64, bool) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	ring := clampInt(min(w, h)/64, 1, 8)
	var samples [][3]uint8
	total, clear := 0, 0
	visit := func(x, y int) {
		p := img.Pix[(y*w+x)*4:]
		total++
		if p[3] < alphaOpaque {
			clear++
			return
		}
		samples = append(samples, [3]uint8{p[0], p[1], p[2]})
	}
	for y := 0; y < h; y++ {
		if y < ring || y >= h-ring {
			for x := 0; x < w; x++ {
				visit(x, y)
			}
			continue
		}
		for x := 0; x < min(ring, w); x++ {
			visit(x, y)
		}
		for x := max(ring, w-ring); x < w; x++ {
			visit(x, y)
		}
	}
	if total == 0 || clear*10 >= total*9 {
		return color.NRGBA{}, 0, true
	}
	if len(samples) == 0 {
		return color.NRGBA{}, 0, false
	}
	// The most common colour, at 4 bits a channel, seeds the search; the mean of the samples near it refines it.
	var buckets [4096]int
	for _, s := range samples {
		buckets[int(s[0]>>4)<<8|int(s[1]>>4)<<4|int(s[2]>>4)]++
	}
	best := 0
	for b, n := range buckets {
		if n > buckets[best] {
			best = b
		}
	}
	center := [3]float64{float64(best>>8&15)*16 + 8, float64(best>>4&15)*16 + 8, float64(best&15)*16 + 8}
	tol2 := float64(defaultTolerance * defaultTolerance)
	near := 0
	for pass := 0; pass < 3; pass++ {
		var sum [3]float64
		near = 0
		for _, s := range samples {
			dr, dg, db := float64(s[0])-center[0], float64(s[1])-center[1], float64(s[2])-center[2]
			if dr*dr+dg*dg+db*db <= tol2 {
				sum[0] += float64(s[0])
				sum[1] += float64(s[1])
				sum[2] += float64(s[2])
				near++
			}
		}
		if near == 0 {
			break
		}
		center = [3]float64{sum[0] / float64(near), sum[1] / float64(near), sum[2] / float64(near)}
	}
	key := color.NRGBA{R: uint8(round(center[0])), G: uint8(round(center[1])), B: uint8(round(center[2])), A: 255}
	return key, float64(near) / float64(total), false
}

// keyMask marks the pixels to keep. A pixel is background when it is within tol of key or already transparent. With
// contiguous, only background connected to the image border is removed; holes also removes enclosed background
// regions of at least holesMin pixels, such as the middle of a ring. It returns the count of background pixels.
func keyMask(img *image.NRGBA, key color.NRGBA, tol int, contiguous, holes bool, holesMin int) ([]bool, int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	n := w * h
	tol2 := tol * tol
	isKey := func(i int) bool {
		p := img.Pix[i*4 : i*4+4]
		return p[3] < alphaOpaque || dist2(p, key) <= tol2
	}
	bg := make([]bool, n)
	removed := 0
	if !contiguous {
		for i := 0; i < n; i++ {
			if isKey(i) {
				bg[i] = true
				removed++
			}
		}
		return invert(bg), removed
	}
	// grow marks every pixel reachable from the seeds through four-connected pixels that ok accepts, and reports each.
	grow := func(seeds []int32, mark []bool, ok func(int) bool, visit func(int)) {
		stack := seeds
		for len(stack) > 0 {
			i := int(stack[len(stack)-1])
			stack = stack[:len(stack)-1]
			visit(i)
			x, y := i%w, i/w
			try := func(j int) {
				if !mark[j] && ok(j) {
					mark[j] = true
					stack = append(stack, int32(j))
				}
			}
			if x > 0 {
				try(i - 1)
			}
			if x < w-1 {
				try(i + 1)
			}
			if y > 0 {
				try(i - w)
			}
			if y < h-1 {
				try(i + w)
			}
		}
	}
	var seeds []int32
	seed := func(i int) {
		if !bg[i] && isKey(i) {
			bg[i] = true
			seeds = append(seeds, int32(i))
		}
	}
	for x := 0; x < w; x++ {
		seed(x)
		seed((h-1)*w + x)
	}
	for y := 0; y < h; y++ {
		seed(y * w)
		seed(y*w + w - 1)
	}
	grow(seeds, bg, isKey, func(int) { removed++ })
	if holes {
		// An enclosed region is cleared only if it is big enough to be a hole and not a speck of background-coloured
		// detail inside the sprite.
		seen := make([]bool, n)
		for i := 0; i < n; i++ {
			if bg[i] || seen[i] || !isKey(i) {
				continue
			}
			seen[i] = true
			var region []int32
			grow([]int32{int32(i)}, seen, func(j int) bool { return !bg[j] && isKey(j) }, func(j int) { region = append(region, int32(j)) })
			if len(region) >= holesMin {
				for _, j := range region {
					bg[j] = true
				}
				removed += len(region)
			}
		}
	}
	return invert(bg), removed
}

func invert(b []bool) []bool {
	for i := range b {
		b[i] = !b[i]
	}
	return b
}

// finishMatte turns the keep mask into alpha: it erodes the mask by shrink pixels, softens the edge by feather
// pixels, and repaints every soft pixel with the colour of the nearest solid one.
func finishMatte(img *image.NRGBA, fg []bool, shrink, feather int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for r := 0; r < shrink; r++ {
		next := make([]bool, len(fg))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				i := y*w + x
				if !fg[i] {
					continue
				}
				// The image edge is not background, so a sprite that touches it is not eaten from outside.
				edge := (x > 0 && !fg[i-1]) || (x < w-1 && !fg[i+1]) || (y > 0 && !fg[i-w]) || (y < h-1 && !fg[i+w])
				next[i] = !edge
			}
		}
		fg = next
	}
	cover := make([]float32, len(fg))
	for i, keep := range fg {
		if keep {
			cover[i] = 1
		}
	}
	if feather > 0 {
		cover = boxBlur(cover, w, h, feather)
	}
	known := make([]bool, len(fg))
	for i := range cover {
		a := uint8(float32(img.Pix[i*4+3])*cover[i] + 0.5)
		if cover[i] >= 0.999 {
			a = img.Pix[i*4+3]
			known[i] = a == 255
		}
		img.Pix[i*4+3] = a
	}
	bleed(img, known, 2*feather+2)
}

// boxBlur averages each value with the values within r pixels, replicating the edge.
func boxBlur(src []float32, w, h, r int) []float32 {
	tmp := make([]float32, len(src))
	out := make([]float32, len(src))
	norm := float32(2*r + 1)
	for y := 0; y < h; y++ {
		row := src[y*w : (y+1)*w]
		for x := 0; x < w; x++ {
			var sum float32
			for k := -r; k <= r; k++ {
				sum += row[clampInt(x+k, 0, w-1)]
			}
			tmp[y*w+x] = sum / norm
		}
	}
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			var sum float32
			for k := -r; k <= r; k++ {
				sum += tmp[clampInt(y+k, 0, h-1)*w+x]
			}
			out[y*w+x] = sum / norm
		}
	}
	return out
}

// bleed paints each visible, not-yet-known pixel with the mean colour of its known neighbours, a ring at a time, so
// the colour of a soft edge comes from inside the sprite and not from the background it was cut from.
func bleed(img *image.NRGBA, known []bool, rounds int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	type paint struct {
		i       int
		r, g, b uint8
	}
	for r := 0; r < rounds; r++ {
		var updates []paint
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				i := y*w + x
				if known[i] || img.Pix[i*4+3] == 0 {
					continue
				}
				sr, sg, sb, n := 0, 0, 0, 0
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						nx, ny := x+dx, y+dy
						if (dx == 0 && dy == 0) || nx < 0 || ny < 0 || nx >= w || ny >= h {
							continue
						}
						j := ny*w + nx
						if known[j] {
							sr += int(img.Pix[j*4])
							sg += int(img.Pix[j*4+1])
							sb += int(img.Pix[j*4+2])
							n++
						}
					}
				}
				if n > 0 {
					updates = append(updates, paint{i, uint8(sr / n), uint8(sg / n), uint8(sb / n)})
				}
			}
		}
		if len(updates) == 0 {
			return
		}
		for _, u := range updates {
			img.Pix[u.i*4], img.Pix[u.i*4+1], img.Pix[u.i*4+2] = u.r, u.g, u.b
			known[u.i] = true
		}
	}
}

func optInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func joinNotes(notes []string) string {
	out := ""
	for i, n := range notes {
		if i > 0 {
			out += "; "
		}
		out += n
	}
	return out
}
