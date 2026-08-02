package shuttle

import (
	"context"
	"fmt"
	"strconv"
)

// scope is the action table of a render pass in progress. Only the
// goroutine doing the render touches it, so it needs no lock of its own;
// the finished table is handed to the session under the session's.
type scope struct {
	// indicators are the pending-state signals this render asked for, so the
	// root can declare them before anything reads one.
	indicators map[string]bool

	node  *node
	gen   uint64
	n     int
	table map[string]Action
	// seen collects the keys of the children this pass rendered, so the
	// ones it left out can be unmounted afterwards.
	seen map[string]bool
}

// path is the component's position, which every id and signal name derives
// from.
func (sc *scope) path() path { return sc.node.path }

// saw records that a child key was rendered in this pass.
func (sc *scope) saw(key string) { sc.seen[key] = true }

type scopeKey struct{}

func withScope(ctx context.Context, sc *scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, sc)
}

func scopeFrom(ctx context.Context) (*scope, bool) {
	sc, ok := ctx.Value(scopeKey{}).(*scope)
	return sc, ok
}

// register files fn under an id naming its render generation and its
// position in that render: "3-a2".
//
// Deterministic rather than random, because the capability here is the
// session id, which is unguessable. Every id in a live table belongs to
// markup this client was just served, so working through them reaches
// nothing the client was not shown - and determinism is what lets a test
// assert that two renders of the same state produce the same bytes.
//
// The generation prefix is what makes positional numbering safe. Position 1
// of the next render is not necessarily the same button (think a Save that
// replaces an Edit), so an id that named only its position would let a
// click sent against stale markup run the wrong closure.
func (sc *scope) register(fn Action) string {
	sc.n++
	id := strconv.FormatUint(sc.gen, 10) + "-a" + strconv.Itoa(sc.n)
	sc.table[id] = fn
	return id
}

// actionExpr is the Datastar expression a bound event fires. It names the
// component as well as the action, because tables are per component now -
// and it is scoped so the request uploads this component's signals and
// nothing else.
// The session travels in a header rather than the path - see
// [SessionHeader] - so the URL names only what is being acted on.
func (sc *scope) actionExpr(actionID string) string {
	sess := sc.node.sess
	return fmt.Sprintf(`@post('%s/act/%s/%s', {headers: %s, %s})`,
		sess.prefix+routePrefix, sc.path().nodeID(), actionID,
		sessionHeaderExpr(sess), filterFor(sc.path().namespace()))
}

// OnEvent binds a DOM event to a server-side closure, returning one option
// of the component package's own type.
//
// The ctx is the one handed to [Component.Render]: it carries the render
// pass's action table, which is what fn is registered in. Calling this
// with any other context - a bare context.Background, a component rendered
// outside a session - binds nothing, because an attribute pointing at an
// unregistered action is worse than no attribute at all.
func OnEvent[O ~func(T), T any](ctx context.Context, attr Attrs[O], event string, fn Action, extras ...Extra) O {
	return onAttr(ctx, attr, "data-on:"+event, fn, extras)
}

// onAttr is the shape every closure binding shares: register fn in this
// render's table, and put the expression that calls it on one attribute.
//
// The attribute is a parameter because not every trigger is the "on" plugin
// with a key. [OnIntersect] is its own plugin with its own attribute name.
func onAttr[O ~func(T), T any](ctx context.Context, attr Attrs[O], name string, fn Action, extras []Extra) O {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return func(T) {}
	}
	return bind(attr, sc.pairs(pair{name, sc.actionExpr(sc.register(fn))}, extras)...)
}

// pairs resolves a binding's extras against this scope, in the order they
// were given.
func (sc *scope) pairs(handler pair, extras []Extra) []pair {
	out := make([]pair, 0, len(extras)+1)
	out = append(out, handler)
	for _, extra := range extras {
		out = append(out, extra(sc))
	}
	return out
}

// OnClick binds a click to a server-side closure. The common case:
//
//	button.New(
//	    button.Primary,
//	    shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
//	        c.Count++
//	        return nil
//	    }),
//	)
func OnClick[O ~func(T), T any](ctx context.Context, attr Attrs[O], fn Action, extras ...Extra) O {
	return OnEvent(ctx, attr, "click", fn, extras...)
}

// OnSubmit binds a form submission to a server-side closure, for the
// commit half of a form.
//
// The binding carries __prevent, without which the browser also submits the
// form the ordinary way and navigates the page out from under the session.
func OnSubmit[O ~func(T), T any](ctx context.Context, attr Attrs[O], fn Action, extras ...Extra) O {
	return OnEvent(ctx, attr, "submit__prevent", fn, extras...)
}

// OnIntersect runs a closure when the element scrolls into view - the
// trigger a sentinel at the end of a list needs, and the whole of infinite
// scroll that has to happen in the browser.
//
// The attribute is data-on-intersect, with a hyphen and no key, because
// Datastar v1 registers this as a plugin of its own rather than as an event
// name for the "on" plugin - its requirement is {key: "denied"}, so
// data-on:intersect is an error rather than a synonym. That is the one
// place the colon rule in this package does not apply.
//
// # It fires every time, and that is what makes a feed work
//
// The observer reports a transition, so an element that is already visible
// and stays visible fires once. On its own that stalls a feed the first
// time a page of items is too short to push the sentinel off screen: the
// list stops loading with the sentinel sitting in plain view.
//
// What prevents it is a property of how Datastar re-applies attributes,
// read out of the v1.0.2 bundle rather than hoped for. Every render gives
// the action a new id, so the expression this writes changes; Datastar's
// mutation observer sees the changed value and calls the plugin's apply
// again, which constructs a fresh IntersectionObserver and observes the
// element - and observe always delivers an initial callback with the
// current state. Still in view, so it fires, so the next page loads, until
// the viewport is full or the component stops rendering a sentinel. The new
// apply runs before the old cleanup, so the element is never unobserved in
// between.
//
// Two consequences worth knowing. The closure must be idempotent in the
// sense that it advances a cursor rather than re-reading one, since it can
// run several times before the reader does anything. And a component that
// re-renders for unrelated reasons while the sentinel is on screen will
// load another page; render no sentinel once the source is exhausted, which
// is what [live.Feed] does.
func OnIntersect[O ~func(T), T any](ctx context.Context, attr Attrs[O], fn Action, extras ...Extra) O {
	return onAttr(ctx, attr, "data-on-intersect", fn, extras)
}
