package shuttle

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/field"
	"github.com/pietjan/loom/input"
)

// counter is the milestone-1 component: some state, and two actions bound
// as closures over it.
type counter struct {
	Base
	Count int
}

func (c *counter) Render(ctx context.Context) templ.Component {
	inc := button.New(
		button.Primary,
		OnClick(ctx, button.Attr, func(context.Context) error {
			c.Count++
			return nil
		}),
	)
	reset := button.New(
		button.Outline,
		OnClick(ctx, button.Attr, func(context.Context) error {
			c.Count = 0
			return nil
		}),
	)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := fmt.Fprintf(w, `<output id="count">%d</output>`, c.Count); err != nil {
			return err
		}
		if err := inc.Render(templ.WithChildren(ctx, templ.Raw("+1")), w); err != nil {
			return err
		}
		return reset.Render(templ.WithChildren(ctx, templ.Raw("reset")), w)
	})
}

// form is a component whose markup contains Loom-generated element ids.
// Those ids are the morph's reconciliation key, so they are what the
// determinism test is really about.
type form struct {
	Base
	Err string
}

func (f *form) Render(ctx context.Context) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return field.Root(field.Error(f.Err)).
			Render(templ.WithChildren(ctx, input.New()), w)
	})
}

// panicker fails the way a real handler bug does.
type panicker struct {
	Base
	Calls int
}

func (p *panicker) Render(ctx context.Context) templ.Component {
	return button.New(
		OnClick(ctx, button.Attr, func(context.Context) error {
			p.Calls++
			if p.Calls == 1 {
				panic("boom")
			}
			return nil
		}),
	)
}

// --- helpers ---------------------------------------------------------------

var (
	sessionRE = regexp.MustCompile(`/_shuttle/live/([0-9a-f]+)`)
	// The expression carries fetch options after the URL, so this stops at
	// the closing quote of the path rather than the closing paren.
	clickRE = regexp.MustCompile(`data-on:click="@post\(&#39;([^&]+)&#39;`)
	// genRE normalises the generation half of an action id, so two renders
	// of the same state can be compared byte for byte.
	genRE = regexp.MustCompile(`/[0-9]+-a([0-9]+)`)
)

func normalise(markup string) string { return genRE.ReplaceAllString(markup, "/N-a$1") }

// clickURLs returns the endpoints the markup's click handlers post to, in
// document order - the same URLs a browser would use.
func clickURLs(t *testing.T, markup string) []string {
	t.Helper()
	var urls []string
	for _, m := range clickRE.FindAllStringSubmatch(markup, -1) {
		urls = append(urls, m[1])
	}
	if len(urls) == 0 {
		t.Fatalf("no click bindings in %q", markup)
	}
	return urls
}

// fragment extracts the component's markup from a rendered page.
func fragment(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, `<div id="shuttle-`)
	end := strings.Index(page, "\n</body>")
	if start < 0 || end < start {
		t.Fatalf("no component fragment in page %q", page)
	}
	return page[start:end]
}

func getPage(t *testing.T, srv *httptest.Server) (page, sid string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET page: status %d", resp.StatusCode)
	}

	m := sessionRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("page carries no session id: %q", body)
	}
	return string(body), m[1]
}

// sseReader reads one SSE event block at a time without letting a stalled
// stream hang the test run.
type sseReader struct {
	lines chan string
	errs  chan error
	close func()
}

func openStream(t *testing.T, srv *httptest.Server, sid string) *sseReader {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+routePrefix+"/live/"+sid, nil)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("open stream: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("stream content type = %q", ct)
	}

	r := newSSEReader(resp.Body)
	t.Cleanup(r.close)
	r.greeting(t)
	return r
}

// greeting consumes the empty signal patch a stream opens with, so a test
// reading "the first event" gets the one it meant. The greeting itself has
// its own test - it is what tells a reconnected page it is connected again
// without waiting for a heartbeat.
func (r *sseReader) greeting(t *testing.T) {
	t.Helper()
	if evt := r.event(t); !strings.Contains(evt, "datastar-patch-signals") {
		t.Fatalf("a stream opened with %q, want the greeting", evt)
	}
}

// newSSEReader pumps an event stream into channels, so a stalled stream
// fails a test by timing out rather than hanging the run.
func newSSEReader(body io.ReadCloser) *sseReader {
	r := &sseReader{
		lines: make(chan string, 64),
		errs:  make(chan error, 1),
		close: func() { body.Close() },
	}
	go func() {
		br := bufio.NewReader(body)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				r.lines <- strings.TrimRight(line, "\r\n")
			}
			if err != nil {
				r.errs <- err
				return
			}
		}
	}()
	return r
}

// event returns the next complete event block.
func (r *sseReader) event(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-r.lines:
			if line == "" {
				if b.Len() > 0 {
					return b.String()
				}
				continue
			}
			b.WriteString(line)
			b.WriteString("\n")
		case err := <-r.errs:
			t.Fatalf("stream closed early: %v (partial %q)", err, b.String())
		case <-deadline:
			t.Fatalf("timed out waiting for an event (partial %q)", b.String())
		}
	}
}

// silent reports whether the stream stays quiet, which is what attaching to
// an already-rendered page should do.
func (r *sseReader) silent(d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case line := <-r.lines:
			// Blank lines are frame separators; a leftover one from the
			// previous event is not the stream saying anything new.
			if strings.TrimSpace(line) == "" {
				continue
			}
			return false
		case <-deadline:
			return true
		}
	}
}

func post(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("action request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post action: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// --- tests -----------------------------------------------------------------

// TestPageWorksBeforeAnyScriptRuns covers phase one of the two-phase
// render: a complete document, rendered on the server, that already shows
// the component's state.
func TestPageWorksBeforeAnyScriptRuns(t *testing.T) {
	srv := httptest.NewServer(New(func() Component { return &counter{Count: 7} }))
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)

	for _, want := range []string{
		"<!doctype html>",
		`<script type="module" src="` + DefaultScriptURL + `"></script>`,
		`<div id="shuttle-c" data-shuttle="component">`,
		`<output id="count">7</output>`,
		`data-ui="button"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q:\n%s", want, page)
		}
	}
}

// TestAttachOptionsAreNotOptional guards the settings a persistent stream
// is opened with.
//
// retry: 'always' is mandatory - the default 'auto' does not reconnect
// after a clean close. openWhenHidden defaults to true, which is Datastar's
// own guidance and what a framework holding state per page needs: with it
// off, a hidden tab's stream closes and Grace later evicts the session, so
// returning to that tab reloads the page and loses it.
func TestAttachOptionsAreNotOptional(t *testing.T) {
	srv := httptest.NewServer(New(func() Component { return &counter{} }))
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)

	// data-init, because Datastar v1 has no load event; on <body>, because
	// anything inside the morph target would re-initialise on every patch.
	want := fmt.Sprintf(
		`<body data-init="@get(&#39;/_shuttle/live/%s&#39;, {openWhenHidden: true, retry: &#39;always&#39;})">`,
		sid,
	)
	if !strings.Contains(page, want) {
		t.Errorf("attach expression wrong.\nwant %s\nin:\n%s", want, page)
	}
}

// TestOpenWhenHiddenCanBeTurnedOff. It is a pointer so that false is
// expressible: a deployment stuck on HTTP/1.1 with many tabs open may want
// hidden pages to give their connection back, at the cost of the session
// behind them once Grace expires.
func TestOpenWhenHiddenCanBeTurnedOff(t *testing.T) {
	off := false
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.OpenWhenHidden = &off
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)
	if !strings.Contains(page, "openWhenHidden: false") {
		t.Errorf("OpenWhenHidden was not honoured: %q", page)
	}
	// retry stays mandatory either way.
	if !strings.Contains(page, "retry: &#39;always&#39;") {
		t.Errorf("retry is not 'always': %q", page)
	}
}

// TestGraceIsConfigurable, because how long a session should outlive its
// stream depends on how long a tab might sit in the background.
func TestGraceIsConfigurable(t *testing.T) {
	h := New(func() Component { return &counter{} })
	if got := h.grace(); got != DefaultGrace {
		t.Errorf("unset grace = %v, want %v", got, DefaultGrace)
	}

	h.Grace = 5 * time.Minute
	if got := h.grace(); got != 5*time.Minute {
		t.Errorf("grace = %v, want 5m", got)
	}
}

// TestPageIsNotCacheable: the document embeds the session id, which is the
// page's capability for every action it renders.
func TestPageIsNotCacheable(t *testing.T) {
	srv := httptest.NewServer(New(func() Component { return &counter{} }))
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestDeadAndLiveRendersMatch is the most valuable test in the project.
//
// The first morph replaces markup the server rendered into the page with
// markup the session renders standalone. If those two disagree on a single
// element id, the morph stops recognising the elements it is supposed to be
// preserving and throws away focus and scroll. Comparing whole bytes is
// how id drift gets caught, rather than discovered by hand later.
//
// Action ids legitimately carry a render generation, so they are normalised
// out; element ids are compared exactly.
func TestDeadAndLiveRendersMatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func() Component
	}{
		{"plain", func() Component { return &counter{Count: 3} }},
		// field emits three Loom-generated ids per instance, which is where
		// drift between a page render and a fragment render shows up.
		{"loom ids", func() Component { return &form{Err: "required"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(tc.make)
			srv := httptest.NewServer(h)
			// Registered rather than deferred: a stream is a long-lived request, and
			// Close waits for it. Cleanups run last-in-first-out, so any stream
			// opened below is closed before the server starts waiting.
			t.Cleanup(srv.Close)

			page, sid := getPage(t, srv)
			dead := fragment(t, page)

			sess, ok := h.sessions.get(sid)
			if !ok {
				t.Fatal("session not registered by the page render")
			}
			live, err := sess.Render(context.Background())
			if err != nil {
				t.Fatalf("live render: %v", err)
			}

			if normalise(dead) != normalise(live) {
				t.Errorf("dead and live renders differ:\ndead %s\nlive %s", dead, live)
			}
			if !strings.Contains(dead, `id="`+sess.RootID()+`"`) {
				t.Errorf("fragment lost its morph target: %s", dead)
			}
		})
	}
}

// TestLoomIDsAreStableAcrossRenders states the invariant on its own, so a
// failure says what broke rather than just "the bytes differ".
func TestLoomIDsAreStableAcrossRenders(t *testing.T) {
	sess := newSession("test", &form{})
	ctx := context.Background()

	first, err := sess.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := sess.Render(ctx)
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}

	ids := regexp.MustCompile(`id="(loom-[^"]+)"`)
	a, b := ids.FindAllStringSubmatch(first, -1), ids.FindAllStringSubmatch(second, -1)
	if len(a) == 0 {
		t.Fatalf("expected Loom-generated ids in %q", first)
	}
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("Loom ids drifted between renders:\nfirst %v\nsecond %v", a, b)
	}
}

// TestCounterRoundTrip is the architecture, end to end and headless: a page
// is served, a stream attaches to it, a click reaches a closure holding
// server-side state, and the morph comes back down the stream.
func TestCounterRoundTrip(t *testing.T) {
	srv := httptest.NewServer(New(func() Component { return &counter{} }))
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	// Attaching must not patch anything: the DOM the client already has is
	// the render the session just did.
	if !stream.silent(150 * time.Millisecond) {
		t.Error("attaching to an already-rendered page sent a patch")
	}

	markup := fragment(t, page)
	for want := 1; want <= 3; want++ {
		// Post to the increment endpoint the current markup names, exactly
		// as a browser would - which also proves the action ids in each
		// morph are the ones the next click can use.
		if code := post(t, srv, clickURLs(t, markup)[0]); code != http.StatusNoContent {
			t.Fatalf("click %d: status %d, want 204", want, code)
		}

		evt := stream.event(t)
		if !strings.Contains(evt, "event: datastar-patch-elements") {
			t.Fatalf("click %d: not an element patch: %q", want, evt)
		}
		if !strings.Contains(evt, "data: selector #shuttle-c") {
			t.Fatalf("click %d: patch does not target the component root: %q", want, evt)
		}
		if !strings.Contains(evt, fmt.Sprintf(`<output id="count">%d</output>`, want)) {
			t.Fatalf("click %d: state not in patch: %q", want, evt)
		}
		markup = evt
	}

	// The second binding is reset, so state really is held server-side
	// across independent requests rather than echoed back by the client.
	if code := post(t, srv, clickURLs(t, markup)[1]); code != http.StatusNoContent {
		t.Fatalf("reset: status %d", code)
	}
	if evt := stream.event(t); !strings.Contains(evt, `<output id="count">0</output>`) {
		t.Errorf("reset did not take: %q", evt)
	}
}

// TestServerPushReachesThePage covers the unprompted path: a change made by
// an application goroutine, with no client event to answer. This is the
// reason to hold state on the server at all, and it is what pub/sub,
// presence and timers will all be built on.
func TestServerPushReachesThePage(t *testing.T) {
	c := &counter{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	// Exactly what an application goroutine would do. Do runs the mutation on
	// the session's goroutine, so it cannot land in the middle of an action,
	// and re-renders afterwards.
	if err := c.Do(func(context.Context) error {
		c.Count = 42
		return nil
	}); err != nil {
		t.Fatalf("do: %v", err)
	}

	evt := stream.event(t)
	if !strings.Contains(evt, `<output id="count">42</output>`) {
		t.Errorf("push did not reach the page: %q", evt)
	}
	if !strings.Contains(evt, "data: selector #shuttle-c") {
		t.Errorf("push did not target the component root: %q", evt)
	}
}

// TestPushBeforeAttachIsNotLost: a push landing in the gap between the page
// render and the stream opening - or during a reconnect - has to be waiting
// when the stream arrives, not dropped.
func TestPushBeforeAttachIsNotLost(t *testing.T) {
	c := &counter{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)

	if err := c.Do(func(context.Context) error {
		c.Count = 99
		return nil
	}); err != nil {
		t.Fatalf("do: %v", err)
	}

	stream := openStream(t, srv, sid)
	if evt := stream.event(t); !strings.Contains(evt, `<output id="count">99</output>`) {
		t.Errorf("push made before the stream attached was lost: %q", evt)
	}
}

// TestStaleClickRunsItsOwnClosure covers the race the generation prefix
// exists for: a click sent against markup that a morph has already
// replaced. It must run the closure it was rendered for, and never
// whichever handler now occupies that position.
func TestStaleClickRunsItsOwnClosure(t *testing.T) {
	c := &counter{}
	sess := newSession("test", c)
	ctx := context.Background()

	first, err := sess.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	staleReset := lastPathSegment(clickURLs(t, first)[1])

	// A render happens, so the client's markup is now one generation stale.
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}
	c.Count = 5

	if err := sess.Invoke(ctx, "c", staleReset); err != nil {
		t.Fatalf("stale click rejected: %v", err)
	}
	if c.Count != 0 {
		t.Errorf("stale click ran the wrong closure: count = %d, want 0", c.Count)
	}

	// Two generations on, it is genuinely gone rather than misrouted.
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if err := sess.Invoke(ctx, "c", staleReset); !errors.Is(err, ErrNoAction) {
		t.Errorf("long-stale click: err = %v, want ErrNoAction", err)
	}
}

func lastPathSegment(url string) string {
	return url[strings.LastIndex(url, "/")+1:]
}

// TestUnknownSessionAndAction: both are routine, not exceptional - an
// evicted session and a click on markup left in a background tab.
func TestUnknownSessionAndAction(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)

	if code := post(t, srv, routePrefix+"/act/deadbeef/c/1-a1"); code != http.StatusNotFound {
		t.Errorf("unknown session: status %d, want 404", code)
	}
	if code := post(t, srv, routePrefix+"/act/"+sid+"/c/9-a9"); code != http.StatusNotFound {
		t.Errorf("unknown action: status %d, want 404", code)
	}
	if code := post(t, srv, routePrefix+"/act/"+sid+"/c-7/1-a1"); code != http.StatusNotFound {
		t.Errorf("unknown component: status %d, want 404", code)
	}

	// A stream for a session this server does not have gets a real stream
	// carrying a reload, not a 404. Datastar would retry a 404 forever, and
	// no amount of retrying brings back state that lived in memory the
	// server no longer has - after a restart, starting over is the only way
	// back.
	resp, err := srv.Client().Get(srv.URL + routePrefix + "/live/deadbeef")
	if err != nil {
		t.Fatalf("stream for unknown session: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("stream for unknown session: status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "location.reload()") {
		t.Errorf("stream for unknown session did not send the page home: %q", body)
	}
}

// TestSecondStreamIsRejected: one stream per page. A second would give the
// session two writers and lose the ordering guarantee that makes the single
// stream worth having.
func TestSecondStreamIsRejected(t *testing.T) {
	h := New(func() Component { return &counter{} })
	srv := httptest.NewServer(h)
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	openStream(t, srv, sid)

	resp, err := srv.Client().Get(srv.URL + routePrefix + "/live/" + sid)
	if err != nil {
		t.Fatalf("second stream: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second stream: status %d, want 409", resp.StatusCode)
	}
}

// TestActionPanicKeepsTheSessionAlive: a handler bug should cost the client
// a failed click, not the page. The component tree and the stream are both
// still there afterwards.
func TestActionPanicKeepsTheSessionAlive(t *testing.T) {
	h := New(func() Component { return &panicker{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)
	url := clickURLs(t, fragment(t, page))[0]

	if code := post(t, srv, url); code != http.StatusInternalServerError {
		t.Fatalf("panicking action: status %d, want 500", code)
	}
	if _, ok := h.sessions.get(sid); !ok {
		t.Fatal("session died with the action")
	}

	// The same id still resolves, because the failed action never triggered
	// a re-render, and this time the handler succeeds.
	if code := post(t, srv, url); code != http.StatusNoContent {
		t.Errorf("action after panic: status %d, want 204", code)
	}
	if evt := stream.event(t); !strings.Contains(evt, "event: datastar-patch-elements") {
		t.Errorf("no patch after recovery: %q", evt)
	}
}

// TestSessionOutlivesItsStream: Datastar reconnects on its own, so state
// has to survive the gap - but not forever, or a closed tab is a memory
// leak.
func TestSessionOutlivesItsStream(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.sessions.grace = 40 * time.Millisecond
	srv := httptest.NewServer(h)
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)
	stream.close()

	// Within the grace window the component is still there, and a second
	// stream can take over where the first left off.
	waitFor(t, time.Second, func() bool {
		sess, ok := h.sessions.get(sid)
		return ok && !sess.isAttached()
	}, "stream to be released")

	if _, ok := h.sessions.get(sid); !ok {
		t.Fatal("session evicted the moment its stream dropped")
	}
	openStream(t, srv, sid)
}

// TestUnattachedSessionIsCollected: a page loaded and abandoned - a crawler,
// a client with script disabled - must not pin a component tree in memory.
func TestUnattachedSessionIsCollected(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.sessions.attach = 30 * time.Millisecond
	srv := httptest.NewServer(h)
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	waitFor(t, time.Second, func() bool {
		_, ok := h.sessions.get(sid)
		return !ok
	}, "unattached session to be collected")
}

// TestUnmountRunsOnEviction.
func TestUnmountRunsOnEviction(t *testing.T) {
	c := &closer{unmounted: make(chan struct{})}
	h := New(func() Component { return c })
	h.sessions.attach = 20 * time.Millisecond
	srv := httptest.NewServer(h)
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	waitFor(t, time.Second, func() bool {
		_, ok := h.sessions.get(sid)
		return !ok
	}, "session to be collected")
	waitFor(t, time.Second, c.done, "Unmount to run")
}

type closer struct {
	counter
	unmounted chan struct{}
}

func (c *closer) Unmount(context.Context) {
	if c.unmounted == nil {
		return
	}
	close(c.unmounted)
}

func (c *closer) done() bool {
	select {
	case <-c.unmounted:
		return true
	default:
		return false
	}
}

// TestMountSeesParams.
func TestMountSeesParams(t *testing.T) {
	srv := httptest.NewServer(New(func() Component { return &mounted{} }))
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/?start=12")
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `<output id="count">12</output>`) {
		t.Errorf("Mount did not see the params: %s", body)
	}
}

type mounted struct{ counter }

func (m *mounted) Mount(_ context.Context, p Params) error {
	_, err := fmt.Sscanf(p.Get("start"), "%d", &m.Count)
	return err
}

// TestBindingOutsideASessionIsInert: a shuttle component rendered by
// something that is not a session has nowhere to register a closure, so it
// must not emit a handler attribute pointing at an action that does not
// exist.
func TestBindingOutsideASessionIsInert(t *testing.T) {
	c := &counter{}
	got := render(t, c.Render(context.Background()))

	if strings.Contains(got, "data-on:click") {
		t.Errorf("bound a handler with no session to register it in: %q", got)
	}
	if !strings.Contains(got, `data-ui="button"`) {
		t.Errorf("component did not render at all: %q", got)
	}
}

// TestPushCoalesces: patches carry the region's complete state, so a client
// that cannot keep up should skip intermediate renders rather than queue
// them. The newest render always wins.
func TestPushCoalesces(t *testing.T) {
	c := &counter{}
	sess := newSession("test", c)
	ctx := context.Background()
	t.Cleanup(func() { sess.close(ctx) })

	// Five pushes inside one piece of work. Push marks the component dirty
	// rather than rendering, so the session collapses them into a single
	// render of the newest state.
	if err := c.Do(func(ctx context.Context) error {
		for range 5 {
			c.Count++
			if err := c.Push(ctx); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("do: %v", err)
	}

	var got []patch
	waitFor(t, time.Second, func() bool {
		got = append(got, sess.take()...)
		return len(got) > 0
	}, "the coalesced render")

	if len(got) != 1 {
		t.Fatalf("five pushes produced %d patches, want 1", len(got))
	}
	if !strings.Contains(got[0].html, `<output id="count">5</output>`) {
		t.Errorf("render is not the newest state: %q", got[0].html)
	}
}

// TestPushAfterCloseFails.
func TestPushAfterCloseFails(t *testing.T) {
	sess := newSession("test", &counter{})
	sess.close(context.Background())

	if err := sess.Push(context.Background()); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("push after close: err = %v, want ErrSessionClosed", err)
	}
}

// TestPushWithoutASession: Base's methods have to fail cleanly on a
// component nobody mounted, because that is the shape of a unit test.
func TestPushWithoutASession(t *testing.T) {
	c := &counter{}
	if err := c.Push(context.Background()); !errors.Is(err, ErrNotMounted) {
		t.Errorf("err = %v, want ErrNotMounted", err)
	}
}

// TestSessionCapIsEnforced: without a cap, opening pages is a
// memory-exhaustion primitive.
func TestSessionCapIsEnforced(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.sessions.max = 2
	srv := httptest.NewServer(h)
	// Registered rather than deferred: a stream is a long-lived request, and
	// Close waits for it. Cleanups run last-in-first-out, so any stream
	// opened below is closed before the server starts waiting.
	t.Cleanup(srv.Close)

	for range 2 {
		getPage(t, srv)
	}

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("over the cap: status %d, want 503", resp.StatusCode)
	}
}

// quietLogger keeps the tests that provoke errors on purpose from writing
// them to the run's output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitFor(t *testing.T, within time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
