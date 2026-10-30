package challenge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/web/browser"
	"github.com/relocode/relo/internal/web/cdp"
)

// --- tests that run without a browser ---

func TestClassify(t *testing.T) {
	cases := []struct {
		src  string
		kind Kind
		mode Mode
		ok   bool
	}{
		{"https://challenges.cloudflare.com/turnstile/v0/x", KindTurnstile, ModeAutoCheckbox, true},
		{"https://www.google.com/recaptcha/api2/anchor?ar=1", KindRecaptchaV2, ModeAutoCheckbox, true},
		{"https://www.google.com/recaptcha/api2/bframe?k=x", KindRecaptchaV2, ModeUserPuzzle, true},
		{"https://newassets.hcaptcha.com/captcha/v1/x/static/hcaptcha.html#frame=checkbox", KindHCaptcha, ModeAutoCheckbox, true},
		{"https://newassets.hcaptcha.com/captcha/v1/x/static/hcaptcha.html#frame=challenge", KindHCaptcha, ModeUserPuzzle, true},
		{"https://newassets.hcaptcha.com/captcha/v1/x/static/hcaptcha.html", KindHCaptcha, ModeUserPuzzle, true},
		{"https://www.google.com/maps", "", "", false},
		{"https://www.google.com/recaptcha/api2/other", "", "", false},
		{"http://127.0.0.1:8080/x", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		kind, mode, ok := classify(c.src)
		if kind != c.kind || mode != c.mode || ok != c.ok {
			t.Errorf("classify(%q) = %q %q %v, want %q %q %v", c.src, kind, mode, ok, c.kind, c.mode, c.ok)
		}
	}
}

func TestFindMarkersPiercesShadowRoot(t *testing.T) {
	iframe := func(src string) *domNode {
		return &domNode{NodeName: "IFRAME", Attributes: []string{"src", src}}
	}
	shadowFrame := func(src string) *domNode {
		return &domNode{NodeName: "DIV", ShadowRoots: []*domNode{{NodeName: "#document-fragment", Children: []*domNode{iframe(src)}}}}
	}
	title := func(s string) *domNode {
		return &domNode{NodeName: "TITLE", Children: []*domNode{{NodeType: 3, NodeValue: s}}}
	}
	page := func(head []*domNode, body ...*domNode) *domNode {
		return &domNode{NodeName: "#document", Children: []*domNode{{NodeName: "HTML", Children: []*domNode{
			{NodeName: "HEAD", Children: head},
			{NodeName: "BODY", Children: body},
		}}}}
	}
	orchestrate := &domNode{NodeName: "SCRIPT", Attributes: []string{"src", "/cdn-cgi/challenge-platform/h/b/orchestrate/x.js"}}
	beacon := &domNode{NodeName: "SCRIPT", Attributes: []string{"src", "/cdn-cgi/challenge-platform/scripts/jsd/main.js"}}

	cases := []struct {
		name         string
		root         *domNode
		interstitial bool
		frames       int
	}{
		{"interstitial by title and script", page([]*domNode{title("Just a moment..."), orchestrate}, shadowFrame("https://challenges.cloudflare.com/x")), true, 1},
		{"interstitial by challenge id", page(nil, &domNode{NodeName: "DIV", Attributes: []string{"id", "challenge-form"}}), true, 0},
		{"beacon script alone is not a challenge", page([]*domNode{title("Example"), beacon}, shadowFrame("https://www.google.com/recaptcha/api2/anchor")), false, 1},
	}
	for _, c := range cases {
		interstitial, frames := findMarkers(c.root)
		if interstitial != c.interstitial || len(frames) != c.frames {
			t.Errorf("%s: interstitial=%v frames=%d, want %v %d", c.name, interstitial, len(frames), c.interstitial, c.frames)
		}
	}
}

func TestConfirmWindow(t *testing.T) {
	if got := (Options{Pace: 1}).confirmWindow(); got != noWidgetWindow {
		t.Errorf("Pace 1 window = %v, want %v", got, noWidgetWindow)
	}
	if got := (Options{}).confirmWindow(); got != 0 {
		t.Errorf("Pace 0 window = %v, want 0 (fast tests)", got)
	}
	if got := (Options{Pace: 100}).confirmWindow(); got != noWidgetWindowPaced {
		t.Errorf("Pace 100 window = %v, want clamp %v", got, noWidgetWindowPaced)
	}
}

func TestClickGuardAllowsOneClick(t *testing.T) {
	var g clickGuard
	if err := g.claim(); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := g.claim(); !errors.Is(err, ErrAlreadyClicked) {
		t.Fatalf("second claim = %v, want ErrAlreadyClicked", err)
	}
}

// --- real-browser tests: run only with RELO_WEB_BROWSER_TESTS=1, never headed ---

// reports collects the fixture's click reports, sent by the widget frames as image requests.
type reports struct {
	mu sync.Mutex
	q  []string
}

func (r *reports) add(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.q = append(r.q, q)
}

func (r *reports) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.q = nil
}

// count returns how many reports contain sub.
func (r *reports) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.q {
		if strings.Contains(q, sub) {
			n++
		}
	}
	return n
}

const interstitialPage = `<!doctype html><html><head><title>Just a moment...</title>
<script src="/cdn-cgi/challenge-platform/h/b/orchestrate/x.js"></script></head>
<body><div id="challenge-form"></div></body></html>`

func page(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// shadowFrame hosts an iframe inside an open shadow root, so only a pierced DOM sees it.
func shadowFrame(src string) string {
	return `<!doctype html><html><body style="margin:0"><div id="host" style="position:absolute;left:0;top:0"></div><script>
const root = document.getElementById('host').attachShadow({mode: 'open'});
root.innerHTML = '<iframe src="` + src + `" width="300" height="150" style="border:0"></iframe>';
</script></body></html>`
}

// lateFrame injects the widget iframe from page script after a delay. With a
// delay the first scan of the page finds nothing. Injection always happens
// after load, so Chromium attaches the OOPIF with an empty targetInfo url: the
// case that needs the frame resolved by its session rather than by URL.
func lateFrame(src string, after time.Duration) string {
	return `<!doctype html><html><body style="margin:0"><div id="host" style="position:absolute;left:0;top:0"></div><script>
setTimeout(() => {
  document.getElementById('host').innerHTML = '<iframe src="` + src + `" width="300" height="150" style="border:0"></iframe>';
}, ` + strconv.FormatInt(after.Milliseconds(), 10) + `);
</script></body></html>`
}

// checkboxPage is the widget frame. Its checkbox sits in a shadow root and reports
// each mousedown: whether the event was trusted and landed inside the box.
// In mode=solve the box is checked on mousedown; in mode=stuck it never checks.
func checkboxPage(anchor bool) string {
	return checkboxBody(anchor, `if (mode === 'solve') cb.setAttribute('aria-checked', 'true');`) +
		`</script></body></html>`
}

// checkboxBody is all of the widget frame's HTML except the closing tags. The
// trailing report callback is closed but mode-specific checking is not.
func checkboxBody(anchor bool, check string) string {
	el := `<input type="checkbox" id="cb" style="position:absolute;left:100px;top:60px;width:20px;height:20px;margin:0">`
	if anchor {
		el = `<div id="recaptcha-anchor" role="checkbox" aria-checked="false" style="position:absolute;left:100px;top:60px;width:28px;height:28px"></div>`
	}
	return `<!doctype html><html><body style="margin:0"><div id="host"></div><script>
const root = document.getElementById('host').attachShadow({mode: 'open'});
root.innerHTML = '` + el + `';
const cb = root.firstElementChild;
const mode = new URLSearchParams(location.search).get('mode');
cb.addEventListener('mousedown', (e) => {
  const b = cb.getBoundingClientRect();
  const inside = e.clientX >= b.left && e.clientX <= b.right && e.clientY >= b.top && e.clientY <= b.bottom;
  new Image().src = '/report?ev=down&trusted=' + (e.isTrusted ? 1 : 0) + '&inside=' + (inside ? 1 : 0);
  ` + check + `});`
}

// lateCheckboxPage always solves on mousedown. Its widget ignores clicks for its
// first 1.5s, like a backend that is not ready yet, but the page queues the
// mousedown that arrived meanwhile and replays it once ready: a real widget
// keeps the first click, it does not make the user click twice.
func lateCheckboxPage() string {
	return checkboxBody(false, `if (readyAt) { pending = true; } else { cb.setAttribute('aria-checked', 'true'); }`) +
		`var readyAt = true, pending = false;
setTimeout(() => {
  readyAt = false;
  if (pending) cb.setAttribute('aria-checked', 'true');
}, 1500);
` + "</script></body></html>"
}

// challengeServer serves the top pages and the widget frames. Every widget host
// (challenges.cloudflare.com, www.google.com) is mapped to this server by
// --host-resolver-rules, so iframes are cross-site and become OOPIFs.
func challengeServer(rep *reports) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/top/interstitial", func(w http.ResponseWriter, r *http.Request) { page(w, interstitialPage) })
	mux.HandleFunc("/top/puzzle", func(w http.ResponseWriter, r *http.Request) {
		page(w, shadowFrame("https://www.google.com:"+portOf(r)+"/recaptcha/api2/bframe"))
	})
	mux.HandleFunc("/top/turnstile", func(w http.ResponseWriter, r *http.Request) {
		page(w, shadowFrame("https://challenges.cloudflare.com:"+portOf(r)+"/turnstile?mode="+r.URL.Query().Get("mode")))
	})
	mux.HandleFunc("/top/late", func(w http.ResponseWriter, r *http.Request) {
		page(w, lateFrame("https://challenges.cloudflare.com:"+portOf(r)+"/turnstile-late?mode=solve", 0))
	})
	mux.HandleFunc("/top/scripted", func(w http.ResponseWriter, r *http.Request) {
		page(w, lateFrame("https://challenges.cloudflare.com:"+portOf(r)+"/turnstile-scripted?mode=solve", 0))
	})
	mux.HandleFunc("/turnstile-scripted", func(w http.ResponseWriter, r *http.Request) {
		page(w, checkboxPage(false))
	})
	mux.HandleFunc("/top/scripted-anchor", func(w http.ResponseWriter, r *http.Request) {
		page(w, lateFrame("https://www.google.com:"+portOf(r)+"/recaptcha/api2/anchor?mode=solve", 0))
	})
	mux.HandleFunc("/turnstile-late", func(w http.ResponseWriter, r *http.Request) {
		page(w, lateCheckboxPage())
	})
	mux.HandleFunc("/top/recaptcha", func(w http.ResponseWriter, r *http.Request) {
		page(w, shadowFrame("https://www.google.com:"+portOf(r)+"/recaptcha/api2/anchor?mode="+r.URL.Query().Get("mode")))
	})
	mux.HandleFunc("/turnstile", func(w http.ResponseWriter, r *http.Request) { page(w, checkboxPage(false)) })
	mux.HandleFunc("/recaptcha/api2/anchor", func(w http.ResponseWriter, r *http.Request) { page(w, checkboxPage(true)) })
	mux.HandleFunc("/recaptcha/api2/bframe", func(w http.ResponseWriter, r *http.Request) {
		page(w, `<!doctype html><body style="margin:0;background:#fff">puzzle</body>`)
	})
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		rep.add(r.URL.RawQuery)
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewTLSServer(mux)
}

// portOf returns the port the request arrived on, so fixture URLs can name it.
func portOf(r *http.Request) string {
	_, port, _ := strings.Cut(r.Host, ":")
	return port
}

// openPage creates a target, attaches a flattened session, sets the genuine UA and navigates.
func openPage(t *testing.T, ctx context.Context, p *browser.Process, target string) string {
	t.Helper()
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
		t.Fatalf("SetUserAgent: %v", err)
	}
	if err := s.Call(ctx, "Page.enable", nil, nil); err != nil {
		t.Fatalf("Page.enable: %v", err)
	}
	sub := s.Subscribe()
	defer sub.Unsubscribe()
	if err := s.Call(ctx, "Page.navigate", map[string]any{"url": target}, nil); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if _, err := cdp.WaitEvent(ctx, sub, "Page.loadEventFired"); err != nil {
		t.Fatalf("waiting for load: %v", err)
	}
	return s.ID()
}

func TestBrowserChallenges(t *testing.T) {
	if os.Getenv("RELO_WEB_BROWSER_TESTS") != "1" {
		t.Skip("set RELO_WEB_BROWSER_TESTS=1 to drive a real browser")
	}
	if _, err := browser.Find(""); err != nil {
		t.Skip("no Chromium-family browser installed")
	}
	rep := &reports{}
	srv := challengeServer(rep)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	p, err := browser.Launch(ctx, browser.Options{
		Lane:     browser.LaneHidden,
		StateDir: t.TempDir(),
		ExtraArgs: []string{
			"--host-resolver-rules=MAP challenges.cloudflare.com 127.0.0.1,MAP www.google.com 127.0.0.1",
			"--ignore-certificate-errors", // test fixtures only: self-signed TLS server
		},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	cases := []struct {
		name    string
		path    string
		pace    float64 // 0 keeps the built-in case fast; nonzero scales human-paced delays
		outcome Outcome
		clicks  int
		kind    Kind
		mode    Mode
	}{
		{"puzzle_left_to_user", "/top/puzzle", 0, NeedsUser, 0, KindRecaptchaV2, ModeUserPuzzle},
		{"interstitial_without_checkbox", "/top/interstitial", 0, StillChallenged, 0, KindInterstitial, ModeAutoWait},
		{"turnstile_solves_after_one_click", "/top/turnstile?mode=solve", 0, Solved, 1, KindTurnstile, ModeAutoCheckbox},
		{"turnstile_stuck_still_one_click", "/top/turnstile?mode=stuck", 0, StillChallenged, 1, KindTurnstile, ModeAutoCheckbox},
		{"recaptcha_anchor_solves_after_one_click", "/top/recaptcha?mode=solve", 0, Solved, 1, KindRecaptchaV2, ModeAutoCheckbox},
		// Injected 1.5s after load, while the scan window is open; its OOPIF
		// attach carries an empty url, so it is found through its frame tree.
		{"late_widget_detected_and_clicked", "/top/late", 0, Solved, 1, KindTurnstile, ModeAutoCheckbox},
		// Widget iframe written by page script right after load: its OOPIF attach
		// carries an empty url and it is not visible to the first scan.
		{"scripted_widget_detected_and_clicked", "/top/scripted", 0, Solved, 1, KindTurnstile, ModeAutoCheckbox},
		// Same for a recaptcha anchor, which clears by aria-checked on click.
		{"scripted_anchor_detected_and_clicked", "/top/scripted-anchor", 0, Solved, 1, KindRecaptchaV2, ModeAutoCheckbox},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep.reset()
			// 120s so a stuck case (auto-wait 1s + confirm 3s + click + verify) still fits.
			sctx, scancel := context.WithTimeout(ctx, 120*time.Second)
			defer scancel()
			sid := openPage(t, sctx, p, srv.URL+c.path)

			var attempts []Attempt
			res, err := Run(sctx, p.Conn, sid, Options{
				AutoWait:   time.Second,
				AfterClick: 1500 * time.Millisecond,
				Pace:       c.pace,
				Log:        func(a Attempt) { attempts = append(attempts, a) },
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Outcome != c.outcome {
				t.Errorf("outcome = %q, want %q (attempts %+v)", res.Outcome, c.outcome, attempts)
			}
			if res.Clicked != (c.clicks > 0) {
				t.Errorf("clicked = %v, want %v", res.Clicked, c.clicks > 0)
			}
			found := false
			for _, w := range res.Widgets {
				found = found || (w.Kind == c.kind && w.Mode == c.mode)
			}
			if !found {
				t.Errorf("widgets %+v: no %s/%s", res.Widgets, c.kind, c.mode)
			}
			if !res.Detected {
				t.Errorf("detected = false, want a challenge")
			}
			if got := rep.count("ev=down"); got != c.clicks {
				t.Errorf("mousedown count = %d, want %d", got, c.clicks)
			}
			if got := rep.count("trusted=1&inside=1"); got != c.clicks {
				t.Errorf("trusted inside-box mousedown count = %d, want %d", got, c.clicks)
			}
		})
	}
}
