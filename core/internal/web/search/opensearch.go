package search

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"

	web "github.com/relocode/relo/internal/web/types"
)

// Description is a site's OpenSearch description: the name it gives itself and the search URLs it lists.
// It is what relo.web.discover reports back to the settings, so the user sees and approves the template.
type Description struct {
	// ShortName is the description's ShortName element, the site's own name for its search.
	ShortName string `json:"shortName,omitempty"`
	// Templates are the usable search URLs in the document's order.
	Templates []DescriptionURL `json:"templates,omitempty"`
}

// DescriptionURL is one search URL of a Description.
type DescriptionURL struct {
	// Template is the search URL with %s where the query goes.
	Template string `json:"template"`
	// Kind is the result kind the URL returns.
	Kind web.ResultKind `json:"kind"`
}

// ParseDescription reads an OpenSearch description document. It returns every Url it lists whose type this
// package can read, in the document's order; a description with no usable URL is an error.
func ParseDescription(doc []byte) (Description, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return Description{}, err
	}
	var d Description
	var urls []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "url":
				urls = append(urls, n)
			case "shortname":
				if d.ShortName == "" {
					d.ShortName = collapseSpace(text(n))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	for _, u := range urls {
		tmpl := strings.TrimSpace(attr(u, "template"))
		if tmpl == "" {
			continue
		}
		kind, ok := descriptionKind(attr(u, "type"))
		if !ok {
			continue
		}
		d.Templates = append(d.Templates, DescriptionURL{Template: FillTemplate(tmpl, ""), Kind: kind})
	}
	if len(d.Templates) == 0 {
		return Description{}, errors.New("search: OpenSearch description has no usable URL")
	}
	return d, nil
}

// descriptionKind maps an OpenSearch Url type to the result kind it returns, and says whether it is one
// this package can read.
func descriptionKind(mediaType string) (web.ResultKind, bool) {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch {
	case strings.Contains(mt, "rss"):
		return web.KindRSS, true
	case strings.Contains(mt, "atom"):
		return web.KindAtom, true
	case strings.Contains(mt, "json"):
		return web.KindJSON, true
	case strings.Contains(mt, "html"):
		return web.KindHTML, true
	}
	return "", false
}

// FillTemplate turns an OpenSearch template into a plain one: {searchTerms} becomes query, or %s when
// query is empty, and every other parameter takes its default, written with "?" as in {count?20}. A
// parameter without a default is left out.
func FillTemplate(tmpl, query string) string {
	var b strings.Builder
	rest := tmpl
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			return b.String() + rest
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return b.String() + rest
		}
		b.WriteString(rest[:i])
		param := rest[i+1 : i+j]
		rest = rest[i+j+1:]
		name, def, hasDefault := strings.Cut(param, "?")
		if strings.EqualFold(strings.TrimSpace(name), "searchterms") {
			if query == "" {
				b.WriteString("%s")
			} else {
				b.WriteString(url.QueryEscape(query))
			}
			continue
		}
		if !hasDefault {
			continue
		}
		if eq := strings.IndexByte(def, '='); eq >= 0 {
			def = def[eq+1:]
		}
		if v := strings.TrimSpace(def); v != "" {
			b.WriteString(url.QueryEscape(v))
		}
	}
}

// FillOpenSearch is FillTemplate with an empty query: the template as a %s URL.
func FillOpenSearch(tmpl string) string {
	return FillTemplate(tmpl, "")
}

// searchRelata lists the link relation values that advertise a search description.
var searchRelata = []string{"search", "searchterms"}

// isSearchLink reports whether a link element's rel and type advertise a search description.
func isSearchLink(n *html.Node, base *url.URL) (*url.URL, bool) {
	if n.Type != html.ElementNode || strings.ToLower(n.Data) != "link" {
		return nil, false
	}
	relOK := false
	for _, word := range strings.Fields(strings.ToLower(attr(n, "rel"))) {
		for _, want := range searchRelata {
			if word == want {
				relOK = true
			}
		}
	}
	if !relOK {
		return nil, false
	}
	typ := strings.ToLower(attr(n, "type"))
	if typ != "" && !strings.Contains(typ, "opensearchdescription") && !strings.Contains(typ, "rss") &&
		!strings.Contains(typ, "atom") && !strings.Contains(typ, "suggestion") && !strings.Contains(typ, "json") {
		return nil, false
	}
	href := strings.TrimSpace(attr(n, "href"))
	if href == "" {
		return nil, false
	}
	u, err := url.Parse(href)
	if err != nil {
		return nil, false
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}
	return u, true
}

// findSearchLinks returns the search description URLs a document advertises, in order.
func findSearchLinks(root *html.Node, base *url.URL) []*url.URL {
	var out []*url.URL
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if u, ok := isSearchLink(n, base); ok {
			out = append(out, u)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

// String renders the URL for a query, for logs and messages.
func (d DescriptionURL) String() string {
	return fmt.Sprintf("%s (%s)", FillTemplate(d.Template, "query"), d.Kind)
}
