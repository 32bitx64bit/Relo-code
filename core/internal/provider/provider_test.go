package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func sample() []Provider {
	return []Provider{
		{ID: "acme", Name: "Acme", Kind: KindOpenAI, BaseURL: "https://api.acme.test/v1/", Models: []Model{{ID: "big", Name: "Big", Context: 200000, Output: 16000, Reasoning: true}, {ID: "plain"}}},
		{ID: "claude-x", Kind: KindAnthropic, BaseURL: "http://127.0.0.1:8080"},
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(sample()); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]Provider{
		"id":         {{ID: "Bad Id", Kind: KindOpenAI, BaseURL: "https://x.test"}},
		"dup":        {{ID: "a", Kind: KindOpenAI, BaseURL: "https://x.test"}, {ID: "a", Kind: KindOpenAI, BaseURL: "https://x.test"}},
		"kind":       {{ID: "a", Kind: "gemini", BaseURL: "https://x.test"}},
		"scheme":     {{ID: "a", Kind: KindOpenAI, BaseURL: "ftp://x.test"}},
		"nohost":     {{ID: "a", Kind: KindOpenAI, BaseURL: "https://"}},
		"model":      {{ID: "a", Kind: KindOpenAI, BaseURL: "https://x.test", Models: []Model{{ID: "has space"}}}},
		"dupmodel":   {{ID: "a", Kind: KindOpenAI, BaseURL: "https://x.test", Models: []Model{{ID: "m"}, {ID: "m"}}}},
		"negative":   {{ID: "a", Kind: KindOpenAI, BaseURL: "https://x.test", Models: []Model{{ID: "m", Context: -1}}}},
		"quotemodel": {{ID: "a", Kind: KindOpenAI, BaseURL: "https://x.test", Models: []Model{{ID: `m"x`}}}},
	}
	for name, ps := range bad {
		if Validate(ps) == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestBuildConfigShapeAndKeyReference(t *testing.T) {
	cfg, err := BuildConfig(sample(), map[string]string{"acme": "sekret"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "sekret") {
		t.Fatal("config must reference the env var, never embed the key")
	}
	var top struct {
		Provider map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(cfg), &top); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]map[string]any{"provider": top.Provider}
	acme := got["provider"]["acme"]
	if acme["npm"] != "@ai-sdk/openai-compatible" {
		t.Fatalf("npm = %v", acme["npm"])
	}
	opts := acme["options"].(map[string]any)
	if opts["baseURL"] != "https://api.acme.test/v1" || opts["apiKey"] != "{env:RELO_KEY_ACME}" {
		t.Fatalf("options = %v", opts)
	}
	big := acme["models"].(map[string]any)["big"].(map[string]any)
	limit := big["limit"].(map[string]any)
	if limit["context"].(float64) != 200000 || limit["output"].(float64) != 16000 || big["reasoning"] != true {
		t.Fatalf("big = %v", big)
	}
	plain := acme["models"].(map[string]any)["plain"].(map[string]any)
	if _, ok := plain["limit"]; ok {
		t.Fatal("unknown limits must be omitted, not zeroed")
	}
	claude := got["provider"]["claude-x"]
	if claude["npm"] != "@ai-sdk/anthropic" {
		t.Fatalf("anthropic npm = %v", claude["npm"])
	}
	if _, ok := claude["options"].(map[string]any)["apiKey"]; ok {
		t.Fatal("no key given: apiKey option must be absent")
	}
	again, _ := BuildConfig(sample(), map[string]string{"acme": "sekret"}, false)
	if again != cfg {
		t.Fatal("BuildConfig must be deterministic")
	}
}

func TestEnv(t *testing.T) {
	none, err := Env(nil, nil, false)
	if err != nil || len(none) != 1 || none[0] != `OPENCODE_CONFIG_CONTENT={"enabled_providers":[]}` {
		t.Fatalf("no providers must still disable every built-in provider: %v %v", none, err)
	}
	env, err := Env(sample(), map[string]string{"acme": "k1", "claude-x": ""}, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "OPENCODE_CONFIG_CONTENT=") || !strings.Contains(joined, "RELO_KEY_ACME=k1") || strings.Contains(joined, "RELO_KEY_CLAUDE_X") {
		t.Fatalf("env = %q", env)
	}
	if EnvName("claude-x") != "RELO_KEY_CLAUDE_X" {
		t.Fatalf("EnvName = %s", EnvName("claude-x"))
	}
	if _, err := Env([]Provider{{ID: "BAD"}}, nil, false); err == nil {
		t.Fatal("invalid provider must error")
	}
}

func TestFingerprintChangesOnProviderOrKey(t *testing.T) {
	a := Fingerprint(sample(), map[string]string{"acme": "k1"})
	if a != Fingerprint(sample(), map[string]string{"acme": "k1"}) {
		t.Fatal("fingerprint must be stable")
	}
	if a == Fingerprint(sample(), map[string]string{"acme": "k2"}) {
		t.Fatal("key change must change fingerprint")
	}
	ps := sample()
	ps[0].Models[0].Context = 1
	if a == Fingerprint(ps, map[string]string{"acme": "k1"}) {
		t.Fatal("provider change must change fingerprint")
	}
	if strings.Contains(a, "k1") {
		t.Fatal("fingerprint leaks key")
	}
}

// OpenCode rejects a provider whose model limit has only one of context/output; live-verified on 2.0.27.
func TestBuildConfigNeverEmitsHalfALimit(t *testing.T) {
	ps := []Provider{{ID: "r", Kind: KindOpenAI, BaseURL: "http://localhost:1/v1", Models: []Model{
		{ID: "ctx-only", Context: 1000000}, {ID: "out-only", Output: 4000}, {ID: "both", Context: 5000, Output: 600}, {ID: "none"}}}}
	cfg, err := BuildConfig(ps, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider map[string]struct {
			Models map[string]struct {
				Limit map[string]int `json:"limit"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(cfg), &got); err != nil {
		t.Fatal(err)
	}
	ms := got.Provider["r"].Models
	want := map[string]map[string]int{
		"ctx-only": {"context": 1000000, "output": 32000},
		"out-only": {"context": 200000, "output": 4000},
		"both":     {"context": 5000, "output": 600},
		"none":     nil,
	}
	for id, w := range want {
		l := ms[id].Limit
		if w == nil && l != nil {
			t.Errorf("%s: unknown limits must stay omitted, got %v", id, l)
		}
		if w != nil && (l["context"] != w["context"] || l["output"] != w["output"] || len(l) != 2) {
			t.Errorf("%s: limit = %v, want %v", id, l, w)
		}
	}
}

func TestBuildConfigEnablesOnlyReloProviders(t *testing.T) {
	cfg, err := BuildConfig(sample(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Enabled []string `json:"enabled_providers"`
	}
	if err := json.Unmarshal([]byte(cfg), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Enabled) != 2 || got.Enabled[0] != "acme" || got.Enabled[1] != "claude-x" {
		t.Fatalf("enabled_providers = %v, want the Relo providers sorted", got.Enabled)
	}
	open, err := BuildConfig(sample(), nil, true)
	if err != nil || strings.Contains(open, "enabled_providers") {
		t.Fatalf("allowBuiltin must leave the catalog alone: %s %v", open, err)
	}
}
