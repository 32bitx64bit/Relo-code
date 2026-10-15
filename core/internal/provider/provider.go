// Package provider models user-defined model providers (OpenAI- and Anthropic-compatible endpoints)
// and turns them into OpenCode configuration. API keys never live in a Provider: they travel in a
// separate map and reach OpenCode only through child-process environment variables.
package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const (
	KindOpenAI    = "openai"
	KindAnthropic = "anthropic"

	// What OpenCode assumes for a custom model that declares no limit; used to complete a half-declared one.
	defaultContext = 200000
	defaultOutput  = 32000

	pkgOpenAI    = "@ai-sdk/openai-compatible"
	pkgAnthropic = "@ai-sdk/anthropic"
)

// Model is one model a provider serves. Zero Context/Output mean "unknown".
// Efforts are the reasoning levels the endpoint advertises (empty = unknown/none).
type Model struct {
	ID        string   `json:"id"`
	Name      string   `json:"name,omitempty"`
	Context   int      `json:"context,omitempty"`
	Output    int      `json:"output,omitempty"`
	Reasoning bool     `json:"reasoning,omitempty"`
	Efforts   []string `json:"efforts,omitempty"`
}

type Provider struct {
	ID      string  `json:"id"`
	Name    string  `json:"name,omitempty"`
	Kind    string  `json:"kind"`
	BaseURL string  `json:"baseURL"`
	Models  []Model `json:"models"`
}

var (
	idRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
	modelRe = regexp.MustCompile(`^[^\s"\\]{1,200}$`)
	envRe   = regexp.MustCompile(`[^A-Z0-9]`)
)

// Validate checks structure only. It never contacts the network.
func Validate(ps []Provider) error {
	seen := map[string]bool{}
	for i, p := range ps {
		if !idRe.MatchString(p.ID) {
			return fmt.Errorf("provider %d: id %q must match %s", i+1, p.ID, idRe)
		}
		if seen[p.ID] {
			return fmt.Errorf("provider %q: duplicate id", p.ID)
		}
		seen[p.ID] = true
		if p.Kind != KindOpenAI && p.Kind != KindAnthropic {
			return fmt.Errorf("provider %q: kind must be %q or %q", p.ID, KindOpenAI, KindAnthropic)
		}
		u, err := url.Parse(strings.TrimSpace(p.BaseURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q: baseURL must be an http(s) URL", p.ID)
		}
		ids := map[string]bool{}
		for _, m := range p.Models {
			if !modelRe.MatchString(m.ID) {
				return fmt.Errorf("provider %q: invalid model id %q", p.ID, m.ID)
			}
			if ids[m.ID] {
				return fmt.Errorf("provider %q: duplicate model %q", p.ID, m.ID)
			}
			ids[m.ID] = true
			if m.Context < 0 || m.Output < 0 {
				return fmt.Errorf("provider %q model %q: negative limit", p.ID, m.ID)
			}
		}
	}
	return nil
}

// EnvName is the environment variable that carries the API key for provider id.
func EnvName(id string) string {
	return "RELO_KEY_" + envRe.ReplaceAllString(strings.ToUpper(id), "_")
}

// BuildConfig renders the OPENCODE_CONFIG_CONTENT JSON. An apiKey option referencing the provider's
// env var is emitted only when keys has a non-empty entry for that provider. Output is deterministic.
//
// Unless allowBuiltin is set, enabled_providers lists exactly the Relo providers, so OpenCode's own catalog
// (opencode, openai, anthropic, ...) is switched off and the only models offered are the ones the user added.
// An empty provider list still disables everything.
func BuildConfig(ps []Provider, keys map[string]string, allowBuiltin bool) (string, error) {
	if err := Validate(ps); err != nil {
		return "", err
	}
	providers := map[string]any{}
	for _, p := range ps {
		pkg := pkgOpenAI
		if p.Kind == KindAnthropic {
			pkg = pkgAnthropic
		}
		options := map[string]any{"baseURL": strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")}
		if keys[p.ID] != "" {
			options["apiKey"] = "{env:" + EnvName(p.ID) + "}"
		}
		models := map[string]any{}
		for _, m := range p.Models {
			entry := map[string]any{}
			if m.Name != "" {
				entry["name"] = m.Name
			}
			if m.Context > 0 || m.Output > 0 {
				// OpenCode drops the WHOLE provider ("skipped malformed recognized value") when a limit has
				// only one of context/output, so fill the missing half with OpenCode's own defaults for models
				// with no limit. Relo's view of the model (context/output shown and budgeted) comes from this
				// config, not from OpenCode's echo, so the filler never reaches the UI or the budget.
				limit := map[string]any{"context": defaultContext, "output": defaultOutput}
				if m.Context > 0 {
					limit["context"] = m.Context
				}
				if m.Output > 0 {
					limit["output"] = m.Output
				}
				entry["limit"] = limit
			}
			if m.Reasoning || len(m.Efforts) > 0 {
				entry["reasoning"] = true
			}
			models[m.ID] = entry
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		providers[p.ID] = map[string]any{"npm": pkg, "name": name, "options": options, "models": models}
	}
	cfg := map[string]any{}
	if len(providers) > 0 {
		cfg["provider"] = providers
	}
	if !allowBuiltin {
		ids := make([]string, 0, len(ps))
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		sort.Strings(ids)
		cfg["enabled_providers"] = ids
	}
	b, err := json.Marshal(cfg)
	return string(b), err
}

// Env returns the child-process environment additions: OPENCODE_CONFIG_CONTENT plus one
// RELO_KEY_<ID> per provider that has a key. The config is always present: even with no providers it
// carries enabled_providers=[] so nothing from OpenCode's built-in catalog leaks in.
func Env(ps []Provider, keys map[string]string, allowBuiltin bool) ([]string, error) {
	cfg, err := BuildConfig(ps, keys, allowBuiltin)
	if err != nil {
		return nil, err
	}
	env := []string{"OPENCODE_CONFIG_CONTENT=" + cfg}
	for _, p := range ps {
		if k := keys[p.ID]; k != "" {
			env = append(env, EnvName(p.ID)+"="+k)
		}
	}
	return env, nil
}

// Fingerprint changes whenever the provider set or any key changes. It is a hash, so it is safe to
// compare and log; it does not reveal key material.
func Fingerprint(ps []Provider, keys map[string]string) string {
	h := sha256.New()
	cfg, _ := json.Marshal(ps)
	h.Write(cfg)
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h.Write([]byte{0})
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write([]byte(keys[id]))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
