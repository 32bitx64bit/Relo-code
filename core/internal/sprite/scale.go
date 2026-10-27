package sprite

import (
	"errors"
	"fmt"
	"image"
	"math"
	"strings"

	"golang.org/x/image/draw"
)

// lanczos3 is the Lanczos windowed sinc with three lobes, a sharp filter that keeps detail when a large picture is
// shrunk to game size.
var lanczos3 = &draw.Kernel{Support: 3, At: func(t float64) float64 {
	t = math.Abs(t)
	switch {
	case t >= 3:
		return 0
	case t == 0:
		return 1
	}
	pt := math.Pi * t
	return 3 * math.Sin(pt) * math.Sin(pt/3) / (pt * pt)
}}

// area averages every source pixel under a destination pixel, which is what a downscale by a whole number should do.
var area = &draw.Kernel{Support: 0.5, At: func(t float64) float64 {
	if math.Abs(t) <= 0.5 {
		return 1
	}
	return 0
}}

func interpolator(name string) (*draw.Kernel, bool, error) {
	switch strings.ToLower(name) {
	case "", "lanczos":
		return lanczos3, false, nil
	case "catmullrom":
		return draw.CatmullRom, false, nil
	case "bilinear":
		return draw.BiLinear, false, nil
	case "area":
		return area, false, nil
	case "nearest":
		return nil, true, nil
	default:
		return nil, false, fmt.Errorf("filter %q is not one of lanczos, catmullrom, area, bilinear, nearest", name)
	}
}

// resample scales img to w by h.
//
// Colour is averaged weighted by alpha, as a ratio of weighted sums, so a transparent pixel contributes nothing and a
// sprite of one colour keeps exactly that colour at its edge however the filter rings. Averaging straight colour, or
// clamping premultiplied channels to 8 bits, either darkens the edge or leaves a bright rim.
func resample(img *image.NRGBA, w, h int, k *draw.Kernel, nearest bool) *image.NRGBA {
	sw, sh := img.Rect.Dx(), img.Rect.Dy()
	if nearest {
		dst := image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.NearestNeighbor.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		return dst
	}
	premul := make([]float32, sw*sh*4)
	for i := 0; i < sw*sh; i++ {
		a := float32(img.Pix[i*4+3]) / 255
		for c := 0; c < 3; c++ {
			premul[i*4+c] = float32(img.Pix[i*4+c]) / 255 * a
		}
		premul[i*4+3] = a
	}
	across := scaleAxis(premul, sw, sh, w, true, k)
	down := scaleAxis(across, w, sh, h, false, k)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		a := down[i*4+3]
		if a < 1.0/512 {
			continue
		}
		for c := 0; c < 3; c++ {
			v := down[i*4+c] / a
			dst.Pix[i*4+c] = uint8(clampInt(round(float64(v)*255), 0, 255))
		}
		dst.Pix[i*4+3] = uint8(clampInt(round(float64(a)*255), 0, 255))
	}
	return dst
}

// scaleAxis resamples four-channel float pixels along one axis: each row to newN pixels when horizontal, each column
// when not. Reading beyond the edge repeats the edge pixel.
func scaleAxis(src []float32, w, h, newN int, horizontal bool, k *draw.Kernel) []float32 {
	n := h
	if horizontal {
		n = w
	}
	ratio := float64(n) / float64(newN)
	// Shrinking widens the kernel by the ratio, which is what averages the source pixels that land on one output pixel.
	stretch := math.Max(1, ratio)
	type tap struct {
		idx int
		w   float32
	}
	taps := make([][]tap, newN)
	for i := range taps {
		centre := (float64(i)+0.5)*ratio - 0.5
		lo, hi := int(math.Ceil(centre-k.Support*stretch)), int(math.Floor(centre+k.Support*stretch))
		var sum float64
		list := make([]tap, 0, hi-lo+1)
		for j := lo; j <= hi; j++ {
			wt := k.At((float64(j) - centre) / stretch)
			if wt == 0 {
				continue
			}
			list = append(list, tap{clampInt(j, 0, n-1), float32(wt)})
			sum += wt
		}
		for j := range list {
			list[j].w /= float32(sum)
		}
		taps[i] = list
	}
	if horizontal {
		out := make([]float32, newN*h*4)
		for y := 0; y < h; y++ {
			for x, list := range taps {
				var acc [4]float32
				for _, t := range list {
					p := src[(y*w+t.idx)*4 : (y*w+t.idx)*4+4]
					acc[0] += p[0] * t.w
					acc[1] += p[1] * t.w
					acc[2] += p[2] * t.w
					acc[3] += p[3] * t.w
				}
				copy(out[(y*newN+x)*4:], acc[:])
			}
		}
		return out
	}
	out := make([]float32, w*newN*4)
	for y, list := range taps {
		for x := 0; x < w; x++ {
			var acc [4]float32
			for _, t := range list {
				p := src[(t.idx*w+x)*4 : (t.idx*w+x)*4+4]
				acc[0] += p[0] * t.w
				acc[1] += p[1] * t.w
				acc[2] += p[2] * t.w
				acc[3] += p[3] * t.w
			}
			copy(out[(y*w+x)*4:], acc[:])
		}
	}
	return out
}

// place copies src into dst with its top-left corner at x, y. Parts outside dst are dropped.
func place(dst, src *image.NRGBA, x, y int) {
	for sy := 0; sy < src.Rect.Dy(); sy++ {
		dy := y + sy
		if dy < 0 || dy >= dst.Rect.Dy() {
			continue
		}
		sx0, sx1 := max(0, -x), min(src.Rect.Dx(), dst.Rect.Dx()-x)
		if sx0 >= sx1 {
			continue
		}
		copy(dst.Pix[dy*dst.Stride+(x+sx0)*4:dy*dst.Stride+(x+sx1)*4], src.Pix[sy*src.Stride+sx0*4:sy*src.Stride+sx1*4])
	}
}

// parseAnchor reads center, top, bottom, left, right or a pair such as bottom-left into -1, 0 or 1 on each axis.
func parseAnchor(a string) (int, int, error) {
	hx, vy := 0, 0
	if a == "" {
		return 0, 0, nil
	}
	for _, tok := range strings.FieldsFunc(strings.ToLower(a), func(r rune) bool { return r == '-' || r == ' ' || r == '_' }) {
		switch tok {
		case "center", "centre", "middle":
		case "left":
			hx = -1
		case "right":
			hx = 1
		case "top":
			vy = -1
		case "bottom":
			vy = 1
		default:
			return 0, 0, fmt.Errorf("anchor %q is not center, top, bottom, left, right or a pair such as bottom-left", a)
		}
	}
	return hx, vy, nil
}

// fit scales each picture to sit inside a box of the given size, with a margin, and puts it on a transparent canvas of
// exactly that size.
func (s *state) fit(op Op) error {
	if len(op.Size) != 2 || op.Size[0] < 1 || op.Size[1] < 1 {
		return errors.New("fit needs size: [width, height]")
	}
	W, H, m := op.Size[0], op.Size[1], op.Margin
	if W > MaxSide || H > MaxSide {
		return fmt.Errorf("size may not exceed %d pixels on a side", MaxSide)
	}
	if m < 0 || 2*m >= W || 2*m >= H {
		return fmt.Errorf("margin %d leaves no room in a %dx%d box", m, W, H)
	}
	interp, nearest, err := interpolator(op.Filter)
	if err != nil {
		return err
	}
	hx, vy, err := parseAnchor(op.Anchor)
	if err != nil {
		return err
	}
	trim := op.Trim == nil || *op.Trim
	contents := make([]*image.NRGBA, len(s.frames))
	for i, f := range s.frames {
		contents[i] = f
		if trim {
			r, ok := bounds(f, alphaVisible)
			if !ok {
				return fmt.Errorf("frame %d has nothing visible to fit; run matte first", i+1)
			}
			contents[i] = crop(f, r)
		}
	}
	scaleFor := func(c *image.NRGBA) float64 {
		sc := math.Min(float64(W-2*m)/float64(c.Rect.Dx()), float64(H-2*m)/float64(c.Rect.Dy()))
		if !op.Upscale && sc > 1 {
			sc = 1
		}
		return sc
	}
	common := math.Inf(1)
	if op.Common {
		for _, c := range contents {
			common = math.Min(common, scaleFor(c))
		}
	}
	shrunk := 0
	for i, c := range contents {
		sc := scaleFor(c)
		if op.Common {
			sc = common
		}
		cw, ch := c.Rect.Dx(), c.Rect.Dy()
		tw, th := max(1, round(float64(cw)*sc)), max(1, round(float64(ch)*sc))
		if tw != cw || th != ch {
			c = resample(c, tw, th, interp, nearest)
			shrunk++
		}
		canvas := image.NewNRGBA(image.Rect(0, 0, W, H))
		ox := m + (W-2*m-tw)*(hx+1)/2
		oy := m + (H-2*m-th)*(vy+1)/2
		place(canvas, c, ox, oy)
		s.frames[i] = canvas
	}
	s.note("fit", fmt.Sprintf("%dx%d box, margin %d; %d of %d resampled", W, H, m, shrunk, len(contents)))
	return nil
}

// resize scales each picture by a size, a factor or a longest edge.
func (s *state) resize(op Op) error {
	given := 0
	if len(op.Size) > 0 {
		given++
	}
	if op.Scale != 0 {
		given++
	}
	if op.MaxEdge != 0 {
		given++
	}
	if given != 1 {
		return errors.New("resize needs exactly one of size, scale or maxEdge")
	}
	interp, nearest, err := interpolator(op.Filter)
	if err != nil {
		return err
	}
	if op.Sharpen < 0 || op.Sharpen > 2 {
		return errors.New("sharpen must be between 0 and 2")
	}
	for i, f := range s.frames {
		fw, fh := f.Rect.Dx(), f.Rect.Dy()
		var tw, th int
		switch {
		case len(op.Size) == 2:
			tw, th = op.Size[0], op.Size[1]
			if tw < 0 || th < 0 || (tw == 0 && th == 0) {
				return errors.New("size needs a positive width, height or both")
			}
			switch {
			case tw == 0:
				tw = round(float64(th) * float64(fw) / float64(fh))
			case th == 0:
				th = round(float64(tw) * float64(fh) / float64(fw))
			case !op.Stretch && math.Abs(float64(tw)/float64(th)-float64(fw)/float64(fh)) > 0.01*float64(fw)/float64(fh):
				return fmt.Errorf("%dx%d changes the aspect ratio of the %dx%d picture; use fit to place it in a box, give one side as 0, or set stretch", tw, th, fw, fh)
			}
		case op.Scale != 0:
			if op.Scale < 0 {
				return errors.New("scale must be positive")
			}
			tw, th = round(float64(fw)*op.Scale), round(float64(fh)*op.Scale)
		case op.MaxEdge != 0:
			if op.MaxEdge < 1 {
				return errors.New("maxEdge must be positive")
			}
			sc := float64(op.MaxEdge) / float64(max(fw, fh))
			tw, th = round(float64(fw)*sc), round(float64(fh)*sc)
		default:
			return errors.New("resize needs size: [width, height], scale or maxEdge")
		}
		tw, th = max(1, tw), max(1, th)
		if tw > MaxSide || th > MaxSide {
			return fmt.Errorf("the result would be %dx%d, over the %d pixel limit", tw, th, MaxSide)
		}
		if !op.Upscale && (tw > fw || th > fh) && len(op.Size) != 2 {
			tw, th = min(tw, fw), min(th, fh)
		}
		if tw != fw || th != fh {
			f = resample(f, tw, th, interp, nearest)
		}
		if op.Sharpen > 0 {
			f = unsharp(f, op.Sharpen)
		}
		s.frames[i] = f
	}
	s.note("resize", fmt.Sprintf("%s filter, sharpen %.1f", strings.ToLower(orDefault(op.Filter, "lanczos")), op.Sharpen))
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// unsharp sharpens by amount using a three by three average as the blurred copy. It works on premultiplied pixels, so
// a transparent neighbour does not darken the edge.
func unsharp(img *image.NRGBA, amount float64) *image.NRGBA {
	src := premul(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(src.Rect)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			var sum [4]int
			n := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := clampInt(x+dx, 0, w-1), clampInt(y+dy, 0, h-1)
					j := (ny*w + nx) * 4
					for c := 0; c < 4; c++ {
						sum[c] += int(src.Pix[j+c])
					}
					n++
				}
			}
			for c := 0; c < 4; c++ {
				blurred := float64(sum[c]) / float64(n)
				v := float64(src.Pix[i+c]) + amount*(float64(src.Pix[i+c])-blurred)
				dst.Pix[i+c] = uint8(clampInt(round(v), 0, 255))
			}
		}
	}
	return unpremul(dst)
}
