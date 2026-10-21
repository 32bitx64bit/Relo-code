package app

import (
	"reflect"
	"testing"

	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

func TestReconcileCustomModels(t *testing.T) {
	synth := []string{"low", "medium", "high"}
	in := []runtime.Model{
		{ID: "claude-opus-4", Provider: "anthropic", Context: 200000, Output: 32000, Efforts: []string{"low", "high"}},
		{ID: "unknown-x", Provider: "mine", Context: 200000, Output: 32000, Efforts: synth},
		{ID: "claude-opus-4", Provider: "mine", Context: 200000, Output: 32000, Efforts: synth},
		{ID: "r1", Provider: "mine", Context: 200000, Output: 32000, Efforts: synth},
		{ID: "sized", Provider: "mine", Context: 200000, Output: 32000, Efforts: synth},
		{ID: "other", Provider: "builtin", Context: 1000, Output: 100},
	}
	ps := []provider.Provider{{ID: "mine", Kind: "openai", BaseURL: "https://x.test", Models: []provider.Model{
		{ID: "unknown-x"},
		{ID: "claude-opus-4"},
		{ID: "r1", Reasoning: true},
		{ID: "sized", Context: 64000, Output: 4000, Efforts: []string{"minimal", "high"}},
	}}}
	got := reconcile(in, ps)
	want := map[string]runtime.Model{
		"mine/unknown-x":          {ID: "unknown-x", Provider: "mine"},
		"mine/claude-opus-4":      {ID: "claude-opus-4", Provider: "mine", Context: 200000, Output: 32000, Efforts: []string{"low", "high"}, Vision: "yes", VisionSource: "name"},
		"mine/r1":                 {ID: "r1", Provider: "mine", Efforts: synth},
		"mine/sized":              {ID: "sized", Provider: "mine", Context: 64000, Output: 4000, Efforts: []string{"minimal", "high"}},
		"builtin/other":           in[5],
		"anthropic/claude-opus-4": {ID: "claude-opus-4", Provider: "anthropic", Context: 200000, Output: 32000, Efforts: []string{"low", "high"}, Vision: "yes", VisionSource: "name"},
	}
	for _, m := range got {
		if w := want[m.Provider+"/"+m.ID]; !reflect.DeepEqual(m, w) {
			t.Errorf("%s/%s = %+v, want %+v", m.Provider, m.ID, m, w)
		}
	}
	if in[1].Context != 200000 || len(in[1].Efforts) != 3 {
		t.Fatal("input mutated")
	}
	plain := []runtime.Model{{ID: "other", Provider: "builtin", Context: 1000}}
	if !reflect.DeepEqual(reconcile(plain, nil), plain) {
		t.Fatal("no providers must pass through")
	}
}

func TestReconcileVision(t *testing.T) {
	yes, no := true, false
	in := []runtime.Model{
		// OpenCode's catalog: real for built-in providers, made up for custom ones.
		{ID: "shared", Provider: "anthropic", Vision: "no", VisionSource: "catalog"},
		{ID: "shared", Provider: "mine", Vision: "yes", VisionSource: "catalog"},
		{ID: "made-up", Provider: "mine", Vision: "yes", VisionSource: "catalog"},
		{ID: "set-no", Provider: "mine", Vision: "yes", VisionSource: "catalog"},
		{ID: "set-yes", Provider: "mine", Vision: "no", VisionSource: "catalog"},
		{ID: "deepseek-chat", Provider: "mine", Vision: "yes", VisionSource: "catalog"},
		{ID: "gemini-2.5-pro", Provider: "google"},
		{ID: "dropped", Provider: "mine", Vision: "yes", VisionSource: "catalog"},
	}
	ps := []provider.Provider{{ID: "mine", Kind: "openai", BaseURL: "https://x.test", Models: []provider.Model{
		{ID: "shared"}, {ID: "made-up"}, {ID: "set-no", Vision: &no}, {ID: "set-yes", Vision: &yes}, {ID: "deepseek-chat"},
	}}}
	want := map[string]string{
		"anthropic/shared":      "no/catalog",
		"mine/shared":           "no/catalog", // a built-in twin's catalog beats OpenCode's made-up answer
		"mine/made-up":          "/",          // nothing real is known
		"mine/set-no":           "no/configured",
		"mine/set-yes":          "yes/configured",
		"mine/deepseek-chat":    "no/name",
		"google/gemini-2.5-pro": "yes/name",
		"mine/dropped":          "/", // a custom model missing from the config never keeps OpenCode's answer
	}
	for _, m := range reconcile(in, ps) {
		if got := m.Vision + "/" + m.VisionSource; got != want[m.Provider+"/"+m.ID] {
			t.Errorf("%s/%s vision = %s, want %s", m.Provider, m.ID, got, want[m.Provider+"/"+m.ID])
		}
	}
}
