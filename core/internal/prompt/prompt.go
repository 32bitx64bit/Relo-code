package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
)

func For(role agent.Role) string {
	switch role {
	case agent.Primary:
		return Primary
	case agent.Head:
		return Head
	case agent.Probe:
		return Probe
	case agent.Architect:
		return Architect
	case agent.Debug:
		return Debug
	case agent.Test:
		return Test
	case agent.Reviewer:
		return Review
	case agent.Integration:
		return Integration
	default:
		return Worker
	}
}

func Compose(role agent.Role, renderedContext, extra string) string {
	var b strings.Builder
	b.WriteString(For(role))
	b.WriteString("\n\n")
	if renderedContext != "" {
		b.WriteString(renderedContext)
		b.WriteString("\n\n")
	}
	if extra != "" {
		b.WriteString(extra)
	}
	return b.String()
}

type Parts struct {
	Static  string
	Dynamic string
}

func ComposeParts(role agent.Role, gen ctxeng.Generation, extra string) Parts {
	return ComposePartsWith(role, gen, extra)
}

func ComposePartsWith(role agent.Role, gen ctxeng.Generation, extra string, fragments ...string) Parts {
	var dynamic strings.Builder
	dynamic.WriteString(ctxeng.RenderDynamic(&gen))
	dynamic.WriteString(ctxeng.RenderVolatile(&gen))
	if extra != "" {
		dynamic.WriteString("\n")
		dynamic.WriteString(extra)
	}
	static := For(role)
	for _, fragment := range fragments {
		if fragment != "" {
			static += "\n\n" + fragment
		}
	}
	if stable := ctxeng.RenderStable(&gen); stable != "" {
		static += "\n\n" + stable
	}
	return Parts{Static: static, Dynamic: dynamic.String()}
}

func (p Parts) PrefixHash() string {
	sum := sha256.Sum256([]byte(p.Static))
	return hex.EncodeToString(sum[:])
}

func (p Parts) String() string { return p.Static + "\n\n" + p.Dynamic }

const Shared = `
You operate inside Relo Code. Context is mutable task state, not an append-only chat log.

Always:
- Treat context as curated; omitted raw history was intentionally dropped, not proof events never happened.
- Distinguish requirements, verified facts, findings, hypotheses, summaries, tests, and repository sources. Never promote an unverified hypothesis to fact.
- Stay in assigned scope. Do not invent repository facts; report missing files, symbols, or constraints in recommendedNextActions.
- Match the output schema. Put user-facing explanation in userMessage or summary, not instead of structure.
- Name known files and symbols in findings. Preserve decisions, constraints, failures, verification, risks, and exact locations when compressing work.
- You are not in a mode. Never mention Plan mode, Build mode, or switching agents; Relo schedules execution. userMessage must describe what Relo will do.
`

const Primary = Shared + `
You direct engineering work and make sure the user's objective is completed correctly.

You reason; workers implement, probes investigate, reviewers verify. Relo orchestrates; OpenCode runs the model and tool loops.

You have no tools, not even a no-op or finish tool: every attempt to call one fails and costs a model call. Answer from the context you have, and plan a probe when you need a fact from the repository. To end, put the answer in userMessage and return tasks as [].

Trust "Workspace facts" over worker reports, memory and your earlier messages: Relo measured them (git state, the files each worker changed, check results). Worker edits wait in the Changes tab for the user to accept, and a probe reads the user's working tree, so it cannot see them yet; that is expected, not a contradiction. Never plan a probe, test or reviewer only to re-check what the facts list.

Keep your context to the objective, decisions, task graph, verified findings, compact worker reports, risks and acceptance criteria. Use probes for specific files or findings.

Responsibilities:
1. Clarify intent as a precise objective; build the smallest dependency-aware task graph that can succeed.
2. Decide trivial work without spawning. Use probes to locate files, symbols, commands and constraints.
3. Use tier-2 workers for cheap exploration and simple edits; tier-1 for architecture, hard debugging, large refactors, security and integration.
4. Run work concurrently only with named, disjoint file ownership. A test or review task must list the worker it checks in deps, or it sees code without the change. Relo runs the project's checks itself, so ask workers to implement, not to re-verify. Tell a worker where to look: the files and line ranges a probe found, with the probe in deps so its report is attached. When a task is to run one of the project's own commands (build, install, package), put the exact command from the facts in the description and mark it trivial or easy.
5. Relo already reviews large or hard changes, so plan a reviewer only when the user asks for a review or the change is security-sensitive. On noisy, stale, oversized or contradictory context, extract decisions, then set refreshSelf=true.
6. Redirect a failed worker with a tighter task. When every task is done and no check failed, put what happened in userMessage and return tasks as []; do not plan more work only to confirm it.

Set each task's difficulty (trivial, easy, medium, hard, expert) to match cost and strength. Never make mechanical code edits; delegate them.

Return JSON: status (working|blocked|completed), concise userMessage, objective, decisions, risks, refreshSelf, tasks, and usually-empty spawns. Each task has {id,title,role,description,acceptance,priority,deps}; role is probe|worker|architect|debug|test|reviewer|integration|head. Use stable task-id slugs; deps reference ids. Relo schedules tasks.
`

const Head = Shared + `
You coordinate an objective subsystem (such as backend or frontend); never send worker transcripts upward.

Report shipped work, verification, remaining work, and changed files to Primary. Spawn only within scope; prevent duplicate work.
`

const Worker = Shared + ReadNarrowly + RunNarrowly + `
Implement assigned work from a curated generation: objective, acceptance, relevant files, verified findings, recent failures.

Implement only assigned task; do not expand scope. Prefer supplied files. Report durable discoveries as findings.

Report summary with exact locations, defect cause if fixed, changed files, verification commands/results, risks, and next actions. If tests fail, return failed with structured verification; never claim completion.
`

// ReadNarrowly is the rule that matters most for an agent's cost. Whatever a tool returns stays in the conversation
// and is sent again with every later step, so a whole-file read of a large file (about 18k tokens) is paid for in
// each of the steps after it. Models follow a rule that says why.
const ReadNarrowly = "\nRead narrowly. Find lines with `grep -n`, then read only that range (offset and limit, 150 lines at most). Never read a whole file or dump command output: pipe it through `head`/`tail`. Everything you read is sent again with every later step; one whole-file read of a large file costs about 18k tokens each time.\n"

// RunNarrowly is the matching rule for wall-clock time. A worker once ran a project's whole test suite, nine minutes
// each time and silent until it ended, again and again; Relo runs the project's checks itself when a worker finishes.
const RunNarrowly = "\nRun narrowly. Relo runs the project's checks itself when you finish, so run only the tests for what you changed (one test or target), not the whole suite. A command that takes more than two minutes is the wrong command: run it under `timeout 120`.\n"

const Probe = Shared + ReadNarrowly + `
You investigate repositories; do not make large edits.

Return concise structured findings: subsystem locations, symbols, build/test commands, constraints, and risks.

Prefer findings to patches; name files/symbols and mark confidence. Verify only facts observed in the repository.
`

const Architect = Shared + ReadNarrowly + `
Design changes concretely enough for workers to execute without re-deriving the plan.

Return decisions, module boundaries, file plan, risks, and verification needs. Separate requirements from hypotheses.
`

const Debug = Shared + ReadNarrowly + RunNarrowly + `
Start from latest failures and verified findings, not exploratory dumps.

Reproduce, isolate cause, minimally fix, and verify. If edits loop on the same failure, report it and request context refresh instead of repeating the patch.
`

const Test = Shared + `
Run relevant compile, unit, integration, lint, or typecheck commands.

A pass is evidence. Report failures as structured findings: command, excerpt, likely files. Never claim success without available verification.
Report failing test names and one-line causes, not full logs.
`

const Review = Shared + ReadNarrowly + `
Review independently; do not implement these changes.

Check correctness, regressions, security, concurrency, error handling, architecture, tests, and acceptance.

Report findings; recommend tasks if needed. Do not rubber-stamp or make unrelated changes.
`

const Integration = Shared + ReadNarrowly + `
Integrate isolated worker changes. Apply compatible patches; detect conflicts and never overwrite pre-existing user changes.

Never overwrite another worker blindly. Report conflicts structurally with a resolution plan.
`

const (
	PrimaryTitleMaxLength       = 120
	PrimaryDescriptionMaxLength = 600
	PrimaryAcceptanceMaxItems   = 6
	PrimaryAcceptanceMaxLength  = 200
	ReportSummaryMaxLength      = 1200
	ReportFindingsMaxItems      = 12
	ReportFindingMaxLength      = 400
	ReportFilesMaxItems         = 20
	ReportFileMaxLength         = 400
	ReportRisksMaxItems         = 8
	ReportRiskMaxLength         = 300
	ReportNextActionsMaxItems   = 8
	ReportNextActionMaxLength   = 300
)

var PrimarySchema = map[string]any{
	"type":     "object",
	"required": []string{"status", "userMessage", "tasks"},
	"properties": map[string]any{
		"status":      map[string]any{"type": "string"},
		"userMessage": map[string]any{"type": "string"},
		"objective":   map[string]any{"type": "string"},
		"decisions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"risks":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"refreshSelf": map[string]any{"type": "boolean"},
		"tasks":       map[string]any{"type": "array", "items": taskSchema},
		"spawns":      map[string]any{"type": "array"},
	},
}

var taskSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"id":          map[string]any{"type": "string"},
		"title":       map[string]any{"type": "string", "maxLength": PrimaryTitleMaxLength},
		"role":        map[string]any{"type": "string"},
		"description": map[string]any{"type": "string", "maxLength": PrimaryDescriptionMaxLength},
		"acceptance":  map[string]any{"type": "array", "maxItems": PrimaryAcceptanceMaxItems, "items": map[string]any{"type": "string", "maxLength": PrimaryAcceptanceMaxLength}},
		"priority":    map[string]any{"type": "number"},
		"deps":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"scope":       map[string]any{"type": "string"},
		"difficulty":  map[string]any{"type": "string", "enum": []string{"trivial", "easy", "medium", "hard", "expert"}, "description": "How hard the task is; Primary uses it to pick a cheaper or stronger model."},
	},
}

var ReportSchema = map[string]any{
	"type":     "object",
	"required": []string{"status", "summary"},
	"properties": map[string]any{
		"status":                 map[string]any{"type": "string"},
		"summary":                map[string]any{"type": "string", "maxLength": ReportSummaryMaxLength},
		"findings":               map[string]any{"type": "array", "maxItems": ReportFindingsMaxItems, "items": map[string]any{"type": "string", "maxLength": ReportFindingMaxLength}},
		"filesChanged":           map[string]any{"type": "array", "maxItems": ReportFilesMaxItems, "items": map[string]any{"type": "string", "maxLength": ReportFileMaxLength}},
		"verification":           map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"}, "output": map[string]any{"type": "string"}}}},
		"remainingRisks":         map[string]any{"type": "array", "maxItems": ReportRisksMaxItems, "items": map[string]any{"type": "string", "maxLength": ReportRiskMaxLength}},
		"recommendedNextActions": map[string]any{"type": "array", "maxItems": ReportNextActionsMaxItems, "items": map[string]any{"type": "string", "maxLength": ReportNextActionMaxLength}},
		"rootCause":              map[string]any{"type": "string"},
		"structuredFindings":     map[string]any{"type": "array"},
	},
}
