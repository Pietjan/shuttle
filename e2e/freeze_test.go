//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/mxschmitt/playwright-go"

	"github.com/pietjan/shuttle"
)

// heavyTicker re-renders on its own, with a payload big enough that a render
// queued behind a frozen page fills the socket rather than slipping through
// in a packet: the case that wedged a real page, a 174 KB re-render.
type heavyTicker struct {
	shuttle.Base
	Count int
}

func (c *heavyTicker) Mount(context.Context, shuttle.Params) error {
	return c.Every(500*time.Millisecond, func(context.Context) error {
		c.Count++
		return nil
	})
}

func (c *heavyTicker) Render(context.Context) templ.Component {
	count := c.Count
	pad := strings.Repeat(fmt.Sprintf("%08d", count), 25000)
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p data-count="%d"></p><p hidden>%s</p>`, count, pad)
		return err
	})
}

// TestAFrozenPageCatchesUp freezes a page the way Chrome freezes a hidden
// tab - Edge calls it a sleeping tab - with renders queued behind it, and
// thaws it. On Windows Chrome a stream frozen for over half a minute came
// back delivering nothing, with no error and no end (measured on 2026-10-21
// against valheim-world's map, whose close button stopped working after the
// tab sat in the background); the Chromium in the playwright container does
// not, so the stall itself is pinned by the muted-stream tests below, and
// this one pins that a freeze and the shim's reopen on resume leave a page
// that carries on, on the session it had.
func TestAFrozenPageCatchesUp(t *testing.T) {
	url, _ := serve(t, func() shuttle.Component { return &heavyTicker{} })
	p := visit(t, url)

	count := func() int {
		v, err := p.Evaluate(`() => +(document.querySelector('[data-count]') || {dataset: {count: -1}}).dataset.count`)
		if err != nil {
			return -1
		}
		n, _ := v.(int)
		return n
	}
	eventually(t, "the heavyTicker to tick", func() bool { return count() >= 2 })

	cdp, err := p.Context().NewCDPSession(p)
	if err != nil {
		t.Fatalf("cdp: %v", err)
	}
	if _, err := cdp.Send("Page.setWebLifecycleState", map[string]any{"state": "frozen"}); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	time.Sleep(15 * time.Second)
	if _, err := cdp.Send("Page.setWebLifecycleState", map[string]any{"state": "active"}); err != nil {
		t.Fatalf("thaw: %v", err)
	}

	before := count()
	eventuallySlow(t, "renders to arrive after the thaw", func() bool { return count() > before+2 }, func() {
		t.Logf("count stuck at %d after the thaw", count())
	})
	if c := count(); c < 30 {
		t.Errorf("count %d after a 15 s freeze at two a second: a fresh session, not the one the page left", c)
	}
}

// mutable serves cmp with a switch that mutes the streams already open:
// their writes are accepted and dropped. It is the state a frozen tab left a
// real one in - the server writing without a fault, the page hearing
// nothing, and neither side failing - which the playwright Chromium does not
// reproduce by freezing.
type mutable struct {
	URL  string
	gen  atomic.Int64
	Mute func()
}

func serveMutable(t *testing.T, cmp func() shuttle.Component, opts ...func(*shuttle.Handler)) *mutable {
	t.Helper()
	h := shuttle.New(cmp)
	h.Title = "e2e"
	for _, opt := range opts {
		opt(h)
	}
	m := &mutable{}
	m.Mute = func() { m.gen.Add(1) }

	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/live") {
			w = &muffled{ResponseWriter: w, gen: m.gen.Load(), now: &m.gen}
		}
		h.ServeHTTP(w, r)
	})
	ts := httptest.NewUnstartedServer(mux)
	_ = ts.Listener.Close()
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)

	host := os.Getenv("SHUTTLE_E2E_HOST")
	if host == "" {
		host = "host.docker.internal"
	}
	m.URL = fmt.Sprintf("http://%s:%d/", host, l.Addr().(*net.TCPAddr).Port)
	return m
}

// muffled is one stream's writer: it passes everything through until the
// server mutes the streams open at the time, and then swallows it.
type muffled struct {
	http.ResponseWriter
	gen int64
	now *atomic.Int64
}

func (m *muffled) muted() bool { return m.now.Load() != m.gen }

func (m *muffled) Write(p []byte) (int, error) {
	if m.muted() {
		return len(p), nil
	}
	return m.ResponseWriter.Write(p)
}

func (m *muffled) Flush() {
	if m.muted() {
		return
	}
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *muffled) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// bumpWhileMuted mutes the page's stream and clicks: the action runs on the
// server, its re-render goes down the muted stream, and the page goes on
// showing 1. What brings it to 2 is what the test is about.
func bumpWhileMuted(t *testing.T, app *mutable) playwright.Page {
	t.Helper()
	p := visit(t, app.URL)
	if err := p.Locator(`[data-role="bump"]`).Click(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := expect(p.Locator(`[data-count="1"]`)).ToBeAttached(); err != nil {
		t.Fatalf("never counted: %v", err)
	}
	app.Mute()
	if err := p.Locator(`[data-role="bump"]`).Click(); err != nil {
		t.Fatalf("bump while muted: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if v, _ := p.Evaluate(`() => !!document.querySelector('[data-count="2"]')`); v == true {
		t.Fatal("the muted stream delivered the re-render, so the test proves nothing")
	}
	return p
}

// TestAStalledStreamIsReopened is the watchdog. The server writes at least
// every heartbeat, so a stream silent for well over one has stalled, and
// the shim reopens it; the session survives the reopen and re-sends the
// page, so the click that went down the dead stream shows.
func TestAStalledStreamIsReopened(t *testing.T) {
	app := serveMutable(t, func() shuttle.Component { return &counter{} },
		func(h *shuttle.Handler) { h.Heartbeat = time.Second })
	p := bumpWhileMuted(t, app)
	eventuallySlow(t, "the stalled stream to be reopened", func() bool {
		v, err := p.Evaluate(`() => !!document.querySelector('[data-count="2"]')`)
		return err == nil && v == true
	}, nil)
}

// TestAResumedPageReopensItsStream is the thaw. With the default heartbeat
// the watchdog would wait over a minute; the browser's resume event, which
// it fires when it thaws a frozen page, reopens the stream at once.
func TestAResumedPageReopensItsStream(t *testing.T) {
	app := serveMutable(t, func() shuttle.Component { return &counter{} })
	p := bumpWhileMuted(t, app)
	if _, err := p.Evaluate(`() => document.dispatchEvent(new Event('resume'))`); err != nil {
		t.Fatalf("resume: %v", err)
	}
	eventually(t, "the resumed page to catch up", func() bool {
		v, err := p.Evaluate(`() => !!document.querySelector('[data-count="2"]')`)
		return err == nil && v == true
	})
}
