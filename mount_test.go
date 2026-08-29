package shuttle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// TestMountedUnderAPrefix: a real app has other routes, so shuttle has to
// live somewhere other than the server root - and every URL it renders has
// to point back at where it actually is.
func TestMountedUnderAPrefix(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.Prefix = "/dashboard"

	app := http.NewServeMux()
	app.Handle("/dashboard/", h)
	app.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "the app's own home page")
	})

	srv := httptest.NewTestServer(t, app)
	t.Cleanup(srv.Close)

	// The app's own routes are untouched.
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	home, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(home), "the app's own home page") {
		t.Errorf("shuttle swallowed the app's root: %q", home)
	}

	// The page renders where it is mounted.
	resp, err = srv.Client().Get(srv.URL + "/dashboard/")
	if err != nil {
		t.Fatalf("GET /dashboard/: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dashboard/: status %d", resp.StatusCode)
	}

	page := string(body)
	sid := sessionRE.FindStringSubmatch(page)
	if sid == nil {
		t.Fatalf("no session id in %q", page)
	}

	// Both the stream and the actions point under the prefix.
	if !strings.Contains(page, "/dashboard/_shuttle/live") {
		t.Errorf("stream URL ignores the prefix: %q", page)
	}
	for _, u := range clickURLs(t, fragment(t, page)) {
		if !strings.HasPrefix(u, "/dashboard/_shuttle/act/") {
			t.Errorf("action URL ignores the prefix: %q", u)
		}
	}

	// And they actually work from there.
	stream := openStreamAt(t, srv, sid[1], "/dashboard/_shuttle/live")
	if code := post(t, srv, sid[1], clickURLs(t, fragment(t, page))[0]); code != http.StatusNoContent {
		t.Fatalf("action under prefix: status %d", code)
	}
	if evt := stream.event(t); !strings.Contains(evt, `<output id="count">1</output>`) {
		t.Errorf("no patch from the mounted handler: %q", evt)
	}
}

// TestPrefixDoesNotSwallowSiblingRoutes.
func TestPrefixDoesNotSwallowSiblingRoutes(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.Prefix = "/live"
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	for _, p := range []string{"/", "/other", "/live/deeper"} {
		resp, err := srv.Client().Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
	}
	if n := h.sessions.len(); n != 0 {
		t.Errorf("%d sessions created by requests outside the mount point", n)
	}
}

// TestCustomShellOwnsTheDocument: an app that cannot control its own html
// and body tags cannot set a theme class, a lang, or its own scripts.
func TestCustomShellOwnsTheDocument(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.Shell = func(w io.Writer, p Page) error {
		_, err := fmt.Fprintf(w,
			`<!doctype html><html class="dark" lang="nl"><head><title>%s</title>`+
				`<script type="module" src="%s"></script></head>`+
				`<body class="antialiased" data-init="%s">%s</body></html>`,
			p.Title, p.ScriptURL, p.Attach, p.Body)
		return err
	}
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)

	for _, want := range []string{
		`<html class="dark" lang="nl">`,
		`<body class="antialiased"`,
		`data-init="@get(`,
		`<div id="shuttle-c"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q in %q", want, page)
		}
	}
}

// TestShellGetsWhatItNeeds.
func TestShellGetsWhatItNeeds(t *testing.T) {
	h := New(func() Component { return &counter{Count: 4} })
	h.Logger = quietLogger()
	h.Title = "my app"
	h.Head = `<link rel="stylesheet" href="/app.css">`

	var got Page
	h.Shell = func(w io.Writer, p Page) error {
		got = p
		return DefaultShell(w, p)
	}
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	getPage(t, srv)

	if got.Title != "my app" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Head != `<link rel="stylesheet" href="/app.css">` {
		t.Errorf("Head = %q", got.Head)
	}
	if got.ScriptURL != DefaultScriptURL {
		t.Errorf("ScriptURL = %q", got.ScriptURL)
	}
	if !strings.HasPrefix(got.Attach, "@get('/_shuttle/live'") {
		t.Errorf("Attach = %q", got.Attach)
	}
	if !strings.Contains(got.Body, `<output id="count">4</output>`) {
		t.Errorf("Body = %q", got.Body)
	}
}

// openStreamAt opens the page's stream at an explicit path.
func openStreamAt(t *testing.T, srv *httptest.Server, sid, path string) *sseReader {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SessionHeader, sid)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("open stream %s: status %d", path, resp.StatusCode)
	}

	r := newSSEReader(resp.Body)
	t.Cleanup(func() { resp.Body.Close() })
	r.greeting(t)
	return r
}

// TestSubtreeServesEveryPathUnderTheMount. One handler owning a subtree is
// what lets a whole site be one session - and therefore one stream, rather
// than one per page in a browser that only allows about six.
func TestSubtreeServesEveryPathUnderTheMount(t *testing.T) {
	h := New(func() Component { return &pathAware{} })
	h.Logger = quietLogger()
	h.Prefix = "/e"
	h.Subtree = true

	app := http.NewServeMux()
	app.Handle("/e/", h)
	srv := httptest.NewTestServer(t, app)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/e/", "/e/counter/", "/e/deep/nested/page"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
			continue
		}
		// The component is told where it is, which is how one session can
		// render a different page per path.
		if !strings.Contains(string(body), `<p id="path">`+path+`</p>`) {
			t.Errorf("GET %s did not reach the component: %s", path, body)
		}
	}
}

// TestSubtreeIsOptIn, because a catch-all at the server root would mint a
// session for every stray GET.
func TestSubtreeIsOptIn(t *testing.T) {
	h := New(func() Component { return &pathAware{} })
	h.Logger = quietLogger()
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404 without Subtree", resp.StatusCode)
	}
	if n := h.sessions.len(); n != 0 {
		t.Errorf("%d sessions created off the mount point", n)
	}
}

// TestNavigateMovesThePath: navigating within a session has to change what
// Path reports, or a subtree component would keep rendering the old page.
func TestNavigateMovesThePath(t *testing.T) {
	c := &pathAware{}
	sess := newSession("test", c)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	sess.urlPath = "/e/counter/"
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}

	if err := c.Navigate(ctx, "/e/table/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if got := c.Path(); got != "/e/table/" {
		t.Errorf("Path() = %q, want /e/table/", got)
	}
	if c.Seen != "/e/table/" {
		t.Errorf("HandleParams saw %q", c.Seen)
	}
}

// pathAware renders wherever it happens to be.
type pathAware struct {
	Base
	Seen string
}

func (p *pathAware) HandleParams(context.Context, Params) error {
	p.Seen = p.Path()
	return nil
}

func (p *pathAware) Render(context.Context) templ.Component {
	return templ.Raw(`<p id="path">` + templ.EscapeString(p.Path()) + `</p>`)
}
