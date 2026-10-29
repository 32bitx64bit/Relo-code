package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/art"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
)

func TestArtSettingsReadsThePayloadAndKeepsSafeDefaults(t *testing.T) {
	got := artSettings(map[string]any{
		"enabled": true, "provider": "openai-main", "model": "gpt-image-2.5-sunburst", "quality": "HIGH", "size": "1536x1024",
		"candidates": float64(3), "sessionBudgetUSD": float64(7.5), "sessionJobLimit": float64(4), "approval": "each",
	})
	want := art.Settings{Enabled: true, Provider: "openai-main", Model: "gpt-image-2.5-sunburst", Quality: "high", Size: "1536x1024",
		Count: 3, BudgetUSD: 7.5, SessionJobLimit: 4, Approval: "each"}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// Anything missing, or not a boolean true, leaves the spending off.
	if empty := artSettings(map[string]any{"enabled": "yes"}); empty.Enabled || empty.Approval != "session" {
		t.Fatalf("defaults = %+v", empty)
	}
}

// 0 turns a limit off, so a limit that was never sent has to get its default instead, and a limit that is sent as 0 has
// to stay 0 all the way through.
func TestArtSettingsTellsAnAbsentLimitFromAnUnlimitedOne(t *testing.T) {
	absent := artSettings(map[string]any{"enabled": true})
	if absent.BudgetUSD != art.DefaultBudgetUSD || absent.SessionJobLimit != art.DefaultJobLimit {
		t.Fatalf("absent limits = %+v", absent)
	}
	off := artSettings(map[string]any{"enabled": true, "sessionBudgetUSD": float64(0), "sessionJobLimit": float64(0)})
	if off.BudgetUSD != 0 || off.SessionJobLimit != 0 {
		t.Fatalf("limits sent as 0 = %+v", off)
	}
	// One limit off does not turn off the other.
	half := artSettings(map[string]any{"enabled": true, "sessionBudgetUSD": float64(0)})
	if half.BudgetUSD != 0 || half.SessionJobLimit != art.DefaultJobLimit {
		t.Fatalf("budget off only = %+v", half)
	}
}

// Picture generation has no IPC of its own: the agents' tool does the work and the user's answer comes through the
// ordinary permission prompt. Nothing from the earlier browser-based design should still answer.
func TestThereAreNoArtIPCMethods(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	for _, method := range []string{"art.jobs", "art.budget", "art.pick", "art.cancel", "art.resume", "art.import", "art.reply"} {
		if _, err := core.Handle(method, json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s still answers", method)
		}
	}
	for _, name := range []string{"art.job", "art.requested", "art.abort"} {
		if noPersistEvents[name] {
			t.Errorf("%s is still listed as an event", name)
		}
	}
}

func TestApplySettingsTurnsPictureGenerationOnAndOff(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	core.Coord.CreateSession(dir, "make a sprite", orchestrator.ModeBuild)
	core.applySettings(map[string]any{"art": map[string]any{"enabled": true}})
	if !core.Coord.ArtEnabled() {
		t.Fatal("the art section did not switch the tools on")
	}
	core.applySettings(map[string]any{"art": map[string]any{"enabled": false}})
	if core.Coord.ArtEnabled() {
		t.Fatal("the art section did not switch the tools off")
	}
	// The old section name is ignored, so a stale client cannot switch spending on.
	core.applySettings(map[string]any{"midjourney": map[string]any{"enabled": true}})
	if core.Coord.ArtEnabled() {
		t.Fatal("an unknown section switched the tools on")
	}
}

func TestArtEndpointNamesTheProviderSoOpenRouterRoutingApplies(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	// An OpenRouter provider behind a base URL whose host is not openrouter.ai (a proxy or a custom slug): only the
	// provider name lets the art package route to POST /images instead of /images/edits.
	core.applyProviders(map[string]any{
		"providers": []any{
			map[string]any{"id": "openrouter1", "kind": "openai", "baseURL": "https://api.example.test/v1", "models": []any{}},
		},
		"providerKeys": map[string]any{"openrouter1": "sk-or-TESTKEY1234"},
	})

	ep, err := core.artEndpoint("openrouter1")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Provider != "openrouter1" {
		t.Fatalf("art endpoint drops the provider ID the art package detects OpenRouter with: %+v", ep)
	}
	if ep.BaseURL != "https://api.example.test/v1" || ep.Key != "sk-or-TESTKEY1234" {
		t.Fatalf("art endpoint changed the base URL or key: %+v", ep)
	}
}

func TestArtEndpointComesFromTheProviderSettingsAndOnlyFromOpenAIStyleOnes(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	if _, err := core.artEndpoint(""); err == nil || !strings.Contains(err.Error(), "no OpenAI-style provider") {
		t.Fatalf("with no providers: %v", err)
	}

	core.applyProviders(map[string]any{
		"providers": []any{
			map[string]any{"id": "claude", "kind": "anthropic", "baseURL": "https://api.anthropic.com/v1", "models": []any{}},
			map[string]any{"id": "openai", "kind": "openai", "baseURL": "https://api.openai.com/v1", "models": []any{}},
			map[string]any{"id": "router", "kind": "openai", "baseURL": "http://127.0.0.1:20128/v1", "models": []any{}},
		},
		"providerKeys": map[string]any{"claude": "sk-ant-CLAUDEKEY1234", "openai": "sk-test-OPENAIKEY1234", "router": "sk-test-ROUTERKEY1234"},
	})

	// Default: the first OpenAI-style provider, never the Anthropic one listed before it.
	ep, err := core.artEndpoint("")
	if err != nil || ep.BaseURL != "https://api.openai.com/v1" || ep.Key != "sk-test-OPENAIKEY1234" {
		t.Fatalf("default endpoint = %v (%v)", ep, err)
	}
	if ep, err := core.artEndpoint("router"); err != nil || ep.BaseURL != "http://127.0.0.1:20128/v1" || ep.Key != "sk-test-ROUTERKEY1234" {
		t.Fatalf("named endpoint = %v (%v)", ep, err)
	}

	// Anthropic cannot generate images, and the error says so without showing its key.
	_, err = core.artEndpoint("claude")
	if err == nil || !strings.Contains(err.Error(), "cannot generate images") || strings.Contains(err.Error(), "CLAUDEKEY") {
		t.Fatalf("anthropic provider: %v", err)
	}
	_, err = core.artEndpoint("nope")
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("unknown provider: %v", err)
	}

	// A provider with no key still gives an endpoint (a local router may not want one); the endpoint prints without it.
	core.applyProviders(map[string]any{"providerKeys": map[string]any{}})
	if ep, err := core.artEndpoint("openai"); err != nil || ep.Key != "" {
		t.Fatalf("no key: %v %v", ep, err)
	}
}
