// Package sprite turns a generated or drawn picture into a game-ready sprite: background removal, trimming, fitting,
// resizing, outlining, slicing, contact sheets and rule checks. Everything here is deterministic and local; it never
// touches the network, so it is useful on any image, whatever made it.
package sprite

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"strings"

	"golang.org/x/image/draw"

	// Decoders for image.Decode beyond PNG, which is imported above.
	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

const (
	// MaxPixels bounds a decoded input. A generated picture is about 1 to 4 megapixels; the bound leaves room for a 4x
	// upscale while keeping the working copies (a few bytes per pixel each) well under a gigabyte.
	MaxPixels = 16_000_000
	// MaxFrames bounds the images one pipeline holds after a slice.
	MaxFrames = 64
	// MaxSide bounds the width or height of anything a step produces.
	MaxSide = 4096
	// alphaOpaque is the alpha at and above which a pixel counts as part of the sprite.
	alphaOpaque = 128
	// alphaVisible is the alpha above which a pixel counts as ink when measuring bounds and margins.
	alphaVisible = 8
)

// Decode reads a PNG, JPEG, GIF (first frame), WebP or BMP into straight-alpha pixels with the origin at 0,0.
func Decode(data []byte) (*image.NRGBA, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", errors.New("not a PNG, JPEG, GIF, WebP or BMP image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", errors.New("image has no pixels")
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, "", fmt.Errorf("image is %dx%d, over the %d megapixel limit", cfg.Width, cfg.Height, MaxPixels/1_000_000)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode failed: %v", err)
	}
	return toNRGBA(img), format, nil
}

// toNRGBA copies any image into tightly packed straight-alpha pixels whose bounds start at the origin.
func toNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	if n, ok := src.(*image.NRGBA); ok {
		for y := 0; y < b.Dy(); y++ {
			from := (y+b.Min.Y-n.Rect.Min.Y)*n.Stride + (b.Min.X-n.Rect.Min.X)*4
			copy(dst.Pix[y*dst.Stride:y*dst.Stride+b.Dx()*4], n.Pix[from:from+b.Dx()*4])
		}
		return dst
	}
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// EncodePNG encodes img deterministically: pixels that are fully transparent are written as zeros, so they carry no
// leftover colour, and no metadata is added.
func EncodePNG(img *image.NRGBA) ([]byte, error) {
	clean := clone(img)
	for i := 0; i < len(clean.Pix); i += 4 {
		if clean.Pix[i+3] == 0 {
			clean.Pix[i], clean.Pix[i+1], clean.Pix[i+2] = 0, 0, 0
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, clean); err != nil {
		return nil, fmt.Errorf("encode failed: %v", err)
	}
	return buf.Bytes(), nil
}

func clone(img *image.NRGBA) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, img.Rect.Dx(), img.Rect.Dy()))
	copy(out.Pix, img.Pix)
	return out
}

// crop copies the part of img inside r.
func crop(img *image.NRGBA, r image.Rectangle) *image.NRGBA {
	r = r.Intersect(img.Rect)
	out := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		from := (r.Min.Y+y-img.Rect.Min.Y)*img.Stride + (r.Min.X-img.Rect.Min.X)*4
		copy(out.Pix[y*out.Stride:y*out.Stride+r.Dx()*4], img.Pix[from:from+r.Dx()*4])
	}
	return out
}

// premul converts straight alpha to premultiplied, the form in which averaging pixels is correct.
func premul(n *image.NRGBA) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, n.Rect.Dx(), n.Rect.Dy()))
	for i := 0; i < len(n.Pix); i += 4 {
		a := uint32(n.Pix[i+3])
		dst.Pix[i] = uint8((uint32(n.Pix[i])*a + 127) / 255)
		dst.Pix[i+1] = uint8((uint32(n.Pix[i+1])*a + 127) / 255)
		dst.Pix[i+2] = uint8((uint32(n.Pix[i+2])*a + 127) / 255)
		dst.Pix[i+3] = uint8(a)
	}
	return dst
}

// unpremul converts premultiplied pixels back to straight alpha. A kernel with negative lobes can overshoot, so a
// colour is clamped to its alpha first.
func unpremul(p *image.RGBA) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, p.Rect.Dx(), p.Rect.Dy()))
	for i := 0; i < len(p.Pix); i += 4 {
		a := uint32(p.Pix[i+3])
		if a == 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			v := uint32(p.Pix[i+c])
			if v > a {
				v = a
			}
			dst.Pix[i+c] = uint8((v*255 + a/2) / a)
		}
		dst.Pix[i+3] = uint8(a)
	}
	return dst
}

// parseColor reads #rgb, #rrggbb or #rrggbbaa.
func parseColor(s string) (color.NRGBA, error) {
	t := strings.TrimPrefix(strings.TrimSpace(s), "#")
	switch len(t) {
	case 3:
		t = string([]byte{t[0], t[0], t[1], t[1], t[2], t[2]})
	case 6:
		t += "ff"
	case 8:
	default:
		return color.NRGBA{}, fmt.Errorf("colour %q must be #rgb, #rrggbb or #rrggbbaa", s)
	}
	v, err := strconv.ParseUint(t, 16, 32)
	if err != nil {
		return color.NRGBA{}, fmt.Errorf("colour %q must be #rgb, #rrggbb or #rrggbbaa", s)
	}
	return color.NRGBA{R: uint8(v >> 24), G: uint8(v >> 16), B: uint8(v >> 8), A: uint8(v)}, nil
}

func hex(c color.NRGBA) string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

// dist2 is the squared RGB distance between a pixel and a colour.
func dist2(pix []uint8, c color.NRGBA) int {
	dr, dg, db := int(pix[0])-int(c.R), int(pix[1])-int(c.G), int(pix[2])-int(c.B)
	return dr*dr + dg*dg + db*db
}

// bounds returns the rectangle holding every pixel whose alpha exceeds threshold, and false when there is none.
func bounds(img *image.NRGBA, threshold int) (image.Rectangle, bool) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			if int(row[x*4+3]) > threshold {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				maxY = y
			}
		}
	}
	if maxX < 0 {
		return image.Rectangle{}, false
	}
	return image.Rect(minX, minY, maxX+1, maxY+1), true
}

func clampInt(v, lo, hi int) int {
	return max(lo, min(hi, v))
}

func round(v float64) int { return int(math.Floor(v + 0.5)) }
