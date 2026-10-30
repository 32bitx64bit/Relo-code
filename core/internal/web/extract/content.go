package extract

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// nodeInfo caches what extraction needs to know about one element.
type nodeInfo struct {
	text   int  // characters of visible text inside, hidden parts excluded
	links  int  // characters inside anchors, hidden parts excluded
	hidden bool // the node or an ancestor is not rendered
}

var (
	reUnlikely = regexp.MustCompile(`(?i)-ad-|ai2html|banner|breadcrumb|combx|comment|community|cover-wrap|disqus|extra|gdpr|legends|menu|related|remark|replies|rss|shoutbox|sidebar|skyscraper|social|sponsor|supplemental|ad-break|agegate|pagination|pager|popup|nav|masthead|cookie|consent|newsletter|subscribe|promo|share|toolbar|modal|footer|header|byline|author`)
	rePossible = regexp.MustCompile(`(?i)article|body|content|entry|hentry|h-entry|main|page|post|text|blog|story|markdown|prose`)
	reNegative = regexp.MustCompile(`(?i)hidden|banner|combx|comment|com-|contact|footer|gdpr|masthead|media|meta|outbrain|promo|related|scroll|share|shoutbox|sidebar|skyscraper|sponsor|shopping|tags|widget|nav|menu|advert|ads?[-_]|social|cookie|consent|paywall|byline|author`)
	// reHiddenStyle matches inline styles that take an element out of rendering.
	reHiddenStyle = regexp.MustCompile(`(?i)(?:^|;)\s*display\s*:\s*none|(?:^|;)\s*visibility\s*:\s*hidden|(?:^|;)\s*opacity\s*:\s*0(?:\.0+)?\s*(?:;|$)`)
)

// zeroWidth removes characters that carry no visible text, including the ones used to hide strings inside words.
var zeroWidth = strings.NewReplacer(
	"\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "",
	"\u00ad", "", "\u180e", "",
)

// junkTags are dropped with all of their content.
var junkTags = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Iframe: true, atom.Object: true,
	atom.Embed: true, atom.Svg: true, atom.Canvas: true, atom.Template: true, atom.Form: true,
	atom.Input: true, atom.Button: true, atom.Select: true, atom.Textarea: true, atom.Link: true,
	atom.Meta: true, atom.Head: true,
}

// findTitle returns the document title text.
func findTitle(root *html.Node) string {
	var title []string
	walk(root, func(n *html.Node) bool {
		switch n.Type {
		case html.ElementNode:
			if n.DataAtom == atom.Body {
				return false
			}
		case html.TextNode:
			if p := n.Parent; p != nil && p.DataAtom == atom.Title {
				title = append(title, n.Data)
			}
		}
		return true
	})
	return strings.Join(title, " ")
}

// findContent returns the root of the main content.
func findContent(root *html.Node) *html.Node {
	body := findFirst(root, atom.Body)
	if body == nil {
		body = root
	}
	infos := make(map[*html.Node]*nodeInfo)
	measure(body, infos)
	if infos[body].text == 0 {
		return body
	}
	if best := bestSignal(body, infos); best != nil {
		return best
	}
	if best := bestScored(body, infos); best != nil {
		return best
	}
	return body
}

// bestSignal picks the node that says it is the main content and holds a large share of the page text.
func bestSignal(body *html.Node, infos map[*html.Node]*nodeInfo) *html.Node {
	total := infos[body].text
	var best *html.Node
	bestText := 0
	walk(body, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return true
		}
		info := infos[n]
		if info == nil || info.hidden {
			return false
		}
		if info.text > bestText && isSignalNode(n) && n.DataAtom != atom.Li {
			best, bestText = n, info.text
		}
		return true
	})
	if best == nil {
		return nil
	}
	// One article carrying two thirds of the page is the page. A smaller share may be a single post among many
	// (a forum thread): a thread container is named as one, scoring decides and keeps every post.
	if bestText*3 >= total*2 && !containsPost(best) {
		return best
	}
	return nil
}

// containsPost reports whether n, a candidate content root, holds posts of a discussion rather than an article.
func containsPost(n *html.Node) bool {
	return hasClass(n, "thread|forum|comments|replies")
}

// hasClass reports whether a class or id contains one of the alternate names, separated by a bar.
func hasClass(n *html.Node, alternates string) bool {
	pattern := regexp.MustCompile(`(?i)(?:^|[\s_-])(?:` + alternates + `)(?:$|[\s_-]|\d)`)
	classish := attrString(n, "class") + " " + attrString(n, "id")
	return pattern.MatchString(classish)
}

// isSignalNode reports whether a node declares itself main content.
func isSignalNode(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Main, atom.Article:
		return true
	}
	if strings.EqualFold(attrString(n, "role"), "main") {
		return true
	}
	id, class := attrString(n, "id"), attrString(n, "class")
	return rePossible.MatchString(id) || rePossible.MatchString(class)
}

// bestScored is readability's candidate scoring: a text block scores its parent and half its grandparent by its
// text length, commas and weight. The parent of many blocks wins.
func bestScored(body *html.Node, infos map[*html.Node]*nodeInfo) *html.Node {
	scores := make(map[*html.Node]float64)
	contribs := make(map[*html.Node]int)
	var seen []*html.Node
	add := func(n *html.Node, s float64) {
		if _, ok := scores[n]; !ok {
			seen = append(seen, n)
		}
		scores[n] += s
		contribs[n]++
	}
	walk(body, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return true
		}
		if node := infos[n]; node != nil && node.hidden {
			return false
		}
		var text string
		switch n.DataAtom {
		case atom.P, atom.Td, atom.Pre, atom.Blockquote, atom.Dd, atom.Dt, atom.Figcaption:
			text = textOf(n)
		case atom.Li:
			// A list item's own prose, without the lists nested inside it.
			text = directText(n)
		case atom.Div, atom.Section, atom.Article, atom.Main:
			// Div-based pages put paragraphs in plain divs; score the leaf ones only.
			if hasBlockChild(n) {
				return true
			}
			text = textOf(n)
		default:
			return true
		}
		if len([]rune(text)) < 25 {
			return true
		}
		s := 1 + float64(strings.Count(text, ",")) + float64(minInt(len([]rune(text))/100, 3))
		if n.DataAtom == atom.Pre {
			s++
		}
		parent := n.Parent
		if parent == nil || parent == body {
			return false
		}
		if grand := parent.Parent; grand != nil && grand != body {
			add(parent, s)
			add(grand, s/2)
			// A div-based paragraph is a candidate next to its own wrapper.
			switch n.DataAtom {
			case atom.Div, atom.Section, atom.Article, atom.Main:
				add(n, s)
			}
		}
		return true
	})
	var top *html.Node
	topScore := 0.0
	for _, n := range seen {
		s := scores[n] * (1 - linkDensity(n, infos))
		classish := strings.ToLower(n.Data + " " + attrString(n, "class") + " " + attrString(n, "id"))
		if reNegative.MatchString(classish) {
			s -= 25
		}
		if rePossible.MatchString(classish) {
			s += 25
		}
		if reUnlikely.MatchString(classish) {
			s -= 10
		}
		scores[n] = s
		if s > topScore {
			top, topScore = n, s
		}
	}
	if top == nil || top.Parent == nil {
		return top
	}
	parent := top.Parent
	if parent == body || scores[parent] <= 0 {
		return top
	}
	node := top
	if contribs[parent] >= 2 && scores[parent] > topScore*0.01 {
		node = parent
	}
	// A post or comment body is a candidate of its own, but its wrapper names the whole item: the author's name
	// and links belong to the post. Step out while the wrapper is not the thread itself.
	if n := node.Parent; !containsPost(node) && n != nil && n != body && !containsPost(n) && scores[n] > 0 &&
		!reUnlikely.MatchString(strings.ToLower(attrString(n, "class"))) && linkDensity(n, infos) < 0.25 &&
		infos[n].text >= infos[node].text && attrString(node, "class") != "" {
		node = n
	}
	// A thread holds posts from many people: its posts follow one another, so when the page is a discussion the
	// thread itself stays the root and every post keeps its own paragraphs.
	for n := node.Parent; n != nil && n != body; n = n.Parent {
		if containsPost(n) {
			return n
		}
	}
	return node
}

func tagDivisor(a atom.Atom) float64 {
	switch a {
	case atom.Div, atom.Article, atom.Section, atom.Main:
		return 5
	case atom.Blockquote:
		return 3
	case atom.Td, atom.Li, atom.Pre:
		return 2
	}
	return 1
}

func linkDensity(n *html.Node, infos map[*html.Node]*nodeInfo) float64 {
	info := infos[n]
	if info == nil || info.text == 0 {
		return 0
	}
	return float64(info.links) / float64(info.text)
}

// measure fills in text, link and hidden information for n and its descendants. Parents are measured first.
func measure(n *html.Node, infos map[*html.Node]*nodeInfo) *nodeInfo {
	info := &nodeInfo{}
	if n.Type == html.ElementNode {
		info.hidden = isHidden(n)
	}
	if p := n.Parent; p != nil {
		if pi := infos[p]; pi != nil {
			info.hidden = info.hidden || pi.hidden
		}
	}
	infos[n] = info
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			if info.hidden || (c.Parent != nil && c.Parent.DataAtom == atom.Title) {
				continue
			}
			size := len([]rune(zeroWidth.Replace(c.Data)))
			info.text += size
			if c.Parent != nil && c.Parent.DataAtom == atom.A {
				info.links += size
			}
		case html.ElementNode:
			if junkTags[c.DataAtom] {
				continue
			}
			child := measure(c, infos)
			info.text += child.text
			info.links += child.links
		}
	}
	return info
}

// isHidden reports whether n is taken out of rendering by its own attributes: hidden, aria-hidden true, a
// closed dialog, or an inline display:none, visibility:hidden or opacity:0.
func isHidden(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if n.DataAtom == atom.Input && strings.EqualFold(attrString(n, "type"), "hidden") {
		return true
	}
	if _, ok := attr(n, "hidden"); ok {
		return true
	}
	if strings.EqualFold(attrString(n, "aria-hidden"), "true") {
		return true
	}
	if n.DataAtom == atom.Dialog {
		if _, open := attr(n, "open"); !open {
			return true
		}
	}
	return reHiddenStyle.MatchString(attrString(n, "style"))
}

// findByline returns the article author: an element marked as a byline or author near the content.
func findByline(root, content *html.Node) string {
	for n := content; n != nil; n = n.Parent {
		if b := bylineUnder(n, content, 0); b != "" {
			return b
		}
	}
	return bylineUnder(root, content, 0)
}

func bylineUnder(root, content *html.Node, depth int) string {
	if root == nil || depth > 5 {
		return ""
	}
	var found string
	walk(root, func(n *html.Node) bool {
		if found != "" {
			return false
		}
		if n.Type != html.ElementNode {
			return true
		}
		classish := strings.ToLower(attrString(n, "class") + " " + attrString(n, "id") + " " + attrString(n, "itemprop") + " " + attrString(n, "rel"))
		if strings.Contains(classish, "byline") || strings.Contains(classish, "author") || strings.Contains(classish, "writer") {
			if t := textOf(n); t != "" && len([]rune(t)) <= 120 {
				found = t
				return false
			}
		}
		return true
	})
	if found != "" {
		return found
	}
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if c == content {
			continue
		}
		if b := bylineUnder(c, content, depth+1); b != "" {
			return b
		}
	}
	return ""
}

// walk visits n and its descendants in document order; a false result skips the node's children.
func walk(n *html.Node, fn func(*html.Node) bool) {
	if !fn(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

func findFirst(root *html.Node, a atom.Atom) *html.Node {
	var found *html.Node
	walk(root, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if n.Type == html.ElementNode && n.DataAtom == a {
			found = n
			return false
		}
		return true
	})
	return found
}

// hasBlockChild reports whether n contains a block element, so its text lives in children.
func hasBlockChild(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && blockTags[c.DataAtom] && !junkTags[c.DataAtom] && !isHidden(c) {
			return true
		}
	}
	return false
}

// directText returns the text of n's inline content, without nested blocks.
func directText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && blockTags[c.DataAtom] && !junkTags[c.DataAtom] {
			continue
		}
		b.WriteString(plainText(c))
		b.WriteByte(' ')
	}
	return tidyFlow(b.String())
}

// textOf returns the visible text of n collapsed to one line.
func textOf(n *html.Node) string {
	return tidyFlow(plainText(n))
}

// plainText returns the visible text of n, with blocks separated by spaces.
func plainText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(c *html.Node) {
		switch c.Type {
		case html.TextNode:
			b.WriteString(c.Data)
			return
		case html.ElementNode:
		default:
			return
		}
		if junkTags[c.DataAtom] || isHidden(c) {
			return
		}
		if c.DataAtom == atom.Br {
			b.WriteByte(' ')
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			rec(k)
		}
		if blockTags[c.DataAtom] {
			b.WriteByte(' ')
		}
	}
	rec(n)
	return zeroWidth.Replace(b.String())
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key && a.Namespace == "" {
			return a.Val, true
		}
	}
	return "", false
}

func attrString(n *html.Node, key string) string {
	v, _ := attr(n, key)
	return v
}

// firstHeading returns the first h1 (or h2) text of n.
func firstHeading(n *html.Node) string {
	var found string
	walk(n, func(c *html.Node) bool {
		if found != "" {
			return false
		}
		if c.Type == html.ElementNode && (c.DataAtom == atom.H1 || c.DataAtom == atom.H2) {
			found = textOf(c)
			return false
		}
		return true
	})
	return found
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
