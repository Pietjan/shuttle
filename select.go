package shuttle

import (
	"slices"
	"strings"

	"golang.org/x/net/html"

	"github.com/starfederation/datastar-go/datastar"
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

// applyPatch applies one patch to the kit's copy of the page, the way the
// browser would.
//
// The testing kit needs it because a scoped re-render is the normal case: an
// action on a child patches only that child, so tracking the root's markup
// alone would leave every assertion looking at the render before the one
// that just happened.
//
// The mode is honoured rather than assumed, and that is not thoroughness for
// its own sake. A stream's append names the *container* as its target and
// carries one item as its markup, so treating every patch as an outer
// replace does not merely miss the item - it swaps the whole container for
// it. A component that streams anything would then have its markup quietly
// dismantled part way through a test, and every assertion after that point
// would be about a document the browser never had.
func applyPatch(markup, id, replacement string, mode datastar.ElementPatchMode) (string, bool) {
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return markup, false
	}

	target := findByID(doc, id)
	if target == nil || target.Parent == nil {
		return markup, false
	}

	switch mode {
	case datastar.ElementPatchModeRemove:
		target.Parent.RemoveChild(target)

	case datastar.ElementPatchModeInner:
		nodes, ok := parseIn(target, replacement)
		if !ok {
			return markup, false
		}
		for c := target.FirstChild; c != nil; c = target.FirstChild {
			target.RemoveChild(c)
		}
		place(target, nodes, nil)

	case datastar.ElementPatchModeAppend:
		nodes, ok := parseIn(target, replacement)
		if !ok {
			return markup, false
		}
		place(target, nodes, nil)

	case datastar.ElementPatchModePrepend:
		nodes, ok := parseIn(target, replacement)
		if !ok {
			return markup, false
		}
		place(target, nodes, target.FirstChild)

	default: // outer, which is what a re-render is
		parent := target.Parent
		nodes, ok := parseIn(parent, replacement)
		if !ok {
			return markup, false
		}
		nodes = keepIgnored(doc, nodes)
		place(parent, nodes, target)
		parent.RemoveChild(target)
	}

	body := findBody(doc)
	if body == nil {
		return markup, false
	}
	return innerHTML(body)
}

// parseIn parses fragment in the context of parent.
//
// Parsing in context is what makes a streamed <li> land as an <li>: parsed
// against a <ul> it is one, parsed against nothing the tokeniser drops the
// tag and keeps only the text.
func parseIn(parent *html.Node, fragment string) ([]*html.Node, bool) {
	nodes, err := html.ParseFragment(strings.NewReader(fragment), parent)
	if err != nil {
		return nil, false
	}
	return nodes, true
}

// place puts nodes before before, or at the end when before is nil.
func place(parent *html.Node, nodes []*html.Node, before *html.Node) {
	for _, n := range nodes {
		if before != nil {
			parent.InsertBefore(n, before)
			continue
		}
		parent.AppendChild(n)
	}
}

// keepIgnored carries data-ignore-morph containers over from the live
// document into an incoming render, subtree and all.
//
// Without it the kit models a morph that does not exist. Datastar skips a
// pair of elements outright when both carry the attribute - read from the
// v1.0.2 bundle, where the morph returns immediately if the old and the new
// element both have it, or if the old one sits inside something that does -
// so a component's re-render cannot disturb what was streamed into its
// container.
//
// A kit that overwrote the container instead would lose every streamed item
// on the component's next render. Not visibly, either: the test would go on
// passing until it asserted on something streamed a while ago, and then
// report that the browser had lost it, which is the opposite of what
// happens.
func keepIgnored(doc *html.Node, incoming []*html.Node) []*html.Node {
	const ignore = "data-ignore-morph"

	// Collected first, then swapped: the swap detaches nodes, and moving
	// the tree around while walking it reads worse than it works.
	type swap struct{ from, to *html.Node }
	var swaps []swap

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if id := attr(n, "id"); id != "" {
				if _, ok := lookupAttr(n, ignore); ok {
					if live := findByID(doc, id); live != nil {
						if _, ok := lookupAttr(live, ignore); ok {
							swaps = append(swaps, swap{from: n, to: live})
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range incoming {
		walk(n)
	}

	for _, s := range swaps {
		if s.to.Parent != nil {
			s.to.Parent.RemoveChild(s.to)
		}
		if s.from.Parent != nil {
			s.from.Parent.InsertBefore(s.to, s.from)
			s.from.Parent.RemoveChild(s.from)
			continue
		}
		// A top-level node of the fragment: it has no parent to swap it
		// out of, so the caller's slice is what has to change.
		for i, n := range incoming {
			if n == s.from {
				incoming[i] = s.to
			}
		}
	}
	return incoming
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
