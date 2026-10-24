package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/taskgraph"
)

var fence = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

func ParseJSON[T any](s string) (T, error) {
	var zero T
	s = strings.TrimSpace(s)
	if m := fence.FindStringSubmatch(s); len(m) > 1 {
		s = m[1]
	} else {
		start := strings.IndexByte(s, '{')
		end := strings.LastIndexByte(s, '}')
		if start >= 0 && end > start {
			s = s[start : end+1]
		}
	}
	err := json.Unmarshal([]byte(s), &zero)
	return zero, err
}

type Turn struct {
	Status      string   `json:"status"`
	UserMessage string   `json:"userMessage"`
	Objective   string   `json:"objective"`
	Decisions   []string `json:"decisions"`
	// Constraints are the rules the Primary read out of the request and context for every sub agent to follow. A
	// non-empty list replaces the session's; an empty one leaves it as it was.
	Constraints []string   `json:"constraints"`
	Risks       []string   `json:"risks"`
	RefreshSelf bool       `json:"refreshSelf"`
	Tasks       []TurnTask `json:"tasks"`
	// ResolvedChanges names conflicted changes the head has replaced. Core discards them through the reject path.
	ResolvedChanges []string `json:"resolvedChanges"`
	// ResolvedBlocks answers tasks waiting on a question. Core records each answer on its task before admission;
	// an unknown or already answered taskId is ignored.
	ResolvedBlocks []ResolvedBlock `json:"resolvedBlocks"`
}

type ResolvedBlock struct {
	TaskID string `json:"taskId"`
	Answer string `json:"answer"`
}

type TurnTask struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Role        string   `json:"role"`
	Description string   `json:"description"`
	Acceptance  []string `json:"acceptance"`
	Priority    int      `json:"priority"`
	Deps        []string `json:"deps"`
	Paths       []string `json:"paths"`
	Scope       string   `json:"scope"`
	Difficulty  string   `json:"difficulty"`
	Vision      bool     `json:"vision"`
}

type RawReport struct {
	Status          string       `json:"status"`
	Summary         string       `json:"summary"`
	Findings        []string     `json:"findings"`
	Files           []string     `json:"filesChanged"`
	Verify          []RawVerify  `json:"verification"`
	Risks           []string     `json:"remainingRisks"`
	Next            []string     `json:"recommendedNextActions"`
	Cause           string       `json:"rootCause"`
	Handoff         string       `json:"handoff"`
	Question        string       `json:"question"`
	Structured      []RawFinding `json:"structuredFindings"`
	ValidationError string       `json:"-"`
	unstructured    bool         // a prose reply, not a valid report object; never a valid negative verdict
}

type RawVerify struct {
	Command string `json:"command"`
	Status  string `json:"status"`
	Output  string `json:"output"`
}

type RawFinding struct {
	Kind       string   `json:"kind"`
	Summary    string   `json:"summary"`
	Files      []string `json:"files"`
	Symbols    []string `json:"symbols"`
	Confidence float64  `json:"confidence"`
	Verified   bool     `json:"verified"`
	RootCause  string   `json:"rootCause"`
	Evidence   []string `json:"evidence"`
}

// UnmarshalJSON reads a report field by field. A model that writes one field in an unexpected shape (a verification
// entry as a plain string, a confidence as "high") loses that field, not the whole report and the tokens it cost.
func (r *RawReport) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = RawReport{
		Status: looseText(fields["status"]), Summary: looseText(fields["summary"]), Cause: looseText(fields["rootCause"]),
		Handoff: looseText(fields["handoff"]), Question: looseText(fields["question"]),
		Findings: looseList(fields["findings"]), Files: looseList(fields["filesChanged"]),
		Risks: looseList(fields["remainingRisks"]), Next: looseList(fields["recommendedNextActions"]),
	}
	for _, raw := range looseItems(fields["verification"]) {
		var v RawVerify
		if json.Unmarshal(raw, &v) == nil {
			r.Verify = append(r.Verify, v)
		}
	}
	for _, raw := range looseItems(fields["structuredFindings"]) {
		var f RawFinding
		if json.Unmarshal(raw, &f) == nil {
			r.Structured = append(r.Structured, f)
		}
	}
	return nil
}

// UnmarshalJSON accepts a verification entry as an object, or as the plain string models often write instead
// ("go test ./... passed"), which is kept as the command text.
func (v *RawVerify) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		*v = RawVerify{Command: looseText(data)}
		return nil
	}
	*v = RawVerify{Command: looseText(obj["command"]), Status: looseText(obj["status"]), Output: looseText(obj["output"])}
	return nil
}

// UnmarshalJSON accepts the confidence as a number, a number in a string, or high/medium/low.
func (f *RawFinding) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*f = RawFinding{
		Kind: looseText(obj["kind"]), Summary: looseText(obj["summary"]), RootCause: looseText(obj["rootCause"]),
		Files: looseList(obj["files"]), Symbols: looseList(obj["symbols"]), Evidence: looseList(obj["evidence"]),
	}
	switch conf := strings.ToLower(strings.TrimSpace(looseText(obj["confidence"]))); conf {
	case "high":
		f.Confidence = 0.9
	case "medium":
		f.Confidence = 0.6
	case "low":
		f.Confidence = 0.3
	default:
		f.Confidence, _ = strconv.ParseFloat(conf, 64)
	}
	f.Verified, _ = strconv.ParseBool(strings.TrimSpace(looseText(obj["verified"])))
	return nil
}

// looseItems returns the elements of a JSON array, or the value itself as the only element when it is not an array.
func looseItems(raw json.RawMessage) []json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		return items
	}
	return []json.RawMessage{raw}
}

func looseList(raw json.RawMessage) []string {
	var out []string
	for _, item := range looseItems(raw) {
		if t := looseText(item); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// looseText reads any JSON value as text: a string as itself, an object as its most descriptive field, anything
// else as its JSON.
func looseText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, key := range []string{"summary", "text", "description", "message", "title", "path", "file", "name", "command", "finding", "risk", "action"} {
			if t := looseText(obj[key]); t != "" {
				return t
			}
		}
	}
	return string(raw)
}

func ParseTurn(body string, known func(id string) bool) (Turn, error) {
	var turn Turn
	if err := json.Unmarshal([]byte(strictJSONBody(body)), &turn); err != nil {
		return Turn{}, fmt.Errorf("invalid turn JSON: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strictJSONBody(body)), &fields); err != nil {
		return Turn{}, fmt.Errorf("invalid turn JSON: %w", err)
	}
	var problems []string
	for _, field := range []string{"status", "userMessage", "tasks"} {
		if _, ok := fields[field]; !ok {
			problems = append(problems, fmt.Sprintf("turn is missing required field %q", field))
		}
	}
	if strings.TrimSpace(turn.Status) == "" {
		problems = append(problems, "turn field \"status\" must not be empty")
	}
	if strings.TrimSpace(turn.UserMessage) == "" {
		problems = append(problems, "turn field \"userMessage\" must not be empty")
	}
	if rawTasks := strings.TrimSpace(string(fields["tasks"])); rawTasks != "" && (rawTasks[0] != '[' || rawTasks == "null") {
		problems = append(problems, "turn field \"tasks\" must be an array")
	}
	ids := make(map[string]bool, len(turn.Tasks))
	var taskFields []map[string]json.RawMessage
	if rawTasks := strings.TrimSpace(string(fields["tasks"])); rawTasks != "" && rawTasks != "null" {
		if err := json.Unmarshal([]byte(rawTasks), &taskFields); err != nil {
			problems = append(problems, "turn field \"tasks\" must be an array")
		}
	}
	for i := range turn.Tasks {
		task := &turn.Tasks[i]
		prefix := fmt.Sprintf("tasks[%d]", i)
		if i < len(taskFields) {
			for _, field := range []string{"id", "title", "role"} {
				if _, ok := taskFields[i][field]; !ok {
					problems = append(problems, prefix+fmt.Sprintf(" is missing required field %q", field))
				}
			}
		}
		if strings.TrimSpace(task.ID) == "" {
			problems = append(problems, prefix+" field \"id\" is required")
		} else if ids[task.ID] {
			problems = append(problems, fmt.Sprintf("%s duplicates task id %q", prefix, task.ID))
		} else {
			ids[task.ID] = true
		}
		if strings.TrimSpace(task.Title) == "" {
			problems = append(problems, prefix+" field \"title\" is required")
		}
		role, ok := agent.ParseRole(task.Role)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s has unknown role %q", prefix, task.Role))
		} else {
			task.Role = string(role)
		}
		task.Difficulty = routing.NormalizeDifficulty(task.Difficulty)
		seenPaths := map[string]bool{}
		declared := make([]string, 0, len(task.Paths))
		for _, raw := range task.Paths {
			p := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
			if p == "" {
				problems = append(problems, prefix+` has an empty path`)
				continue
			}
			p = path.Clean(p)
			if p == "." || p == ".." || path.IsAbs(p) || strings.HasPrefix(p, "../") {
				problems = append(problems, fmt.Sprintf("%s has path %q outside the workspace", prefix, raw))
				continue
			}
			if seenPaths[p] {
				continue
			}
			seenPaths[p] = true
			declared = append(declared, p)
		}
		task.Paths = declared
	}
	graphTasks := make([]taskgraph.Task, 0, len(turn.Tasks))
	for _, task := range turn.Tasks {
		graphTasks = append(graphTasks, taskgraph.Task{ID: task.ID, Depends: task.Deps})
	}
	if err := taskgraph.ValidateDependencies(graphTasks, known); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return Turn{}, fmt.Errorf("invalid turn: %s", strings.Join(problems, "; "))
	}
	turn.Constraints = cleanConstraints(turn.Constraints)
	return turn, nil
}

// cleanConstraints trims, drops repeats and keeps what the schema allows. A model that writes more is not worth a
// repair round; what it wrote first is what it thought mattered most.
func cleanConstraints(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		line := strings.Join(strings.Fields(raw), " ")
		if r := []rune(line); len(r) > prompt.PrimaryConstraintMaxLength {
			line = string(r[:prompt.PrimaryConstraintMaxLength-1]) + "…"
		}
		key := strings.ToLower(line)
		if line == "" || seen[key] {
			continue
		}
		seen[key] = true
		if out = append(out, line); len(out) == prompt.PrimaryConstraintsMaxItems {
			break
		}
	}
	return out
}

func ParseReport(body string) (agent.Report, error) {
	var report RawReport
	if err := json.Unmarshal([]byte(strictJSONBody(body)), &report); err != nil {
		return agent.Report{}, fmt.Errorf("invalid report JSON: %w", err)
	}
	// A report that lists findings but forgot the summary still says what was found, and one without a status is
	// a finished report: failing either would cost a repair round for a formality.
	if strings.TrimSpace(report.Summary) == "" && len(report.Findings) > 0 {
		report.Summary = strings.Join(report.Findings, "; ")
	}
	if strings.TrimSpace(report.Summary) == "" && strings.TrimSpace(report.Question) != "" {
		report.Summary = report.Question
	}
	if strings.TrimSpace(report.Summary) == "" {
		return agent.Report{}, fmt.Errorf("invalid report: report field \"summary\" is required and must not be empty")
	}
	if strings.TrimSpace(report.Status) == "" {
		report.Status = string(agent.Completed)
	}
	status := agent.Status(strings.ToLower(strings.TrimSpace(report.Status)))
	var verify []agent.Verify
	for _, item := range report.Verify {
		verify = append(verify, agent.Verify{Command: item.Command, Status: item.Status, Output: item.Output})
	}
	return agent.Report{
		Status: status, Summary: report.Summary, Findings: report.Findings, Files: report.Files, Verify: verify,
		Risks: report.Risks, Next: report.Next, Cause: report.Cause, Handoff: report.Handoff, Question: report.Question, Raw: report.Summary,
	}, nil
}

func strictJSONBody(body string) string {
	body = strings.TrimSpace(body)
	if match := fence.FindStringSubmatch(body); len(match) > 1 {
		return match[1]
	}
	// Models often introduce the object with a sentence ("Here is the plan:"). Take the outermost braces.
	if !strings.HasPrefix(body, "{") {
		if start, end := strings.Index(body, "{"), strings.LastIndex(body, "}"); start >= 0 && end > start {
			return body[start : end+1]
		}
	}
	return body
}

// toolTags are the tags a model writes when it tries to call a tool it was not given.
var toolTags = []string{"function_calls", "invoke", "tool_call", "tool_use", "tool_code"}

// stripToolMarkup removes tool-call markup from a reply and says whether there was any.
//
// A Primary has no tools, and a model that has been taught to end its turn with a call will sometimes try one
// anyway: the whole reply was `<invoke name="noop"></invoke>`, which Relo showed the user as the answer. The markup
// is never an answer, and what is left around it may be.
func stripToolMarkup(s string) (string, bool) {
	had := false
	for {
		start, tag := -1, ""
		for _, name := range toolTags {
			open := "<" + name
			for from := 0; from < len(s); {
				i := strings.Index(s[from:], open)
				if i < 0 {
					break
				}
				i += from
				end := i + len(open)
				// "<invoked>" is not "<invoke".
				if end >= len(s) || strings.ContainsRune(" \t\r\n>/", rune(s[end])) {
					if start < 0 || i < start {
						start, tag = i, name
					}
					break
				}
				from = end
			}
		}
		if start < 0 {
			return strings.TrimSpace(s), had
		}
		had = true
		end := len(s)
		closing := "</" + tag + ">"
		if j := strings.Index(s[start:], closing); j >= 0 {
			end = start + j + len(closing)
		}
		s = s[:start] + s[end:]
	}
}
