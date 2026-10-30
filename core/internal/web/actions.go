package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relocode/relo/internal/web/page"
)

func validateRequest(req Request) error {
	switch req.Action {
	case ActionStatus, ActionSnapshot, ActionBack, ActionWait, ActionScreenshot, ActionClose:
	case ActionSearch:
		if strings.TrimSpace(req.Query) == "" {
			return errors.New("web: search needs a query")
		}
	case ActionFetch, ActionOpen:
		if strings.TrimSpace(req.URL) == "" {
			return fmt.Errorf("web: %s needs a URL", req.Action)
		}
	case ActionClick, ActionType:
		if req.Ref < 1 {
			return fmt.Errorf("web: %s needs a snapshot ref", req.Action)
		}
	case ActionPress:
		if strings.TrimSpace(req.Key) == "" {
			return errors.New("web: press needs a key")
		}
	case ActionScroll:
		switch req.Direction {
		case "", "up", "down": // empty is the documented default: down
		default:
			return fmt.Errorf("web: scroll direction must be up or down, not %q", req.Direction)
		}
		if req.Amount < 0 {
			return errors.New("web: scroll amount cannot be negative")
		}
	default:
		return fmt.Errorf("web: unsupported action %q", req.Action)
	}
	if req.Action == ActionWait && req.Amount < 0 {
		return errors.New("web: wait amount cannot be negative")
	}
	return nil
}

func (e *Engine) runAction(ctx context.Context, env *actionEnv, req Request) (Result, error) {
	switch req.Action {
	case ActionSearch:
		return e.searchFlow(ctx, env, req.Query)
	case ActionFetch:
		return e.openFlow(ctx, env, req.URL, false, req.MaxChars)
	case ActionOpen:
		return e.openFlow(ctx, env, req.URL, true, req.MaxChars)
	case ActionClose:
		tab, _ := e.currentTab(env.agent)
		if tab != nil {
			env.tier = tab.tier
		}
		return Result{Tier: env.tier}, e.closeAgentTab(env.agent)
	case ActionWait:
		d := time.Duration(req.Amount) * time.Millisecond
		if req.Amount == 0 {
			d = time.Second
		}
		if d > MaxWait {
			d = MaxWait
		}
		if err := sleepContext(ctx, d); err != nil {
			return Result{}, err
		}
		return e.snapshotResult(ctx, env)
	case ActionSnapshot:
		return e.snapshotResult(ctx, env)
	case ActionScreenshot:
		tab, err := e.currentTab(env.agent)
		if err != nil {
			return Result{}, err
		}
		env.tier = tab.tier
		actionCtx, cancel := context.WithTimeout(ctx, navigationTimeout)
		defer cancel()
		rawURL, refused := e.guardCurrent(ctx, env, tab, actionCtx)
		if refused != nil {
			return refused.render(tab.tier), nil
		}
		png, err := tab.tab.Screenshot(actionCtx)
		if err != nil {
			return Result{}, err
		}
		return Result{Tier: tab.tier, URL: rawURL, Screenshot: png,
			Notes: []string{"screenshot contains untrusted web content"}}, nil
	case ActionClick, ActionType, ActionPress, ActionScroll, ActionBack:
		return e.inputAction(ctx, env, req)
	default:
		return Result{}, fmt.Errorf("web: unsupported action %q", req.Action)
	}
}

func (e *Engine) inputAction(ctx context.Context, env *actionEnv, req Request) (Result, error) {
	tab, err := e.currentTab(env.agent)
	if err != nil {
		return Result{}, err
	}
	env.tier = tab.tier
	actionCtx, cancel := context.WithTimeout(ctx, navigationTimeout)
	defer cancel()
	rawURL, refused := e.guardCurrent(ctx, env, tab, actionCtx)
	if refused != nil {
		return refused.render(tab.tier), nil
	}
	var notes []string
	switch req.Action {
	case ActionClick:
		err = tab.tab.Click(actionCtx, req.Ref)
	case ActionType:
		err = tab.tab.Type(actionCtx, req.Ref, req.Text)
	case ActionPress:
		err = tab.tab.Press(actionCtx, req.Key)
	case ActionScroll:
		notes, err = scrollTab(actionCtx, tab.tab, req)
	case ActionBack:
		err = tab.tab.Back(actionCtx)
	}
	if err != nil {
		return Result{}, err
	}
	rawURL, err = tab.tab.URL(actionCtx)
	if err != nil {
		return Result{}, err
	}
	if err := e.guardURL(env.settings, rawURL); err != nil {
		r := &guardRefusal{url: rawURL, err: err}
		e.resetBlockedTab(tab, r)
		env.agent.lastSearch = nil
		return r.render(tab.tier), nil
	}
	if err := e.handleChallenge(ctx, env, tab, rawURL); err != nil {
		return Result{}, err
	}
	result, err := e.snapshotResult(ctx, env)
	if err != nil {
		return Result{}, err
	}
	result.Notes = append(result.Notes, notes...)
	return result, nil
}

// maxScrollCSSPixels bounds how far one scroll moves the page, so a single
// action cannot fling a long page past its content. The relo_web schema caps
// amount at the same number.
const maxScrollCSSPixels = 5000

// scrollDelta turns a scroll Request into the wheel distance in CSS pixels:
// positive is down, negative is up. Amount 0 is one viewport height, read from
// the page in the isolated world; a page that reports no height at all falls
// back to the default so the wheel still moves. The distance is clamped to
// maxScrollCSSPixels.
func scrollDelta(ctx context.Context, tab browserTab, req Request) (int, error) {
	amount := req.Amount
	if amount == 0 {
		height, err := tab.ViewportHeight(ctx)
		if err != nil {
			return 0, err
		}
		amount = height
		if amount <= 0 {
			amount = defaultScrollCSSPixels
		}
	}
	if amount > maxScrollCSSPixels {
		amount = maxScrollCSSPixels
	}
	if req.Direction == "up" {
		return -amount, nil
	}
	return amount, nil
}

// scrollTab turns the wheel by the request's distance and reports whether the
// page moved. An unchanged window.scrollY means the page itself did not scroll:
// an inner scroll container may have taken the wheel instead, which no snapshot
// of the page can show.
func scrollTab(ctx context.Context, tab browserTab, req Request) ([]string, error) {
	before, err := tab.ScrollY(ctx)
	if err != nil {
		return nil, err
	}
	dy, err := scrollDelta(ctx, tab, req)
	if err != nil {
		return nil, err
	}
	if err := tab.Scroll(ctx, dy); err != nil {
		return nil, err
	}
	after, err := tab.ScrollY(ctx)
	if err != nil {
		return nil, err
	}
	if after != before {
		return nil, nil
	}
	return []string{fmt.Sprintf("scroll: the page did not move; window.scrollY stayed at %d, so an inner scroll container may have scrolled instead", before)}, nil
}

func (e *Engine) snapshotResult(ctx context.Context, env *actionEnv) (Result, error) {
	tab, err := e.currentTab(env.agent)
	if err != nil {
		return Result{}, err
	}
	env.tier = tab.tier
	actionCtx, cancel := context.WithTimeout(ctx, navigationTimeout)
	defer cancel()
	rawURL, refused := e.guardCurrent(ctx, env, tab, actionCtx)
	if refused != nil {
		return refused.render(tab.tier), nil
	}
	elements, err := tab.tab.Snapshot(actionCtx)
	if err != nil {
		return Result{}, err
	}
	result := Result{Tier: tab.tier, URL: rawURL, Elements: convertElements(elements),
		Content: untrustedEnvelope(page.RenderSnapshot(elements))}
	return result, nil
}

// guardRefusal records a tab whose current page the guard refused. The action
// that found it returns no page content, only the note below.
type guardRefusal struct {
	url string
	err error
}

func (r *guardRefusal) render(tier Tier) Result {
	return Result{Tier: tier, State: StateDone, Outcome: OutcomeDone,
		Notes: []string{refusalNote(r)}}
}

func refusalNote(r *guardRefusal) string {
	return fmt.Sprintf("blocked: page at %s was refused by the URL guard (%v); the tab was reset to about:blank and no page content is returned", r.url, r.err)
}

// guardCurrent reads the tab's current main-frame URL and applies the URL guard
// before any page content leaves the engine. A refused tab is navigated to
// about:blank so the blocked page can neither be read nor acted on afterwards.
// A browser that refuses the reset navigation reports the refused URL with no
// error, so the refusal is kept for that URL: the reset landed on about:blank at
// most, and the block still holds, so that URL is reported once more and the
// refusal is then spent. about:blank itself is the reset target: it is never
// re-guarded, on either the first or a later read, as no page content exists
// there. A refusal that ends on some other page the guard accepts is cleared,
// so a second read reports the reset page's own state.
func (e *Engine) guardCurrent(ctx context.Context, env *actionEnv, tab *agentTab, actionCtx context.Context) (string, *guardRefusal) {
	rawURL, err := tab.tab.URL(actionCtx)
	if err != nil {
		return "", nil // the next page call reports the same fault
	}
	if rawURL == aboutBlank {
		return rawURL, nil // the reset target; no page content exists there
	}
	e.mu.Lock()
	refusal := tab.refusal
	replay := refusal != nil && refusal.url == rawURL
	tab.refusal = nil
	e.mu.Unlock()
	if replay {
		return rawURL, refusal
	}
	if err := e.guardURL(env.settings, rawURL); err != nil {
		r := &guardRefusal{url: rawURL, err: err}
		e.resetBlockedTab(tab, r)
		e.mu.Lock()
		env.agent.lastSearch = nil
		e.mu.Unlock()
		return rawURL, r
	}
	return rawURL, nil
}

// resetBlockedTab navigates tab away from a refused page and remembers the
// refusal. A failed reset is ignored: that URL was blocked, so whatever the tab
// shows now was not read, and the refusal must not be lost to a navigation error.
// Only a reset that lands on a different refused page carries a newer refusal.
func (e *Engine) resetBlockedTab(tab *agentTab, r *guardRefusal) {
	blankCtx, cancel := context.WithTimeout(context.Background(), navigationTimeout)
	defer cancel()
	returned := tab.tab.Navigate(blankCtx, aboutBlank, "")
	landed, err := tab.tab.URL(blankCtx)
	e.mu.Lock()
	settings := e.settings
	e.mu.Unlock()
	keep := r
	if returned == nil && err == nil && landed != r.url && landed != aboutBlank {
		if check := e.guardURL(settings, landed); check != nil {
			keep = &guardRefusal{url: landed, err: check}
		}
	}
	e.mu.Lock()
	tab.refusal = keep
	e.mu.Unlock()
}

// aboutBlank is the reset target for a tab that reached a refused page.
const aboutBlank = "about:blank"

func convertElements(elements []page.Element) []Element {
	out := make([]Element, 0, len(elements))
	for _, element := range elements {
		out = append(out, Element{Ref: element.Index, Role: element.Role, Name: element.Name})
	}
	return out
}
