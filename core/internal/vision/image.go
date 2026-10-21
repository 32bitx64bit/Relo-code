package vision

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"

	// Decoders for image.Decode beyond the PNG, JPEG and GIF ones imported above.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

const (
	// DefaultMaxEdge is the longest edge sent when the caller names none. Every vision provider takes it without
	// resizing on its side.
	DefaultMaxEdge = 1568
	MinMaxEdge     = 256
	// MaxMaxEdge is the largest edge the high-resolution vision models accept.
	MaxMaxEdge = 2576
	// MaxFileBytes and MaxPixels refuse a file before it is decoded, so a huge or hostile one cannot take the memory.
	MaxFileBytes = 40 << 20
	MaxPixels    = 36_000_000
	// MaxImageBytes keeps one encoded image under the 5 MB base64 limit of the strictest provider.
	MaxImageBytes = 3584 << 10
	// maxGIFScan bounds the file size for which every GIF frame is decoded to tell whether it is animated.
	maxGIFScan = 8 << 20
)

// jpegQualities are tried in order when an image is too big as PNG, before it is shrunk further.
var jpegQualities = []int{90, 75}

// Region is a crop in source pixels, measured from the image's top-left corner.
type Region struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Options says how to prepare an image. The zero value sends the whole image at DefaultMaxEdge.
type Options struct {
	MaxEdge int
	Region  *Region
}

// Picture is an image ready to show a model, and what was done to get it there.
type Picture struct {
	MIME string
	Data []byte
	// Format is the source file's format: png, jpeg, gif, webp or bmp.
	Format                    string
	SourceWidth, SourceHeight int
	Width, Height             int
	// Crop is the region actually cut, clamped to the image. Nil when the whole image was used.
	Crop *Region
	// Scale is source pixels per sent pixel: 1 when the image was not shrunk.
	Scale float64
	// Animated means the GIF has more frames than the first one shown.
	Animated bool
	// Original means the file's own bytes were sent unchanged.
	Original bool
}

// ApproxTokens is a rough input-token cost of the picture, using Anthropic's width × height / 750.
func (p Picture) ApproxTokens() int {
	return (p.Width*p.Height + 749) / 750
}

// Prepare decodes an image file, crops and shrinks it as asked, and encodes it for a model. It reads at most
// MaxFileBytes and refuses images over MaxPixels before decoding them.
func Prepare(r io.Reader, opts Options) (Picture, error) {
	maxEdge := opts.MaxEdge
	if maxEdge == 0 {
		maxEdge = DefaultMaxEdge
	}
	if maxEdge < MinMaxEdge || maxEdge > MaxMaxEdge {
		return Picture{}, fmt.Errorf("maxEdge must be between %d and %d", MinMaxEdge, MaxMaxEdge)
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return Picture{}, fmt.Errorf("read failed: %v", err)
	}
	if len(data) > MaxFileBytes {
		return Picture{}, fmt.Errorf("file is larger than %d MiB", MaxFileBytes>>20)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if looksLikeSVG(data) {
			return Picture{}, errors.New("SVG is vector source, not pixels: read it as text, or render it with relo_canvas capture and view that")
		}
		return Picture{}, errors.New("not a PNG, JPEG, GIF, WebP or BMP image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Picture{}, errors.New("image has no pixels")
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return Picture{}, fmt.Errorf("image is %dx%d, over the %d megapixel limit", cfg.Width, cfg.Height, MaxPixels/1_000_000)
	}
	pic := Picture{Format: format, SourceWidth: cfg.Width, SourceHeight: cfg.Height, Scale: 1}

	if opts.Region == nil && (format == "png" || format == "jpeg") && len(data) <= MaxImageBytes {
		if w, h := fit(cfg.Width, cfg.Height, maxEdge); w == cfg.Width && h == cfg.Height {
			pic.MIME, pic.Data, pic.Width, pic.Height, pic.Original = "image/"+format, data, w, h, true
			return pic, nil
		}
	}

	src, animated, err := decode(data, format)
	if err != nil {
		return Picture{}, fmt.Errorf("decode failed: %v", err)
	}
	pic.Animated = animated
	bounds := src.Bounds()
	area := bounds
	if opts.Region != nil {
		reg := *opts.Region
		if reg.X < 0 || reg.Y < 0 || reg.Width <= 0 || reg.Height <= 0 {
			return Picture{}, errors.New("region needs x and y of 0 or more and a positive width and height")
		}
		area = image.Rect(reg.X, reg.Y, reg.X+reg.Width, reg.Y+reg.Height).Add(bounds.Min).Intersect(bounds)
		if area.Empty() {
			return Picture{}, fmt.Errorf("region lies outside the %dx%d image", bounds.Dx(), bounds.Dy())
		}
		pic.Crop = &Region{X: area.Min.X - bounds.Min.X, Y: area.Min.Y - bounds.Min.Y, Width: area.Dx(), Height: area.Dy()}
	}
	w, h := fit(area.Dx(), area.Dy(), maxEdge)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if w == area.Dx() && h == area.Dy() {
		draw.Draw(dst, dst.Bounds(), src, area.Min, draw.Src)
	} else {
		// Catmull-Rom widens its kernel as it shrinks, so small UI text averages out instead of aliasing.
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, area, draw.Src, nil)
	}
	mime, out, sent, err := encodeWithin(dst, MaxImageBytes)
	if err != nil {
		return Picture{}, err
	}
	pic.MIME, pic.Data = mime, out
	pic.Width, pic.Height = sent.Dx(), sent.Dy()
	pic.Scale = math.Round(float64(area.Dx())/float64(pic.Width)*1000) / 1000
	return pic, nil
}

// encodeWithin encodes img as PNG, or when that is over limit bytes, as JPEG at falling quality and then at falling
// size until it fits. It returns the bounds of what it encoded, which are smaller than img's only in the last case.
func encodeWithin(img *image.RGBA, limit int) (string, []byte, image.Rectangle, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", nil, image.Rectangle{}, fmt.Errorf("encode failed: %v", err)
	}
	if buf.Len() <= limit {
		return "image/png", buf.Bytes(), img.Bounds(), nil
	}
	// A photo-like image can be too big as PNG. JPEG has no alpha, so it is laid on white first.
	flat := image.NewRGBA(img.Bounds())
	draw.Draw(flat, flat.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
	for {
		for _, quality := range jpegQualities {
			buf.Reset()
			if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: quality}); err != nil {
				return "", nil, image.Rectangle{}, fmt.Errorf("encode failed: %v", err)
			}
			if buf.Len() <= limit {
				return "image/jpeg", buf.Bytes(), flat.Bounds(), nil
			}
		}
		w, h := flat.Bounds().Dx()*4/5, flat.Bounds().Dy()*4/5
		if max(w, h) < MinMaxEdge || min(w, h) < 1 {
			return "", nil, image.Rectangle{}, errors.New("image is still too large after encoding; ask for a region")
		}
		smaller := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(smaller, smaller.Bounds(), flat, flat.Bounds(), draw.Src, nil)
		flat = smaller
	}
}

// decode returns the first frame, and for a GIF small enough to scan, whether more frames follow it.
func decode(data []byte, format string) (image.Image, bool, error) {
	if format == "gif" && len(data) <= maxGIFScan {
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return nil, false, err
		}
		if len(g.Image) == 0 {
			return nil, false, errors.New("GIF has no frames")
		}
		return g.Image[0], len(g.Image) > 1, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, false, err
}

// fit shrinks w×h so the longest edge is at most maxEdge, keeping the aspect ratio. It never enlarges.
func fit(w, h, maxEdge int) (int, int) {
	long := max(w, h)
	if long <= maxEdge {
		return w, h
	}
	scale := float64(maxEdge) / float64(long)
	return max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))
}

func looksLikeSVG(data []byte) bool {
	head := strings.ToLower(string(data[:min(len(data), 1024)]))
	return strings.Contains(head, "<svg")
}

// Open opens an image file inside one of roots. A relative name is looked up in each root in order; an absolute name
// must lie inside one of them. Lookups go through os.Root, so a symlink cannot lead outside the root. The returned
// path is the name relative to the root that held it, with forward slashes.
func Open(roots []string, name string) (*os.File, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsRune(name, 0) {
		return nil, "", errors.New("path must name a file in your workspace")
	}
	tried := false
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, rel := range relCandidates(root, name) {
			tried = true
			f, err := openIn(root, rel)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, "", err
			}
			return f, filepath.ToSlash(rel), nil
		}
	}
	if !tried {
		return nil, "", fmt.Errorf("%s is outside your workspace; copy it into the workspace first", name)
	}
	return nil, "", fmt.Errorf("%s: no such file in your workspace", name)
}

// relCandidates turns name into paths relative to root. An absolute name is compared with the root as given and with
// its symlinks resolved, because the agent may see either form.
func relCandidates(root, name string) []string {
	if !filepath.IsAbs(name) {
		rel := filepath.Clean(name)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
		return []string{rel}
	}
	bases := []string{filepath.Clean(root)}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != bases[0] {
		bases = append(bases, real)
	}
	var out []string
	for _, base := range bases {
		rel, err := filepath.Rel(base, filepath.Clean(name))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

func openIn(root, rel string) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("%s: cannot be opened inside your workspace", filepath.ToSlash(rel))
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", filepath.ToSlash(rel))
	}
	if info.Size() > MaxFileBytes {
		f.Close()
		return nil, fmt.Errorf("%s is larger than %d MiB", filepath.ToSlash(rel), MaxFileBytes>>20)
	}
	return f, nil
}
