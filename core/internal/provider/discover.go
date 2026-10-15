package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	discoverTimeout  = 8 * time.Second
	discoverMaxBody  = 4 << 20 // 4 MiB
	discoverMaxPages = 5       // Anthropic pagination bound
	discoverSnippet  = 200     // bytes of an error body echoed back
	scrubMark        = "[redacted]"
)

// tokenCount decodes a JSON number as a token count. Anything else (null, string, bool) becomes 0
// ("unknown") instead of failing the whole listing.
type tokenCount int

func (t *tokenCount) UnmarshalJSON(b []byte) error {
	var f float64
	if json.Unmarshal(b, &f) == nil && f > 0 {
		*t = tokenCount(f)
	}
	return nil
}

// Discover lists the models an endpoint serves. kind is KindOpenAI or KindAnthropic. The request
// is bounded by 8s, refuses redirects to other hosts, and reads at most 4 MiB. apiKey is sent only
// as a request header; it is scrubbed from every error. Results are sorted by id and deduplicated.
func Discover(ctx context.Context, kind, baseURL, apiKey string) ([]Model, error) {
	if err := Validate([]Provider{{ID: "discover", Kind: kind, BaseURL: baseURL}}); err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/models")
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	client := &http.Client{Timeout: discoverTimeout, CheckRedirect: sameHostRedirect}

	var models []Model
	var err error
	if kind == KindAnthropic {
		models, err = discoverAnthropic(ctx, client, base, apiKey)
	} else {
		models, err = discoverOpenAI(ctx, client, base, apiKey)
	}
	if err != nil {
		return nil, errors.New(scrub(err.Error(), apiKey))
	}
	return sortDedupe(models), nil
}

// sameHostRedirect follows redirects only within the original host, so the key header is never
// sent to another origin.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("redirect to another host refused (%s)", req.URL.Host)
	}
	return nil
}

func discoverOpenAI(ctx context.Context, client *http.Client, base, apiKey string) ([]Model, error) {
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	body, err := getBody(ctx, client, base+"/models", headers, apiKey)
	if err != nil {
		return nil, err
	}
	var page struct {
		Data []struct {
			ID               string     `json:"id"`
			ContextLength    tokenCount `json:"context_length"`
			MaxModelLen      tokenCount `json:"max_model_len"`
			ContextWindow    tokenCount `json:"context_window"`
			MaxContextLength tokenCount `json:"max_context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("bad JSON from models endpoint: %v", err)
	}
	var out []Model
	for _, m := range page.Data {
		if !validDiscoveredID(m.ID) {
			continue
		}
		ctxTokens := m.ContextLength
		for _, alt := range []tokenCount{m.MaxModelLen, m.ContextWindow, m.MaxContextLength} {
			if ctxTokens == 0 {
				ctxTokens = alt
			}
		}
		out = append(out, Model{ID: m.ID, Context: int(ctxTokens)})
	}
	return out, nil
}

// anthropicEffortLevels is the fixed ordering of effort levels as the API advertises them.
var anthropicEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

type levelCap struct {
	Supported bool `json:"supported"`
}

type anthropicModel struct {
	ID             string     `json:"id"`
	DisplayName    string     `json:"display_name"`
	MaxInputTokens tokenCount `json:"max_input_tokens"`
	MaxTokens      tokenCount `json:"max_tokens"`
	Capabilities   struct {
		Effort struct {
			Supported bool     `json:"supported"`
			Low       levelCap `json:"low"`
			Medium    levelCap `json:"medium"`
			High      levelCap `json:"high"`
			Xhigh     levelCap `json:"xhigh"`
			Max       levelCap `json:"max"`
		} `json:"effort"`
		Thinking levelCap `json:"thinking"`
	} `json:"capabilities"`
}

func discoverAnthropic(ctx context.Context, client *http.Client, base, apiKey string) ([]Model, error) {
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	if apiKey != "" {
		headers["x-api-key"] = apiKey
	}
	var out []Model
	after := ""
	for page := 0; page < discoverMaxPages; page++ {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		body, err := getBody(ctx, client, base+"/models?"+q.Encode(), headers, apiKey)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Data    []anthropicModel `json:"data"`
			HasMore bool             `json:"has_more"`
			LastID  string           `json:"last_id"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("bad JSON from models endpoint: %v", err)
		}
		for _, m := range resp.Data {
			if !validDiscoveredID(m.ID) {
				continue
			}
			out = append(out, anthropicToModel(m))
		}
		if !resp.HasMore || resp.LastID == "" {
			break
		}
		after = resp.LastID
	}
	return out, nil
}

func anthropicToModel(m anthropicModel) Model {
	e := m.Capabilities.Effort
	var efforts []string
	if e.Supported {
		levels := map[string]levelCap{"low": e.Low, "medium": e.Medium, "high": e.High, "xhigh": e.Xhigh, "max": e.Max}
		for _, name := range anthropicEffortLevels {
			if levels[name].Supported {
				efforts = append(efforts, name)
			}
		}
	}
	return Model{
		ID:        m.ID,
		Name:      m.DisplayName,
		Context:   int(m.MaxInputTokens),
		Output:    int(m.MaxTokens),
		Reasoning: m.Capabilities.Thinking.Supported,
		Efforts:   efforts,
	}
}

// validDiscoveredID keeps only ids Validate-compatible with the generated config.
func validDiscoveredID(id string) bool {
	return modelRe.MatchString(id)
}

func sortDedupe(models []Model) []Model {
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	out := models[:0]
	for i, m := range models {
		if i > 0 && m.ID == out[len(out)-1].ID {
			continue
		}
		out = append(out, m)
	}
	return out
}

// getBody performs one bounded GET and returns the body of a 2xx response.
func getBody(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, apiKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid models URL: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request failed: %v", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, discoverMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("models response read failed: %v", err)
	}
	if len(raw) > discoverMaxBody {
		return nil, fmt.Errorf("models response exceeds %d MiB", discoverMaxBody>>20)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet := strings.Join(strings.Fields(scrub(string(raw), apiKey)), " ")
		if len(snippet) > discoverSnippet {
			snippet = strings.ToValidUTF8(snippet[:discoverSnippet], "") + "..."
		}
		return nil, fmt.Errorf("models request failed: HTTP %d %s", res.StatusCode, snippet)
	}
	return raw, nil
}

// scrub removes the API key from text. An empty key is a no-op.
func scrub(text, apiKey string) string {
	if apiKey == "" {
		return text
	}
	return strings.ReplaceAll(text, apiKey, scrubMark)
}
