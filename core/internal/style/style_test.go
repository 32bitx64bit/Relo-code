package style

import (
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/routing"
)

func TestParseAndStepLevels(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Level
		ok   bool
	}{
		{"off", LevelOff, true}, {" lite ", LevelLite, true}, {"FULL", LevelFull, true}, {"ultra", LevelUltra, true}, {"auto", "", false}, {"bogus", "", false},
	} {
		got, ok := ParseLevel(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseLevel(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	for _, tc := range []struct{ in, up, down Level }{
		{LevelOff, LevelOff, LevelOff}, {LevelLite, LevelFull, LevelOff}, {LevelFull, LevelUltra, LevelLite}, {LevelUltra, LevelUltra, LevelFull},
	} {
		if tc.in.Up() != tc.up || tc.in.Down() != tc.down {
			t.Errorf("%q steps: up %q down %q", tc.in, tc.in.Up(), tc.in.Down())
		}
	}
}

func TestRoleMatrix(t *testing.T) {
	for _, tc := range []struct {
		role         agent.Role
		voice, craft Level
		budget       int
		readOnly     bool
		memory       bool
	}{
		{agent.Primary, LevelOff, LevelOff, 24000, false, true},
		{agent.Head, LevelLite, LevelOff, 12000, false, true},
		{agent.Probe, LevelUltra, LevelOff, 6000, true, false},
		{agent.Worker, LevelFull, LevelFull, 16000, false, false},
		{agent.Debug, LevelFull, LevelFull, 16000, false, false},
		{agent.Integration, LevelFull, LevelFull, 16000, false, false},
		{agent.Architect, LevelFull, LevelLite, 16000, false, true},
		{agent.Test, LevelUltra, LevelOff, 8000, false, true},
		{agent.Reviewer, LevelFull, LevelOff, 12000, false, false},
	} {
		r := Resolve(Prefs{Preset: "auto"}, tc.role, routing.Medium, 1, 0)
		if r.Profile.Voice != tc.voice || r.Profile.Craft != tc.craft || r.Scope.Budget != tc.budget || r.Scope.ReadOnly != tc.readOnly || r.Scope.Memory != tc.memory {
			t.Errorf("role %q: got %+v %+v", tc.role, r.Profile, r.Scope)
		}
		if tc.role == agent.Reviewer && (!r.Profile.Review || !strings.Contains(r.Profile.Fragment(tc.role), "Review touched code")) {
			t.Errorf("reviewer missing review lens: %+v", r.Profile)
		}
	}
	worker := Resolve(Prefs{Preset: "auto"}, agent.Worker, routing.Medium, 1, 0).Scope
	if worker.MaxItemTokens != 2000 || !worker.DependencyReportsOnly || len(worker.Classes) != 4 {
		t.Errorf("worker scope = %+v", worker)
	}
	hasFinding := false
	for _, cls := range worker.Classes {
		if cls == "finding" {
			hasFinding = true
		}
	}
	if !hasFinding {
		t.Errorf("worker scope drops findings: %+v", worker)
	}
	probe := Resolve(Prefs{Preset: "auto"}, agent.Probe, routing.Medium, 1, 0).Scope
	if !probe.ReadOnly || probe.Memory || !probe.DependencyReportsOnly {
		t.Errorf("probe scope = %+v", probe)
	}
}

func TestModifiers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		role       agent.Role
		difficulty string
		attempt    int
		voice      Level
		craft      Level
		budget     int
	}{
		{"trivial boosts and halves", agent.Worker, routing.Trivial, 1, LevelUltra, LevelFull, 8000},
		{"easy boosts and halves", agent.Worker, routing.Easy, 1, LevelUltra, LevelFull, 8000},
		{"expert caps", agent.Worker, routing.Expert, 1, LevelLite, LevelLite, 16000},
		{"retry downshifts voice", agent.Worker, routing.Medium, 2, LevelLite, LevelFull, 16000},
		{"expert then retry", agent.Worker, routing.Expert, 2, LevelOff, LevelLite, 16000},
		{"max preset", agent.Architect, routing.Medium, 1, LevelUltra, LevelLite, 12000},
		{"base budget caps adjusted", agent.Worker, routing.Easy, 1, LevelUltra, LevelFull, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preset := "auto"
			if tc.name == "max preset" {
				preset = "max"
			}
			base := 0
			if tc.name == "base budget caps adjusted" {
				base = 5000
			}
			r := Resolve(Prefs{Preset: preset}, tc.role, tc.difficulty, tc.attempt, base)
			if r.Profile.Voice != tc.voice || r.Profile.Craft != tc.craft || r.Scope.Budget != tc.budget {
				t.Errorf("got %+v budget %d; want v:%s c:%s budget %d", r.Profile, r.Scope.Budget, tc.voice, tc.craft, tc.budget)
			}
		})
	}
}

func TestPresetsOverridesAndUnknownValues(t *testing.T) {
	for _, preset := range []string{"", "off"} {
		r := Resolve(Prefs{Preset: preset}, agent.Worker, routing.Medium, 1, 777)
		if r.Profile.ID() != "" || r.Profile.Fragment(agent.Worker) != "" || r.Scope.Budget != 777 || r.Scope.Memory || len(r.Scope.Classes) != 0 {
			t.Errorf("preset %q did not disable efficiency: %+v %+v", preset, r.Profile, r.Scope)
		}
	}
	r := Resolve(Prefs{Preset: "auto", Voice: "lite", Craft: "ultra", Roles: map[string]RolePref{"worker": {Voice: "full"}}}, agent.Worker, routing.Medium, 1, 0)
	if r.Profile.Voice != LevelFull || r.Profile.Craft != LevelUltra {
		t.Errorf("overrides = %+v", r.Profile)
	}
	r = Resolve(Prefs{Preset: "auto", Voice: "unknown", Craft: "bad", Roles: map[string]RolePref{"worker": {Voice: "bad"}}}, agent.Worker, routing.Medium, 1, 0)
	if r.Profile.Voice != LevelFull || r.Profile.Craft != LevelFull {
		t.Errorf("unknown settings should inherit matrix: %+v", r.Profile)
	}
	r = Resolve(Prefs{Preset: "typo"}, agent.Worker, routing.Medium, 1, 0)
	if r.Profile.Voice != LevelFull || r.Scope.Budget != 16000 {
		t.Errorf("unknown preset should use auto: %+v %+v", r.Profile, r.Scope)
	}
	r = Resolve(Prefs{Preset: "auto", Voice: "ultra", Craft: "ultra"}, agent.Primary, routing.Medium, 1, 0)
	if r.Profile.Craft != LevelOff {
		t.Errorf("primary received craft: %+v", r.Profile)
	}
	if PlanLeanLine(Prefs{}) != "" || PlanLeanLine(Prefs{PlanLean: true}) == "" {
		t.Fatal("PlanLeanLine does not follow setting")
	}
}

func TestProfileIDsAndFragments(t *testing.T) {
	if got := (Profile{Voice: LevelFull, Craft: LevelLite, Review: true}).ID(); got != "v:full|c:lite|r" {
		t.Fatalf("ID = %q", got)
	}
	if got := (Profile{Voice: LevelOff, Craft: LevelOff}).ID(); got != "" {
		t.Fatalf("off ID = %q", got)
	}
	roles := []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Worker, agent.Reviewer, agent.Architect, agent.Debug, agent.Test, agent.Integration}
	for _, role := range roles {
		for _, profile := range []Profile{{Voice: LevelFull, Craft: LevelFull}, {Voice: LevelUltra, Craft: LevelUltra, Review: role == agent.Reviewer}} {
			first := profile.Fragment(role)
			if first != profile.Fragment(role) {
				t.Errorf("fragment differs for role %s", role)
			}
			if role == agent.Primary && strings.Contains(first, "Need it?") {
				t.Errorf("primary received craft: %s", first)
			}
			t.Logf("role=%s profile=%s tokens=%d", role, profile.ID(), ctxeng.Counter(first))
		}
	}
	for _, level := range []Level{LevelLite, LevelFull, LevelUltra} {
		if n := ctxeng.Counter(voiceFragments[level]); n > 140 {
			t.Errorf("voice %s = %d tokens", level, n)
		}
		if n := ctxeng.Counter(craftFragments[level]); n > 160 {
			t.Errorf("craft %s = %d tokens", level, n)
		}
	}
	if n := ctxeng.Counter(reviewFragment); n > 170 {
		t.Errorf("review = %d tokens", n)
	}
	for _, role := range roles {
		for _, v := range []Level{LevelLite, LevelFull, LevelUltra} {
			for _, c := range []Level{LevelOff, LevelLite, LevelFull, LevelUltra} {
				p := Profile{Voice: v, Craft: c, Review: role == agent.Reviewer}
				if n := ctxeng.Counter(p.Fragment(role)); n > 330 {
					t.Errorf("role %s v:%s c:%s = %d tokens", role, v, c, n)
				}
			}
		}
	}
}

func TestSafetyClauseOnce(t *testing.T) {
	roles := []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Worker, agent.Reviewer, agent.Architect, agent.Debug, agent.Test, agent.Integration}
	const marker = "irreversible actions"
	for _, role := range roles {
		for _, v := range []Level{LevelOff, LevelLite, LevelFull, LevelUltra} {
			for _, c := range []Level{LevelOff, LevelLite, LevelFull, LevelUltra} {
				for _, review := range []bool{false, true} {
					p := Profile{Voice: v, Craft: c, Review: review}
					out := p.Fragment(role)
					if out == "" {
						continue
					}
					if got := strings.Count(out, marker); got != 1 {
						t.Errorf("role=%s v=%s c=%s review=%v count=%d output=%q", role, v, c, review, got, out)
					}
				}
			}
		}
	}
}

func TestFragmentJoinsWithRealNewlines(t *testing.T) {
	out := Profile{Voice: LevelFull, Craft: LevelFull}.Fragment(agent.Worker)
	if strings.Contains(out, `\n`) || strings.Count(out, "\n") != 1 {
		t.Fatalf("want one real newline, no literal backslash-n: %q", out)
	}
}
