package sprite

import (
	"fmt"
	"image"
	"image/color"
	"strings"
)

// MaxOps bounds the steps in one pipeline.
const MaxOps = 24

// Op is one step of a pipeline. Op names the step; the other fields are its parameters, and a field a step does not
// use is ignored. The steps are inspect, matte, trim, fit, resize, outline, mirror, rotate, slice, sheet and check.
type Op struct {
	Op string `json:"op"`

	// Mode picks a variant: matte key or mask, slice grid or components.
	Mode string `json:"mode,omitempty"`
	// Color is the background colour for matte (#rrggbb, or auto to detect it from the border) and the line colour
	// for outline.
	Color string `json:"color,omitempty"`

	// matte
	Tolerance    *int   `json:"tolerance,omitempty"`    // RGB distance counted as background (default 48)
	Shrink       *int   `json:"shrink,omitempty"`       // pixels the edge is pulled in to cut the blended fringe (default 1)
	Feather      *int   `json:"feather,omitempty"`      // pixels the edge is softened over (default 1)
	Contiguous   *bool  `json:"contiguous,omitempty"`   // only remove background connected to the border (default true)
	Holes        bool   `json:"holes,omitempty"`        // also remove enclosed background regions
	HolesMinArea int    `json:"holesMinArea,omitempty"` // smallest enclosed region holes removes, in pixels (default 64)
	Mask         string `json:"mask,omitempty"`         // path of a mask picture, white where the sprite is

	// trim
	Threshold *int `json:"threshold,omitempty"` // alpha above which a pixel counts as ink (default 8)
	Padding   int  `json:"padding,omitempty"`   // transparent pixels kept around the trimmed sprite

	// fit and resize
	Size    []int   `json:"size,omitempty"`    // [width, height]
	Margin  int     `json:"margin,omitempty"`  // fit: transparent border inside the box
	Anchor  string  `json:"anchor,omitempty"`  // fit: center, top, bottom, left, right, or a pair like bottom-left
	Filter  string  `json:"filter,omitempty"`  // lanczos (default), catmullrom, area, bilinear, nearest
	Upscale bool    `json:"upscale,omitempty"` // fit and resize: allow enlarging (default: only shrink)
	Trim    *bool   `json:"trim,omitempty"`    // fit: use the bounds of the ink, not the whole image (default true)
	Common  bool    `json:"common,omitempty"`  // fit: give every frame the smallest scale any frame needs
	Scale   float64 `json:"scale,omitempty"`   // resize: factor
	MaxEdge int     `json:"maxEdge,omitempty"` // resize: longest edge
	Stretch bool    `json:"stretch,omitempty"` // resize: allow a size that changes the aspect ratio
	Sharpen float64 `json:"sharpen,omitempty"` // resize: unsharp amount, 0 to 2

	// outline
	Width int `json:"width,omitempty"` // line width in pixels (default 1)

	// mirror and rotate
	Axis    string `json:"axis,omitempty"`    // mirror: h (left to right, default) or v
	Degrees int    `json:"degrees,omitempty"` // rotate: 90, 180 or 270 clockwise

	// slice
	Cols    int `json:"cols,omitempty"`
	Rows    int `json:"rows,omitempty"`
	Gap     int `json:"gap,omitempty"`     // components: pixels apart that still count as one sprite
	MinArea int `json:"minArea,omitempty"` // components: smallest sprite, in pixels (default 64)

	// sheet
	Background string `json:"background,omitempty"` // checker (default), #rrggbb, or the path of a picture to tile
	Scales     []int  `json:"scales,omitempty"`     // magnifications to show each sprite at (default 1 and 3)
	Labels     *bool  `json:"labels,omitempty"`

	// check
	Rules  *Rules `json:"rules,omitempty"`
	Strict bool   `json:"strict,omitempty"` // stop the pipeline, and write nothing, when a rule fails
}

// Env is what a pipeline may reach outside its pictures.
type Env struct {
	// ReadFile loads a file the caller allows, such as a mask or a sheet background. Nil refuses all of them.
	ReadFile func(name string) ([]byte, error)
	// Output names the file the result is meant for, which the name rule of check tests.
	Output string
}

// Rect is a rectangle in pixels.
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// FrameInfo describes one picture.
type FrameInfo struct {
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Opaque  float64 `json:"opaque"`            // fraction of pixels that are part of the sprite
	Partial float64 `json:"partial,omitempty"` // fraction that are partly transparent
	Ink     *Rect   `json:"ink,omitempty"`     // bounds of the visible pixels; absent when there are none
	Colors  int     `json:"colors"`            // distinct colours among the solid pixels, at 5 bits a channel
}

// Step records what one op did.
type Step struct {
	Op     string      `json:"op"`
	Note   string      `json:"note,omitempty"`
	Frames []FrameInfo `json:"frames,omitempty"`
}

// Check is the outcome of one rule on one picture.
type Check struct {
	Frame  int    `json:"frame"`
	Rule   string `json:"rule"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Report says what a pipeline did and what its result looks like.
type Report struct {
	Steps   []Step      `json:"steps"`
	Checks  []Check     `json:"checks,omitempty"`
	Frames  []FrameInfo `json:"frames"`
	Failed  bool        `json:"failed,omitempty"`  // a check rule failed
	Aborted string      `json:"aborted,omitempty"` // set when a strict check stopped the pipeline
}

type state struct {
	frames  []*image.NRGBA
	env     Env
	key     *color.NRGBA // the background colour the last matte removed
	report  Report
	aborted bool
}

func (s *state) note(op, text string) {
	s.report.Steps = append(s.report.Steps, Step{Op: op, Note: text})
}

var opNames = []string{"inspect", "matte", "trim", "fit", "resize", "outline", "mirror", "rotate", "slice", "sheet", "check"}

// OpNames lists the ops a pipeline may use.
func OpNames() []string { return append([]string(nil), opNames...) }

// Run applies ops to in, in order. It returns the resulting pictures and a report. A step that cannot be applied
// ends the run with an error that names the step; a strict check that fails ends it early, with Report.Aborted set.
func Run(in *image.NRGBA, ops []Op, env Env) ([]*image.NRGBA, *Report, error) {
	if len(ops) > MaxOps {
		return nil, nil, fmt.Errorf("a pipeline may have at most %d steps", MaxOps)
	}
	s := &state{frames: []*image.NRGBA{clone(in)}, env: env}
	for i, op := range ops {
		if err := s.apply(op); err != nil {
			return nil, nil, fmt.Errorf("step %d (%s): %w", i+1, op.Op, err)
		}
		if s.aborted {
			break
		}
		for _, f := range s.frames {
			if f.Rect.Dx() > MaxSide || f.Rect.Dy() > MaxSide {
				return nil, nil, fmt.Errorf("step %d (%s): the result is %dx%d, over the %d pixel limit", i+1, op.Op, f.Rect.Dx(), f.Rect.Dy(), MaxSide)
			}
		}
	}
	for _, f := range s.frames {
		s.report.Frames = append(s.report.Frames, describe(f))
	}
	return s.frames, &s.report, nil
}

func (s *state) apply(op Op) error {
	switch op.Op {
	case "inspect":
		step := Step{Op: "inspect"}
		for _, f := range s.frames {
			step.Frames = append(step.Frames, describe(f))
		}
		s.report.Steps = append(s.report.Steps, step)
		return nil
	case "matte":
		return s.matte(op)
	case "trim":
		return s.trim(op)
	case "fit":
		return s.fit(op)
	case "resize":
		return s.resize(op)
	case "outline":
		return s.outline(op)
	case "mirror":
		return s.mirror(op)
	case "rotate":
		return s.rotate(op)
	case "slice":
		return s.slice(op)
	case "sheet":
		return s.sheet(op)
	case "check":
		return s.check(op)
	default:
		return fmt.Errorf("unknown op %q; the ops are %s", op.Op, strings.Join(opNames, ", "))
	}
}

// describe measures a picture.
func describe(img *image.NRGBA) FrameInfo {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	info := FrameInfo{Width: w, Height: h}
	var opaque, partial int
	var seen [32768]bool
	for i := 0; i < w*h; i++ {
		p := img.Pix[i*4 : i*4+4]
		if p[3] >= alphaOpaque {
			opaque++
			c := int(p[0]>>3)<<10 | int(p[1]>>3)<<5 | int(p[2]>>3)
			if !seen[c] {
				seen[c] = true
				info.Colors++
			}
		}
		if p[3] > 0 && p[3] < 255 {
			partial++
		}
	}
	info.Opaque = ratio(opaque, w*h)
	info.Partial = ratio(partial, w*h)
	if r, ok := bounds(img, alphaVisible); ok {
		info.Ink = &Rect{X: r.Min.X, Y: r.Min.Y, Width: r.Dx(), Height: r.Dy()}
	}
	return info
}

func ratio(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(round(float64(n)/float64(of)*10000)) / 10000
}
