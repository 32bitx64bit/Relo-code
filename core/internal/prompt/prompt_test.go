package prompt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
)

func TestPartsPrefixStabilityAcrossTurnsAndRefresh(t *testing.T) {
	gen := ctxeng.Generation{
		Number: 1, Phase: ctxeng.PhaseImplement, Strategy: "variable", Budget: 10000,
		Items: []ctxeng.Item{
			{ID: "objective", Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Objective", Content: strings.Repeat("Stable task constraint. ", 30)},
			{ID: "finding", Class: ctxeng.ClassFinding, Source: ctxeng.SourceVerified, Title: "Finding", Content: "Existing verified implementation fact."},
		},
	}
	var common, total int
	var previous string
	for turn := 0; turn < 30; turn++ {
		parts := ComposeParts(agent.Primary, gen, "User update: "+strings.Repeat("next step ", turn+1))
		current := parts.String()
		if turn > 0 {
			n := longestCommonPrefix(previous, current)
			common += n
			total += len(previous)
		}
		previous = current
	}
	within := float64(common) / float64(total)
	if within < 0.95 {
		t.Fatalf("within-generation cache ratio %.4f", within)
	}
	t.Logf("within-generation cache ratio %.4f", within)

	before := ComposeParts(agent.Primary, gen, "User update after boundary")
	gen.Number = 2
	gen.Reason = "intentional refresh"
	gen.Items = append(gen.Items, ctxeng.Item{ID: "new", Class: ctxeng.ClassFinding, Source: ctxeng.SourceWorker, Title: "New", Content: "Refreshed generation finding."})
	after := ComposeParts(agent.Primary, gen, "User update after boundary")
	if before.Static != after.Static || before.PrefixHash() != after.PrefixHash() {
		t.Fatal("refresh changed cacheable prefix")
	}
	refresh := float64(longestCommonPrefix(before.String(), after.String())) / float64(len(before.String()))
	t.Logf("refresh common-prefix ratio %.4f; L0+L1 retained, L2+L3 refreshed", refresh)
}

func TestComposeCompatibilityAndPrimaryDifficulty(t *testing.T) {
	ctx := "context"
	extra := "extra"
	if got, want := Compose(agent.Worker, ctx, extra), For(agent.Worker)+"\n\n"+ctx+"\n\n"+extra; got != want {
		t.Fatalf("Compose mismatch: %q", got)
	}
	item := PrimarySchema["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)
	difficulty := item["properties"].(map[string]any)["difficulty"].(map[string]any)
	if got := difficulty["enum"].([]string); strings.Join(got, ",") != "trivial,easy,medium,complex,hard,expert" {
		t.Fatalf("difficulty enum = %v", got)
	}
}

func TestComposePartsWithFragments(t *testing.T) {
	gen := ctxeng.Generation{Number: 1, Phase: ctxeng.PhaseImplement, Budget: 1000, Items: []ctxeng.Item{{ID: "x", Class: ctxeng.ClassFinding, Content: "dynamic"}}}
	a := ComposePartsWith(agent.Worker, gen, "volatile", "voice", "craft")
	b := ComposePartsWith(agent.Worker, gen, "volatile", "voice", "craft")
	c := ComposePartsWith(agent.Worker, gen, "volatile", "voice", "other")
	if a.Static != b.Static || a.PrefixHash() != b.PrefixHash() {
		t.Fatal("identical fragments changed static prefix")
	}
	if a.PrefixHash() == c.PrefixHash() || !strings.Contains(a.Static, For(agent.Worker)+"\n\nvoice\n\ncraft") {
		t.Fatal("fragment order or hash incorrect")
	}
	if a.Dynamic != c.Dynamic {
		t.Fatal("static fragments changed dynamic content")
	}
}

func TestTaskIndependentPrefixAndVolatileRetry(t *testing.T) {
	roles := []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Architect, agent.Debug, agent.Worker, agent.Test, agent.Reviewer, agent.Integration}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			first := ctxeng.Generation{Items: []ctxeng.Item{
				{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Task", Content: "first task"},
				{Class: ctxeng.ClassTask, Source: ctxeng.SourceParent, Title: "Previous attempt failed", Content: "retry reason"},
			}}
			second := ctxeng.Generation{Items: []ctxeng.Item{{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Task", Content: "second task"}}}
			a := ComposePartsWith(role, first, "ask", "voice")
			b := ComposePartsWith(role, second, "ask", "voice")
			if a.Static != b.Static || !strings.Contains(a.Stable, "first task") || !strings.Contains(b.Stable, "second task") {
				t.Fatal("task-specific text changed the role prefix or left the stable block")
			}
			contract := "schema contract"
			prefix := contract + a.Static + "\n\n"
			if !strings.HasPrefix(a.System(contract), prefix) || !strings.HasPrefix(b.System(contract), prefix) || strings.Index(a.System(contract), contract) != 0 {
				t.Fatal("schema and role must precede task-specific context")
			}
			if strings.Contains(a.Static+a.Stable, "retry reason") || !strings.Contains(a.Dynamic, "## Current user input and failures\n### Latest failure [parent_report]\nretry reason") {
				t.Fatal("retry note not rendered as volatile with parent provenance")
			}
			if first.Items[1].Title != "Previous attempt failed" {
				t.Fatal("composition mutated generation")
			}
		})
	}
}

func TestSchemaCapsAndRolePromptSizes(t *testing.T) {
	for name, schema := range map[string]any{"primary": PrimarySchema, "report": ReportSchema} {
		if _, err := json.Marshal(schema); err != nil {
			t.Fatalf("%s schema invalid: %v", name, err)
		}
	}
	props := PrimarySchema["properties"].(map[string]any)
	taskProps := props["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if taskProps["title"].(map[string]any)["maxLength"] != PrimaryTitleMaxLength || taskProps["description"].(map[string]any)["maxLength"] != PrimaryDescriptionMaxLength {
		t.Fatal("primary task string caps missing")
	}
	acceptance := taskProps["acceptance"].(map[string]any)
	if acceptance["maxItems"] != PrimaryAcceptanceMaxItems || acceptance["items"].(map[string]any)["maxLength"] != PrimaryAcceptanceMaxLength {
		t.Fatal("primary acceptance caps missing")
	}
	if _, ok := taskProps["paths"].(map[string]any); !ok {
		t.Fatal("primary task schema has no paths field")
	}
	reportProps := ReportSchema["properties"].(map[string]any)
	if reportProps["summary"].(map[string]any)["maxLength"] != ReportSummaryMaxLength {
		t.Fatal("report summary cap missing")
	}
	for _, field := range []string{"findings", "filesChanged", "remainingRisks", "recommendedNextActions"} {
		item := reportProps[field].(map[string]any)
		if item["maxItems"] == nil || item["items"].(map[string]any)["maxLength"] == nil {
			t.Fatalf("report caps missing for %s", field)
		}
	}
	roles := []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Architect, agent.Debug, agent.Worker, agent.Test, agent.Reviewer, agent.Integration}
	// Primary rose from 6067 to 6157 when memoryActions was added (590-byte item 9). Autonomy rules (assume-and-proceed,
	// head answers blocks first, fix obvious in-scope limitations) raised every role; the caps below hold them.
	// 2026-11-02: Primary +225 (item 10: do not push commits and repairs onto the user, believe a missing-file report),
	// Worker +295 and Debug +126 (long measurements run detached instead of under a 120 s cap that made them impossible;
	// a one-line wiring change outside paths is part of the task).
	// 2026-11-03: Primary +480 (it never reads code: the repository map and workspace facts come each turn, relo_agents
	// ask returns a checked answer from a reader, and what a task builds on goes to its assumes for the worker to check).
	before := map[agent.Role]int{agent.Primary: 6950, agent.Head: 1627, agent.Probe: 1829, agent.Architect: 2430, agent.Debug: 2929, agent.Worker: 3467, agent.Test: 1510, agent.Reviewer: 1802, agent.Integration: 2445}
	for _, role := range roles {
		prompt := For(role)
		t.Logf("role=%s bytes before=%d after=%d", role, before[role], len(prompt))
		if len(prompt) > before[role] {
			t.Errorf("%s prompt grew: %d > %d bytes", role, len(prompt), before[role])
		}
		// For already includes Shared; count what is actually sent rather than charging Shared twice.
		if total := len(prompt); total > 7000 {
			t.Errorf("%s prompt too large: %d chars", role, total)
		}
	}
}

func TestPrimaryStaticHasNoModeLeakRule(t *testing.T) {
	const rule = "You are not in a mode. Never mention Plan mode, Build mode, or switching agents; Relo schedules execution. userMessage must describe what Relo will do."
	gen := ctxeng.Generation{Number: 1, Phase: ctxeng.PhaseImplement, Budget: 1000}
	first := ComposeParts(agent.Primary, gen, "")
	if !strings.Contains(first.Static, rule) {
		t.Fatal("Primary static prompt missing mode-leak rule")
	}
	for i := 0; i < 10; i++ {
		again := ComposeParts(agent.Primary, gen, "update")
		if again.Static != first.Static || again.PrefixHash() != first.PrefixHash() {
			t.Fatalf("Primary static prompt not deterministic on call %d", i)
		}
	}
	if !strings.Contains(For(agent.Worker), rule) {
		t.Fatal("shared prompt missing mode-leak rule")
	}
}

func TestPrimaryPromptSplitsWorkWithoutCollisions(t *testing.T) {
	p := For(agent.Primary)
	for _, rule := range []string{
		"one task covers one subsystem and at most about 25 files",
		"explicit file scope in paths",
		"add a dependency only when one needs the other's output",
		"Never let more than one task edit the same shared ledger, changelog, status document or registry file",
		"ONE integration task after the source tasks finish",
		"Architect tasks put the whole design in their report summary",
	} {
		if !strings.Contains(p, rule) {
			t.Errorf("Primary prompt missing planning rule %q", rule)
		}
	}
}

func TestPrimaryPromptRatesTheTaskNotTheFeature(t *testing.T) {
	p := For(agent.Primary)
	for _, rule := range []string{
		"rate what this task's agent must still work out, not the feature or project",
		"When unsure, pick lower",
		"- complex: ",
		"A new feature is not hard by itself",
	} {
		if !strings.Contains(p, rule) {
			t.Errorf("Primary prompt missing difficulty rule %q", rule)
		}
	}
	if strings.Contains(p, "tier-1 for") {
		t.Error("Primary prompt still routes by tier name; difficulty picks the model")
	}
}

func TestPrimaryPromptDefinesCurrentInstructionPrecedence(t *testing.T) {
	for _, rule := range []string{
		"latest user message overrides earlier instructions",
		"not competing active objectives",
		"using Previous assistant reply",
		"decisions list replaces all earlier decisions",
		"interpretations, not verified facts",
	} {
		if !strings.Contains(Primary, rule) {
			t.Errorf("Primary prompt missing state rule %q", rule)
		}
	}
}

func longestCommonPrefix(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func TestWorkersAreToldToRunOnlyTheTestsTheyChanged(t *testing.T) {
	for _, role := range []agent.Role{agent.Worker, agent.Debug} {
		p := strings.ToLower(For(role))
		for _, rule := range []string{"in your own worktree", "one test or target for the touched package or file", "never run the whole suite", "put `timeout` on every command", "command time limit"} {
			if !strings.Contains(p, rule) {
				t.Errorf("%s is not told to run fast scoped tests only: missing %q", role, rule)
			}
		}
		// A blanket two-minute cap made every measurement impossible: ten-year simulations ran past it, agents gave up
		// after 300 steps and failed twice. A measurement runs detached and is read from its log.
		// The command time limit is the Primary's to set per task (commandMinutes), not a fixed number in the prompt.
		for _, rule := range []string{"never skip or shorten a needed run", "detached", "nohup", "tail"} {
			if !strings.Contains(p, rule) {
				t.Errorf("%s is not told how to run a long measurement: missing %q", role, rule)
			}
		}
		if strings.Contains(p, "over two minutes is wrong") || strings.Contains(p, "timeout 120") {
			t.Errorf("%s is again given a fixed two-minute command cap", role)
		}
	}
	for _, role := range []agent.Role{agent.Primary, agent.Probe, agent.Reviewer} {
		if strings.Contains(For(role), "Never run the whole suite") {
			t.Errorf("%s was given a rule about running tests that is not its job", role)
		}
	}
}

func TestAgentsAssumeAndProceedAndFixObviousLimits(t *testing.T) {
	for _, role := range []agent.Role{agent.Worker, agent.Architect, agent.Debug, agent.Integration} {
		p := For(role)
		for _, rule := range []string{"Assume and proceed", "record it as an assumption", "destructive or irreversible actions", "credentials or secrets", "provider/quota/billing"} {
			if !strings.Contains(p, rule) {
				t.Errorf("%s is not told to assume and proceed: missing %q", role, rule)
			}
		}
	}
	if !strings.Contains(For(agent.Worker), "Fix obvious limitations, edge cases and TODOs inside your assigned files and scope") {
		t.Error("worker is not told to fix obvious in-scope limitations")
	}
	if !strings.Contains(Primary, "Answer it yourself first via resolvedBlocks") {
		t.Error("Primary is not told to answer worker blocks itself before the user")
	}
}

func TestPrimaryPlansNoExtraCheckTasks(t *testing.T) {
	for _, rule := range []string{"never add one to re-check facts or a change already described", "Relo runs the full checks once a batch of workers ends", "they do not re-verify", "Every test or review task depends on the workers it checks"} {
		if !strings.Contains(Primary, rule) {
			t.Errorf("Primary prompt missing batching rule %q", rule)
		}
	}
}

func TestCanvasGuidanceCoversSemanticPlanningRules(t *testing.T) {
	for _, rule := range []string{
		"Code-editor-first",
		"Design, prototype, mockup, layout or visual-explore requests normally use Canvas",
		"Explicit output requirements ALWAYS override Canvas",
		"A redesign of the existing app works on the real files",
		"list or open the existing artifact and update it from its revision",
		"iterate at most 3 times",
		"A captured image is not automatically visually verified",
		"create and update are build-mode only",
	} {
		if !strings.Contains(CanvasGuidance, rule) {
			t.Errorf("canvas guidance missing rule %q", rule)
		}
	}
}

func TestPrimarySchemaCanAskForAModelThatSees(t *testing.T) {
	props := PrimarySchema["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	field, ok := props["vision"].(map[string]any)
	if !ok || field["type"] != "boolean" {
		t.Fatalf("task schema has no boolean vision field: %v", props["vision"])
	}
	if d, _ := field["description"].(string); !strings.Contains(d, "model that can see") || !strings.Contains(d, "screenshot") {
		t.Fatalf("vision field does not say when to set it: %q", d)
	}
}

func TestPrimarySchemaOffersTheArtFlagOnlyWhenArtIsOn(t *testing.T) {
	taskProps := func(schema map[string]any) map[string]any {
		return schema["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	}
	if _, ok := taskProps(PrimarySchemaFor(false))["art"]; ok {
		t.Fatal("the art flag is offered while art is off")
	}
	on := PrimarySchemaFor(true)
	field, ok := taskProps(on)["art"].(map[string]any)
	if !ok || field["type"] != "boolean" {
		t.Fatalf("task schema has no boolean art field: %v", taskProps(on)["art"])
	}
	if _, ok := taskProps(on)["vision"]; !ok {
		t.Fatal("the art schema lost the other task fields")
	}
	if _, ok := taskProps(PrimarySchema)["art"]; ok {
		t.Fatal("building the art schema changed the base schema")
	}
	if again := PrimarySchemaFor(true); len(again) != len(on) {
		t.Fatal("the art schema is not stable between calls")
	}
	for _, k := range []string{"required", "type", "additionalProperties"} {
		if (PrimarySchema[k] == nil) != (on[k] == nil) {
			t.Fatalf("the art schema dropped %q", k)
		}
	}
}

// The Primary does not read code. It is told where facts come from, and that what a task builds on and it did not
// measure goes to assumes, which the task's worker checks.
func TestPrimaryIsToldItNeverReadsCodeAndHowToGetFacts(t *testing.T) {
	p := For(agent.Primary)
	for _, want := range []string{"never read code", "Repository map", "action ask", "assumes"} {
		if !strings.Contains(p, want) {
			t.Errorf("Primary prompt lacks %q", want)
		}
	}
	task := taskSchema["properties"].(map[string]any)["assumes"].(map[string]any)
	if task["maxItems"] == nil {
		t.Error("assumes has no cap")
	}
	if _, ok := ReportSchema["properties"].(map[string]any)["deviations"]; !ok {
		t.Error("the worker report has no deviations field")
	}
	if !strings.Contains(Reader, "Cite every claim") || !strings.Contains(Reader, "Never guess") {
		t.Error("the reader prompt does not demand citations")
	}
}
