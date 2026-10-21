package app

import (
	"context"
	"fmt"

	"github.com/relocode/relo/internal/claudecode"
)

// startClaudeCode brings up the Claude Code runtime when relo.claudeCode.enabled is on. It never fails the backend
// start: without the CLI, or signed out, Claude Code's models are simply not offered, and backend.status says why.
// procMu is held.
func (a *App) startClaudeCode(ctx context.Context) *claudecode.Runtime {
	a.provMu.Lock()
	on, path := a.claudeOn, a.claudePath
	a.provMu.Unlock()
	if !on {
		a.setClaudeCodeStatus(map[string]any{"enabled": false})
		return nil
	}
	fail := func(err error) *claudecode.Runtime {
		a.Log.Warn("Claude Code unavailable", map[string]any{"error": err.Error()})
		a.setClaudeCodeStatus(map[string]any{"enabled": true, "ready": false, "error": err.Error()})
		return nil
	}
	bin, err := claudecode.Look(path)
	if err != nil {
		return fail(err)
	}
	cc, err := claudecode.New(ctx, bin, func(format string, args ...any) {
		a.Log.Warn(fmt.Sprintf(format, args...), nil)
	})
	if err != nil {
		return fail(err)
	}
	info := cc.Info()
	models := make([]string, 0, len(info.Models))
	for _, m := range info.Models {
		models = append(models, m.ID)
	}
	a.Log.Info("Claude Code ready", map[string]any{"path": bin, "version": info.Version, "plan": info.Plan, "models": models})
	a.setClaudeCodeStatus(map[string]any{"enabled": true, "ready": true, "path": bin, "version": info.Version, "plan": info.Plan})
	return cc
}

func (a *App) setClaudeCodeStatus(st map[string]any) {
	a.statusMu.Lock()
	a.claudeStatus = st
	a.statusMu.Unlock()
}

func (a *App) claudeCodeStatus() map[string]any {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	if a.claudeStatus == nil {
		return map[string]any{"enabled": false}
	}
	out := make(map[string]any, len(a.claudeStatus))
	for k, v := range a.claudeStatus {
		out[k] = v
	}
	return out
}
