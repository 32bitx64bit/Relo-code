package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// The engine reads scroll metrics through browserTab, so the shared fake
// implements the two reads with a page that reports no metrics of its own.
func (t *fakeTab) ViewportHeight(context.Context) (int, error) { return 0, nil }
func (t *fakeTab) ScrollY(context.Context) (int, error)        { return 0, nil }

// scrollingTab models a page whose scroll position the engine can read: the
// wheel either moves the page, or an inner scroll container takes it and the
// page stands still. readErr fails the reads, as a broken page would.
type scrollingTab struct {
	*fakeTab
	mu      sync.Mutex
	height  int
	scrollY int
	inner   bool
	readErr error
}

func (t *scrollingTab) ViewportHeight(context.Context) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.height, t.readErr
}

func (t *scrollingTab) ScrollY(context.Context) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollY, t.readErr
}

func (t *scrollingTab) Scroll(_ context.Context, dy int) error {
	t.events.add(fmt.Sprintf("scroll:%d", dy))
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inner {
		return nil // the inner container took the wheel; the page stands still
	}
	t.scrollY += dy
	if t.scrollY < 0 {
		t.scrollY = 0
	}
	return nil
}

func (t *scrollingTab) position() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollY
}

// scrollingLane hands the engine tabs whose scroll position a test controls.
type scrollingLane struct{ *fakeLane }

func (l *scrollingLane) Open(ctx context.Context) (browserTab, error) {
	tab, err := l.fakeLane.Open(ctx)
	if err != nil {
		return nil, err
	}
	return &scrollingTab{fakeTab: tab.(*fakeTab), height: 640}, nil
}

func newScrollEngine(t *testing.T) (*Engine, *scrollingLane, *eventLog) {
	t.Helper()
	engine, hidden, _, events := newFakeEngine(t, Settings{})
	lane := &scrollingLane{fakeLane: hidden}
	engine.laneFactory = func(context.Context, Tier, Settings, string) (browserLane, error) {
		return lane, nil
	}
	return engine, lane, events
}

func openScrollTab(t *testing.T, engine *Engine, agent string) *scrollingTab {
	t.Helper()
	if _, err := runActionForTest(t, engine, agent, Request{Action: ActionOpen, URL: "https://site.example/"}); err != nil {
		t.Fatal(err)
	}
	agentTab, err := engine.currentTab(engine.agents[agent])
	if err != nil {
		t.Fatal(err)
	}
	return agentTab.tab.(*scrollingTab)
}

func TestScrollDirectionMustBeUpOrDown(t *testing.T) {
	engine, _, events := newScrollEngine(t)
	for _, direction := range []string{"left", "right", "sideways", "Down", "down "} {
		_, err := engine.Do(context.Background(), "a", Request{Action: ActionScroll, Direction: direction})
		if err == nil {
			t.Fatalf("direction %q was accepted", direction)
		}
		if !strings.Contains(err.Error(), "must be up or down") {
			t.Fatalf("direction %q: error = %v", direction, err)
		}
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("a refused direction did work: %v", got)
	}
}

func TestScrollDeltaClampsAndTakesDirection(t *testing.T) {
	tall := &scrollingTab{height: 640}
	short := &scrollingTab{} // a page that reports no viewport height
	for _, tc := range []struct {
		name string
		tab  *scrollingTab
		req  Request
		want int
	}{
		{"no direction is down", tall, Request{}, 640},
		{"down one viewport", tall, Request{Direction: "down"}, 640},
		{"up one viewport", tall, Request{Direction: "up"}, -640},
		{"down an amount", tall, Request{Direction: "down", Amount: 120}, 120},
		{"up an amount", tall, Request{Direction: "up", Amount: 120}, -120},
		{"the cap itself", tall, Request{Direction: "down", Amount: maxScrollCSSPixels}, maxScrollCSSPixels},
		{"down clamps at the cap", tall, Request{Direction: "down", Amount: 99999}, maxScrollCSSPixels},
		{"up clamps at the cap", tall, Request{Direction: "up", Amount: 99999}, -maxScrollCSSPixels},
		{"no viewport falls back to the default", short, Request{Direction: "down"}, defaultScrollCSSPixels},
	} {
		got, err := scrollDelta(context.Background(), tc.tab, tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: dy = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestScrollDeltaReportsAFailedRead(t *testing.T) {
	broken := &scrollingTab{readErr: errors.New("page: isolated read threw")}
	if _, err := scrollDelta(context.Background(), broken, Request{Direction: "down"}); err == nil {
		t.Fatal("a page that cannot be read was scrolled anyway")
	}
}

func TestScrollClampsTheWheelDistance(t *testing.T) {
	engine, _, events := newScrollEngine(t)
	openScrollTab(t, engine, "a")
	for _, req := range []Request{
		{Action: ActionScroll, Direction: "down", Amount: 99999},
		{Action: ActionScroll, Direction: "up", Amount: 99999},
	} {
		if _, err := runActionForTest(t, engine, "a", req); err != nil {
			t.Fatal(err)
		}
	}
	assertEventOrder(t, events.snapshot(), []string{"scroll:5000", "scroll:-5000"})
}

func TestScrollZeroAmountIsOneViewportHeight(t *testing.T) {
	engine, _, events := newScrollEngine(t)
	tab := openScrollTab(t, engine, "a")
	tab.mu.Lock()
	tab.height = 700
	tab.mu.Unlock()
	for _, req := range []Request{
		{Action: ActionScroll, Direction: "down"},
		{Action: ActionScroll, Direction: "up"},
	} {
		if _, err := runActionForTest(t, engine, "a", req); err != nil {
			t.Fatal(err)
		}
	}
	assertEventOrder(t, events.snapshot(), []string{"scroll:700", "scroll:-700"})
}

func TestScrollNotesWhenThePageDidNotMove(t *testing.T) {
	engine, _, events := newScrollEngine(t)
	tab := openScrollTab(t, engine, "a")
	tab.mu.Lock()
	tab.inner = true // an inner scroll container takes the wheel
	tab.mu.Unlock()
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionScroll, Direction: "down", Amount: 400})
	if err != nil {
		t.Fatal(err)
	}
	assertEventOrder(t, events.snapshot(), []string{"scroll:400"})
	if got := tab.position(); got != 0 {
		t.Fatalf("scrollY = %d; want the page to stand still", got)
	}
	var note string
	for _, n := range result.Notes {
		if strings.Contains(n, "did not move") {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("notes = %v; want a note that the page did not move", result.Notes)
	}
	if !strings.Contains(note, "inner scroll container") {
		t.Fatalf("note = %q; want the inner scroll container named as the cause", note)
	}
	// Only the scroll stood still: the page itself is still read.
	if !strings.HasPrefix(result.Content, untrustedOpen) || !strings.HasSuffix(result.Content, untrustedClose) {
		t.Fatalf("content = %q; want the page snapshot", result.Content)
	}
}

func TestScrollSaysNothingWhenThePageMoved(t *testing.T) {
	engine, _, _ := newScrollEngine(t)
	tab := openScrollTab(t, engine, "a")
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionScroll, Direction: "down", Amount: 400})
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range result.Notes {
		if strings.Contains(note, "did not move") {
			t.Fatalf("notes = %v; the page moved, so no note belongs here", result.Notes)
		}
	}
	if got := tab.position(); got != 400 {
		t.Fatalf("scrollY = %d, want 400", got)
	}
}

func TestRealBrowserScrollsClampsAndNotesAStillPage(t *testing.T) {
	if os.Getenv("RELO_WEB_BROWSER_TESTS") != "1" {
		t.Skip("set RELO_WEB_BROWSER_TESTS=1 to drive a real hidden browser")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Tall</title></head><body style="margin:0"><main style="height:9000px"><p>tall page</p></main></body></html>`)
	}))
	defer server.Close()
	engine, err := New(Settings{Mode: ModeHidden, Provider: ProviderCustom,
		CustomTemplate: server.URL + "/search?q=%s", CustomOrigin: server.URL, CustomResultKind: KindJSON,
		DisableFallback: true, StateDir: t.TempDir(),
		RateLimitPerAgent: 1000000000, RateLimitGlobal: 1000000000}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.pacer.interval = 0
	const agent = "scroll-agent"
	if _, err := runActionForTest(t, engine, agent, Request{Action: ActionOpen, URL: server.URL + "/tall"}); err != nil {
		t.Fatal(err)
	}
	agentTab, err := engine.currentTab(engine.agents[agent])
	if err != nil {
		t.Fatal(err)
	}
	scrollY := func() int {
		y, err := agentTab.tab.ScrollY(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return y
	}
	height, err := agentTab.tab.ViewportHeight(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if height <= 0 {
		t.Fatalf("viewport height = %d", height)
	}

	// No amount is one viewport height, read from the page.
	first, err := runActionForTest(t, engine, agent, Request{Action: ActionScroll, Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if got := scrollY(); got < height-1 {
		t.Fatalf("scrollY = %d after one viewport of %d", got, height)
	}
	for _, note := range first.Notes {
		if strings.Contains(note, "did not move") {
			t.Fatalf("notes = %v; the page moved", first.Notes)
		}
	}

	// A huge amount is clamped to the cap, so the page moves the cap and no further.
	if _, err := runActionForTest(t, engine, agent, Request{Action: ActionScroll, Direction: "down", Amount: 99999}); err != nil {
		t.Fatal(err)
	}
	if got := scrollY(); got > height+maxScrollCSSPixels+100 {
		t.Fatalf("scrollY = %d; want at most one viewport plus %d", got, height+maxScrollCSSPixels)
	}

	// Back to the top, then one scroll further: the page cannot move.
	for i := 0; i < 3 && scrollY() > 0; i++ {
		if _, err := runActionForTest(t, engine, agent, Request{Action: ActionScroll, Direction: "up", Amount: maxScrollCSSPixels}); err != nil {
			t.Fatal(err)
		}
	}
	if got := scrollY(); got != 0 {
		t.Fatalf("scrollY = %d after scrolling up; want 0", got)
	}
	still, err := runActionForTest(t, engine, agent, Request{Action: ActionScroll, Direction: "up"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range still.Notes {
		if strings.Contains(note, "did not move") && strings.Contains(note, "inner scroll container") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %v; want a note that the page did not move", still.Notes)
	}
}
