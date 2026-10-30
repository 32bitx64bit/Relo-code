// Package challenge detects bot challenges on a page and clicks at most one
// checkbox. It reads the DOM over CDP (DOM.getDocument with pierce) and never
// runs script in the page. Image and audio puzzles are reported, never touched.
package challenge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/relocode/relo/internal/web/cdp"
)

// Kind names a challenge family.
type Kind string

const (
	KindInterstitial Kind = "cloudflare_interstitial"
	KindTurnstile    Kind = "turnstile"
	KindRecaptchaV2  Kind = "recaptcha_v2"
	KindHCaptcha     Kind = "hcaptcha"
)

// Mode says how the agent may act on a widget.
type Mode string

const (
	ModeAutoWait     Mode = "auto_wait"     // wait for the page to resolve it
	ModeAutoCheckbox Mode = "auto_checkbox" // wait, then at most one checkbox click
	ModeUserPuzzle   Mode = "user_puzzle"   // image or audio puzzle: hand to the user
)

// Widget is one challenge found on the page.
type Widget struct {
	Kind    Kind
	Mode    Mode
	Host    string // hostname of the widget frame; empty for the interstitial
	Visible bool   // has a rendered box in the top frame

	iframeBackendID int64
}

// rect is a box in CSS pixels of the frame it was measured in.
type rect struct{ X, Y, W, H float64 }

// domNode is the subset of DOM.Node this package reads.
type domNode struct {
	BackendNodeID   int64      `json:"backendNodeId"`
	NodeName        string     `json:"nodeName"`
	NodeType        int        `json:"nodeType"`
	NodeValue       string     `json:"nodeValue"`
	Attributes      []string   `json:"attributes"`
	Children        []*domNode `json:"children"`
	ShadowRoots     []*domNode `json:"shadowRoots"`
	ContentDocument *domNode   `json:"contentDocument"`
}

// classify maps an iframe src to a widget. ok is false for frames that are not
// challenge widgets. Unknown hCaptcha frames are treated as puzzles (conservative).
func classify(src string) (Kind, Mode, bool) {
	u, err := url.Parse(src)
	if err != nil || u.Hostname() == "" {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "challenges.cloudflare.com":
		return KindTurnstile, ModeAutoCheckbox, true
	case host == "google.com" || strings.HasSuffix(host, ".google.com") ||
		host == "recaptcha.net" || strings.HasSuffix(host, ".recaptcha.net"):
		if !strings.HasPrefix(u.Path, "/recaptcha/") {
			return "", "", false
		}
		switch {
		case strings.Contains(u.Path, "/bframe"):
			return KindRecaptchaV2, ModeUserPuzzle, true
		case strings.Contains(u.Path, "/anchor"):
			return KindRecaptchaV2, ModeAutoCheckbox, true
		}
		return "", "", false
	case host == "hcaptcha.com" || strings.HasSuffix(host, ".hcaptcha.com"):
		// hCaptcha selects its frame by a param in the query or the fragment.
		params := u.RawQuery + "&" + u.Fragment
		switch {
		case strings.Contains(params, "frame=checkbox"):
			return KindHCaptcha, ModeAutoCheckbox, true
		default: // frame=challenge, or unknown: never click
			return KindHCaptcha, ModeUserPuzzle, true
		}
	}
	return "", "", false
}

// attr returns the value of an attribute, or "" when absent.
func attr(n *domNode, name string) string {
	for i := 0; i+1 < len(n.Attributes); i += 2 {
		if n.Attributes[i] == name {
			return n.Attributes[i+1]
		}
	}
	return ""
}

// walkDOM visits n, its shadow roots and its content document, depth first.
func walkDOM(n *domNode, fn func(*domNode)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		walkDOM(c, fn)
	}
	for _, c := range n.ShadowRoots {
		walkDOM(c, fn)
	}
	walkDOM(n.ContentDocument, fn)
}

// textOf concatenates the text children of n.
func textOf(n *domNode) string {
	var b strings.Builder
	for _, c := range n.Children {
		if c.NodeType == 3 {
			b.WriteString(c.NodeValue)
		}
	}
	return b.String()
}

// boxOf returns the content box of a rendered node. ok is false when the
// node has no box (display none, detached, or not rendered).
func boxOf(ctx context.Context, conn *cdp.Conn, sid string, backendID int64) (rect, bool, error) {
	var out struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	err := conn.Call(ctx, sid, "DOM.getBoxModel", map[string]any{"backendNodeId": backendID}, &out)
	var perr *cdp.Error
	if errors.As(err, &perr) {
		return rect{}, false, nil // "Could not compute box model": not rendered
	}
	if err != nil {
		return rect{}, false, err
	}
	q := out.Model.Content
	if len(q) < 8 {
		return rect{}, false, nil
	}
	minX, maxX := min(min(q[0], q[2]), min(q[4], q[6])), max(max(q[0], q[2]), max(q[4], q[6]))
	minY, maxY := min(min(q[1], q[3]), min(q[5], q[7])), max(max(q[1], q[3]), max(q[5], q[7]))
	r := rect{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}
	return r, r.W > 0 && r.H > 0, nil
}

// pageScan is one DOM read of the top frame.
type pageScan struct {
	Widgets      []Widget
	Interstitial bool
}

func (p pageScan) hasPuzzle() bool {
	for _, w := range p.Widgets {
		if w.Mode == ModeUserPuzzle && w.Visible {
			return true
		}
	}
	return false
}

func (p pageScan) hasCheckbox() bool {
	for _, w := range p.Widgets {
		if w.Mode == ModeAutoCheckbox {
			return true
		}
	}
	return false
}

// scanPage reads the pierced DOM of session sid and lists the challenges in it.
func scanPage(ctx context.Context, conn *cdp.Conn, sid string) (pageScan, error) {
	var doc struct {
		Root *domNode `json:"root"`
	}
	if err := conn.Call(ctx, sid, "DOM.getDocument", map[string]any{"depth": -1, "pierce": true}, &doc); err != nil {
		return pageScan{}, fmt.Errorf("challenge: DOM.getDocument: %w", err)
	}
	interstitial, frames := findMarkers(doc.Root)
	var ps pageScan
	ps.Interstitial = interstitial
	if ps.Interstitial {
		ps.Widgets = append(ps.Widgets, Widget{Kind: KindInterstitial, Mode: ModeAutoWait, Visible: true})
	}
	for _, f := range frames {
		kind, mode, ok := classify(attr(f, "src"))
		if !ok {
			continue
		}
		u, _ := url.Parse(attr(f, "src"))
		_, visible, err := boxOf(ctx, conn, sid, f.BackendNodeID)
		if err != nil {
			return pageScan{}, err
		}
		ps.Widgets = append(ps.Widgets, Widget{
			Kind:            kind,
			Mode:            mode,
			Host:            strings.ToLower(u.Hostname()),
			Visible:         visible,
			iframeBackendID: f.BackendNodeID,
		})
	}
	return ps, nil
}

// findMarkers walks the pierced tree once. It reports whether the page is a
// Cloudflare interstitial and returns every iframe in document order.
func findMarkers(root *domNode) (interstitial bool, frames []*domNode) {
	var title string
	var idMarker, scriptMarker bool
	walkDOM(root, func(n *domNode) {
		switch strings.ToUpper(n.NodeName) {
		case "TITLE":
			title = textOf(n)
		case "IFRAME":
			frames = append(frames, n)
		case "SCRIPT":
			if strings.Contains(attr(n, "src"), "/cdn-cgi/challenge-platform/") {
				scriptMarker = true
			}
		}
		switch attr(n, "id") {
		case "challenge-form", "challenge-running", "cf-challenge-running":
			idMarker = true
		}
	})
	titleMarker := strings.HasPrefix(strings.TrimSpace(title), "Just a moment")
	return idMarker || (titleMarker && scriptMarker), frames
}
