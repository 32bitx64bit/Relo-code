// Package vision knows which models can see images and prepares image files for one to look at.
//
// It imports nothing else from Relo, so the provider, app, orchestrator and agentmsg packages can all use it.
package vision

import (
	"regexp"
	"strings"
)

// A model's image support. Unknown is the zero value: nothing said either way.
const (
	Unknown = ""
	Yes     = "yes"
	No      = "no"
)

// Where an answer came from, strongest first. Configured is what the user (or discovery) wrote into the provider's
// model list; Catalog is OpenCode's built-in model data; Name is a guess from the model id.
const (
	SourceConfigured = "configured"
	SourceCatalog    = "catalog"
	SourceName       = "name"
)

// FromBool turns a declared flag into a status: nil stays unknown.
func FromBool(b *bool) string {
	switch {
	case b == nil:
		return Unknown
	case *b:
		return Yes
	default:
		return No
	}
}

// FromInputs reads a modality list such as ["text","image"]. An empty list says nothing; a list without image says no.
func FromInputs(inputs []string) string {
	if len(inputs) == 0 {
		return Unknown
	}
	for _, in := range inputs {
		if strings.EqualFold(strings.TrimSpace(in), "image") {
			return Yes
		}
	}
	return No
}

var (
	// Families whose models have all taken images since the first release Relo is likely to meet.
	seesRe = regexp.MustCompile(`claude-(3|opus|sonnet|haiku|fable|[4-9])|gpt-4o|gpt-4\.1|gpt-4-turbo|gpt-[5-9]|chatgpt-4o|(^|[/:_-])o[134]($|[/:_-])|gemini|pixtral|llava|internvl|minicpm-v|glm-\d+(\.\d+)?v|qvq`)
	// Words that name a vision model whatever its family.
	seesWordRe = regexp.MustCompile(`(^|[/:._-])(vl|vlm|vision|omni)($|[/:._\d-])`)
	// Families that are text only, unless a vision word above says otherwise.
	blindRe = regexp.MustCompile(`deepseek|gpt-oss|gpt-3\.5|codestral|qwen[\d.]*-coder|claude-(2|instant)`)
	// Small o-series models that never took images.
	blindMiniRe = regexp.MustCompile(`(^|[/:_-])o[13]-mini`)
)

// Guess says whether a model id names a model that takes images, from the id alone. It answers only for families it
// is confident about and returns Unknown for the rest; a configured or catalog answer always beats it.
func Guess(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return Unknown
	}
	if seesWordRe.MatchString(id) || strings.Contains(id, "deepseek-ocr") {
		return Yes
	}
	if blindRe.MatchString(id) || blindMiniRe.MatchString(id) {
		return No
	}
	if seesRe.MatchString(id) {
		return Yes
	}
	return Unknown
}

// Resolve picks the strongest answer: configured, then catalog, then the name guess.
func Resolve(configured *bool, catalog, id string) (status, source string) {
	if s := FromBool(configured); s != Unknown {
		return s, SourceConfigured
	}
	if catalog == Yes || catalog == No {
		return catalog, SourceCatalog
	}
	if s := Guess(id); s != Unknown {
		return s, SourceName
	}
	return Unknown, ""
}
