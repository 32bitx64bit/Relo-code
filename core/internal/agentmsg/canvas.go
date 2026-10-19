package agentmsg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// relo_canvas is an optional tool on the same authenticated loopback endpoint as "agents". It is registered only
// through WithCanvas, so the default catalog keeps exactly one tool.
const (
	canvasToolName = "canvas"
	// maxCanvasBodyBytes bounds the raw JSON body of a canvas call. JSON escaping can inflate source text, so the
	// transport cap sits above the 4 MiB payload cap; the payload itself is validated separately.
	maxCanvasBodyBytes = 6 << 20
	// maxCanvasPayload is the overall decoded-content limit for one canvas request (sources plus assets).
	maxCanvasPayload = 4 << 20
	maxCanvasFiles   = 64
	maxCanvasText    = 512 << 10
	maxCanvasName    = 120
	maxCanvasPath    = 256
	maxCanvasSize    = 4096
	maxCanvasIDLen   = 80
	// Capture waits for a headless render, so it gets a longer deadline than other actions.
	canvasTimeout        = 45 * time.Second
	canvasCaptureTimeout = 60 * time.Second
	// maxCanvasImageBytes caps the screenshot payload accepted back from the extension.
	maxCanvasImageBytes = 4 << 20
)

var (
	canvasIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)
	canvasActions       = map[string]bool{"list": true, "open": true, "create": true, "update": true, "inspect": true, "capture": true, "popout": true, "close": true}
	canvasFormats       = map[string]bool{"html": true, "react": true, "svg": true}
	canvasNeedsID       = map[string]bool{"open": true, "update": true, "inspect": true, "capture": true, "popout": true, "close": true}
	canvasWritesSource  = map[string]bool{"create": true, "update": true}
	errCanvasInvalidKey = errors.New("invalid canvas key")
)

// canvasToolDescriptor describes relo_canvas to the model. Actions that write source are listed so the model can
// choose them; the core still enforces mode and trust rules, so a denied call returns a tool error.
func canvasToolDescriptor() map[string]any {
	return map[string]any{
		"name":        canvasToolName,
		"description": "Relo Canvas: visual artifacts shown to the user. Use your own capability key. action: list, open, create, update, inspect, capture, popout, close. Prefer create/update for design, mockup or visual-exploration requests. Do NOT use Canvas when the user explicitly asks for an HTML/React/SVG/project file: create those real files with repository tools instead. Use inspect/capture to check a render (max 3 iterations). Never claim you saw a screenshot unless an image block is returned.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   256,
					"description": "Per-agent capability key from your prompt",
				},
				"action": map[string]any{
					"type": "string",
					"enum": []string{"list", "open", "create", "update", "inspect", "capture", "popout", "close"},
				},
				"id": map[string]any{
					"type":        "string",
					"maxLength":   maxCanvasIDLen,
					"description": "Canvas artifact id from list or create (required for all actions except list and create)",
				},
				"name": map[string]any{
					"type":        "string",
					"maxLength":   maxCanvasName,
					"description": "Short display name (create only)",
				},
				"format": map[string]any{
					"type":        "string",
					"enum":        []string{"html", "react", "svg"},
					"description": "Artifact format (create only)",
				},
				"entry": map[string]any{
					"type":        "string",
					"maxLength":   maxCanvasPath,
					"description": "Entry file name present in files (create and update)",
				},
				"files": map[string]any{
					"type":                 "object",
					"maxProperties":        maxCanvasFiles,
					"additionalProperties": map[string]any{"type": "string", "maxLength": maxCanvasText},
					"description":          "Complete source files by relative path (create and update); updates REPLACE all files, not merge. At most 64 files, 512 KiB each, 4 MiB total.",
				},
				"assets": map[string]any{
					"type":                 "object",
					"maxProperties":        maxCanvasFiles,
					"additionalProperties": map[string]any{"type": "object", "properties": map[string]any{"mime": map[string]any{"type": "string"}, "data": map[string]any{"type": "string"}}, "required": []string{"mime", "data"}},
					"description":          "Base64 assets by relative path with mime (create and update)",
				},
				"revision": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Revision you are updating from (update only); stale revisions are rejected",
				},
				"width":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxCanvasSize, "description": "Viewport width in CSS pixels (create and capture)"},
				"height": map[string]any{"type": "integer", "minimum": 1, "maximum": maxCanvasSize, "description": "Viewport height in CSS pixels (create and capture)"},
			},
			"required":             []string{"key", "action"},
			"additionalProperties": false,
		},
	}
}

// callCanvas validates a relo_canvas invocation. It checks shape and size only; trust, mode and key ownership are
// enforced by the core handler, which receives the decoded request.
func (s *Server) callCanvas(ctx context.Context, arguments json.RawMessage) (any, *rpcError) {
	if len(arguments) == 0 {
		return nil, invalidParams("arguments are required")
	}
	if len(arguments) > maxCanvasBodyBytes {
		return nil, invalidParams("canvas request too large")
	}
	dec := json.NewDecoder(bytes.NewReader(arguments))
	dec.DisallowUnknownFields()
	var args CanvasRequest
	if err := dec.Decode(&args); err != nil {
		return nil, invalidParams("invalid arguments")
	}
	if err := validateCanvas(args); err != nil {
		return nil, invalidParams(err.Error())
	}
	timeout := canvasTimeout
	if args.Action == "capture" {
		timeout = canvasCaptureTimeout
	}
	out, err := s.invokeCanvas(ctx, args, timeout)
	if err != nil {
		return toolResult(err.Error(), true), nil
	}
	text, image, err := canvasContent(out)
	if err != nil {
		return toolResult("result is not serializable", true), nil
	}
	content := []any{map[string]any{"type": "text", "text": text}}
	if image != nil {
		content = append(content, map[string]any{"type": "image", "mimeType": image.MIME, "data": image.Data})
	}
	return map[string]any{"content": content, "isError": false}, nil
}

// validateCanvas enforces the wire contract before the core sees the request. It never echoes the key.
func validateCanvas(a CanvasRequest) error {
	if a.Key == "" || len(a.Key) > 256 {
		return errCanvasInvalidKey
	}
	if !canvasActions[a.Action] {
		return errors.New("action must be list, open, create, update, inspect, capture, popout, or close")
	}
	if a.ID != "" && !canvasIDPattern.MatchString(a.ID) {
		return errors.New("id must be 1 to 80 lowercase letters, digits or '-' and start with a letter or digit")
	}
	if canvasNeedsID[a.Action] && a.ID == "" {
		return fmt.Errorf("id is required for %s", a.Action)
	}
	if a.Action == "create" && a.Format == "" {
		return errors.New("format is required for create")
	}
	if a.Format != "" && !canvasFormats[a.Format] {
		return errors.New("format must be html, react, or svg")
	}
	if len(a.Name) > maxCanvasName {
		return fmt.Errorf("name must be at most %d bytes", maxCanvasName)
	}
	if a.Revision < 0 {
		return errors.New("revision must not be negative")
	}
	if a.Action == "update" && a.Revision < 1 {
		return errors.New("update requires the current revision (at least 1)")
	}
	if a.Width < 0 || a.Width > maxCanvasSize || a.Height < 0 || a.Height > maxCanvasSize {
		return fmt.Errorf("width and height must be between 1 and %d", maxCanvasSize)
	}
	if !canvasWritesSource[a.Action] {
		if len(a.Files) > 0 || len(a.Assets) > 0 || a.Entry != "" || a.Format != "" {
			return fmt.Errorf("%s does not accept source fields", a.Action)
		}
		return nil
	}
	if a.Action == "update" && len(a.Files) == 0 && len(a.Assets) == 0 {
		return nil // metadata-only update such as a revision bump
	}
	if len(a.Files) > maxCanvasFiles || len(a.Assets) > maxCanvasFiles {
		return fmt.Errorf("at most %d files and %d assets", maxCanvasFiles, maxCanvasFiles)
	}
	total := len(a.Name) + len(a.Entry)
	for path, text := range a.Files {
		if err := validateCanvasPath(path); err != nil {
			return err
		}
		if len(text) > maxCanvasText {
			return fmt.Errorf("file %s exceeds %d bytes", path, maxCanvasText)
		}
		total += len(path) + len(text)
	}
	for path, asset := range a.Assets {
		if err := validateCanvasPath(path); err != nil {
			return err
		}
		if asset.MIME == "" || asset.Data == "" {
			return fmt.Errorf("asset %s needs mime and data", path)
		}
		decoded, err := base64.StdEncoding.DecodeString(asset.Data)
		if err != nil {
			return fmt.Errorf("asset %s is not valid base64", path)
		}
		if len(decoded) > maxCanvasText {
			return fmt.Errorf("asset %s exceeds %d bytes", path, maxCanvasText)
		}
		total += len(path) + len(asset.MIME) + len(asset.Data)
	}
	if total > maxCanvasPayload {
		return fmt.Errorf("canvas content exceeds %d bytes", maxCanvasPayload)
	}
	if a.Action == "create" && a.Entry == "" {
		return errors.New("entry is required for create")
	}
	if a.Entry != "" {
		if err := validateCanvasPath(a.Entry); err != nil {
			return err
		}
		if _, ok := a.Files[a.Entry]; !ok && len(a.Files) > 0 {
			return errors.New("entry must name a file in files")
		}
	}
	return nil
}

func validateCanvasPath(p string) error {
	if p == "" || len(p) > maxCanvasPath || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") {
		return fmt.Errorf("invalid file path %q", truncate(p, 64))
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid file path %q", truncate(p, 64))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// invokeCanvas runs the canvas handler with a deadline that matches the action, and converts panics into tool errors.
func (s *Server) invokeCanvas(ctx context.Context, req CanvasRequest, timeout time.Duration) (out any, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("canvas handler failed")
		}
	}()
	return s.canvas(ctx, req)
}

// canvasContent serializes a canvas result. When the result carries a PNG capture, the base64 payload is lifted out
// of the text metadata and returned as an image content block, so it is never duplicated in the text.
func canvasContent(out any) (string, *CanvasImage, error) {
	if res, ok := out.(CanvasResult); ok {
		data, err := json.Marshal(res.Text)
		if err != nil {
			return "", nil, err
		}
		return string(data), validImage(res.Image), nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", nil, err
	}
	var generic map[string]any
	if json.Unmarshal(raw, &generic) != nil {
		return string(raw), nil, nil
	}
	capture, ok := generic["capture"].(map[string]any)
	if !ok {
		return string(raw), nil, nil
	}
	mime, _ := capture["mime"].(string)
	data, _ := capture["data"].(string)
	image := validImage(&CanvasImage{MIME: mime, Data: data})
	delete(capture, "data")
	if image != nil {
		capture["imageAttached"] = true
	}
	text, err := json.Marshal(generic)
	if err != nil {
		return "", nil, err
	}
	return string(text), image, nil
}

// validImage keeps only well-formed PNG captures within the image size cap. Anything else is text-only, which keeps
// unexpected payloads out of the model context.
func validImage(img *CanvasImage) *CanvasImage {
	if img == nil || img.MIME != "image/png" || img.Data == "" || len(img.Data) > base64.StdEncoding.EncodedLen(maxCanvasImageBytes) {
		return nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(img.Data); err != nil || len(decoded) > maxCanvasImageBytes {
		return nil
	}
	return img
}

// isCanvasCall reports whether a raw JSON-RPC body is a relo_canvas tools/call. It only inspects the envelope and
// never decodes the arguments, so it is safe to run on oversized bodies.
func isCanvasCall(body []byte) bool {
	var env struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &env) != nil {
		return false
	}
	return env.Method == "tools/call" && env.Params.Name == canvasToolName
}
