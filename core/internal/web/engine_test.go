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
	"time"

	"github.com/relocode/relo/internal/web/challenge"
	"github.com/relocode/relo/internal/web/page"
)

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type fakeLane struct {
	tier       Tier
	events     *eventLog
	mu         sync.Mutex
	tabs       []*fakeTab
	openErr    error
	pageForURL func(string) string
	challenges []challenge.Result
}

func (l *fakeLane) Open(context.Context) (browserTab, error) {
	if l.openErr != nil {
		return nil, l.openErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tab := &fakeTab{lane: l, events: l.events, html: "<html><title>Fake</title><main><p>fake page</p></main></html>",
		challenges: append([]challenge.Result(nil), l.challenges...)}
	l.tabs = append(l.tabs, tab)
	l.events.add("open:" + string(l.tier))
	return tab, nil
}

func (l *fakeLane) Close() error { return nil }

func (l *fakeLane) lastTab() *fakeTab {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.tabs) == 0 {
		return nil
	}
	return l.tabs[len(l.tabs)-1]
}

type fakeTab struct {
	lane       *fakeLane
	events     *eventLog
	mu         sync.Mutex
	url        string
	html       string
	text       string
	elements   []page.Element
	cookies    []page.Cookie
	setCookies []page.Cookie
	challenges []challenge.Result
	clicks     int
	navigates  int
	closed     bool
	block      chan struct{}
	started    chan struct{}
	startOnce  sync.Once
}

func (t *fakeTab) Navigate(ctx context.Context, rawURL, _ string) error {
	t.events.add("navigate:" + string(t.lane.tier) + ":" + rawURL)
	if t.started != nil {
		t.startOnce.Do(func() { close(t.started) })
	}
	if t.block != nil {
		select {
		case <-t.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.navigates++
	t.url = rawURL
	if t.lane.pageForURL != nil {
		t.html = t.lane.pageForURL(rawURL)
	}
	return nil
}

func (t *fakeTab) URL(context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.url, nil
}

func (t *fakeTab) Snapshot(context.Context) ([]page.Element, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]page.Element(nil), t.elements...), nil
}

func (t *fakeTab) Click(_ context.Context, ref int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clicks++
	t.events.add(fmt.Sprintf("click:%s:%d", t.lane.tier, ref))
	for _, element := range t.elements {
		if element.Index == ref {
			t.url = element.Href
			t.html = "<html><title>Clicked</title><main><p>clicked article body</p></main></html>"
		}
	}
	return nil
}

func (t *fakeTab) Type(_ context.Context, ref int, text string) error {
	t.events.add(fmt.Sprintf("type:%d:%s", ref, text))
	return nil
}
func (t *fakeTab) Press(_ context.Context, key string) error {
	t.events.add("press:" + key)
	return nil
}
func (t *fakeTab) Scroll(_ context.Context, dy int) error {
	t.events.add(fmt.Sprintf("scroll:%d", dy))
	return nil
}
func (t *fakeTab) Back(context.Context) error { t.events.add("back"); return nil }
func (t *fakeTab) HTML(context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.html, nil
}
func (t *fakeTab) Text(context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.text != "" {
		return t.text, nil
	}
	return t.html, nil
}
func (t *fakeTab) Screenshot(context.Context) ([]byte, error) { return []byte("png"), nil }
func (t *fakeTab) GetCookies(context.Context) ([]page.Cookie, error) {
	t.events.add("cookies:" + string(t.lane.tier))
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]page.Cookie(nil), t.cookies...), nil
}
func (t *fakeTab) SetCookies(_ context.Context, cookies []page.Cookie) error {
	t.events.add(fmt.Sprintf("set-cookies:%s:%d", t.lane.tier, len(cookies)))
	t.mu.Lock()
	t.setCookies = append([]page.Cookie(nil), cookies...)
	t.mu.Unlock()
	return nil
}
func (t *fakeTab) RunChallenge(_ context.Context, options challenge.Options) (challenge.Result, error) {
	t.events.add(fmt.Sprintf("challenge:%s:disable=%t", t.lane.tier, options.DisableClick))
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.challenges) == 0 {
		return challenge.Result{Outcome: challenge.Solved}, nil
	}
	result := t.challenges[0]
	t.challenges = t.challenges[1:]
	return result, nil
}
func (t *fakeTab) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.events.add("close:" + string(t.lane.tier))
	return nil
}

func newFakeEngine(t *testing.T, settings Settings) (*Engine, *fakeLane, *fakeLane, *eventLog) {
	t.Helper()
	settings.StateDir = t.TempDir()
	settings.RateLimitPerAgent = 1000000000
	settings.RateLimitGlobal = 1000000000
	engine, err := New(settings, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	engine.pacer.interval = 0
	engine.urlCheck = func(Settings, string) error { return nil }
	events := &eventLog{}
	hidden := &fakeLane{tier: TierHidden, events: events}
	window := &fakeLane{tier: TierWindow, events: events}
	engine.laneFactory = func(_ context.Context, tier Tier, _ Settings, _ string) (browserLane, error) {
		if tier == TierWindow {
			return window, nil
		}
		return hidden, nil
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine, hidden, window, events
}

func runActionForTest(t *testing.T, engine *Engine, agent string, req Request) (Result, error) {
	t.Helper()
	running, err := engine.Do(context.Background(), agent, req)
	if err != nil {
		return Result{}, err
	}
	if running.State != StateRunning {
		return running, nil
	}
	for i := 0; i < 3; i++ {
		result, waitErr := engine.Wait(context.Background(), agent, running.Op, MaxWait)
		if waitErr != nil || result.State != StateRunning {
			return result, waitErr
		}
	}
	return Result{}, errors.New("test operation did not finish")
}

func TestOperationsWaitAndEnvelope(t *testing.T) {
	engine, _, _, _ := newFakeEngine(t, Settings{})
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionOpen, URL: "https://site.example/"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateDone || result.Outcome != OutcomeDone || result.Op != string(ActionOpen) {
		t.Fatalf("result = %+v", result)
	}
	if !strings.HasPrefix(result.Content, untrustedOpen) || !strings.HasSuffix(result.Content, untrustedClose) {
		t.Fatalf("content is not enveloped: %q", result.Content)
	}
	if got := boundedWait(2 * MaxWait); got != MaxWait {
		t.Fatalf("boundedWait = %v", got)
	}
	if got := boundedWait(time.Second); got != time.Second {
		t.Fatalf("boundedWait short = %v", got)
	}
}

func TestLimiterAndSharedDomainPacing(t *testing.T) {
	now := time.Unix(1000, 0)
	limiter := newRateLimiter(10)
	limiter.now = func() time.Time { return now }
	if delay := limiter.reserve(); delay != 0 {
		t.Fatalf("first limiter delay = %v", delay)
	}
	if delay := limiter.reserve(); delay != 6*time.Second {
		t.Fatalf("second limiter delay = %v", delay)
	}
	now = now.Add(12 * time.Second)
	if delay := limiter.reserve(); delay != 0 {
		t.Fatalf("due limiter delay = %v", delay)
	}

	pacer := newDomainPacer(time.Second)
	pacer.now = func() time.Time { return now }
	if delay := pacer.reserve("https://a.example.co.uk/one"); delay != 0 {
		t.Fatalf("first domain delay = %v", delay)
	}
	if delay := pacer.reserve("https://b.example.co.uk/two"); delay != time.Second {
		t.Fatalf("shared domain delay = %v", delay)
	}
	if delay := pacer.reserve("https://other.example/"); delay != 0 {
		t.Fatalf("other domain delay = %v", delay)
	}
}

func TestCustomProviderFallbackAddsNote(t *testing.T) {
	origin := "https://custom.example"
	engine, hidden, _, _ := newFakeEngine(t, Settings{Provider: ProviderCustom,
		CustomTemplate: origin + "/search?q=%s", CustomOrigin: origin})
	hidden.pageForURL = func(raw string) string {
		if strings.HasPrefix(raw, origin) {
			return "<html><main><p>no results here</p></main></html>"
		}
		return `<html><body><div class="result results_links"><a class="result__a" href="https://site.example/article">Fallback result</a><a class="result__snippet">A fallback snippet</a></div></body></html>`
	}
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSearch, Query: "relo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Title != "Fallback result" {
		t.Fatalf("results = %+v", result.Results)
	}
	if !containsString(result.Notes, "custom search provider failed; used duckduckgo fallback") {
		t.Fatalf("notes = %v", result.Notes)
	}
}

func TestChallengeEscalatesOnceWithDomainCookiesThenHandsOff(t *testing.T) {
	engine, hidden, window, events := newFakeEngine(t, Settings{Mode: ModeAuto})
	widget := challenge.Widget{Kind: challenge.KindTurnstile, Mode: challenge.ModeAutoCheckbox, Visible: true}
	hidden.challenges = []challenge.Result{{Outcome: challenge.StillChallenged, Detected: true, Widgets: []challenge.Widget{widget}, Clicked: true}}
	window.challenges = []challenge.Result{{Outcome: challenge.StillChallenged, Detected: true, Widgets: []challenge.Widget{widget}}}
	asked := false
	engine.hooks.Challenge = func(_ context.Context, handoff Handoff) (bool, error) {
		asked = true
		events.add("handoff:" + handoff.Kind)
		return false, nil
	}
	// Open creates the tab lazily; install domain cookies before its challenge.
	hidden.pageForURL = func(string) string { return "<html><title>challenge</title></html>" }
	engine.laneFactory = func(_ context.Context, tier Tier, _ Settings, _ string) (browserLane, error) {
		if tier == TierWindow {
			return window, nil
		}
		return &cookieLane{fakeLane: hidden}, nil
	}

	result, err := runActionForTest(t, engine, "agent", Request{Action: ActionOpen, URL: "https://site.example/challenge"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateNeedsUser || result.Tier != TierWindow || result.Challenge == nil || !asked {
		t.Fatalf("result = %+v, asked = %t", result, asked)
	}
	if !containsString(result.Notes, "hidden lane challenged; escalated once to the window lane") {
		t.Fatalf("notes = %v", result.Notes)
	}
	got := events.snapshot()
	wantOrder := []string{"challenge:hidden", "cookies:hidden", "close:hidden", "open:window", "set-cookies:window:1", "navigate:window", "challenge:window:disable=true", "handoff:"}
	assertEventOrder(t, got, wantOrder)
}

func TestSuccessfulEscalationExtractsWindowPage(t *testing.T) {
	engine, hidden, window, _ := newFakeEngine(t, Settings{Mode: ModeAuto})
	widget := challenge.Widget{Kind: challenge.KindTurnstile, Mode: challenge.ModeAutoCheckbox, Visible: true}
	hidden.challenges = []challenge.Result{{Outcome: challenge.StillChallenged, Detected: true, Widgets: []challenge.Widget{widget}}}
	window.challenges = []challenge.Result{{Outcome: challenge.Solved}}
	hidden.pageForURL = func(string) string { return "<html><title>hidden challenge</title></html>" }
	result, err := runActionForTest(t, engine, "agent", Request{Action: ActionOpen, URL: "https://site.example/challenge"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != TierWindow || result.Title != "Fake" || !strings.Contains(result.Content, "fake page") {
		t.Fatalf("result read from wrong lane: %+v", result)
	}
}

type cookieLane struct{ *fakeLane }

func (l *cookieLane) Open(ctx context.Context) (browserTab, error) {
	tab, err := l.fakeLane.Open(ctx)
	if err == nil {
		fake := tab.(*fakeTab)
		fake.cookies = []page.Cookie{{Name: "session", Value: "secret", Domain: ".example"},
			{Name: "other", Value: "no", Domain: "other.invalid"}}
	}
	return tab, err
}

func TestSearchResultOpenUsesSERPClick(t *testing.T) {
	origin := "https://custom.example"
	engine, hidden, _, events := newFakeEngine(t, Settings{Provider: ProviderCustom,
		CustomTemplate: origin + "/search?q=%s", CustomOrigin: origin})
	const article = "https://site.example/article"
	hidden.pageForURL = func(string) string {
		return `<html><main><div><a href="` + article + `">A useful article</a><p>A sufficiently long snippet.</p></div></main></html>`
	}
	if _, err := runActionForTest(t, engine, "a", Request{Action: ActionSearch, Query: "article"}); err != nil {
		t.Fatal(err)
	}
	tab := hidden.lastTab()
	tab.elements = []page.Element{{Index: 1, Role: "link", Name: "A useful article", Href: article}}
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionOpen, URL: article})
	if err != nil {
		t.Fatal(err)
	}
	if tab.clicks != 1 || tab.navigates != 1 {
		t.Fatalf("clicks = %d, navigations = %d; want click-only open after search", tab.clicks, tab.navigates)
	}
	if !strings.Contains(result.Content, "clicked article body") || !strings.HasPrefix(result.Content, untrustedOpen) {
		t.Fatalf("content = %q", result.Content)
	}
	assertEventOrder(t, events.snapshot(), []string{"navigate:hidden", "click:hidden:1"})
}

func TestGlobalTabCapAndForgetAgentCancellation(t *testing.T) {
	t.Run("tab cap", func(t *testing.T) {
		engine, _, _, _ := newFakeEngine(t, Settings{MaxTabs: 1})
		if _, err := runActionForTest(t, engine, "one", Request{Action: ActionOpen, URL: "https://one.example/"}); err != nil {
			t.Fatal(err)
		}
		_, err := runActionForTest(t, engine, "two", Request{Action: ActionOpen, URL: "https://two.example/"})
		if !errors.Is(err, errTooManyTabs) {
			t.Fatalf("second agent error = %v", err)
		}
	})

	t.Run("forget cancels", func(t *testing.T) {
		engine, hidden, _, _ := newFakeEngine(t, Settings{})
		hidden.pageForURL = func(string) string { return "unused" }
		lane := hidden
		engine.laneFactory = func(context.Context, Tier, Settings, string) (browserLane, error) {
			return &blockingLane{fakeLane: lane}, nil
		}
		running, err := engine.Do(context.Background(), "a", Request{Action: ActionOpen, URL: "https://site.example/"})
		if err != nil {
			t.Fatal(err)
		}
		var tab *fakeTab
		deadline := time.Now().Add(time.Second)
		for tab == nil && time.Now().Before(deadline) {
			tab = hidden.lastTab()
			time.Sleep(time.Millisecond)
		}
		if tab == nil {
			t.Fatal("tab was not opened")
		}
		<-tab.started
		engine.ForgetAgent("a")
		result, err := engine.Wait(context.Background(), "a", running.Op, time.Second)
		if !errors.Is(err, context.Canceled) || result.Outcome != OutcomeCancelled {
			t.Fatalf("Wait = %+v, %v", result, err)
		}
		if !tab.closed {
			t.Fatal("ForgetAgent did not close the tab")
		}
	})
}

type blockingLane struct{ *fakeLane }

func (l *blockingLane) Open(context.Context) (browserTab, error) {
	l.mu.Lock()
	tab := &fakeTab{lane: l.fakeLane, events: l.events, html: "unused",
		block: make(chan struct{}), started: make(chan struct{})}
	l.tabs = append(l.tabs, tab)
	l.mu.Unlock()
	l.events.add("open:" + string(l.tier))
	return tab, nil
}

func TestPlainHTTPFallbackIsFlagged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"title":"HTTP result","url":"https://site.example/","snippet":"plain"}]`)
	}))
	defer server.Close()
	engine, err := New(Settings{Provider: ProviderCustom, CustomTemplate: server.URL + "/search?q=%s",
		CustomOrigin: server.URL, CustomResultKind: KindJSON, DisableFallback: true, StateDir: t.TempDir(),
		RateLimitPerAgent: 1000000000, RateLimitGlobal: 1000000000}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.pacer.interval = 0
	engine.laneFactory = func(context.Context, Tier, Settings, string) (browserLane, error) {
		return nil, errors.New("no test browser")
	}
	result, err := runActionForTest(t, engine, "a", Request{Action: ActionSearch, Query: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != TierHTTP || len(result.Results) != 1 || !containsString(result.Notes, "browser unavailable; used plain Go HTTP") {
		t.Fatalf("result = %+v", result)
	}
}

func TestDiscoverExplicitTemplateNeedsNoBrowser(t *testing.T) {
	engine, err := New(Settings{}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	provider, err := engine.DiscoverProvider(context.Background(), "https://search.example/find?q=%s")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name != ProviderCustom || provider.Origin != "https://search.example" || provider.ResultKind != KindHTML {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestBrowserSearchAndFetch(t *testing.T) {
	if os.Getenv("RELO_WEB_BROWSER_TESTS") != "1" {
		t.Skip("set RELO_WEB_BROWSER_TESTS=1 for the hidden-browser smoke test")
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"title":"Local article","url":%q,"snippet":"local snippet"}]`, server.URL+"/article")
		case "/article":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>Extracted article</title></head><body><main><h1>Extracted article</h1><p>Readable markdown body.</p></main></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var logMu sync.Mutex
	var logs []string
	engine, err := New(Settings{Mode: ModeHidden, Provider: ProviderCustom,
		CustomTemplate: server.URL + "/search?q=%s", CustomOrigin: server.URL, CustomResultKind: KindJSON,
		DisableFallback: true, StateDir: t.TempDir(), RateLimitPerAgent: 1000000000, RateLimitGlobal: 1000000000}, Hooks{Log: func(msg string) {
		logMu.Lock()
		logs = append(logs, msg)
		logMu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.pacer.interval = 0
	searched, err := runActionForTest(t, engine, "browser-agent", Request{Action: ActionSearch, Query: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if searched.Tier != TierHidden || len(searched.Results) != 1 {
		t.Fatalf("search = %+v", searched)
	}
	fetched, err := runActionForTest(t, engine, "browser-agent", Request{Action: ActionFetch, URL: server.URL + "/article"})
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Tier != TierHidden || !strings.Contains(fetched.Content, "Readable markdown body.") ||
		!strings.HasPrefix(fetched.Content, untrustedOpen) {
		t.Fatalf("fetch = %+v", fetched)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !containsStringPrefix(logs, "web stealth self-check lane=hidden") {
		t.Fatalf("self-check not logged: %v", logs)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsStringPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func assertEventOrder(t *testing.T, events []string, orderedPrefixes []string) {
	t.Helper()
	position := 0
	for _, want := range orderedPrefixes {
		found := false
		for position < len(events) {
			event := events[position]
			position++
			if strings.HasPrefix(event, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("event %q missing in order: %v", want, events)
		}
	}
}
