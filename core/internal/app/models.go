package app

import (
	"context"

	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/vision"
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
//
// Image support is resolved the same way (internal/vision): configured, then a built-in model with the same id,
// then a guess from the id. OpenCode's own answer for a custom model is ignored, because it says ["text","image"]
// for every model that declares nothing. A built-in model the catalog says nothing about gets the guess too.
func reconcile(models []runtime.Model, ps []provider.Provider) []runtime.Model {
	if len(ps) == 0 {
		out := make([]runtime.Model, len(models))
		copy(out, models)
		for i := range out {
			guessVision(&out[i])
		}
		return out
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
			if custom[m.Provider] {
				m.Vision, m.VisionSource = vision.Resolve(nil, vision.Unknown, m.ID)
			} else {
				guessVision(m)
			}
			continue
		}
		catalog := vision.Unknown
		for _, d := range models {
			if d.ID == m.ID && !custom[d.Provider] && d.Vision != vision.Unknown {
				catalog = d.Vision
				break
			}
		}
		m.Vision, m.VisionSource = vision.Resolve(c.Vision, catalog, m.ID)
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

// guessVision fills an unknown answer from the model id.
func guessVision(m *runtime.Model) {
	if m.Vision != vision.Unknown {
		return
	}
	if m.Vision = vision.Guess(m.ID); m.Vision != vision.Unknown {
		m.VisionSource = vision.SourceName
	}
}
