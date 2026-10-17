package app

import "github.com/relocode/relo/internal/orchestrator"

func autoEligible(ch orchestrator.Change) bool {
	return ch.Status == "proposed" && ch.Conflict == "" && !ch.Resolving && !ch.Superseded
}

// Events only mark work dirty. One asynchronous worker drains bursts, including events emitted by its own decisions.
func (a *App) scheduleAutoApply() {
	if !a.autoApply.Load() {
		return
	}
	a.autoMu.Lock()
	defer a.autoMu.Unlock()
	if a.autoStopped {
		return
	}
	a.autoDirty = true
	if a.autoDone != nil {
		return
	}
	a.autoDone = make(chan struct{})
	go a.safely("auto-apply changes", a.drainAutoApply)
}

func (a *App) drainAutoApply() {
	a.autoMu.Lock()
	defer func() {
		close(a.autoDone)
		a.autoDone = nil
		a.autoMu.Unlock()
	}()
	for a.autoDirty && !a.autoStopped && a.autoApply.Load() {
		a.autoDirty = false
		a.autoMu.Unlock()
		func() {
			defer a.autoMu.Lock()
			a.supersedeStale()
			_, _ = a.acceptAll(true)
		}()
	}
}

func (a *App) stopAutoApply() {
	a.autoMu.Lock()
	a.autoStopped = true
	done := a.autoDone
	a.autoMu.Unlock()
	if done != nil {
		<-done
	}
}
