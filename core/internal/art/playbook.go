package art

import (
	_ "embed"
	"sort"
	"strings"
)

//go:embed playbook.md
var playbookSource string

// PlaybookTopics are the sections of the playbook.
func PlaybookTopics() []string {
	var names []string
	for _, s := range playbookSections() {
		names = append(names, s.name)
	}
	sort.Strings(names)
	return names
}

type playbookSection struct{ name, text string }

func playbookSections() []playbookSection {
	var out []playbookSection
	var cur *playbookSection
	for _, line := range strings.Split(playbookSource, "\n") {
		if name, ok := strings.CutPrefix(line, "## "); ok {
			out = append(out, playbookSection{name: strings.TrimSpace(name)})
			cur = &out[len(out)-1]
			continue
		}
		if cur != nil {
			cur.text += line + "\n"
		}
	}
	for i := range out {
		out[i].text = strings.TrimSpace(out[i].text)
	}
	return out
}

// Playbook returns the guide: one topic, or the overview when topic is empty. An unknown topic returns the list.
func Playbook(topic string) (string, bool) {
	topic = strings.ToLower(strings.TrimSpace(topic))
	if topic == "" {
		topic = "overview"
	}
	for _, s := range playbookSections() {
		if s.name == topic {
			return s.text, true
		}
	}
	return "No playbook topic " + topic + ". Topics: " + strings.Join(PlaybookTopics(), ", ") + ".", false
}
