package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	ctxeng "github.com/relocode/relo/internal/context"
)

func TestPrimaryLongRunKeepsOnlyCurrentTaskState(t *testing.T) {
	c, _, _ := newGuidanceCoord(t)
	p := c.ensurePrimary()
	for i := 0; i < 40; i++ {
		message := fmt.Sprintf("user instruction %d: preserve API %d", i, i)
		c.mu.Lock()
		c.addChatLocked(Chat{Role: "user", Text: message})
		c.mu.Unlock()
		c.seedPrimary(message, nil)
		c.applyTurn(p, Turn{Objective: fmt.Sprintf("objective %d", i), Decisions: []string{fmt.Sprintf("decision %d", i)}})
		g, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, item := range g.Items {
			counts[item.Title]++
			if item.Title == "Objective" && item.Content != fmt.Sprintf("objective %d", i) {
				t.Fatalf("stale objective after turn %d: %+v", i, item)
			}
			if item.Title == "Latest user message" && item.Content != message {
				t.Fatalf("stale latest message after turn %d: %+v", i, item)
			}
			if item.Title == "Decision" && (item.Content != fmt.Sprintf("decision %d", i) || item.Verified || item.Source != ctxeng.SourceSummary) {
				t.Fatalf("stale or falsely verified decision after turn %d: %+v", i, item)
			}
		}
		for _, title := range []string{"Objective", "Latest user message", "Decision", "Mode"} {
			if counts[title] != 1 {
				t.Fatalf("turn %d: %s count = %d", i, title, counts[title])
			}
		}
		if _, err := c.Engine.Refresh(p.ID, "integrate reports"); err != nil {
			t.Fatal(err)
		}
	}
	c.applyTurn(p, Turn{})
	g, _ := c.Engine.Refresh(p.ID, "unchanged turn")
	if !strings.Contains(ctxeng.Render(g), "decision 39") || c.Current().Objective != "objective 39" {
		t.Fatal("omitted task state cleared the current objective or decisions")
	}
	for _, marker := range []string{"user instruction 0:", "user instruction 15:", "user instruction 39:"} {
		if !strings.Contains(ctxeng.Render(g), marker) {
			t.Fatalf("verbatim user requirement lost across long run: %s", marker)
		}
	}
}

func TestPrimaryRetainsQuestionForShortUserSelection(t *testing.T) {
	c, _, _ := newGuidanceCoord(t)
	p := c.ensurePrimary()
	c.mu.Lock()
	c.sess.Messages = []Chat{
		{Role: "user", Text: "Investigate the coordination issue; do not install anything."},
		{Role: "assistant", Text: "1. Rebuild only. 2. Implement the verified fix."},
		{Role: "user", Text: "option 2"},
	}
	c.mu.Unlock()
	c.seedPrimary("option 2", nil)
	g, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := ctxeng.Render(g)
	for _, text := range []string{"option 2", "2. Implement the verified fix.", "do not install anything"} {
		if !strings.Contains(got, text) {
			t.Fatalf("fresh Primary context lost conversation anchor %q: %s", text, got)
		}
	}
}

func TestLatestDecisionsSurviveReloadWithoutAnotherGeneration(t *testing.T) {
	c, _, _ := newGuidanceCoord(t)
	p := c.ensurePrimary()
	c.applyTurn(p, Turn{Objective: "old objective", Decisions: []string{"old decision"}})
	g, _ := c.Engine.Build(ctxeng.BuildRequest{AgentID: p.ID})
	// Final answers update durable state after the last model input was built.
	c.applyTurn(p, Turn{Objective: "current objective", Decisions: []string{"current decision"}})
	data, err := json.Marshal(c.Current())
	if err != nil {
		t.Fatal(err)
	}
	var restored Session
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	c.Engine = ctxeng.New()
	c.Engine.RestoreGeneration(*g)
	c.Restore(&restored)
	c.seedPrimary("continue", nil)
	next, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := ctxeng.Render(next)
	if strings.Contains(got, "old decision") || strings.Contains(got, "old objective") || !strings.Contains(got, "current decision") || !strings.Contains(got, "current objective") {
		t.Fatalf("reload lost final-turn state: %s", got)
	}
}
