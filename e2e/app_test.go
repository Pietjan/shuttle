//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/playwright-community/playwright-go"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/shuttle"
)

// --- server push -----------------------------------------------------------

const topic = "room"

// room subscribes on mount and shouts on click. Two pages of it share a
// broker, which is the whole premise: state on the server, so a message
// reaches every connected page without the client asking.
type room struct {
	shuttle.Base
	Heard []string
}

func (r *room) Mount(ctx context.Context, _ shuttle.Params) error {
	return r.Join(ctx, topic, "guest")
}

func (r *room) HandleInfo(_ context.Context, msg any) error {
	if ev, ok := msg.(shuttle.PresenceEvent); ok {
		r.Heard = append(r.Heard, fmt.Sprintf("presence:%v", ev.Joined))
		return nil
	}
	r.Heard = append(r.Heard, fmt.Sprint(msg))
	return nil
}

func (r *room) Render(ctx context.Context) templ.Component {
	shout := button.New(
		button.Attr("data-role", "shout"),
		shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
			return r.Publish(actx, topic, "hello")
		}),
	)

	heard, here := r.Heard, len(r.Presence(topic))
	return seq(with(shout, text("shout")),
		templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			_, err := fmt.Fprintf(w, `<p data-here="%d"></p><ul data-heard>`, here)
			if err != nil {
				return err
			}
			for _, line := range heard {
				if _, err := fmt.Fprintf(w, `<li>%s</li>`, templ.EscapeString(line)); err != nil {
					return err
				}
			}
			_, err = io.WriteString(w, `</ul>`)
			return err
		}))
}

// TestPublishReachesAnotherPage: two browsers, one handler. Nothing in the
// second page asked for this - it is the stream doing what holding state on
// the server is for.
func TestPublishReachesAnotherPage(t *testing.T) {
	url, _ := serve(t, func() shuttle.Component { return &room{} })
	first := visit(t, url)
	second := visit(t, url)

	if err := first.Locator(`[data-role="shout"]`).Click(); err != nil {
		t.Fatalf("shout: %v", err)
	}
	if err := expect(second.Locator(`[data-heard]`)).ToContainText("hello"); err != nil {
		t.Errorf("the message never reached the other page: %v", err)
	}
}

// TestPresenceCountsBothPages, and counts them down again when one leaves.
func TestPresenceCountsBothPages(t *testing.T) {
	// A session outlives its stream by Grace (30s by default) so a reconnect
	// keeps its state - which also keeps it on the roster. Shorten it, or
	// this test would be asserting the default timeout rather than presence.
	url, _ := serve(t, func() shuttle.Component { return &room{} },
		func(h *shuttle.Handler) { h.Grace = 300 * time.Millisecond })
	first := visit(t, url)
	second := visit(t, url)

	if err := expect(first.Locator(`[data-here="2"]`)).ToBeAttached(); err != nil {
		t.Fatalf("the second page never joined the roster: %v", err)
	}

	if err := second.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The roster follows the session, and the session follows the stream.
	if err := expect(first.Locator(`[data-here="1"]`)).ToBeAttached(); err != nil {
		t.Errorf("a closed page stayed on the roster: %v", err)
	}
}

// --- pending state ---------------------------------------------------------

type slow struct {
	shuttle.Base
	Ran int
}

func (s *slow) Render(ctx context.Context) templ.Component {
	save := button.New(
		button.Attr("data-role", "save"),
		button.Attr("data-attr:disabled", shuttle.IndicatorRef(ctx, "saving")),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			time.Sleep(700 * time.Millisecond)
			s.Ran++
			return nil
		}, shuttle.Indicator("saving")),
	)
	return with(save, text("save"))
}

// slowClickFirst is the same component with the two attributes swapped: the
// click binding is rendered before the one that reads the indicator signal.
type slowClickFirst struct {
	shuttle.Base
	Ran int
}

func (s *slowClickFirst) Render(ctx context.Context) templ.Component {
	save := button.New(
		button.Attr("data-role", "save"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			time.Sleep(700 * time.Millisecond)
			s.Ran++
			return nil
		}, shuttle.Indicator("saving")),
		button.Attr("data-attr:disabled", shuttle.IndicatorRef(ctx, "saving")),
	)
	return with(save, text("save"))
}

// TestIndicatorTracksTheRequest. The signal is the client's own: nothing is
// sent to say the work started, and nothing is sent to say it finished.
//
// Both orders, because the first draft of this test only passed in one of
// them: an expression reading the indicator before data-indicator had
// created the signal took an error that killed the click binding on the same
// element, silently. The root declares the signal now, and this is what says
// so - a component author should not have to know which option to pass
// first.
func TestIndicatorTracksTheRequest(t *testing.T) {
	t.Run("read before bound", func(t *testing.T) { indicatorRuns(t, &slow{}, func() int { return 0 }) })
	t.Run("bound before read", func(t *testing.T) {
		cmp := &slowClickFirst{}
		indicatorRunsWith(t, cmp, func() int { return cmp.Ran })
	})
}

func indicatorRuns(t *testing.T, cmp *slow, _ func() int) {
	t.Helper()
	indicatorRunsWith(t, cmp, func() int { return cmp.Ran })
}

func indicatorRunsWith(t *testing.T, cmp shuttle.Component, ran func() int) {
	t.Helper()
	p := open(t, func() shuttle.Component { return cmp })

	save := p.Locator(`[data-role="save"]`)
	if err := expect(save).ToBeEnabled(); err != nil {
		t.Fatalf("disabled before anything happened: %v", err)
	}

	if err := save.Click(); err != nil {
		t.Fatalf("click: %v", err)
	}
	if err := expect(save).ToBeDisabled(); err != nil {
		t.Errorf("the indicator never lit: %v", err)
	}
	if err := expect(save).ToBeEnabled(); err != nil {
		t.Errorf("the indicator never cleared: %v", err)
	}
	eventually(t, "the action to have run", func() bool { return ran() == 1 })
}

// --- navigation ------------------------------------------------------------

type tabs struct {
	shuttle.Base
	Tab string
}

func (c *tabs) HandleParams(_ context.Context, p shuttle.Params) error {
	c.Tab = p.Get("tab")
	if c.Tab == "" {
		c.Tab = "one"
	}
	return nil
}

func (c *tabs) QueryParams() url.Values { return url.Values{"tab": {c.Tab}} }

func (c *tabs) Render(ctx context.Context) templ.Component {
	parts := []templ.Component{}
	for _, name := range []string{"one", "two"} {
		tab := name
		parts = append(parts, with(button.New(
			button.Attr("data-tab", tab),
			shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
				return c.Navigate(actx, "/?tab="+tab)
			}),
		), text(tab)))
	}

	shown := c.Tab
	parts = append(parts, templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p data-showing=%q></p>`, shown)
		return err
	}))
	return seq(parts...)
}

// TestBackButtonRestoresTheView is the one piece of navigation the server
// cannot see for itself, and the reason shuttle ships a popstate listener at
// all: the history entry is pushed from the server, and the button that
// undoes it is the browser's.
func TestBackButtonRestoresTheView(t *testing.T) {
	p := open(t, func() shuttle.Component { return &tabs{} })

	if err := p.Locator(`[data-tab="two"]`).Click(); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if err := expect(p.Locator(`[data-showing="two"]`)).ToBeAttached(); err != nil {
		t.Fatalf("never switched: %v", err)
	}
	if err := expectPage(p).ToHaveURL(regexp.MustCompile(`tab=two`)); err != nil {
		t.Errorf("the URL did not follow: %v", err)
	}

	if _, err := p.GoBack(); err != nil {
		t.Fatalf("back: %v", err)
	}
	if err := expect(p.Locator(`[data-showing="one"]`)).ToBeAttached(); err != nil {
		t.Errorf("back did not restore the view - no page load, so this is the shim: %v", err)
	}
}

// --- connection state ------------------------------------------------------

// TestConnectionStateReachesThePage. A page whose server went away looks
// perfectly fine, which is why the shim says so on <html> - and why an app
// can style it.
func TestConnectionStateReachesThePage(t *testing.T) {
	url, srv := serve(t, func() shuttle.Component { return &slow{} })
	p := visit(t, url)

	// CloseClientConnections rather than Close: Close waits for outstanding
	// requests, and the stream is a request that never finishes.
	srv.CloseClientConnections()

	if err := expect(p.Locator("html.shuttle-reconnecting")).ToBeAttached(); err != nil {
		t.Errorf("a dropped stream left the page claiming to be connected: %v", err)
	}
}

// --- scoped re-render ------------------------------------------------------

type board struct {
	shuttle.Base
}

func (b *board) Render(ctx context.Context) templ.Component {
	rows := make([]templ.Component, 0, 2)
	for _, name := range []string{"a", "b"} {
		key := name
		rows = append(rows, shuttle.Child(ctx, key, func() shuttle.Component {
			return &tally{Name: key}
		}))
	}
	return seq(rows...)
}

type tally struct {
	shuttle.Base
	Name  string
	Ticks int
}

func (c *tally) Render(ctx context.Context) templ.Component {
	tick := button.New(
		button.Attr("data-tick", c.Name),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Ticks++
			return nil
		}),
	)
	name, ticks := c.Name, c.Ticks
	return seq(with(tick, text("tick")),
		templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			_, err := fmt.Fprintf(w, `<p data-count=%q>%d</p>`, name, ticks)
			return err
		}))
}

// TestOnlyTheClickedChildIsPatched. The per-node action table and the morph
// target exist for this, and only a browser can see it: the sibling is
// marked from the page, and a patch that replaced it would take the mark.
func TestOnlyTheClickedChildIsPatched(t *testing.T) {
	p := open(t, func() shuttle.Component { return &board{} })

	sibling := p.Locator(`[data-count="b"]`)
	if _, err := sibling.Evaluate(`el => el.dataset.marked = "yes"`, nil); err != nil {
		t.Fatalf("mark: %v", err)
	}

	if err := p.Locator(`[data-tick="a"]`).Click(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if err := expect(p.Locator(`[data-count="a"]`)).ToHaveText("1"); err != nil {
		t.Fatalf("the clicked child did not re-render: %v", err)
	}

	if err := expect(sibling).ToHaveAttribute("data-marked", "yes"); err != nil {
		t.Errorf("the sibling was patched too, so the re-render was not scoped: %v", err)
	}
}

// --- teardown --------------------------------------------------------------

type switcher struct {
	shuttle.Base
	Show string
}

func (s *switcher) Render(ctx context.Context) templ.Component {
	swap := button.New(
		button.Attr("data-role", "swap"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			s.Show = "second"
			return nil
		}),
	)

	key := s.Show
	if key == "" {
		key = "first"
	}
	return seq(with(swap, text("swap")),
		shuttle.Child(ctx, key, func() shuttle.Component { return &ticker{} }))
}

// ticker patches itself on a timer. Unmounted, it must stop: a patch aimed
// at an element that has left the page is silent server-side and only a
// console warning here.
type ticker struct {
	shuttle.Base
	Ticks int
}

func (c *ticker) Mount(context.Context, shuttle.Params) error {
	return c.Every(100*time.Millisecond, func(ctx context.Context) error {
		c.Ticks++
		return c.Push(ctx)
	})
}

func (c *ticker) Render(_ context.Context) templ.Component {
	ticks := c.Ticks
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p data-ticks="%d"></p>`, ticks)
		return err
	})
}

// TestUnmountedComponentStopsPatching, which only a browser can check: the
// server sends a patch for a missing element happily, and the client drops
// it with a console warning nobody server-side ever hears.
func TestUnmountedComponentStopsPatching(t *testing.T) {
	p := open(t, func() shuttle.Component { return &switcher{} })

	var warnings []string
	p.OnConsole(func(m playwright.ConsoleMessage) {
		if m.Type() == "warning" || m.Type() == "error" {
			warnings = append(warnings, m.Text())
		}
	})

	if err := expect(p.Locator(`[data-ticks]`)).ToBeAttached(); err != nil {
		t.Fatalf("the first child never rendered: %v", err)
	}
	if err := p.Locator(`[data-role="swap"]`).Click(); err != nil {
		t.Fatalf("swap: %v", err)
	}

	// Long enough for a good few ticks of the timer the old child started.
	time.Sleep(600 * time.Millisecond)

	for _, w := range warnings {
		if containsAny(w, "No targets found", "not found", "PatchElements") {
			t.Errorf("an unmounted component kept patching: %q", w)
		}
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if contains(haystack, n) {
			return true
		}
	}
	return false
}

var _ = strconv.Itoa
