package agentmsg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// relo_image is an optional tool on the same authenticated loopback endpoint as "agents". It shows an agent image
// files from its workspace. The request is small (a key and a few paths), so it fits the default body limit; the
// pictures travel only in the response.
const (
	imageToolName = "image"
	// MaxImagePaths bounds the files one call may show.
	MaxImagePaths = 4
	MinImageEdge  = 256
	MaxImageEdge  = 2576
	imageTimeout  = 30 * time.Second
	// maxImageBlockBytes caps one decoded picture; maxImageResultBytes caps all of a call's pictures together.
	maxImageBlockBytes  = 4 << 20
	maxImageResultBytes = 12 << 20
)

// ImageRequest is the decoded relo_image invocation.
type ImageRequest struct {
	Key   string   `json:"key"`
	Paths []string `json:"paths"`
	// Region crops every image to the same rectangle, in source pixels.
	Region *ImageRegion `json:"region,omitempty"`
	// MaxEdge is the longest edge sent, 0 for the handler's default.
	MaxEdge int `json:"maxEdge,omitempty"`
}

// ImageRegion is a crop rectangle in source pixels from the image's top-left corner.
type ImageRegion struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Image is an MCP image content block. Data is base64 and is never repeated in the text part.
type Image struct {
	MIME string
	Data string
}

// ImageResult is what an ImageHandler returns: Text is serialized as the text block, and each image follows it as an
// image block, in order.
type ImageResult struct {
	Text   any
	Images []Image
}

// ImageHandler services a validated relo_image call. A non-nil error becomes a tool error.
type ImageHandler func(context.Context, ImageRequest) (ImageResult, error)

// WithImages registers the relo_image tool.
func WithImages(handler ImageHandler) Option {
	return func(s *Server) { s.images = handler }
}

func imageToolDescriptor() map[string]any {
	return map[string]any{
		"name":        imageToolName,
		"description": "Relo image viewer: shows image files from your workspace (PNG, JPEG, GIF, WebP, BMP) as images you can see, such as screenshots, reference designs and rendered output. Use your own capability key. paths: 1-4 files, shown in order; to compare a result with its reference, pass both. Large images are shrunk to maxEdge; the text part gives each image's source size and scale, so ask again with region (source pixels) to see detail. It does not take screenshots: write one to a file first. Never claim you saw an image unless an image block is returned.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   256,
					"description": "Per-agent capability key from your prompt",
				},
				"paths": map[string]any{
					"type":        "array",
					"minItems":    1,
					"maxItems":    MaxImagePaths,
					"items":       map[string]any{"type": "string", "maxLength": maxPathBytes},
					"description": "Image files relative to your workspace (absolute paths must be inside it)",
				},
				"region": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"x":      map[string]any{"type": "integer", "minimum": 0},
						"y":      map[string]any{"type": "integer", "minimum": 0},
						"width":  map[string]any{"type": "integer", "minimum": 1},
						"height": map[string]any{"type": "integer", "minimum": 1},
					},
					"required":             []string{"x", "y", "width", "height"},
					"additionalProperties": false,
					"description":          "Optional crop in source pixels, applied to every path",
				},
				"maxEdge": map[string]any{
					"type":        "integer",
					"minimum":     MinImageEdge,
					"maximum":     MaxImageEdge,
					"description": "Longest edge sent, in pixels (default 1568)",
				},
			},
			"required":             []string{"key", "paths"},
			"additionalProperties": false,
		},
	}
}

// callImage validates a relo_image invocation and builds its content: one text block, then one image block per
// picture the handler returned.
func (s *Server) callImage(ctx context.Context, arguments json.RawMessage) (any, *rpcError) {
	if len(arguments) == 0 {
		return nil, invalidParams("arguments are required")
	}
	dec := json.NewDecoder(bytes.NewReader(arguments))
	dec.DisallowUnknownFields()
	var args ImageRequest
	if err := dec.Decode(&args); err != nil {
		return nil, invalidParams("invalid arguments")
	}
	if err := validateImageRequest(args); err != nil {
		return nil, invalidParams(err.Error())
	}
	out, err := s.invokeImages(ctx, args)
	if err != nil {
		return toolResult(err.Error(), true), nil
	}
	text, err := json.Marshal(out.Text)
	if err != nil {
		return toolResult("result is not serializable", true), nil
	}
	content := []any{map[string]any{"type": "text", "text": string(text)}}
	total := 0
	for _, img := range out.Images {
		size, ok := pictureSize(img)
		if !ok || total+size > maxImageResultBytes {
			// The handler sized its pictures already; an image that fails here is dropped rather than sent broken.
			continue
		}
		total += size
		content = append(content, map[string]any{"type": "image", "mimeType": img.MIME, "data": img.Data})
	}
	return map[string]any{"content": content, "isError": false}, nil
}

// validateImageRequest enforces the wire contract. It never echoes the key.
func validateImageRequest(a ImageRequest) error {
	if a.Key == "" || len(a.Key) > 256 {
		return errors.New("invalid image key")
	}
	if len(a.Paths) == 0 || len(a.Paths) > MaxImagePaths {
		return fmt.Errorf("paths must name 1 to %d files", MaxImagePaths)
	}
	if err := validatePaths(a.Paths); err != nil {
		return err
	}
	if r := a.Region; r != nil && (r.X < 0 || r.Y < 0 || r.Width < 1 || r.Height < 1) {
		return errors.New("region needs x and y of 0 or more and a positive width and height")
	}
	if a.MaxEdge != 0 && (a.MaxEdge < MinImageEdge || a.MaxEdge > MaxImageEdge) {
		return fmt.Errorf("maxEdge must be between %d and %d", MinImageEdge, MaxImageEdge)
	}
	return nil
}

// pictureSize returns the decoded size of a PNG or JPEG block, or false for anything else.
func pictureSize(img Image) (int, bool) {
	if (img.MIME != "image/png" && img.MIME != "image/jpeg") || img.Data == "" || len(img.Data) > base64.StdEncoding.EncodedLen(maxImageBlockBytes) {
		return 0, false
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil || len(decoded) > maxImageBlockBytes {
		return 0, false
	}
	return len(decoded), true
}

// invokeImages runs the handler with a deadline and converts panics into tool errors.
func (s *Server) invokeImages(ctx context.Context, req ImageRequest) (out ImageResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			out = ImageResult{}
			err = errors.New("image handler failed")
		}
	}()
	return s.images(ctx, req)
}
