package app

import (
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
)

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

// The raw canvas payloads carry artifact source and screenshots, so they stay out of the event log. The
// canvas audit sink stores a metadata-only row under the same event names instead.
func TestCanvasEventsArePersistedAsMetadataOnly(t *testing.T) {
	for _, name := range []string{"canvas.requested", "canvas.reply"} {
		if !noPersistEvents[name] {
			t.Errorf("the %s payload must stay out of the event log", name)
		}
	}
	payload := orchestrator.CanvasAudit{
		Event:      "canvas.requested",
		Action:     "create",
		Role:       "worker",
		Owner:      "agent-1",
		ArtifactID: "art_1",
		RequestID:  "canvas_1",
		Mode:       "build",
		Outcome:    "requested",
		Reason:     "canvas create was refused",
	}.Payload()
	for _, key := range []string{"event", "action", "role", "owner", "artifactId", "requestId", "mode", "outcome", "reason"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("the canvas audit payload is missing %q: %v", key, payload)
		}
	}
	for _, key := range []string{"files", "assets", "source", "entry", "name", "result"} {
		if _, ok := payload[key]; ok {
			t.Errorf("the canvas audit payload must not carry %q: %v", key, payload)
		}
	}
}
