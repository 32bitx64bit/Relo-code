package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relocode/relo/internal/web/cdp"
)

// Real-browser tests run only when RELO_WEB_BROWSER_TESTS=1. They never launch a
// headed window: only the hidden lane is started here.
func requireBrowser(t *testing.T) Installed {
	t.Helper()
	if os.Getenv("RELO_WEB_BROWSER_TESTS") != "1" {
		t.Skip("set RELO_WEB_BROWSER_TESTS=1 to drive a real browser")
	}
	inst, err := Find("")
	if errors.Is(err, ErrNotFound) {
		t.Skip("no Chromium-family browser installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func launchHidden(t *testing.T, extra ...string) *Process {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := Launch(ctx, Options{Lane: LaneHidden, StateDir: t.TempDir(), ExtraArgs: extra})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// openPage creates a target, attaches a flattened session and navigates to url.
func openPage(t *testing.T, p *Process, ctx context.Context, url string) *cdp.Session {
	t.Helper()
	return openPageWith(t, p, ctx, url, nil)
}

// openPageWith is openPage with a hook that runs on the session before navigation.
func openPageWith(t *testing.T, p *Process, ctx context.Context, url string, before func(*cdp.Session) error) *cdp.Session {
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
	if err := s.Call(ctx, "Page.enable", nil, nil); err != nil {
		t.Fatalf("Page.enable: %v", err)
	}
	if before != nil {
		if err := before(s); err != nil {
			t.Fatalf("pre-navigate hook: %v", err)
		}
	}
	sub := s.Subscribe()
	defer sub.Unsubscribe()
	if err := s.Call(ctx, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if _, err := cdp.WaitEvent(ctx, sub, "Page.loadEventFired"); err != nil {
		t.Fatalf("waiting for load: %v", err)
	}
	return s
}

// --- tests that run without a browser ---

func TestBuildArgsLanes(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "hidden")
	hidden := buildArgs(LaneHidden, profile, "", false)
	hiddenProxied := buildArgs(LaneHidden, profile, "http://proxy.invalid:8080", false)
	window := buildArgs(LaneWindow, profile, "http://proxy.invalid:8080", false)
	softGL := buildArgs(LaneWindow, profile, "", true)

	joined := func(a []string) string { return strings.Join(a, "\n") }
	for _, bad := range forbiddenFlags {
		for _, a := range [][]string{hidden, window, softGL} {
			if strings.Contains(joined(a), bad) {
				t.Errorf("forbidden flag %s present: %v", bad, a)
			}
		}
	}
	mustHave := func(name string, args []string, flags ...string) {
		t.Helper()
		for _, f := range flags {
			found := false
			for _, a := range args {
				if a == f || strings.HasPrefix(a, f+"=") {
					found = true
				}
			}
			if !found {
				t.Errorf("%s lane missing %s: %v", name, f, args)
			}
		}
	}
	mustNotHave := func(name string, args []string, flags ...string) {
		t.Helper()
		for _, f := range flags {
			for _, a := range args {
				if a == f || strings.HasPrefix(a, f+"=") {
					t.Errorf("%s lane must not have %s: %v", name, f, args)
				}
			}
		}
	}
	// The measured anti-detection flags belong to both lanes: the window lane
	// is judged by the same pages as the hidden one.
	stealth := []string{
		"--remote-debugging-pipe",
		"--user-data-dir",
		"--window-size",
		"--force-webrtc-ip-handling-policy",
		"--disable-blink-features=AutomationControlled",
		"--use-gl=angle",
		"--use-angle=gl",
	}
	mustHave("hidden", hidden, append(stealth, "--headless", "--screen-info")...)
	mustHave("window", window, append(stealth, "--proxy-server")...)
	// Security review: whenever --proxy-server is emitted, loopback must be
	// routed through it too. Chromium bypasses its proxy for 127.0.0.1 and
	// localhost by default, so a page fetch of a loopback origin would skip the
	// netguard proxy entirely. Both lanes get it, because the escalation window
	// is pointed at the same guard proxy as the hidden lane.
	mustHave("hidden", hiddenProxied, "--proxy-server", "--proxy-bypass-list=<-loopback>")
	mustHave("window", window, "--proxy-server", "--proxy-bypass-list=<-loopback>")
	// Without a proxy there is nothing to bypass.
	mustNotHave("hidden", hidden, "--proxy-server", "--proxy-bypass-list")
	for _, a := range window {
		if a == "--headless=new" || strings.HasPrefix(a, "--headless") || strings.HasPrefix(a, "--screen-info") {
			t.Errorf("window lane must be headed, got %s", a)
		}
	}
	// SoftwareGL is the opt-out for hosts where the ANGLE flags do not start.
	mustHave("softwareGL", softGL, "--user-data-dir", "--window-size",
		"--disable-blink-features=AutomationControlled")
	mustNotHave("softwareGL", softGL, "--use-gl=angle", "--use-angle=gl")
	if !strings.Contains(joined(hidden), "--user-data-dir="+profile) {
		t.Errorf("hidden lane not using dedicated profile: %v", hidden)
	}
	if !strings.Contains(joined(window), "--user-data-dir="+profile) {
		t.Errorf("window lane not using dedicated profile: %v", window)
	}
}

func TestLaunchValidation(t *testing.T) {
	if _, err := Launch(context.Background(), Options{Lane: LaneHidden}); err == nil {
		t.Error("expected error without StateDir")
	}
	if _, err := Launch(context.Background(), Options{Lane: "bogus", StateDir: t.TempDir()}); err == nil {
		t.Error("expected error for unknown lane")
	}
	if _, err := Find(filepath.Join(t.TempDir(), "missing-browser")); err == nil {
		t.Error("expected error for missing configured browser path")
	}
}

// --- shutdown: no browser needed ---

// fakeBrowserExitCode is the status the stand-in browser exits with once the
// pipe is closed, so a surfaced exit error can be told from a swallowed one.
const fakeBrowserExitCode = 3

// fakeExitDelay is how long the stand-in browser takes to exit after the pipe
// closes, so a Close that returns early is observable.
const fakeExitDelay = 300 * time.Millisecond

// TestFakeBrowserChild is not a test. The test binary re-executes itself with
// RELO_WEB_FAKE_BROWSER=1 to stand in for a browser: it reads the CDP pipe (fd
// 3) until the parent closes it, then exits non-zero, the way a browser that
// died does.
func TestFakeBrowserChild(t *testing.T) {
	if os.Getenv("RELO_WEB_FAKE_BROWSER") != "1" {
		t.Skip("helper process; started by startFake")
	}
	pipe := os.NewFile(3, "cdp-command")
	defer pipe.Close()
	_, _ = io.Copy(io.Discard, pipe)
	time.Sleep(fakeExitDelay)
	os.Exit(fakeBrowserExitCode)
}

// startFake starts the stand-in browser through the real start path.
func startFake(t *testing.T) *Process {
	t.Helper()
	t.Setenv("RELO_WEB_FAKE_BROWSER", "1")
	p, err := start(Installed{Path: os.Args[0], Family: "fake"}, LaneHidden, t.TempDir(),
		[]string{"-test.run=TestFakeBrowserChild"})
	if err != nil {
		t.Fatalf("start stand-in browser: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// wantExit fails unless err is the stand-in browser's exit error.
func wantExit(t *testing.T, err error) {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != fakeBrowserExitCode {
		t.Errorf("Close = %v, want exit status %d", err, fakeBrowserExitCode)
	}
}

// TestCloseWaitsForExit covers the shutdown contract: every Close call blocks
// until the process is gone and returns the error it exited with.
func TestCloseWaitsForExit(t *testing.T) {
	p := startFake(t)
	const callers = 4
	errs := make(chan error, callers)
	release := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			<-release
			errs <- p.Close()
		}()
	}
	started := time.Now()
	close(release)
	for i := 0; i < callers; i++ {
		select {
		case err := <-errs:
			wantExit(t, err)
		case <-time.After(2 * shutdownGrace):
			t.Fatal("Close did not return")
		}
	}
	if elapsed := time.Since(started); elapsed < fakeExitDelay {
		t.Errorf("Close returned after %v; the browser exits %v after the pipe closes", elapsed, fakeExitDelay)
	}
	if p.cmd.ProcessState == nil {
		t.Error("Close returned before the process was reaped")
	}
	// A Close after the process is gone reports the same exit error.
	wantExit(t, p.Close())
}

// No Runtime.enable anywhere in the production sources of this package tree.
func TestNoRuntimeEnable(t *testing.T) {
	for _, dir := range []string{".", "../cdp"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
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
			if strings.Contains(string(b), "Runtime.enable") {
				t.Errorf("%s sends Runtime.enable", f)
			}
		}
	}
}

// --- real browser tests (env-gated, hidden lane only) ---

func TestPipeSmoke(t *testing.T) {
	requireBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>relo-ok</title><p>hello</p>`))
	}))
	defer srv.Close()

	p := launchHidden(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if p.Version == "" {
		t.Fatal("Browser.getVersion returned no product")
	}
	s := openPage(t, p, ctx, srv.URL+"/")
	title, err := s.Evaluate(ctx, "document.title")
	if err != nil {
		t.Fatal(err)
	}
	if title != "relo-ok" {
		t.Fatalf("title = %q, want relo-ok", title)
	}
	t.Logf("version=%s", p.Version)
}

// TestLoopbackGoesThroughProxy is the security-review regression check for the
// bypass list: with --proxy-bypass-list=<-loopback>, a page fetch of a loopback
// origin must reach the guard proxy, not the origin. Chromium bypasses its
// proxy for 127.0.0.1 by default, so without the flag the origin answers
// directly and the guard never sees the request.
func TestLoopbackGoesThroughProxy(t *testing.T) {
	requireBrowser(t)

	const proxyTitle = "relo-via-proxy"
	// The origin is never supposed to be reached: it answers with a different
	// title and fails the test if it is served at all.
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("loopback origin reached directly, bypassing the proxy: %s %s", r.Method, r.URL)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>relo-direct</title>`))
	}))
	defer direct.Close()

	var hits atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><title>%s</title>`, proxyTitle)
	}))
	defer guard.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := Launch(ctx, Options{Lane: LaneHidden, StateDir: t.TempDir(), Proxy: guard.URL})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	s := openPage(t, p, ctx, direct.URL+"/")
	title, err := s.Evaluate(ctx, "document.title")
	if err != nil {
		t.Fatal(err)
	}
	if title != proxyTitle {
		t.Errorf("title = %q, want %q: the loopback request did not go through the proxy", title, proxyTitle)
	}
	if got := hits.Load(); got == 0 {
		t.Error("guard proxy saw no request")
	}
}

// fingerprint is measured read-only from the page; no JS property is patched.
const fingerprintJS = `(async () => {
  const o = {};
  o.webdriver = navigator.webdriver;
  o.ua = navigator.userAgent;
  const u = navigator.userAgentData;
  if (u) {
    o.brands = u.brands;
    o.mobile = u.mobile;
    o.uach = await u.getHighEntropyValues(['platform','platformVersion','architecture','bitness','model','uaFullVersion','fullVersionList','wow64']);
  }
  const c = document.createElement('canvas');
  const gl = c.getContext('webgl');
  if (gl) {
    const d = gl.getExtension('WEBGL_debug_renderer_info');
    o.webglRenderer = d ? gl.getParameter(d.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER);
  }
  o.screen = [screen.width, screen.height];
  return JSON.stringify(o);
})()`

type fingerprint struct {
	WebDriver     any               `json:"webdriver"`
	UA            string            `json:"ua"`
	Brands        []json.RawMessage `json:"brands"`
	UACH          map[string]any    `json:"uach"`
	WebGLRenderer string            `json:"webglRenderer"`
	Screen        []int             `json:"screen"`
	ServerSawUA   string            `json:"-"`
	ServerSawCHUA string            `json:"-"`
}

// measure loads a page from srv in s and returns the page-side fingerprint plus
// the headers the server actually received.
func measure(t *testing.T, ctx context.Context, p *Process, srv *httptest.Server, seen *requestHeaders, before func(*cdp.Session) error) fingerprint {
	t.Helper()
	s := openPageWith(t, p, ctx, srv.URL+"/", before)
	raw, err := s.Evaluate(ctx, fingerprintJS)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	var fp fingerprint
	if err := json.Unmarshal([]byte(raw), &fp); err != nil {
		t.Fatalf("decode fingerprint %q: %v", raw, err)
	}
	fp.ServerSawUA, fp.ServerSawCHUA = seen.get()
	return fp
}

type requestHeaders struct {
	mu   sync.Mutex
	ua   string
	chua string
}

func (r *requestHeaders) set(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ua = req.UserAgent()
	r.chua = req.Header.Get("Sec-CH-UA")
}

func (r *requestHeaders) get() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ua, r.chua
}

func newMeasureServer(seen *requestHeaders) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		seen.set(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>measure</title>`))
	}))
}

// TestUserAgentMeasure compares ways to remove the headless and webdriver signals.
// Run with -v to read the numbers. Each variant is one hidden-lane launch.
func TestUserAgentMeasure(t *testing.T) {
	requireBrowser(t)
	var seen requestHeaders
	srv := newMeasureServer(&seen)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Shipped hidden flags (already include AutomationControlled off) and no UA change.
	base := measure(t, ctx, launchHidden(t), srv, &seen, nil)
	t.Logf("V1 shipped flags, no UA change: %s", describe(base))

	// Derived UA: same Chrome build, HeadlessChrome token removed.
	ua := strings.Replace(base.UA, "HeadlessChrome/", "Chrome/", 1)

	v2 := measure(t, ctx, launchHidden(t, "--user-agent="+ua), srv, &seen, nil)
	t.Logf("V2 --user-agent flag: %s", describe(v2))

	v3 := measure(t, ctx, launchHidden(t), srv, &seen, func(s *cdp.Session) error {
		return SetUserAgent(ctx, s)
	})
	t.Logf("V3 SetUserAgent (Emulation+metadata): %s", describe(v3))

	v4 := measure(t, ctx, launchHidden(t), srv, &seen, func(s *cdp.Session) error {
		return s.Call(ctx, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ua}, nil)
	})
	t.Logf("V4 Emulation UA only (no metadata): %s", describe(v4))
}

func describe(fp fingerprint) string {
	return fmt.Sprintf("webdriver=%v ua=%q wire_ua=%q wire_sec_ch_ua=%q uach_platform=%v uach_fullVersion=%v webgl=%q screen=%v",
		fp.WebDriver, fp.UA, fp.ServerSawUA, fp.ServerSawCHUA, fp.UACH["platform"], fp.UACH["uaFullVersion"], fp.WebGLRenderer, fp.Screen)
}

// TestHiddenFingerprint checks the shipped hidden lane: default flags plus SetUserAgent.
func TestHiddenFingerprint(t *testing.T) {
	requireBrowser(t)
	var seen requestHeaders
	srv := newMeasureServer(&seen)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fp := measure(t, ctx, launchHidden(t), srv, &seen, func(s *cdp.Session) error {
		return SetUserAgent(ctx, s)
	})
	t.Logf("shipped hidden: %s", describe(fp))
	if fp.WebDriver != false {
		t.Errorf("navigator.webdriver = %v, want false", fp.WebDriver)
	}
	if strings.Contains(fp.UA, "HeadlessChrome") || strings.Contains(fp.ServerSawUA, "HeadlessChrome") {
		t.Errorf("headless token leaked: page=%q wire=%q", fp.UA, fp.ServerSawUA)
	}
	if fp.UACH["uaFullVersion"] == "" || fp.UACH["uaFullVersion"] == nil {
		t.Errorf("high-entropy uaFullVersion empty: %v", fp.UACH)
	}
	if fp.ServerSawCHUA == "" {
		t.Errorf("Sec-CH-UA not sent")
	}
}
