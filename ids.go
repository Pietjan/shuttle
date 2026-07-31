package shuttle

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Element ids and signal names are both derived from a component's position
// in the tree, rendered two ways: hyphens for the DOM, dots for Datastar's
// signal paths.
//
//	root component      id="shuttle-c"        signals under c.
//	its second child    id="shuttle-c-2"      signals under c.2.
//	a named element     id="shuttle-c-2-email"
//
// Deriving them from position rather than a counter is what makes a
// fragment re-render produce the same ids as the page did. Loom numbers its
// own ids from a per-render counter, so a component rendered alone would
// otherwise get different ids than the same component rendered inside a
// page - and a morph that cannot match ids throws away focus and scroll.
//
// There is a second signal root, and the reason is worth stating: `c.` is
// the contract between client state and DecodeSignals, so anything under it
// can decode into a component's fields. An indicator is ephemera the server
// must never read - and a component with a field tagged `saving` would
// otherwise silently decode one. Keeping it out also keeps it off the wire,
// since filterSignals includes /^c\./ and nothing else.
const (
	idPrefix      = "shuttle-"
	signalRoot    = "c"
	indicatorRoot = "ind"
)

// path is a component's position in the tree: nil for the root, then one
// index per level down.
type path []int

// nodeID is the path as a compact identifier: "c", "c-1", "c-1-2". It names
// a component instance in an action URL.
func (p path) nodeID() string {
	var b strings.Builder
	b.WriteString(signalRoot)
	for _, i := range p {
		b.WriteByte('-')
		b.WriteString(strconv.Itoa(i))
	}
	return b.String()
}

// elementID is the path as an element id.
func (p path) elementID() string { return idPrefix + p.nodeID() }

// namespace is the path as a Datastar signal namespace.
func (p path) namespace() string {
	var b strings.Builder
	b.WriteString(signalRoot)
	for _, i := range p {
		b.WriteByte('.')
		b.WriteString(strconv.Itoa(i))
	}
	return b.String()
}

// child returns the path of the nth child. Callers must copy rather than
// append in place, since a parent's path outlives the call.
func (p path) child(n int) path {
	c := make(path, len(p)+1)
	copy(c, p)
	c[len(p)] = n
	return c
}

// idFor is the id of a named element inside this component.
func (sc *scope) idFor(name string) string {
	return sc.path().elementID() + "-" + name
}

// declare records an indicator so the component root can declare it.
func (sc *scope) declare(name string) {
	if sc.indicators == nil {
		sc.indicators = map[string]bool{}
	}
	sc.indicators[name] = true
}

// indicatorDecls are the data-signals attributes declaring this render's
// indicators, for the component's root element.
//
// Datastar's own plugin creates the signal when it initialises
// data-indicator - but attributes are processed in document order, so any
// expression on the same element that reads the signal *earlier* finds
// nothing there, and taking that error kills the rest of that element's
// attributes with it. A button whose data-attr:disabled came before its
// data-on:click lost the click entirely, silently, and only in a browser.
//
// Declaring them on the root, which is written before any of them, means the
// order options are passed in stops mattering. __ifmissing so a re-render
// never resets one that is currently true.
func (sc *scope) indicatorDecls() string {
	if len(sc.indicators) == 0 {
		return ""
	}
	names := make([]string, 0, len(sc.indicators))
	for name := range sc.indicators {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, ` data-signals:%s__ifmissing="false"`, sc.indicatorFor(name))
	}
	return b.String()
}

// indicatorFor is the signal path of a named indicator inside this
// component: "ind.c.1.saving". The component's path is in it, so two
// instances of the same component do not share one.
func (sc *scope) indicatorFor(name string) string {
	return indicatorRoot + "." + sc.path().namespace() + "." + name
}

// ID gives a Loom component an explicit, deterministic id, scoped to the
// component instance rendering it.
//
//	input.New(shuttle.ID(ctx, input.ID, "email"))
//
// Worth using on anything the morph has to keep track of - a focused
// input, a scrolled list, an element referenced by aria-describedby.
// Without it the element falls back to Loom's per-render counter, which is
// deterministic for one component but collides once two components on a
// page each render a field.
func ID[O ~func(T), T any](ctx context.Context, id IDs[O], name string) O {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return func(T) {}
	}
	return id(sc.idFor(name))
}

// ElementID returns the id ID would assign, for markup a component writes
// itself - a `for` attribute, an aria reference, a morph target of its own.
// It returns "" outside a render pass.
func ElementID(ctx context.Context, name string) string {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return ""
	}
	return sc.idFor(name)
}

var idAttrRE = regexp.MustCompile(`\sid="([^"]*)"`)

// DuplicateIDs reports ids appearing more than once in markup, sorted.
//
// Worth checking in a test over every component. A duplicate id is the
// quietest failure in this whole design: Datastar's morph excludes it from
// the persistent-id set and silently drops that subtree to soft matching,
// so the symptom is not an error but focus and scroll going missing under
// load.
func DuplicateIDs(markup string) []string {
	seen := map[string]int{}
	for _, m := range idAttrRE.FindAllStringSubmatch(markup, -1) {
		seen[m[1]]++
	}

	var dupes []string
	for id, n := range seen {
		if n > 1 {
			dupes = append(dupes, id)
		}
	}
	sort.Strings(dupes)
	return dupes
}
