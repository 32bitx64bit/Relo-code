package page

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/relocode/relo/internal/web/cdp"
)

// navGrace is how long Click waits for a navigation to start after the release.
// It is real time, not scaled by the pacing scale: the navigation's own latency
// does not depend on how fast a human would click.
const navGrace = 1500 * time.Millisecond

// scrollSettle bounds how long Scroll waits for smooth scrolling to stop.
const scrollSettle = 3 * time.Second

// Click moves the pointer along a curved path to a point inside numbered element
// index and clicks it with a real press and release. If the click starts a
// navigation, Click returns after that navigation's load.
func (t *Tab) Click(ctx context.Context, index int) error {
	el, err := t.element(index)
	if err != nil {
		return err
	}
	target, err := t.clickPoint(ctx, el.BackendNodeID)
	if err != nil {
		return err
	}
	sub := t.s.Subscribe()
	defer sub.Unsubscribe()

	if err := t.moveMouse(ctx, target); err != nil {
		return err
	}
	if err := t.pace.pause(ctx, 90*time.Millisecond, 220*time.Millisecond); err != nil {
		return err
	}
	if err := t.mouseButton(ctx, "mousePressed", "left", 1, 1); err != nil {
		return err
	}
	if err := t.pace.pause(ctx, 50*time.Millisecond, 80*time.Millisecond); err != nil {
		return err
	}
	if err := t.mouseButton(ctx, "mouseReleased", "left", 0, 1); err != nil {
		return err
	}
	return t.settle(ctx, sub)
}

// clickPoint scrolls the node into view and returns a random point in the middle of
// its box, not its exact centre, as a person would aim.
func (t *Tab) clickPoint(ctx context.Context, backendNodeID int64) (Point, error) {
	if err := t.s.Call(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": backendNodeID}, nil); err != nil {
		return Point{}, fmt.Errorf("page: scroll into view: %w", err)
	}
	var box struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := t.s.Call(ctx, "DOM.getBoxModel", map[string]any{"backendNodeId": backendNodeID}, &box); err != nil {
		return Point{}, fmt.Errorf("page: element box: %w", err)
	}
	q := box.Model.Content
	if len(q) != 8 {
		return Point{}, errors.New("page: element has no box")
	}
	minX, maxX := min(q[0], q[2], q[4], q[6]), max(q[0], q[2], q[4], q[6])
	minY, maxY := min(q[1], q[3], q[5], q[7]), max(q[1], q[3], q[5], q[7])
	w, h := maxX-minX, maxY-minY
	if w <= 0 || h <= 0 {
		return Point{}, errors.New("page: element is empty")
	}
	// Aim within the middle 60% of the box.
	return Point{
		X: minX + w*(0.2+0.6*t.pace.rnd.Float64()),
		Y: minY + h*(0.2+0.6*t.pace.rnd.Float64()),
	}, nil
}

// moveMouse sends mouseMoved events along a curved path from the last pointer position.
func (t *Tab) moveMouse(ctx context.Context, to Point) error {
	pts := t.pace.mousePath(t.mouse, to, stepCount(t.mouse, to))
	for _, p := range pts[1:] {
		if err := t.dispatchMouse(ctx, "mouseMoved", p, "none", 0, 0); err != nil {
			return err
		}
		if err := t.pace.pause(ctx, 6*time.Millisecond, 10*time.Millisecond); err != nil {
			return err
		}
	}
	t.mouse = to
	return nil
}

func (t *Tab) mouseButton(ctx context.Context, typ, button string, buttons, clickCount int) error {
	return t.dispatchMouse(ctx, typ, t.mouse, button, buttons, clickCount)
}

func (t *Tab) dispatchMouse(ctx context.Context, typ string, p Point, button string, buttons, clickCount int) error {
	params := map[string]any{
		"type":       typ,
		"x":          p.X,
		"y":          p.Y,
		"button":     button,
		"buttons":    buttons,
		"clickCount": clickCount,
	}
	if err := t.s.Call(ctx, "Input.dispatchMouseEvent", params, nil); err != nil {
		return fmt.Errorf("page: %s: %w", typ, err)
	}
	return nil
}

// settle returns after a navigation that the input started has loaded, or after a
// same-document navigation has changed the URL. If no navigation starts within
// navGrace, it returns with nil.
func (t *Tab) settle(ctx context.Context, sub *cdp.Subscription) error {
	grace := time.NewTimer(navGrace)
	defer grace.Stop()
	var graceC <-chan time.Time = grace.C
	started := false
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return cdp.ErrClosed
			}
			switch ev.Method {
			case "Page.frameStartedLoading":
				started = true
				graceC = nil
			case "Page.frameStoppedLoading":
				if started {
					t.resetWorld()
					return nil
				}
			case "Page.navigatedWithinDocument":
				// A same-document navigation (a form GET that only changes the
				// query, or a hash change) fires no loader signal of its own; the
				// URL is already the new one, so only the snapshot is stale.
				t.resetWorld()
				return nil
			}
		case <-graceC:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Type focuses numbered element index with a click, then types text one key at a
// time with variable cadence. A newline is sent as Enter. If typing starts a
// navigation (a form submit on Enter, for example), Type returns after that
// navigation's load, as Click does.
func (t *Tab) Type(ctx context.Context, index int, text string) error {
	if err := t.Click(ctx, index); err != nil {
		return err
	}
	sub := t.s.Subscribe()
	defer sub.Unsubscribe()
	for _, r := range text {
		if r == '\n' {
			if err := t.pressKey(ctx, "Enter"); err != nil {
				return err
			}
			continue
		}
		if err := t.typeRune(ctx, r); err != nil {
			return err
		}
		if err := t.pace.pause(ctx, t.pace.keyDelay(r), 0); err != nil {
			return err
		}
	}
	return t.settle(ctx, sub)
}

func (t *Tab) typeRune(ctx context.Context, r rune) error {
	s := string(r)
	down := map[string]any{"type": "keyDown", "key": s, "text": s, "unmodifiedText": s}
	if err := t.s.Call(ctx, "Input.dispatchKeyEvent", down, nil); err != nil {
		return fmt.Errorf("page: type: %w", err)
	}
	up := map[string]any{"type": "keyUp", "key": s}
	if err := t.s.Call(ctx, "Input.dispatchKeyEvent", up, nil); err != nil {
		return fmt.Errorf("page: type: %w", err)
	}
	return nil
}

// keyDef describes a key for Input.dispatchKeyEvent.
type keyDef struct {
	key, code string
	vk        int
	text      string // set for keys that produce a character event
}

// keyDefs holds the named keys Press accepts. Single characters are accepted too
// (see pressDef), so "a", "A" and "5" work without being listed here.
var keyDefs = map[string]keyDef{
	"Enter":      {"Enter", "Enter", 13, "\r"},
	"Tab":        {"Tab", "Tab", 9, ""},
	"Escape":     {"Escape", "Escape", 27, ""},
	"Backspace":  {"Backspace", "Backspace", 8, ""},
	"Delete":     {"Delete", "Delete", 46, ""},
	"Space":      {" ", "Space", 32, " "},
	"Home":       {"Home", "Home", 36, ""},
	"End":        {"End", "End", 35, ""},
	"PageUp":     {"PageUp", "PageUp", 33, ""},
	"PageDown":   {"PageDown", "PageDown", 34, ""},
	"ArrowLeft":  {"ArrowLeft", "ArrowLeft", 37, ""},
	"ArrowUp":    {"ArrowUp", "ArrowUp", 38, ""},
	"ArrowRight": {"ArrowRight", "ArrowRight", 39, ""},
	"ArrowDown":  {"ArrowDown", "ArrowDown", 40, ""},
}

// modifierKeys maps chord modifier names to their modifier mask bit.
var modifierKeys = map[string]int{
	"Alt": 1, "Control": 2, "Meta": 4, "Shift": 8,
}

// modifierVK is the virtual key code of each modifier key itself.
var modifierVK = map[string]int{"Alt": 18, "Control": 17, "Meta": 91, "Shift": 16}

// pressDef resolves the key part of a Press argument.
func pressDef(name string) (keyDef, bool) {
	if def, ok := keyDefs[name]; ok {
		return def, true
	}
	r := []rune(name)
	if len(r) != 1 {
		return keyDef{}, false
	}
	c := r[0]
	if c >= 'a' && c <= 'z' {
		s := string(c)
		return keyDef{key: s, code: "Key" + strings.ToUpper(s), vk: int(c), text: s}, true
	}
	if c >= '0' && c <= '9' {
		s := string(c)
		return keyDef{key: s, code: "Digit" + s, vk: int(c), text: s}, true
	}
	return keyDef{}, false
}

// pressKey sends one named key. It is Press without a trailing navigation settle,
// for use inside Type where several keys may be sent before one navigation.
func (t *Tab) pressKey(ctx context.Context, name string) error {
	keys, mods, err := parseChord(name)
	if err != nil {
		return err
	}
	mask := 0
	pressed := make([]string, 0, len(mods))
	defer func() {
		for i := len(pressed) - 1; i >= 0; i-- {
			m := pressed[i]
			_ = t.dispatchKey(ctx, "keyUp", m, keyDef{key: m, code: m, vk: modifierVK[m]}, 0)
		}
	}()
	for _, m := range mods {
		mask |= modifierKeys[m]
		pressed = append(pressed, m)
		if err := t.dispatchKey(ctx, "rawKeyDown", m, keyDef{key: m, code: m, vk: modifierVK[m]}, mask); err != nil {
			return err
		}
	}
	def, ok := pressDef(keys)
	if !ok {
		return fmt.Errorf("page: unknown key %q", name)
	}
	down := def.key
	if mask&8 != 0 && len(def.key) == 1 && def.key >= "a" && def.key <= "z" {
		down = strings.ToUpper(def.key)
	}
	if err := t.dispatchKey(ctx, "keyDown", down, def, mask); err != nil {
		return err
	}
	if err := t.pace.pause(ctx, 40*time.Millisecond, 50*time.Millisecond); err != nil {
		return err
	}
	if err := t.dispatchKey(ctx, "keyUp", down, def, mask); err != nil {
		return err
	}
	return nil
}

// parseChord splits a Press argument such as "Control+Shift+a" into its key and
// its modifier names, which are returned in the order they were written.
func parseChord(name string) (string, []string, error) {
	parts := strings.Split(name, "+")
	if len(parts) > 5 {
		return "", nil, fmt.Errorf("page: unknown key %q", name)
	}
	if strings.Contains(name, "++") || strings.HasSuffix(name, "+") {
		return "", nil, fmt.Errorf("page: unknown key %q", name)
	}
	var mods []string
	for _, p := range parts[:len(parts)-1] {
		if _, ok := modifierKeys[p]; !ok {
			return "", nil, fmt.Errorf("page: unknown modifier %q in key %q", p, name)
		}
		mods = append(mods, p)
	}
	last := parts[len(parts)-1]
	if last == "" {
		return "", nil, fmt.Errorf("page: unknown key %q", name)
	}
	return last, mods, nil
}

// dispatchKey sends one Input.dispatchKeyEvent. mods is the modifier mask
// (1 Alt, 2 Control, 4 Meta, 8 Shift).
func (t *Tab) dispatchKey(ctx context.Context, typ, key string, def keyDef, mods int) error {
	params := map[string]any{
		"type": typ, "key": key, "code": def.code,
		"windowsVirtualKeyCode": def.vk, "nativeVirtualKeyCode": def.vk,
	}
	if mods != 0 {
		params["modifiers"] = mods
	}
	if def.text != "" {
		params["text"] = def.text
		params["unmodifiedText"] = def.text
	}
	if err := t.s.Call(ctx, "Input.dispatchKeyEvent", params, nil); err != nil {
		return fmt.Errorf("page: key %s: %w", def.code, err)
	}
	return nil
}

// Press sends one key and, if it starts a navigation, returns after that
// navigation's load. name is a named key ("Enter", "Tab", "ArrowDown"), a single
// character ("a"), or a chord of Control/Shift/Alt/Meta and a key ("Control+a").
func (t *Tab) Press(ctx context.Context, name string) error {
	sub := t.s.Subscribe()
	defer sub.Unsubscribe()
	if err := t.pressKey(ctx, name); err != nil {
		return err
	}
	return t.settle(ctx, sub)
}

// Scroll turns the mouse wheel by dy CSS pixels (positive is down) at the last pointer
// position, in uneven notches with pauses between them.
func (t *Tab) Scroll(ctx context.Context, dy int) error {
	for _, step := range t.pace.wheelSteps(dy) {
		params := map[string]any{
			"type": "mouseWheel", "x": t.mouse.X, "y": t.mouse.Y,
			"deltaX": 0, "deltaY": step,
		}
		if err := t.s.Call(ctx, "Input.dispatchMouseEvent", params, nil); err != nil {
			return fmt.Errorf("page: scroll: %w", err)
		}
		if err := t.pace.pause(ctx, 40*time.Millisecond, 90*time.Millisecond); err != nil {
			return err
		}
	}
	if err := t.waitScrollStill(ctx); err != nil {
		return err
	}
	// A short dwell after the page stops moving.
	return t.pace.pause(ctx, 250*time.Millisecond, 250*time.Millisecond)
}

// ScrollY returns window.scrollY of the main frame, read from the isolated
// world. A page can scroll itself, so the value is a reading, not a fact.
func (t *Tab) ScrollY(ctx context.Context) (int, error) {
	return t.isolatedInt(ctx, `window.scrollY`)
}

// ViewportHeight returns window.innerHeight in CSS pixels, read from the
// isolated world.
func (t *Tab) ViewportHeight(ctx context.Context) (int, error) {
	return t.isolatedInt(ctx, `window.innerHeight`)
}

// isolatedInt runs expr in the isolated world and parses its rounded integer
// value.
func (t *Tab) isolatedInt(ctx context.Context, expr string) (int, error) {
	raw, err := t.evalIsolated(ctx, `String(Math.round(`+expr+`))`)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("page: read %s: %q is not a number", expr, raw)
	}
	return value, nil
}

// waitScrollStill polls window.scrollY in the isolated world until two readings in a
// row match, so smooth scrolling has finished before the next click is aimed. It is
// bounded by scrollSettle in real time.
func (t *Tab) waitScrollStill(ctx context.Context) error {
	deadline := time.Now().Add(scrollSettle)
	prev := ""
	for time.Now().Before(deadline) {
		cur, err := t.evalIsolated(ctx, `String(Math.round(window.scrollY))`)
		if err != nil {
			return err
		}
		if cur == prev {
			return nil
		}
		prev = cur
		select {
		case <-time.After(60 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Back returns to the previous history entry. It uses Page.navigateToHistoryEntry
// rather than an Alt+Left key event, because browser accelerators are not reached
// by key events sent to the page. It returns ErrNoHistory when there is none.
func (t *Tab) Back(ctx context.Context) error {
	h, err := t.history(ctx)
	if err != nil {
		return fmt.Errorf("page: history: %w", err)
	}
	if h.CurrentIndex <= 0 {
		return ErrNoHistory
	}
	sub := t.s.Subscribe()
	defer sub.Unsubscribe()

	if err := t.pace.pause(ctx, 300*time.Millisecond, 400*time.Millisecond); err != nil {
		return err
	}
	entry := h.Entries[h.CurrentIndex-1].ID
	if err := t.s.Call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": entry}, nil); err != nil {
		return fmt.Errorf("page: back: %w", err)
	}
	t.elements = nil
	t.resetWorld()
	// A cross-document entry fires load; a same-document one fires navigatedWithinDocument.
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return cdp.ErrClosed
			}
			switch ev.Method {
			case "Page.loadEventFired", "Page.navigatedWithinDocument", "Page.frameStoppedLoading":
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
