package search

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	web "github.com/relocode/relo/internal/web/types"
)

// ErrUnsupportedProvider is returned for a provider that has no built-in builder or parser.
var ErrUnsupportedProvider = errors.New("search: unsupported provider")

// ErrNoResults marks a page that has no results in it at all, such as a challenge or an error page.
var ErrNoResults = errors.New("search: no results in page")

// engine describes one built-in provider: its query URL, its origin and its page parser.
type engine struct {
	// Name is the provider's identifier.
	Name web.ProviderName
	// Template is the search URL with %s where the escaped query goes.
	Template string
	// Origin is the scheme and host the query URL stays on.
	Origin string
	// selectors read the provider's result page; defined in serp.go, one per provider.
	selectors serpSelectors
}

// builtins holds the built-in providers.
var builtins = map[web.ProviderName]engine{
	web.ProviderDuckDuckGo: {Name: web.ProviderDuckDuckGo, Template: "https://html.duckduckgo.com/html/?q=%s", Origin: "https://html.duckduckgo.com", selectors: duckDuckGoSelectors},
	web.ProviderBrave:      {Name: web.ProviderBrave, Template: "https://search.brave.com/search?q=%s", Origin: "https://search.brave.com", selectors: braveSelectors},
	web.ProviderBing:       {Name: web.ProviderBing, Template: "https://www.bing.com/search?q=%s", Origin: "https://www.bing.com", selectors: bingSelectors},
}

// DefaultProvider returns the built-in provider named name; an empty name means DuckDuckGo.
func DefaultProvider(name web.ProviderName) (web.Provider, error) {
	if name == "" {
		name = web.ProviderDuckDuckGo
	}
	b, ok := builtins[name]
	if !ok {
		return web.Provider{}, fmt.Errorf("%w: %q", ErrUnsupportedProvider, name)
	}
	return web.Provider{Name: b.Name, Template: b.Template, ResultKind: web.KindHTML, Source: web.SourceUser, Origin: b.Origin}, nil
}

// Provider builds the provider for a setting. An empty name means DuckDuckGo. A custom setting needs a
// template; its result kind defaults to HTML and its origin is taken from the template when empty.
func Provider(name web.ProviderName, customTemplate string, kind web.ResultKind, origin string) (web.Provider, error) {
	if name == "" || name == web.ProviderDuckDuckGo || name == web.ProviderBrave || name == web.ProviderBing {
		return DefaultProvider(name)
	}
	if name != web.ProviderCustom {
		return web.Provider{}, fmt.Errorf("%w: %q", ErrUnsupportedProvider, name)
	}
	if strings.TrimSpace(customTemplate) == "" {
		return web.Provider{}, errors.New("search: custom provider needs a template")
	}
	return makeProvider(web.ProviderCustom, customTemplate, kind, origin, web.SourceUser)
}

// makeProvider fills a provider's kind and origin, deriving the origin from the template when none is
// given, and checks the template is an http(s) URL with %s where the query goes (or no placeholder at
// all, in which case QueryURL appends q=).
func makeProvider(name web.ProviderName, template string, kind web.ResultKind, origin string, source web.SourceKind) (web.Provider, error) {
	if kind == "" {
		kind = web.KindHTML
	}
	switch kind {
	case web.KindHTML, web.KindRSS, web.KindAtom, web.KindJSON:
	default:
		return web.Provider{}, fmt.Errorf("search: unsupported result kind %q", kind)
	}
	template = strings.TrimSpace(template)
	probe := strings.ReplaceAll(template, "%s", "test")
	u, err := url.Parse(probe)
	if err != nil {
		return web.Provider{}, fmt.Errorf("search: template %q: %w", template, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return web.Provider{}, fmt.Errorf("search: template %q is not an http(s) URL", template)
	}
	if origin == "" {
		origin = u.Scheme + "://" + u.Host
	}
	return web.Provider{Name: name, Template: template, ResultKind: kind, Source: source, Origin: origin}, nil
}

// QueryURL fills the provider's template with query, percent-escaped.
func QueryURL(p web.Provider, query string) (string, error) {
	if strings.TrimSpace(p.Template) == "" {
		return "", errors.New("search: provider has no template")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return "", errors.New("search: empty query")
	}
	escaped := url.QueryEscape(q)
	if strings.Contains(p.Template, "%s") {
		return strings.ReplaceAll(p.Template, "%s", escaped), nil
	}
	sep := "?"
	if strings.Contains(p.Template, "?") {
		sep = "&"
	}
	return p.Template + sep + "q=" + escaped, nil
}

// Fetcher fetches a search page or a search description. It returns the status code, the Content-Type
// header and the body, decoded to UTF-8. Implementations apply the network guard; this package makes no
// requests itself.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (status int, contentType string, body []byte, err error)
}

// Hit is one search result.
type Hit struct {
	// Title is the result's title.
	Title string `json:"title,omitempty"`
	// URL is the result's page address.
	URL string `json:"url,omitempty"`
	// Snippet is the result's short description.
	Snippet string `json:"snippet,omitempty"`
}

// Search runs one query against p: it fetches the provider's query URL and parses the results. Results
// are untrusted web data; redirect links are unwrapped.
func Search(ctx context.Context, f Fetcher, p web.Provider, query string) ([]Hit, error) {
	if f == nil {
		return nil, errors.New("search: search without a fetcher")
	}
	raw, err := QueryURL(p, query)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	status, _, body, err := f.Fetch(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("search: fetch %s: %w", p.Name, err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("search: fetch %s: status %d", p.Name, status)
	}
	return Parse(p, body, u)
}

// Parse reads results from body for provider p, picking the parser from the provider and its kind.
func Parse(p web.Provider, body []byte, pageURL *url.URL) ([]Hit, error) {
	kind := p.ResultKind
	if kind == "" {
		kind = web.KindHTML
	}
	switch kind {
	case web.KindRSS:
		return parseRSS(body)
	case web.KindAtom:
		return parseAtom(body)
	case web.KindJSON:
		return parseJSON(body)
	case web.KindHTML:
		if b, ok := builtins[p.Name]; ok {
			return parseSERP(body, pageURL, b.selectors)
		}
		return parseSERP(body, pageURL, genericSelectors)
	}
	return nil, fmt.Errorf("search: unsupported result kind %q", kind)
}
