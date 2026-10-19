package app

import "testing"

func TestCanvasFlagOnlyTrueEnablesCanvas(t *testing.T) {
	cases := []struct {
		name string
		p    map[string]any
		want bool
	}{
		{"absent", map[string]any{}, false},
		{"top-level true", map[string]any{"canvas": true}, true},
		{"top-level false", map[string]any{"canvas": false}, false},
		{"string true is not trusted", map[string]any{"canvas": "true"}, false},
		{"capabilities true", map[string]any{"capabilities": map[string]any{"canvas": true}}, true},
		{"capabilities false", map[string]any{"capabilities": map[string]any{"canvas": false}}, false},
	}
	for _, tc := range cases {
		if got := canvasFlag(tc.p); got != tc.want {
			t.Errorf("%s: canvasFlag = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCanvasEventsAreNeverPersisted(t *testing.T) {
	for _, name := range []string{"canvas.requested", "canvas.reply"} {
		if !noPersistEvents[name] {
			t.Errorf("%s must be excluded from the event log", name)
		}
	}
}
