package runtime

import "testing"

func TestPromptTokens(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage Usage
		want  int
	}{
		{"empty", Usage{}, 0},
		{"uncached", Usage{Input: 500, Output: 249, Reasoning: 10}, 500},
		{"cache read and write", Usage{Input: 500, Output: 249, CacheRead: 8_000, CacheWrite: 100}, 8_600},
		{"Claude native usage behind router row 20438", Usage{Input: 2, Output: 8_034, CacheRead: 66_457, CacheWrite: 2_685}, 69_144},
		{"router row 20438 client wire omits cache", Usage{Input: 71_144, Output: 8_034}, 71_144},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.PromptTokens(); got != tc.want {
				t.Fatalf("prompt tokens = %d, want %d", got, tc.want)
			}
		})
	}
}
