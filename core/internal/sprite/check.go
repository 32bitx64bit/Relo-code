package sprite

import (
	"fmt"
	"image"
	"image/color"
	"path"
	"regexp"
	"strings"
)

// defaultKeyResidue is the share of visible pixels that may still look like the removed background before the
// keyResidue rule fails. It applies whenever a matte removed a colour earlier in the same pipeline.
const defaultKeyResidue = 0.01

// keyResidueDistance2 is the squared RGB distance within which a pixel still reads as the background colour.
const keyResidueDistance2 = 2 * defaultTolerance * 2 * defaultTolerance

// Rules is what check tests. A field left at its zero value is not tested.
type Rules struct {
	Width     int `json:"width,omitempty"`  // exact width
	Height    int `json:"height,omitempty"` // exact height
	MaxWidth  int `json:"maxWidth,omitempty"`
	MaxHeight int `json:"maxHeight,omitempty"`
	// Alpha requires a transparent background: some pixels fully transparent and some solid.
	Alpha bool `json:"alpha,omitempty"`
	// Margin is the fewest fully transparent pixels there must be between the sprite and every edge.
	Margin    *int     `json:"margin,omitempty"`
	MinOpaque *float64 `json:"minOpaque,omitempty"` // fewest solid pixels, as a fraction of the picture
	MaxOpaque *float64 `json:"maxOpaque,omitempty"` // most solid pixels, as a fraction of the picture
	MaxBytes  int      `json:"maxBytes,omitempty"`  // largest encoded PNG
	// NamePattern is a regular expression the file name of the output must match.
	NamePattern string `json:"namePattern,omitempty"`
	// MaxKeyResidue is the share of visible pixels allowed to still look like the removed background (default 0.01).
	MaxKeyResidue *float64 `json:"maxKeyResidue,omitempty"`
}

func (s *state) check(op Op) error {
	rules := op.Rules
	if rules == nil {
		return fmt.Errorf("check needs rules")
	}
	var results []Check
	add := func(frame int, rule string, ok bool, detail string) {
		results = append(results, Check{Frame: frame, Rule: rule, OK: ok, Detail: detail})
	}
	if rules.NamePattern != "" {
		re, err := regexp.Compile(rules.NamePattern)
		if err != nil {
			return fmt.Errorf("namePattern is not a regular expression: %v", err)
		}
		if s.env.Output == "" {
			add(0, "namePattern", false, "no output file is named in this request")
		} else {
			name := path.Base(strings.ReplaceAll(s.env.Output, "\\", "/"))
			add(0, "namePattern", re.MatchString(name), name)
		}
	}
	for i, f := range s.frames {
		n := i + 1
		w, h := f.Rect.Dx(), f.Rect.Dy()
		info := describe(f)
		if rules.Width > 0 {
			add(n, "width", w == rules.Width, fmt.Sprintf("%d, want %d", w, rules.Width))
		}
		if rules.Height > 0 {
			add(n, "height", h == rules.Height, fmt.Sprintf("%d, want %d", h, rules.Height))
		}
		if rules.MaxWidth > 0 {
			add(n, "maxWidth", w <= rules.MaxWidth, fmt.Sprintf("%d, most %d", w, rules.MaxWidth))
		}
		if rules.MaxHeight > 0 {
			add(n, "maxHeight", h <= rules.MaxHeight, fmt.Sprintf("%d, most %d", h, rules.MaxHeight))
		}
		if rules.Alpha {
			clear := hasClearPixel(f)
			add(n, "alpha", clear && info.Opaque > 0, fmt.Sprintf("transparent pixels: %t, solid share %.2f", clear, info.Opaque))
		}
		if rules.Margin != nil {
			r, ok := bounds(f, alphaVisible)
			switch {
			case !ok:
				add(n, "margin", false, "nothing visible")
			default:
				least := min(min(r.Min.X, r.Min.Y), min(w-r.Max.X, h-r.Max.Y))
				add(n, "margin", least >= *rules.Margin, fmt.Sprintf("%d, want at least %d", least, *rules.Margin))
			}
		}
		if rules.MinOpaque != nil {
			add(n, "minOpaque", info.Opaque >= *rules.MinOpaque, fmt.Sprintf("%.3f, want at least %.3f", info.Opaque, *rules.MinOpaque))
		}
		if rules.MaxOpaque != nil {
			add(n, "maxOpaque", info.Opaque <= *rules.MaxOpaque, fmt.Sprintf("%.3f, want at most %.3f", info.Opaque, *rules.MaxOpaque))
		}
		if rules.MaxBytes > 0 {
			data, err := EncodePNG(f)
			if err != nil {
				return err
			}
			add(n, "maxBytes", len(data) <= rules.MaxBytes, fmt.Sprintf("%d bytes, most %d", len(data), rules.MaxBytes))
		}
		if s.key != nil {
			limit := defaultKeyResidue
			if rules.MaxKeyResidue != nil {
				limit = *rules.MaxKeyResidue
			}
			share := keyResidue(f, *s.key)
			add(n, "keyResidue", share <= limit, fmt.Sprintf("%.4f of visible pixels still look like %s, most %.4f", share, hex(*s.key), limit))
		}
	}
	failed := ""
	for _, c := range results {
		if !c.OK {
			s.report.Failed = true
			if failed != "" {
				failed += ", "
			}
			failed += c.Rule
			if c.Frame > 0 && len(s.frames) > 1 {
				failed += fmt.Sprintf(" (frame %d)", c.Frame)
			}
		}
	}
	s.report.Checks = append(s.report.Checks, results...)
	if failed == "" {
		s.note("check", fmt.Sprintf("%d rules passed", len(results)))
		return nil
	}
	s.note("check", "failed: "+failed)
	if op.Strict {
		s.aborted = true
		s.report.Aborted = "strict check failed: " + failed
	}
	return nil
}

func hasClearPixel(img *image.NRGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] == 0 {
			return true
		}
	}
	return false
}

// keyResidue is the share of visible pixels whose colour is close to the background colour that was removed.
func keyResidue(img *image.NRGBA, key color.NRGBA) float64 {
	visible, near := 0, 0
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i+3] <= alphaVisible {
			continue
		}
		visible++
		if dist2(img.Pix[i:i+4], key) <= keyResidueDistance2 {
			near++
		}
	}
	if visible == 0 {
		return 0
	}
	return float64(near) / float64(visible)
}
