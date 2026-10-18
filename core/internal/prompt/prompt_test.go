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
	if got := difficulty["enum"].([]string); strings.Join(got, ",") != "trivial,easy,medium,hard,expert" {
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
	for _, role := range roles {
		prompt := For(role)
		t.Logf("role=%s chars=%d", role, len(prompt))
		if total := len(Shared) + len(prompt); total > 5600 {
			t.Errorf("shared + %s prompt too large: %d chars", role, total)
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
		p := For(role)
		if !strings.Contains(p, "not the whole suite") || !strings.Contains(p, "timeout 120") {
			t.Errorf("%s is not told to keep its test runs short", role)
		}
	}
	for _, role := range []agent.Role{agent.Primary, agent.Probe, agent.Reviewer} {
		if strings.Contains(For(role), "not the whole suite") {
			t.Errorf("%s was given a rule about running tests that is not its job", role)
		}
	}
}
