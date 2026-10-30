package extract

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// chromeTags hold page chrome. They are skipped unless the chosen content root is one of them.
var chromeTags = map[atom.Atom]bool{
	atom.Nav: true, atom.Aside: true, atom.Footer: true, atom.Header: true,
}

// blockTags are rendered as their own blocks.
var blockTags = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true, atom.Ul: true,
	atom.Ol: true, atom.Table: true, atom.Blockquote: true, atom.Pre: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.Hr: true, atom.Figure: true,
	atom.Figcaption: true, atom.Nav: true, atom.Aside: true, atom.Footer: true, atom.Header: true, atom.Dl: true,
	atom.Dt: true, atom.Dd: true, atom.Summary: true, atom.Details: true,
}

// containerTags pass their blocks through without a wrapper of their own.
var containerTags = map[atom.Atom]bool{
	atom.Div: true, atom.Section: true, atom.Main: true, atom.Body: true, atom.Html: true, atom.Header: true,
	atom.Footer: true, atom.Nav: true, atom.Aside: true, atom.Figure: true, atom.Details: true, atom.Fieldset: true,
	atom.Center: true,
}

var (
	reFence         = regexp.MustCompile("^```")
	reCodeFenceRun  = regexp.MustCompile("`{3,}")
	reInlineRun     = regexp.MustCompile("`+")
	reOrderedMarker = regexp.MustCompile(`^(\s*)(\d+)\.(\s)`)
	reManySpaces    = regexp.MustCompile(`[ \t\x{00a0}]+`)
	reSpacePunct    = regexp.MustCompile(` ([,.;:!?)\]])`)
)

// markdownRenderer writes the content subtree as markdown and records the links it kept, in order.
type markdownRenderer struct {
	base      *url.URL
	chromeCut bool // the content root sits inside page chrome, so chrome is rendered
	seen      map[string]bool
	list      []string
}

func newRenderer(base *url.URL) *markdownRenderer {
	return &markdownRenderer{base: base, seen: map[string]bool{}}
}

// links lists the links in document order, without duplicates.
func (r *markdownRenderer) links() []string {
	return r.list
}

func (r *markdownRenderer) addLink(href string) {
	if href == "" || r.seen[href] {
		return
	}
	r.seen[href] = true
	r.list = append(r.list, href)
}

// absolute resolves href against the page URL and drops schemes a readable page cannot use.
func (r *markdownRenderer) absolute(href string) string {
	href = strings.TrimSpace(zeroWidth.Replace(href))
	if href == "" {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "mailto":
		default:
			return ""
		}
	}
	if u.Scheme == "" && u.Host == "" && u.Path == "" && u.Fragment != "" {
		return href // a fragment of the same page
	}
	if r.base == nil {
		return href
	}
	return r.base.ResolveReference(u).String()
}

// skip reports whether n is left out of the output.
func (r *markdownRenderer) skip(n *html.Node) bool {
	if n.Type != html.ElementNode || junkTags[n.DataAtom] || isHidden(n) {
		return true
	}
	return chromeTags[n.DataAtom] && !r.chromeCut
}

// blocks renders the children of n, each block separated by a blank line.
func (r *markdownRenderer) blocks(n *html.Node) string {
	var out []string
	r.walkBlocks(n, &out)
	return strings.Join(out, "\n\n")
}

// walkBlocks appends each block child of n to out. A run of inline nodes (bare text, spans, links) is one
// block: a paragraph of its own.
func (r *markdownRenderer) walkBlocks(n *html.Node, out *[]string) {
	var run *flow
	flush := func() {
		if run != nil {
			if text := tidyFlow(run.md.String()); text != "" {
				*out = append(*out, r.escapeFence(text))
			}
		}
		run = nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && blockTags[c.DataAtom] {
			flush()
			r.block(c, out)
			continue
		}
		if run == nil {
			run = &flow{}
		}
		run.add(r, c)
	}
	flush()
}

// block renders one block element.
func (r *markdownRenderer) block(n *html.Node, out *[]string) {
	if r.skip(n) {
		return
	}
	// A wrapper holding a lone text block contributes that block itself: "div > div > p" is one paragraph, not
	// the wrapper's text run plus the paragraph.
	if containerTags[n.DataAtom] && singleTextBlock(n) {
		r.walkBlocks(n, out)
		return
	}
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		if text := tidyFlow(r.inline(n)); text != "" {
			level := headingLevel(n.DataAtom)
			*out = append(*out, strings.Repeat("#", level)+" "+r.escapeFence(text))
		}
	case atom.P, atom.Dt:
		if text := tidyFlow(r.inline(n)); text != "" {
			*out = append(*out, r.escapeFence(text))
		}
	case atom.Dd:
		if text := tidyFlow(r.inline(n)); text != "" {
			*out = append(*out, "  "+r.escapeFence(text))
		}
	case atom.Pre:
		if code := codeText(n); strings.TrimSpace(code) != "" {
			*out = append(*out, codeBlock(code, codeLanguage(n)))
		}
	case atom.Ul, atom.Ol:
		if items := r.renderList(n, 0); len(items) > 0 {
			*out = append(*out, strings.Join(items, "\n"))
		}
	case atom.Blockquote:
		if inner := r.blocks(n); strings.TrimSpace(inner) != "" {
			var quoted []string
			for _, line := range strings.Split(inner, "\n") {
				quoted = append(quoted, strings.TrimRight("> "+line, " "))
			}
			*out = append(*out, strings.Join(quoted, "\n"))
		}
	case atom.Table:
		if table := r.table(n); table != "" {
			*out = append(*out, table)
		}
	case atom.Hr:
		*out = append(*out, "---")
	case atom.Figcaption, atom.Summary:
		if text := tidyFlow(r.inline(n)); text != "" {
			*out = append(*out, r.escapeFence(text))
		}
	default:
		// Containers contribute their blocks; inline-only elements become a paragraph.
		if containerTags[n.DataAtom] {
			r.walkBlocks(n, out)
			return
		}
		if text := tidyFlow(r.inline(n)); text != "" {
			*out = append(*out, r.escapeFence(text))
		}
	}
}

// headingLevel returns 1 to 6 for an h1 to h6.
func headingLevel(a atom.Atom) int {
	for i, h := range headingAtoms {
		if a == h {
			return i + 1
		}
	}
	return 2
}

var headingAtoms = [...]atom.Atom{atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6}

// singleTextBlock reports whether n holds exactly one text block and nothing else, apart from inline chrome.
func singleTextBlock(n *html.Node) bool {
	count := 0
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode:
			if strings.TrimSpace(c.Data) != "" {
				return false
			}
		case c.Type == html.ElementNode:
			if blockTags[c.DataAtom] {
				if chromeTags[c.DataAtom] || junkTags[c.DataAtom] || isHidden(c) {
					continue
				}
				count++
				continue
			}
			// Inline content is only tolerated when it is empty of text (an icon, a hidden label).
			if strings.TrimSpace(plainText(c)) != "" {
				return false
			}
		}
	}
	return count == 1
}

// escapeFence stops prose from opening a code fence by mistake.
func (r *markdownRenderer) escapeFence(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if reFence.MatchString(line) {
			lines[i] = "\\" + line
		}
	}
	return strings.Join(lines, "\n")
}

// flow accumulates inline markdown for one paragraph.
type flow struct{ md strings.Builder }

func (f *flow) add(r *markdownRenderer, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		f.md.WriteString(escapeText(zeroWidth.Replace(n.Data)))
	case html.ElementNode:
		if r.skip(n) {
			return
		}
		f.md.WriteString(r.inlineNode(n))
	}
}

// take returns the paragraph collected so far and starts a new one.
func (f *flow) take(parts *[]string) {
	if text := tidyFlow(f.md.String()); text != "" {
		*parts = append(*parts, text)
	}
	f.md.Reset()
}

// inline renders the inline content of an element; a block inside it keeps its own place.
func (r *markdownRenderer) inline(n *html.Node) string {
	var parts []string
	var f flow
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && blockTags[c.DataAtom] {
			f.take(&parts)
			var inner []string
			r.block(c, &inner)
			if text := strings.Join(inner, "\n\n"); text != "" {
				parts = append(parts, text)
			}
			continue
		}
		f.add(r, c)
	}
	f.take(&parts)
	return strings.Join(parts, "\n\n")
}

// inlineNode renders one inline element: a link, emphasis, code, image or nothing.
func (r *markdownRenderer) inlineNode(n *html.Node) string {
	a := n.DataAtom
	switch a {
	case atom.Br:
		return "\n"
	case atom.Wbr:
		return ""
	case atom.Img:
		alt := tidyFlow(attrString(n, "alt"))
		src := r.absolute(attrString(n, "src"))
		if src == "" {
			return ""
		}
		r.addLink(src)
		return "![" + alt + "](" + escapeURL(src) + ")"
	case atom.Code, atom.Kbd, atom.Samp:
		text := strings.Join(strings.Fields(zeroWidth.Replace(rawText(n))), " ")
		if text == "" {
			return ""
		}
		return codeSpan(text)
	case atom.Strong, atom.B:
		if inner := tidyFlow(r.inline(n)); inner != "" {
			return "**" + inner + "**"
		}
		return ""
	case atom.Em, atom.I, atom.Cite, atom.Var:
		if inner := tidyFlow(r.inline(n)); inner != "" {
			return "*" + inner + "*"
		}
		return ""
	case atom.Del, atom.S, atom.Strike:
		if inner := tidyFlow(r.inline(n)); inner != "" {
			return "~~" + inner + "~~"
		}
		return ""
	case atom.A:
		inner := tidyFlow(r.inline(n))
		href := r.absolute(attrString(n, "href"))
		if href == "" || strings.HasPrefix(href, "#") {
			return inner
		}
		r.addLink(href)
		if inner == "" {
			inner = escapeText(href)
		}
		if inner == href {
			return "<" + href + ">"
		}
		return "[" + inner + "](" + escapeURL(href) + ")"
	}
	return r.inline(n)
}

// codeSpan writes inline code, choosing a fence the text does not contain.
func codeSpan(text string) string {
	runs := 1
	for _, run := range reInlineRun.FindAllString(text, -1) {
		if len(run) >= runs {
			runs = len(run) + 1
		}
	}
	fence := strings.Repeat("`", runs)
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		return fence + " " + text + " " + fence
	}
	return fence + text + fence
}

// renderList renders a ul or ol; nested lists are indented under their item.
func (r *markdownRenderer) renderList(list *html.Node, depth int) []string {
	var items []string
	number := 0
	if start := atoi(strings.TrimSpace(attrString(list, "start")), 1); start > 0 {
		number = start - 1
	}
	for li := list.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.DataAtom != atom.Li || r.skip(li) {
			continue
		}
		marker := "- "
		if list.DataAtom == atom.Ol {
			number++
			marker = itoa(number) + ". "
		}
		body := tidyFlow(r.listItem(li, depth))
		if body == "" {
			continue
		}
		lines := strings.Split(body, "\n")
		// A line that opens like an ordered item inside a bullet would be read as a new list; escape the dot.
		lines[0] = reOrderedMarker.ReplaceAllString(lines[0], "$1$2\\.$3")
		pad := strings.Repeat("  ", depth) + strings.Repeat(" ", len(marker))
		items = append(items, strings.Repeat("  ", depth)+marker+lines[0])
		for _, line := range lines[1:] {
			if strings.TrimSpace(line) == "" {
				items = append(items, "")
				continue
			}
			items = append(items, pad+line)
		}
	}
	return items
}

// listItem renders one li: its inline lead, then nested lists and blocks indented beneath it.
func (r *markdownRenderer) listItem(li *html.Node, depth int) string {
	var lead flow
	var tail []string
	for c := li.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.DataAtom == atom.Ul || c.DataAtom == atom.Ol) {
			if items := r.renderList(c, depth+1); len(items) > 0 {
				tail = append(tail, strings.Join(items, "\n"))
			}
			continue
		}
		if c.Type == html.ElementNode && blockTags[c.DataAtom] {
			lead.take(&tail)
			var inner []string
			r.block(c, &inner)
			if text := strings.Join(inner, "\n\n"); text != "" {
				tail = append(tail, text)
			}
			continue
		}
		lead.add(r, c)
	}
	lead.take(&tail)
	if len(tail) == 0 {
		return ""
	}
	return tidyFlow(strings.Join(tail, "\n\n"))
}

// table renders a table as a markdown grid. A leading row of th cells, or the first row alone, is the header.
func (r *markdownRenderer) table(table *html.Node) string {
	var header []string
	var rows [][]string
	walk(table, func(n *html.Node) bool {
		if n.Type != html.ElementNode || junkTags[n.DataAtom] {
			return true
		}
		if isHidden(n) {
			return false
		}
		if n != table && n.DataAtom == atom.Table {
			return false // a nested table is outside this grid
		}
		if n.DataAtom != atom.Tr {
			return true
		}
		var cells []string
		hasTh := false
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || (c.DataAtom != atom.Td && c.DataAtom != atom.Th) {
				continue
			}
			if c.DataAtom == atom.Th {
				hasTh = true
			}
			cell := tidyFlow(r.inline(c))
			if strings.TrimSpace(cell) == "" {
				cell = " "
			}
			cells = append(cells, strings.ReplaceAll(cell, "|", "\\|"))
		}
		if len(cells) == 0 {
			return true
		}
		if hasTh && header == nil {
			header = cells
			return true
		}
		rows = append(rows, cells)
		return false
	})
	if header == nil {
		if len(rows) == 0 {
			return ""
		}
		header, rows = rows[0], rows[1:]
	}
	width := len(header)
	for _, row := range rows {
		if len(row) > width {
			width = len(row)
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("|")
		for i := 0; i < width; i++ {
			cell := " "
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
	}
	writeRow(header)
	b.WriteString("|")
	for i := 0; i < width; i++ {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, cells := range rows {
		writeRow(cells)
	}
	return strings.TrimRight(b.String(), "\n")
}

// codeBlock writes a fenced block with a fence longer than any run inside the code.
func codeBlock(code, lang string) string {
	fence := "```"
	for _, run := range reCodeFenceRun.FindAllString(code, -1) {
		if len(run) >= len(fence) {
			fence = strings.Repeat("`", len(run)+1)
		}
	}
	return fence + lang + "\n" + strings.TrimRight(code, "\n") + "\n" + fence
}

// codeText returns the text of a code block with its whitespace intact.
func codeText(pre *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(c *html.Node) {
		switch c.Type {
		case html.TextNode:
			b.WriteString(zeroWidth.Replace(c.Data))
			return
		case html.ElementNode:
		default:
			return
		}
		if junkTags[c.DataAtom] || isHidden(c) {
			return
		}
		if c.DataAtom == atom.Br {
			b.WriteByte('\n')
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			rec(k)
		}
	}
	rec(pre)
	// <pre><code> often starts with the newline that follows the tag; browsers do not show it.
	return strings.TrimLeft(strings.ReplaceAll(b.String(), "\r\n", "\n"), "\n")
}

// codeLanguage reads the language a code block declares, from data-lang or a language- class.
func codeLanguage(pre *html.Node) string {
	if lang := strings.TrimSpace(attrString(pre, "data-lang")); isValidLang(lang) {
		return lang
	}
	nodes := []*html.Node{pre}
	if code := findFirst(pre, atom.Code); code != nil {
		nodes = append(nodes, code)
	}
	for _, n := range nodes {
		for _, a := range n.Attr {
			if a.Key != "class" && a.Key != "data-lang" && a.Key != "lang" {
				continue
			}
			for _, field := range strings.Fields(a.Val) {
				for _, prefix := range []string{"language-", "lang-"} {
					if candidate := strings.TrimPrefix(field, prefix); candidate != field && isValidLang(candidate) {
						return candidate
					}
				}
			}
		}
	}
	return ""
}

func isValidLang(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '+', c == '#', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// rawText returns the text of n with its whitespace intact.
func rawText(n *html.Node) string { return codeText(n) }

// tidyFlow collapses whitespace inside one flow of text and trims it. A blank line is kept as "\n".
func tidyFlow(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	var out []string
	blank := false
	for _, line := range lines {
		line = reSpacePunct.ReplaceAllString(reManySpaces.ReplaceAllString(line, " "), "$1")
		line = strings.TrimSpace(line)
		if line == "" {
			if blank || len(out) == 0 {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, line)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// escapeText protects the bracket character that would open a markdown link from prose.
func escapeText(s string) string {
	return strings.ReplaceAll(s, "[", "\\[")
}

// escapeURL writes a URL safely inside (...) or <>, encoding the characters that would end it.
func escapeURL(u string) string {
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E").Replace(u)
}

func itoa(i int) string { return strconv.Itoa(i) }

func atoi(s string, fallback int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}
