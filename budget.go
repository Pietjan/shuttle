package shuttle

import "time"

// A bucket is the request budget of one session: tokens that accrue at a
// steady rate up to a cap, one spent per request, and a refusal when there
// are none.
//
// Every transport request a page makes queues work onto that page's single
// goroutine, so the thing being rationed is not bandwidth but a component's
// attention - and, through pub/sub, everybody else's. One client publishing
// in a loop costs every subscriber on the topic a render apiece, which is
// the case a limit exists for: the server survives it and the room does
// not.
//
// A bucket rather than a counter per window, for one reason that matters
// here. A window has to be reset, and the reset is its flaw: two hundred
// requests at 0:59 and two hundred at 1:01 is four hundred in two seconds
// and inside the rules. A bucket has no window, so there is nothing to
// reset and no boundary to sit on.
//
// The consequence for a long-lived page is the one to want: **elapsed time
// is not credit**. Refill stops at the cap, so a session open all day and
// idle is in exactly the state of one opened a minute ago and idle - full,
// not owed a day's worth of requests. Only recent spending is remembered,
// where recent is set by the rate.
type bucket struct {
	// tokens is what was available at last.
	tokens float64
	// last is when tokens was computed. The zero value means the bucket has
	// never been used, which take reads as full.
	last time.Time
}

// take spends a token if there is one, refilling first for the time that
// has passed. It reports whether the request may proceed.
//
// Refill is computed on demand rather than by a ticker. At the registry's
// cap that would be ten thousand goroutines waking up to enforce a limit
// whose whole purpose is to protect the process from load; an idle session
// should cost nothing, and here it costs nothing.
func (b *bucket) take(rate, burst float64, now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * rate
		if b.tokens > burst {
			b.tokens = burst
		}
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allow spends one of this session's request tokens.
//
// A rate of zero or less turns the limit off, which is expressible on
// purpose: a deployment behind its own gateway may already ration this, and
// two limiters disagreeing is worse than one.
func (s *Session) allow(rate, burst float64, now time.Time) bool {
	if rate <= 0 {
		return true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.budget.take(rate, burst, now)
}
