package shuttle

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPageBudgetRefusesAFlood is the hole the per-session budget cannot
// cover, because the page route is where sessions come from: a client
// refused by one session's bucket takes another session.
func TestPageBudgetRefusesAFlood(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	// Small and slow, so the test neither sleeps nor guesses.
	h.PageRate = 1
	h.PageBurst = 3
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for i := range 3 {
		if code := getStatus(t, srv, nil); code != http.StatusOK {
			t.Fatalf("page load %d of the burst: status %d, want 200", i+1, code)
		}
	}

	resp := getResponse(t, srv, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the fourth page load: status %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After %q, want 1 - a second is a token back at this rate", got)
	}
}

// TestARefusedPageMintsNoSession. The point of the limit is what a page
// load costs, so the refusal has to happen before the cost is paid: no
// component is built, nothing is mounted, and the registry does not grow.
func TestARefusedPageMintsNoSession(t *testing.T) {
	var built int
	h := New(func() Component { built++; return &counter{} })
	h.Logger = quietLogger()
	h.PageRate = 1
	h.PageBurst = 1
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	if code := getStatus(t, srv, nil); code != http.StatusOK {
		t.Fatalf("the first page load: status %d, want 200", code)
	}
	if code := getStatus(t, srv, nil); code != http.StatusTooManyRequests {
		t.Fatalf("the second page load: status %d, want 429", code)
	}

	if built != 1 {
		t.Errorf("the factory ran %d times, want it refused before it ran", built)
	}
	if n := h.sessions.len(); n != 1 {
		t.Errorf("%d sessions in the registry, want 1", n)
	}
}

// TestPageBudgetIsPerClient is the property that makes it worth having.
// One client emptying its bucket must leave everybody else's alone, or the
// limiter is the outage it was meant to prevent.
func TestPageBudgetIsPerClient(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.PageRate = 1
	h.PageBurst = 2
	// Every request in a test comes from the loopback, so the client has to
	// be named some other way. This is also the hook an app behind a proxy
	// sets, so exercising it here is not only a convenience.
	h.ClientIP = func(r *http.Request) string { return r.Header.Get("X-Test-Client") }
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	noisy := http.Header{"X-Test-Client": []string{"noisy"}}
	quiet := http.Header{"X-Test-Client": []string{"quiet"}}

	for range 2 {
		getStatus(t, srv, noisy)
	}
	if code := getStatus(t, srv, noisy); code != http.StatusTooManyRequests {
		t.Fatalf("the noisy client: status %d, want 429", code)
	}

	if code := getStatus(t, srv, quiet); code != http.StatusOK {
		t.Errorf("the quiet client: status %d, want 200 - the budget is not per client", code)
	}
}

// TestPageBudgetCanBeTurnedOff, for a deployment already rationing this at
// its gateway - the same bargain the request budget offers, and for the
// same reason: two limiters disagreeing is worse than one.
func TestPageBudgetCanBeTurnedOff(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	h.PageRate = -1
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for i := range 30 {
		if code := getStatus(t, srv, nil); code != http.StatusOK {
			t.Fatalf("page load %d with the limit off: status %d, want 200", i+1, code)
		}
	}
}

// TestTheDefaultBudgetPassesOrdinaryUse. A limiter that refuses a person
// opening a few tabs would be removed by the first app to hit it, so the
// default has to leave normal browsing alone.
func TestTheDefaultBudgetPassesOrdinaryUse(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for i := range int(DefaultPageBurst) {
		if code := getStatus(t, srv, nil); code != http.StatusOK {
			t.Fatalf("page load %d of the default burst: status %d, want 200", i+1, code)
		}
	}
}

// TestForwardedClientIPIgnoresAForgedPrefix is the one that decides whether
// this is a limiter or a formality.
//
// A client can put anything in X-Forwarded-For, so the entries it wrote are
// still in the header when the request arrives - a proxy appends, it does
// not replace. Counting from the end skips exactly the entries written by
// trusted proxies and lands on what the nearest one saw, which is the only
// address in the header nobody downstream chose.
func TestForwardedClientIPIgnoresAForgedPrefix(t *testing.T) {
	for name, tc := range map[string]struct {
		hops    int
		header  []string
		remote  string
		want    string
		because string
	}{
		"one proxy": {
			hops:    1,
			header:  []string{"203.0.113.7"},
			remote:  "10.0.0.1:5000",
			want:    "203.0.113.7",
			because: "the only entry was written by the proxy",
		},
		"one proxy, forged prefix": {
			hops:    1,
			header:  []string{"198.51.100.9, 203.0.113.7"},
			remote:  "10.0.0.1:5000",
			want:    "203.0.113.7",
			because: "the client sent the first entry itself",
		},
		"two proxies": {
			hops:    2,
			header:  []string{"203.0.113.7, 10.0.0.2"},
			remote:  "10.0.0.1:5000",
			want:    "203.0.113.7",
			because: "the last entry is the inner proxy",
		},
		"two proxies, forged prefix": {
			hops:    2,
			header:  []string{"1.1.1.1, 2.2.2.2, 203.0.113.7, 10.0.0.2"},
			remote:  "10.0.0.1:5000",
			want:    "203.0.113.7",
			because: "two forged entries change nothing, because counting starts at the end",
		},
		"separate headers": {
			hops:    2,
			header:  []string{"203.0.113.7", "10.0.0.2"},
			remote:  "10.0.0.1:5000",
			want:    "203.0.113.7",
			because: "a proxy may add its own header instead of extending one",
		},
		"a chain shorter than hops": {
			hops:    2,
			header:  []string{"203.0.113.7"},
			remote:  "10.0.0.1:5000",
			want:    "10.0.0.1",
			because: "misconfiguration falls back to the connection, which cannot be forged",
		},
		"no header at all": {
			hops:    1,
			header:  nil,
			remote:  "10.0.0.1:5000",
			want:    "10.0.0.1",
			because: "nothing to read",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.header {
				r.Header.Add("X-Forwarded-For", v)
			}

			if got := ForwardedClientIP(tc.hops)(r); got != tc.want {
				t.Errorf("client %q, want %q - %s", got, tc.want, tc.because)
			}
		})
	}
}

// TestIPv6IsChargedByPrefix. Keyed on the whole address, a limit per IPv6
// client is a limit per request: the smallest allocation anybody gets is a
// /64, so an attacker never has to meet the same bucket twice.
func TestIPv6IsChargedByPrefix(t *testing.T) {
	first := narrow("2001:db8:1:2:3:4:5:6")
	second := narrow("2001:db8:1:2:ffff:ffff:ffff:ffff")
	if first != second {
		t.Errorf("two addresses in one /64 keyed as %q and %q, want one bucket", first, second)
	}

	if other := narrow("2001:db8:1:3::1"); other == first {
		t.Errorf("a different /64 shares the bucket %q", other)
	}

	// IPv4 is charged as itself: the equivalent reasoning does not apply,
	// since an address is scarce and usually one machine.
	if got := narrow("203.0.113.7"); got != "203.0.113.7" {
		t.Errorf("IPv4 keyed as %q, want the address itself", got)
	}
	// And an identifier that is not an address at all passes through, so
	// ClientIP can return an account id.
	if got := narrow("tenant-42"); got != "tenant-42" {
		t.Errorf("a non-address key became %q", got)
	}
}

// TestFullBucketsAreForgotten. A map keyed by something the client chooses
// is a memory-exhaustion primitive unless something empties it, and the
// limiter becoming the resource exhaustion would be an unusually poor
// trade.
//
// Forgetting a refilled bucket is exact rather than approximate: take
// starts a fresh bucket at the cap, so a bucket that has earned everything
// back holds no information and dropping it cannot let anybody past.
func TestFullBucketsAreForgotten(t *testing.T) {
	var c clients
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// A token every ten seconds, so a sweep interval is not long enough to
	// refill a bucket that was emptied - which is what lets the test tell
	// "still spending" apart from "stopped a minute ago".
	const rate, burst = 0.1, 10.0

	// A hundred clients each load one page.
	for i := range 100 {
		c.allow(fmt.Sprintf("client-%d", i), rate, burst, t0)
	}
	if n := len(c.buckets); n != 100 {
		t.Fatalf("%d buckets, want 100", n)
	}

	// One of them empties its bucket, so it still has something to
	// remember: it is the only client whose next request depends on what it
	// did before.
	const spender = "client-0"
	for c.allow(spender, rate, burst, t0) {
	}
	was := c.buckets[spender]

	// A sweep interval later, everybody who stopped has refilled.
	at := t0.Add(sweepEvery + time.Second)
	c.allow(spender, rate, burst, at)

	if n := len(c.buckets); n != 1 {
		t.Errorf("%d buckets after the sweep, want only the one still spending", n)
	}
	if c.buckets[spender] != was {
		t.Error("the sweep forgot the client that was still spending")
	}
}

// TestAForgottenClientIsWhereItWouldHaveBeen: the sweep is a no-op on
// behaviour, not an amnesty. A client whose bucket is dropped gets a fresh
// one, and a fresh one is full - which is exactly what the dropped bucket
// had refilled to.
func TestAForgottenClientIsWhereItWouldHaveBeen(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	at := t0.Add(sweepEvery + time.Second)

	// Swept: the bucket is dropped and rebuilt.
	var swept clients
	for swept.allow("a", 1, 10, t0) {
	}
	var afterSweep int
	for swept.allow("a", 1, 10, at) {
		afterSweep++
	}

	// Not swept: the same bucket refills in place.
	var kept clients
	for kept.allow("a", 1, 10, t0) {
	}
	kept.swept = at // pretend a sweep just happened, so none runs
	var afterRefill int
	for kept.allow("a", 1, 10, at) {
		afterRefill++
	}

	if afterSweep != afterRefill {
		t.Errorf("a swept client got %d requests and a remembered one %d - the sweep is not free",
			afterSweep, afterRefill)
	}
}

// getResponse loads the page and keeps the response, for the headers.
func getResponse(t *testing.T, srv *httptest.Server, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func getStatus(t *testing.T, srv *httptest.Server, header http.Header) int {
	t.Helper()
	return getResponse(t, srv, header).StatusCode
}
