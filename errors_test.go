package shuttle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
)

var errRender = errors.New("render blew up")

// syncBuffer is a log sink safe to read while the session's own goroutine
// is writing to it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// breaks fails its render once told to, which is how a real component fails:
// fine at first, broken after some state changes under it.
type breaks struct {
	Base
	Broken bool
	Clicks int
}

func (b *breaks) Render(ctx context.Context) templ.Component {
	boom := button.New(
		OnClick(ctx, button.Attr, func(context.Context) error {
			b.Clicks++
			b.Broken = true
			return nil
		}),
	)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if b.Broken {
			return errRender
		}
		if _, err := fmt.Fprintf(w, `<p id="clicks">%d</p>`, b.Clicks); err != nil {
			return err
		}
		return boom.Render(templ.WithChildren(ctx, templ.Raw("break me")), w)
	})
}

// TestRenderErrorOnFirstPaintIsAnHTTPError. Nothing has been sent yet, so
// the request can carry the failure.
func TestRenderErrorOnFirstPaintIsAnHTTPError(t *testing.T) {
	h := New(func() Component { return &breaks{Broken: true} })
	h.Logger = quietLogger()
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
	if n := h.sessions.len(); n != 0 {
		t.Errorf("%d sessions left behind by a failed first render", n)
	}
}

// TestRenderErrorAfterAnActionOnlyReachesTheLog.
//
// This is the honest shape of the current design, not an accident: the
// action itself succeeded, so the POST is a truthful 204. The re-render
// happens afterwards on the session's goroutine, where the request is
// already gone - so the component's error goes to the Logger and the page
// keeps whatever markup it had.
func TestRenderErrorAfterAnActionOnlyReachesTheLog(t *testing.T) {
	var log syncBuffer
	h := New(func() Component { return &breaks{} })
	h.Logger = slog.New(slog.NewTextHandler(&log, nil))
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	// The action runs fine; the render it triggers does not.
	if code := post(t, srv, sid, clickURLs(t, fragment(t, page))[0]); code != http.StatusNoContent {
		t.Fatalf("action: status %d, want 204 - the action itself succeeded", code)
	}

	waitFor(t, time.Second, func() bool {
		return strings.Contains(log.String(), errRender.Error())
	}, "the render error to be logged")

	// Nothing is patched, because there is no markup to patch.
	if !stream.silent(200 * time.Millisecond) {
		t.Error("a failed render sent a patch anyway")
	}

	// And the session is still alive: one bad render is not a dead page.
	if _, ok := h.sessions.get(sid); !ok {
		t.Error("the session died with the render")
	}
}

// guarded renders a fallback instead of failing, which is the error
// boundary the plan calls for: one component's failure costs that
// component, not the page.
type guarded struct{ breaks }

func (g *guarded) RenderError(_ context.Context, err error) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, werr := fmt.Fprintf(w, `<p id="oops">could not render: %s</p>`,
			templ.EscapeString(err.Error()))
		return werr
	})
}

// TestFallbackRendersInsteadOfFailing.
func TestFallbackRendersInsteadOfFailing(t *testing.T) {
	sess := newSession("test", &guarded{breaks{Broken: true}})
	t.Cleanup(func() { sess.close(context.Background()) })

	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("a component with a fallback still failed: %v", err)
	}
	if !strings.Contains(markup, `<p id="oops">could not render: render blew up</p>`) {
		t.Errorf("fallback markup missing: %q", markup)
	}
	// Still a well-formed component: the morph target survives, so the page
	// can recover on the next render.
	if !strings.Contains(markup, `<div id="shuttle-c" data-shuttle="component"`) {
		t.Errorf("fallback lost the morph target: %q", markup)
	}
}

// TestFallbackLeavesNoHalfWrittenMarkup: a component that fails partway
// through must not ship the half it managed.
func TestFallbackLeavesNoHalfWrittenMarkup(t *testing.T) {
	sess := newSession("test", &halfway{})
	t.Cleanup(func() { sess.close(context.Background()) })

	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(markup, "half a component") {
		t.Errorf("partial markup escaped into the patch: %q", markup)
	}
	if !strings.Contains(markup, "oops") {
		t.Errorf("fallback did not run: %q", markup)
	}
}

type halfway struct{ Base }

func (h *halfway) Render(context.Context) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		io.WriteString(w, "<p>half a component</p>")
		return errRender
	})
}

func (h *halfway) RenderError(context.Context, error) templ.Component {
	return templ.Raw(`<p id="oops">oops</p>`)
}

// TestPanicInRenderDoesNotKillTheSession is the one that matters most: the
// session runs everything on one goroutine, so a panic escaping a render
// would take that goroutine with it and leave a page whose every later
// click hangs rather than fails.
func TestPanicInRenderDoesNotKillTheSession(t *testing.T) {
	var log syncBuffer
	c := &exploder{}
	h := New(func() Component { return c })
	h.Logger = slog.New(slog.NewTextHandler(&log, nil))
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)
	url := clickURLs(t, fragment(t, page))[0]

	// This click makes the next render panic.
	if code := post(t, srv, sid, url); code != http.StatusNoContent {
		t.Fatalf("action: status %d", code)
	}
	// Matched against the sentinel rather than a phrase, so rewording the
	// message cannot quietly stop this from checking anything.
	waitFor(t, time.Second, func() bool {
		return strings.Contains(log.String(), ErrPanic.Error())
	}, "the panic to be reported")

	// The session's goroutine is still running: another click still runs its
	// action and re-renders, rather than hanging until the client gives up.
	// This one toggles the panic back off.
	if code := post(t, srv, sid, url); code != http.StatusNoContent {
		t.Fatalf("action after a panicking render: status %d", code)
	}
	if evt := stream.event(t); !strings.Contains(evt, "data: selector #shuttle-c") {
		t.Errorf("session did not recover: %q", evt)
	}
}

// exploder panics in its render once armed.
type exploder struct {
	Base
	Boom bool
}

func (e *exploder) Render(ctx context.Context) templ.Component {
	arm := button.New(
		OnClick(ctx, button.Attr, func(context.Context) error {
			e.Boom = !e.Boom
			return nil
		}),
	)
	boom := e.Boom

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if boom {
			panic("component blew up")
		}
		return arm.Render(templ.WithChildren(ctx, templ.Raw("arm")), w)
	})
}

// TestPanicInAnActionKeepsTheSessionUsable, the same way.
func TestPanicInAnActionKeepsTheSessionUsable(t *testing.T) {
	h := New(func() Component { return &panicker{} })
	h.Logger = quietLogger()
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	url := clickURLs(t, fragment(t, page))[0]

	if code := post(t, srv, sid, url); code != http.StatusInternalServerError {
		t.Fatalf("panicking action: status %d, want 500", code)
	}
	// The 500 came back rather than the request hanging on a dead goroutine.
	if code := post(t, srv, sid, url); code != http.StatusNoContent {
		t.Errorf("action after a panic: status %d, want 204", code)
	}
	if _, ok := h.sessions.get(sid); !ok {
		t.Error("the session died with the action")
	}
}

// TestTheCapabilityNeverReachesTheLog. The session id is what stands
// between a stranger and this page's actions, and the failure path is
// exactly where an identifier gets added without thinking: something went
// wrong, so log everything about it. A log aggregator, an error tracker and
// a screenshot in a bug report are all places a capability must not be.
//
// Session.Tag is what makes this a rule you can follow rather than one you
// have to give up - it tells sessions apart in the log without being the
// thing that unlocks them.
func TestTheCapabilityNeverReachesTheLog(t *testing.T) {
	var log syncBuffer
	h := New(func() Component { return &panicker{} })
	h.Logger = slog.New(slog.NewTextHandler(&log, nil))
	h.Debug = true
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)

	// Fail an action, which is the loudest thing the handler logs.
	if code := post(t, srv, sid, clickURLs(t, fragment(t, page))[0]); code != http.StatusInternalServerError {
		t.Fatalf("panicking action: status %d, want 500", code)
	}
	waitFor(t, time.Second, func() bool {
		return strings.Contains(log.String(), "action failed")
	}, "the action failure to be logged")

	if strings.Contains(log.String(), sid) {
		t.Errorf("the session id is in the log:\n%s", log.String())
	}

	// And the line is still worth having: one session is told from another.
	sess, ok := h.sessions.get(sid)
	if !ok {
		t.Fatal("the session died with the action")
	}
	if tag := sess.Tag(); tag == "" || !strings.Contains(log.String(), tag) {
		t.Errorf("no tag %q to tell this session apart by:\n%s", tag, log.String())
	}
}

// TestTheCapabilityNeverReachesAURL. Redacting shuttle's own log only ever
// fixed the half of this one process controls: a URL is the most copied
// string in a system, and an access log, a proxy and an APM span all record
// one before any of this code runs. So the id is not in a URL at all - it
// travels in a header, and every request the page makes carries it there.
//
// The test reads the page the way the browser does: every URL the markup
// hands the client, from all four kinds of request.
func TestTheCapabilityNeverReachesAURL(t *testing.T) {
	h := New(func() Component { return &gallery{} })
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)

	// Every transport path the page carries, however it is quoted: the
	// stream, the actions, the navigation report, the upload endpoint and
	// the health check.
	paths := regexp.MustCompile(`/_shuttle/[A-Za-z0-9/_.-]*`).FindAllString(page, -1)
	if len(paths) < 4 {
		t.Fatalf("found only %v, expected every transport route in:\n%s", paths, page)
	}
	for _, p := range paths {
		if strings.Contains(p, sid) {
			t.Errorf("the session id is in a URL the page will request: %q", p)
		}
	}

	// It is in the page, of course - the client has to send it from
	// somewhere. What matters is that it is a header when it goes.
	if !strings.Contains(page, SessionHeader) {
		t.Errorf("no %s header for the client to send:\n%s", SessionHeader, page)
	}
	if !strings.Contains(page, sid) {
		t.Error("the page carries no session id at all, so nothing can connect")
	}
}

// TestInternalErrorsDoNotReachTheClient. A failure is exactly when a
// handler is most tempted to say everything it knows, and an HTTP body is
// the one place that must not happen: ErrPanic carries the panic value,
// which is whatever was in scope - a path on disk, a query, a key.
//
// The log still gets all of it, which is the trade: the operator sees the
// cause, the client sees the status.
func TestInternalErrorsDoNotReachTheClient(t *testing.T) {
	var log syncBuffer
	h := New(func() Component { return &panicker{} })
	h.Logger = slog.New(slog.NewTextHandler(&log, nil))
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)

	req, err := http.NewRequest(http.MethodPost,
		srv.URL+clickURLs(t, fragment(t, page))[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SessionHeader, sid)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
	if strings.Contains(string(body), "boom") {
		t.Errorf("the panic value came back to the client: %q", body)
	}
	if strings.Contains(string(body), ErrPanic.Error()) {
		t.Errorf("the response names the failure mode: %q", body)
	}
	// It did not simply vanish, either.
	waitFor(t, time.Second, func() bool {
		return strings.Contains(log.String(), "boom")
	}, "the panic value to reach the log")
}

// TestRefusalsStillSayWhy. Genericising every error would take the useful
// half with it: a refusal is a rule of this package's own that the client
// broke, and it needs to be told which.
func TestRefusalsStillSayWhy(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		want   string
	}{
		"too large":  {ErrFileTooLarge, http.StatusRequestEntityTooLarge, ErrFileTooLarge.Error()},
		"wrong type": {ErrFileType, http.StatusBadRequest, ErrFileType.Error()},
		"wrapped":    {fmt.Errorf("%w: photo.png", ErrFileTooLarge), 413, ErrFileTooLarge.Error()},
		// Not a refusal: the status, and nothing else.
		"internal": {errRender, http.StatusInternalServerError, "Internal Server Error"},
		"panic":    {fmt.Errorf("%w: secrets in here", ErrPanic), 500, "Internal Server Error"},
		"bad":      {errors.New("some parser said this"), http.StatusBadRequest, "bad request"},
	} {
		t.Run(name, func(t *testing.T) {
			got := publicMessage(tc.status, tc.err)
			if got != tc.want {
				t.Errorf("publicMessage = %q, want %q", got, tc.want)
			}
			// Whatever it says, it is never the wrapped detail.
			if strings.Contains(got, "secrets in here") || strings.Contains(got, "photo.png") {
				t.Errorf("detail leaked into %q", got)
			}
		})
	}
}

// TestActionBodiesAreCapped. Signals are decoded into memory whole, so
// without a ceiling one POST is a way to exhaust the process. Navigation
// and uploads already had one; actions did not.
func TestActionBodiesAreCapped(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.MaxSignalBytes = 1 << 10
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	action := clickURLs(t, fragment(t, page))[0]

	big := fmt.Sprintf(`{"c":{"draft":%q}}`, strings.Repeat("x", 4<<10))
	if code := postBody(t, srv, sid, action, big); code != http.StatusBadRequest {
		t.Errorf("an oversized body: status %d, want 400", code)
	}
	// The session is unharmed, and a reasonable body still works.
	if code := postBody(t, srv, sid, action, `{"c":{"draft":"fine"}}`); code != http.StatusNoContent {
		t.Errorf("a normal body after an oversized one: status %d, want 204", code)
	}
}

// TestTransportResponsesAreNotCacheable. Every transport route answers for
// the session in the header, so the same URL is two different answers for
// two pages - a cache keying on the URL alone would be entitled to confuse
// them.
func TestTransportResponsesAreNotCacheable(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)

	req, err := http.NewRequest(http.MethodPost,
		srv.URL+clickURLs(t, fragment(t, page))[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SessionHeader, sid)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Vary"); got != SessionHeader {
		t.Errorf("Vary = %q, want %q", got, SessionHeader)
	}
}

// TestTagIsNotDerivedFromTheID. A truncated hash would also keep the id out
// of the log, and would also be a function of it - which is an argument
// about how many characters are safe rather than a design with no such
// question in it. Minting the tag separately is what removes the question.
func TestTagIsNotDerivedFromTheID(t *testing.T) {
	a := newSession("same-id", &counter{})
	b := newSession("same-id", &counter{})
	t.Cleanup(func() {
		a.close(context.Background())
		b.close(context.Background())
	})

	if a.Tag() == b.Tag() {
		t.Errorf("two sessions with the same id share a tag (%q), so the tag is derived from it", a.Tag())
	}
	if strings.Contains(a.Tag(), a.ID()) || strings.Contains(a.ID(), a.Tag()) {
		t.Errorf("tag %q and id %q overlap", a.Tag(), a.ID())
	}
}

// TestRenderErrorReachesTheHandlerLogger, so it is never swallowed
// silently even though it cannot reach the client.
func TestRenderErrorReachesTheHandlerLogger(t *testing.T) {
	var log syncBuffer
	c := &breaks{}
	h := New(func() Component { return c })
	h.Logger = slog.New(slog.NewTextHandler(&log, nil))
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)

	if err := c.Do(func(context.Context) error {
		c.Broken = true
		return nil
	}); err != nil {
		t.Fatalf("do: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return strings.Contains(log.String(), errRender.Error())
	}, "the render error to be logged")

	if !strings.Contains(log.String(), "render failed") {
		t.Errorf("log does not say what failed:\n%s", log.String())
	}
	if _, ok := h.sessions.get(sid); !ok {
		t.Error("the session died with the render")
	}
}
