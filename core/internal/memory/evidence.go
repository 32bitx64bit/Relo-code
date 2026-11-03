package memory

import (
	"regexp"
	"time"
	"unicode/utf8"
)

// Evidence and provenance shapes (design §3, §7). Every field is bounded and passes the same unsafe scan as titles.
// Evidence references are created by trusted runtime adapters; this package only checks their shape and that a
// promotion cites references that were recorded on the candidate.

// Evidence types (design §3).
const (
	EvidenceFileObservation  = "file_observation"
	EvidenceCommandResult    = "command_result"
	EvidenceUserDecision     = "user_decision"
	EvidenceAcceptedArtifact = "accepted_artifact"
)

// Evidence outcomes.
const (
	ResultPassed   = "passed"
	ResultFailed   = "failed"
	ResultObserved = "observed"
	ResultAbsent   = "absent"
)

// Bounds for evidence and the other new fields. Values over a bound are rejected, never truncated.
const (
	MaxEvidence     = 8
	MaxArgv         = 16
	MaxArgLen       = 200
	MaxEnvironment  = 200
	MaxTags         = 8
	MaxTagLen       = 40
	MaxEvidenceFile = 8
	// CommandEvidenceTTL bounds how long a command_result can justify a promotion (design §5: command facts expire).
	CommandEvidenceTTL = 30 * 24 * time.Hour
)

// Command is the argv a command fact records. Cwd is a workspace-relative path or ".". Environment is a bounded,
// non-secret description, never variable values.
type Command struct {
	Argv        []string `json:"argv"`
	Cwd         string   `json:"cwd"`
	Environment string   `json:"environment,omitempty"`
}

// Scope carries the non-path scope metadata. Paths and symbols stay in Entry.Files and Entry.Symbols.
type Scope struct {
	Tags []string `json:"tags,omitempty"`
}

// EvidenceFile is one workspace-relative path with the SHA-256 of the bytes that were inspected.
type EvidenceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	// Absent marks a verified absence: the path did not exist when observed. It carries no SHA256. Design §5 gives no
	// field for this marker; this is the minimal shape, and an existing file later contradicts it.
	Absent bool `json:"absent,omitempty"`
}

// EvidenceAnchor is the commit and file state a runtime adapter verified for one cited reference at promotion. Core
// stamps it onto that reference only after checking it matches what the candidate recorded.
type EvidenceAnchor struct {
	Commit string         `json:"commit,omitempty"`
	Files  []EvidenceFile `json:"files,omitempty"`
}

// Evidence is a reference to one durable local observation, plus sanitized metadata. It never holds transcripts,
// output snippets or environment values.
type Evidence struct {
	Type       string         `json:"type"`
	SessionID  string         `json:"sessionId,omitempty"`
	AgentID    string         `json:"agentId,omitempty"`
	Ref        string         `json:"ref"`
	ObservedAt time.Time      `json:"observedAt"`
	Commit     string         `json:"commit,omitempty"`
	Files      []EvidenceFile `json:"files,omitempty"`
	// Command is the check command a command_result records, as the check record states it. Core resolves the Ref
	// against that record and requires the same command, so a ref cannot stand for another command's check.
	Command string `json:"command,omitempty"`
	Result  string `json:"result"`
}

// Promotion records the head decision that made a candidate permanent. Core fills it; callers never supply it.
type Promotion struct {
	HeadTurnID string    `json:"headTurnId"`
	Rationale  string    `json:"rationale"`
	PromotedAt time.Time `json:"promotedAt"`
}

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tagPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.\-]{0,39}$`)
)

// validateEvidence checks the shape, bounds and outcome rules of every reference. A file_observation must carry
// hashes; a command_result must be passed or failed; user_decision and accepted_artifact are observed only.
func validateEvidence(evs []Evidence) error {
	if len(evs) > MaxEvidence {
		return ErrInvalid
	}
	for _, ev := range evs {
		if !safeIDPattern.MatchString(ev.Ref) {
			return ErrInvalid
		}
		if ev.SessionID != "" && !safeIDPattern.MatchString(ev.SessionID) {
			return ErrInvalid
		}
		if ev.AgentID != "" && !safeIDPattern.MatchString(ev.AgentID) {
			return ErrInvalid
		}
		if ev.ObservedAt.IsZero() {
			return ErrInvalid
		}
		if ev.Commit != "" && !commitPattern.MatchString(ev.Commit) {
			return ErrInvalid
		}
		if err := checkEvidenceOutcome(ev); err != nil {
			return err
		}
		if ev.Command != "" {
			if ev.Type != EvidenceCommandResult || !ValidEvidenceCommand(ev.Command) {
				return ErrInvalid
			}
			if unsafe(ev.Command) {
				return ErrUnsafe
			}
		}
		if len(ev.Files) > MaxEvidenceFile {
			return ErrInvalid
		}
		for _, f := range ev.Files {
			if !validPath(f.Path) {
				return ErrInvalid
			}
			if unsafe(f.Path) {
				return ErrUnsafe
			}
			if f.SHA256 != "" && !sha256Pattern.MatchString(f.SHA256) {
				return ErrInvalid
			}
			if f.Absent && f.SHA256 != "" {
				return ErrInvalid
			}
			if ev.Type == EvidenceFileObservation && f.SHA256 == "" && !f.Absent {
				return ErrInvalid
			}
		}
	}
	return nil
}

// ValidEvidencePath reports whether p is a workspace-relative path the memory store accepts and that holds no unsafe
// text. Runtime adapters use it to refuse evidence files before they are recorded or resolved.
func ValidEvidencePath(p string) bool {
	return validPath(p) && !unsafe(p)
}

// ValidEvidenceCommand reports whether s fits a recorded check command: non-empty, within MaxArgLen runes, and free of
// unsafe text.
func ValidEvidenceCommand(s string) bool {
	n := utf8.RuneCountInString(s)
	return n > 0 && n <= MaxArgLen && !unsafe(s)
}

func checkEvidenceOutcome(ev Evidence) error {
	switch ev.Type {
	case EvidenceFileObservation:
		if ev.Result != ResultObserved && ev.Result != ResultAbsent {
			return ErrInvalid
		}
		if len(ev.Files) == 0 {
			return ErrInvalid
		}
	case EvidenceCommandResult:
		if ev.Result != ResultPassed && ev.Result != ResultFailed {
			return ErrInvalid
		}
	case EvidenceUserDecision, EvidenceAcceptedArtifact:
		if ev.Result != ResultObserved {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// validateCommand bounds the argv and cwd and scans every argument: an argument that may carry a credential is
// rejected, never stored.
func validateCommand(c *Command) error {
	if c == nil {
		return nil
	}
	if len(c.Argv) == 0 || len(c.Argv) > MaxArgv {
		return ErrInvalid
	}
	for _, a := range c.Argv {
		if n := utf8.RuneCountInString(a); n == 0 || n > MaxArgLen {
			return ErrInvalid
		}
		if unsafe(a) {
			return ErrUnsafe
		}
	}
	if c.Cwd != "." && !validPath(c.Cwd) {
		return ErrInvalid
	}
	if utf8.RuneCountInString(c.Environment) > MaxEnvironment {
		return ErrInvalid
	}
	if unsafe(c.Environment) {
		return ErrUnsafe
	}
	return nil
}

// validateScope bounds the tags. Tags are lowercase identifiers only.
func validateScope(s *Scope) error {
	if s == nil {
		return nil
	}
	if len(s.Tags) > MaxTags {
		return ErrInvalid
	}
	for _, t := range s.Tags {
		if !tagPattern.MatchString(t) || utf8.RuneCountInString(t) > MaxTagLen {
			return ErrInvalid
		}
	}
	return nil
}

// validateDecisionText checks the head rationale and turn identity of a promotion.
func validateDecisionText(headTurnID, rationale string) error {
	if !safeIDPattern.MatchString(headTurnID) {
		return ErrInvalid
	}
	if n := utf8.RuneCountInString(rationale); n == 0 || n > MaxBody {
		return ErrInvalid
	}
	if unsafe(rationale) {
		return ErrUnsafe
	}
	return nil
}
