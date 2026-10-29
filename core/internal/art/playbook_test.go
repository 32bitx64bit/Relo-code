package art

import (
	"strings"
	"testing"
)

func TestPlaybookTopicsAndLookup(t *testing.T) {
	want := []string{"cost", "game-dev uses", "iterate", "jobs", "overview", "prompts", "refs", "sprites", "views"}
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
		for _, stale := range []string{"Midjourney", "Fast time", "handoff", "Omni", "dark grey", "dark gray",
			"/chat/completions", "/images/edits", "/images/generations"} {
			if strings.Contains(text, stale) {
				t.Errorf("topic %s still mentions %q", topic, stale)
			}
		}
	}
}

// The playbook must describe the contract the code accepts: refs with a role and weight, plain-text prompts, the repeat
// cap, the stop on a failed refs job, and sprites on a flat key colour.
func TestPlaybookMatchesArtContract(t *testing.T) {
	text := func(topic string) string {
		s, ok := Playbook(topic)
		if !ok {
			t.Fatalf("no topic %q", topic)
		}
		return s
	}
	checks := []struct{ topic, want string }{
		{"prompts", "--sref"},
		{"prompts", "--cref"},
		{"prompts", "--ar"},
		{"prompts", "--stylize"},
		{"prompts", "--raw"},
		{"prompts", "stylize: 100"},
		{"refs", "{id, role, weight}"},
		{"refs", "`style`"},
		{"refs", "`subject`"},
		{"refs", "`composition`"},
		{"refs", "at most one subject"},
		{"refs", "weight"},
		{"refs", "image 1"},
		{"refs", "style reference"},
		{"refs", "stop"},
		{"refs", "Never run it again without the references"},
		{"refs", "text-only"},
		{"iterate", "twice per session"},
		{"sprites", "#ff00ff"},
		{"sprites", "one object, centred"},
		{"sprites", "no text"},
		{"sprites", "no other objects"},
		{"sprites", "no shadow"},
		{"sprites", "flat solid"},
		{"sprites", "not a copy"},
		{"sprites", "proportions"},
		{"sprites", "not already transparent"},
		{"views", "checker"},
	}
	for _, c := range checks {
		if !strings.Contains(text(c.topic), c.want) {
			t.Errorf("topic %s never mentions %q", c.topic, c.want)
		}
	}
	if s := text("overview"); !strings.Contains(s, "light checker") {
		t.Errorf("overview does not call the sheet a light checker")
	}
	if s := text("sprites"); strings.Contains(s, "transparent` first") && !strings.Contains(s, "`opaque`") {
		t.Errorf("sprites tells agents to ask for transparent without the opaque fallback")
	}
}

// The playbook must tell a text-to-image job from a reference-guided one, say that reference support varies by model and
// that a refused reference means stop, and tell agents to finish a generated sheet with relo_sprite.
func TestPlaybookCoversJobKindsModelLimitsAndSheets(t *testing.T) {
	text := func(topic string) string {
		s, ok := Playbook(topic)
		if !ok {
			t.Fatalf("no topic %q", topic)
		}
		return s
	}
	checks := []struct{ topic, want string }{
		{"overview", "text-to-image"},
		{"overview", "reference-guided"},
		{"jobs", "text-to-image"},
		{"jobs", "reference-guided"},
		{"jobs", "Every job is one model"},
		{"jobs", "vary by model"},
		{"jobs", "stop and report"},
		{"jobs", "not a copy of the picture"},
		{"jobs", "proportions"},
		{"jobs", "palette"},
		{"refs", "varies by model"},
		{"sprites", "`slice`"},
		{"sprites", "`fit`"},
		{"sprites", "`alpha: true`"},
	}
	for _, c := range checks {
		if !strings.Contains(text(c.topic), c.want) {
			t.Errorf("topic %s never mentions %q", c.topic, c.want)
		}
	}
	// The sheet guidance has to name the steps an agent runs, on the tool the agent has.
	for _, want := range []string{"`relo_sprite`", "`size`", "`margin`", "`common`", "`check`", "sheet"} {
		if s := text("sprites"); !strings.Contains(s, want) {
			t.Errorf("sprites never names %q for postprocessing a sheet", want)
		}
	}
}

// The game-dev uses section must cover the asset kinds and say a reference guides rather than copies.
func TestPlaybookCoversGameDevUses(t *testing.T) {
	text, ok := Playbook("game-dev uses")
	if !ok {
		t.Fatalf("no game-dev uses topic: %q", text)
	}
	for _, want := range []string{"Sprites and sheets", "Tilesets", "Textures", "Backgrounds", "UI assets", "Edits and variations",
		"not a copy of the picture", "`refs`", "`relo_sprite`"} {
		if !strings.Contains(text, want) {
			t.Errorf("game-dev uses never mentions %q", want)
		}
	}
}

// The playbook must claim only the checks the code makes: published capabilities are checked before spending where the
// provider publishes them, and otherwise the provider decides. It must also say what happens on a rate limit or a
// timeout, and that a job marks a cost that is only an estimate.
func TestPlaybookClaimsOnlyTheChecksTheCodeMakes(t *testing.T) {
	read := func(topic string) string {
		s, ok := Playbook(topic)
		if !ok {
			t.Fatalf("no topic %q", topic)
		}
		return s
	}
	checks := []struct{ topic, want string }{
		{"overview", "where the provider publishes them"},
		{"overview", "nothing is checked in advance"},
		{"overview", "fails at the provider"},
		{"refs", "before it spends"},
		{"refs", "fails at the provider"},
		{"jobs", "before it spends"},
		{"jobs", "the job goes out and fails at the provider"},
		{"jobs", "rate limit"},
		{"jobs", "timeout"},
		{"jobs", "stop and report"},
		{"jobs", "cost is estimated"},
		{"cost", "(`costFromEstimate`)"},
		{"cost", "still the estimate"},
		{"cost", "timed out"},
	}
	for _, c := range checks {
		if !strings.Contains(read(c.topic), c.want) {
			t.Errorf("topic %s never mentions %q", c.topic, c.want)
		}
	}
	// The old text promised a text-to-image preflight and an unconditional refusal; the code has neither.
	for _, topic := range PlaybookTopics() {
		for _, overclaim := range []string{"can draw from text alone", "Relo starts the job only for a model it has checked",
			"Relo checks what the model supports before it spends"} {
			if strings.Contains(read(topic), overclaim) {
				t.Errorf("topic %s still overclaims: %q", topic, overclaim)
			}
		}
	}
	// Canonicalised whitespace, so a rewrapped paragraph does not hide the overclaim.
	for _, topic := range PlaybookTopics() {
		text := strings.Join(strings.Fields(read(topic)), " ")
		for _, overclaim := range []string{"Relo sends one only when the chosen model's published capabilities list it",
			"sends one only when", "only when the chosen model", "only when the model lists it"} {
			if strings.Contains(strings.ToLower(text), strings.ToLower(overclaim)) {
				t.Errorf("topic %s still overclaims parameter sending: %q", topic, overclaim)
			}
		}
	}
	// Stays endpoint-neutral: the agent never names a provider path.
	for _, topic := range PlaybookTopics() {
		for _, path := range []string{"/images", "/chat", "openrouter.ai", "OpenRouter"} {
			if strings.Contains(read(topic), path) {
				t.Errorf("topic %s names the endpoint %q", topic, path)
			}
		}
	}
}

// Model-dependent parameters must be described as checked against published capabilities, not promised: the playbook
// names aspect_ratio, resolution and seed, points at status for the supported parameters, and says what happens to a
// media type Relo cannot store.
func TestPlaybookDocumentsModelDependentParameters(t *testing.T) {
	read := func(topic string) string {
		s, ok := Playbook(topic)
		if !ok {
			t.Fatalf("no topic %q", topic)
		}
		return s
	}
	checks := []struct{ topic, want string }{
		{"prompts", "`params`"},
		{"prompts", "`aspect_ratio`"},
		{"prompts", "`resolution`"},
		{"prompts", "`seed`"},
		{"prompts", "model-dependent"},
		{"prompts", "`supported_parameters`"},
		{"prompts", "`unknown`"},
		{"prompts", "before anything is spent"},
		{"prompts", "the parameter is sent anyway"},
		{"prompts", "provider decides"},
		{"prompts", "dropped"},
		{"jobs", "`aspect_ratio`"},
		{"jobs", "`resolution`"},
		{"jobs", "`seed`"},
		{"jobs", "model-dependent"},
		{"jobs", "`supported_parameters`"},
		{"jobs", "`unknown`"},
		{"jobs", "unlisted model"},
		{"jobs", "provider decides"},
		{"jobs", "dropped"},
		{"jobs", "media type"},
		{"jobs", "PNG"},
		{"jobs", "WebP"},
	}
	for _, c := range checks {
		if !strings.Contains(read(c.topic), c.want) {
			t.Errorf("topic %s never mentions %q", c.topic, c.want)
		}
	}
	// Stays endpoint-neutral and free of another generator's name, including in the new text.
	for _, topic := range PlaybookTopics() {
		for _, gone := range []string{"/images", "/chat", "openrouter.ai", "OpenRouter", "Midjourney"} {
			if strings.Contains(read(topic), gone) {
				t.Errorf("topic %s names %q", topic, gone)
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
	for _, want := range []string{"`refs`", "`generate`", "`wait`", "`relo_sprite`", "`background`", "`quality`", "`size`", "`count`", "`params`"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("playbook never mentions %s", want)
		}
	}
	for _, gone := range []string{"`vary`", "`upscale`", "`budget`"} {
		if strings.Contains(all.String(), gone) {
			t.Errorf("playbook mentions %s, which the tool no longer has", gone)
		}
	}
}
