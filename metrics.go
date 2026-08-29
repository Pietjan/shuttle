package shuttle

import "sync/atomic"

// Stats is a point-in-time picture of what a Handler is doing, from
// [Handler.Stats]. The two gauges say what is true now; everything else
// counts since the process started and only ever grows.
//
// This is the whole observability surface, on purpose. Shuttle does not
// pick a metrics system: poll this from whatever exporter the app already
// has - an expvar.Func, a prometheus collector, a log line on a ticker -
// and the counters become rates by differencing. The places these numbers
// come from are exactly the places this design concentrates load where
// nothing else can see it: one goroutine runs each page, and one publish
// costs every subscriber a render.
type Stats struct {
	// Sessions is how many pages are live right now - each one a component
	// tree in memory. The registry caps this; watch it against that cap.
	Sessions int
	// Attached is how many of them currently hold their stream. The gap
	// between the two is pages inside a reconnect or eviction window.
	Attached int

	// PagesServed counts successful page loads - each one minted a session.
	// PagesRefused counts loads the per-client budget turned away, and
	// SessionsRefused the ones the registry refused: at its cap, or shut
	// down.
	PagesServed     int64
	PagesRefused    int64
	SessionsRefused int64

	// Actions, Navs and Uploads count requests that completed;
	// ActionsFailed and UploadsRefused the ones that did not.
	// BudgetRefused counts requests the per-session budget stopped before
	// any of them ran.
	Actions        int64
	ActionsFailed  int64
	Navs           int64
	Uploads        int64
	UploadsRefused int64
	UploadBytes    int64
	BudgetRefused  int64

	// Patches counts everything written to streams - re-renders, stream
	// operations, scripts and signal writes. OpsDropped counts stream
	// operations discarded because a detached session's backlog was full:
	// any value here means a client came back to a list with holes in it.
	Patches    int64
	OpsDropped int64

	// RenderErrors and Panics count component failures the session
	// contained. They are bugs in components, not load - the right
	// baseline is zero.
	RenderErrors int64
	Panics       int64

	// Takeovers counts streams displaced by a newer attach - normal in
	// ones (a reconnect), suspicious in a stream (a page fighting itself).
	// Reloads counts unknown-session streams sent home, which after a
	// deploy is every open page, and any other time is sessions dying
	// under their pages. SessionsEnded counts every session torn down,
	// whatever ended it.
	Takeovers     int64
	Reloads       int64
	SessionsEnded int64
}

// counters is the live, concurrently-written half of Stats. One instance
// per Handler, shared with every session it creates; the testing kit's
// standalone sessions get their own, which nothing reads.
type counters struct {
	pagesServed     atomic.Int64
	pagesRefused    atomic.Int64
	sessionsRefused atomic.Int64

	actions        atomic.Int64
	actionsFailed  atomic.Int64
	navs           atomic.Int64
	uploads        atomic.Int64
	uploadsRefused atomic.Int64
	uploadBytes    atomic.Int64
	budgetRefused  atomic.Int64

	patches    atomic.Int64
	opsDropped atomic.Int64

	renderErrors atomic.Int64
	panics       atomic.Int64

	takeovers     atomic.Int64
	reloads       atomic.Int64
	sessionsEnded atomic.Int64
}

// Stats returns what the handler is doing right now. Safe from any
// goroutine, cheap enough to poll: the counters are atomics, and the two
// gauges are one pass over the session registry.
func (h *Handler) Stats() Stats {
	c := h.counters()
	s := Stats{
		PagesServed:     c.pagesServed.Load(),
		PagesRefused:    c.pagesRefused.Load(),
		SessionsRefused: c.sessionsRefused.Load(),
		Actions:         c.actions.Load(),
		ActionsFailed:   c.actionsFailed.Load(),
		Navs:            c.navs.Load(),
		Uploads:         c.uploads.Load(),
		UploadsRefused:  c.uploadsRefused.Load(),
		UploadBytes:     c.uploadBytes.Load(),
		BudgetRefused:   c.budgetRefused.Load(),
		Patches:         c.patches.Load(),
		OpsDropped:      c.opsDropped.Load(),
		RenderErrors:    c.renderErrors.Load(),
		Panics:          c.panics.Load(),
		Takeovers:       c.takeovers.Load(),
		Reloads:         c.reloads.Load(),
		SessionsEnded:   c.sessionsEnded.Load(),
	}
	s.Sessions, s.Attached = h.sessions.gauges()
	return s
}
