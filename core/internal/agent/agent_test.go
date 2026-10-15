package agent

import "testing"

func TestRoleProperties(t *testing.T) {
	tests := []struct {
		role       Role
		tier       Tier
		difficulty string
	}{
		{Primary, TierPrimary, "expert"}, {Head, Tier1, "hard"}, {Probe, Tier2, "easy"},
		{Worker, Tier1, "medium"}, {Architect, Tier1, "hard"}, {Debug, Tier1, "medium"},
		{Integration, Tier1, "medium"}, {Test, Tier1, "easy"}, {Reviewer, Tier1, "easy"},
	}
	for _, tt := range tests {
		if got := TierFor(tt.role); got != tt.tier {
			t.Errorf("TierFor(%q) = %q, want %q", tt.role, got, tt.tier)
		}
		if got := DefaultDifficulty(tt.role); got != tt.difficulty {
			t.Errorf("DefaultDifficulty(%q) = %q, want %q", tt.role, got, tt.difficulty)
		}
	}
}

func TestParseRole(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  Role
		ok    bool
	}{{" WORKER ", Worker, true}, {"reviewer", Reviewer, true}, {"bad-role", "", false}} {
		got, ok := ParseRole(tt.input)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseRole(%q) = %q, %v; want %q, %v", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}
