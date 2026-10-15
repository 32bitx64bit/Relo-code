package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

// applyProviders reads providers and providerKeys from an initialize/settings.apply payload into App state.
// Absent keys leave the current value alone. Invalid providers are dropped with a warning that names the
// problem but never the API key. It runs before the runtime starts, so it takes only provMu.
func (a *App) applyProviders(p map[string]any) {
	a.provMu.Lock()
	defer a.provMu.Unlock()
	if raw, ok := p["providers"]; ok {
		var list []provider.Provider
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &list)
		}
		if err := provider.Validate(list); err != nil {
			a.Log.Warn("providers ignored", map[string]any{"error": err.Error()})
			list = nil
		}
		a.providers = list
	}
	if raw, ok := p["providerKeys"].(map[string]any); ok {
		keys := make(map[string]string, len(raw))
		for id, v := range raw {
			if k, ok := v.(string); ok && k != "" {
				keys[id] = k
			}
		}
		a.providerKeys = keys
	}
}

// providerEnv is the extra environment for the OpenCode child.
func (a *App) providerEnv() ([]string, error) {
	a.provMu.Lock()
	defer a.provMu.Unlock()
	return provider.Env(a.providers, a.providerKeys, shareOpenCode())
}

// markProviderRunning records which provider config the live OpenCode child was started with.
func (a *App) markProviderRunning() {
	a.provMu.Lock()
	a.runningProvider = provider.Fingerprint(a.providers, a.providerKeys)
	a.restartPending = false
	a.provMu.Unlock()
}

// providersChanged reports whether desired provider config differs from what the child runs with.
func (a *App) providersChanged() bool {
	a.provMu.Lock()
	defer a.provMu.Unlock()
	return provider.Fingerprint(a.providers, a.providerKeys) != a.runningProvider
}

func (a *App) setRestartPending(v bool) {
	a.provMu.Lock()
	a.restartPending = v
	a.provMu.Unlock()
}

func (a *App) isRestartPending() bool {
	a.provMu.Lock()
	defer a.provMu.Unlock()
	return a.restartPending
}

// restartRuntime starts a fresh OpenCode child with current provider config; startRuntime retires the old
// one. Refused while agents are active: their sessions live in the old child.
func (a *App) restartRuntime(ctx context.Context) (runtime.Health, error) {
	if a.Coord.Busy() {
		a.setRestartPending(true)
		return a.getStatus(), fmt.Errorf("agents are running; the backend restarts when they finish")
	}
	h, err := a.startRuntime(ctx)
	a.refreshModels(ctx)
	return h, err
}

// applyPendingRestart restarts OpenCode once the work that deferred it is over. Provider and model edits made while
// agents run are only stored; nothing used to apply them afterwards, so a model added mid-run stayed out of the
// pickers until the window was reloaded. It runs on every agent completion and does nothing unless a restart is
// pending and nothing is running or about to run: the child holds every agent's session, so restarting it between
// one task finishing and the next starting would cut the next one off.
func (a *App) applyPendingRestart() {
	if !a.isRestartPending() || !a.Coord.Idle() {
		return
	}
	if !a.restarting.CompareAndSwap(false, true) {
		return
	}
	defer a.restarting.Store(false)
	if !a.isRestartPending() || !a.Coord.Idle() {
		return
	}
	if _, err := a.restartRuntime(context.Background()); err != nil {
		a.Log.Warn("deferred backend restart failed", map[string]any{"error": err.Error()})
	}
	a.setRestartPending(false) // one attempt per edit: it is not retried on every later completion
	a.emit("backend.status", a.backendStatus())
}

// refreshModels snapshots the runtime catalog into Settings.Models so windows and efforts are available
// to the orchestrator, and fills any empty model slot with a sensible default.
func (a *App) refreshModels(ctx context.Context) {
	models, err := a.models(ctx)
	if err != nil {
		a.Log.Warn("list models failed", map[string]any{"error": err.Error()})
		return
	}
	catalog := make(map[string]runtime.Model, len(models))
	for _, m := range models {
		catalog[m.Provider+"/"+m.ID] = m
	}
	a.Coord.UpdateSettings(func(s *orchestrator.Settings) { s.Models, s.ModelsKnown = catalog, true })
}

// settingsApply re-applies a settings payload to a running core. When provider config changed and no agent
// is active, the OpenCode child restarts so the new providers and keys take effect; otherwise the restart is
// left pending until the next idle apply or backend.restart.
func (a *App) settingsApply(ctx context.Context, p map[string]any) (any, error) {
	a.applySettings(p)
	restarted := false
	if a.providersChanged() {
		if a.Coord.Busy() {
			a.setRestartPending(true)
		} else if _, err := a.restartRuntime(ctx); err != nil {
			a.Log.Warn("backend restart failed", map[string]any{"error": err.Error()})
		} else {
			restarted = true
		}
	} else {
		a.refreshModels(ctx)
	}
	st := a.backendStatus()
	a.emit("backend.status", st)
	return map[string]any{"applied": true, "restarted": restarted, "restartPending": a.isRestartPending(), "backend": st}, nil
}

// providersDiscover lists models from a candidate endpoint. Failures come back in the payload so the UI can
// show them next to the form instead of as an RPC error. The API key is used for the request only.
func (a *App) providersDiscover(ctx context.Context, p map[string]any) (any, error) {
	models, err := provider.Discover(ctx, str(p, "kind", ""), str(p, "baseURL", ""), str(p, "apiKey", ""))
	if err != nil {
		return map[string]any{"models": []provider.Model{}, "error": err.Error()}, nil
	}
	if models == nil {
		models = []provider.Model{}
	}
	return map[string]any{"models": models, "error": ""}, nil
}
