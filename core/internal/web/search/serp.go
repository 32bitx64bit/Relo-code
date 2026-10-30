package search

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// serpSelectors are one provider's SERP selectors. Each built-in provider has its own; the generic
// heuristic has none.
type serpSelectors struct {
	// result matches a result container in document order.
	result func(*html.Node) bool
	// title picks the result's title link inside its container.
	title func(*html.Node) *html.Node
	// desc picks the result's description inside its container.
	desc func(*html.Node) *html.Node
	// skip marks nodes that can never be results, such as the site's own navigation.
	skip func(*html.Node) bool
}

// duckDuckGoSelectors read html.duckduckgo.com/html/. Each result is a div holding a .result__a title
// link and a .result__snippet description.
var duckDuckGoSelectors = serpSelectors{
	result: func(n *html.Node) bool {
		return n.DataAtom == atom.Div && classWord(n, "result") && classWord(n, "results_links")
	},
	title: func(n *html.Node) *html.Node { return firstAnchorWithClass(n, "result__a") },
	desc:  func(n *html.Node) *html.Node { return firstAnchorWithClass(n, "result__snippet") },
	skip:  isSERPChrome,
}

// braveSelectors read search.brave.com. Each result is a .snippet block holding a .snippet-title link
// and a .snippet-description.
var braveSelectors = serpSelectors{
	result: func(n *html.Node) bool { return n.DataAtom == atom.Div && classWord(n, "snippet") },
	title: func(n *html.Node) *html.Node {
		if c := firstByClass(n, "snippet-title", func(c *html.Node) bool { return c.DataAtom == atom.A }); c != nil {
			return c
		}
		return firstAnchorWithClass(n, "heading-serpresult")
	},
	desc: func(n *html.Node) *html.Node {
		for _, class := range []string{"snippet-description", "snippet-content"} {
			if c := firstByClass(n, class); c != nil {
				return c
			}
		}
		return nil
	},
	skip: isSERPChrome,
}

// bingSelectors read www.bing.com/search. Each result is an li.b_algo holding an h2 anchor and a
// .b_caption description.
var bingSelectors = serpSelectors{
	result: func(n *html.Node) bool { return n.DataAtom == atom.Li && classWord(n, "b_algo") },
	title: func(n *html.Node) *html.Node {
		return firstByTag(n, atom.H2, func(c *html.Node) bool { return c.DataAtom == atom.A })
	},
	desc: func(n *html.Node) *html.Node {
		for _, class := range []string{"b_caption", "b_lineclamp2", "b_algoSlug"} {
			if c := firstByClass(n, class); c != nil {
				return c
			}
		}
		return nil
	},
	skip: isSERPChrome,
}

// genericSelectors scrape a page this package does not know by heuristic; they name no provider classes.
var genericSelectors = serpSelectors{}

// isSERPChrome refuses the navigation and forms every SERP shares, and nothing else: provider result
// blocks often carry words like "header" or "related" in their class names.
func isSERPChrome(n *html.Node) bool {
	return n.DataAtom == atom.Nav || n.DataAtom == atom.Form
}

// genericSkip refuses parts of a page that never hold results.
func genericSkip(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Nav, atom.Header, atom.Footer, atom.Aside, atom.Form:
		return true
	}
	if hasAttr(n, "hidden") || attrContains(n, "aria-hidden", "true") {
		return true
	}
	for _, word := range []string{"ad", "ads", "sponsored", "promo", "related", "sidebar", "navbar", "menu", "footer", "header", "search-form", "pagination", "pager"} {
		if classWord(n, word) || attrContains(n, "id", word) {
			return true
		}
	}
	return false
}

// minGenericText is the least title length a generic result link needs; shorter links are menu items.
const minGenericText = 3

// parseSERP reads hits from an HTML SERP. pageURL resolves relative links. Provider selectors are
// tried first; when they find nothing the generic heuristic runs, so a redesigned page still yields
// results.
func parseSERP(body []byte, pageURL *url.URL, sel serpSelectors) ([]Hit, error) {
	doc, err := parseHTML(body)
	if err != nil {
		return nil, err
	}
	if sel.result != nil {
		if hits := collectSERP(doc, pageURL, sel); len(hits) > 0 {
			return hits, nil
		}
	}
	hits := genericSERP(doc, pageURL)
	if len(hits) == 0 {
		return nil, ErrNoResults
	}
	return hits, nil
}

// collectSERP walks the document in order and reads each result container the selector matches.
func collectSERP(doc *html.Node, pageURL *url.URL, sel serpSelectors) []Hit {
	seen := map[string]bool{}
	var hits []Hit
	var walk func(n *html.Node, inSkip bool)
	walk = func(n *html.Node, inSkip bool) {
		if n.Type == html.ElementNode {
			if sel.skip != nil && sel.skip(n) {
				inSkip = true
			}
			if !inSkip && sel.result(n) {
				if hit, ok := readResult(n, pageURL, sel, seen); ok {
					hits = append(hits, hit)
				}
				// Results do not nest; skip the container's subtree now that it is read.
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inSkip)
		}
	}
	walk(doc, false)
	return hits
}

// readResult turns one result container into a hit, unwrapping redirect links. A container without
// a usable link (a video block, a knowledge card) is not a result.
func readResult(container *html.Node, pageURL *url.URL, sel serpSelectors, seen map[string]bool) (Hit, bool) {
	titleNode := sel.title(container)
	if titleNode == nil {
		return Hit{}, false
	}
	target, ok := unwrapLink(pageURL, attr(titleNode, "href"))
	if !ok || seen[target] {
		return Hit{}, false
	}
	title := collapseSpace(text(titleNode))
	if title == "" {
		return Hit{}, false
	}
	seen[target] = true
	hit := Hit{Title: title, URL: target}
	var desc *html.Node
	if sel.desc != nil {
		desc = sel.desc(container)
		if desc == nil {
			// A provider's description class moved: fall back to the first descriptive block.
			desc = firstMatch(container, func(c *html.Node) bool {
				return c.DataAtom == atom.P || c.DataAtom == atom.Div || c.DataAtom == atom.Span
			})
		}
	}
	if desc != nil {
		hit.Snippet = strings.TrimSpace(strings.TrimPrefix(descText(desc), title))
	}
	return hit, true
}

// descText is a description's text without headings. Links inside a description are kept because
// DuckDuckGo links its snippet. Callers drop the title when the description starts with it.
func descText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode {
			switch c.DataAtom {
			case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Script, atom.Style:
				return
			}
		}
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		for child := c.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return collapseSpace(b.String())
}

// genericSERP finds results on any SERP: a link with a meaningful title, then its block as the snippet
// when that block also holds a description. Links already seen and links that only point back into the
// search site are left out, so a menu or a pager does not become a result.
func genericSERP(doc *html.Node, pageURL *url.URL) []Hit {
	if doc == nil {
		return nil
	}
	var hits []Hit
	seen := map[string]bool{}
	var walk func(n *html.Node, inSkip bool)
	walk = func(n *html.Node, inSkip bool) {
		if n.Type == html.ElementNode {
			if genericSkip(n) {
				inSkip = true
			}
			if !inSkip && n.DataAtom == atom.A {
				h := attr(n, "href")
				if strings.TrimSpace(h) != "" && !strings.HasPrefix(h, "#") && !strings.HasPrefix(strings.ToLower(h), "javascript:") {
					target, ok := unwrapLink(pageURL, h)
					title := collapseSpace(text(n))
					if ok && !seen[target] && !sameSite(pageURL, target) && len([]rune(title)) >= minGenericText {
						seen[target] = true
						hit := Hit{Title: title, URL: target}
						if block := linkBlock(doc, n); block != nil {
							hit.Snippet = strings.TrimSpace(strings.TrimPrefix(collapseSpace(text(block)), title))
						}
						hits = append(hits, hit)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inSkip)
		}
	}
	walk(doc, false)
	return hits
}

// linkBlock returns the nearest ancestor of link that holds a description too, or nil.
func linkBlock(root, link *html.Node) *html.Node {
	for up := link.Parent; up != nil && up != root; up = up.Parent {
		if up.Type != html.ElementNode || up.DataAtom == atom.Body || genericSkip(up) {
			continue
		}
		if len(collapseSpace(text(up))) > len(collapseSpace(text(link)))+10 {
			return up
		}
	}
	return nil
}

// sameSite reports whether target is on the search page's host or one of its subdomains.
func sameSite(pageURL *url.URL, target string) bool {
	if pageURL == nil {
		return false
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return false
	}
	ph := strings.ToLower(pageURL.Hostname())
	th := strings.ToLower(u.Hostname())
	return th == ph || strings.HasSuffix(th, "."+ph)
}

// unwrapLink resolves href against pageURL and turns a redirect link back into the address it leads
// to. It reports false for links that are not web addresses.
func unwrapLink(pageURL *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(html.UnescapeString(href))
	if href == "" {
		return "", false
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	if pageURL != nil {
		u = pageURL.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if target := redirectTarget(u); target != "" {
		return target, true
	}
	u.Fragment = ""
	return u.String(), true
}

// redirectTarget reads the real address out of a redirect link.
func redirectTarget(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	q := u.Query()
	switch {
	case host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com"):
		if v := firstParam(q, "uddg", "u"); v != "" {
			return cleanTarget(v)
		}
	case host == "bing.com" || strings.HasSuffix(host, ".bing.com"):
		if v := firstParam(q, "u"); v != "" {
			if t := decodeBingTarget(v); t != "" {
				return t
			}
		}
	case host == "google.com" || strings.HasSuffix(host, ".google.com") || strings.HasPrefix(host, "google."):
		if v := firstParam(q, "q", "url"); v != "" {
			return cleanTarget(v)
		}
	}
	// Any site's own redirect endpoint, such as /l/?uddg= or /redirect?url=.
	path := strings.ToLower(u.Path)
	if strings.Contains(path, "redirect") || strings.Contains(path, "/url") {
		if v := firstParam(q, "url", "u", "target", "to", "uddg", "q"); v != "" {
			return cleanTarget(v)
		}
	}
	return ""
}

// decodeBingTarget reads Bing's u parameter, an "a1" marker followed by the base64url of the address.
func decodeBingTarget(v string) string {
	if len(v) > 2 && v[0] == 'a' && v[1] >= '1' && v[1] <= '9' {
		v = v[2:]
	}
	if data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(v, "=")); err == nil {
		if t := cleanTarget(string(data)); t != "" {
			return t
		}
	}
	return cleanTarget(v)
}

// cleanTarget accepts only a web address with a host.
func cleanTarget(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// firstParam returns the first non-empty value among names.
func firstParam(q url.Values, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(q.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// parseHTML parses a page's bytes as HTML. Documents arrive UTF-8 because Fetcher decodes them.
func parseHTML(body []byte) (*html.Node, error) {
	if len(body) == 0 {
		return nil, errors.New("search: empty page")
	}
	return html.Parse(bytes.NewReader(body))
}
