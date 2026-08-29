package shuttle

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Page budget defaults, per client.
//
// A page load is a human action, so the ceiling is set by what a person
// does rather than by what a machine can: opening a handful of tabs at once
// is the burst, and one a second sustained is already faster than anybody
// reads. This is deliberately an order of magnitude under the per-session
// budget in budget.go, because the two ration different things - that one
// bounds what an established page may ask for, this one bounds how many
// pages may exist.
const (
	// DefaultPageRate is the sustained page loads per second one client
	// earns back.
	DefaultPageRate = 1.0

	// DefaultPageBurst is the most it can load at once.
	DefaultPageBurst = 10.0
)

// The bookkeeping this limiter needs is itself keyed by something the
// client chooses, which is the trap every per-IP limiter has to answer:
// remembering everybody is a memory-exhaustion primitive wearing a
// limiter's clothes.
const (
	// sweepEvery bounds how often the map is walked, so the cost of
	// forgetting is amortised over a minute's traffic rather than paid per
	// request.
	sweepEvery = time.Minute

	// maxClients forces a sweep regardless of the clock, and is the ceiling
	// the map is held under.
	maxClients = 100_000
)

// clients is one [bucket] per client, with the map kept small by forgetting
// the clients that have nothing left to remember.
//
// Eviction is exact rather than approximate, which is the property that
// makes it safe: a bucket that has earned its way back to the cap is
// indistinguishable from one that has never been used, because take starts
// a fresh bucket full. So dropping a full bucket cannot let anybody past
// the limit - there is no state in it. Only a client that has spent
// recently is remembered, and that set is bounded by the rate.
type clients struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

// allow spends one of key's page tokens.
func (c *clients) allow(key string, rate, burst float64, now time.Time) bool {
	if rate <= 0 {
		return true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.buckets == nil {
		c.buckets = map[string]*bucket{}
		c.swept = now
	}
	c.sweep(rate, burst, now)

	b, ok := c.buckets[key]
	if !ok {
		b = &bucket{}
		c.buckets[key] = b
	}
	return b.take(rate, burst, now)
}

// sweep forgets the clients whose buckets have refilled. Called with the
// lock held.
func (c *clients) sweep(rate, burst float64, now time.Time) {
	if len(c.buckets) < maxClients && now.Sub(c.swept) < sweepEvery {
		return
	}
	c.swept = now

	for key, b := range c.buckets {
		// tokens is only what was in hand at last, so the question is not
		// whether the bucket was full then but whether enough time has
		// passed to fill it since.
		if b.tokens+now.Sub(b.last).Seconds()*rate >= burst {
			delete(c.buckets, key)
		}
	}

	if len(c.buckets) >= maxClients {
		// Everything still here is spending faster than it earns, and there
		// are a hundred thousand of them - a distributed flood, which a
		// per-client limit was never going to answer anyway. Forgetting is
		// the lesser harm: it hands every one of them a fresh burst, which
		// is exactly the position this process was in before the limiter
		// existed, and it keeps the bookkeeping from becoming the outage.
		clear(c.buckets)
	}
}

// clientKey identifies who a page load is charged to.
func (h *Handler) clientKey(r *http.Request) string {
	if h.ClientIP != nil {
		return narrow(h.ClientIP(r))
	}
	return narrow(remoteIP(r))
}

// remoteIP is the address the connection actually came from, which is the
// only one nobody can lie about.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// narrow reduces an address to the unit a limit should apply to: an IPv4
// address is itself, and an IPv6 address is its /64.
//
// The prefix is not tidiness. A residential IPv6 allocation is a /64 at
// worst and commonly a /56, so a limit keyed on the full address is a limit
// per attempt: an attacker with one subnet has eighteen quintillion keys
// and never meets the same bucket twice. The /64 is the smallest thing that
// is reliably one customer.
//
// Anything that does not parse as an address is passed through untouched,
// so a [Handler.ClientIP] returning an account id or a tenant name works as
// well as one returning an address.
func narrow(s string) string {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return s
	}
	if ip.Is4() || ip.Is4In6() {
		return ip.Unmap().String()
	}
	// Zones name a link-local interface and never a distinct customer.
	p, err := ip.WithZone("").Prefix(64)
	if err != nil {
		return s
	}
	return p.String()
}

// ForwardedClientIP returns a [Handler.ClientIP] that reads the client's
// address out of X-Forwarded-For, for a handler running behind hops trusted
// proxies - one for a single nginx or Caddy in front, two for a CDN in
// front of that.
//
// It counts from the end rather than taking the first entry, and that is
// the whole security argument. X-Forwarded-For is appended to, so the
// entries nearest the end were written by the proxies nearest here, and
// only those are trustworthy; everything before them is whatever the client
// sent, which an attacker seeds with as many forged addresses as it likes.
// Reading the leftmost entry - the common mistake - is not a limiter at
// all, since the client picks its own key.
//
// The connection's own address is not in the header, so hops-1 entries at
// the end were written by proxies, and the one before them is the client.
//
// hops must be exact, and that is a real requirement rather than tuning
// advice. Too low charges the proxy, which degrades safely. Too high does
// not: the position it reads then lies in the client-written part of the
// header, and the header's total length is also client-controlled - so a
// client that pads it puts whatever it likes at that position and picks its
// own bucket per request, which is no limiter at all. The chosen entry is
// at least required to parse as an address, so the keys a forger mints are
// confined to address shape, but confinement is not a fix: count your
// proxies.
//
// A handler that is not behind a proxy must not use this - use the default,
// which reads the connection.
func ForwardedClientIP(hops int) func(*http.Request) string {
	return func(r *http.Request) string {
		if hops < 1 {
			return remoteIP(r)
		}

		// Header.Values because a chain of proxies may each add their own
		// header rather than extending one, and the two are equivalent.
		var chain []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for part := range strings.SplitSeq(v, ",") {
				if s := strings.TrimSpace(part); s != "" {
					chain = append(chain, s)
				}
			}
		}
		if i := len(chain) - hops; i >= 0 {
			// A proxy writes addresses. An entry that is not one was written
			// by the client - a misconfigured hop count is reading the wrong
			// part of the header, and arbitrary strings must not become
			// bucket keys.
			if _, err := netip.ParseAddr(chain[i]); err == nil {
				return chain[i]
			}
		}
		return remoteIP(r)
	}
}

// metered refuses a page load from a client that is minting sessions faster
// than a person opens tabs.
//
// This is the one route that has to be rationed by client rather than by
// session, because it is the route that hands out sessions: the per-session
// budget cannot bound a caller who simply takes another session, and every
// page load costs a component tree, a mount and whatever that mount
// subscribes to. The registry's cap keeps that from exhausting memory, but
// a cap alone is not fairness - one client can hold all ten thousand slots
// and every real page load is refused for as long as it keeps going, which
// is a denial of service made out of nothing but ordinary requests.
//
// Like the session budget, it runs before the handler: a refusal costs a
// map lookup and a mutex, and nothing is mounted, rendered or registered.
func (h *Handler) metered(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rate := h.pageRate()
		// Checked here rather than only inside allow, so that a deployment
		// with the limit off does not parse a header per page load.
		if rate > 0 {
			key := h.clientKey(r)
			if !h.pages.allow(key, rate, h.pageBurst(), time.Now()) {
				h.log().Warn("shuttle: page budget exhausted",
					"client", key, "path", r.URL.Path)
				w.Header().Set("Retry-After", retryAfter(rate))
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
		}
		next(w, r)
	}
}

// retryAfter is how long until one token is back, rounded up and never less
// than the second the header can express.
func retryAfter(rate float64) string {
	secs := max(int(math.Ceil(1/rate)), 1)
	return strconv.Itoa(secs)
}

func (h *Handler) pageRate() float64 {
	if h.PageRate == 0 {
		return DefaultPageRate
	}
	return h.PageRate
}

func (h *Handler) pageBurst() float64 {
	if h.PageBurst <= 0 {
		return DefaultPageBurst
	}
	return h.PageBurst
}
