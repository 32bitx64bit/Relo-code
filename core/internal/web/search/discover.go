package search

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	web "github.com/relocode/relo/internal/web/types"
)

// ErrNeedTemplate is returned when a site offers no search description and no usable search form, so
// only the user can supply a template, with %s where the query goes.
var ErrNeedTemplate = errors.New("search: site has no discoverable search; ask the user for a search URL with %s where the query goes")

// maxDiscoveryFetches bounds the description documents one discovery reads.
const maxDiscoveryFetches = 6

// Discover finds the search template of pageURL the way a browser would. It reads the page's OpenSearch
// descriptions first and prefers the feed kinds; otherwise it infers a GET search form. When neither
// works it returns ErrNeedTemplate: ask the user for a search URL with %s.
func Discover(ctx context.Context, f Fetcher, pageURL string) (web.Provider, error) {
	if f == nil {
		return web.Provider{}, errors.New("search: discover without a fetcher")
	}
	page, err := parsePageURL(pageURL)
	if err != nil {
		return web.Provider{}, err
	}
	status, _, body, err := f.Fetch(ctx, page.String())
	if err != nil {
		return web.Provider{}, fmt.Errorf("search: fetch %s: %w", page, err)
	}
	if status >= 400 || len(body) == 0 {
		return web.Provider{}, fmt.Errorf("search: fetch %s: status %d", page, status)
	}
	root, err := parseHTML(body)
	if err != nil {
		return web.Provider{}, err
	}
	if p, ok := discoverOpenSearch(ctx, f, root, page); ok {
		return p, nil
	}
	if p, ok := discoverForm(root, page); ok {
		return p, nil
	}
	return web.Provider{}, ErrNeedTemplate
}

// kindRank orders result kinds for discovery: feeds and JSON first, an HTML page last.
func kindRank(kind web.ResultKind) int {
	switch kind {
	case web.KindRSS, web.KindAtom, web.KindJSON:
		return 2
	case web.KindHTML:
		return 1
	}
	return 0
}

// discoverOpenSearch reads the search descriptions the page advertises. Feed kinds beat HTML ones; the
// first usable URL of the best description wins.
func discoverOpenSearch(ctx context.Context, f Fetcher, root *html.Node, page *url.URL) (web.Provider, bool) {
	var best web.Provider
	found := false
	for i, linkURL := range findSearchLinks(root, page) {
		if i >= maxDiscoveryFetches {
			break
		}
		status, _, body, err := f.Fetch(ctx, linkURL.String())
		if err != nil || status >= 400 || len(body) == 0 {
			continue
		}
		desc, err := ParseDescription(body)
		if err != nil {
			continue
		}
		for _, t := range desc.Templates {
			if !sameOrigin(linkURL, t.Template) {
				continue // a description must not point the search at another site
			}
			p, err := makeProvider(web.ProviderCustom, t.Template, t.Kind, "", web.SourceOpenSearch)
			if err != nil {
				continue
			}
			if !found || kindRank(p.ResultKind) > kindRank(best.ResultKind) {
				best, found = p, true
			}
			if kindRank(best.ResultKind) == 2 {
				return best, true // nothing ranks higher
			}
		}
	}
	return best, found
}

// sameOrigin reports whether a template's URL is on the origin of the description that listed it.
func sameOrigin(description *url.URL, template string) bool {
	u, err := url.Parse(strings.ReplaceAll(template, "%s", "test"))
	if err != nil {
		return false
	}
	return u.Scheme == description.Scheme && u.Host == description.Host
}

// discoverForm infers a template from the page's search form: a GET form with a text field is turned
// into the URL a browser would submit. A POST-only form cannot be a query URL, so it is refused.
func discoverForm(root *html.Node, page *url.URL) (web.Provider, bool) {
	for _, form := range forms(root) {
		method := strings.ToUpper(strings.TrimSpace(attr(form, "method")))
		if method == "POST" || method == "DIALOG" {
			continue
		}
		field, hidden := queryField(form)
		if field == "" {
			continue
		}
		action := strings.TrimSpace(attr(form, "action"))
		if action == "" {
			action = page.String()
		}
		au, err := url.Parse(action)
		if err != nil {
			continue
		}
		au = page.ResolveReference(au)
		if au.Scheme != "http" && au.Scheme != "https" {
			continue
		}
		p, err := makeProvider(web.ProviderCustom, buildFormTemplate(au, field, hidden), web.KindHTML, "", web.SourceForm)
		if err != nil {
			continue
		}
		return p, true
	}
	return web.Provider{}, false
}

// buildFormTemplate renders a form action as a URL with %s for field's value, keeping the form's other
// query parameters and hidden inputs so the query URL matches a browser submission.
func buildFormTemplate(action *url.URL, field string, hidden [][2]string) string {
	var parts []string
	seenField := false
	for _, kv := range parsePairs(action.RawQuery) {
		if kv[0] == field {
			if !seenField {
				parts = append(parts, url.QueryEscape(field)+"=%s")
				seenField = true
			}
			continue
		}
		parts = append(parts, url.QueryEscape(kv[0])+"="+url.QueryEscape(kv[1]))
	}
	for _, kv := range hidden {
		if kv[0] == field {
			seenField = true // the hidden value is a default; the query replaces it
			continue
		}
		parts = append(parts, url.QueryEscape(kv[0])+"="+url.QueryEscape(kv[1]))
	}
	if !seenField {
		parts = append(parts, url.QueryEscape(field)+"=%s")
	}
	action.RawQuery = strings.Join(parts, "&")
	action.Fragment = ""
	return action.String()
}

// parsePairs reads a raw query into ordered key, value pairs.
func parsePairs(raw string) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		uk, err := url.QueryUnescape(k)
		if err != nil {
			uk = k
		}
		uv, err := url.QueryUnescape(v)
		if err != nil {
			uv = v
		}
		out = append(out, [2]string{uk, uv})
	}
	return out
}

// forms returns every form element in the document.
func forms(root *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Form {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

// queryField returns the name of the form's query text field, and the form's hidden values that a
// browser submits with it. A search-looking name wins; otherwise the first text field. Buttons, hidden
// fields and password fields are never the query.
func queryField(form *html.Node) (string, [][2]string) {
	var first, named string
	var hidden [][2]string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil && named == ""; c = c.NextSibling {
			if c.Type == html.ElementNode {
				name := strings.TrimSpace(attr(c, "name"))
				typ := strings.ToLower(strings.TrimSpace(attr(c, "type")))
				if typ == "" {
					typ = "text"
				}
				switch {
				case typ == "hidden" && name != "" && !hasAttr(c, "disabled"):
					hidden = append(hidden, [2]string{name, attr(c, "value")})
				case (typ == "text" || typ == "search") && name != "":
					if isQueryName(name) {
						named = name
						return
					}
					if first == "" {
						first = name
					}
				}
			}
			walk(c)
		}
	}
	walk(form)
	if named != "" {
		return named, hidden
	}
	return first, hidden
}

// isQueryName reports whether a field name looks like a search query.
func isQueryName(name string) bool {
	switch strings.ToLower(name) {
	case "q", "query", "search", "s", "keyword", "keywords", "term", "terms", "searchterm", "searchtext":
		return true
	}
	return false
}

// parsePageURL accepts an http(s) address.
func parsePageURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("search: %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("search: %q is not an http(s) URL", raw)
	}
	u.Fragment = ""
	return u, nil
}
