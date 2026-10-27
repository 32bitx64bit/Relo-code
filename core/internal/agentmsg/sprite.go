package agentmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relocode/relo/internal/sprite"
)

// relo_sprite is an optional tool on the same authenticated loopback endpoint as "agents". It runs a pipeline of
// local picture operations on one image in the agent's workspace and can write the result back as a PNG. The request
// is small (a key, a path and a few steps), so it fits the default body limit; pictures travel only in the response.
const (
	spriteToolName = "sprite"
	spriteTimeout  = 30 * time.Second
)

// SpriteRequest is the decoded relo_sprite invocation.
type SpriteRequest struct {
	Key   string      `json:"key"`
	Input SpriteInput `json:"input"`
	// Ops are applied in order. Empty means inspect the picture and report on it.
	Ops []sprite.Op `json:"ops,omitempty"`
	// Output is the PNG file to write in the agent's workspace. When the pipeline ends with several pictures it must
	// contain {n}, which stands for each picture's number from 1.
	Output string `json:"output,omitempty"`
	// Overwrite lets Output replace a file that already exists.
	Overwrite bool `json:"overwrite,omitempty"`
	// Preview returns the result as a contact sheet picture.
	Preview bool `json:"preview,omitempty"`
}

// SpriteInput names the picture a pipeline starts from.
type SpriteInput struct {
	Path string `json:"path"`
}

// SpriteHandler services a validated relo_sprite call. Its result has the same shape as relo_image's: one text part
// and any number of pictures. A non-nil error becomes a tool error.
type SpriteHandler func(context.Context, SpriteRequest) (ImageResult, error)

// WithSprite registers the relo_sprite tool.
func WithSprite(handler SpriteHandler) Option {
	return func(s *Server) { s.sprite = handler }
}

func spriteToolDescriptor() map[string]any {
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	op := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"op":           map[string]any{"type": "string", "enum": sprite.OpNames()},
			"mode":         str("matte: key (default) or mask. slice: grid or components (default components, grid when cols or rows is set)"),
			"color":        str("matte: the background colour as #rrggbb, or auto (default) to read it from the picture's border. outline: the line colour (default #000000)"),
			"tolerance":    integer("matte: RGB distance counted as background (default 48)"),
			"shrink":       integer("matte: pixels the edge is pulled in to cut the blended fringe (default 1)"),
			"feather":      integer("matte: pixels the edge is softened over (default 1)"),
			"contiguous":   boolean("matte: only remove background connected to the border (default true)"),
			"holes":        boolean("matte: also remove enclosed background regions, such as the middle of a ring"),
			"holesMinArea": integer("matte: smallest enclosed region holes removes, in pixels (default 64)"),
			"mask":         str("matte mask: path of a picture that is white where the sprite is"),
			"threshold":    integer("trim: alpha above which a pixel counts as visible (default 8)"),
			"padding":      integer("trim: transparent pixels kept around the sprite"),
			"size":         map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 2, "maxItems": 2, "description": "fit: the box [width, height]. resize: [width, height]; give one as 0 to keep the aspect ratio"},
			"margin":       integer("fit: transparent border kept inside the box"),
			"anchor":       str("fit: center (default), top, bottom, left, right, or a pair such as bottom-left"),
			"filter":       map[string]any{"type": "string", "enum": []string{"lanczos", "catmullrom", "area", "bilinear", "nearest"}, "description": "fit and resize: lanczos by default; nearest keeps hard pixel edges"},
			"upscale":      boolean("fit and resize: allow enlarging (default: only shrink)"),
			"trim":         boolean("fit: fit the visible part, not the whole picture (default true)"),
			"common":       boolean("fit: give every picture the smallest scale any of them needs, so relative sizes survive a slice"),
			"scale":        map[string]any{"type": "number", "description": "resize: factor"},
			"maxEdge":      integer("resize: longest edge in pixels"),
			"stretch":      boolean("resize: allow a size that changes the aspect ratio"),
			"sharpen":      map[string]any{"type": "number", "minimum": 0, "maximum": 2, "description": "resize: unsharp amount"},
			"width":        integer("outline: line width in pixels (default 1, at most 6). It never grows the picture: leave margin in fit"),
			"axis":         map[string]any{"type": "string", "enum": []string{"h", "v"}, "description": "mirror: h flips left to right (default), v top to bottom"},
			"degrees":      map[string]any{"type": "integer", "enum": []int{90, 180, 270}, "description": "rotate: clockwise"},
			"cols":         integer("slice grid: columns"),
			"rows":         integer("slice grid: rows"),
			"gap":          integer("slice components: pixels apart that still count as one sprite"),
			"minArea":      integer("slice components: smallest piece kept, in pixels (default 64)"),
			"background":   str("sheet: checker (default), a colour such as #3a6b35, or the path of a picture to tile"),
			"scales":       map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "maxItems": 4, "description": "sheet: magnifications, each 1 to 8 (default [1, 3])"},
			"labels":       boolean("sheet: label each row (default true)"),
			"rules": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"width":         integer("exact width"),
					"height":        integer("exact height"),
					"maxWidth":      integer("largest width"),
					"maxHeight":     integer("largest height"),
					"alpha":         boolean("require a transparent background: some pixels clear and some solid"),
					"margin":        integer("fewest clear pixels between the sprite and every edge"),
					"minOpaque":     map[string]any{"type": "number", "description": "fewest solid pixels, as a fraction of the picture"},
					"maxOpaque":     map[string]any{"type": "number", "description": "most solid pixels, as a fraction of the picture"},
					"maxBytes":      integer("largest encoded PNG"),
					"namePattern":   str("regular expression the output file name must match"),
					"maxKeyResidue": map[string]any{"type": "number", "description": "share of visible pixels allowed to still look like the removed background (default 0.01)"},
				},
				"additionalProperties": false,
				"description":          "check: the rules to test. Leave out a rule to skip it",
			},
			"strict": boolean("check: stop the run, and write nothing, when a rule fails"),
		},
		"required":             []string{"op"},
		"additionalProperties": false,
	}
	return map[string]any{
		"name": spriteToolName,
		"description": "Relo sprite pipeline: turns a picture into a game-ready sprite, locally and deterministically. Use your own capability key. " +
			"input.path is an image in your workspace: a generator's output, a reference, or a sprite you made. ops run on it in order. " +
			"matte turns a flat background into transparency: ask the generator for a solid background colour the sprite does not contain, such as #ff00ff, and matte reads it from the border (or name it with color) and repaints the soft edge so no trace of it is left. " +
			"Then trim, fit (scale into size [w,h] with a margin and an anchor), resize, outline, mirror, rotate, slice (one picture into many) and sheet (a contact sheet at several magnifications). " +
			"inspect reports a picture's size, how much of it is solid, and its bounds. check tests rules and, with strict, stops the run before anything is written. " +
			"output writes a PNG into your workspace, where it shows up among your changes; put {n} in the name to write several pictures, and set overwrite to replace a file. " +
			"preview returns the result as an image on a checker background at 1x and 3x: look at it before you report. " +
			"The text part lists each step, every check and the size of each result. " +
			"Example: ops [{op:matte},{op:fit,size:[128,128],margin:4},{op:outline},{op:check,rules:{width:128,height:128,alpha:true},strict:true}], output \"art/out/iron.png\", preview true.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string", "minLength": 1, "maxLength": 256, "description": "Per-agent capability key from your prompt"},
				"input": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{"path": map[string]any{"type": "string", "maxLength": maxPathBytes, "description": "Image file relative to your workspace (absolute paths must be inside it)"}},
					"required":             []string{"path"},
					"additionalProperties": false,
				},
				"ops":       map[string]any{"type": "array", "maxItems": sprite.MaxOps, "items": op, "description": "Steps to apply, in order. Leave empty to inspect the picture"},
				"output":    map[string]any{"type": "string", "maxLength": maxPathBytes, "description": "PNG file to write in your workspace. Use {n} when the pipeline ends with several pictures"},
				"overwrite": map[string]any{"type": "boolean", "description": "Replace output if it exists"},
				"preview":   map[string]any{"type": "boolean", "description": "Return the result as a contact sheet image"},
			},
			"required":             []string{"key", "input"},
			"additionalProperties": false,
		},
	}
}

// callSprite validates a relo_sprite invocation and builds its content: one text block, then the preview picture if
// the handler returned one.
func (s *Server) callSprite(ctx context.Context, arguments json.RawMessage) (any, *rpcError) {
	if len(arguments) == 0 {
		return nil, invalidParams("arguments are required")
	}
	dec := json.NewDecoder(bytes.NewReader(arguments))
	dec.DisallowUnknownFields()
	var args SpriteRequest
	if err := dec.Decode(&args); err != nil {
		return nil, invalidParams("invalid arguments")
	}
	if err := validateSpriteRequest(args); err != nil {
		return nil, invalidParams(err.Error())
	}
	out, err := s.invokeSprite(ctx, args)
	if err != nil {
		return toolResult(err.Error(), true), nil
	}
	return imageContent(out), nil
}

// validateSpriteRequest enforces the wire contract. It never echoes the key.
func validateSpriteRequest(a SpriteRequest) error {
	if a.Key == "" || len(a.Key) > 256 {
		return errors.New("invalid sprite key")
	}
	if err := validatePaths([]string{a.Input.Path}); err != nil {
		return errors.New("input.path must name an image file in your workspace")
	}
	if len(a.Ops) > sprite.MaxOps {
		return fmt.Errorf("ops holds at most %d steps", sprite.MaxOps)
	}
	known := map[string]bool{}
	for _, name := range sprite.OpNames() {
		known[name] = true
	}
	for i, op := range a.Ops {
		if !known[op.Op] {
			return fmt.Errorf("ops[%d].op %q is not one of %s", i, op.Op, strings.Join(sprite.OpNames(), ", "))
		}
	}
	if a.Output != "" {
		if err := validatePaths([]string{a.Output}); err != nil {
			return errors.New("output must be a workspace-relative file path")
		}
		if !strings.HasSuffix(strings.ToLower(a.Output), ".png") {
			return errors.New("output must be a .png file")
		}
	}
	return nil
}

// invokeSprite runs the handler with a deadline and converts panics into tool errors.
func (s *Server) invokeSprite(ctx context.Context, req SpriteRequest) (out ImageResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, spriteTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			out = ImageResult{}
			err = errors.New("sprite handler failed")
		}
	}()
	return s.sprite(ctx, req)
}
