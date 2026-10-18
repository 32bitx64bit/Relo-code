package style

import (
	"strings"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/routing"
)

type Level string

const (
	LevelOff   Level = "off"
	LevelLite  Level = "lite"
	LevelFull  Level = "full"
	LevelUltra Level = "ultra"
)

func ParseLevel(s string) (Level, bool) {
	l := Level(strings.ToLower(strings.TrimSpace(s)))
	switch l {
	case LevelOff, LevelLite, LevelFull, LevelUltra:
		return l, true
	default:
		return "", false
	}
}

func (l Level) Up() Level {
	switch l {
	case LevelLite:
		return LevelFull
	case LevelFull:
		return LevelUltra
	case LevelUltra:
		return LevelUltra
	default:
		return l
	}
}

func (l Level) Down() Level {
	switch l {
	case LevelUltra:
		return LevelFull
	case LevelFull:
		return LevelLite
	case LevelLite:
		return LevelOff
	default:
		return l
	}
}

type RolePref struct{ Voice, Craft string }

type Prefs struct {
	Preset         string
	Voice, Craft   string
	Roles          map[string]RolePref
	PlanLean       bool
	ReportMaxChars int // 0 means 1200.
}

type Profile struct {
	Voice, Craft Level
	Review       bool
}

func (p Profile) ID() string {
	var parts []string
	if p.Voice != LevelOff {
		parts = append(parts, "v:"+string(p.Voice))
	}
	if p.Craft != LevelOff {
		parts = append(parts, "c:"+string(p.Craft))
	}
	if p.Review {
		parts = append(parts, "r")
	}
	return strings.Join(parts, "|")
}

type Scope struct {
	Budget        int
	MaxItemTokens int
	// Classes are the context engine's class names (ctxeng.Class). A name it does not know matches nothing, so every
	// item of the class it was meant for is dropped from the pack.
	Classes               []string
	Memory                bool
	DependencyReportsOnly bool
	ReadOnly              bool
}

type Resolved struct {
	Profile Profile
	Scope   Scope
}

// Resolve applies modifiers in order: matrix -> preset -> difficulty -> retry -> global overrides -> role overrides -> Primary craft clamp -> review lens -> budget cap.
func Resolve(p Prefs, role agent.Role, difficulty string, attempt int, baseBudget int) Resolved {
	profile, scope := matrix(role)
	preset := strings.ToLower(strings.TrimSpace(p.Preset))
	if preset == "" || preset == "off" {
		// Budget 0 means "caller keeps its default".
		return Resolved{Profile: Profile{Voice: LevelOff, Craft: LevelOff}, Scope: Scope{Budget: baseBudget}}
	}
	if preset != "max" {
		preset = "auto"
	}
	if preset == "max" {
		profile.Voice = profile.Voice.Up()
		if role != agent.Primary {
			scope.Budget = scope.Budget * 3 / 4
		}
	}
	switch strings.ToLower(strings.TrimSpace(difficulty)) {
	case routing.Trivial, routing.Easy:
		profile.Voice = profile.Voice.Up()
		if role != agent.Primary {
			scope.Budget /= 2
		}
	case routing.Expert:
		profile.Voice = capLite(profile.Voice)
		profile.Craft = capLite(profile.Craft)
	}
	if attempt >= 2 {
		profile.Voice = profile.Voice.Down()
	}
	if v := setting(p.Voice); v != "auto" {
		profile.Voice = v
	}
	if c := setting(p.Craft); c != "auto" {
		profile.Craft = c
	}
	if rp, ok := p.Roles[string(role)]; ok {
		if v := setting(rp.Voice); v != "auto" {
			profile.Voice = v
		}
		if c := setting(rp.Craft); c != "auto" {
			profile.Craft = c
		}
	}
	if role == agent.Primary {
		profile.Craft = LevelOff
	}
	if role == agent.Reviewer {
		profile.Review = true
	}
	if baseBudget > 0 && scope.Budget > baseBudget {
		scope.Budget = baseBudget
	}
	return Resolved{Profile: profile, Scope: scope}
}

func setting(s string) Level {
	if strings.EqualFold(strings.TrimSpace(s), "auto") || strings.TrimSpace(s) == "" {
		return "auto"
	}
	if l, ok := ParseLevel(s); ok {
		return l
	}
	return "auto"
}

func capLite(l Level) Level {
	if l == LevelFull || l == LevelUltra {
		return LevelLite
	}
	return l
}

func matrix(role agent.Role) (Profile, Scope) {
	p := Profile{Voice: LevelOff, Craft: LevelOff}
	s := Scope{Budget: 12000, Memory: true}
	switch role {
	case agent.Primary:
		s.Budget = 24000
	case agent.Head:
		p.Voice, s.Budget = LevelLite, 12000
	case agent.Probe:
		p.Voice, s.Budget = LevelUltra, 6000
		s.Memory, s.DependencyReportsOnly, s.ReadOnly = false, true, true
	case agent.Worker, agent.Debug, agent.Integration:
		p.Voice, p.Craft, s.Budget = LevelFull, LevelLite, 16000
		s.Classes, s.Memory, s.DependencyReportsOnly, s.MaxItemTokens = []string{"task_state", "code", "report", "finding"}, false, true, 2000
	case agent.Architect:
		p.Voice, p.Craft, s.Budget = LevelFull, LevelLite, 16000
	case agent.Test:
		p.Voice, s.Budget = LevelUltra, 8000
	case agent.Reviewer:
		p.Voice, p.Review, s.Budget = LevelFull, true, 12000
		s.Classes, s.Memory = []string{"task_state", "finding", "code"}, false
	}
	return p, s
}

func (p Profile) Fragment(role agent.Role) string {
	var parts []string
	craftOn := role != agent.Primary && p.Craft != LevelOff
	if p.Voice != LevelOff {
		if text := voiceFragments[p.Voice]; text != "" {
			parts = append(parts, text)
		}
	}
	if craftOn {
		if text := craftFragments[p.Craft]; text != "" {
			parts = append(parts, text)
		}
	}
	if p.Review {
		parts = append(parts, reviewFragment)
	}
	if p.Voice == LevelOff && (craftOn || p.Review) {
		parts = append(parts, safetyLine)
	}
	return strings.Join(parts, "\n")
}

func PlanLeanLine(p Prefs) string {
	if p.PlanLean {
		return "Merge tasks that need no agent; drop tasks without independent work."
	}
	return ""
}

// Vendor prompt files cost ~1.1k tokens; compact originals preserve net savings.
var voiceFragments = map[Level]string{
	LevelLite:  "Lead with answer. Prefer `[thing] [action] [reason]`; trim articles, filler, pleasantries, and hedging. Keep one idea per sentence, <=20 words. Preserve negation, numbers, and units exactly. Keep code, commands, paths, error strings, and JSON keys verbatim. Never change JSON field names or required fields. Use normal sentences for security warnings, irreversible actions, or confused users.",
	LevelFull:  "Answer first. Use `[thing] [action] [reason]`. Drop articles, filler, pleasantries, and hedging. Prefer fragments; one idea per sentence, <=20 words. Keep negation, numbers, and units exact. Keep code, commands, paths, error strings, and JSON keys verbatim. Never change JSON field names or required fields. Use normal sentences for security warnings, irreversible actions, or confused users.",
	LevelUltra: "Answer first. Fragments; `[thing] [action] [reason]`. Remove articles, filler, pleasantries, hedging. One idea per sentence, <=20 words. Preserve negation, numbers, units exactly. Code, commands, paths, error strings, JSON keys stay verbatim. Never change JSON field names or required fields. Security warnings, irreversible actions, confused users: use normal sentences.",
}

var craftFragments = map[Level]string{
	LevelLite:  "Before adding code: need it? Already present? Stdlib? Native feature? Installed dependency? Then make the smallest change that works and fits the surrounding code. No unrequested abstractions, rewrites, or boilerplate. Keep trust-boundary validation, data-loss-safe error handling, security, and accessibility. Add a small runnable test for non-trivial logic. Name deliberate shortcuts and skipped work in the report's risks.",
	LevelFull:  "Need it? Existing code? Stdlib? Native? Installed dependency? Make the smallest change that works and fits the surrounding code. No unrequested abstractions, rewrites, or boilerplate. Never cut trust-boundary validation, data-loss-safe error handling, security, or accessibility. Non-trivial logic needs one small runnable test. Name deliberate shortcuts and skipped work in the report's risks.",
	LevelUltra: "Need code? Check existing code, stdlib, native feature, installed dependency first. Then the smallest change that works and fits the surrounding code. No unrequested abstractions, rewrites, or boilerplate. Never cut input validation, safe error handling, security, accessibility. Non-trivial logic: one small runnable test. Name shortcuts and skipped work in the report's risks.",
}

const reviewFragment = "Review touched code, not diff alone. Report each bug, security risk, load risk, missing test, and cut candidate with consequence and fix. Prioritize concrete failures over style preferences."
const safetyLine = "Keep JSON fields unchanged. Use normal sentences for security warnings and irreversible actions."
