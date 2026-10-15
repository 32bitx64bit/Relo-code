package app

import (
	"context"

	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

// models lists the runtime catalog with user-defined provider models reconciled against Relo config.
func (a *App) models(ctx context.Context) ([]runtime.Model, error) {
	rt := a.Coord.RuntimeSnapshot()
	if rt == nil {
		return nil, nil
	}
	ms, err := rt.Models(ctx)
	if err != nil {
		return nil, err
	}
	a.provMu.Lock()
	ps := a.providers
	a.provMu.Unlock()
	return reconcile(ms, ps), nil
}

// reconcile makes custom-provider models reflect what the user (or discovery) configured. OpenCode invents
// 200k/32k and low/medium/high for custom models that declare none, which is indistinguishable from real
// data, so those values are replaced: configured fields win, unknown stay zero/empty, and only then does a
// same-ID model from a built-in provider donate missing fields. A model with reasoning off and no declared
// efforts gets none of OpenCode's synthetic variants. models is not mutated.
func reconcile(models []runtime.Model, ps []provider.Provider) []runtime.Model {
	if len(ps) == 0 {
		return models
	}
	conf := map[string]provider.Model{}
	for _, p := range ps {
		for _, m := range p.Models {
			conf[p.ID+"/"+m.ID] = m
		}
	}
	custom := map[string]bool{}
	for _, p := range ps {
		custom[p.ID] = true
	}
	out := make([]runtime.Model, len(models))
	copy(out, models)
	for i := range out {
		m := &out[i]
		c, ok := conf[m.Provider+"/"+m.ID]
		if !ok {
			continue
		}
		m.Context, m.Output = c.Context, c.Output
		m.Efforts = nil
		if len(c.Efforts) > 0 {
			m.Efforts = append([]string(nil), c.Efforts...)
		} else if c.Reasoning {
			m.Efforts = models[i].Efforts // reasoning declared, levels unknown: keep OpenCode's list
		}
		for _, d := range models {
			if d.ID != m.ID || custom[d.Provider] {
				continue
			}
			if m.Context == 0 {
				m.Context = d.Context
			}
			if m.Output == 0 {
				m.Output = d.Output
			}
			if len(m.Efforts) == 0 {
				m.Efforts = append([]string(nil), d.Efforts...)
			}
			if m.Context > 0 && m.Output > 0 && len(m.Efforts) > 0 {
				break
			}
		}
	}
	return out
}
