package art

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoint is an OpenAI-compatible API: a base URL that ends where the paths start (https://api.openai.com/v1) and the
// key for it. It prints without the key, so it can sit in a log line or an error by accident.
type Endpoint struct {
	BaseURL  string
	Key      string
	Provider string
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

// imageCall describes an image request, including reference pictures when present.
type imageCall struct {
	Model       string
	Prompt      string
	Size        string
	Quality     string
	Background  string
	AspectRatio string
	Resolution  string
	Seed        *int64
	N           int
	Refs        [][]byte // PNG data, in the order the prompt may refer to them
	// defaults identifies settings inherited rather than explicitly requested by the agent.
	defaults  map[string]bool
	recovered bool // older queued jobs do not retain which values were explicitly requested
}

// imageReply is what came back.
type imageReply struct {
	Images [][]byte
	Usage  Usage
	// HasUsage is false when the endpoint reported no token counts, so the cost can only be estimated.
	HasUsage bool
	// CostUSD and HasCost carry usage.cost in dollars when the endpoint reported it.
	CostUSD float64
	HasCost bool
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

// dataURLLike matches a base64 picture data URL. It carries picture bytes, so it must not reach an error or a log.
var dataURLLike = regexp.MustCompile(`data:[A-Za-z0-9.+\-]+/[A-Za-z0-9.+\-]+;base64,[A-Za-z0-9+/=]*`)

// scrub removes the key, anything shaped like one, and any base64 picture data URL from text that may be shown or logged.
func scrub(text, key string) string {
	if key != "" {
		text = strings.ReplaceAll(text, key, "***")
	}
	text = dataURLLike.ReplaceAllString(text, "data:***")
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
	if isOpenRouter(ep) {
		caps, listed, err := checkModelSupport(ctx, client, ep, c)
		if err != nil {
			return imageReply{}, err
		}
		if listed {
			dropInherited(&c, caps)
		}
		// n is sent, with the whole count in one call, only when the listed n range holds the count.
		// Otherwise each picture is its own call, each billed, each on its own deadline.
		count := max(1, c.N)
		calls, perCall, n := count, 1, 0
		if rangeAccepts(caps["n"], count) {
			calls, perCall, n = 1, count, count
		}
		var (
			result  imageReply
			total   float64
			allCost = true
		)
		for i := 0; i < calls; i++ {
			callCtx, cancel := context.WithTimeout(ctx, requestTimeout*time.Duration(perCall))
			reply, err := callImageOnce(callCtx, client, ep, c, true, caps, n)
			cancel()
			if err != nil {
				// The model list may have been fetched with a key the provider now refuses, so do not keep trusting it.
				if ce, ok := err.(*CallError); ok && (ce.Status == http.StatusUnauthorized || ce.Status == http.StatusForbidden) {
					clearModelListing(ep.BaseURL)
				}
				// Earlier successful calls may have incurred charges even if this one was refused.
				if ce, ok := err.(*CallError); ok && i > 0 {
					ce.Billed = true
				}
				return imageReply{}, err
			}
			if len(reply.Images) != perCall {
				return imageReply{}, &CallError{Message: fmt.Sprintf("OpenRouter returned an unexpected number of pictures for one request (asked for %d, got %d). Stop and report this to the user", perCall, len(reply.Images)), Billed: true}
			}
			result.Images = append(result.Images, reply.Images...)
			if reply.HasCost {
				total += reply.CostUSD
			} else {
				allCost = false
			}
		}
		// The job takes the reported cost only when every picture reported one; otherwise it keeps its estimate.
		result.CostUSD, result.HasCost = total, allCost
		return result, nil
	}
	if c.AspectRatio != "" || c.Resolution != "" || c.Seed != nil {
		return imageReply{}, &CallError{Message: "aspect_ratio, resolution and seed are only supported for OpenRouter"}
	}
	return callImageOnce(ctx, client, ep, c, false, nil, 0)
}

const (
	// modelListingTTL is how long a fetched OpenRouter model list is reused.
	modelListingTTL = 10 * time.Minute
	// preflightTimeout bounds the model list request, which is only a check.
	preflightTimeout = 15 * time.Second
	// maxModelListBytes bounds the model list read back.
	maxModelListBytes = 8 << 20
)

// listings caches the OpenRouter model list per base URL: model id -> supported_parameters.
var listings = struct {
	sync.Mutex
	byBase map[string]cachedListing
}{byBase: map[string]cachedListing{}}

type cachedListing struct {
	at     time.Time
	params map[string]modelCapabilities
}

// modelCapabilities is one listed model's supported_parameters: parameter name -> live descriptor.
type modelCapabilities map[string]paramDescriptor

// paramDescriptor is one live typed descriptor: {"type":"enum","values":[...]}, {"type":"range","min":0,"max":4} or
// {"type":"boolean"}. A descriptor of any other shape keeps only its name: the parameter counts as listed, with no limits.
type paramDescriptor struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
	Min    *int     `json:"min"`
	Max    *int     `json:"max"`
}

// hasParam reports whether the model lists the parameter, whatever its descriptor.
func hasParam(caps modelCapabilities, name string) bool {
	_, ok := caps[name]
	return ok
}

// rangeAccepts reports whether a range descriptor holds n. Other types accept nothing.
func rangeAccepts(d paramDescriptor, n int) bool {
	return d.Type == "range" && (d.Min == nil || n >= *d.Min) && (d.Max == nil || n <= *d.Max)
}

// rangeText describes a range descriptor for an error message.
func rangeText(d paramDescriptor) string {
	lo, hi := "any", "any"
	if d.Min != nil {
		lo = strconv.Itoa(*d.Min)
	}
	if d.Max != nil {
		hi = strconv.Itoa(*d.Max)
	}
	return lo + " to " + hi
}

// valueAccepted reports whether an enum descriptor accepts v. Without a value list there is nothing to refuse.
func valueAccepted(d paramDescriptor, v string) bool {
	return d.Type != "enum" || len(d.Values) == 0 || contains(d.Values, v)
}

// paramNames lists a model's parameter names in order, for status.
func paramNames(caps modelCapabilities) []string {
	names := make([]string, 0, len(caps))
	for name := range caps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// inheritable are the settings a job can inherit from the manager's defaults rather than the agent's request.
var inheritable = []string{"size", "quality", "background"}

// sentValue is the value Relo would send for a parameter, or "" when nothing is sent.
func sentValue(c imageCall, name string) string {
	var v string
	switch name {
	case "size":
		v = c.Size
	case "quality":
		v = c.Quality
	case "background":
		v = c.Background
	case "aspect_ratio":
		v = c.AspectRatio
	case "resolution":
		v = c.Resolution
	}
	if v == "auto" && (name == "size" || name == "quality" || name == "background") {
		return ""
	}
	return v
}

// checkModelSupport checks listed capabilities before any OpenRouter POST. Failed listings and unlisted models proceed.
func checkModelSupport(ctx context.Context, client *http.Client, ep Endpoint, c imageCall) (modelCapabilities, bool, error) {
	caps, listed := modelParams(ctx, client, ep, c.Model)
	if !listed {
		return nil, false, nil
	}
	for _, p := range []struct {
		name     string
		required bool
	}{
		{"input_references", len(c.Refs) > 0},
		{"size", !c.recovered && c.Size != "" && c.Size != "auto" && !c.defaults["size"]},
		{"quality", !c.recovered && c.Quality != "" && c.Quality != "auto" && !c.defaults["quality"]},
		{"background", !c.recovered && c.Background != "" && c.Background != "auto" && !c.defaults["background"]},
		{"aspect_ratio", c.AspectRatio != ""},
		{"resolution", c.Resolution != ""},
		{"seed", c.Seed != nil},
	} {
		if p.required && !hasParam(caps, p.name) {
			return nil, true, &CallError{Message: fmt.Sprintf("OpenRouter model does not support requested parameter %s. No picture request was sent. Stop and report this to the user", p.name)}
		}
	}
	if len(c.Refs) > 0 && !rangeAccepts(caps["input_references"], len(c.Refs)) && caps["input_references"].Type == "range" {
		return nil, true, &CallError{Message: fmt.Sprintf("OpenRouter model accepts %s input_references, but this job has %d. No picture request was sent. Stop and report this to the user", rangeText(caps["input_references"]), len(c.Refs))}
	}
	for _, name := range []string{"size", "quality", "background", "aspect_ratio", "resolution"} {
		// Inherited settings are dropped by dropInherited, not refused; recovered jobs cannot tell them apart.
		inherited := c.defaults[name] || (c.recovered && contains(inheritable, name))
		v := sentValue(c, name)
		if v == "" || inherited || valueAccepted(caps[name], v) {
			continue
		}
		return nil, true, &CallError{Message: fmt.Sprintf("OpenRouter model does not accept the requested %s. Accepted values: %s. No picture request was sent. Stop and report this to the user", name, strings.Join(caps[name].Values, ", "))}
	}
	return caps, true, nil
}

// dropInherited clears inherited settings the listed model does not accept, so no unsupported default is sent.
func dropInherited(c *imageCall, caps modelCapabilities) {
	for _, p := range []struct {
		name  string
		value *string
	}{{"size", &c.Size}, {"quality", &c.Quality}, {"background", &c.Background}} {
		if c.defaults[p.name] && (!hasParam(caps, p.name) || !valueAccepted(caps[p.name], *p.value)) {
			*p.value = ""
		}
	}
}

// modelParams shares the cached listing between status and preflight. It reports false for a failed listing or an unlisted model.
func modelParams(ctx context.Context, client *http.Client, ep Endpoint, model string) (modelCapabilities, bool) {
	base := strings.TrimRight(strings.TrimSpace(ep.BaseURL), "/")
	params, ok := cachedModelParams(base)
	if !ok {
		fetched, err := fetchModelParams(ctx, client, ep, base)
		if err != nil {
			return nil, false
		}
		params = fetched
		listings.Lock()
		listings.byBase[base] = cachedListing{at: time.Now(), params: fetched}
		listings.Unlock()
	}
	caps, listed := params[model]
	return caps, listed
}

// clearModelListing drops the cached model list for base, so the next job asks the provider again.
func clearModelListing(base string) {
	listings.Lock()
	delete(listings.byBase, strings.TrimRight(strings.TrimSpace(base), "/"))
	listings.Unlock()
}

// cachedModelParams returns the cached model list for base while it is fresh.
func cachedModelParams(base string) (map[string]modelCapabilities, bool) {
	listings.Lock()
	defer listings.Unlock()
	c, ok := listings.byBase[base]
	if !ok || time.Since(c.at) > modelListingTTL {
		return nil, false
	}
	return c.params, true
}

// fetchModelParams reads GET {base}/images/models into model id -> supported_parameters. A model whose entry does not
// decode is left out, and the rest of the listing still counts.
func fetchModelParams(ctx context.Context, client *http.Client, ep Endpoint, base string) (map[string]modelCapabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/images/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if ep.Key != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("model list answered HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes))
	if err != nil {
		return nil, err
	}
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	params := make(map[string]modelCapabilities, len(body.Data))
	for _, item := range body.Data {
		var m struct {
			ID                  string                     `json:"id"`
			SupportedParameters map[string]json.RawMessage `json:"supported_parameters"`
		}
		// Only this model is skipped when its entry is malformed, e.g. supported_parameters is not an object.
		if json.Unmarshal(item, &m) != nil || m.ID == "" || m.SupportedParameters == nil {
			continue
		}
		caps := make(modelCapabilities, len(m.SupportedParameters))
		for name, desc := range m.SupportedParameters {
			var d paramDescriptor
			if json.Unmarshal(desc, &d) != nil {
				d = paramDescriptor{}
			}
			caps[name] = d
		}
		params[m.ID] = caps
	}
	return params, nil
}

func isOpenRouter(ep Endpoint) bool {
	if strings.EqualFold(strings.TrimSpace(ep.Provider), "openrouter") {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(ep.BaseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

// callImageOnce makes one request; OpenRouter uses POST /images for both request types.
func callImageOnce(ctx context.Context, client *http.Client, ep Endpoint, c imageCall, openRouter bool, caps modelCapabilities, n int) (imageReply, error) {
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
	if openRouter {
		path = "/images"
		body, contentType, err = referenceBody(c, caps, n)
	} else if len(c.Refs) == 0 {
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
		if openRouter && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return imageReply{}, &CallError{Message: "the OpenRouter image call failed before an answer came back. Stop and report this to the user", Billed: true}
		}
		// A transport error carries the URL and no key, but scrub anyway.
		message := scrub(transportMessage(err), ep.Key)
		if openRouter {
			message += ". Stop and report this to the user"
		}
		return imageReply{}, &CallError{Message: message, Billed: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if openRouter {
			msg := "the OpenRouter answer was cut off. Stop and report this to the user"
			if errors.Is(err, context.DeadlineExceeded) {
				msg = "the OpenRouter image call timed out while reading the answer. Stop and report this to the user"
			}
			return imageReply{}, &CallError{Status: resp.StatusCode, Message: msg, Billed: true}
		}
		return imageReply{}, &CallError{Status: resp.StatusCode, Message: "the answer was cut off: " + scrub(err.Error(), ep.Key), Billed: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ce := statusError(resp.StatusCode, path, raw, ep.Key)
		if openRouter {
			ce = openRouterStatusError(resp.StatusCode, raw, ep.Key)
			if len(c.Refs) == 0 {
				ce.Message = strings.ReplaceAll(ce.Message, " The provider reported that input_references is unsupported.", "")
			}
			if resp.StatusCode == http.StatusBadRequest {
				detail := strings.ToLower(openRouterDetail(raw, ep.Key))
				for _, p := range []struct {
					name string
					sent bool
				}{
					{"aspect_ratio", c.AspectRatio != ""}, {"resolution", c.Resolution != ""}, {"seed", c.Seed != nil},
					{"size", c.Size != "" && c.Size != "auto"}, {"quality", c.Quality != "" && c.Quality != "auto"},
					{"background", c.Background != "" && c.Background != "auto"}, {"input_references", len(c.Refs) > 0},
					{"n", n > 0}, {"output_format", hasParam(caps, "output_format")},
				} {
					if p.sent && regexp.MustCompile(`\b`+p.name+`\b`).MatchString(detail) {
						ce.Message += " Provider mentioned requested parameter " + p.name + "."
					}
				}
			}
		}
		if !openRouter && len(c.Refs) > 0 && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnsupportedMediaType || resp.StatusCode == http.StatusNotImplemented || ((resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) && (strings.Contains(strings.ToLower(ce.Message), "not supported") || strings.Contains(strings.ToLower(ce.Message), "unsupported")))) {
			ce.Message = "reference images are not supported by the provider: " + ce.Message
		}
		return imageReply{}, ce
	}
	if openRouter {
		return decodeRefReply(raw, resp.StatusCode, ep.Key)
	}
	return decodeReply(raw, ep.Key)
}

// referenceBody is the OpenRouter POST /images request: the prompt and every reference as an input_references
// image_url object holding a PNG data URL, in order. Each picture is one call.
// n is sent only when non-zero; the caller sets it when the listed n range holds the job's count.
func referenceBody(c imageCall, caps modelCapabilities, n int) ([]byte, string, error) {
	refs := make([]map[string]any, 0, len(c.Refs))
	for _, ref := range c.Refs {
		refs = append(refs, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(ref)},
		})
	}
	m := map[string]any{"model": c.Model, "prompt": c.Prompt}
	if len(refs) > 0 {
		m["input_references"] = refs
	}
	if n > 0 {
		m["n"] = n
	}
	// Relo asks for PNG only where the listing accepts it; a listing that offers only svg gets the provider default.
	if d, ok := caps["output_format"]; ok && valueAccepted(d, "png") {
		m["output_format"] = "png"
	}
	if c.Size != "" && c.Size != "auto" {
		m["size"] = c.Size
	}
	if c.Quality != "" && c.Quality != "auto" {
		m["quality"] = c.Quality
	}
	if c.Background != "" && c.Background != "auto" {
		m["background"] = c.Background
	}
	if c.AspectRatio != "" {
		m["aspect_ratio"] = c.AspectRatio
	}
	if c.Resolution != "" {
		m["resolution"] = c.Resolution
	}
	if c.Seed != nil {
		m["seed"] = *c.Seed
	}
	b, err := json.Marshal(m)
	return b, "application/json", err
}

// decodeRefReply reads an OpenRouter answer. Each picture must be base64 or a base64 image data URL.
func decodeRefReply(raw []byte, status int, keys ...string) (imageReply, error) {
	var body replyBody
	unsupported := func(detail string) (imageReply, error) {
		return imageReply{}, &CallError{Status: status, Message: "OpenRouter response is unusable: " + detail + ". Stop and report this to the user", Billed: true}
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return unsupported(fmt.Sprintf("the answer was not JSON (HTTP %d)", status))
	}
	var out imageReply
	for i, d := range body.Data {
		if d.MediaType != "" && d.MediaType != "image/png" && d.MediaType != "image/jpeg" && d.MediaType != "image/webp" {
			// Only echo a syntactically safe media type, never arbitrary provider data.
			mediaType := "invalid media type"
			key := ""
			if len(keys) > 0 {
				key = keys[0]
			}
			// Long uninterrupted tokens can be encoded payloads, not safe diagnostics.
			if regexp.MustCompile(`^[A-Za-z0-9.+-]{1,64}/[A-Za-z0-9.+-]{1,64}$`).MatchString(d.MediaType) && !regexp.MustCompile(`[A-Za-z0-9]{16}`).MatchString(d.MediaType) && scrub(d.MediaType, key) == d.MediaType {
				mediaType = d.MediaType
			}
			return unsupported("unsupported media_type " + mediaType)
		}
		var (
			img []byte
			err error
		)
		switch {
		case d.B64 != "":
			img, err = base64.StdEncoding.DecodeString(d.B64)
		case strings.HasPrefix(d.URL, "data:"):
			img, err = decodeImageDataURL(d.URL)
		case d.URL != "":
			return unsupported(fmt.Sprintf("picture %d is a link, which Relo does not fetch", i+1))
		default:
			return unsupported(fmt.Sprintf("picture %d in the answer is empty", i+1))
		}
		if err != nil || len(img) == 0 {
			return unsupported(fmt.Sprintf("picture %d in the answer is not valid picture data", i+1))
		}
		out.Images = append(out.Images, img)
	}
	if len(out.Images) == 0 {
		return unsupported(fmt.Sprintf("no picture was returned (HTTP %d)", status))
	}
	if body.Usage != nil {
		out.CostUSD, out.HasCost = reportedCost(body.Usage.Cost)
	}
	return out, nil
}

// reportedCost reads usage.cost, which OpenRouter reports in dollars. Anything else is ignored, so the job keeps its estimate.
func reportedCost(raw json.RawMessage) (float64, bool) {
	var v *float64
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil || *v < 0 {
		return 0, false
	}
	return *v, true
}

// decodeImageDataURL decodes a base64 image data URL such as data:image/png;base64,....
func decodeImageDataURL(u string) ([]byte, error) {
	meta, data, ok := strings.Cut(u, ",")
	if !ok || !strings.HasPrefix(meta, "data:") || !strings.HasSuffix(meta, ";base64") {
		return nil, errors.New("not a base64 data URL")
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64"))
	if err != nil || !strings.HasPrefix(mediaType, "image/") {
		return nil, errors.New("invalid media type")
	}
	return base64.StdEncoding.DecodeString(data)
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

// errorDetail is the endpoint's own wording from an error body, scrubbed and on one line.
func errorDetail(raw []byte, key string) string {
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
	return oneLine(scrub(detail, key))
}

// openRouterError is OpenRouter's error body. When an upstream model provider refuses a picture, OpenRouter answers with a
// generic message ("Provider returned error") and carries the provider's own error, as JSON text, in metadata.raw.
type openRouterError struct {
	Error struct {
		Message  string          `json:"message"`
		Code     json.RawMessage `json:"code"`
		Type     string          `json:"type"`
		Metadata struct {
			Raw      json.RawMessage `json:"raw"`
			Provider string          `json:"provider_name"`
		} `json:"metadata"`
	} `json:"error"`
}

// rawText is a JSON value as text: a string is unquoted, any other value is kept as written.
func rawText(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}

// openRouterDetail is the whole reason text of an OpenRouter error, upstream provider reason included. It is for matching
// against fixed phrases and is never shown as it stands.
func openRouterDetail(raw []byte, key string) string {
	detail := errorDetail(raw, key)
	if len(raw) > maxErrorBytes {
		raw = raw[:maxErrorBytes]
	}
	var env openRouterError
	if json.Unmarshal(raw, &env) == nil && len(env.Error.Metadata.Raw) > 0 {
		detail += " " + oneLine(scrub(rawText(env.Error.Metadata.Raw), key))
	}
	return detail
}

var (
	// safeCode is the shape of a provider error code Relo is willing to repeat: a short lowercase identifier.
	safeCode = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,47}$`)
	// safeProviderName is the shape of an upstream provider's name Relo is willing to repeat.
	safeProviderName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,39}$`)
)

// providerRefusal names why the upstream provider refused a request, in fixed words. Provider wording is never copied: it
// can carry secrets, picture bytes or retry advice. Only a class found by fixed phrases, a short identifier code and the
// provider's name are repeated. It returns "" when nothing is recognised, which keeps the plain status message.
func providerRefusal(raw []byte, key string) string {
	if len(raw) > maxErrorBytes {
		raw = raw[:maxErrorBytes]
	}
	var env openRouterError
	_ = json.Unmarshal(raw, &env)
	// The provider's own code sits inside the raw text when that is JSON, otherwise in the outer error.
	var inner openRouterError
	_ = json.Unmarshal([]byte(rawText(env.Error.Metadata.Raw)), &inner)
	code := ""
	for _, c := range []string{rawText(inner.Error.Code), inner.Error.Type, rawText(env.Error.Code), env.Error.Type} {
		if c = strings.TrimSpace(c); c != "null" && safeCode.MatchString(c) {
			code = c
			break
		}
	}
	text := strings.ToLower(openRouterDetail(raw, key) + " " + code)
	var out string
	switch {
	case containsAny(text, "moderation", "safety system", "safety_violations", "content policy", "content_policy", "flagged", "policy violation"):
		out = " The provider reported that its safety filter refused the prompt or a reference picture."
	case containsAny(text, "invalid image", "invalid_image", "image_parse", "unsupported image", "image file or mode"):
		out = " The provider reported that it could not read a reference picture."
	case containsAny(text, "string_above_max_length", "maximum length", "too long"):
		out = " The provider reported that the prompt is too long."
	}
	if code != "" {
		out += " Provider code: " + code + "."
	}
	if name := strings.TrimSpace(env.Error.Metadata.Provider); safeProviderName.MatchString(name) {
		out += " Provider: " + name + "."
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// openRouterStatusError explains a refused OpenRouter POST {base}/images. Each status has its own message that carries the
// status. Only a 502 is treated as billed: it can arrive after the upstream model has charged, which is a hypothesis
// about OpenRouter, not a documented rule.
func openRouterStatusError(status int, raw []byte, key string) *CallError {
	// Use a fixed diagnostic, not provider wording, which can contain secrets, image bytes or unsafe retry advice.
	diagnostic := ""
	detail := strings.ToLower(openRouterDetail(raw, key))
	if strings.Contains(detail, "input_references") && (strings.Contains(detail, "unsupported") || strings.Contains(detail, "not supported")) {
		diagnostic = " The provider reported that input_references is unsupported."
	}
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		diagnostic += providerRefusal(raw, key)
	}
	var msg string
	switch status {
	case http.StatusBadRequest:
		msg = "OpenRouter rejected the request as invalid (HTTP 400)"
	case http.StatusUnauthorized, http.StatusForbidden:
		msg = fmt.Sprintf("OpenRouter rejected the API key (HTTP %d). The user has to fix the key in Relo's provider settings", status)
	case http.StatusPaymentRequired:
		msg = "OpenRouter has no credit for this picture (HTTP 402). The user has to add credit at OpenRouter"
	case http.StatusNotFound:
		msg = "OpenRouter has no image route for this request (HTTP 404)"
	case http.StatusTooManyRequests:
		msg = "OpenRouter rate limited the request (HTTP 429)"
	case http.StatusBadGateway:
		msg = "OpenRouter could not get a picture from the model provider (HTTP 502)"
	default:
		msg = fmt.Sprintf("OpenRouter refused the request (HTTP %d)", status)
	}
	return &CallError{Status: status, Message: msg + "." + diagnostic + " Stop and report this to the user", Billed: status == http.StatusBadGateway}
}

// statusError turns an HTTP error into a message an agent can act on. The endpoint's own wording is kept, scrubbed.
func statusError(status int, path string, raw []byte, key string) *CallError {
	detail := errorDetail(raw, key)
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
		B64       string `json:"b64_json"`
		URL       string `json:"url"`
		MediaType string `json:"media_type"`
	} `json:"data"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		Details      struct {
			Text  int `json:"text_tokens"`
			Image int `json:"image_tokens"`
		} `json:"input_tokens_details"`
		// Cost is kept raw so a cost of another JSON type cannot make the whole answer unreadable.
		Cost json.RawMessage `json:"cost"`
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
