package shuttle

import (
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// A small selector engine, enough to write assertions against a component's
// markup without pulling in a browser or a full CSS implementation:
//
//	button                  a tag
//	#save                   an id
//	.rounded                a class
//	[data-ui=button]        an attribute, with or without a value
//	div[data-shuttle] span  descendants
//
// No combinators beyond descendant, no pseudo-classes. A component test
// that needs more than this is usually asserting on the wrong thing.

// selector is a chain of steps, each of which must match an ancestor of the
// next.
type selector []step

// step is one compound selector: at most one tag, id and class, and any
// number of attribute conditions.
type step struct {
	tag   string
	id    string
	class string
	attrs []attrCond
}

type attrCond struct {
	key, val string
	// exact distinguishes [k=v] from [k], which only tests presence.
	exact bool
}

// parseSelector reads a selector, returning nil if it is empty.
func parseSelector(s string) selector {
	var sel selector
	for part := range strings.FieldsSeq(s) {
		if st, ok := parseStep(part); ok {
			sel = append(sel, st)
		}
	}
	return sel
}

func parseStep(s string) (step, bool) {
	var st step

	// Attributes first, so the tag/id/class scan sees a clean prefix.
	for {
		open := strings.IndexByte(s, '[')
		if open < 0 {
			break
		}
		close := strings.IndexByte(s[open:], ']')
		if close < 0 {
			break
		}
		close += open

		cond := s[open+1 : close]
		if before, after, ok := strings.Cut(cond, "="); ok {
			st.attrs = append(st.attrs, attrCond{
				key:   strings.TrimSpace(before),
				val:   strings.Trim(strings.TrimSpace(after), `"'`),
				exact: true,
			})
		} else {
			st.attrs = append(st.attrs, attrCond{key: strings.TrimSpace(cond)})
		}
		s = s[:open] + s[close+1:]
	}

	for s != "" {
		switch s[0] {
		case '#':
			st.id, s = scanName(s[1:])
		case '.':
			st.class, s = scanName(s[1:])
		default:
			st.tag, s = scanName(s)
		}
	}

	return st, st.tag != "" || st.id != "" || st.class != "" || len(st.attrs) > 0
}

// scanName reads up to the next '#' or '.'.
func scanName(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' || s[i] == '.' {
			return s[:i], s[i:]
		}
	}
	return s, ""
}

// matches reports whether n satisfies one step.
func (st step) matches(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if st.tag != "" && !strings.EqualFold(n.Data, st.tag) {
		return false
	}
	if st.id != "" && attr(n, "id") != st.id {
		return false
	}
	if st.class != "" && !hasClass(n, st.class) {
		return false
	}
	for _, c := range st.attrs {
		v, ok := lookupAttr(n, c.key)
		if !ok || (c.exact && v != c.val) {
			return false
		}
	}
	return true
}

func lookupAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func attr(n *html.Node, key string) string {
	v, _ := lookupAttr(n, key)
	return v
}

func hasClass(n *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), class)
}

// selectAll returns every element in markup matching sel, in document
// order.
func selectAll(markup, sel string) ([]*html.Node, error) {
	steps := parseSelector(sel)
	if len(steps) == 0 {
		return nil, nil
	}

	root, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return nil, err
	}

	var found []*html.Node
	var walk func(n *html.Node, depth int)
	walk = func(n *html.Node, depth int) {
		next := depth
		if depth < len(steps) && steps[depth].matches(n) {
			next = depth + 1
			if next == len(steps) {
				found = append(found, n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, next)
		}
	}
	walk(root, 0)

	return found, nil
}

// applyPatch replaces the element carrying id with replacement, which is
// what the browser's morph does when a patch arrives.
//
// The testing kit needs it because a scoped re-render is the normal case: an
// action on a child patches only that child, so tracking the root's markup
// alone would leave every assertion looking at the render before the one
// that just happened.
func applyPatch(markup, id, replacement string) (string, bool) {
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return markup, false
	}

	target := findByID(doc, id)
	if target == nil || target.Parent == nil {
		return markup, false
	}
	parent := target.Parent

	nodes, err := html.ParseFragment(strings.NewReader(replacement), parent)
	if err != nil {
		return markup, false
	}
	for _, n := range nodes {
		parent.InsertBefore(n, target)
	}
	parent.RemoveChild(target)

	body := findBody(doc)
	if body == nil {
		return markup, false
	}
	return innerHTML(body)
}

func findByID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode && attr(n, "id") == id {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "body" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findBody(c); found != nil {
			return found
		}
	}
	return nil
}

// innerHTML serialises an element's children.
func innerHTML(n *html.Node) (string, bool) {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return "", false
		}
	}
	return b.String(), true
}

// textOf returns an element's text, with runs of whitespace collapsed - the
// form an assertion wants to compare against.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
