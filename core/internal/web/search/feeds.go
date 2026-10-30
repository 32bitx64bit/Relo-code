package search

import (
	"encoding/json"
	"encoding/xml"
	"strings"

	"golang.org/x/net/html"
)

// parseRSS reads hits from an RSS 2.0 or RSS 1.0 (RDF) feed.
func parseRSS(body []byte) ([]Hit, error) {
	var doc struct {
		XMLName xml.Name `xml:"rss"`
		Channel struct {
			Items []rssItem `xml:"item"`
		} `xml:"channel"`
		Items []rssItem `xml:"item"` // RDF feeds list items at the top level
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	items := doc.Channel.Items
	if len(items) == 0 {
		items = doc.Items
	}
	hits := make([]Hit, 0, len(items))
	for _, it := range items {
		link := strings.TrimSpace(it.Link)
		if link == "" {
			link = strings.TrimSpace(it.GUID)
		}
		if !isHTTP(link) {
			continue
		}
		hits = append(hits, Hit{Title: collapseSpace(it.Title), URL: link, Snippet: collapseSpace(stripTags(it.Description))})
	}
	return requireHits(hits)
}

// rssItem is one RSS item.
type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	Description string `xml:"description"`
}

// parseAtom reads hits from an Atom feed.
func parseAtom(body []byte) ([]Hit, error) {
	var doc struct {
		XMLName xml.Name    `xml:"feed"`
		Entries []atomEntry `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		link := ""
		for _, l := range e.Links {
			if (l.Rel == "" || l.Rel == "alternate") && isHTTP(l.Href) {
				link = strings.TrimSpace(l.Href)
				break
			}
		}
		if link == "" && isHTTP(e.ID) {
			link = strings.TrimSpace(e.ID)
		}
		if link == "" {
			continue
		}
		summary := e.Summary
		if strings.TrimSpace(summary) == "" {
			summary = e.Content
		}
		hits = append(hits, Hit{Title: collapseSpace(e.Title), URL: link, Snippet: collapseSpace(stripTags(summary))})
	}
	return requireHits(hits)
}

// atomEntry is one Atom entry.
type atomEntry struct {
	Title   string     `xml:"title"`
	ID      string     `xml:"id"`
	Summary string     `xml:"summary"`
	Content string     `xml:"content"`
	Links   []atomLink `xml:"link"`
}

// atomLink is one Atom link element.
type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

// parseJSON reads hits from JSON results. It accepts a top-level array of objects or an object holding that
// array under results, hits, items, data, entries, pages, links or web.pages (DuckDuckGo's shape). Each
// object may name its fields freely; the usual spellings are recognised case-insensitively.
func parseJSON(body []byte) ([]Hit, error) {
	var hits []Hit
	seen := map[string]bool{}
	for _, doc := range jsonValue(body) {
		rows, _ := jsonRows(doc)
		for _, row := range rows {
			obj, ok := row.(map[string]any)
			if !ok {
				continue
			}
			link := jsonString(obj, "url", "u", "href", "link", "firstURL", "page_url")
			if !isHTTP(link) || seen[link] {
				continue
			}
			seen[link] = true
			hits = append(hits, Hit{
				Title:   jsonString(obj, "title", "name", "text", "heading"),
				URL:     link,
				Snippet: jsonString(obj, "snippet", "description", "desc", "body", "abstract", "content", "summary"),
			})
		}
	}
	return requireHits(hits)
}

// jsonValue decodes every JSON document body holds: the whole body, then each fenced block, then each part
// that starts at a bracket. A page's text may wrap the JSON in prose, as a fenced block or a browser lane's
// report does, and may hold several documents.
func jsonValue(body []byte) []any {
	var docs []any
	try := func(s string) {
		dec := json.NewDecoder(strings.NewReader(s))
		var v any
		if err := dec.Decode(&v); err != nil {
			return
		}
		if rows, ok := jsonRows(v); ok && firstObject(rows) {
			docs = append(docs, v)
		}
	}
	try(string(body))
	for _, candidate := range jsonCandidates(string(body)) {
		try(candidate)
	}
	return docs
}

// jsonCandidates returns the parts of text that could be a JSON document: fenced blocks first, then lines
// and the rest of the text from any bracket on, so a report that prints a label and a pretty-printed array is
// read too.
func jsonCandidates(text string) []string {
	var out []string
	var add func(s string)
	add = func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	rest := text
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			break
		}
		rest = rest[open+3:]
		end := strings.Index(rest, "```")
		if end < 0 {
			break
		}
		block := rest[:end]
		if nl := strings.IndexByte(block, '\n'); nl >= 0 {
			block = block[nl+1:] // drop the fence's language tag
		}
		add(block)
		rest = rest[end+3:]
	}
	for _, line := range strings.Split(text, "\n") {
		add(line)
		if i := strings.IndexAny(line, "[{"); i >= 0 {
			add(line[i:])
		}
	}
	if i := strings.IndexAny(text, "[{"); i >= 0 {
		add(text[i:])
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if i := strings.IndexAny(line, "[{"); i >= 0 {
			add(line) // a line holding a bracket may open an array that continues on later lines
		}
	}
	return out
}

// firstObject reports whether a row list leads with an object, so prose that merely starts with a bracket
// is not mistaken for results.
func firstObject(rows []any) bool {
	if len(rows) == 0 {
		return false
	}
	_, ok := rows[0].(map[string]any)
	return ok
}

// jsonRows finds the result array in a decoded JSON value.
func jsonRows(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case map[string]any:
		for _, key := range []string{"results", "hits", "items", "data", "entries", "pages", "links", "response", "web"} {
			child, ok := lookupKey(t, key)
			if !ok {
				continue
			}
			if rows, ok := jsonRows(child); ok {
				return rows, true
			}
		}
	}
	return nil, false
}

// jsonString returns the first non-empty string among keys, matched case-insensitively.
func jsonString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := lookupKey(obj, k); ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return collapseSpace(stripTags(s))
			}
		}
	}
	return ""
}

// lookupKey finds a key in obj by its lowercase name.
func lookupKey(obj map[string]any, key string) (any, bool) {
	for k, v := range obj {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// isHTTP reports whether link is an absolute web address.
func isHTTP(link string) bool {
	l := strings.ToLower(link)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// requireHits reports ErrNoResults for an empty list.
func requireHits(hits []Hit) ([]Hit, error) {
	if len(hits) == 0 {
		return nil, ErrNoResults
	}
	return hits, nil
}

// stripTags keeps the text of an HTML fragment and drops its markup.
func stripTags(s string) string {
	root, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	return text(root)
}
