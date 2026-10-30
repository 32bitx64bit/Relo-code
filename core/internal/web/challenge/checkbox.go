package challenge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/web/cdp"
)

// Outcome is the end state of Run.
type Outcome string

const (
	Solved          Outcome = "solved"
	StillChallenged Outcome = "still_challenged"
	NeedsUser       Outcome = "needs_user"
)

// ErrAlreadyClicked is returned when a second checkbox click is attempted in one challenge.
var ErrAlreadyClicked = errors.New("challenge: checkbox already clicked for this challenge")

// Attempt is one logged step. Stage is detect, wait, click or verify.
type Attempt struct {
	Stage   string
	Kind    Kind
	Mode    Mode
	Outcome Outcome // set on detect and verify, empty otherwise
	Detail  string  // counts and hostnames only; never page text
}

// Options tunes Run. The zero Pace disables human delays and the no-challenge
// confirmation (tests only).
type Options struct {
	AutoWait     time.Duration // bounded wait for the page to auto-resolve before any click
	AfterClick   time.Duration // bounded wait to verify the click
	Pace         float64       // scale on human delays; 0 skips them
	DisableClick bool          // wait and detect only; never click a checkbox
	Log          func(Attempt)
}

// DefaultOptions returns the shipped timing: 15s auto-wait, 10s verify, full pacing.
func DefaultOptions() Options {
	return Options{AutoWait: 15 * time.Second, AfterClick: 10 * time.Second, Pace: 1}
}

const (
	// noWidgetWindow is how long Run keeps scanning before reporting no challenge,
	// so a widget injected by page script after load is still seen.
	noWidgetWindow = 6 * time.Second
	// noWidgetWindowPaced keeps the scan window bounded even when Pace is large.
	noWidgetWindowPaced = 12 * time.Second
	// noWidgetPoll is the gap between scans of the empty page.
	noWidgetPoll = 500 * time.Millisecond
)

// confirmWindow scales the no-challenge confirmation by Pace, clamped to a bound.
func (o Options) confirmWindow() time.Duration {
	if o.Pace <= 0 {
		return 0
	}
	return min(time.Duration(float64(noWidgetWindow)*o.Pace), noWidgetWindowPaced)
}

// Result is what Run found and did.
type Result struct {
	Outcome  Outcome
	Detected bool
	Widgets  []Widget
	Clicked  bool
}

// clickGuard allows exactly one checkbox click per Run.
type clickGuard struct {
	mu   sync.Mutex
	used bool
}

// claim reserves the single click. A failed click does not re-arm the guard.
func (g *clickGuard) claim() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.used {
		return ErrAlreadyClicked
	}
	g.used = true
	return nil
}

// Run detects challenges on the page session pageID, waits a bounded time for
// auto-resolve, clicks at most one checkbox (human-paced, trusted input), then
// verifies. Image and audio puzzles return NeedsUser without any interaction.
func Run(ctx context.Context, conn *cdp.Conn, pageID string, opt Options) (Result, error) {
	var res Result
	fr := &frames{conn: conn, page: pageID}
	defer fr.close()
	var guard clickGuard

	// Track child frames before the first scan, so an attach (and its empty URL
	// for script-injected iframes) cannot be missed. It is released once no
	// challenge is found.
	if err := fr.start(ctx); err != nil {
		return res, err
	}
	defer fr.release(ctx)
	ps, err := scanPageQuiet(ctx, conn, pageID, fr, opt)
	if err != nil {
		return res, err
	}
	res.Widgets = ps.Widgets
	if len(ps.Widgets) == 0 {
		fr.release(ctx)
		res.Outcome = Solved
		opt.log(Attempt{Stage: "detect", Outcome: Solved, Detail: "no challenge"})
		return res, nil
	}
	res.Detected = true
	for _, w := range ps.Widgets {
		opt.log(Attempt{Stage: "detect", Kind: w.Kind, Mode: w.Mode, Detail: "host=" + w.Host})
	}
	if ps.hasPuzzle() {
		res.Outcome = NeedsUser
		opt.log(Attempt{Stage: "detect", Mode: ModeUserPuzzle, Outcome: NeedsUser, Detail: "puzzle left to user"})
		return res, nil
	}

	out, ps, err := settle(ctx, conn, pageID, fr, opt.AutoWait, "wait", opt)
	if err != nil || out != StillChallenged {
		res.Outcome = out
		return res, err
	}
	if opt.DisableClick {
		res.Outcome = out
		opt.log(Attempt{Stage: "click", Outcome: StillChallenged, Detail: "checkbox click disabled"})
		return res, nil
	}
	clicked, err := clickOnce(ctx, conn, fr, &guard, ps, opt)
	if err != nil {
		return res, err
	}
	res.Clicked = clicked
	if clicked {
		out, _, err = settle(ctx, conn, pageID, fr, opt.AfterClick, "verify", opt)
		if err != nil {
			return res, err
		}
	}
	res.Outcome = out
	return res, nil
}

// scanPageQuiet scans the page, and while it shows no challenge keeps rescanning
// until opt's confirm window ends. Auto-attach starts before the wait so widget
// frames injected during it are tracked. It returns the first scan that found a
// widget, or the last empty scan when the window expires.
func scanPageQuiet(ctx context.Context, conn *cdp.Conn, page string, fr *frames, opt Options) (pageScan, error) {
	ps, err := scanPage(ctx, conn, page)
	if err != nil {
		return ps, err
	}
	deadline := time.Now().Add(opt.confirmWindow())
	if len(ps.Widgets) > 0 || !time.Now().Before(deadline) {
		return ps, nil
	}
	if err := fr.start(ctx); err != nil {
		return ps, err
	}
	for time.Now().Before(deadline) {
		if err := wait(ctx, noWidgetPoll); err != nil {
			return ps, err
		}
		if ps, err = scanPage(ctx, conn, page); err != nil {
			return ps, err
		}
		if len(ps.Widgets) > 0 {
			return ps, nil
		}
	}
	return ps, nil
}

func (o Options) log(a Attempt) {
	if o.Log != nil {
		o.Log(a)
	}
}

// settle polls until the challenge clears (Solved), a puzzle appears (NeedsUser)
// or d passes (StillChallenged).
func settle(ctx context.Context, conn *cdp.Conn, page string, fr *frames, d time.Duration, stage string, opt Options) (Outcome, pageScan, error) {
	deadline := time.Now().Add(d)
	for {
		ps, err := scanPage(ctx, conn, page)
		if err != nil {
			return "", ps, err
		}
		if stage == "wait" && !hasAutoCheckbox(ps) {
			// A widget may attach early and vanish while its frame navigates, so
			// never treat an empty page as an auto-pass: keep scanning the bound.
			if !time.Now().Before(deadline) {
				opt.log(Attempt{Stage: stage, Outcome: StillChallenged, Detail: "widget never appeared"})
				return StillChallenged, ps, nil
			}
			if err := wait(ctx, noWidgetPoll); err != nil {
				return "", ps, err
			}
			continue
		}
		if ps.hasPuzzle() {
			opt.log(Attempt{Stage: stage, Mode: ModeUserPuzzle, Outcome: NeedsUser, Detail: "puzzle left to user"})
			return NeedsUser, ps, nil
		}
		cleared, err := fr.allChecked(ctx, conn, ps)
		if err != nil {
			return "", ps, err
		}
		if !ps.Interstitial && cleared {
			opt.log(Attempt{Stage: stage, Outcome: Solved})
			return Solved, ps, nil
		}
		if !time.Now().Before(deadline) {
			opt.log(Attempt{Stage: stage, Outcome: StillChallenged, Detail: "bound reached"})
			return StillChallenged, ps, nil
		}
		if err := wait(ctx, 500*time.Millisecond); err != nil {
			return "", ps, err
		}
	}
}

// clickOnce clicks the first unchecked, rendered checkbox of an auto-checkbox widget.
// It reports whether a click was made.
func clickOnce(ctx context.Context, conn *cdp.Conn, fr *frames, guard *clickGuard, ps pageScan, opt Options) (bool, error) {
	for _, w := range ps.Widgets {
		if w.Mode != ModeAutoCheckbox {
			continue
		}
		if err := fr.await(ctx, w.Host, 3*time.Second); err != nil {
			return false, err
		}
		for _, sid := range fr.lookup(w.Host) {
			boxes, err := scanCheckboxes(ctx, conn, sid)
			if err != nil {
				return false, err
			}
			for _, b := range boxes {
				if b.checked || !b.visible {
					continue
				}
				if err := guard.claim(); err != nil {
					return false, err
				}
				p := point{
					X: b.r.X + b.r.W*(0.25+0.5*rand.Float64()),
					Y: b.r.Y + b.r.H*(0.25+0.5*rand.Float64()),
				}
				opt.log(Attempt{Stage: "click", Kind: w.Kind, Mode: w.Mode, Detail: "host=" + w.Host})
				return true, humanClick(ctx, conn, sid, p, opt.Pace)
			}
		}
	}
	return false, nil
}

type point struct{ X, Y float64 }

// humanClick moves the pointer along a short curve, dwells, then presses and
// releases with trusted Input events. pace scales every delay; 0 skips them.
func humanClick(ctx context.Context, conn *cdp.Conn, sid string, to point, pace float64) error {
	from := point{X: math.Max(0, to.X-120-rand.Float64()*80), Y: math.Max(0, to.Y-60-rand.Float64()*60)}
	const steps = 5
	for i := 1; i <= steps; i++ {
		t := float64(i) / steps
		p := point{X: from.X + (to.X-from.X)*t, Y: from.Y + (to.Y-from.Y)*t}
		if i < steps {
			p.X += (rand.Float64() - 0.5) * 3
			p.Y += (rand.Float64() - 0.5) * 3
		}
		if err := mouse(ctx, conn, sid, "mouseMoved", "none", p, 0, 0); err != nil {
			return err
		}
		if err := pause(ctx, pace, 15*time.Millisecond, 35*time.Millisecond); err != nil {
			return err
		}
	}
	if err := pause(ctx, pace, 150*time.Millisecond, 400*time.Millisecond); err != nil {
		return err
	}
	if err := mouse(ctx, conn, sid, "mousePressed", "left", to, 1, 1); err != nil {
		return err
	}
	if err := pause(ctx, pace, 60*time.Millisecond, 140*time.Millisecond); err != nil {
		return err
	}
	return mouse(ctx, conn, sid, "mouseReleased", "left", to, 0, 1)
}

func mouse(ctx context.Context, conn *cdp.Conn, sid, typ, button string, p point, buttons, clickCount int) error {
	return conn.Call(ctx, sid, "Input.dispatchMouseEvent", map[string]any{
		"type": typ, "x": p.X, "y": p.Y, "button": button,
		"buttons": buttons, "clickCount": clickCount, "pointerType": "mouse",
	}, nil)
}

// pause waits a random delay in [lo,hi] scaled by pace. pace 0 returns at once.
func pause(ctx context.Context, pace float64, lo, hi time.Duration) error {
	if pace <= 0 {
		return ctx.Err()
	}
	d := lo + time.Duration(rand.Float64()*float64(hi-lo))
	return wait(ctx, time.Duration(float64(d)*pace))
}

// wait sleeps d or until ctx ends.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type checkbox struct {
	backendID int64
	checked   bool
	visible   bool
	r         rect
}

// scanCheckboxes lists checkbox-like controls in a frame session, pierced and measured.
func scanCheckboxes(ctx context.Context, conn *cdp.Conn, sid string) ([]checkbox, error) {
	var doc struct {
		Root *domNode `json:"root"`
	}
	if err := conn.Call(ctx, sid, "DOM.getDocument", map[string]any{"depth": -1, "pierce": true}, &doc); err != nil {
		return nil, ignoreStale(err)
	}
	var nodes []*domNode
	walkDOM(doc.Root, func(n *domNode) {
		isInput := strings.EqualFold(n.NodeName, "INPUT") && strings.EqualFold(attr(n, "type"), "checkbox")
		switch {
		case isInput, attr(n, "role") == "checkbox", attr(n, "id") == "recaptcha-anchor", attr(n, "id") == "checkbox":
			nodes = append(nodes, n)
		}
	})
	var out []checkbox
	for _, n := range nodes {
		r, visible, err := boxOf(ctx, conn, sid, n.BackendNodeID)
		if err != nil {
			return nil, ignoreStale(err)
		}
		out = append(out, checkbox{
			backendID: n.BackendNodeID,
			checked:   attr(n, "aria-checked") == "true",
			visible:   visible,
			r:         r,
		})
	}
	return out, nil
}

// ignoreStale drops protocol errors from a child session that went away mid-scan.
func ignoreStale(err error) error {
	var perr *cdp.Error
	if errors.As(err, &perr) {
		return nil
	}
	return err
}

// frames tracks OOPIF child sessions attached through Target.setAutoAttach on the page.
// Child iframes are never paused and never re-attached: only the attach event is
// read, so the child keeps running exactly as Chromium started it.
type frames struct {
	conn    *cdp.Conn
	page    string
	started bool              // guarded by mu
	sub     *cdp.Subscription // guarded by mu

	mu   sync.Mutex
	kids map[string][]string // lowercase hostname -> flattened child session ids
	// pending holds child session ids whose attach carried no usable URL
	// (Chromium sends an empty url for iframes page script injects). Their
	// targetInfo is fetched on lookup so they can still be keyed by host.
	pending map[string]string // child session id -> target id
}

// start subscribes to attach events, then enables auto-attach. Subscribe first so no event is missed.
func (f *frames) start(ctx context.Context) error {
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		return nil
	}
	f.started = true
	f.kids = make(map[string][]string)
	f.pending = make(map[string]string)
	sub := f.conn.Subscribe(f.page)
	f.sub = sub
	f.mu.Unlock()
	go f.pump(sub)
	return f.conn.Call(ctx, f.page, "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true,
	}, nil)
}

// pump files each attached child frame by host. A child may attach before it
// commits its navigation, and Chromium sends an empty url for an iframe page
// script injects; such children are kept pending and resolved later.
func (f *frames) pump(sub *cdp.Subscription) {
	for ev := range sub.C {
		if ev.Method != "Target.attachedToTarget" {
			continue
		}
		var p struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfo"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.SessionID == "" || p.TargetInfo.Type != "iframe" {
			continue
		}
		host := hostOf(p.TargetInfo.URL)
		f.mu.Lock()
		if host == "" {
			if p.TargetInfo.TargetID != "" {
				// Children are never paused, so a pending session keeps running;
				// its target info is read again on lookup.
				f.pending[p.SessionID] = p.TargetInfo.TargetID
			}
		} else {
			f.kids[host] = append(f.kids[host], p.SessionID)
		}
		f.mu.Unlock()
	}
}

// hostOf returns the lowercase hostname of a frame URL, or "" when the URL is
// empty or names no host.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// resolve asks the browser for the current target info of children that
// attached without a usable URL, and re-keys those that now name a host.
// Chromium 154 keeps returning an empty url for script-injected iframes, so a
// pending child is also probed over its own session, which knows the frame url
// once it commits. Both probes are DOM-safe: no Runtime, no page script.
func (f *frames) resolve(ctx context.Context) error {
	f.mu.Lock()
	pending := make(map[string]string, len(f.pending))
	for sid, id := range f.pending {
		pending[sid] = id
	}
	f.mu.Unlock()
	for sid, tid := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var info struct {
			TargetInfo struct {
				URL string `json:"url"`
			} `json:"targetInfo"`
		}
		host := ""
		var err error
		if tid != "" {
			err = f.conn.Call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": tid}, &info)
			host = hostOf(info.TargetInfo.URL)
		}
		if err == nil && host == "" {
			// The target info may never name the host; ask the child session
			// itself, which knows its frame url once it commits.
			var ft struct {
				FrameTree struct {
					Frame struct {
						URL string `json:"url"`
					} `json:"frame"`
				} `json:"frameTree"`
			}
			if err = f.conn.Call(ctx, sid, "Page.getFrameTree", nil, &ft); err == nil {
				host = hostOf(ft.FrameTree.Frame.URL)
			}
		}
		if err != nil {
			var perr *cdp.Error
			if errors.As(err, &perr) {
				// The target is gone; drop it.
				f.mu.Lock()
				delete(f.pending, sid)
				f.mu.Unlock()
				continue
			}
			if ctx.Err() != nil {
				return err
			}
			// A transient failure: keep it pending for a later poll.
			continue
		}
		if host == "" {
			continue
		}
		f.mu.Lock()
		if _, ok := f.pending[sid]; ok {
			delete(f.pending, sid)
			f.kids[host] = append(f.kids[host], sid)
		}
		f.mu.Unlock()
	}
	return nil
}

// lookup returns the child sessions of host. Callers must resolve first so
// children that attached without a URL are keyed.
func (f *frames) lookup(host string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kids[host]...)
}

// await polls for a child session of host until one attaches or d passes.
func (f *frames) await(ctx context.Context, host string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if err := f.resolve(ctx); err != nil {
			return err
		}
		if len(f.lookup(host)) > 0 || !time.Now().Before(deadline) {
			return nil
		}
		if err := wait(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

// hasAutoCheckbox reports whether ps shows a widget the user can automatically clear.
func hasAutoCheckbox(ps pageScan) bool {
	for _, w := range ps.Widgets {
		if w.Mode == ModeAutoCheckbox {
			return true
		}
	}
	return false
}

// allChecked reports whether every auto-checkbox widget shows a checked control.
func (f *frames) allChecked(ctx context.Context, conn *cdp.Conn, ps pageScan) (bool, error) {
	if err := f.resolve(ctx); err != nil {
		return false, err
	}
	for _, w := range ps.Widgets {
		if w.Mode != ModeAutoCheckbox {
			continue
		}
		checked := false
		for _, sid := range f.lookup(w.Host) {
			boxes, err := scanCheckboxes(ctx, conn, sid)
			if err != nil {
				return false, err
			}
			for _, b := range boxes {
				checked = checked || b.checked
			}
		}
		if !checked {
			return false, nil
		}
	}
	return true, nil
}

// release turns auto-attach off for the page and detaches the children it
// created. It is idempotent.
func (f *frames) release(ctx context.Context) {
	f.mu.Lock()
	if !f.started {
		f.mu.Unlock()
		return
	}
	f.started = false
	sids := make(map[string]struct{})
	for _, v := range f.kids {
		for _, sid := range v {
			sids[sid] = struct{}{}
		}
	}
	for sid := range f.pending {
		sids[sid] = struct{}{}
	}
	f.kids = nil
	f.pending = nil
	sub := f.sub
	f.sub = nil
	f.mu.Unlock()

	_ = f.conn.Call(ctx, f.page, "Target.setAutoAttach", map[string]any{
		"autoAttach": false, "waitForDebuggerOnStart": false, "flatten": true,
	}, nil)
	if sub != nil {
		sub.Unsubscribe()
	}
	for sid := range sids {
		_ = f.conn.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": sid}, nil)
	}
}

// close releases with its own deadline so cleanup still runs after the
// caller's context ends.
func (f *frames) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.release(ctx)
}
