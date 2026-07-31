package shuttle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
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
	l.sess.params = p
	l.sess.url = l.sess.path(nil)

	err := l.sess.call(func() error {
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

// fire finds the element's binding for an event and invokes it.
func (l *Live) fire(event, sel string) *Live {
	l.tb.Helper()

	node := l.first(sel)
	if node == nil {
		l.tb.Fatalf("shuttle: no element matches %q in:\n%s", sel, l.markup)
		return l
	}

	// The attribute may carry modifiers - data-on:input__debounce.300ms -
	// so match on the prefix rather than the whole name.
	want := "data-on:" + event
	expr := ""
	for _, a := range node.Attr {
		if a.Key == want || strings.HasPrefix(a.Key, want+"__") {
			expr = a.Val
			break
		}
	}
	if expr == "" {
		l.tb.Fatalf("shuttle: %q has no %s binding (attributes: %s)", sel, event, attrNames(node))
		return l
	}

	m := actionRE.FindStringSubmatch(expr)
	if m == nil {
		l.tb.Fatalf("shuttle: %q's %s binding is not a server action: %s", sel, event, expr)
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

	if err := l.sess.call(fn); err != nil {
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
	_ = l.sess.call(func() error { return nil })

	for _, p := range l.sess.take() {
		switch {
		case p.target == l.sess.RootID():
			l.markup = p.html
		case p.html != "":
			// A scoped re-render: a child patched itself, so splice it in
			// the way the browser's morph would. Without this an assertion
			// after an action on a child would still be looking at the
			// render before it.
			if merged, ok := applyPatch(l.markup, p.target, p.html); ok {
				l.markup = merged
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
