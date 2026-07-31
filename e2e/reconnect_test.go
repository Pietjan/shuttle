//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/shuttle"
)

// counter is state worth losing: if a reconnect brings back a different
// component instance, the number resets and the test says so.
type counter struct {
	shuttle.Base
	Count int
}

func (c *counter) Render(ctx context.Context) templ.Component {
	bump := button.New(
		button.Attr("data-role", "bump"),
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Count++
			return nil
		}),
	)

	count := c.Count
	return seq(with(bump, text("bump")),
		templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			_, err := fmt.Fprintf(w, `<p data-count="%d"></p>`, count)
			return err
		}))
}

// TestReconnectKeepsTheSession. A stream that drops is not a session that
// ended: Grace is how long the state waits for the page to come back, and
// this is what that promise looks like from the page - the count survives,
// so the component instance did.
func TestReconnectKeepsTheSession(t *testing.T) {
	// A short heartbeat, and the reason is the finding this test produced:
	// the server only learns a client is gone when a write to it fails, and
	// until it does, sess.attach refuses the reconnect with a 409. So the
	// window a page spends reconnecting is bounded by Heartbeat - 25s by
	// default, which is a long time to look broken.
	app := serveRestartable(t, func() shuttle.Component { return &counter{} })
	p := visit(t, app.URL)

	for range 2 {
		if err := p.Locator(`[data-role="bump"]`).Click(); err != nil {
			t.Fatalf("bump: %v", err)
		}
	}
	if err := expect(p.Locator(`[data-count="2"]`)).ToBeAttached(); err != nil {
		t.Fatalf("never counted: %v", err)
	}

	app.Drop()

	// Datastar retries with a backoff, so this waits longer than a normal
	// assertion would - and reports what the page said while it waited,
	// because "not connected" is three different states.
	//
	// This is the test that found the greeting: before it, the page said
	// "reconnecting" for a whole heartbeat after the stream was already
	// back, because nothing had arrived down it yet.
	var seen []string
	eventuallySlow(t, "the stream to come back", func() bool {
		v, err := p.Evaluate(`() => document.documentElement.dataset.shuttleState`)
		if err != nil {
			return false
		}
		state, _ := v.(string)
		if len(seen) == 0 || seen[len(seen)-1] != state {
			seen = append(seen, state)
		}
		return state == "connected"
	}, func() {
		t.Logf("states seen: %v", seen)
		for _, line := range app.Statuses() {
			t.Logf("server: %s", line)
		}
	})

	// The proof: the next click continues from 2 rather than starting over,
	// so the reconnect found the session it left.
	if err := p.Locator(`[data-role="bump"]`).Click(); err != nil {
		t.Fatalf("bump after reconnect: %v", err)
	}
	if err := expect(p.Locator(`[data-count="3"]`)).ToBeAttached(); err != nil {
		t.Errorf("the reconnect got a fresh component, so the state was lost: %v", err)
	}
}

// TestUnknownSessionReloadsThePage is the deploy case, and the most
// expensive failure this design has: after a restart every open page holds a
// session the new process never heard of, and Datastar would retry that 404
// until it gave up for good. So the stream answers with a reload instead.
//
// Without this, one deploy permanently breaks every page anyone has open.
func TestUnknownSessionReloadsThePage(t *testing.T) {
	app := serveRestartable(t, func() shuttle.Component { return &counter{} })
	p := visit(t, app.URL)

	if err := p.Locator(`[data-role="bump"]`).Click(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := expect(p.Locator(`[data-count="1"]`)).ToBeAttached(); err != nil {
		t.Fatalf("never counted: %v", err)
	}

	// Something the reload cannot survive, so the test can tell a reload
	// from a re-render.
	if _, err := p.Evaluate(`() => window.__beforeRestart = true`); err != nil {
		t.Fatalf("mark: %v", err)
	}

	app.Restart()

	eventually(t, "the page to reload itself", func() bool {
		v, err := p.Evaluate(`() => window.__beforeRestart === true`)
		if err != nil {
			// Navigation in flight: the evaluate lost its context, which is
			// itself the reload happening.
			return false
		}
		marked, _ := v.(bool)
		return !marked
	})

	// And it comes back as a working page against the new process, mounted
	// from its URL - which is what makes a cheap, idempotent Mount a real
	// constraint rather than advice.
	if err := expect(p.Locator("html.shuttle-connected")).ToBeAttached(); err != nil {
		t.Fatalf("the reloaded page never connected: %v", err)
	}
	if err := expect(p.Locator(`[data-count="0"]`)).ToBeAttached(); err != nil {
		t.Errorf("the new process did not mount a fresh component: %v", err)
	}
	if err := p.Locator(`[data-role="bump"]`).Click(); err != nil {
		t.Fatalf("bump after reload: %v", err)
	}
	if err := expect(p.Locator(`[data-count="1"]`)).ToBeAttached(); err != nil {
		t.Errorf("the reloaded page does not work: %v", err)
	}
	for _, line := range app.Statuses() {
		t.Logf("server: %s", line)
	}
}

var _ = context.Background
