package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/web/page"
)

// newGuardEngine makes an engine whose page reads keep the engine's own URL
// guard; only the opening navigation is relaxed, so a fixture on loopback can be
// landed on and the guard is exercised on the tab's URL afterwards.
func newGuardEngine(t *testing.T, settings Settings) (*Engine, *fakeLane) {
	t.Helper()
	engine, hidden, _, _ := newFakeEngine(t, settings)
	engine.urlCheck = func(_ Settings, raw string) error {
		if strings.Contains(raw, "/land") {
			return nil
		}
		return checkURL(engine.settings, raw)
	}
	return engine, hidden
}

func openLandTab(t *testing.T, engine *Engine, agent string) *fakeTab {
	t.Helper()
	if _, err := runActionForTest(t, engine, agent, Request{Action: ActionOpen, URL: "https://land.example/start"}); err != nil {
		t.Fatal(err)
	}
	tab, err := engine.currentTab(engine.agents[agent])
	if err != nil {
		t.Fatal(err)
	}
	return tab.tab.(*fakeTab)
}

func setTabURL(t *testing.T, engine *Engine, agent, rawURL string) *fakeTab {
	t.Helper()
	tab := openLandTab(t, engine, agent)
	tab.mu.Lock()
	tab.url = rawURL
	tab.mu.Unlock()
	return tab
}

func assertRefusedNoContent(t *testing.T, result Result, tab *fakeTab, wantURL string) {
	t.Helper()
	if result.Content != "" || len(result.Elements) != 0 || len(result.Screenshot) != 0 {
		t.Fatalf("refused page leaked content: content=%q elements=%d screenshot=%d",
			result.Content, len(result.Elements), len(result.Screenshot))
	}
	if result.URL != "" || result.Title != "" {
		t.Fatalf("refused page leaked its URL or title: %+v", result)
	}
	if result.Outcome != OutcomeDone || result.State != StateDone {
		t.Fatalf("result state = %s/%s", result.State, result.Outcome)
	}
	found := false
	for _, note := range result.Notes {
		if strings.Contains(note, "refused") && strings.Contains(note, wantURL) && strings.Contains(note, "about:blank") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %q; want a refusal note naming %s", result.Notes, wantURL)
	}
	tab.mu.Lock()
	landed, navigates := tab.url, tab.navigates
	tab.mu.Unlock()
	if landed != aboutBlank {
		t.Fatalf("tab URL = %q; want about:blank", landed)
	}
	if navigates < 2 {
		t.Fatalf("navigations = %d; want a reset navigation", navigates)
	}
}

func TestSnapshotRefusesPrivateTabURL(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	tab := setTabURL(t, engine, "a", server.URL+"/admin")
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	assertRefusedNoContent(t, result, tab, server.URL+"/admin")
}

func TestSnapshotRefusesFileTabURL(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	tab := setTabURL(t, engine, "a", "file:///etc/passwd")
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	assertRefusedNoContent(t, result, tab, "file:///etc/passwd")
}

func TestScreenshotRefusesPrivateTabURL(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	tab := setTabURL(t, engine, "a", server.URL+"/admin")
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionScreenshot})
	if err != nil {
		t.Fatal(err)
	}
	assertRefusedNoContent(t, result, tab, server.URL+"/admin")
}

func TestWaitRefusesPrivateTabURL(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	tab := setTabURL(t, engine, "a", server.URL+"/admin")
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionWait, Amount: 1})
	if err != nil {
		t.Fatal(err)
	}
	assertRefusedNoContent(t, result, tab, server.URL+"/admin")
}

func TestClickRefusesPrivateRedirectTarget(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	tab := setTabURL(t, engine, "a", "https://land.example/start")
	tab.mu.Lock()
	tab.elements = []page.Element{{Index: 1, Role: "link", Name: "local", Href: server.URL + "/admin"}}
	tab.mu.Unlock()
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionClick, Ref: 1})
	if err != nil {
		t.Fatal(err)
	}
	assertRefusedNoContent(t, result, tab, server.URL+"/admin")
}

func TestProviderOriginStaysExemptForSnapshot(t *testing.T) {
	origin := "http://127.0.0.1:1"
	engine, _ := newGuardEngine(t, Settings{Provider: ProviderCustom,
		CustomTemplate: origin + "/search?q=%s", CustomOrigin: origin})
	tab := setTabURL(t, engine, "a", origin+"/search?q=relo")
	tab.mu.Lock()
	tab.elements = []page.Element{{Index: 1, Role: "link", Name: "result"}}
	tab.mu.Unlock()
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != origin+"/search?q=relo" || !strings.Contains(result.Content, "result") {
		t.Fatalf("exempt provider page was not returned: %+v", result)
	}
	tab.mu.Lock()
	landed, navigates := tab.url, tab.navigates
	tab.mu.Unlock()
	if landed != origin+"/search?q=relo" || navigates != 1 {
		t.Fatalf("exempt tab was reset: url=%q navigates=%d", landed, navigates)
	}
}

func TestRefusedPageStaysResetAcrossReads(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	tab := setTabURL(t, engine, "a", server.URL+"/admin")
	tab.mu.Lock()
	tab.elements = []page.Element{{Index: 1, Role: "link", Name: "private"}}
	blockedHTML := tab.html
	tab.mu.Unlock()
	if _, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot}); err != nil {
		t.Fatal(err)
	}
	// A reset navigation the browser refused leaves the blocked page in place.
	tab.mu.Lock()
	tab.url, tab.html = server.URL+"/admin", blockedHTML
	tab.mu.Unlock()
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" || len(result.Elements) != 0 {
		t.Fatalf("content returned after reset: %+v", result)
	}
	for _, note := range result.Notes {
		if strings.Contains(note, server.URL+"/admin") && strings.Contains(note, "refused") {
			return
		}
	}
	t.Fatalf("notes = %q; want the refusal named again", result.Notes)
}

func TestResetThatLandsElsewhereClearsRefusal(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	tab := openLandTab(t, engine, "a")
	// As in TestGuardCurrentKeepsRefusalWhenResetIsRedirected, the page model
	// refreshes the tab URL on every navigation, so the tab also models a reset
	// that landed on a public page here.
	tab.lane.pageForURL = func(string) string { return "<html><main><p>landed page</p></main></html>" }
	tab.mu.Lock()
	tab.url = server.URL + "/admin"
	tab.mu.Unlock()
	refused, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if refused.Content != "" || len(refused.Elements) != 0 {
		t.Fatalf("refused page leaked content: %+v", refused)
	}
	tab.mu.Lock()
	landed := tab.url
	tab.mu.Unlock()
	if landed != aboutBlank {
		t.Fatalf("reset URL = %q; want about:blank", landed)
	}
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != aboutBlank {
		t.Fatalf("second read URL = %q; want about:blank", result.URL)
	}
	for _, note := range result.Notes {
		if strings.Contains(note, "refused") {
			t.Fatalf("second read replayed a spent refusal: %q", result.Notes)
		}
	}
}

func TestGuardCurrentKeepsRefusalWhenResetIsRedirected(t *testing.T) {
	engine, _ := newGuardEngine(t, Settings{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	tab := openLandTab(t, engine, "a")
	engine.urlCheck = func(_ Settings, raw string) error {
		if strings.Contains(raw, "/land") {
			return nil
		}
		return errors.New("netguard: refused by the test guard")
	}
	tab.mu.Lock()
	tab.url = server.URL + "/admin"
	tab.mu.Unlock()
	// Like TestResetThatLandsElsewhereClearsRefusal, the page model reports every
	// navigation as landed on about:blank, so the tab is reset there.
	tab.lane.pageForURL = func(string) string { return "<html><main><p>private</p></main></html>" }
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" || len(result.Elements) != 0 {
		t.Fatalf("refused page leaked content: %+v", result)
	}
	joined := strings.Join(result.Notes, " ")
	if !strings.Contains(joined, "refused") || !strings.Contains(joined, server.URL+"/admin") {
		t.Fatalf("notes = %q; want the refusal named", result.Notes)
	}
	// The reset landed on about:blank: the tab's own URL is read as itself.
	if rawURL, landedRefusal := engine.guardCurrent(t.Context(), &actionEnv{agent: engine.agents["a"], settings: engine.settings},
		engine.agents["a"].tab, t.Context()); landedRefusal != nil || rawURL != aboutBlank {
		t.Fatalf("about:blank tab: url=%q refusal=%+v", rawURL, landedRefusal)
	}
	tab.mu.Lock()
	landed := tab.url
	tab.mu.Unlock()
	if landed != aboutBlank {
		t.Fatalf("after the refused land: tab URL = %q; want about:blank recovered", landed)
	}
}

func TestAboutBlankIsRefusedByGuard(t *testing.T) {
	if err := checkURL(Settings{}, aboutBlank); err == nil {
		t.Fatal("about:blank was accepted by the URL guard")
	}
}

func TestRealBrowserRefusesPrivateTabURL(t *testing.T) {
	if os.Getenv("RELO_WEB_BROWSER_TESTS") != "1" {
		t.Skip("set RELO_WEB_BROWSER_TESTS=1 to drive a real hidden browser")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><main><p>private page</p></main></html>"))
	}))
	defer server.Close()
	engine, err := New(Settings{Mode: ModeHidden, StateDir: t.TempDir(),
		RateLimitPerAgent: 1000000000, RateLimitGlobal: 1000000000}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.pacer.interval = 0
	engine.urlCheck = func(_ Settings, raw string) error {
		if strings.Contains(raw, "/land") {
			return nil
		}
		return checkURL(engine.settings, raw)
	}
	if _, err := runActionForTest(t, engine, "a", Request{Action: ActionOpen, URL: server.URL + "/land"}); err != nil {
		t.Fatal(err)
	}
	tab, err := engine.currentTab(engine.agents["a"])
	if err != nil {
		t.Fatal(err)
	}
	navCtx := t.Context()
	if err := tab.tab.Navigate(navCtx, "about:blank", ""); err != nil {
		t.Fatal(err)
	}
	// Land on the private page through a scripted location change the engine
	// cannot see in advance, then confirm the read is refused.
	if err := tab.tab.Navigate(navCtx, server.URL+"/admin", ""); err != nil {
		t.Fatal(err)
	}
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" || len(result.Elements) != 0 {
		t.Fatalf("refused page leaked content: %+v", result)
	}
	rawURL, err := tab.tab.URL(navCtx)
	if err != nil {
		t.Fatal(err)
	}
	if rawURL != aboutBlank {
		t.Fatalf("tab URL = %q; want about:blank", rawURL)
	}
}
