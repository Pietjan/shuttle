package shuttle

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
)

// leaver redirects, which is the one action whose effect is a script the
// stream injects - the case a CSP nonce has to cover long after the page
// response that minted it.
type leaver struct{ Base }

func (l *leaver) Render(ctx context.Context) templ.Component {
	go_ := button.New(OnClick(ctx, button.Attr, func(actx context.Context) error {
		return l.Redirect(actx, "/elsewhere")
	}))
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return go_.Render(templ.WithChildren(ctx, templ.Raw("go")), w)
	})
}

// TestNonceReachesEveryScript. Under a script-src without 'unsafe-inline',
// every script shuttle produces needs the page's nonce: the inline shim and
// the Datastar module at page load, and - the half that is easy to miss -
// every script the stream injects afterwards, which passes CSP by carrying
// the nonce of the page it lands in.
func TestNonceReachesEveryScript(t *testing.T) {
	const nonce = "dGVzdC1ub25jZQ=="
	h := New(func() Component { return &leaver{} })
	h.Logger = quietLogger()
	h.Nonce = func(*http.Request) string { return nonce }
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)

	// Both page-load scripts carry it, and the attach expression forwards
	// it as a header so a post-restart reload can carry it too.
	if got := strings.Count(page, `nonce="`+nonce+`"`); got < 2 {
		t.Errorf("%d nonced scripts in the page, want the module and the shim", got)
	}
	if !strings.Contains(page, NonceHeader) {
		t.Error("the stream's headers do not forward the nonce")
	}

	// A script injected later - a redirect - carries the same nonce.
	stream := openStream(t, srv, sid)
	if code := post(t, srv, sid, clickURLs(t, fragment(t, page))[0]); code != http.StatusNoContent {
		t.Fatalf("redirect action: status %d", code)
	}
	evt := stream.event(t)
	if !strings.Contains(evt, "/elsewhere") {
		t.Fatalf("no redirect script arrived: %q", evt)
	}
	if !strings.Contains(evt, `nonce="`+nonce+`"`) {
		t.Errorf("the injected script carries no nonce: %q", evt)
	}
}

// TestNonceIsValidatedBeforeItIsMarkup. The hook's value lands in
// attributes and the header's value in an injected script, so both are
// gated to the one alphabet a nonce can have - an attribute-breaking value
// is dropped, not escaped.
func TestNonceIsValidatedBeforeItIsMarkup(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.Nonce = func(*http.Request) string { return `"><script>alert(1)</script>` }
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)
	if strings.Contains(page, "alert(1)") || strings.Contains(page, "nonce=") {
		t.Errorf("an invalid nonce reached the page: %q", page)
	}

	// The reload path reads the nonce from a client-written header.
	for header, want := range map[string]bool{
		"dmFsaWQ=":               true,
		`foo" onx="pwn`:          false,
		strings.Repeat("a", 300): false,
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+routePrefix+"/live", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(SessionHeader, "deadbeef")
		req.Header.Set(NonceHeader, header)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("reload stream: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if got := strings.Contains(string(body), "nonce="); got != want {
			t.Errorf("header %.20q: nonce in reload = %t, want %t (%q)", header, got, want, body)
		}
		if strings.Contains(string(body), "pwn") {
			t.Errorf("a hostile header value was echoed: %q", body)
		}
	}
}
