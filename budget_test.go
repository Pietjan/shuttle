package shuttle

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBucketRefillsWithoutAReset is the property a window cannot have.
//
// A counter per window has to be cleared, and clearing it is the hole: the
// whole allowance twice over, across a boundary, inside the rules. A bucket
// has no boundary - it earns back continuously, so spending is smoothed
// rather than forgiven all at once.
func TestBucketRefillsWithoutAReset(t *testing.T) {
	var b bucket
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// Ten a second, ten in hand. The first ten are free, the eleventh is not.
	for i := range 10 {
		if !b.take(10, 10, t0) {
			t.Fatalf("refused request %d of a full bucket", i+1)
		}
	}
	if b.take(10, 10, t0) {
		t.Error("an empty bucket allowed a request")
	}

	// Half a second earns five back, and no more.
	at := t0.Add(500 * time.Millisecond)
	for i := range 5 {
		if !b.take(10, 10, at) {
			t.Fatalf("refused request %d of the five earned back", i+1)
		}
	}
	if b.take(10, 10, at) {
		t.Error("half a second bought more than half a second's worth")
	}
}

// TestALongSessionIsNotOwedAnything is the question this design has to
// answer, because a page here is meant to stay open all day.
//
// Elapsed time is not credit. Refill stops at the cap, so an idle session
// is full and never more than full - eight hours of sitting there buys the
// same burst as one minute, and cannot be spent as eight hours' worth at
// once. Which is also why there is nothing to reset: the bucket is already
// in the state a reset would put it in.
func TestALongSessionIsNotOwedAnything(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	for name, idle := range map[string]time.Duration{
		"a minute": time.Minute,
		"an hour":  time.Hour,
		"a day":    24 * time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			var b bucket
			// Spend everything, then wait.
			for b.take(10, 40, t0) {
			}

			at := t0.Add(idle)
			var got int
			for b.take(10, 40, at) {
				got++
			}
			if got != 40 {
				t.Errorf("after %s idle a session could make %d requests, want the burst of 40", idle, got)
			}
		})
	}
}

// TestBucketStartsFull, so the first thing a page does is never refused.
func TestBucketStartsFull(t *testing.T) {
	var b bucket
	now := time.Now()

	var got int
	for b.take(10, 40, now) {
		got++
	}
	if got != 40 {
		t.Errorf("a fresh bucket allowed %d requests, want 40", got)
	}
}

// TestBudgetRefusesAFlood drives it through the handler, where the refusal
// has to happen before anything is decoded or queued.
func TestBudgetRefusesAFlood(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	// Small and slow, so the test neither sleeps nor guesses.
	h.RequestRate = 1
	h.RequestBurst = 3
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	// Navigation rather than a click: an action id is only good for two
	// generations, and every successful action renders a new one, so
	// hammering one URL would measure that instead of the budget.
	nav := routePrefix + "/nav"

	for i := range 3 {
		if code := postBody(t, srv, sid, nav, `{"url":"/"}`); code != http.StatusNoContent {
			t.Fatalf("request %d of the burst: status %d, want 204", i+1, code)
		}
	}

	resp := postResponse(t, srv, sid, nav)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the fourth request: status %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("a 429 with no Retry-After")
	}
	// The session is unharmed - this is a refusal, not a failure.
	if _, ok := h.sessions.get(sid); !ok {
		t.Error("the session was torn down by its own rate limit")
	}
}

// TestARefusedRequestNeverReachesTheComponent. The check runs before the
// handler, so a spent budget costs a map lookup and a mutex - nothing is
// decoded and nothing is queued onto the session's goroutine, which is the
// resource actually being protected.
func TestARefusedRequestNeverReachesTheComponent(t *testing.T) {
	c := &counter{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	h.RequestRate = 1
	h.RequestBurst = 1
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	action := clickURLs(t, fragment(t, page))[0]

	if code := post(t, srv, sid, action); code != http.StatusNoContent {
		t.Fatalf("the first click: status %d, want 204", code)
	}
	if c.Count != 1 {
		t.Fatalf("the action ran %d times, want 1", c.Count)
	}

	// The budget is spent, so this is refused before anything looks at what
	// it names - it never reaches the action, stale or otherwise.
	if code := post(t, srv, sid, action); code != http.StatusTooManyRequests {
		t.Errorf("the second click: status %d, want 429", code)
	}
	if c.Count != 1 {
		t.Errorf("the action ran %d times, want it refused before it ran", c.Count)
	}
}

// TestBudgetIsPerSession: one page spending everything must not refuse
// another, or a single noisy tab takes the application down for everybody -
// which is the failure a limiter is supposed to prevent.
func TestBudgetIsPerSession(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.RequestRate = 1
	h.RequestBurst = 2
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	noisy, noisySid := getPage(t, srv)
	quiet, quietSid := getPage(t, srv)

	noisyAction := clickURLs(t, fragment(t, noisy))[0]
	for range 3 {
		post(t, srv, noisySid, noisyAction)
	}
	if code := post(t, srv, noisySid, noisyAction); code != http.StatusTooManyRequests {
		t.Fatalf("the noisy page: status %d, want 429", code)
	}

	if code := post(t, srv, quietSid, clickURLs(t, fragment(t, quiet))[0]); code != http.StatusNoContent {
		t.Errorf("the quiet page: status %d, want 204 - the budget is not per session", code)
	}
}

// TestBudgetCanBeTurnedOff, for a deployment already rationing this at its
// gateway. Two limiters disagreeing is worse than one.
func TestBudgetCanBeTurnedOff(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.RequestRate = -1
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	nav := routePrefix + "/nav"

	for i := range 60 {
		if code := postBody(t, srv, sid, nav, `{"url":"/"}`); code != http.StatusNoContent {
			t.Fatalf("request %d with the limit off: status %d, want 204", i+1, code)
		}
	}
}

// TestEveryChangingRouteIsBudgeted. Actions are the obvious one, but every
// transport request queues work onto the same goroutine, so a limit on one
// route is a limit on nothing.
func TestEveryChangingRouteIsBudgeted(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.RequestRate = 1
	h.RequestBurst = 1
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	for name, path := range map[string]string{
		"action": routePrefix + "/act/c/1-a1",
		"nav":    routePrefix + "/nav",
		"upload": routePrefix + "/upload/c/photos",
	} {
		t.Run(name, func(t *testing.T) {
			_, sid := getPage(t, srv)
			// Spend the one token, whatever the route makes of the request.
			postBody(t, srv, sid, path, `{"url":"/"}`)

			if code := postBody(t, srv, sid, path, `{"url":"/"}`); code != http.StatusTooManyRequests {
				t.Errorf("%s: status %d, want 429", name, code)
			}
		})
	}
}

// postResponse is post with the response kept, for the headers.
func postResponse(t *testing.T, srv *httptest.Server, sid, path string) *http.Response {
	t.Helper()
	// Client first: an in-memory test server only materialises URL on its
	// first Client call, and the request below reads it.
	client := srv.Client()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SessionHeader, sid)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
