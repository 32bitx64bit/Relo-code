package taskgraph

import (
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// stageMarker matches the leading "<token> stage <N>" shape of a staged title.
var stageMarker = regexp.MustCompile(`^(.*?) stage (\d+)\b`)

// FamilyKey derives the respawn-loop family of a task. Two tasks that keep
// re-creating the same work - reworded titles, fresh ids - share a key, so a
// dispatcher can cap how often one family runs.
func FamilyKey(t Task) string {
	paths := make([]string, 0, len(t.Paths))
	for _, p := range t.Paths {
		p = path.Clean(filepath.ToSlash(strings.TrimSpace(p)))
		if p == "" || p == "." {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return strings.Join([]string{
		familyTitle(t.Title),
		strings.ToLower(strings.TrimSpace(t.Role)),
		strings.Join(paths, ","),
		strings.ToLower(strings.TrimSpace(t.Scope)),
	}, "|")
}

// familyTitle returns the token before a "stage N" marker plus that stage
// number, so "widget stage 2 per the plan" and "widget stage 2" collapse to one
// family. Titles without a stage marker use their whole normalized form.
func familyTitle(title string) string {
	norm := normalizeTitle(title)
	if m := stageMarker.FindStringSubmatch(norm); m != nil && m[1] != "" {
		return m[1] + ":" + m[2]
	}
	return norm
}

// normalizeTitle lowercases, trims and collapses every run of spaces or
// punctuation into a single space.
func normalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	pendingSpace := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if pendingSpace && b.Len() > 0 {
				b.WriteRune(' ')
			}
			pendingSpace = false
			b.WriteRune(r)
			continue
		}
		pendingSpace = true
	}
	return b.String()
}
