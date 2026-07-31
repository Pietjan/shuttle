//go:build e2e

// Browser end-to-end tests: the things only a browser can answer - whether a
// popover dismisses itself, whether a morph keeps what was typed, whether a
// streamed item survives its component re-rendering. Each was verified once
// by hand and then written down in a markdown file, which is a note that
// cannot fail when the behaviour does.
//
// The handler runs in-process on the host; the browser runs in the official
// playwright image as a browser server the tests connect to over WebSocket.
// Run via `make test/e2e`.
package e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/playwright-community/playwright-go"

	"github.com/pietjan/shuttle"
)

// stylesheet is the sheet `make site/css` compiles. Most of these tests do
// not need it - behaviour is behaviour unstyled - but layout is CSS, so a
// test that measures one has to serve the other. `make test/e2e` builds it
// first.
const stylesheet = "../example/site/static/styles.css"

// styled serves that sheet and links it into the page.
func styled(h *shuttle.Handler) {
	h.Head = `<link rel="stylesheet" href="/styles.css">`
}

// serve mounts cmp and returns its URL as seen from inside the container.
//
// The listener is on every interface rather than the loopback: the browser
// is in a container and comes back through host.docker.internal, so a
// httptest.Server on 127.0.0.1 is invisible to it - which presents as a page
// that never loads rather than as a connection refused.
func serve(t *testing.T, cmp func() shuttle.Component, opts ...func(*shuttle.Handler)) (string, *httptest.Server) {
	t.Helper()

	h := shuttle.New(cmp)
	h.Title = "e2e"
	for _, opt := range opts {
		opt(h)
	}

	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	// The stylesheet route sits beside the handler rather than inside it:
	// shuttle owns its own paths, and an app's assets are the app's.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /styles.css", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, stylesheet)
	})
	mux.Handle("/", h)

	ts := httptest.NewUnstartedServer(mux)
	_ = ts.Listener.Close()
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)

	host := os.Getenv("SHUTTLE_E2E_HOST")
	if host == "" {
		host = "host.docker.internal"
	}
	return fmt.Sprintf("http://%s:%d/", host, l.Addr().(*net.TCPAddr).Port), ts
}

// restartable serves cmp behind a handler that can be swapped for a fresh
// one, which is what a deploy looks like from a page's point of view: the
// process it was talking to is gone and its session with it.
// recorder remembers the status a request got, so a test can say what the
// server answered rather than guessing from the page.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type restartable struct {
	Statuses func() []string
	URL      string
	swap     func()
	drop     func()
	current  func() *shuttle.Handler
}

// Restart replaces the handler and cuts the connections, so the page
// reconnects to a server that has never heard of its session.
func (r *restartable) Restart() { r.swap(); r.drop() }

// Drop cuts the connections without forgetting anything - the reconnect
// finds its own session still there.
func (r *restartable) Drop() { r.drop() }

func serveRestartable(t *testing.T, cmp func() shuttle.Component, opts ...func(*shuttle.Handler)) *restartable {
	t.Helper()

	build := func() *shuttle.Handler {
		h := shuttle.New(cmp)
		h.Title = "e2e"
		for _, opt := range opts {
			opt(h)
		}
		return h
	}

	var current atomic.Pointer[shuttle.Handler]
	current.Store(build())

	statuses := &struct {
		sync.Mutex
		seen []string
	}{}

	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /styles.css", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, stylesheet)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w, status: 200}
		current.Load().ServeHTTP(rec, r)
		statuses.Lock()
		statuses.seen = append(statuses.seen, fmt.Sprintf("%s %s -> %d",
			r.Method, r.URL.Path, rec.status))
		statuses.Unlock()
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
	return &restartable{
		Statuses: func() []string {
			statuses.Lock()
			defer statuses.Unlock()
			return append([]string(nil), statuses.seen...)
		},
		URL:     fmt.Sprintf("http://%s:%d/", host, l.Addr().(*net.TCPAddr).Port),
		swap:    func() { current.Store(build()) },
		drop:    ts.CloseClientConnections,
		current: current.Load,
	}
}

func bootBrowser(t *testing.T) playwright.Page {
	t.Helper()
	ws := os.Getenv("SHUTTLE_E2E_WS")
	if ws == "" {
		t.Skip("SHUTTLE_E2E_WS not set; run via make test/e2e")
	}

	// Browsers live in the container; only the protocol driver is local.
	if err := playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true}); err != nil {
		t.Fatalf("install playwright driver: %v", err)
	}
	pw, err := playwright.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pw.Stop() })

	// The browser server may still be starting inside the container.
	var browser playwright.Browser
	deadline := time.Now().Add(60 * time.Second)
	for {
		browser, err = pw.Chromium.Connect(ws)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect to playwright server at %s: %v", ws, err)
		}
		time.Sleep(time.Second)
	}
	t.Cleanup(func() { _ = browser.Close() })

	page, err := browser.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// open serves the component and loads it, waiting for the stream rather than
// for a duration: shuttle's own shim puts the state on <html>, so the page
// says when it is live.
func open(t *testing.T, cmp func() shuttle.Component, opts ...func(*shuttle.Handler)) playwright.Page {
	t.Helper()
	url, _ := serve(t, cmp, opts...)
	return visit(t, url)
}

// needsStylesheet skips a layout test when the sheet has not been compiled,
// rather than measuring an unstyled page and reporting nonsense.
func needsStylesheet(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(stylesheet); err != nil {
		t.Skipf("no compiled stylesheet at %s - run `make site/css`", stylesheet)
	}
}

// visit opens another page on a server that is already running, which is
// what a test of server push needs: two pages, one handler, one broker.
func visit(t *testing.T, url string) playwright.Page {
	t.Helper()
	page := bootBrowser(t)

	if _, err := page.Goto(url); err != nil {
		t.Fatalf("goto %s: %v", url, err)
	}
	if err := expect(page.Locator("html.shuttle-connected")).ToBeAttached(); err != nil {
		t.Fatalf("page never connected: %v", err)
	}
	return page
}

// expect is playwright's own assertion API, which retries until its timeout -
// the right shape for a page whose updates arrive down a stream rather than
// as the answer to the click.
func expect(l playwright.Locator) playwright.LocatorAssertions {
	return playwright.NewPlaywrightAssertions(5000).Locator(l)
}

// expectPage is the same assertions for the page itself - the URL, the title.
func expectPage(p playwright.Page) playwright.PageAssertions {
	return playwright.NewPlaywrightAssertions(5000).Page(p)
}

// eventuallySlow is eventually with the patience a reconnect needs:
// Datastar backs off between retries, so this waits on a timer rather than
// on a round trip. onFail runs before the failure, to report what was seen -
// which is what turned "it never reconnects" into "it reconnected at once
// and the page was not told".
func eventuallySlow(t *testing.T, what string, fn func() bool, onFail func()) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if onFail != nil {
		onFail()
	}
	t.Fatalf("timed out waiting for %s", what)
}

// eventually retries a condition that is not about an element: a field on the
// component, a measurement taken in the page.
func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
