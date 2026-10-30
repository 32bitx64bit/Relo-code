package search

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	web "github.com/relocode/relo/internal/web/types"
)

// httptestFetcher fetches through an in-process test server; tests never touch the network.
type httptestFetcher struct {
	client  *http.Client
	base    string
	fetched []string
}

func (f *httptestFetcher) Fetch(_ context.Context, rawURL string) (int, string, []byte, error) {
	if !strings.HasPrefix(rawURL, f.base) {
		return 0, "", nil, errors.New("fetch outside the test server: " + rawURL)
	}
	f.fetched = append(f.fetched, rawURL)
	resp, err := f.client.Get(rawURL)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	var b strings.Builder
	if _, err := io.Copy(&b, resp.Body); err != nil {
		return 0, "", nil, err
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), []byte(b.String()), nil
}

func newFetcher(t *testing.T, h http.HandlerFunc) (*httptestFetcher, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &httptestFetcher{client: srv.Client(), base: srv.URL}, srv
}

// pageURL parses a URL for resolving results.
func pageURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// parsed parses body as p's results, failing the test on error.
func parsed(t *testing.T, p web.Provider, body string, page *url.URL) []Hit {
	t.Helper()
	hits, err := Parse(p, []byte(body), page)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	return hits
}

func wantHit(t *testing.T, hits []Hit, title, link, snippet string) {
	t.Helper()
	for _, h := range hits {
		if h.Title == title {
			if h.URL != link {
				t.Fatalf("hit %q URL = %q, want %q", title, h.URL, link)
			}
			if snippet != "" && h.Snippet != snippet {
				t.Fatalf("hit %q snippet = %q, want %q", title, h.Snippet, snippet)
			}
			return
		}
	}
	t.Fatalf("hit %q missing from %+v", title, hits)
}

func TestProvider(t *testing.T) {
	for _, name := range []web.ProviderName{web.ProviderDuckDuckGo, web.ProviderBrave, web.ProviderBing} {
		p, err := Provider(name, "", "", "")
		if err != nil {
			t.Fatalf("Provider(%s): %v", name, err)
		}
		got, err := QueryURL(p, "relo web & co")
		if err != nil {
			t.Fatalf("QueryURL: %v", err)
		}
		if want := strings.Replace(p.Template, "%s", "relo+web+%26+co", 1); got != want {
			t.Fatalf("%s query URL = %q, want %q", name, got, want)
		}
		if p.ResultKind != web.KindHTML || p.Origin == "" || p.Source != web.SourceUser {
			t.Fatalf("%s provider = %+v", name, p)
		}
	}
	p, err := Provider("", "", "", "")
	if err != nil || p.Name != web.ProviderDuckDuckGo {
		t.Fatalf("empty provider = %+v, %v", p, err)
	}
	if _, err := Provider("nope", "", "", ""); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("unknown provider err = %v", err)
	}
	if _, err := Provider(web.ProviderCustom, " ", "", ""); err == nil {
		t.Fatal("custom provider without template accepted")
	}
	if _, err := Provider(web.ProviderCustom, "file:///etc/osdd?q=%s", "", ""); err == nil {
		t.Fatal("custom provider with a file template accepted")
	}
	if _, err := Provider(web.ProviderCustom, "https://search.example/?q=%s", web.KindHTML, "https://other.example"); err != nil {
		t.Fatal(err)
	}

	p, err = Provider(web.ProviderCustom, "https://search.example/?q=%s", "", "")
	if err != nil {
		t.Fatalf("custom provider: %v", err)
	}
	if p.Origin != "https://search.example" || p.ResultKind != web.KindHTML || p.Source != web.SourceUser {
		t.Fatalf("custom provider = %+v", p)
	}
	if _, err := QueryURL(p, "  "); err == nil {
		t.Fatal("empty query accepted")
	}
}

func TestParseDuckDuckGoSERP(t *testing.T) {
	body := `<!doctype html><html><body>
<div class="results">
<div class="result results_links">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=abc">The Go <b>Programming</b> Language</a></h2>
  <a class="result__snippet" href="#">Go is an open source <b>programming</b> language.</a>
</div>
<div class="result results_links">
  <h2 class="result__title"><a class="result__a" href="https://example.com/direct">Direct page</a></h2>
  <div class="result__snippet">No redirect here.</div>
</div>
</div></body></html>`
	hits := parsed(t, web.Provider{Name: web.ProviderDuckDuckGo, ResultKind: web.KindHTML}, body, pageURL(t, "https://html.duckduckgo.com/html/?q=go"))
	wantHit(t, hits, "The Go Programming Language", "https://go.dev/doc/", "Go is an open source programming language.")
	wantHit(t, hits, "Direct page", "https://example.com/direct", "No redirect here.")
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestParseBraveSERP(t *testing.T) {
	body := `<html><body>
<div id="results" class="snippet">
  <div class="snippet-title"><a href="https://sqlite.org/">SQLite Home Page</a></div>
  <div class="snippet-description">SQLite is a C library that implements an SQL database engine.</div>
</div>
<div id="results2" class="snippet snippet-2">
  <div class="snippet-title"><a href="https://go.dev/">The Go Programming Language</a></div>
  <div class="snippet-content">Build simple, secure, scalable systems.</div>
</div>
</body></html>`
	hits := parsed(t, web.Provider{Name: web.ProviderBrave, ResultKind: web.KindHTML}, body, pageURL(t, "https://search.brave.com/search?q=db"))
	wantHit(t, hits, "SQLite Home Page", "https://sqlite.org/", "SQLite is a C library that implements an SQL database engine.")
	wantHit(t, hits, "The Go Programming Language", "https://go.dev/", "Build simple, secure, scalable systems.")
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestParseBingSERPUnwrapsBase64Redirect(t *testing.T) {
	// The href's u parameter is "a1" followed by the base64url of https://store.example/.
	body := `<html><body><ol id="b_results">
<li class="b_algo"><h2><a href="https://www.bing.com/ck/a?!&&p=x&u=a1aHR0cHM6Ly9zdG9yZS5leGFtcGxlLw&ntb=1">Store result</a></h2>
  <div class="b_caption"><p class="b_lineclamp2">Buy things at the store.</p></div></li>
<li class="b_algo"><h2><a href="https://plain.example/page">Plain result</a></h2>
  <p class="b_lineclamp2">A plain snippet.</p></li>
</ol></body></html>`
	hits := parsed(t, web.Provider{Name: web.ProviderBing, ResultKind: web.KindHTML}, body, pageURL(t, "https://www.bing.com/search?q=store"))
	wantHit(t, hits, "Store result", "https://store.example/", "Buy things at the store.")
	wantHit(t, hits, "Plain result", "https://plain.example/page", "A plain snippet.")
}

func TestParseGenericSERP(t *testing.T) {
	body := `<html><body>
<nav><a href="https://unknown.test/?page=2">Next page</a></nav>
<div class="results">
  <div class="item"><a href="https://a.example/one">First result</a><p>A description of the first result.</p></div>
  <div class="item"><a href="https://b.example/two">Second <em>result</em></a><p>Another description.</p></div>
  <div class="sponsored"><a href="https://ads.example/buy">Buy now</a></div>
</div></body></html>`
	hits := parsed(t, web.Provider{Name: web.ProviderCustom, ResultKind: web.KindHTML}, body, pageURL(t, "https://unknown.test/?q=x"))
	wantHit(t, hits, "First result", "https://a.example/one", "A description of the first result.")
	wantHit(t, hits, "Second result", "https://b.example/two", "Another description.")
	for _, h := range hits {
		if strings.Contains(h.URL, "unknown.test") || strings.Contains(h.URL, "ads.example") {
			t.Fatalf("unexpected hit %+v", h)
		}
	}
}

func TestParseEmptySERPIsNoResults(t *testing.T) {
	_, err := Parse(web.Provider{Name: web.ProviderDuckDuckGo, ResultKind: web.KindHTML}, []byte(`<html><body><p>Checking your browser</p></body></html>`), pageURL(t, "https://html.duckduckgo.com/"))
	if !errors.Is(err, ErrNoResults) {
		t.Fatalf("err = %v, want ErrNoResults", err)
	}
}

func TestParseRSS(t *testing.T) {
	body := `<?xml version="1.0"?><rss version="2.0"><channel>
<item><title>Alpha</title><link>https://alpha.example/</link><description>First &amp; best.</description></item>
<item><title>Beta</title><link>https://beta.example/</link><description><![CDATA[<p>Second <b>bold</b> item</p>]]></description></item>
</channel></rss>`
	hits := parsed(t, web.Provider{Name: web.ProviderCustom, ResultKind: web.KindRSS}, body, nil)
	wantHit(t, hits, "Alpha", "https://alpha.example/", "First & best.")
	wantHit(t, hits, "Beta", "https://beta.example/", "Second bold item")
}

func TestParseAtom(t *testing.T) {
	body := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
<entry><title>Gamma</title><link rel="alternate" href="https://gamma.example/"/><summary>Gamma summary</summary></entry>
<entry><title>Delta</title><link rel="self" href="https://delta.example/self"/><link rel="alternate" href="https://delta.example/"/><content>Delta content</content></entry>
</feed>`
	hits := parsed(t, web.Provider{Name: web.ProviderCustom, ResultKind: web.KindAtom}, body, nil)
	wantHit(t, hits, "Gamma", "https://gamma.example/", "Gamma summary")
	wantHit(t, hits, "Delta", "https://delta.example/", "Delta content")
}

func TestParseJSON(t *testing.T) {
	body := `{"web":{"pages":[
		{"Title":"Epsilon","URL":"https://epsilon.example/","Snippet":"JSON result"},
		{"title":"Zeta","url":"https://zeta.example/","description":"Second JSON result"}
	]}}`
	hits := parsed(t, web.Provider{Name: web.ProviderCustom, ResultKind: web.KindJSON}, body, nil)
	wantHit(t, hits, "Epsilon", "https://epsilon.example/", "JSON result")
	wantHit(t, hits, "Zeta", "https://zeta.example/", "Second JSON result")

	arr := `[{"name":"Eta","href":"https://eta.example/","desc":"Array form"}]`
	hits = parsed(t, web.Provider{Name: web.ProviderCustom, ResultKind: web.KindJSON}, arr, nil)
	wantHit(t, hits, "Eta", "https://eta.example/", "Array form")
}

func TestParseJSONFromPageText(t *testing.T) {
	// A browser lane reports results as page text: a fenced block, then a line holding an array.
	body := "browsed https://site.test/search\n```json\n[{\"title\":\"In fence\",\"url\":\"https://in.example/\"}]\n```\nplain [{\"title\":\"Plain\",\"url\":\"https://plain.example/\",\"snippet\":\"Line snippet\"}]\n"
	hits := parsed(t, web.Provider{Name: web.ProviderCustom, ResultKind: web.KindJSON}, body, nil)
	wantHit(t, hits, "In fence", "https://in.example/", "")
	wantHit(t, hits, "Plain", "https://plain.example/", "Line snippet")
}

func TestDiscoverOpenSearchWithFeedTemplate(t *testing.T) {
	var f *httptestFetcher
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><head><link rel="search" type="application/opensearchdescription+xml" href="/osdd.xml"></head><body>
<form action="/search" method="get"><input type="text" name="wiki-query"></form></body></html>`))
		case "/osdd.xml":
			w.Write([]byte(`<?xml version="1.0"?><OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
<ShortName>Site</ShortName>
<Url type="text/html" method="get" template="` + f.base + `/results?q={searchTerms}&amp;lang={language?}"/>
<Url type="application/rss+xml" method="get" template="` + f.base + `/results.rss?query={searchTerms}"/>
</OpenSearchDescription>`))
		case "/results.rss":
			w.Write([]byte("<rss><channel><item><title>Found</title><link>https://found.example/</link><description>Feed hit.</description></item></channel></rss>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f = &httptestFetcher{client: srv.Client(), base: srv.URL}

	p, err := Discover(context.Background(), f, srv.URL+"/")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.Source != web.SourceOpenSearch || p.ResultKind != web.KindRSS {
		t.Fatalf("provider = %+v, want opensearch rss", p)
	}
	if want := srv.URL + "/results.rss?query=%s"; p.Template != want {
		t.Fatalf("template = %q, want %q", p.Template, want)
	}
	if p.Origin != srv.URL || p.Name != web.ProviderCustom {
		t.Fatalf("provider = %+v", p)
	}
	hits, err := Search(context.Background(), f, p, "hello world")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	wantHit(t, hits, "Found", "https://found.example/", "Feed hit.")
	if last := f.fetched[len(f.fetched)-1]; last != srv.URL+"/results.rss?query=hello+world" {
		t.Fatalf("searched %q", last)
	}
}

func TestDiscoverOpenSearchHTMLUrl(t *testing.T) {
	f, srv := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/osdd.xml" {
			w.Write([]byte(`<osdd><ShortName>Docs</ShortName><Url type="text/html" template="` + fBase(r) + `/find?terms={searchTerms}"/></osdd>`))
			return
		}
		w.Write([]byte(`<html><head><link rel="search" href="/osdd.xml"></head></html>`))
	})
	p, err := Discover(context.Background(), f, srv.URL+"/")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.Source != web.SourceOpenSearch || p.ResultKind != web.KindHTML || p.Template != srv.URL+"/find?terms=%s" {
		t.Fatalf("provider = %+v", p)
	}
}

// fBase returns a request's base URL, for description documents built inside a handler.
func fBase(r *http.Request) string {
	return "http://" + r.Host
}

func TestDiscoverOpenSearchIgnoresForeignOrigin(t *testing.T) {
	f, srv := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/osdd.xml" {
			w.Write([]byte(`<osdd><Url type="text/html" template="https://elsewhere.example/?q={searchTerms}"/></osdd>`))
			return
		}
		w.Write([]byte(`<html><head><link rel="search" href="/osdd.xml"></head>
<body><form action="/q"><input type="text" name="q"></form></body></html>`))
	})
	p, err := Discover(context.Background(), f, srv.URL+"/")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.Source != web.SourceForm || p.Template != srv.URL+"/q?q=%s" {
		t.Fatalf("provider = %+v, want the form", p)
	}
}

func TestDiscoverFormInference(t *testing.T) {
	f, srv := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body>
<form role="search" action="/find"><input type="hidden" name="site" value="docs"><input type="search" name="q" placeholder="Search"></form>
</body></html>`))
	})
	p, err := Discover(context.Background(), f, srv.URL+"/docs/")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.Source != web.SourceForm || p.ResultKind != web.KindHTML {
		t.Fatalf("provider = %+v, want form html", p)
	}
	if want := srv.URL + "/find?site=docs&q=%s"; p.Template != want {
		t.Fatalf("template = %q, want %q", p.Template, want)
	}
}

func TestDiscoverPrefersFormQueryField(t *testing.T) {
	f, srv := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><form action="">
<input type="text" name="username"><input type="text" name="query"><input type="submit"></form></body></html>`))
	})
	p, err := Discover(context.Background(), f, srv.URL+"/search")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := srv.URL + "/search?query=%s"; p.Template != want {
		t.Fatalf("template = %q, want %q", p.Template, want)
	}
}

func TestDiscoverRejectsPOSTOnlyForm(t *testing.T) {
	f, srv := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><form action="/search" method="post"><input type="text" name="q"></form></body></html>`))
	})
	_, err := Discover(context.Background(), f, srv.URL+"/")
	if !errors.Is(err, ErrNeedTemplate) {
		t.Fatalf("err = %v, want ErrNeedTemplate", err)
	}
}

func TestDiscoverFallsBackToUserTemplate(t *testing.T) {
	f, srv := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><h1>No search here</h1><a href="/x">X</a></body></html>`))
	})
	_, err := Discover(context.Background(), f, srv.URL+"/")
	if !errors.Is(err, ErrNeedTemplate) {
		t.Fatalf("err = %v, want ErrNeedTemplate", err)
	}
}

func TestDiscoverBrokenOpenSearchFallsBackToForm(t *testing.T) {
	f, srv := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/osdd.xml" {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		w.Write([]byte(`<html><head><link rel="search" type="application/opensearchdescription+xml" href="/osdd.xml"></head>
<body><form action="/q"><input type="text" name="q"></form></body></html>`))
	})
	p, err := Discover(context.Background(), f, srv.URL+"/")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.Source != web.SourceForm || p.Template != srv.URL+"/q?q=%s" {
		t.Fatalf("provider = %+v", p)
	}
}

func TestDiscoverRefusesNonHTTPURL(t *testing.T) {
	f, _ := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := Discover(context.Background(), f, "file:///etc/passwd"); err == nil {
		t.Fatal("file URL accepted")
	}
}

func TestFillOpenSearch(t *testing.T) {
	cases := map[string]string{
		"https://x.test/?q={searchTerms}":                               "https://x.test/?q=%s",
		"https://x.test/?q={searchTerms}&lang={language?}":              "https://x.test/?q=%s&lang=",
		"https://x.test/?q={searchTerms}&n={count?20}&s={startIndex?1}": "https://x.test/?q=%s&n=20&s=1",
		"https://x.test/?q={searchTerms}&m={moz:method?}":               "https://x.test/?q=%s&m=",
	}
	for in, want := range cases {
		if got := FillOpenSearch(in); got != want {
			t.Fatalf("FillOpenSearch(%q) = %q, want %q", in, got, want)
		}
	}
}
