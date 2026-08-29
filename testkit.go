package shuttle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"github.com/starfederation/datastar-go/datastar"
)

// The testing kit drives a component the way the transport does - mount,
// act, re-render - with no browser, no HTTP server and no stream. What it
// asserts on is the markup the client would actually have received, not a
// render performed for the test's benefit, so a component that renders
// nothing on an action fails here the same way it would in a page.

// TB is the part of testing.TB the kit uses. Declaring it rather than
// importing testing keeps the library free of a test-only dependency.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Live is a mounted component under test.
type Live struct {
	tb   TB
	sess *Session

	// markup is the latest render of the root component.
	markup string
	// signals are the client values the next action carries.
	signals map[string]any
	// patches are everything pushed since the last call to Patches.
	patches []patch
}

// Test mounts cmp and renders it once, running Mount and HandleParams the
// way a page load would.
//
//	live := shuttle.Test(t, &Counter{})
//	live.Click("button")
//	live.Assert().Text("#count", "1")
//
// For a component driven by [Base.Every], wrap the test in
// [testing/synctest.Test]: the session's goroutine and its timers all live
// in the bubble, so time.Sleep advances the fake clock and tick counts are
// exact instead of raced against the wall clock.
//
//	synctest.Test(t, func(t *testing.T) {
//	    live := shuttle.Test(t, &Clock{})
//	    time.Sleep(3 * time.Second)
//	    synctest.Wait()
//	    live.Assert().Text("#ticks", "3")
//	})
func Test(tb TB, cmp Component) *Live {
	tb.Helper()

	l := &Live{
		tb:      tb,
		sess:    newSession("test", cmp),
		signals: map[string]any{},
	}
	l.sess.onError = func(what string, err error) {
		tb.Errorf("shuttle: %s failed: %v", what, err)
	}
	tb.Cleanup(func() { l.sess.close(context.Background()) })

	l.mount(nil)
	return l
}

// mount runs the lifecycle a first page load would.
func (l *Live) mount(p Params) {
	l.tb.Helper()

	ctx := context.Background()
	l.sess.mu.Lock()
	l.sess.params = p
	l.sess.url = l.sess.pathLocked(nil)
	l.sess.mu.Unlock()

	err := l.sess.call(ctx, func() error {
		if m, ok := l.sess.root.cmp.(Mounter); ok {
			if err := m.Mount(ctx, p); err != nil {
				return fmt.Errorf("mount: %w", err)
			}
		}
		if h, ok := l.sess.root.cmp.(ParamsHandler); ok {
			if err := h.HandleParams(ctx, p); err != nil {
				return fmt.Errorf("params: %w", err)
			}
		}
		var err error
		l.markup, err = l.sess.Render(ctx)
		return err
	})
	if err != nil {
		l.tb.Fatalf("shuttle: mounting: %v", err)
	}
	// The kit is now holding this markup the way a browser holds its first
	// paint, so the generations in it get the same in-flight-click grace.
	l.sess.markTreeSent()
}

// Component returns the instance under test, for asserting on its state
// directly - which is often the clearest thing a component test can do.
func (l *Live) Component() Component { return l.sess.root.cmp }

// Session returns the session driving it.
func (l *Live) Session() *Session { return l.sess }

// HTML returns the latest markup of the root component.
func (l *Live) HTML() string { return l.markup }

// Signal sets a client-side signal value carried by the next action, as
// though the user had typed it into a bound input.
//
// The name is local: it lands in the namespace of whichever component the
// action fires on, exactly as DecodeSignals will read it. That mirrors the
// browser, where filterSignals scopes each action's payload to its own
// component - so a value set here never leaks into a different component's
// action the way a global store would let it.
//
// Values are sticky, like the browser's signal store they stand in for: a
// value set once rides on every later action until it is set again.
func (l *Live) Signal(name string, value any) *Live {
	l.signals[name] = value
	return l
}

// Params changes the page's query parameters, the way a filter or the back
// button would: HandleParams runs and the component re-renders, without a
// remount.
func (l *Live) Params(query string) *Live {
	l.tb.Helper()

	u, err := parseLocation(query)
	if err != nil {
		l.tb.Fatalf("shuttle: bad location %q: %v", query, err)
	}
	l.run("navigating", func() error {
		return l.sess.applyLocation(context.Background(), u)
	})
	return l
}

// Click fires the click handler on the first element matching sel.
func (l *Live) Click(sel string) *Live {
	l.tb.Helper()
	return l.fire("click", sel)
}

// Submit fires the submit handler on the first element matching sel.
func (l *Live) Submit(sel string) *Live {
	l.tb.Helper()
	return l.fire("submit", sel)
}

// Change fires the input handler on the first element matching sel - the
// validate-as-you-type half of a form. Set the value with Signal first.
func (l *Live) Change(sel string) *Live {
	l.tb.Helper()
	return l.fire("input", sel)
}

// Intersect fires the handler for the first element matching sel scrolling
// into view - the trigger behind infinite scroll, and the one binding a
// test cannot reach by naming a DOM event, since it is its own Datastar
// plugin rather than an event name.
//
// What it cannot stand in for is the re-arm: in a browser, the sentinel
// fires again by itself whenever a render leaves it on screen, and only a
// browser knows what is on screen. Call it once per page you expect to be
// loaded, and leave "does it keep loading until the viewport is full" to
// the end-to-end suite.
func (l *Live) Intersect(sel string) *Live {
	l.tb.Helper()
	return l.fireAttr("data-on-intersect", "intersect", sel)
}

// Publish sends a message through the session's broker, so a component's
// HandleInfo can be tested without a second page.
func (l *Live) Publish(topic string, msg any) *Live {
	l.tb.Helper()

	if err := l.sess.broker.Publish(context.Background(), topic, msg); err != nil {
		l.tb.Fatalf("shuttle: publishing to %q: %v", topic, err)
	}
	l.settle()
	return l
}

// Patches returns everything pushed since the last call, so a test can
// assert on streamed items and server pushes rather than only on the
// component's own markup.
func (l *Live) Patches() []string {
	l.settle()

	out := make([]string, 0, len(l.patches))
	for _, p := range l.patches {
		out = append(out, p.html)
	}
	l.patches = nil
	return out
}

// actionRE finds a binding's endpoint in a rendered attribute.
var actionRE = regexp.MustCompile(`@post\('([^']*)'`)

// fire finds the element's binding for a DOM event and invokes it.
func (l *Live) fire(event, sel string) *Live {
	l.tb.Helper()
	return l.fireAttr("data-on:"+event, event, sel)
}

// fireAttr invokes the action bound to one attribute. The attribute is a
// parameter because not every trigger is the "on" plugin with an event key;
// what is being simulated is passed alongside it, for the failure message.
func (l *Live) fireAttr(want, what, sel string) *Live {
	l.tb.Helper()

	node := l.first(sel)
	if node == nil {
		l.tb.Fatalf("shuttle: no element matches %q in:\n%s", sel, l.markup)
		return l
	}

	// The attribute may carry modifiers - data-on:input__debounce.300ms -
	// so match on the prefix rather than the whole name.
	expr := ""
	for _, a := range node.Attr {
		if a.Key == want || strings.HasPrefix(a.Key, want+"__") {
			expr = a.Val
			break
		}
	}
	if expr == "" {
		l.tb.Fatalf("shuttle: %q has no %s binding (attributes: %s)", sel, what, attrNames(node))
		return l
	}

	m := actionRE.FindStringSubmatch(expr)
	if m == nil {
		l.tb.Fatalf("shuttle: %q's %s binding is not a server action: %s", sel, what, expr)
		return l
	}

	nodeID, actionID, ok := splitAction(m[1])
	if !ok {
		l.tb.Fatalf("shuttle: cannot read the action out of %q", m[1])
		return l
	}

	target, ok := l.sess.node(nodeID)
	if !ok {
		l.tb.Fatalf("shuttle: no component mounted at %q", nodeID)
		return l
	}
	fn, ok := target.lookup(actionID)
	if !ok {
		l.tb.Fatalf("shuttle: no action %q on %q", actionID, nodeID)
		return l
	}

	ctx := l.signalContext()
	l.run("acting", func() error {
		if err := fn(ctx); err != nil {
			return err
		}
		return target.push(ctx)
	})
	return l
}

// signalContext builds the payload an action would arrive with.
func (l *Live) signalContext() context.Context {
	ctx := context.Background()
	if len(l.signals) == 0 {
		return ctx
	}
	raw, err := json.Marshal(l.signals)
	if err != nil {
		l.tb.Fatalf("shuttle: encoding signals: %v", err)
		return ctx
	}
	return withSignals(ctx, raw)
}

// run performs one piece of session work and waits for the render it
// causes, so an assertion never races the component it is about to check.
func (l *Live) run(what string, fn func() error) {
	l.tb.Helper()

	if err := l.sess.call(context.Background(), fn); err != nil {
		l.tb.Fatalf("shuttle: %s: %v", what, err)
		return
	}
	l.settle()
}

// settle waits for the session's goroutine to finish what it was given and
// collects whatever it pushed.
func (l *Live) settle() {
	// The mailbox is first-in-first-out, so when an empty item comes back
	// the work queued before it has run - and its renders with it.
	if err := l.sess.call(context.Background(), func() error { return nil }); err != nil {
		// A closed session cannot settle; saying so beats asserting on
		// whatever markup happened to be lying around.
		l.tb.Errorf("shuttle: settling: %v", err)
		return
	}

	for _, p := range l.sess.take() {
		switch {
		case p.script != "":
			// Nothing to apply: navigation travels down the stream as a
			// script rather than as markup.
		case p.signals != "":
			// A server-side signal write; the kit's flat signal map does not
			// model the namespaced store, so it is recorded but not applied.
		default:
			// Every patch lands where the browser would put it: a child
			// splicing in its own re-render, or a stream operation adding,
			// replacing or removing one item of a container. Without this
			// an assertion after an action would still be looking at the
			// render before it.
			//
			// The root's own re-render goes through the same path rather
			// than replacing the markup wholesale, because that is the one
			// patch that has to leave data-ignore-morph containers alone -
			// and the shortcut is what would throw away everything a
			// component had streamed into one.
			if merged, ok := applyPatch(l.markup, p.target, p.html, p.mode); ok {
				l.markup = merged
			} else if p.target == l.sess.RootID() && p.mode == datastar.ElementPatchModeOuter {
				l.markup = p.html
			} else {
				// A browser drops a patch whose target is missing with only a
				// console warning; a test should be louder, because every
				// assertion after this one would run against stale markup.
				l.tb.Errorf("shuttle: patch target %q is not in the page", p.target)
			}
		}
		l.patches = append(l.patches, p)
	}
}

// first returns the first element matching sel in the current markup.
func (l *Live) first(sel string) *html.Node {
	l.tb.Helper()

	found, err := selectAll(l.markup, sel)
	if err != nil {
		l.tb.Fatalf("shuttle: parsing markup: %v", err)
		return nil
	}
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

func attrNames(n *html.Node) string {
	names := make([]string, 0, len(n.Attr))
	for _, a := range n.Attr {
		names = append(names, a.Key)
	}
	return strings.Join(names, " ")
}

// splitAction pulls the component and action out of an action URL.
func splitAction(u string) (nodeID, actionID string, ok bool) {
	parts := strings.Split(strings.Trim(u, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[len(parts)-2], parts[len(parts)-1], true
}

// parseLocation accepts "a=1&b=2", "?a=1" or "/path?a=1".
func parseLocation(s string) (*url.URL, error) {
	if !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "?") {
		s = "?" + s
	}
	return url.Parse(s)
}
