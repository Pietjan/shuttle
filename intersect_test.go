package shuttle

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// watcher is the shape a sentinel has: an element that asks for more when it
// scrolls into view, and stops being rendered once there is no more.
type watcher struct {
	Base
	Loaded int
	Max    int
}

func (w *watcher) Render(ctx context.Context) templ.Component {
	done := w.Loaded >= w.Max

	var sentinel templ.Component = templ.NopComponent
	if !done {
		sentinel = spanWith(OnIntersect(ctx, spanAttr, func(context.Context) error {
			w.Loaded++
			return nil
		}), "sentinel")
	}

	return templ.ComponentFunc(func(ctx context.Context, wr io.Writer) error {
		if _, err := fmt.Fprintf(wr, `<p class="count">%d</p>`, w.Loaded); err != nil {
			return err
		}
		return sentinel.Render(ctx, wr)
	})
}

// spanAttr is a minimal stand-in for a Loom package's Attr, so these tests
// exercise the binding rather than a component's option plumbing.
type spanConfig struct{ attrs []pair }

func spanAttr(key string, val ...string) func(*spanConfig) {
	v := ""
	if len(val) > 0 {
		v = val[0]
	}
	return func(c *spanConfig) { c.attrs = append(c.attrs, pair{key, v}) }
}

func spanWith(opt func(*spanConfig), class string) templ.Component {
	var c spanConfig
	opt(&c)
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		fmt.Fprintf(&b, `<span class=%q`, class)
		for _, a := range c.attrs {
			fmt.Fprintf(&b, ` %s=%q`, a.key, templ.EscapeString(a.val))
		}
		b.WriteString(`></span>`)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

// TestIntersectIsItsOwnPluginNotAnEventName is the detail the rest of this
// package's syntax rule would get wrong.
//
// Everywhere else in Datastar v1 the form is data-on:<event>, and this file
// documents the one exception: on-intersect is registered as a plugin in
// its own right, with {key: "denied"}, so data-on:intersect is not a
// synonym for it - it is an error that takes the rest of the element's
// attributes with it.
func TestIntersectIsItsOwnPluginNotAnEventName(t *testing.T) {
	live := Test(t, &watcher{Max: 3})

	markup := live.HTML()
	if !strings.Contains(markup, "data-on-intersect=") {
		t.Errorf("no data-on-intersect binding in %q", markup)
	}
	if strings.Contains(markup, "data-on:intersect") {
		t.Errorf("bound as an event name, which Datastar refuses: %q", markup)
	}
}

// TestIntersectRunsTheClosure, through the testing kit, which needs its own
// way in for the same reason: the binding cannot be reached by naming a DOM
// event.
func TestIntersectRunsTheClosure(t *testing.T) {
	w := &watcher{Max: 3}
	live := Test(t, w)

	live.Intersect(".sentinel")
	if w.Loaded != 1 {
		t.Fatalf("the sentinel loaded %d times, want 1", w.Loaded)
	}
	live.Assert().Text(".count", "1")
}

// TestEveryRenderRearmsTheSentinel is the property infinite scroll rests on,
// asserted at the only place a Go test can see it: the expression.
//
// In a browser, a fresh IntersectionObserver reports the element's current
// state as soon as it observes, so a sentinel still on screen fires again
// and the next page loads. Datastar only re-applies the plugin when the
// attribute's value *changes* - so if two renders ever wrote the same
// expression, the observer would not be rebuilt and a feed would stall with
// its sentinel in plain view. The generation prefix in the action id is
// what keeps that from happening, and it is load-bearing here in a way
// nothing else in this package depends on.
func TestEveryRenderRearmsTheSentinel(t *testing.T) {
	w := &watcher{Max: 3}
	live := Test(t, w)

	first := sentinelExpr(t, live.HTML())
	live.Intersect(".sentinel")
	second := sentinelExpr(t, live.HTML())

	if first == second {
		t.Errorf("both renders wrote %q, so the observer is never rebuilt and the feed stalls", first)
	}
}

// TestTheSentinelStopsBeingRenderedAtTheEnd. Nothing else stops the loading:
// as long as the element is on screen and the component keeps re-rendering,
// it keeps asking. Exhaustion has to be expressed by not rendering it.
func TestTheSentinelStopsBeingRenderedAtTheEnd(t *testing.T) {
	w := &watcher{Max: 2}
	live := Test(t, w)

	live.Intersect(".sentinel")
	live.Intersect(".sentinel")

	if got := live.HTML(); strings.Contains(got, "data-on-intersect") {
		t.Errorf("the sentinel outlived the last page: %q", got)
	}
	live.Assert().Text(".count", "2")
}

// sentinelExpr pulls the bound expression out of a render.
func sentinelExpr(t *testing.T, markup string) string {
	t.Helper()
	const key = `data-on-intersect="`
	i := strings.Index(markup, key)
	if i < 0 {
		t.Fatalf("no sentinel in %q", markup)
	}
	rest := markup[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated attribute in %q", markup)
	}
	return rest[:j]
}
