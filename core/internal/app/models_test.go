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
		"mine/claude-opus-4":      {ID: "claude-opus-4", Provider: "mine", Context: 200000, Output: 32000, Efforts: []string{"low", "high"}},
		"mine/r1":                 {ID: "r1", Provider: "mine", Efforts: synth},
		"mine/sized":              {ID: "sized", Provider: "mine", Context: 64000, Output: 4000, Efforts: []string{"minimal", "high"}},
		"builtin/other":           in[5],
		"anthropic/claude-opus-4": in[0],
	}
	for _, m := range got {
		if w := want[m.Provider+"/"+m.ID]; !reflect.DeepEqual(m, w) {
			t.Errorf("%s/%s = %+v, want %+v", m.Provider, m.ID, m, w)
		}
	}
	if in[1].Context != 200000 || len(in[1].Efforts) != 3 {
		t.Fatal("input mutated")
	}
	if !reflect.DeepEqual(reconcile(in, nil), in) {
		t.Fatal("no providers must pass through")
	}
}
