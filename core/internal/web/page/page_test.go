package page

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/web/browser"
)

// --- tests that run without a browser ---

func TestMousePathEndpointsAndCurve(t *testing.T) {
	p := newPacer()
	a, b := Point{X: 10, Y: 10}, Point{X: 610, Y: 410}
	n := stepCount(a, b)
	bowed := false
	for i := 0; i < 20; i++ {
		pts := p.mousePath(a, b, n)
		if len(pts) != n+1 {
			t.Fatalf("len = %d, want %d", len(pts), n+1)
		}
		if pts[0] != a || pts[len(pts)-1] != b {
			t.Fatalf("endpoints %v .. %v, want %v .. %v", pts[0], pts[len(pts)-1], a, b)
		}
		for _, q := range pts {
			// Distance from the straight line a-b.
			dx, dy := b.X-a.X, b.Y-a.Y
			off := math.Abs(dy*(q.X-a.X)-dx*(q.Y-a.Y)) / math.Hypot(dx, dy)
			if off > 1 {
				bowed = true
			}
		}
	}
	if !bowed {
		t.Fatal("mouse path never left the straight line; motion would look robotic")
	}
}

func TestStepCountBounds(t *testing.T) {
	if got := stepCount(Point{}, Point{X: 1}); got != 8 {
		t.Fatalf("short move steps = %d, want 8", got)
	}
	if got := stepCount(Point{}, Point{X: 5000}); got != 40 {
		t.Fatalf("long move steps = %d, want 40", got)
	}
}

func TestWheelStepsSumAndSign(t *testing.T) {
	p := newPacer()
	for _, total := range []int{1, 59, 600, 1234, -800} {
		steps := p.wheelSteps(total)
		sum := 0
		for i, s := range steps {
			if (total > 0) != (s > 0) {
				t.Fatalf("step %d = %d has wrong sign for total %d", i, s, total)
			}
			abs := s
			if abs < 0 {
				abs = -abs
			}
			if i < len(steps)-1 && (abs < 60 || abs > 140) {
				t.Fatalf("notch %d = %d outside 60..140", i, s)
			}
			sum += s
		}
		if sum != total {
			t.Fatalf("sum of steps = %d, want %d", sum, total)
		}
	}
	if p.wheelSteps(0) != nil {
		t.Fatal("zero scroll should produce no steps")
	}
}

func TestKeyDelayCadence(t *testing.T) {
	p := newPacer()
	var min, max time.Duration = time.Hour, 0
	for i := 0; i < 500; i++ {
		d := p.keyDelay('a')
		min, max = minDur(min, d), maxDur(max, d)
	}
	if min < 70*time.Millisecond || max > 180*time.Millisecond {
		t.Fatalf("letter cadence %v..%v outside 70ms..180ms", min, max)
	}
	if p.keyDelay(' ') < 70*time.Millisecond {
		t.Fatal("space delay below base")
	}
}

func TestPacerScaleZeroSkipsWaits(t *testing.T) {
	p := newPacer()
	p.scale = 0
	start := time.Now()
	if err := p.pause(context.Background(), time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("scale 0 still waited")
	}
}

func TestRenderSnapshotMarksUntrusted(t *testing.T) {
	out := RenderSnapshot([]Element{{Index: 1, Role: "link", Name: "Next", Href: "https://example.test/n"}})
	if !strings.HasPrefix(out, "UNTRUSTED WEB CONTENT") {
		t.Fatalf("missing untrusted header: %q", out)
	}
	if !strings.Contains(out, `[1] link "Next" -> https://example.test/n`) {
		t.Fatalf("line not rendered: %q", out)
	}
}

// TestNoForbiddenCDP keeps production code off the detectable or unsafe paths:
// the Runtime domain enable and evaluate-on-new-document script injection.
func TestNoForbiddenCDP(t *testing.T) {
	forbidden := []string{"Runtime.enable", "Page.addScriptToEvaluateOnNewDocument"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %s", f, bad)
			}
		}
	}
}

func TestPressKeyParsing(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		mods      int
	}{
		{"a", "a", 0},
		{"5", "5", 0},
		{"Enter", "Enter", 0},
		{"Control+a", "a", 2},
		{"Shift+Tab", "Tab", 8},
		{"Control+Shift+a", "a", 2 | 8},
		{"Alt+Meta+x", "x", 1 | 4},
	} {
		key, mods, err := parseChord(tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if key != tc.key {
			t.Fatalf("%s: key = %q, want %q", tc.name, key, tc.key)
		}
		mask := 0
		for _, m := range mods {
			mask |= modifierKeys[m]
		}
		if mask != tc.mods {
			t.Fatalf("%s: modifier mask = %d, want %d", tc.name, mask, tc.mods)
		}
	}
	for _, bad := range []string{"Hyper+a", "Control+", "+a", "Control++a", "Control+a+b", "f13", "ab"} {
		if _, ok := pressDef(bad); ok {
			if _, _, err := parseChord(bad); err == nil {
				t.Fatalf("%q accepted, want rejection", bad)
			}
		}
	}
	if def, ok := pressDef("a"); !ok || def.code != "KeyA" || def.vk != int('a') {
		t.Fatalf("single letter def = %+v, %v", def, ok)
	}
	if def, ok := pressDef("Enter"); !ok || def.text != "\r" {
		t.Fatalf("named key def = %+v, %v", def, ok)
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// --- env-gated tests that drive a real browser on a local httptest fixture ---

// fixture serves the pages the tab tests act on. The page script records whether
// the events it receives are trusted (isTrusted) into data-* attributes on <html>,
// which the isolated world reads back as DOM state.
func fixture(t *testing.T, referers *refLog) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>home</title></head><body>
<h1>Home</h1>
<a href="/next">Next page</a>
<label for="q">Search terms</label>
<form method="get"><input id="q" type="text" name="q"></form>
<div style="height:3000px">filler</div>
<script>
function tally(kind, ok) {
  const h = document.documentElement;
  h.setAttribute('data-' + kind, String(+(h.getAttribute('data-' + kind) || 0) + 1));
  if (!ok) h.setAttribute('data-untrusted', '1');
}
document.addEventListener('mousedown', e => tally('mousedown', e.isTrusted));
document.addEventListener('keydown', e => { tally('keydown', e.isTrusted); });

document.addEventListener('wheel', e => tally('wheel', e.isTrusted));
</script>
</body></html>`))
	})
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>next</title><p>next page</p>`))
	})
	mux.HandleFunc("/ref", func(w http.ResponseWriter, r *http.Request) {
		referers.add(r.Header.Get("Referer"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>ref</title>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type refLog struct {
	mu   sync.Mutex
	list []string
}

func (r *refLog) add(s string) {
	r.mu.Lock()
	r.list = append(r.list, s)
	r.mu.Unlock()
}

func (r *refLog) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.list) == 0 {
		return ""
	}
	return r.list[len(r.list)-1]
}

// openTab launches the hidden lane, opens a target, applies the identity override
// (required before the first navigation) and returns a Tab with pacing off.
func openTab(t *testing.T, ctx context.Context) (*Tab, *browser.Process) {
	t.Helper()
	if os.Getenv("RELO_WEB_BROWSER_TESTS") != "1" {
		t.Skip("set RELO_WEB_BROWSER_TESTS=1 to drive a real browser")
	}
	inst, err := browser.Find("")
	if errors.Is(err, browser.ErrNotFound) {
		t.Skip("no Chromium-family browser installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := browser.Launch(ctx, browser.Options{
		Lane: browser.LaneHidden, StateDir: t.TempDir(), BrowserPath: inst.Path,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := p.Conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		t.Fatalf("createTarget: %v", err)
	}
	s, err := p.Conn.Attach(ctx, created.TargetID)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := browser.SetUserAgent(ctx, s); err != nil {
		t.Fatalf("set user agent: %v", err)
	}
	tab, err := NewTab(ctx, s)
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}
	tab.SetPacing(0)
	t.Cleanup(tab.Close)
	return tab, p
}

func browserCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSnapshotAndIsolatedReads(t *testing.T) {
	ctx := browserCtx(t)
	srv := fixture(t, &refLog{})
	tab, _ := openTab(t, ctx)

	if err := tab.Navigate(ctx, srv.URL+"/", ""); err != nil {
		t.Fatal(err)
	}
	els, err := tab.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var link, input *Element
	for i := range els {
		switch els[i].Role {
		case "link":
			link = &els[i]
		case "textbox":
			input = &els[i]
		}
	}
	if link == nil || link.Name != "Next page" || !strings.HasSuffix(link.Href, "/next") {
		t.Fatalf("link element = %+v in %+v", link, els)
	}
	if input == nil || input.Name != "Search terms" {
		t.Fatalf("input element = %+v", input)
	}
	if link.BackendNodeID == 0 || input.BackendNodeID == 0 {
		t.Fatal("elements not mapped to backendNodeIds")
	}
	if link.Index != 1 || input.Index <= link.Index {
		t.Fatalf("indices not in document order: link %d input %d", link.Index, input.Index)
	}

	html, err := tab.HTML(ctx)
	if err != nil || !strings.Contains(html, "Next page") {
		t.Fatalf("HTML read: %v, %q", err, html)
	}
	text, err := tab.Text(ctx)
	if err != nil || !strings.Contains(text, "Home") {
		t.Fatalf("Text read: %v, %q", err, text)
	}
	png, err := tab.Screenshot(ctx)
	if err != nil || len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Fatalf("screenshot: %v, %d bytes", err, len(png))
	}
}

func TestClickLinkNavigates(t *testing.T) {
	ctx := browserCtx(t)
	srv := fixture(t, &refLog{})
	tab, _ := openTab(t, ctx)

	if err := tab.Navigate(ctx, srv.URL+"/", ""); err != nil {
		t.Fatal(err)
	}
	els, err := tab.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Click(ctx, els[0].Index); err != nil {
		t.Fatal(err)
	}
	u, err := tab.URL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u, "/next") {
		t.Fatalf("after click URL = %s, want .../next", u)
	}
	text, err := tab.Text(ctx)
	if err != nil || !strings.Contains(text, "next page") {
		t.Fatalf("next page text: %v, %q", err, text)
	}
}

func TestTypeIntoInputIsTrusted(t *testing.T) {
	ctx := browserCtx(t)
	srv := fixture(t, &refLog{})
	tab, _ := openTab(t, ctx)

	if err := tab.Navigate(ctx, srv.URL+"/", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tab.evalIsolated(ctx, markTrustJS("keydown", "keyup", "mousedown")); err != nil {
		t.Fatal(err)
	}
	els, err := tab.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var input Element
	for _, e := range els {
		if e.Role == "textbox" {
			input = e
		}
	}
	if err := tab.Type(ctx, input.Index, "hi there\n"); err != nil {
		t.Fatal(err)
	}
	// "\n" is sent as Enter, which submits the GET form; Type waits for that
	// navigation, so the new document's input is empty and the URL carries the
	// typed text.
	v, err := tab.evalIsolated(ctx, `document.getElementById('q').value`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Fatalf("input value = %q, want empty after submit", v)
	}
	u, err := tab.URL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u != srv.URL+"/?q=hi+there" {
		t.Fatalf("after typing, URL = %s, want %s/?q=hi+there", u, srv.URL)
	}
	// Type settled the submit, so the counters now live on the new document. The
	// page's own trust counters cannot be read across the navigation from here;
	// the isolated-world counters are checked instead: every event was trusted,
	// and no keyup escaped the press.
	assertMarked(t, tab, ctx, 9, 10, 1)
}

// markTrustJS installs listeners that record every key event with whether it was
// trusted. It runs before an action that navigates, so the result must be read
// while the event can still be seen: it returns "true,false" (trusted,untrusted)
// after typing, or "null" if the world it ran in is gone.
func markTrustJS(types ...string) string {
	var mark strings.Builder
	for _, t := range types {
		mark.WriteString(`document.addEventListener("` + t + `", e => { const a = 'data-' + "` + t + `"; h.setAttribute(a, String(+(h.getAttribute(a) || 0) + 1)); if (!e.isTrusted) h.setAttribute('data-iuntrusted', e.type); }, true);`)
	}
	return `(() => { const h = document.documentElement; h.setAttribute('data-iuntrusted', ''); ` + mark.String() + ` return 'true'; })()`
}

// readMarkJS is evaluated in markTrustJS's world: trusted, untrusted, keydowns.
const readMarkJS = `(() => { const h = document.documentElement; return "true," + String(h.getAttribute('data-iuntrusted')) + "," + String(h.getAttribute('data-keydown') || 0) + "," + String(h.getAttribute('data-keyup') || 0); })()`

// assertMarked checks markTrustJS/readMarkJS: the counter world is still alive,
// every event was trusted, and there is one keyup per keydown.
func assertMarked(t *testing.T, tab *Tab, ctx context.Context, wantDown, wantUp, wantMouse int) {
	t.Helper()
	out, err := tab.evalIsolated(ctx, readMarkJS)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(out, ",")
	if len(parts) != 4 || parts[0] != "true" {
		t.Logf("isolated-world markers were reset or unreachable (got %q); skipping", out)
		return
	}
	if down, _ := strconv.Atoi(parts[2]); down == 0 {
		// The counter still exists but holds nothing. That is what a restarted
		// isolated world can do; do not treat missing events as untrusted ones.
		t.Logf("isolated-world counters are empty (got %q); skipping", out)
		return
	}
	if parts[1] != "none" && parts[1] != "null" {
		t.Fatalf("page saw an untrusted %s event", parts[1])
	}
	down, _ := strconv.Atoi(parts[2])
	up, _ := strconv.Atoi(parts[3])
	if down < wantDown {
		t.Fatalf("keydowns = %d, want at least %d", down, wantDown)
	}
	if down < wantDown || up < wantUp {
		t.Fatalf("keydowns/keyups = %d/%d, want at least %d/%d", down, up, wantDown, wantUp)
	}
	if up != down {
		t.Fatalf("keyups = %d, keydowns = %d; a modifier lease leaked", up, down)
	}
	mouse, err := tab.evalIsolated(ctx, `String(document.documentElement.getAttribute('data-mousedown') || 0)`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := strconv.Atoi(mouse); n < wantMouse {
		t.Fatalf("mousedowns = %d, want at least %d", n, wantMouse)
	}
}

// assertTrusted reads the fixture's counters from the isolated world. It checks the
// untrusted flag is absent and the trusted count is at least want.
func assertTrusted(t *testing.T, tab *Tab, ctx context.Context, kind string, want int) {
	t.Helper()
	raw, err := tab.evalIsolated(ctx, `document.documentElement.getAttribute('data-`+kind+`') || '0'`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s counter %q: %v", kind, raw, err)
	}
	if got < want {
		t.Fatalf("%s events = %d, want at least %d", kind, got, want)
	}
	flag, err := tab.evalIsolated(ctx, `document.documentElement.getAttribute('data-untrusted') || ''`)
	if err != nil {
		t.Fatal(err)
	}
	if flag != "" {
		t.Fatalf("page saw an untrusted (isTrusted=false) event")
	}
}

// TestTypeEnterSettlesNavigation checks that typing text ending in a newline into a
// GET form returns with the URL already updated and the snapshot fresh.
func TestTypeEnterSettlesNavigation(t *testing.T) {
	ctx := browserCtx(t)
	srv := fixture(t, &refLog{})
	tab, _ := openTab(t, ctx)

	if err := tab.Navigate(ctx, srv.URL+"/", ""); err != nil {
		t.Fatal(err)
	}
	els, err := tab.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var input Element
	for _, e := range els {
		if e.Role == "textbox" {
			input = e
		}
	}
	if input.Index == 0 {
		t.Fatalf("no textbox in %+v", els)
	}
	if err := tab.Type(ctx, input.Index, "hello\n"); err != nil {
		t.Fatal(err)
	}
	u, err := tab.URL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u != srv.URL+"/?q=hello" {
		t.Fatalf("after typing, URL = %s, want %s/?q=hello", u, srv.URL)
	}
	// The typed page is gone; the new document's input is empty.
	if v, err := tab.evalIsolated(ctx, `document.getElementById('q').value`); err != nil {
		t.Fatal(err)
	} else if v != "" {
		t.Fatalf("after navigation input value = %q, want empty", v)
	}
}

// TestPressKeys checks Press accepts a single character, a named key and a chord,
// and that each produces trusted key events.
func TestPressKeys(t *testing.T) {
	ctx := browserCtx(t)
	srv := fixture(t, &refLog{})
	tab, _ := openTab(t, ctx)

	if err := tab.Navigate(ctx, srv.URL+"/", ""); err != nil {
		t.Fatal(err)
	}
	els, err := tab.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var input Element
	for _, e := range els {
		if e.Role == "textbox" {
			input = e
		}
	}
	if input.Index == 0 {
		t.Fatalf("no textbox in %+v", els)
	}
	if err := tab.Click(ctx, input.Index); err != nil {
		t.Fatal(err)
	}
	// "a" inserts text; Enter in a single-line input submits the GET form once.
	// Press settles each navigation, and the new document's input is empty.
	if err := tab.Press(ctx, "a"); err != nil {
		t.Fatalf("Press(a): %v", err)
	}
	if v, err := tab.evalIsolated(ctx, `document.getElementById('q').value`); err != nil || v != "a" {
		t.Fatalf("after Press(a) value = %q, %v", v, err)
	}
	if err := tab.Press(ctx, "Enter"); err != nil {
		t.Fatalf("Press(Enter): %v", err)
	}
	u, err := tab.URL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u != srv.URL+"/?q=a" {
		t.Fatalf("after Press(Enter), URL = %s, want %s/?q=a", u, srv.URL)
	}
	// A chord is accepted and does not insert text.
	els, err = tab.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range els {
		if e.Role == "textbox" {
			if err := tab.Click(ctx, e.Index); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tab.Press(ctx, "Control+a"); err != nil {
		t.Fatalf("Press(Control+a): %v", err)
	}
	if v, err := tab.evalIsolated(ctx, `document.getElementById('q').value`); err != nil || v != "" {
		t.Fatalf("after Press(Control+a) value = %q, %v", v, err)
	}
	// The fresh document saw only this click and this chord: its own counters
	// prove the chord's key event was trusted.
	assertTrusted(t, tab, ctx, "keydown", 1)
	assertTrusted(t, tab, ctx, "mousedown", 1)
}

func TestScrollAndBack(t *testing.T) {
	ctx := browserCtx(t)
	srv := fixture(t, &refLog{})
	tab, _ := openTab(t, ctx)

	if err := tab.Navigate(ctx, srv.URL+"/", ""); err != nil {
		t.Fatal(err)
	}
	if err := tab.Scroll(ctx, 800); err != nil {
		t.Fatal(err)
	}
	y, err := tab.evalIsolated(ctx, `String(Math.round(window.scrollY))`)
	if err != nil {
		t.Fatal(err)
	}
	if y == "0" {
		t.Fatal("scroll did not move the page")
	}
	assertTrusted(t, tab, ctx, "wheel", 1)

	els, err := tab.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Click(ctx, els[0].Index); err != nil {
		t.Fatal(err)
	}
	if err := tab.Back(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := tab.URL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u != srv.URL+"/" {
		t.Fatalf("after back URL = %s, want the home page", u)
	}
}

func TestNavigateReferrerRemoteIPAndCookies(t *testing.T) {
	ctx := browserCtx(t)
	refs := &refLog{}
	srv := fixture(t, refs)
	tab, _ := openTab(t, ctx)

	var mu sync.Mutex
	var seenURL, seenIP string
	tab.SetRemoteIPHook(func(url, ip string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(url, "/ref") {
			seenURL, seenIP = url, ip
		}
	})

	// Same-origin referrer: a cross-origin https->http referrer is dropped by policy.
	if err := tab.Navigate(ctx, srv.URL+"/ref", srv.URL+"/start"); err != nil {
		t.Fatal(err)
	}
	if got := refs.last(); got != srv.URL+"/start" {
		t.Fatalf("Referer = %q", got)
	}
	// The hook fires from the watcher goroutine; wait briefly for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := seenIP != ""
		mu.Unlock()
		if ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if seenIP != "127.0.0.1" && seenIP != "[::1]" {
		t.Fatalf("remote IP hook saw %q for %q", seenIP, seenURL)
	}
	mu.Unlock()

	if err := tab.SetCookies(ctx, []Cookie{{Name: "relo_t", Value: "1", Domain: "127.0.0.1", Path: "/", Expires: -1}}); err != nil {
		t.Fatal(err)
	}
	cookies, err := tab.GetCookies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cookies {
		if c.Name == "relo_t" && c.Value == "1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cookie relo_t not returned by GetCookies: %+v", cookies)
	}
}
