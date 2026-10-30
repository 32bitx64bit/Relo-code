package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/web"
)

// webSettings reads the relo.web.* section of the initialize/settings.apply payload into the engine's settings.
// Only a value the core knows changes anything: an unknown mode or provider falls back to the default rather than to
// a strange lane. stateDir is Relo's own state directory, so the agent profiles live under it and never in the user's.
func webSettings(raw map[string]any, stateDir string) web.Settings {
	// The Settings page confirms a discovered template into customSearchTemplate, and that confirmed template wins
	// over the raw URL the user typed: only the engine understands the template format, so the core stores it as it
	// arrives. An empty template leaves the customSearchUrl mapping alone.
	custom := str(raw, "customSearchUrl", "")
	if tmpl := str(raw, "customSearchTemplate", ""); tmpl != "" {
		custom = tmpl
	}
	s := web.Settings{
		Mode:            web.ModeAuto,
		Provider:        web.ProviderDuckDuckGo,
		CustomTemplate:  custom,
		CustomOrigin:    originOf(custom),
		DisableFallback: !boolOr(raw, "customFallback", true),
		BrowserPath:     str(raw, "browserPath", ""),
		StateDir:        stateDir,
	}
	switch m := web.Mode(strings.ToLower(str(raw, "mode", ""))); m {
	case web.ModeHidden, web.ModeBackground, web.ModeVisible:
		s.Mode = m
	}
	switch p := web.ProviderName(strings.ToLower(str(raw, "searchProvider", ""))); p {
	case web.ProviderBrave, web.ProviderBing, web.ProviderCustom:
		s.Provider = p
	}
	switch k := web.ResultKind(strings.ToLower(str(raw, "customSearchFormat", ""))); k {
	case web.KindRSS, web.KindAtom, web.KindJSON, web.KindHTML:
		s.CustomResultKind = k
	}
	// The checkbox click is allowed unless the user turned it off, so an absent key or a value that is not a boolean
	// must not read as false.
	click := true
	if _, ok := raw["autoCheckbox"]; ok {
		click = boolOr(raw, "autoCheckbox", true)
	}
	s.ChallengeClick = &click
	return s
}

// originOf keeps the scheme and host of a search URL: that origin is the one exemption from the private-network
// block, and the engine needs it apart from the template.
func originOf(raw string) string {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return ""
	}
	rest := raw[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return ""
	}
	return strings.ToLower(raw[:i]) + "://" + strings.ToLower(rest)
}

// boolOr reads a boolean that may arrive as JSON true/false or as a string, defaulting to def.
func boolOr(m map[string]any, k string, def bool) bool {
	switch v := m[k].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "on":
			return true
		case "false", "0", "off":
			return false
		}
	}
	return def
}

// webStateDir is where the agent browser profiles live: under Relo's state root, so they are as private as the
// OpenCode child's own state and never anywhere near the user's browser profile.
func (a *App) webStateDir() string {
	if dir := a.stateRoot(); dir != "" {
		return dir
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "relo-code", "opencode")
	}
	return filepath.Join(a.Cfg.Workspace, ".relo", "opencode")
}

// webCurrent is the last relo.web.* section the extension sent, with Relo's state directory filled in. Before the
// first settings.apply it is the defaults, which is enough to answer web.status.
func (a *App) webCurrent() web.Settings {
	a.webCfgMu.Lock()
	raw := a.webCfg
	a.webCfgMu.Unlock()
	return webSettings(raw, a.webStateDir())
}

// startWeb builds the web engine and gives it to the orchestrator, which offers relo_web only once an engine is there.
// A running engine just takes the new settings. A build failure is logged and leaves browsing off rather than taking
// the backend down; the next settings change tries again. A failure to apply settings to a running engine replaces it,
// since a half-configured engine is worse than none.
func (a *App) startWeb() {
	settings := a.webCurrent()
	a.webMu.Lock()
	defer a.webMu.Unlock()
	if a.web != nil {
		err := a.web.SetSettings(settings)
		if err == nil {
			return
		}
		a.Log.Warn("web settings", map[string]any{"error": err.Error()})
		a.Coord.SetWebEngine(nil)
		_ = a.web.Close()
		a.web = nil
	}
	eng, err := web.New(settings, web.Hooks{Challenge: a.Coord.WebChallenge, Log: a.webLog})
	if err != nil {
		a.Log.Warn("web engine", map[string]any{"error": err.Error()})
		return
	}
	a.web = eng
	a.Coord.SetWebEngine(eng)
}

func (a *App) webLog(msg string) { a.Log.Info("web", map[string]any{"message": msg}) }

// closeWeb stops the agent browser processes. It runs in App.Close after the agents have stopped, so no op is left.
func (a *App) closeWeb() {
	a.webMu.Lock()
	eng := a.web
	a.web = nil
	a.webMu.Unlock()
	if eng == nil {
		return
	}
	a.Coord.SetWebEngine(nil)
	if err := eng.Close(); err != nil {
		a.Log.Warn("close web engine failed", map[string]any{"error": err.Error()})
	}
}

// applyWebSettings stores the relo.web.* section from settings.apply and hands it to the engine. Agents cannot reach
// this: only the extension's settings page sends it.
func (a *App) applyWebSettings(raw map[string]any) {
	if raw == nil {
		return
	}
	a.webCfgMu.Lock()
	a.webCfg = raw
	a.webCfgMu.Unlock()
	a.startWeb()
}

// webEngineFor returns the engine, starting it if the settings never mentioned web. It answers nil when the engine
// could not be built.
func (a *App) webEngineFor() *web.Engine {
	a.startWeb()
	a.webMu.Lock()
	defer a.webMu.Unlock()
	return a.web
}

// webStatus answers web.status: what the engine would launch, without starting any browser.
func (a *App) webStatus(ctx context.Context) (any, error) {
	eng := a.webEngineFor()
	if eng == nil {
		return nil, ipc.Fail{C: "web_unavailable", M: "the web engine could not be started; see the log for the reason"}
	}
	st, err := eng.Status(ctx)
	if err != nil {
		return nil, ipc.Fail{C: "web_status_failed", M: err.Error()}
	}
	return st, nil
}

// webDiscover answers web.discover: it tests the custom search URL the way a browser would and returns the template,
// origin and format for the user to confirm. A failure is a message in the reply, not a failed call, so the Settings
// page can show it beside the field the user is editing.
func (a *App) webDiscover(ctx context.Context, p map[string]any) (any, error) {
	raw := strings.TrimSpace(str(p, "url", ""))
	if raw == "" {
		return map[string]any{"error": "enter the search URL to test"}, nil
	}
	if len(raw) > 8192 {
		return map[string]any{"error": "the search URL is too long"}, nil
	}
	eng := a.webEngineFor()
	if eng == nil {
		return nil, ipc.Fail{C: "web_unavailable", M: "the web engine could not be started; see the log for the reason"}
	}
	prov, err := eng.DiscoverProvider(ctx, raw)
	if err != nil {
		return map[string]any{"error": err.Error()}, nil
	}
	return map[string]any{
		"template": prov.Template,
		"origin":   prov.Origin,
		"format":   string(prov.ResultKind),
		"source":   string(prov.Source),
		"provider": string(prov.Name),
		"error":    "",
	}, nil
}
