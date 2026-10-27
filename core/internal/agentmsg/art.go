package agentmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// relo_art is an optional tool on the same authenticated loopback endpoint as "agents". It makes pictures through an
// OpenAI-compatible image API that the user pays for per picture: a guide, reference pictures, a prompt, and then the
// jobs themselves. A job takes from several seconds to a couple of minutes, so generating returns at once and the agent
// comes back with "wait", which blocks for at most artMaxWait. The request is small; pictures travel only in the
// response.
const (
	artToolName = "art"
	// artMaxWait is the longest one wait blocks. It sits under artTimeout, which sits under the server's write timeout.
	artMaxWait = 45
	artTimeout = 55 * time.Second
	// MaxArtSources and MaxArtRefs bound the pictures one call names.
	MaxArtSources = 6
	MaxArtRefs    = 6
	maxArtPrompt  = 4000
	maxArtJobs    = 20
	maxArtPicture = 12
)

// ArtActions are the things relo_art can be asked to do.
var ArtActions = []string{"status", "playbook", "refs", "generate", "wait", "cancel", "jobs"}

// ArtRequest is the decoded relo_art invocation. Which fields matter depends on Action.
type ArtRequest struct {
	Key    string `json:"key"`
	Action string `json:"action"`
	// Topic names a playbook section (action playbook).
	Topic string `json:"topic,omitempty"`
	// Sources are the pictures to store as references (action refs).
	Sources []ArtSource `json:"sources,omitempty"`
	// Prompt and the fields after it describe new pictures (action generate). Refs are ids from action refs, in the
	// order the prompt may call them image 1, image 2, and so on.
	Prompt     string   `json:"prompt,omitempty"`
	Refs       []string `json:"refs,omitempty"`
	Quality    string   `json:"quality,omitempty"`
	Size       string   `json:"size,omitempty"`
	Background string   `json:"background,omitempty"`
	Count      int      `json:"count,omitempty"`
	// Job names the job to wait for or cancel.
	Job string `json:"job,omitempty"`
	// WaitSeconds is how long wait may block, up to artMaxWait.
	WaitSeconds int `json:"waitSeconds,omitempty"`
	// Limit bounds the jobs listed.
	Limit int `json:"limit,omitempty"`
}

// ArtSource is a picture to turn into a reference: a file in the workspace, or a picture of an earlier job.
type ArtSource struct {
	Path  string `json:"path,omitempty"`
	Job   string `json:"job,omitempty"`
	Index int    `json:"index,omitempty"`
}

// ArtHandler services a validated relo_art call. Its result has the same shape as relo_image's: one text part and any
// number of pictures. A non-nil error becomes a tool error.
type ArtHandler func(context.Context, ArtRequest) (ImageResult, error)

// WithArt registers the relo_art tool.
func WithArt(handler ArtHandler) Option {
	return func(s *Server) { s.art = handler }
}

func artToolDescriptor() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	return map[string]any{
		"name": artToolName,
		"description": "Relo art: makes pictures with an image model the user pays for per picture. Use your own capability key. Every job costs real money, so look before you ask for more. " +
			"Start with action status (is it set up, the model, the budget left) and action playbook (the guide to prompts, references, transparency and sprites; topic narrows it). " +
			"action refs stores pictures to start from: sources are files in your workspace {path} or pictures of earlier jobs {job, index}; it returns an id for each. " +
			"action generate starts a job and returns at once: prompt describes the picture in plain language; refs lists stored reference ids in the order the prompt may call them image 1, image 2 and so on (with refs the model edits or restyles them); " +
			"quality, size, background and count override the user's defaults and change the price. " +
			"The user may be asked to approve it. action wait blocks up to waitSeconds (at most " + fmt.Sprint(artMaxWait) + ") for a job and returns a contact sheet of its numbered pictures when it is ready: call it again while the state is still queued or running. " +
			"cancel stops a job that has not finished. jobs lists your jobs and the money used. " +
			"Pictures land in a folder outside your workspace: feed the one you choose to relo_sprite with its path from the job (it is in the result). " +
			"When a call says a limit is reached, stop, use what you have, and tell the user.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":    map[string]any{"type": "string", "minLength": 1, "maxLength": 256, "description": "Per-agent capability key from your prompt"},
				"action": map[string]any{"type": "string", "enum": ArtActions},
				"topic":  str("playbook: overview (default), prompts, refs, sprites, views, iterate or cost"),
				"sources": map[string]any{
					"type": "array", "maxItems": MaxArtSources, "description": "refs: the pictures to store",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":  map[string]any{"type": "string", "maxLength": maxPathBytes, "description": "An image file in your workspace"},
							"job":   str("A job id, with index"),
							"index": integer("The picture number within job, from 1"),
						},
						"additionalProperties": false,
					},
				},
				"prompt": map[string]any{"type": "string", "maxLength": maxArtPrompt, "description": "generate: what to draw, as plain text"},
				"refs": map[string]any{
					"type": "array", "maxItems": MaxArtRefs, "description": "generate: ids returned by action refs, in the order the prompt may call them image 1, image 2, ...",
					"items": map[string]any{"type": "string", "maxLength": 40},
				},
				"quality":     map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "auto"}, "description": "generate: price and detail. Explore at low, finish at medium or high. Default: the user's setting"},
				"size":        map[string]any{"type": "string", "enum": []string{"1024x1024", "1536x1024", "1024x1536", "auto"}, "description": "generate: picture size. Default: the user's setting"},
				"background":  map[string]any{"type": "string", "enum": []string{"auto", "transparent", "opaque"}, "description": "generate: transparent asks for a real alpha channel when the model can; if the provider refuses it, ask for a flat key colour and use relo_sprite matte"},
				"count":       map[string]any{"type": "integer", "minimum": 1, "maximum": 4, "description": "generate: how many pictures to make, each one billed. Default: the user's setting"},
				"job":         str("wait, cancel: the job id, such as j3"),
				"waitSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": artMaxWait, "description": "wait: how long to block (default 25)"},
				"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": maxArtJobs, "description": "jobs: how many to list (default 10)"},
			},
			"required":             []string{"key", "action"},
			"additionalProperties": false,
		},
	}
}

// callArt validates a relo_art invocation and builds its content: one text block, then any pictures.
func (s *Server) callArt(ctx context.Context, arguments json.RawMessage) (any, *rpcError) {
	if len(arguments) == 0 {
		return nil, invalidParams("arguments are required")
	}
	dec := json.NewDecoder(bytes.NewReader(arguments))
	dec.DisallowUnknownFields()
	var args ArtRequest
	if err := dec.Decode(&args); err != nil {
		return nil, invalidParams("invalid arguments")
	}
	if err := validateArtRequest(args); err != nil {
		return nil, invalidParams(err.Error())
	}
	out, err := s.invokeArt(ctx, args)
	if err != nil {
		return toolResult(err.Error(), true), nil
	}
	return imageContent(out), nil
}

// validateArtRequest enforces the wire contract. It never echoes the key. What the image API will accept beyond this
// (the model's own limits) is the provider's to say, and its answer comes back as a tool error.
func validateArtRequest(a ArtRequest) error {
	if a.Key == "" || len(a.Key) > 256 {
		return errors.New("invalid art key")
	}
	known := false
	for _, name := range ArtActions {
		if a.Action == name {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("action %q is not one of %s", a.Action, strings.Join(ArtActions, ", "))
	}
	if len(a.Topic) > 40 {
		return errors.New("topic is too long")
	}
	switch a.Action {
	case "refs":
		if len(a.Sources) == 0 || len(a.Sources) > MaxArtSources {
			return fmt.Errorf("sources must name 1 to %d pictures", MaxArtSources)
		}
		for i, src := range a.Sources {
			switch {
			case src.Path != "" && src.Job != "":
				return fmt.Errorf("sources[%d] names a path or a job, not both", i)
			case src.Path != "":
				if err := validatePaths([]string{src.Path}); err != nil {
					return fmt.Errorf("sources[%d].path must be a file path in your workspace", i)
				}
			case src.Job != "":
				if len(src.Job) > 40 || src.Index < 1 || src.Index > maxArtPicture {
					return fmt.Errorf("sources[%d] needs a job id and a picture number from 1 to %d", i, maxArtPicture)
				}
			default:
				return fmt.Errorf("sources[%d] needs a path, or a job and an index", i)
			}
		}
	case "generate":
		if strings.TrimSpace(a.Prompt) == "" || len(a.Prompt) > maxArtPrompt {
			return fmt.Errorf("prompt must be 1 to %d bytes of text", maxArtPrompt)
		}
		if len(a.Refs) > MaxArtRefs {
			return fmt.Errorf("refs holds at most %d references", MaxArtRefs)
		}
		for i, id := range a.Refs {
			if id == "" || len(id) > 40 {
				return fmt.Errorf("refs[%d] needs an id from action refs", i)
			}
		}
		if len(a.Quality) > 20 || len(a.Size) > 20 || len(a.Background) > 20 {
			return errors.New("quality, size and background are short names; see the tool schema")
		}
		if a.Count < 0 || a.Count > 4 {
			return errors.New("count must be 1 to 4")
		}
	case "wait", "cancel":
		if a.Job == "" || len(a.Job) > 40 {
			return errors.New("job must name a job")
		}
		if a.WaitSeconds < 0 || a.WaitSeconds > artMaxWait {
			return fmt.Errorf("waitSeconds must be 1 to %d", artMaxWait)
		}
	}
	if a.Limit < 0 || a.Limit > maxArtJobs {
		return fmt.Errorf("limit must be 1 to %d", maxArtJobs)
	}
	return nil
}

// invokeArt runs the handler with a deadline and converts panics into tool errors.
func (s *Server) invokeArt(ctx context.Context, req ArtRequest) (out ImageResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, artTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			out = ImageResult{}
			err = errors.New("art handler failed")
		}
	}()
	return s.art(ctx, req)
}
