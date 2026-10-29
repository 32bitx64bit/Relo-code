package agentmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
	Prompt     string    `json:"prompt,omitempty"`
	Refs       []ArtRef  `json:"refs,omitempty"`
	Quality    string    `json:"quality,omitempty"`
	Size       string    `json:"size,omitempty"`
	Background string    `json:"background,omitempty"`
	Count      int       `json:"count,omitempty"`
	Params     ArtParams `json:"params,omitempty"`
	// Job names the job to wait for or cancel.
	Job string `json:"job,omitempty"`
	// WaitSeconds is how long wait may block, up to artMaxWait.
	WaitSeconds int `json:"waitSeconds,omitempty"`
	// Limit bounds the jobs listed.
	Limit int `json:"limit,omitempty"`
}

// ArtParams contains optional OpenRouter image parameters.
type ArtParams struct {
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	Seed        *int64 `json:"seed,omitempty"`
}

// ArtSource is a picture to turn into a reference: a file in the workspace, or a picture of an earlier job.
type ArtSource struct {
	Path  string `json:"path,omitempty"`
	Job   string `json:"job,omitempty"`
	Index int    `json:"index,omitempty"`
}

// ArtRoles are the roles a reference can play in the prompt.
var ArtRoles = []string{"style", "subject", "composition"}

// ArtRef is a stored reference used by generate: the id from action refs, with an optional role and weight. It is
// written in JSON either as the bare id or as {id, role, weight}.
type ArtRef struct {
	ID     string  `json:"id"`
	Role   string  `json:"role,omitempty"`
	Weight float64 `json:"weight,omitempty"`
}

// errArtRefShape is the hint given to an agent whose refs entry is neither an id nor an object of ArtRef's fields.
var errArtRefShape = errors.New("refs entries are an id, or {id, role, weight}")

// UnmarshalJSON accepts a bare id string or an object with the same fields as ArtRef; unknown object fields fail.
func (r *ArtRef) UnmarshalJSON(b []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte(`"`)) {
		var id string
		if err := json.Unmarshal(b, &id); err != nil {
			return err
		}
		*r = ArtRef{ID: id}
		return nil
	}
	type plain ArtRef
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return errArtRefShape
	}
	*r = ArtRef(p)
	return nil
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
		"description": "Relo art: makes pictures with an image model through the user's own OpenAI or OpenRouter account; the user pays per picture. Use your own capability key. Every job costs real money, so look before you ask for more. " +
			"Start with action status (is it set up, the model, the budget left) and action playbook (the guide to prompts, references, transparency and sprites; topic narrows it). " +
			"action refs stores pictures to start from: sources are files in your workspace {path} or pictures of earlier jobs {job, index}; it returns an id for each. " +
			"action generate starts a job and returns at once: prompt describes the picture in plain text. Do not put parameter flags in it (--sref, --ar, --stylize, --chaos, --raw): they are refused, so the prompt is plain text only. " +
			"refs lists stored references, each an id or {id, role, weight}: role is style, subject (one only) or composition, weight 0 to 1. The order is image 1, image 2 and so on, and the prompt is told each role; with refs the model edits or restyles them. " +
			"quality, size, background and count override the user's defaults and change the price. OpenRouter params aspect_ratio, resolution and seed depend on model support; check status. Send the same prompt at most twice per session. " +
			"The user may be asked to approve it. action wait blocks up to waitSeconds (at most " + fmt.Sprint(artMaxWait) + ") for a job and returns a contact sheet of its numbered pictures when it is ready: call it again while the state is still queued or running. " +
			"A job that fails with references: stop and report it to the user; never retry it without the references. " +
			"cancel stops a job that has not finished. jobs lists your jobs and the money used. " +
			"Pictures land in a folder outside your workspace: feed the one you choose to relo_sprite with its path from the job (it is in the result). " +
			"When a call says a limit is reached, stop, use what you have, and tell the user.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":    map[string]any{"type": "string", "minLength": 1, "maxLength": 256, "description": "Per-agent capability key from your prompt"},
				"action": map[string]any{"type": "string", "enum": ArtActions},
				"topic":  str("playbook: overview (default), prompts, refs, sprites, views, game-dev uses, iterate, cost or jobs"),
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
					"type": "array", "maxItems": MaxArtRefs, "description": "generate: ids returned by action refs, or {id, role, weight}; in the order the prompt may call them image 1, image 2, ...",
					"items": map[string]any{"anyOf": []any{
						map[string]any{"type": "string", "minLength": 1, "maxLength": 40},
						map[string]any{
							"type": "object", "required": []string{"id"}, "additionalProperties": false,
							"properties": map[string]any{
								"id":     map[string]any{"type": "string", "minLength": 1, "maxLength": 40, "description": "A reference id from action refs"},
								"role":   map[string]any{"type": "string", "enum": ArtRoles, "description": "style: copy the look; subject: the thing to draw (one only); composition: the layout"},
								"weight": map[string]any{"type": "number", "minimum": 0, "maximum": 1, "description": "How strongly the role is followed, 0 to 1. Default: the provider's"},
							},
						},
					}},
				},
				"quality":    map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "auto"}, "description": "generate: price and detail. Explore at low, finish at medium or high. Default: the user's setting"},
				"size":       map[string]any{"type": "string", "enum": []string{"1024x1024", "1536x1024", "1024x1536", "auto"}, "description": "generate: picture size. Default: the user's setting"},
				"background": map[string]any{"type": "string", "enum": []string{"auto", "transparent", "opaque"}, "description": "generate: transparent asks for a real alpha channel when the model can; if the provider refuses it, ask for a flat key colour and use relo_sprite matte"},
				"count":      map[string]any{"type": "integer", "minimum": 1, "maximum": 4, "description": "generate: how many pictures to make, each one billed. Default: the user's setting"},
				"params": map[string]any{"type": "object", "additionalProperties": false, "description": "generate: optional OpenRouter parameters, subject to model support", "properties": map[string]any{
					"aspect_ratio": map[string]any{"type": "string", "pattern": "^[0-9]+:[0-9]+$", "description": "Aspect ratio as N:N"},
					"resolution":   map[string]any{"type": "string", "pattern": "^[A-Za-z0-9]{1,16}$", "description": "Model-dependent resolution"},
					"seed":         map[string]any{"type": "integer", "minimum": 0, "description": "Nonnegative seed"},
				}},
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
		if errors.Is(err, errArtRefShape) {
			return nil, invalidParams(err.Error())
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field == "params.seed" {
			return nil, invalidParams("seed must be a nonnegative integer")
		}
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

// artParamFlag matches parameter syntax that the prompt must not carry: "--sref", "--ar 16:9", "stylize: 100",
// "chaos 20". Prompts are plain text; references go in refs with a role.
var artParamFlag = regexp.MustCompile(`(?i)(^|\s)--[a-z]|\b(stylize|chaos|raw)\s*[:=]\s*\S|\b(stylize|chaos)\s+\d`)

// ArtPromptError refuses a generate prompt that carries parameter flags. Such a prompt would be sent as text and
// silently do nothing, so it is refused before anything is spent.
func ArtPromptError(prompt string) error {
	if m := artParamFlag.FindString(prompt); m != "" {
		if regexp.MustCompile(`(?i)(^|\s)--(?:ar|aspect)(?:\s|=|$)`).MatchString(prompt) {
			return errors.New("prompt contains an aspect flag; use params.aspect_ratio instead")
		}
		if regexp.MustCompile(`(?i)(^|\s)--seed(?:\s|=|$)`).MatchString(prompt) {
			return errors.New("prompt contains a seed flag; use params.seed instead")
		}
		return fmt.Errorf("prompt contains the parameter %q, which is not supported: the prompt is plain text. Describe the picture in words, and pass references in refs with a role", strings.TrimSpace(m))
	}
	return nil
}

func artInList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
		if a.Params.AspectRatio != "" && !regexp.MustCompile(`^[0-9]+:[0-9]+$`).MatchString(a.Params.AspectRatio) {
			return errors.New("aspect_ratio must be N:N using digits")
		}
		if a.Params.Resolution != "" && !regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`).MatchString(a.Params.Resolution) {
			return errors.New("resolution must be 1 to 16 ASCII letters or digits")
		}
		if a.Params.Seed != nil && *a.Params.Seed < 0 {
			return errors.New("seed must be a nonnegative integer")
		}
		if strings.TrimSpace(a.Prompt) == "" || len(a.Prompt) > maxArtPrompt {
			return fmt.Errorf("prompt must be 1 to %d bytes of text", maxArtPrompt)
		}
		if err := ArtPromptError(a.Prompt); err != nil {
			return err
		}
		if len(a.Refs) > MaxArtRefs {
			return fmt.Errorf("refs holds at most %d references", MaxArtRefs)
		}
		subjects := 0
		for i, r := range a.Refs {
			if r.ID == "" || len(r.ID) > 40 {
				return fmt.Errorf("refs[%d] needs an id from action refs", i)
			}
			if r.Role != "" && !artInList(ArtRoles, r.Role) {
				return fmt.Errorf("refs[%d].role %q must be one of %s", i, r.Role, strings.Join(ArtRoles, ", "))
			}
			if r.Role == "subject" {
				subjects++
			}
			if r.Weight < 0 || r.Weight > 1 {
				return fmt.Errorf("refs[%d].weight must be from 0 to 1; leave it out for the default", i)
			}
		}
		if subjects > 1 {
			return errors.New("refs holds one subject reference at most")
		}
		if composed, _ := ArtPromptWithRefs(a.Prompt, a.Refs); len(composed) > maxArtPrompt {
			return fmt.Errorf("prompt is %d bytes with its reference notes; the limit is %d bytes including them: shorten the prompt", len(composed), maxArtPrompt)
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

// artRoleText says each role in the prompt.
var artRoleText = map[string]string{"style": "style reference", "subject": "subject reference", "composition": "composition reference"}

// ArtPromptWithRefs states each reference's role and weight in the prompt, for example "image 1: style reference (weight
// 0.5)", and returns the reference ids in the same order. Validation measures this prompt, so the limit holds for what
// the image model is sent.
func ArtPromptWithRefs(prompt string, refs []ArtRef) (string, []string) {
	prompt = strings.TrimSpace(prompt)
	if len(refs) == 0 {
		return prompt, nil
	}
	ids := make([]string, 0, len(refs))
	notes := make([]string, 0, len(refs))
	for i, r := range refs {
		ids = append(ids, r.ID)
		note := fmt.Sprintf("image %d: ", i+1)
		if text, ok := artRoleText[r.Role]; ok {
			note += text
		} else {
			note += "reference"
		}
		if r.Weight > 0 {
			note += " (weight " + strconv.FormatFloat(r.Weight, 'f', -1, 64) + ")"
		}
		notes = append(notes, note)
	}
	return prompt + "\nReferences: " + strings.Join(notes, "; ") + ".", ids
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
