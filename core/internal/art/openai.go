package art

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Endpoint is an OpenAI-compatible API: a base URL that ends where the paths start (https://api.openai.com/v1) and the
// key for it. It prints without the key, so it can sit in a log line or an error by accident.
type Endpoint struct {
	BaseURL string
	Key     string
}

func (e Endpoint) String() string   { return e.BaseURL + " (key hidden)" }
func (e Endpoint) GoString() string { return e.String() }

const (
	// requestTimeout bounds one image call. High quality pictures take a minute or two.
	requestTimeout = 6 * time.Minute
	// maxResponseBytes bounds what is read back: four 4K pictures as base64 are well inside it.
	maxResponseBytes = 96 << 20
	// maxErrorBytes bounds the part of an error body that is kept.
	maxErrorBytes = 4 << 10
)

// imageCall is one request to the image API. With Refs it is an edit, which is how the API takes reference pictures;
// without, a plain generation.
type imageCall struct {
	Model      string
	Prompt     string
	Size       string
	Quality    string
	Background string
	N          int
	Refs       [][]byte // PNG data, in the order the prompt may refer to them
}

// imageReply is what came back.
type imageReply struct {
	Images [][]byte
	Usage  Usage
	// HasUsage is false when the endpoint reported no token counts, so the cost can only be estimated.
	HasUsage bool
}

// CallError is a failed image call. Billed is false when the endpoint answered with an error, which it does not charge
// for, and true when the answer never arrived (the connection broke or timed out), because the picture may have been
// made and billed anyway.
type CallError struct {
	Status  int
	Message string
	Billed  bool
}

func (e *CallError) Error() string { return e.Message }

// secretLike matches the shape of an API key, which some endpoints echo back in an error ("Incorrect API key provided:
// sk-abc***").
var secretLike = regexp.MustCompile(`(?i)\b(?:sk|key|rk)-[A-Za-z0-9_\-*]{6,}`)

// scrub removes the key, and anything shaped like one, from text that may be shown or logged.
func scrub(text, key string) string {
	if key != "" {
		text = strings.ReplaceAll(text, key, "***")
	}
	return secretLike.ReplaceAllString(text, "***")
}

func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("redirect to another host refused (%s)", req.URL.Host)
	}
	return nil
}

// newClient returns the client image calls use. The key is only ever sent to the host it was configured for.
func newClient() *http.Client {
	return &http.Client{Timeout: requestTimeout, CheckRedirect: sameHostRedirect}
}

// callImages sends the request and decodes the pictures.
func callImages(ctx context.Context, client *http.Client, ep Endpoint, c imageCall) (imageReply, error) {
	base := strings.TrimRight(strings.TrimSpace(ep.BaseURL), "/")
	if base == "" {
		return imageReply{}, &CallError{Message: "the provider has no base URL"}
	}
	var (
		body        []byte
		contentType string
		path        string
		err         error
	)
	if len(c.Refs) == 0 {
		path = "/images/generations"
		body, contentType, err = generationBody(c)
	} else {
		path = "/images/edits"
		body, contentType, err = editBody(c)
	}
	if err != nil {
		return imageReply{}, &CallError{Message: "build the request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return imageReply{}, &CallError{Message: "the provider's base URL is not valid"}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if ep.Key != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Key)
	}
	resp, err := client.Do(req)
	if err != nil {
		// A transport error carries the URL and no key, but scrub anyway.
		return imageReply{}, &CallError{Message: scrub(transportMessage(err), ep.Key), Billed: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return imageReply{}, &CallError{Status: resp.StatusCode, Message: "the answer was cut off: " + scrub(err.Error(), ep.Key), Billed: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return imageReply{}, statusError(resp.StatusCode, path, raw, ep.Key)
	}
	return decodeReply(raw, ep.Key)
}

func generationBody(c imageCall) ([]byte, string, error) {
	m := map[string]any{"model": c.Model, "prompt": c.Prompt, "n": max(1, c.N), "output_format": "png"}
	if c.Size != "" && c.Size != "auto" {
		m["size"] = c.Size
	}
	if c.Quality != "" && c.Quality != "auto" {
		m["quality"] = c.Quality
	}
	if c.Background != "" && c.Background != "auto" {
		m["background"] = c.Background
	}
	b, err := json.Marshal(m)
	return b, "application/json", err
}

func editBody(c imageCall) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := [][2]string{{"model", c.Model}, {"prompt", c.Prompt}, {"n", strconv.Itoa(max(1, c.N))}, {"output_format", "png"}}
	if c.Size != "" && c.Size != "auto" {
		fields = append(fields, [2]string{"size", c.Size})
	}
	if c.Quality != "" && c.Quality != "auto" {
		fields = append(fields, [2]string{"quality", c.Quality})
	}
	if c.Background != "" && c.Background != "auto" {
		fields = append(fields, [2]string{"background", c.Background})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	for i, data := range c.Refs {
		// The API refuses a file part whose type is not an image, which is what CreateFormFile would label it.
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="%d.png"`, i+1))
		h.Set("Content-Type", "image/png")
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

func transportMessage(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the image call timed out before an answer came back"
	case errors.Is(err, context.Canceled):
		return "the image call was cancelled"
	}
	return "the image call failed: " + err.Error()
}

// statusError turns an HTTP error into a message an agent can act on. The endpoint's own wording is kept, scrubbed.
func statusError(status int, path string, raw []byte, key string) *CallError {
	if len(raw) > maxErrorBytes {
		raw = raw[:maxErrorBytes]
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	detail := ""
	if json.Unmarshal(raw, &env) == nil {
		detail = env.Error.Message
		if detail == "" {
			detail = env.Message
		}
	}
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	detail = oneLine(scrub(detail, key))
	var msg string
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg = fmt.Sprintf("the provider rejected the API key (HTTP %d). The user has to fix the key in Relo's provider settings: %s", status, detail)
	case http.StatusNotFound:
		msg = fmt.Sprintf("this provider has no image API at %s (HTTP 404), so it cannot generate pictures: %s", path, detail)
	case http.StatusTooManyRequests:
		msg = fmt.Sprintf("the provider is rate limiting or out of credit (HTTP 429): %s", detail)
	default:
		msg = fmt.Sprintf("the provider refused the request (HTTP %d): %s", status, detail)
	}
	return &CallError{Status: status, Message: msg}
}

type replyBody struct {
	Data []struct {
		B64 string `json:"b64_json"`
		URL string `json:"url"`
	} `json:"data"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		Details      struct {
			Text  int `json:"text_tokens"`
			Image int `json:"image_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

func decodeReply(raw []byte, key string) (imageReply, error) {
	var body replyBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return imageReply{}, &CallError{Message: "the provider's answer was not JSON: " + scrub(oneLine(string(raw[:min(len(raw), 120)])), key), Billed: true}
	}
	var out imageReply
	for i, d := range body.Data {
		if d.B64 == "" {
			if d.URL != "" {
				return imageReply{}, &CallError{Message: "the provider returned links instead of picture data, which Relo does not fetch. Use a provider that returns base64 images", Billed: true}
			}
			return imageReply{}, &CallError{Message: fmt.Sprintf("picture %d in the answer was empty", i+1), Billed: true}
		}
		img, err := base64.StdEncoding.DecodeString(d.B64)
		if err != nil {
			return imageReply{}, &CallError{Message: fmt.Sprintf("picture %d in the answer was not valid base64", i+1), Billed: true}
		}
		out.Images = append(out.Images, img)
	}
	if len(out.Images) == 0 {
		return imageReply{}, &CallError{Message: "the provider answered without any picture", Billed: true}
	}
	if u := body.Usage; u != nil && (u.InputTokens > 0 || u.OutputTokens > 0) {
		out.HasUsage = true
		out.Usage = Usage{TextIn: u.Details.Text, ImageIn: u.Details.Image, ImageOut: u.OutputTokens}
		if u.Details.Text+u.Details.Image == 0 {
			// No split: price all of it as image input, the dearer rate, so the figure is never low.
			out.Usage.ImageIn = u.InputTokens
		}
	}
	return out, nil
}
