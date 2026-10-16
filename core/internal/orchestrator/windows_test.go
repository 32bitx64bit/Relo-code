package orchestrator

import (
	"testing"

	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
)

func windowSettings(limit int) Settings {
	s := DefaultSettings()
	s.ContextLimit = limit
	s.Models = map[string]runtime.Model{
		"p/big":   {ID: "big", Provider: "p", Context: 1_000_000, Efforts: []string{"low", "high"}},
		"p/small": {ID: "small", Provider: "p", Context: 32_000},
		"p/blind": {ID: "blind", Provider: "p"},
	}
	return s
}

func TestWindowIsMinOfGlobalAndModel(t *testing.T) {
	s := windowSettings(350_000)
	cases := map[string]int{"p/big": 350_000, "p/small": 32_000, "p/blind": 350_000, "p/none": 350_000}
	for model, want := range cases {
		if got := s.Window(model); got != want {
			t.Errorf("%s window = %d, want %d", model, got, want)
		}
	}
	if got := windowSettings(0).Window("p/big"); got != 1_000_000 {
		t.Errorf("no cap: window = %d, want model max", got)
	}
	if got := windowSettings(0).Window("p/blind"); got != 0 {
		t.Errorf("unknown everything: window = %d, want 0", got)
	}
}

func TestBudgetForOnlyEverLowers(t *testing.T) {
	s := windowSettings(100_000)
	if got := s.BudgetFor("p/big", 24_000); got != 24_000 {
		t.Errorf("window larger than base must not raise budget: %d", got)
	}
	if got := s.BudgetFor("p/small", 24_000); got != 24_000 {
		t.Errorf("window*util (27200) above base must keep base: %d", got)
	}
	if got := s.BudgetFor("p/small", 100_000); got != int(32_000*0.85) {
		t.Errorf("small model must cap budget to window*util: %d", got)
	}
	if got := windowSettings(0).BudgetFor("p/blind", 24_000); got != 24_000 {
		t.Errorf("unknown window must leave budget: %d", got)
	}
	tiny := windowSettings(1)
	if got := tiny.BudgetFor("p/big", 24_000); got != 1 {
		t.Errorf("tiny window floors at 1: %d", got)
	}
	s.MaxUtil = 5
	if got := s.BudgetFor("p/small", 1_000_000); got != int(32_000*0.85) {
		t.Errorf("bad utilization falls back to 0.85: %d", got)
	}
}

func TestEffortForRequiresAdvertisedEffort(t *testing.T) {
	s := windowSettings(0)
	s.ModelEfforts = map[string]string{"p/big": "high", "p/small": "high", "p/blind": "low"}
	if got := s.EffortFor("p/big"); got != "high" {
		t.Errorf("advertised effort lost: %q", got)
	}
	if s.EffortFor("p/small") != "" || s.EffortFor("p/blind") != "" || s.EffortFor("p/missing") != "" {
		t.Error("unadvertised effort must be ignored")
	}
	s.ModelEfforts["p/big"] = "ultra"
	if s.EffortFor("p/big") != "" {
		t.Error("unknown effort must be ignored")
	}
}

func TestRouteConfigCarriesMode(t *testing.T) {
	s := DefaultSettings()
	s.PrimaryModel, s.Tier1Model, s.Tier2Model = "p/a", "p/b", "p/c"
	s.ByDifficulty = map[string]string{"hard": "p/z"}
	if got := s.RouteConfig().For("tier1", "worker", "hard"); got != "p/b" {
		t.Errorf("default (simple) must ignore byDifficulty, got %q", got)
	}
	s.RoutingMode = routing.ModeAdvanced
	if got := s.RouteConfig().For("tier1", "worker", "hard"); got != "p/z" {
		t.Errorf("advanced must honour byDifficulty, got %q", got)
	}
}

func TestCompactAfterIsTheShareOfTheWindow(t *testing.T) {
	s := windowSettings(300_000)
	cases := []struct {
		model string
		share float64
		want  int
	}{
		{"p/big", 0.9, 270_000},   // the cap, not the model's million
		{"p/small", 0.9, 28_800},  // a smaller model keeps its own window
		{"p/blind", 0.5, 150_000}, // unknown model max: the cap is the window
		{"p/big", 1, 300_000},
		{"p/big", 0, 0},    // off
		{"p/big", 1.2, 0},  // not a share
		{"p/big", -0.1, 0}, // not a share
	}
	for _, tc := range cases {
		s.CompactAt = tc.share
		if got := s.CompactAfter(tc.model); got != tc.want {
			t.Errorf("%s at %v = %d, want %d", tc.model, tc.share, got, tc.want)
		}
	}
	s = windowSettings(0)
	s.CompactAt = 0.9
	if got := s.CompactAfter("p/blind"); got != 0 {
		t.Errorf("unknown window = %d, want 0 (nothing to take a share of)", got)
	}
	if got := DefaultSettings().CompactAt; got != 0.9 {
		t.Errorf("default share = %v, want 0.9", got)
	}
}
