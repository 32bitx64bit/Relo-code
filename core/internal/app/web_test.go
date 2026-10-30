package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/web"
)

// packageDefaults is what the extension sends when the user never touched the web settings: the defaults declared in
// extension/package.json. Keeping them here lets the test compare the payload with the engine's settings.
func packageDefaults() map[string]any {
	return map[string]any{
		"mode":                 "auto",
		"searchProvider":       "duckduckgo",
		"customSearchUrl":      "",
		"customSearchTemplate": "",
		"customFallback":       true,
		"autoCheckbox":         true,
		"browserPath":          "",
	}
}

// A confirmed template replaces the typed URL, and an empty one leaves the customSearchUrl mapping as it was.
func TestWebSettingsUseTheConfirmedCustomSearchTemplate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        map[string]any
		wantTmpl   string
		wantOrigin string
	}{
		{"template overrides the url", map[string]any{
			"customSearchUrl":      "https://search.example/?q=%s",
			"customSearchTemplate": "https://Search.Example:8443/search?q=%s&src=relo",
		}, "https://Search.Example:8443/search?q=%s&src=relo", "https://search.example:8443"},
		{"empty template keeps the url", map[string]any{
			"customSearchUrl":      "https://search.example/?q=%s",
			"customSearchTemplate": "",
		}, "https://search.example/?q=%s", "https://search.example"},
		{"missing template keeps the url", map[string]any{
			"customSearchUrl": "https://search.example/?q=%s",
		}, "https://search.example/?q=%s", "https://search.example"},
		{"template alone with no url", map[string]any{
			"customSearchTemplate": "https://search.example/api?q=%s",
		}, "https://search.example/api?q=%s", "https://search.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := webSettings(tc.raw, "/state")
			if got.CustomTemplate != tc.wantTmpl || got.CustomOrigin != tc.wantOrigin {
				t.Fatalf("custom = %q / %q, want %q / %q",
					got.CustomTemplate, got.CustomOrigin, tc.wantTmpl, tc.wantOrigin)
			}
			// The confirmed template must not disturb the rest of the mapping.
			if got.Mode != web.ModeAuto || got.Provider != web.ProviderDuckDuckGo || got.StateDir != "/state" {
				t.Fatalf("settings = %+v", got)
			}
		})
	}
}

func TestWebSettingsMapPackageDefaultsOntoTheEngine(t *testing.T) {
	got := webSettings(packageDefaults(), "/state")
	if got.Mode != web.ModeAuto || got.Provider != web.ProviderDuckDuckGo {
		t.Fatalf("defaults = %+v", got)
	}
	if got.DisableFallback {
		t.Fatal("customFallback true must leave the fallback on")
	}
	if got.ChallengeClick == nil || !*got.ChallengeClick {
		t.Fatal("autoCheckbox true must allow the one checkbox click")
	}
	if got.StateDir != "/state" || got.BrowserPath != "" || got.CustomTemplate != "" {
		t.Fatalf("paths = %+v", got)
	}

	full := webSettings(map[string]any{
		"mode": "BACKGROUND", "searchProvider": "Custom", "customSearchUrl": "https://search.example/?q=%s",
		"customFallback": false, "autoCheckbox": false, "browserPath": "/usr/bin/chromium",
	}, "/state")
	want := web.Settings{Mode: web.ModeBackground, Provider: web.ProviderCustom,
		CustomTemplate: "https://search.example/?q=%s", CustomOrigin: "https://search.example",
		DisableFallback: true, BrowserPath: "/usr/bin/chromium", StateDir: "/state"}
	if full.ChallengeClick == nil || *full.ChallengeClick {
		t.Fatal("autoCheckbox false must refuse the click")
	}
	full.ChallengeClick = nil
	if full != want {
		t.Fatalf("got %+v\nwant %+v", full, want)
	}
}

// A value the core does not know must never pick a strange lane or provider: it keeps the safe default instead.
func TestWebSettingsIgnoreUnknownValues(t *testing.T) {
	got := webSettings(map[string]any{"mode": "stealth", "searchProvider": "google", "autoCheckbox": "yes", "customFallback": "no"}, "")
	if got.Mode != web.ModeAuto || got.Provider != web.ProviderDuckDuckGo {
		t.Fatalf("unknown values changed the settings: %+v", got)
	}
	if got.DisableFallback {
		t.Fatal("a non-boolean customFallback must keep the fallback on")
	}
	if got.ChallengeClick == nil || !*got.ChallengeClick {
		t.Fatal("a value that is not a boolean must leave the checkbox click allowed")
	}
}

func TestOriginOfKeepsSchemeAndHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Search.Example:8443/q?x=%s": "https://search.example:8443",
		"http://127.0.0.1:8080/search":       "http://127.0.0.1:8080",
		"https://example.com":                "https://example.com",
		"example.com/search":                 "",
		"":                                   "",
	} {
		if got := originOf(raw); got != want {
			t.Errorf("originOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The Settings page's relo.web.* section must reach the running engine: web.status answers with what that engine
// would launch. Nothing is launched here: BrowserPath names a file that does not exist, so the engine has no
// executable to start, and web.status only reads the settings back.
func TestApplySettingsReachesTheWebEngine(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), StateDir: filepath.Join(dir, "state"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	// A path that is not an executable: browser.Find refuses it, so no browser process can be started from here.
	missing := filepath.Join(dir, "no-such-browser")
	core.applySettings(map[string]any{"web": map[string]any{
		"mode": "hidden", "searchProvider": "brave", "browserPath": missing,
		// The Settings page always sends the confirmed template; a built-in provider must still accept it.
		"customSearchTemplate": "https://search.example/api?q=%s",
	}})
	st := webStatusJSON(t, core)
	if st["ready"] != true {
		t.Fatalf("web.status did not report a running engine: %v", st)
	}
	if st["mode"] != "hidden" || st["provider"] != "brave" {
		t.Fatalf("web.status = %v, want the settings just applied", st)
	}
	if st["browserPath"] != missing {
		t.Fatalf("web.status browserPath = %v, want %q", st["browserPath"], missing)
	}
	if st["tabs"] != float64(0) || st["agents"] != float64(0) {
		t.Fatalf("answering settings must not open a tab: %v", st)
	}
	// A second section reaches the same engine rather than a fresh one: SetSettings replaces the configuration.
	// The Settings page always sends the whole section, so the keys it does not mention keep their defaults.
	core.applySettings(map[string]any{"web": map[string]any{
		"mode": "visible", "searchProvider": "bing", "browserPath": missing,
	}})
	st = webStatusJSON(t, core)
	if st["mode"] != "visible" || st["provider"] != "bing" {
		t.Fatalf("web.status = %v, want the settings re-applied", st)
	}
	if st["browserPath"] != missing {
		t.Fatalf("web.status browserPath = %v, want the path that cannot launch", st["browserPath"])
	}
	// A launched browser would create its agent profile under the state directory, so its absence is the proof
	// that applying settings and answering status started nothing.
	if _, err := os.Stat(filepath.Join(core.webStateDir(), "web")); err == nil {
		t.Fatal("applying settings or answering web.status must not create browser profiles")
	}
}

// webStatusJSON answers web.status and decodes it, failing the test on a transport or engine error.
func webStatusJSON(t *testing.T, core *App) map[string]any {
	t.Helper()
	got, err := core.Handle("web.status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("web.status: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal web.status: %v", err)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("decode web.status: %v", err)
	}
	return st
}

func TestWebIPCAnswersErrorsAsMessages(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	// An empty URL is refused before the engine sees it, as a message the Settings page can show.
	got, err := core.Handle("web.discover", json.RawMessage(`{"url":"  "}`))
	if err != nil {
		t.Fatalf("web.discover: %v", err)
	}
	if m, ok := got.(map[string]any); !ok || m["error"] == "" {
		t.Fatalf("web.discover = %#v, want an error message", got)
	}
}

func TestWebStateDirStaysUnderRelosState(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), StateDir: filepath.Join(dir, "state"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if got, want := core.webStateDir(), filepath.Join(dir, "state"); got != want {
		t.Fatalf("webStateDir = %q, want %q", got, want)
	} else if _, err := os.Stat(filepath.Join(got, "web")); err == nil {
		t.Fatal("building the settings must not create browser profiles")
	}
}
