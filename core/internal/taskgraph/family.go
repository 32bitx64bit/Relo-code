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

// idMarker matches the "<token> stage N" or "<token> sN" shape that names one stage of a plan.
var idMarker = regexp.MustCompile(`(?:^| )(?:stage |s)(\d+)([a-z]?)\b`)

// Thresholds for NearCopy. Titles that are close enough on their own match; a
// looser title match also needs a near-identical scope.
const (
	nearTitleJaccard      = 0.6
	nearLooseTitleJaccard = 0.4
	nearScopeJaccard      = 0.8
	containedShare        = 0.8 // Related: share of the shorter title's content words found in the other
)

// NearCopy compares meaningful title content, not a shared stage number alone.
func NearCopy(a, b Task) bool {
	if strings.ToLower(strings.TrimSpace(a.Role)) != strings.ToLower(strings.TrimSpace(b.Role)) {
		return false
	}
	return nearTitle(a, b)
}

// Related reports whether b is the same work as a under another role, id, wording or path list: the family key
// matches, the titles name the same stage, or the titles are near copies whatever the role. It does not look at
// paths or dependencies; SharedPaths does.
func Related(a, b Task) bool {
	if FamilyKey(a) == FamilyKey(b) {
		return true
	}
	if ia, ib := stageID(a.Title), stageID(b.Title); ia != "" && ib != "" && ia != ib {
		return false // S3d and S3e are different batches of one plan
	}
	if ta, tb := familyTitle(a.Title), familyTitle(b.Title); ta != "" && ta == tb {
		return true
	}
	return nearTitle(a, b) || mostlyContained(contentWords(a.Title), contentWords(b.Title))
}

// mostlyContained reports whether one title's content words are almost all in the other's: a reworded title that
// adds a few words ("Investigate widget contract at stage 2") still names the same work. At least two words must
// be shared, so one common noun does not link two tasks.
func mostlyContained(a, b map[string]bool) bool {
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	smaller := min(len(a), len(b))
	return shared >= 2 && float64(shared) >= containedShare*float64(smaller)
}

// SharedPaths returns the declared paths of a that overlap a path of b. A directory overlaps every file below it,
// the same rule the path leases use. Paths are cleaned first, so "./a/../x.go" and "x.go" match.
func SharedPaths(a, b Task) []string {
	var shared []string
	for _, pa := range cleanPaths(a.Paths) {
		for _, pb := range cleanPaths(b.Paths) {
			if pa == pb || strings.HasPrefix(pa, pb+"/") || strings.HasPrefix(pb, pa+"/") {
				shared = append(shared, pa)
				break
			}
		}
	}
	return shared
}

func cleanPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = path.Clean(filepath.ToSlash(strings.TrimSpace(p)))
		if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") {
			continue
		}
		out = append(out, strings.TrimPrefix(p, "/"))
	}
	return out
}

// nearTitle is NearCopy without the role check.
func nearTitle(a, b Task) bool {
	if ia, ib := stageID(a.Title), stageID(b.Title); ia != "" && ib != "" && ia != ib {
		return false
	}
	if normalizeTitle(a.Title) != "" && normalizeTitle(a.Title) == normalizeTitle(b.Title) {
		return true
	}
	ta, tb := contentWords(a.Title), contentWords(b.Title)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	title := jaccard(ta, tb)
	if title > nearTitleJaccard {
		return true
	}
	return title > nearLooseTitleJaccard && jaccard(wordSet(a.Scope), wordSet(b.Scope)) > nearScopeJaccard
}

// stageID includes the sub-id, if present, so S3d and S3e remain distinct. A title can name a parent stage before
// the sub-stage it works on ("SPT stage 3 batch S3a: ..."), so the last marker is the most specific one.
func stageID(title string) string {
	ms := idMarker.FindAllStringSubmatch(normalizeTitle(title), -1)
	if len(ms) == 0 {
		return ""
	}
	m := ms[len(ms)-1]
	return m[1] + m[2]
}

var genericTitleWords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "for": true, "of": true, "to": true,
	"fix": true, "parse": true, "implement": true, "add": true, "update": true, "build": true,
	"refactor": true, "retry": true, "per": true, "stage": true,
}

func contentWords(title string) map[string]bool {
	title = idMarker.ReplaceAllString(normalizeTitle(title), " ")
	words := wordSet(title)
	for w := range words {
		if genericTitleWords[w] {
			delete(words, w)
		}
	}
	return words
}

// wordSet returns the distinct normalized words of s.
func wordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(normalizeTitle(s)) {
		set[w] = true
	}
	return set
}

// jaccard returns |a∩b| / |a∪b|. Two empty sets count as identical.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
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
