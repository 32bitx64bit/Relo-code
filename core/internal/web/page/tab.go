// Package page drives one attached browser tab over a cdp.Session.
//
// Reads run only in an isolated world created with Page.createIsolatedWorld and
// evaluated by contextId, so page scripts never see the agent's code. Input is sent
// as trusted Input.dispatch* events with human pacing. The package never enables the
// Runtime domain and never injects scripts before page load.
//
// A Tab is not safe for concurrent use. Call SetUserAgent (package browser) on the
// session before NewTab or Navigate.
package page

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/relocode/relo/internal/web/cdp"
)

// worldName names the isolated world used for reads.
const worldName = "relo-read"

// ErrNoHistory is returned by Back when there is no earlier entry.
var ErrNoHistory = errors.New("page: no earlier history entry")

// Tab is one attached page target.
type Tab struct {
	s     *cdp.Session
	pace  *pacer
	mouse Point // last pointer position sent, in viewport CSS pixels

	elements []Element // numbered by the last Snapshot

	ctxID   int // isolated-world execution context for the current document
	frameID string

	sub    *cdp.Subscription
	hookMu sync.Mutex
	hook   func(url, remoteIP string)
}

// NewTab enables the page and network domains on s and starts the response watcher.
// Lifecycle events are enabled so Navigate can wait for "load".
func NewTab(ctx context.Context, s *cdp.Session) (*Tab, error) {
	for _, m := range []string{"Page.enable", "Network.enable"} {
		if err := s.Call(ctx, m, nil, nil); err != nil {
			return nil, fmt.Errorf("page: %s: %w", m, err)
		}
	}
	if err := s.Call(ctx, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}, nil); err != nil {
		return nil, fmt.Errorf("page: lifecycle events: %w", err)
	}
	t := &Tab{
		s:     s,
		pace:  newPacer(),
		mouse: Point{X: 400, Y: 300},
		sub:   s.Subscribe(),
	}
	go t.watch()
	return t, nil
}

// Close stops the response watcher. It does not detach the session.
func (t *Tab) Close() { t.sub.Unsubscribe() }

// SetPacing sets the human-pacing scale. 1 is human speed; 0 removes all waits.
func (t *Tab) SetPacing(scale float64) {
	if scale < 0 {
		scale = 0
	}
	t.pace.scale = scale
}

// SetRemoteIPHook registers fn to be called for every document response with its
// URL and the remote IP the connection went to. The guard uses it to re-check the
// address after a redirect. Pass nil to remove it.
func (t *Tab) SetRemoteIPHook(fn func(url, remoteIP string)) {
	t.hookMu.Lock()
	t.hook = fn
	t.hookMu.Unlock()
}

// watch delivers Network.responseReceived to the remote-IP hook.
func (t *Tab) watch() {
	for ev := range t.sub.C {
		if ev.Method != "Network.responseReceived" {
			continue
		}
		var p struct {
			Type     string `json:"type"`
			Response struct {
				URL             string `json:"url"`
				RemoteIPAddress string `json:"remoteIPAddress"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.Type != "Document" {
			continue
		}
		t.hookMu.Lock()
		fn := t.hook
		t.hookMu.Unlock()
		if fn != nil {
			fn(p.Response.URL, p.Response.RemoteIPAddress)
		}
	}
}

// Navigate loads url, optionally with a Referer, and waits for the "load" lifecycle
// event of that navigation. A same-document navigation returns without waiting.
func (t *Tab) Navigate(ctx context.Context, url, referrer string) error {
	sub := t.s.Subscribe()
	defer sub.Unsubscribe()

	params := map[string]any{"url": url}
	if referrer != "" {
		params["referrer"] = referrer
	}
	var res struct {
		FrameID   string `json:"frameId"`
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := t.s.Call(ctx, "Page.navigate", params, &res); err != nil {
		return fmt.Errorf("page: navigate: %w", err)
	}
	if res.ErrorText != "" {
		return fmt.Errorf("page: navigate %s: %s", url, res.ErrorText)
	}
	t.resetWorld()
	if res.LoaderID == "" {
		return nil
	}
	return waitLifecycle(ctx, sub, res.LoaderID, "load")
}

// waitLifecycle returns when the lifecycle event name fires for loaderID.
func waitLifecycle(ctx context.Context, sub *cdp.Subscription, loaderID, name string) error {
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return cdp.ErrClosed
			}
			if ev.Method != "Page.lifecycleEvent" {
				continue
			}
			var p struct {
				LoaderID string `json:"loaderId"`
				Name     string `json:"name"`
			}
			if json.Unmarshal(ev.Params, &p) == nil && p.LoaderID == loaderID && p.Name == name {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// URL returns the current entry URL from the navigation history.
func (t *Tab) URL(ctx context.Context) (string, error) {
	h, err := t.history(ctx)
	if err != nil {
		return "", err
	}
	if h.CurrentIndex < 0 || h.CurrentIndex >= len(h.Entries) {
		return "", errors.New("page: no current history entry")
	}
	return h.Entries[h.CurrentIndex].URL, nil
}

type historyEntry struct {
	ID  int    `json:"id"`
	URL string `json:"url"`
}

type history struct {
	CurrentIndex int            `json:"currentIndex"`
	Entries      []historyEntry `json:"entries"`
}

func (t *Tab) history(ctx context.Context) (history, error) {
	var h history
	err := t.s.Call(ctx, "Page.getNavigationHistory", nil, &h)
	return h, err
}

// resetWorld forgets the isolated-world context; it is created again on next read.
func (t *Tab) resetWorld() {
	t.elements = nil
	t.ctxID = 0
	t.frameID = ""
}

// world returns the isolated-world execution context of the main frame, creating it
// on first use. Page.createIsolatedWorld is a Page-domain call that does not run page
// scripts and does not expose the world to the page's JavaScript.
func (t *Tab) world(ctx context.Context) (int, error) {
	if t.ctxID != 0 {
		return t.ctxID, nil
	}
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := t.s.Call(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return 0, fmt.Errorf("page: frame tree: %w", err)
	}
	var iso struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	params := map[string]any{
		"frameId":              tree.FrameTree.Frame.ID,
		"worldName":            worldName,
		"grantUniversalAccess": false,
	}
	if err := t.s.Call(ctx, "Page.createIsolatedWorld", params, &iso); err != nil {
		return 0, fmt.Errorf("page: isolated world: %w", err)
	}
	t.frameID = tree.FrameTree.Frame.ID
	t.ctxID = iso.ExecutionContextID
	return t.ctxID, nil
}

// evalIsolated runs expr in the isolated world and returns its string value.
// If the document was replaced and the context is gone, it re-creates the world once.
func (t *Tab) evalIsolated(ctx context.Context, expr string) (string, error) {
	for attempt := 0; ; attempt++ {
		id, err := t.world(ctx)
		if err != nil {
			return "", err
		}
		out, err := t.evalIn(ctx, id, expr)
		if err != nil && attempt == 0 && strings.Contains(err.Error(), "Cannot find context") {
			t.resetWorld()
			continue
		}
		return out, err
	}
}

func (t *Tab) evalIn(ctx context.Context, contextID int, expr string) (string, error) {
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	err := t.s.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"contextId":     contextID,
		"returnByValue": true,
	}, &out)
	if err != nil {
		return "", err
	}
	if len(out.ExceptionDetails) > 0 {
		return "", errors.New("page: isolated read threw")
	}
	return out.Result.Value, nil
}

// HTML returns the document's outerHTML, read from the isolated world.
// The result is untrusted web content.
func (t *Tab) HTML(ctx context.Context) (string, error) {
	return t.evalIsolated(ctx, `document.documentElement ? document.documentElement.outerHTML : ""`)
}

// Text returns the rendered text of the body, read from the isolated world.
// The result is untrusted web content.
func (t *Tab) Text(ctx context.Context) (string, error) {
	return t.evalIsolated(ctx, `document.body ? document.body.innerText : ""`)
}

// Screenshot captures the viewport as PNG bytes.
func (t *Tab) Screenshot(ctx context.Context) ([]byte, error) {
	var out struct {
		Data string `json:"data"`
	}
	if err := t.s.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"}, &out); err != nil {
		return nil, fmt.Errorf("page: screenshot: %w", err)
	}
	return base64.StdEncoding.DecodeString(out.Data)
}

// Cookie is the lane-transfer form of a browser cookie.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"` // seconds since epoch; omitted for session cookies
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite,omitempty"`
}

// GetCookies returns all cookies of the profile.
func (t *Tab) GetCookies(ctx context.Context) ([]Cookie, error) {
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := t.s.Call(ctx, "Network.getAllCookies", nil, &out); err != nil {
		return nil, fmt.Errorf("page: get cookies: %w", err)
	}
	return out.Cookies, nil
}

// SetCookies writes cookies into the profile.
func (t *Tab) SetCookies(ctx context.Context, cookies []Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	// Session cookies come back with expires -1; send them without an expiry.
	for i := range cookies {
		if cookies[i].Expires < 0 {
			cookies[i].Expires = 0
		}
	}
	if err := t.s.Call(ctx, "Network.setCookies", map[string]any{"cookies": cookies}, nil); err != nil {
		return fmt.Errorf("page: set cookies: %w", err)
	}
	return nil
}
