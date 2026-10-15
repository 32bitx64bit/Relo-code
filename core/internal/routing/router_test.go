package routing

import (
	"github.com/relocode/relo/internal/runtime"
	"reflect"
	"testing"

	"github.com/relocode/relo/internal/agent"
)

func TestNormalizeDifficulty(t *testing.T) {
	tests := []struct{ input, want string }{
		{" TRIVIAL ", Trivial}, {"easy", Easy}, {"Medium", Medium}, {" hard ", Hard}, {"expert", Expert}, {"", ""}, {"unknown", ""},
	}
	for _, tt := range tests {
		if got := NormalizeDifficulty(tt.input); got != tt.want {
			t.Errorf("NormalizeDifficulty(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestConfigFor(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		tier       agent.Tier
		role       agent.Role
		difficulty string
		want       string
	}{
		{"role default", Config{Primary: "p", Tier1: "one", Tier2: "two"}, agent.Tier1, agent.Worker, "", "one"},
		{"easy routes tier2", Config{Primary: "p", Tier1: "one", Tier2: "two"}, agent.Tier1, agent.Probe, Easy, "two"},
		{"difficulty override", Config{Mode: ModeAdvanced, Primary: "p", Tier1: "one", Tier2: "two", ByDifficulty: map[string]string{Hard: "custom"}}, agent.Tier2, agent.Worker, Hard, "custom"},
		{"primary role", Config{Primary: "p", Tier1: "one", Tier2: "two", ByDifficulty: map[string]string{Expert: "custom"}}, agent.Tier1, agent.Primary, Easy, "p"},
		{"fallback up", Config{Primary: "p", Tier2: "two"}, agent.Tier1, agent.Worker, Hard, "p"},
		{"fallback down", Config{Tier1: "one"}, agent.TierPrimary, agent.Worker, Expert, "one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.For(tt.tier, tt.role, tt.difficulty); got != tt.want {
				t.Errorf("For() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLadderAndEscalate(t *testing.T) {
	c := Config{Mode: ModeAdvanced, Primary: "p", Tier1: "one", Tier2: "two", ByDifficulty: map[string]string{
		Trivial: "cheap-extra", Easy: "two", Medium: "mid-extra", Hard: "one", Expert: "top-extra",
	}}
	want := []string{"two", "cheap-extra", "one", "mid-extra", "p", "top-extra"}
	if got := c.Ladder(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Ladder() = %v, want %v", got, want)
	}
	if got, ok := c.Escalate("two"); !ok || got != "cheap-extra" {
		t.Fatalf("Escalate(two) = %q, %v", got, ok)
	}
	if got, ok := c.Escalate("top-extra"); ok || got != "" {
		t.Fatalf("Escalate(top-extra) = %q, %v", got, ok)
	}
}

func TestSimpleModeIgnoresByDifficultyAndAdvancedHonoursIt(t *testing.T) {
	cfg := Config{Primary: "p/top", Tier1: "p/mid", Tier2: "p/low", ByDifficulty: map[string]string{Hard: "p/hardcustom", Trivial: "p/tinycustom"}}
	for _, mode := range []string{"", ModeSimple, "garbage"} {
		cfg.Mode = mode
		if got := cfg.For(agent.Tier1, agent.Worker, Hard); got != "p/mid" {
			t.Errorf("mode %q hard = %q, want tier1", mode, got)
		}
		if got := cfg.For(agent.Tier2, agent.Probe, Trivial); got != "p/low" {
			t.Errorf("mode %q trivial = %q, want tier2", mode, got)
		}
		if got := cfg.For(agent.Tier1, agent.Worker, Expert); got != "p/top" {
			t.Errorf("mode %q expert = %q, want primary", mode, got)
		}
		want := []string{"p/low", "p/mid", "p/top"}
		if got := cfg.Ladder(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("mode %q ladder = %v, want %v", mode, got, want)
		}
	}
	cfg.Mode = ModeAdvanced
	if got := cfg.For(agent.Tier1, agent.Worker, Hard); got != "p/hardcustom" {
		t.Errorf("advanced hard = %q", got)
	}
	if got := cfg.For(agent.Tier2, agent.Probe, Trivial); got != "p/tinycustom" {
		t.Errorf("advanced trivial = %q", got)
	}
	if len(cfg.Ladder()) <= 3 {
		t.Errorf("advanced ladder should include custom models: %v", cfg.Ladder())
	}
}

func TestNormalizeMode(t *testing.T) {
	for in, want := range map[string]string{"": ModeSimple, "simple": ModeSimple, "ADVANCED": ModeAdvanced, " advanced ": ModeAdvanced, "x": ModeSimple} {
		if got := NormalizeMode(in); got != want {
			t.Errorf("NormalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickDefaultNeverFallsBackToArbitraryModel(t *testing.T) {
	models := []runtime.Model{{Provider: "opencode", ID: "muse-spark-1.3-contributor-free", Name: "Muse Spark"}}
	if got := PickDefault(models, []string{"opus", "sonnet"}); got != "" {
		t.Fatalf("PickDefault with no match = %q, want empty", got)
	}
	if got := PickDefault(models, []string{"muse"}); got != "opencode/muse-spark-1.3-contributor-free" {
		t.Fatalf("PickDefault with match = %q", got)
	}
	if got := PickDefault(nil, []string{"opus"}); got != "" {
		t.Fatalf("PickDefault on empty catalog = %q, want empty", got)
	}
}
