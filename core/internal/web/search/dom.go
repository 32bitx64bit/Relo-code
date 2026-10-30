package search

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// attr returns the value of n's first attribute whose key equals key.
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// hasAttr reports whether n has key among its attributes.
func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

// attrContains reports whether one of n's key attributes holds word as a whole word, split on spaces,
// hyphens and underscores.
func attrContains(n *html.Node, key, word string) bool {
	for _, a := range n.Attr {
		if !strings.EqualFold(a.Key, key) {
			continue
		}
		if containsWord(a.Val, word) {
			return true
		}
	}
	return false
}

// classWord reports whether n's class attribute holds word as a whole word.
func classWord(n *html.Node, word string) bool {
	return attrContains(n, "class", word)
}

// containsWord reports whether value holds word delimited by non-name characters.
func containsWord(value, word string) bool {
	if value == "" || word == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(value, isWordBreak) {
		if strings.EqualFold(field, word) {
			return true
		}
	}
	return false
}

// isWordBreak reports whether r breaks a class name or id into words.
func isWordBreak(r rune) bool {
	return unicode.IsSpace(r) || r == '-' || r == '_' || r == '.'
}

// firstByClass returns the first descendant of n, in document order, that holds class and passes keep.
func firstByClass(n *html.Node, class string, keep ...func(*html.Node) bool) *html.Node {
	return firstMatch(n, func(c *html.Node) bool {
		if !classWord(c, class) {
			return false
		}
		for _, k := range keep {
			if !k(c) {
				return false
			}
		}
		return true
	})
}

// firstAnchorWithClass returns the first anchor descendant of n holding class.
func firstAnchorWithClass(n *html.Node, class string) *html.Node {
	return firstByClass(n, class, func(c *html.Node) bool { return c.DataAtom == atom.A })
}

// firstByTag returns the first descendant of n with tag that passes keep.
func firstByTag(n *html.Node, tag atom.Atom, keep func(*html.Node) bool) *html.Node {
	return firstMatch(n, func(c *html.Node) bool { return c.DataAtom == tag && keep(c) })
}

// firstMatch returns the first element at or under n matching ok, in document order.
func firstMatch(n *html.Node, ok func(*html.Node) bool) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && ok(c) {
			return c
		}
		if found := firstMatch(c, ok); found != nil {
			return found
		}
	}
	return nil
}

// text returns the text of n and its descendants, with a space between block elements.
func text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		switch c.Type {
		case html.TextNode:
			b.WriteString(c.Data)
		case html.ElementNode:
			if c.DataAtom == atom.Script || c.DataAtom == atom.Style || c.DataAtom == atom.Noscript {
				return
			}
			if isBlockTag(c.DataAtom) {
				b.WriteByte(' ')
			}
		}
		for child := c.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if c.Type == html.ElementNode && isBlockTag(c.DataAtom) {
			b.WriteByte(' ')
		}
	}
	walk(n)
	return b.String()
}

// isBlockTag reports whether tag renders as a block, so its text is separated from its neighbours.
func isBlockTag(tag atom.Atom) bool {
	switch tag {
	case atom.P, atom.Div, atom.Li, atom.Ul, atom.Ol, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Br, atom.Blockquote, atom.Table, atom.Tr, atom.Td, atom.Th, atom.Section, atom.Article, atom.Footer,
		atom.Header, atom.Main, atom.Aside, atom.Nav:
		return true
	}
	return false
}

// collapseSpace trims and folds runs of whitespace to one space.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
