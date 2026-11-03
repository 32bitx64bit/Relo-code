package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
)

func TestTheRepoMapNamesAreasAndRootFilesWithoutCode(t *testing.T) {
	files := []string{"README.md", "Makefile", "engine/src/ai/facts.c", "engine/src/ai/facts.h", "engine/src/ai/rewrite.h", "engine/src/sim/tick.c", "docs/design.md", "docs/notes.md"}
	got := buildRepoMap(files)
	for _, want := range []string{"Root files: Makefile, README.md", "- engine/src/ai/  3 files (.h 2, .c 1)", "- engine/src/sim/  1 files (.c 1)", "- docs/  2 files (.md 2)"} {
		if !strings.Contains(got, want) {
			t.Errorf("map lacks %q:\n%s", want, got)
		}
	}
	if buildRepoMap(nil) != "" {
		t.Error("an empty tree produced a map")
	}
}

// A big tree is shown at the depth that fits, and the map stays within its byte budget.
func TestTheRepoMapStaysWithinItsBudget(t *testing.T) {
	var files []string
	for a := 0; a < 40; a++ {
		for b := 0; b < 40; b++ {
			files = append(files, "pkg"+strings.Repeat("a", a)+"/mod"+strings.Repeat("b", b)+"/file.go")
		}
	}
	got := buildRepoMap(files)
	if len(got) > repoMapMaxBytes+200 {
		t.Fatalf("map is %d bytes, budget %d", len(got), repoMapMaxBytes)
	}
	if strings.Count(got, "\n- ") > repoMapMaxAreas+1 {
		t.Fatalf("map lists %d areas, want at most %d", strings.Count(got, "\n- "), repoMapMaxAreas)
	}
}

func TestTheRepoMapSkipsStateAndDependencies(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"src/a.go", "node_modules/x/y.js", ".relo/sessions/s.db", "build/out.o", "main.go"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(listProjectFiles(dir), ",")
	if !strings.Contains(got, "src/a.go") || !strings.Contains(got, "main.go") {
		t.Fatalf("files = %s", got)
	}
	for _, bad := range []string{"node_modules", ".relo", "build/"} {
		if strings.Contains(got, bad) {
			t.Fatalf("files include %s: %s", bad, got)
		}
	}
}

func TestThePrimaryContextCarriesTheRepoMapAsStableState(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "engine", "a.c"), []byte("int main(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(dir, "", ModeBuild)
	p := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: dir})
	c.Engine.Bind(p.ID, "", string(agent.Primary), 24000)
	c.setRepoMap(p.ID)
	gen := c.Engine.Generation(p.ID, c.Engine.CurrentNumber(p.ID))
	if gen == nil {
		g, err := c.Engine.Refresh(p.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		gen = g
	}
	found := false
	for _, it := range gen.Items {
		if it.Title == "Repository map" && strings.Contains(it.Content, "engine/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no repository map item in the Primary's context: %+v", gen.Items)
	}
}
