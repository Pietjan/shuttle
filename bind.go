package shuttle

import "context"

// This file is the low-level binding layer: how a downstream package
// produces Loom options generically.
//
// Loom gives each component package its own option type - button.Option is
// func(*button.Config), card.Option is func(*card.Config) - and the shared
// constraint that unifies them, opts.HasCommon, lives in internal/. So the
// obvious signature is unavailable:
//
//	func On[T opts.HasCommon](...) func(T)   // opts is internal; won't compile
//
// The way around it is to take the package's own Attr function as a
// parameter and let inference work backwards from its result type. The
// closure-based bindings in action.go are all built on this.

// pair is one attribute waiting to be applied.
type pair struct{ key, val string }

// bind collapses several attributes into a single option.
//
// Returning one option rather than a slice is forced by the language: Go
// forbids mixing a splat with other arguments, so button.New(button.Primary,
// bind(...)...) does not compile. Everything a binding needs has to arrive
// as one value.
func bind[O ~func(T), T any](attr Attrs[O], attrs ...pair) O {
	return func(t T) {
		for _, a := range attrs {
			attr(a.key, a.val)(t)
		}
	}
}

// On binds a DOM event to a Datastar expression.
//
// The event name is passed through as-is. Note that Datastar kebab-cases
// event names by default, so data-on:myEvent listens for "my-event" - that
// is a caller concern until this grows a real action table.
func On[O ~func(T), T any](attr Attrs[O], event, expr string) O {
	return bind(attr, pair{"data-on:" + event, expr})
}

// An Extra is one more attribute a binding carries alongside its handler,
// and the whole reason bind takes a variadic: everything an element needs
// has to arrive as a single option. It is built from the render scope,
// because the attributes worth adding this way are the ones that have to
// name the component they are on. See [Indicator].
type Extra func(sc *scope) pair

// Indicator marks an element with a signal that is true while a request
// this element started is in flight - a save button that disables itself,
// or says "Saving…", without a round trip to find out it is busy:
//
//	button.New(button.Primary,
//	    shuttle.OnClick(ctx, button.Attr, s.save, shuttle.Indicator("saving")),
//	    button.Attr("data-attr:disabled", shuttle.IndicatorRef(ctx, "saving")),
//	)
//
// Datastar counts the element's own fetches, so the signal is true for
// exactly as long as the action POST takes - which is how long the action
// itself takes, since the request does not return until the closure has run
// on the session goroutine. The re-render lands a beat later, down the
// stream.
//
// The signal is namespaced by the component's path and rooted outside the
// component's own namespace, so it neither collides with another instance
// nor travels back to the server. Read it with [IndicatorRef].
func Indicator(name string) Extra {
	return func(sc *scope) pair {
		sc.declare(name)
		return pair{"data-indicator", sc.indicatorFor(name)}
	}
}

// IndicatorRef returns an indicator as an expression reference -
// "$ind.c.1.saving" - which is how this component's markup reads it. It
// returns "" outside a render pass.
//
// It exists because the path names the component's position in the tree: a
// component that is not the root cannot spell its own indicator, and one
// that hard-codes "$ind.c.saving" works alone and silently watches the
// wrong signal the moment it is mounted as somebody's child. Same reason as
// [Ref], and the same failure if you skip it.
func IndicatorRef(ctx context.Context, name string) string {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return ""
	}
	return "$" + sc.indicatorFor(name)
}
