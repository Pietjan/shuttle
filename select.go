package shuttle

import (
	"fmt"
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
//	.rounded.border         classes, compounded
//	[data-ui=button]        an attribute, with or without a value
//	div[data-shuttle] span  descendants
//
// No combinators beyond descendant, no pseudo-classes, no attribute
// operators. A component test that needs more than this is usually
// asserting on the wrong thing.
//
// What it does not support it REFUSES, loudly. The alternative - parsing
// what it understands and quietly matching nothing - turns every Missing()
// and Count(x, 0) over a typo'd or unsupported selector into a pass, which
// is a test asserting that the engine has limits.

// selector is a chain of steps, each of which must match an ancestor of the
// next.
type selector []step

// step is one compound selector: at most one tag and id, any number of
// classes and attribute conditions.
type step struct {
	tag     string
	id      string
	classes []string
	attrs   []attrCond
}

type attrCond struct {
	key, val string
	// exact distinguishes [k=v] from [k], which only tests presence.
	exact bool
}

// parseSelector reads a selector, returning an error for anything it does
// not fully understand.
func parseSelector(s string) (selector, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("shuttle: empty selector")
	}
	var sel selector
	for _, part := range splitSteps(s) {
		st, err := parseStep(part)
		if err != nil {
			return nil, err
		}
		sel = append(sel, st)
	}
	return sel, nil
}

// splitSteps splits on whitespace outside brackets, so [title="hello
// world"] stays one step.
func splitSteps(s string) []string {
	var parts []string
	var b strings.Builder
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			b.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			b.WriteByte(c)
		case c == '[':
			depth++
			b.WriteByte(c)
		case c == ']':
			depth--
			b.WriteByte(c)
		case depth == 0 && (c == ' ' || c == '\t' || c == '\n'):
			if b.Len() > 0 {
				parts = append(parts, b.String())
				b.Reset()
			}
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

func parseStep(s string) (step, error) {
	var st step
	orig := s

	// Attributes first, so the tag/id/class scan sees a clean prefix.
	for {
		open := strings.IndexByte(s, '[')
		if open < 0 {
			break
		}
		end := closingBracket(s, open)
		if end < 0 {
			return st, fmt.Errorf("shuttle: selector %q: unterminated [", orig)
		}

		cond := s[open+1 : end]
		if before, after, ok := strings.Cut(cond, "="); ok {
			key := strings.TrimSpace(before)
			if len(key) > 0 && strings.ContainsAny(key[len(key)-1:], "^$*~|") {
				return st, fmt.Errorf("shuttle: selector %q: attribute operators are not supported, only [k] and [k=v]", orig)
			}
			st.attrs = append(st.attrs, attrCond{
				key:   key,
				val:   strings.Trim(strings.TrimSpace(after), `"'`),
				exact: true,
			})
		} else {
			st.attrs = append(st.attrs, attrCond{key: strings.TrimSpace(cond)})
		}
		s = s[:open] + s[end+1:]
	}

	if i := strings.IndexAny(s, ">+~,:*()"); i >= 0 {
		return st, fmt.Errorf("shuttle: selector %q: %q is not supported - tags, #id, .class, [attr] and descendants only", orig, s[i])
	}

	for s != "" {
		switch s[0] {
		case '#':
			var id string
			id, s = scanName(s[1:])
			if id == "" {
				return st, fmt.Errorf("shuttle: selector %q: # with no id", orig)
			}
			if st.id != "" {
				return st, fmt.Errorf("shuttle: selector %q: two ids in one step", orig)
			}
			st.id = id
		case '.':
			var class string
			class, s = scanName(s[1:])
			if class == "" {
				return st, fmt.Errorf("shuttle: selector %q: . with no class", orig)
			}
			st.classes = append(st.classes, class)
		default:
			var tag string
			tag, s = scanName(s)
			if st.tag != "" {
				return st, fmt.Errorf("shuttle: selector %q: two tags in one step", orig)
			}
			st.tag = tag
		}
	}

	if st.tag == "" && st.id == "" && len(st.classes) == 0 && len(st.attrs) == 0 {
		return st, fmt.Errorf("shuttle: selector %q: empty step", orig)
	}
	return st, nil
}

// closingBracket finds the ] ending the condition opened at open, skipping
// any that sit inside a quoted value.
func closingBracket(s string, open int) int {
	var quote byte
	for i := open + 1; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ']':
			return i
		}
	}
	return -1
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
	for _, class := range st.classes {
		if !hasClass(n, class) {
			return false
		}
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
//
// Every element is tested against the final step and its ancestor chain
// against the rest, rather than walking with a single advancing depth - the
// depth walk stopped searching inside anything that matched, so a .item
// nested in a .item was invisible and Count undercounted silently.
func selectAll(markup, sel string) ([]*html.Node, error) {
	steps, err := parseSelector(sel)
	if err != nil {
		return nil, err
	}

	root, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return nil, err
	}

	var found []*html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if matchesChain(n, steps) {
			found = append(found, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	return found, nil
}

// matchesChain reports whether n matches the last step with ancestors
// satisfying the rest, nearest-last, which is the descendant combinator.
func matchesChain(n *html.Node, steps selector) bool {
	if len(steps) == 0 || !steps[len(steps)-1].matches(n) {
		return false
	}
	i := len(steps) - 2
	for a := n.Parent; a != nil && i >= 0; a = a.Parent {
		if steps[i].matches(a) {
			i--
		}
	}
	return i < 0
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
		// The patched element itself an ignored container, replaced by an
		// ignored container: Datastar skips such a pair outright, so the
		// whole patch is a no-op. Modelled here before keepIgnored, whose
		// swap would otherwise tear the target out of the tree and then
		// try to insert relative to it.
		if _, ok := lookupAttr(target, "data-ignore-morph"); ok {
			for _, n := range nodes {
				if n.Type == html.ElementNode && attr(n, "id") == id {
					if _, ok := lookupAttr(n, "data-ignore-morph"); ok {
						return markup, true
					}
				}
			}
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
