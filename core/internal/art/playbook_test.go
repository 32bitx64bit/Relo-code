package art

import (
	"strings"
	"testing"
)

func TestPlaybookTopicsAndLookup(t *testing.T) {
	want := []string{"cost", "iterate", "overview", "prompts", "refs", "sprites", "views"}
	got := PlaybookTopics()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("topics %v", got)
	}
	if text, ok := Playbook(""); !ok || !strings.Contains(text, "Order of work") {
		t.Fatalf("overview %q", text)
	}
	if text, ok := Playbook(" REFS "); !ok || !strings.Contains(text, "image 1") {
		t.Fatalf("refs %q", text)
	}
	if text, ok := Playbook("nope"); ok || !strings.Contains(text, "Topics:") {
		t.Fatalf("unknown %q", text)
	}
}

// The playbook is the guide agents act on; it must not point them at something that is gone.
func TestPlaybookHasNoLeftoversFromAnotherGenerator(t *testing.T) {
	for _, topic := range PlaybookTopics() {
		text, _ := Playbook(topic)
		for _, stale := range []string{"Midjourney", "Fast time", "--ar", "handoff", "Omni"} {
			if strings.Contains(text, stale) {
				t.Errorf("topic %s still mentions %q", topic, stale)
			}
		}
	}
}

// Every action and parameter the playbook tells an agent to use must exist on the tool.
func TestPlaybookNamesRealActionsAndFields(t *testing.T) {
	var all strings.Builder
	for _, topic := range PlaybookTopics() {
		text, _ := Playbook(topic)
		all.WriteString(text)
	}
	for _, want := range []string{"`refs`", "`generate`", "`wait`", "`relo_sprite`", "`background`", "`quality`", "`size`", "`count`"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("playbook never mentions %s", want)
		}
	}
	for _, gone := range []string{"`vary`", "`upscale`", "`budget`", "`params`"} {
		if strings.Contains(all.String(), gone) {
			t.Errorf("playbook mentions %s, which the tool no longer has", gone)
		}
	}
}
