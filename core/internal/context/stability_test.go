package ctxeng

import (
	"strings"
	"testing"
	"time"
)

func newStabilityEngine() *Engine {
	e := New()
	e.now = func() time.Time { return time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC) }
	e.Bind("p", "t", "primary", 24000)
	e.Ingest("p", []Item{{ID: "obj", Class: ClassTask, Source: SourceUser, Title: "Objective", Content: "Ship cache work", Pinned: true, Durability: 1}})
	return e
}

func decisionItem(content string) Item {
	return Item{Class: ClassTask, Source: SourceVerified, Title: "Decision", Content: content, Verified: true, Durability: 0.9}
}

// A verified decision ingested between two builds of the same generation chain must not
// change the Static block: it renders in Dynamic instead.
func TestMidGenerationDecisionKeepsStaticIdentical(t *testing.T) {
	e := newStabilityEngine()
	g1, err := e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn1"})
	if err != nil {
		t.Fatal(err)
	}
	staticBefore := RenderStable(g1)

	e.Ingest("p", []Item{decisionItem("use sqlite")})
	g2, err := e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := RenderStable(g2); got != staticBefore {
		t.Fatalf("static changed mid-generation:\nbefore=%q\nafter=%q", staticBefore, got)
	}
	dyn := RenderDynamic(g2)
	if !strings.Contains(dyn, "use sqlite") {
		t.Fatalf("decision not visible in dynamic section: %q", dyn)
	}
	if strings.Contains(RenderStable(g2), "use sqlite") {
		t.Fatal("decision leaked into static section")
	}
}

// Deferral persists across further non-boundary builds of the same generation chain.
func TestDeferredDecisionStaysDynamicAcrossBuilds(t *testing.T) {
	e := newStabilityEngine()
	g1, _ := e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn1"})
	static := RenderStable(g1)
	e.Ingest("p", []Item{decisionItem("use sqlite")})
	e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn2"})
	g3, _ := e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn3"})
	if RenderStable(g3) != static {
		t.Fatal("static drifted on third build of same generation")
	}
	if !strings.Contains(RenderDynamic(g3), "use sqlite") {
		t.Fatal("deferred decision disappeared from dynamic section")
	}
}

// A boundary build (refresh) starts a new generation where the decision may move to Static.
func TestDecisionMovesToStaticAfterRefresh(t *testing.T) {
	e := newStabilityEngine()
	e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn1"})
	e.Ingest("p", []Item{decisionItem("use sqlite")})
	e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn2"})

	g, err := e.Refresh("p", "primary requested refresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Deferred) != 0 {
		t.Fatalf("refresh kept deferred ids: %v", g.Deferred)
	}
	if !strings.Contains(RenderStable(g), "use sqlite") {
		t.Fatal("decision not in static after refresh")
	}
	if strings.Contains(RenderDynamic(g), "use sqlite") {
		t.Fatal("decision still rendered dynamic after refresh")
	}
}

// Dynamic text of the new build must extend the previous dynamic text (prefix preserved),
// so the runtime can send only the delta.
func TestDeferredDecisionKeepsDynamicPrefix(t *testing.T) {
	e := newStabilityEngine()
	g1, _ := e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn1"})
	d1 := RenderDynamic(g1)
	e.Ingest("p", []Item{decisionItem("use sqlite")})
	g2, _ := e.Build(BuildRequest{AgentID: "p", Role: "primary", Reason: "turn2"})
	if d2 := RenderDynamic(g2); !strings.HasPrefix(d2, d1) {
		t.Fatalf("dynamic prefix changed:\nprev=%q\nnext=%q", d1, d2)
	}
}
