package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/relocode/relo/internal/web/browser"
	"github.com/relocode/relo/internal/web/cdp"
	"github.com/relocode/relo/internal/web/challenge"
	"github.com/relocode/relo/internal/web/netguard"
	"github.com/relocode/relo/internal/web/page"
)

// browserTab is the page functionality the engine uses. The real implementation
// owns both a page.Tab and its target; tests can supply a small fake.
type browserTab interface {
	Navigate(ctx context.Context, rawURL, referrer string) error
	URL(ctx context.Context) (string, error)
	Snapshot(ctx context.Context) ([]page.Element, error)
	Click(ctx context.Context, ref int) error
	Type(ctx context.Context, ref int, text string) error
	Press(ctx context.Context, key string) error
	Scroll(ctx context.Context, dy int) error
	ScrollY(ctx context.Context) (int, error)
	ViewportHeight(ctx context.Context) (int, error)
	Back(ctx context.Context) error
	HTML(ctx context.Context) (string, error)
	Text(ctx context.Context) (string, error)
	Screenshot(ctx context.Context) ([]byte, error)
	GetCookies(ctx context.Context) ([]page.Cookie, error)
	SetCookies(ctx context.Context, cookies []page.Cookie) error
	RunChallenge(ctx context.Context, options challenge.Options) (challenge.Result, error)
	Close() error
}

type browserLane interface {
	Open(context.Context) (browserTab, error)
	Close() error
}

type laneFactory func(context.Context, Tier, Settings, string) (browserLane, error)

// launchFunc starts one browser process. It is a variable so a test can supply
// a fake launcher instead of a real browser.
var launchFunc = browser.Launch

type laneSlot struct {
	ready chan struct{}
	lane  browserLane
	err   error
}

// guardProxy allows the custom origin to change without changing the address
// already baked into a running browser's --proxy-server flag.
type guardProxy struct {
	mu      sync.RWMutex
	origin  netguard.Origin
	server  *http.Server
	address string
}

func newGuardProxy(origin netguard.Origin) (*guardProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("web: listen for guard proxy: %w", err)
	}
	p := &guardProxy{origin: origin, address: "http://" + listener.Addr().String()}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *guardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	origin := p.origin
	p.mu.RUnlock()
	(&netguard.Proxy{AllowOrigin: origin}).ServeHTTP(w, r)
}

func (p *guardProxy) setOrigin(origin netguard.Origin) {
	p.mu.Lock()
	p.origin = origin
	p.mu.Unlock()
}

func (p *guardProxy) Close() error {
	if p == nil || p.server == nil {
		return nil
	}
	return p.server.Close()
}

// processLane is one lazily launched browser process shared by agent tabs.
type processLane struct {
	process *browser.Process
	hooks   Hooks
	tier    Tier
}

func launchBrowserLane(ctx context.Context, tier Tier, settings Settings, proxy string, hooks Hooks) (browserLane, error) {
	return launchBrowserLaneWithRetry(ctx, tier, settings, proxy, hooks, false, nil)
}

// launchBrowserLaneWithRetry starts a lane, retrying once with software GL when
// the first launch fails. software already tells it to skip the failing attempt;
// remember records a new software choice for later launches.
func launchBrowserLaneWithRetry(ctx context.Context, tier Tier, settings Settings, proxy string, hooks Hooks, software bool, remember func(Tier)) (browserLane, error) {
	process, usedSoftware, err := launchLaneProcess(ctx, tier, settings, proxy, hooks, software)
	if err != nil {
		return nil, err
	}
	if usedSoftware && remember != nil {
		remember(tier)
	}
	return &processLane{process: process, hooks: hooks, tier: tier}, nil
}

// launchLaneProcess starts the lane's browser. When software is true the
// measured ANGLE flags are dropped immediately; otherwise a failed launch is
// retried once with software GL. The second result reports whether software
// rendering was used, so the caller can remember the choice.
func launchLaneProcess(ctx context.Context, tier Tier, settings Settings, proxy string, hooks Hooks, software bool) (*browser.Process, bool, error) {
	lane := browser.LaneHidden
	if tier == TierWindow {
		lane = browser.LaneWindow
	}
	opts := browser.Options{
		Lane: lane, StateDir: settings.StateDir, BrowserPath: settings.BrowserPath, Proxy: proxy,
		SoftwareGL: software,
	}
	process, err := launchFunc(ctx, opts)
	if err == nil {
		return process, software, nil
	}
	if software {
		return nil, false, err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return nil, false, err
	}
	if hooks.Log != nil {
		hooks.Log(fmt.Sprintf("web lane=%s ANGLE launch failed; retrying with software GL: %v", tier, err))
	}
	opts.SoftwareGL = true
	process, retryErr := launchFunc(ctx, opts)
	if retryErr != nil {
		if hooks.Log != nil {
			hooks.Log(fmt.Sprintf("web lane=%s software GL retry failed: %v", tier, retryErr))
		}
		return nil, false, fmt.Errorf("%w (software GL retry: %v)", err, retryErr)
	}
	if hooks.Log != nil {
		hooks.Log(fmt.Sprintf("web lane=%s launched with software GL", tier))
	}
	return process, true, nil
}

func (l *processLane) Open(ctx context.Context) (browserTab, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := l.process.Conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return nil, fmt.Errorf("web: create target: %w", err)
	}
	tab := &cdpBrowserTab{conn: l.process.Conn, targetID: created.TargetID}
	session, err := l.process.Conn.Attach(ctx, created.TargetID)
	if err != nil {
		_ = tab.Close()
		return nil, fmt.Errorf("web: attach target: %w", err)
	}
	tab.session = session
	ok := false
	defer func() {
		if !ok {
			_ = tab.Close()
		}
	}()
	if err := browser.SetUserAgent(ctx, session); err != nil {
		return nil, err
	}
	logStealthSelfCheck(ctx, session, l.tier, l.hooks)
	pageTab, err := page.NewTab(ctx, session)
	if err != nil {
		return nil, err
	}
	tab.Tab = pageTab
	ok = true
	return tab, nil
}

func (l *processLane) Close() error { return l.process.Close() }

type cdpBrowserTab struct {
	*page.Tab
	conn      *cdp.Conn
	session   *cdp.Session
	targetID  string
	closeOnce sync.Once
	closeErr  error
}

func (t *cdpBrowserTab) RunChallenge(ctx context.Context, options challenge.Options) (challenge.Result, error) {
	return challenge.Run(ctx, t.conn, t.session.ID(), options)
}

func (t *cdpBrowserTab) Close() error {
	t.closeOnce.Do(func() {
		if t.Tab != nil {
			t.Tab.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t.closeErr = t.conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.targetID}, nil)
		if errors.Is(t.closeErr, cdp.ErrClosed) {
			t.closeErr = nil
		}
	})
	return t.closeErr
}
