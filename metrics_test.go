package shuttle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStatsCountTheTraffic drives one of everything through a handler and
// reads it back out of Stats. Each assertion is an increment site: a
// counter nothing increments is worse than none, because a dashboard shows
// it flat and calls that health.
func TestStatsCountTheTraffic(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	// A burst of one and a near-zero refill, so the second action in a row
	// is the budget refusal this test wants to see counted.
	h.RequestRate = 0.001
	h.RequestBurst = 1
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	if s := h.Stats(); s.PagesServed != 1 || s.Sessions != 1 {
		t.Errorf("after a page load: served %d, live %d, want 1 and 1", s.PagesServed, s.Sessions)
	}

	first := openStream(t, srv, sid)
	if s := h.Stats(); s.Attached != 1 {
		t.Errorf("attached = %d with a stream open, want 1", s.Attached)
	}

	// One click lands; the next is refused by the request budget.
	url := clickURLs(t, fragment(t, page))[0]
	if code := post(t, srv, sid, url); code != http.StatusNoContent {
		t.Fatalf("action: status %d", code)
	}
	if code := post(t, srv, sid, url); code != http.StatusTooManyRequests {
		t.Fatalf("second action: status %d, want 429", code)
	}
	if s := h.Stats(); s.Actions != 1 || s.BudgetRefused != 1 {
		t.Errorf("actions %d, refused %d, want 1 and 1", s.Actions, s.BudgetRefused)
	}

	// The click's re-render went down the stream as a patch.
	if evt := first.event(t); !strings.Contains(evt, "count") {
		t.Fatalf("no re-render arrived: %q", evt)
	}
	if s := h.Stats(); s.Patches < 1 {
		t.Errorf("patches = %d after a re-render, want at least 1", s.Patches)
	}

	// A newer stream displaces the old one.
	openStream(t, srv, sid)
	if s := h.Stats(); s.Takeovers != 1 {
		t.Errorf("takeovers = %d, want 1", s.Takeovers)
	}

	// A stream for a session this server never had is sent home.
	req, err := http.NewRequest(http.MethodGet, srv.URL+routePrefix+"/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SessionHeader, "deadbeef")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("unknown-session stream: %v", err)
	}
	resp.Body.Close()
	if s := h.Stats(); s.Reloads != 1 {
		t.Errorf("reloads = %d, want 1", s.Reloads)
	}

	// Shutdown ends every session, and the count of endings matches.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	s := h.Stats()
	if s.Sessions != 0 || s.Attached != 0 {
		t.Errorf("after shutdown: %d sessions, %d attached, want 0 and 0", s.Sessions, s.Attached)
	}
	if s.SessionsEnded != s.PagesServed {
		t.Errorf("ended %d sessions but served %d pages; every page's session must be accounted for",
			s.SessionsEnded, s.PagesServed)
	}
}
