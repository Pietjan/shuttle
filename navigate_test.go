package shuttle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
)

// table is the case URL binding exists for: a filtered, sorted view whose
// address can be shared and bookmarked.
type table struct {
	Base
	Filter string
	Sort   string
	Params int // how many times HandleParams ran
}

func (t *table) HandleParams(_ context.Context, p Params) error {
	t.Filter, t.Sort = p.Get("filter"), p.Get("sort")
	t.Params++
	return nil
}

func (t *table) QueryParams() url.Values {
	q := url.Values{}
	if t.Filter != "" {
		q.Set("filter", t.Filter)
	}
	if t.Sort != "" {
		q.Set("sort", t.Sort)
	}
	return q
}

func (t *table) Render(ctx context.Context) templ.Component {
	// Filtering is a patch: same component, new parameters.
	apply := button.New(
		OnClick(ctx, button.Attr, func(actx context.Context) error {
			t.Filter = "open"
			return nil
		}),
	)
	// Going somewhere else entirely is a redirect.
	leave := button.New(
		OnClick(ctx, button.Attr, func(actx context.Context) error {
			return t.Redirect(actx, "/login")
		}),
	)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := fmt.Fprintf(w, `<p id="state">%s/%s</p>`, t.Filter, t.Sort); err != nil {
			return err
		}
		if err := apply.Render(templ.WithChildren(ctx, templ.Raw("apply")), w); err != nil {
			return err
		}
		return leave.Render(templ.WithChildren(ctx, templ.Raw("leave")), w)
	})
}

// scripts pulls the script bodies out of the patches waiting to be sent.
func scripts(ps []patch) []string {
	var out []string
	for _, p := range ps {
		if p.script != "" {
			out = append(out, p.script)
		}
	}
	return out
}

// TestHandleParamsRunsOnFirstRender, so a component reading the URL needs
// only the one hook rather than duplicating itself into Mount.
func TestHandleParamsRunsOnFirstRender(t *testing.T) {
	c := &table{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/?filter=closed&sort=age")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), `<p id="state">closed/age</p>`) {
		t.Errorf("params not applied to the first render: %s", body)
	}
	if c.Params != 1 {
		t.Errorf("HandleParams ran %d times, want 1", c.Params)
	}
}

// TestURLFollowsState: a filter applied by an action has to reach the
// address bar, or the view cannot be shared.
func TestURLFollowsState(t *testing.T) {
	c := &table{}
	sess := newSession("test", c)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	sess.url = sess.path(nil)

	if err := c.Do(func(context.Context) error {
		c.Filter = "open"
		return nil
	}); err != nil {
		t.Fatalf("do: %v", err)
	}

	var got []string
	waitFor(t, time.Second, func() bool {
		got = append(got, scripts(sess.take())...)
		return len(got) > 0
	}, "the history call")

	// replaceState, not pushState: a re-render is not a navigation, and a
	// filter box should not fill the back button with every keystroke.
	if !strings.Contains(got[0], "history.replaceState") {
		t.Errorf("history call = %q, want replaceState", got[0])
	}
	if !strings.Contains(got[0], "filter=open") {
		t.Errorf("history call does not carry the state: %q", got[0])
	}
}

// TestURLDoesNotMoveWhenNothingChanged: re-renders are constant, and each
// one must not rewrite an address bar that is already correct.
func TestURLDoesNotMoveWhenNothingChanged(t *testing.T) {
	c := &table{Filter: "open"}
	sess := newSession("test", c)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	sess.url = sess.path(c.QueryParams())

	for range 3 {
		if err := c.Do(func(context.Context) error { return nil }); err != nil {
			t.Fatalf("do: %v", err)
		}
	}

	// Queue one more item behind them: the mailbox is first-in-first-out, so
	// when this returns the three re-renders have happened.
	if err := sess.call(ctx, func() error { return nil }); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if got := scripts(sess.take()); got != nil {
		t.Errorf("re-rendering moved the address bar: %v", got)
	}
}

// TestNavigateAndReplace differ only in whether the back button remembers.
func TestNavigateAndReplace(t *testing.T) {
	c := &table{}
	sess := newSession("test", c)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}

	// On the session's goroutine, because Navigate and Replace run
	// HandleParams synchronously on whichever goroutine calls them - and
	// HandleParams writes component fields, which the session is rendering
	// from. An action is already there; a test is not.
	if err := sess.call(ctx, func() error { return c.Navigate(ctx, "/?filter=mine") }); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := sess.call(ctx, func() error {
		if c.Filter != "mine" {
			t.Errorf("Navigate did not apply the params: filter = %q", c.Filter)
		}
		return c.Replace(ctx, "/?filter=theirs")
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	settle(t, sess)
	got := scripts(sess.take())
	if len(got) < 2 {
		t.Fatalf("got %d history calls, want at least 2: %v", len(got), got)
	}
	if !strings.Contains(got[0], "history.pushState") || !strings.Contains(got[0], "filter=mine") {
		t.Errorf("first call = %q, want a pushState to filter=mine", got[0])
	}
	if !strings.Contains(got[1], "history.replaceState") {
		t.Errorf("second call = %q, want replaceState", got[1])
	}
}

// TestRedirectLeavesTheSession: a full page load, which is what Datastar's
// own Redirect does, and the only one of the three that throws the session
// away.
func TestRedirectLeavesTheSession(t *testing.T) {
	c := &table{}
	sess := newSession("test", c)
	t.Cleanup(func() { sess.close(context.Background()) })

	if _, err := sess.Render(context.Background()); err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := sess.call(context.Background(), func() error {
		return c.Redirect(context.Background(), "/login")
	}); err != nil {
		t.Fatalf("redirect: %v", err)
	}

	settle(t, sess)
	got := scripts(sess.take())
	if len(got) != 1 || !strings.Contains(got[0], `window.location.href = "/login"`) {
		t.Errorf("redirect scripts = %v", got)
	}
}

// TestBackButtonReachesTheServer. Back and forward are the half of
// navigation the server cannot see for itself, so the page reports them.
func TestBackButtonReachesTheServer(t *testing.T) {
	c := &table{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	// The shim is in the page, pointed at this session.
	if !strings.Contains(page, "popstate") {
		t.Error("no history shim in the page")
	}
	if !strings.Contains(page, routePrefix+"/nav") {
		t.Errorf("shim does not point at this session's endpoint: %q", page)
	}

	// What the shim posts when the user presses back.
	if code := postBody(t, srv, sid, routePrefix+"/nav", `{"url":"/?filter=archived"}`); code != http.StatusNoContent {
		t.Fatalf("nav: status %d, want 204", code)
	}

	if evt := stream.event(t); !strings.Contains(evt, `<p id="state">archived/</p>`) {
		t.Errorf("back button did not re-render the component: %q", evt)
	}
	if c.Params < 2 {
		t.Errorf("HandleParams ran %d times, want it to run again for the back button", c.Params)
	}
}

// TestBackButtonDoesNotBounceTheBrowser: the browser has already moved, so
// the server must not send it back where it was.
func TestBackButtonDoesNotBounceTheBrowser(t *testing.T) {
	c := &table{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	sess, _ := h.sessions.get(sid)
	stream := openStream(t, srv, sid)

	if code := postBody(t, srv, sid, routePrefix+"/nav", `{"url":"/?filter=archived"}`); code != http.StatusNoContent {
		t.Fatalf("nav: status %d", code)
	}
	stream.event(t) // the re-render

	if got := sess.currentURL(); got != "/?filter=archived" {
		t.Errorf("session url = %q, want the URL the browser moved to", got)
	}
	if !stream.silent(150 * time.Millisecond) {
		t.Error("the server pushed the browser somewhere after it navigated itself")
	}
}

// TestNavRejectsRubbish.
func TestNavRejectsRubbish(t *testing.T) {
	h := New(func() Component { return &table{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)

	if code := postBody(t, srv, sid, routePrefix+"/nav", `{not json`); code != http.StatusBadRequest {
		t.Errorf("bad payload: status %d, want 400", code)
	}
	// An unknown session, named in the header rather than the path - the
	// path form would be a routing miss and would pass without ever
	// reaching the session lookup.
	if code := postBody(t, srv, "deadbeef", routePrefix+"/nav", `{"url":"/"}`); code != http.StatusNotFound {
		t.Errorf("unknown session: status %d, want 404", code)
	}
}

// TestNavStaysInsideTheMount. The reported URL is client input and
// Session.Path is what a Subtree component routes on, so an unchecked value
// would move the session to any path on the server without that path ever
// passing the middleware mounted in front of this handler. Legitimate
// navigation is only ever history this handler wrote, and it wrote nothing
// outside its own mount.
func TestNavStaysInsideTheMount(t *testing.T) {
	h := New(func() Component { return &table{} })
	h.Logger = quietLogger()
	h.Prefix = "/app"
	mux := http.NewServeMux()
	mux.Handle("/app/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv, "/app/")

	for name, u := range map[string]string{
		"an absolute URL":         "https://example.com/app/",
		"a schemeless host":       "//example.com/app/",
		"a path outside the app":  "/admin/users",
		"a prefix-shaped sibling": "/appendix",
	} {
		if code := postBody(t, srv, sid, "/app"+routePrefix+"/nav", `{"url":"`+u+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}

	// The paths the handler actually wrote still navigate.
	if code := postBody(t, srv, sid, "/app"+routePrefix+"/nav", `{"url":"/app/?filter=archived"}`); code != http.StatusNoContent {
		t.Errorf("a path under the mount: status %d, want 204", code)
	}
}

// TestNavigationNeedsAMount.
func TestNavigationNeedsAMount(t *testing.T) {
	var c table
	ctx := context.Background()

	for name, err := range map[string]error{
		"Navigate": c.Navigate(ctx, "/"),
		"Replace":  c.Replace(ctx, "/"),
		"Redirect": c.Redirect(ctx, "/"),
	} {
		if !errors.Is(err, ErrNotMounted) {
			t.Errorf("%s = %v, want ErrNotMounted", name, err)
		}
	}
}
