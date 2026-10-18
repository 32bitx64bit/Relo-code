package orchestrator

import (
	"strings"
	"testing"
)

func TestParseTurn(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"valid", `{"status":"working","userMessage":"Implement","tasks":[{"id":"a","title":"Inspect","role":" WORKER ","difficulty":" HARD "}]}`, ""},
		{"fenced", "```json\n{\"status\":\"working\",\"userMessage\":\"Plan\",\"tasks\":[]}\n```", ""},
		{"malformed", `{broken`, "invalid turn JSON"},
		{"cycle", `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A","role":"worker","deps":["b"]},{"id":"b","title":"B","role":"test","deps":["a"]}]}`, "cycle"},
		{"duplicate", `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A","role":"worker"},{"id":"a","title":"B","role":"test"}]}`, "duplicates"},
		{"unknown dependency", `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A","role":"worker","deps":["missing"]}]}`, "unknown dependency"},
		{"unknown role", `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A","role":"robot"}]}`, "unknown role"},
		{"missing task role", `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A"}]}`, `missing required field "role"`},
		{"unknown difficulty", `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A","role":"worker","difficulty":"impossible"}]}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turn, err := ParseTurn(tt.body, nil)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("ParseTurn() error = %v, want containing %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "valid" && (turn.Tasks[0].Role != "worker" || turn.Tasks[0].Difficulty != "hard") {
				t.Fatalf("normalized task = %#v", turn.Tasks[0])
			}
			if tt.name == "unknown difficulty" && turn.Tasks[0].Difficulty != "" {
				t.Fatalf("unknown difficulty = %q", turn.Tasks[0].Difficulty)
			}
		})
	}
}

// paths are normalized and deduplicated, and a path outside the workspace is refused.
func TestParseTurnPaths(t *testing.T) {
	body := `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A","role":"worker","paths":["./core/a.go","core/a.go","core/./b.go"]}]}`
	turn, err := ParseTurn(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := turn.Tasks[0].Paths
	if len(got) != 2 || got[0] != "core/a.go" || got[1] != "core/b.go" {
		t.Fatalf("paths = %v", got)
	}
	for _, bad := range []string{`"/etc/passwd"`, `"../outside"`, `""`} {
		body := `{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"A","role":"worker","paths":[` + bad + `]}]}`
		if _, err := ParseTurn(body, nil); err == nil {
			t.Fatalf("path %s was accepted", bad)
		}
	}
}

func TestParseTurnKnownDependency(t *testing.T) {
	body := `{"status":"working","userMessage":"Continue","tasks":[{"id":"b","title":"Build","role":"worker","deps":["existing"]}]}`
	if _, err := ParseTurn(body, func(id string) bool { return id == "existing" }); err != nil {
		t.Fatal(err)
	}
}

// resolvedChanges is optional: a turn without it still parses, and one with it carries the ids to the app.
func TestParseTurnResolvedChanges(t *testing.T) {
	body := `{"status":"working","userMessage":"Resolved","tasks":[],"resolvedChanges":["chg_a","chg_b"]}`
	turn, err := ParseTurn(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.ResolvedChanges) != 2 || turn.ResolvedChanges[0] != "chg_a" || turn.ResolvedChanges[1] != "chg_b" {
		t.Fatalf("resolvedChanges = %#v", turn.ResolvedChanges)
	}
	plain, err := ParseTurn(`{"status":"working","userMessage":"Plan","tasks":[]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.ResolvedChanges != nil {
		t.Fatalf("omitted resolvedChanges = %#v, want nil", plain.ResolvedChanges)
	}
}

func TestParseReport(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		bad  bool
	}{{"valid", `{"status":"completed","summary":"done","findings":["verified"]}`, false}, {"malformed", `{bad`, true}, {"missing required", `{"status":"completed"}`, true}} {
		t.Run(tt.name, func(t *testing.T) {
			report, err := ParseReport(tt.body)
			if (err != nil) != tt.bad {
				t.Fatalf("ParseReport() error = %v, want bad %v", err, tt.bad)
			}
			if !tt.bad && (report.Summary != "done" || len(report.Findings) != 1) {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestParseTurnToleratesLeadInProse(t *testing.T) {
	body := "Here is the plan:\n{\"status\":\"working\",\"userMessage\":\"Plan\",\"tasks\":[]}\nLet me know."
	if _, err := ParseTurn(body, nil); err != nil {
		t.Fatalf("prose around the object was rejected: %v", err)
	}
}

// The last run lost a probe, and the tokens it spent, to `verification` entries written as plain strings.
func TestParseReportAcceptsLooseShapes(t *testing.T) {
	body := `{"summary":"Renamed the tab.",
	  "verification":["go test ./... passed",{"command":"npm test","status":"passed","output":"ok"}],
	  "findings":[{"summary":"label table lives in main.js"},"docs already updated"],
	  "filesChanged":"webview/src/main.js",
	  "structuredFindings":[{"kind":"architecture","summary":"labels","confidence":"high","verified":"true"},"not an object"]}`
	report, err := ParseReport(body)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "completed" {
		t.Fatalf("status = %q, want it defaulted to completed", report.Status)
	}
	if len(report.Verify) != 2 || report.Verify[0].Command != "go test ./... passed" || report.Verify[1].Command != "npm test" || report.Verify[1].Status != "passed" {
		t.Fatalf("verify = %+v", report.Verify)
	}
	if len(report.Findings) != 2 || report.Findings[0] != "label table lives in main.js" {
		t.Fatalf("findings = %q", report.Findings)
	}
	if len(report.Files) != 1 || report.Files[0] != "webview/src/main.js" {
		t.Fatalf("files = %q", report.Files)
	}
	raw, err := ParseJSON[RawReport](body)
	if err != nil || len(raw.Structured) != 1 || raw.Structured[0].Confidence != 0.9 || !raw.Structured[0].Verified {
		t.Fatalf("structured = %+v err=%v", raw.Structured, err)
	}
}

func TestParseReportUsesFindingsWhenSummaryIsMissing(t *testing.T) {
	report, err := ParseReport(`{"status":"completed","findings":["a is in x.go","b is in y.go"]}`)
	if err != nil || report.Summary != "a is in x.go; b is in y.go" {
		t.Fatalf("report = %+v err=%v", report, err)
	}
	if _, err := ParseReport(`{"status":"completed","findings":[]}`); err == nil {
		t.Fatal("a report with nothing in it was accepted")
	}
}
