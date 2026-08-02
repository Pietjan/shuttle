package shuttle

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"
)

// routePrefix is where the transport lives. The page itself is served from
// the handler's root, so an app's own routes only have to avoid this.
const routePrefix = "/_shuttle"

// DefaultScriptURL is the pinned Datastar core build. Pinned rather than
// floating: Datastar v1 shipped in April 2026 and the attribute syntax
// changed across the 1.0 boundary, so an unpinned bundle can silently stop
// understanding the markup this package emits.
const DefaultScriptURL = "https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.2/bundles/datastar.js"

// DefaultHeartbeat is how often an idle stream writes, when Handler leaves
// it unset.
const DefaultHeartbeat = 25 * time.Second

// DefaultGrace is how long a session outlives its stream by default, so a
// reconnect resumes it instead of starting over.
const DefaultGrace = 30 * time.Second

// DefaultMaxSignalBytes caps an action's JSON body when Handler leaves
// MaxSignalBytes unset.
const DefaultMaxSignalBytes = 64 << 10 // 64 KiB

// Request budget defaults, per session.
//
// Sized against what a page actually does rather than against a round
// number. A change binding fires per keystroke through a client-side
// debounce, so a fast typist produces about three requests a second; a
// table's sort, filter and page can arrive in a burst. The burst is a
// little under the session mailbox's depth, because a burst larger than
// that is one already blocking request goroutines.
const (
	// DefaultRequestRate is the sustained requests per second a session
	// earns back.
	DefaultRequestRate = 10.0

	// DefaultRequestBurst is the most it can spend at once.
	DefaultRequestBurst = 40.0
)

// Handler serves one component as a live page: the first GET returns a
// complete server-rendered document, the page then opens a persistent
// stream and attaches to the markup already in the DOM.
type Handler struct {
	// Prefix is where this handler is mounted, when it is not at the server
	// root - "/dashboard" for a handler reached as
	// mux.Handle("/dashboard/", h). Both the transport routes and the URLs
	// rendered into the page are built from it, so it has to match how the
	// app routes to this handler.
	//
	// Set it before the first request; it is read once.
	Prefix string

	// Shell renders the document around the component on the first GET.
	// Leave it nil for [DefaultShell], or set it to own the page - html and
	// body attributes, meta tags, your own scripts.
	Shell Shell

	// Title is the page's <title>, used by DefaultShell.
	Title string

	// Head is raw HTML appended to <head> - a stylesheet link, most
	// usefully, since Loom's markup expects compiled Tailwind. Emitted
	// verbatim; it is the app's own markup, not user input.
	Head string

	// ScriptURL is the Datastar bundle. Point it at a self-hosted copy to
	// drop the CDN.
	ScriptURL string

	// Logger reports errors that have nowhere else to go - a render that
	// fails after the page's headers are already out, a stream that dies
	// mid-write. Defaults to slog.Default().
	Logger *slog.Logger

	// Debug turns on the checks that are too chatty for production and too
	// valuable to leave to chance in development: every transport request is
	// logged, and every render is scanned for duplicate element ids, which
	// Datastar would otherwise degrade silently.
	Debug bool

	// Broker is the pub/sub backend components publish and subscribe
	// through. Defaults to an in-process one, which is right until the app
	// runs on more than one node.
	Broker Broker

	// OpenWhenHidden keeps a backgrounded tab's stream open, and defaults to
	// true. Datastar recommends it, and for a framework holding state per
	// page it is close to required: with it off, a hidden tab's stream
	// closes, and once Grace expires the session is evicted and the
	// component unmounted. Coming back to that tab reloads the page and
	// loses whatever it held - after half a minute in another tab.
	//
	// The cost is a browser limit rather than a server one. Datastar streams
	// over fetch, not EventSource, so every connected page holds one of the
	// browser's ~6 connections per origin for as long as it is open, shared
	// across tabs. Five or six live pages and the next one simply never
	// loads: no error, the request never gets a connection to go out on.
	//
	// The answer to that is HTTP/2, where every stream shares one connection
	// and the limit stops existing - which is why it is a deployment
	// requirement rather than advice. Turning this off trades a background
	// tab's liveness for its connection, and is only reasonable on HTTP/1.1
	// with Grace raised to cover how long a tab might sit in the background.
	//
	// It is a pointer so that false is expressible and unset still means
	// true.
	OpenWhenHidden *bool

	// Subtree serves every path under Prefix rather than only the mount
	// point itself, so one session can be a whole section of an app: the
	// component reads Base.Path and renders accordingly, and navigating with
	// Base.Navigate swaps what is rendered without a page load.
	//
	// That is worth more than it sounds. Every full page load is a new
	// session and a new stream, and a browser only allows about six
	// connections per origin - so a site whose links reload the page runs
	// out of them. A site that navigates within one session uses one.
	//
	// Only mount a subtree handler somewhere the app routed to
	// deliberately. At the server root it would mint a session for every
	// stray GET.
	Subtree bool

	// CheckOrigin decides whether a request that changes something may
	// proceed. Nil compares the Origin header's host against the request's
	// own and allows a request that sends none, which is the same rule
	// gorilla/websocket applies.
	//
	// Set it to allow another origin - a page embedding this handler from
	// elsewhere - or to refuse the empty case, which no browser produces but
	// every other client does.
	CheckOrigin func(r *http.Request) bool

	// RequestRate is how many requests per second a session earns back, and
	// RequestBurst the most it may spend at once. Zero means the defaults;
	// a negative RequestRate turns the limit off.
	//
	// The budget is per session and refills continuously, so a page open all
	// day is in the same position as one just opened: there is no window to
	// reset and none to wait out. It rations every transport request a page
	// makes, because each one queues work onto that page's single goroutine
	// - and, if the component publishes, onto every subscriber's.
	//
	// It is not a defence against an anonymous flood: a session id is free
	// to anybody who loads the page, so an attacker takes a fresh one rather
	// than emptying a bucket. What it bounds is what one page can do.
	// PageRate is what bounds how many pages there are.
	RequestRate  float64
	RequestBurst float64

	// PageRate is how many page loads per second one client earns back, and
	// PageBurst the most it may load at once. Zero means the defaults; a
	// negative PageRate turns the limit off.
	//
	// This is the limit the request budget cannot be: the page route is
	// where sessions come from, so a caller refused by a session's bucket
	// only has to take another session. A page load mounts a component
	// tree and keeps it, so a client looping over the page route holds
	// every slot in the registry and locks everybody else out - denial of
	// service assembled from requests that are each entirely legitimate.
	//
	// It is charged per client rather than per session, which means it
	// depends on knowing who the client is. See ClientIP: behind a reverse
	// proxy the default charges the proxy, and one bucket for the whole
	// internet is not a limit anybody wants.
	PageRate  float64
	PageBurst float64

	// ClientIP says who a page load is charged to. Nil reads the address
	// the connection came from, which is right for a handler exposed
	// directly and wrong for every handler behind a proxy: there, every
	// request appears to come from the proxy, so the whole site shares one
	// bucket and PageRate becomes a limit on the server rather than on a
	// client. [ForwardedClientIP] is the answer for that case.
	//
	// It does not have to return an address. Anything that identifies a
	// caller works - an account id from a session cookie, a tenant, an API
	// key - and is used verbatim as the bucket's key. Whatever it returns
	// must be something the caller cannot choose freely, or the limit is
	// one bucket per attempt.
	ClientIP func(r *http.Request) string

	// MaxSignalBytes caps the JSON body an action may carry. Zero means
	// DefaultMaxSignalBytes.
	//
	// Signals are a component's client-side state, so the ceiling only has
	// to cover what the component asked for - a form's worth of fields, not
	// a file. Uploads have their own route and their own limit.
	MaxSignalBytes int64

	// Grace is how long a session outlives its stream, so a reconnect
	// resumes it rather than starting over. Zero means DefaultGrace.
	Grace time.Duration

	// Heartbeat is how often an idle stream writes something harmless.
	// It does two jobs: proxies and load balancers close a connection that
	// has been quiet too long, and a write is the only way the server finds
	// out a client has gone away. Zero uses DefaultHeartbeat; negative
	// disables it.
	Heartbeat time.Duration

	factory  func() Component
	sessions *registry
	presence *presence

	// pages is the per-client page budget. A value rather than a pointer so
	// that it needs nothing from New: its map is built on first use.
	pages clients

	// routes is built on the first request, so Prefix can be set after New.
	once   sync.Once
	routes *http.ServeMux
}

// Shell renders the document a browser first loads, around the component's
// own markup.
type Shell func(w io.Writer, p Page) error

// Page is what a [Shell] needs to build the first response.
type Page struct {
	// Title is Handler.Title.
	Title string

	// Head is Handler.Head: raw HTML for the document head.
	Head string

	// ScriptURL is the Datastar bundle to load as a module.
	ScriptURL string

	// Attach is the expression that opens the page's stream. It belongs in
	// a data-init attribute on <body>, HTML-escaped like any attribute
	// value, and it must not sit inside the component's markup - anything
	// there is re-initialised by every patch and would open a second
	// stream.
	Attach string

	// Body is the component's rendered markup, to be written as-is.
	Body string

	// Scripts is shuttle's own required markup - currently the shim that
	// reports the back and forward buttons, which the server has no other
	// way to see. Write it into the document, or lose those buttons.
	Scripts string
}

// New returns a Handler serving component instances produced by factory.
// One instance is created per page load and lives as long as that page
// stays connected.
func New(factory func() Component) *Handler {
	h := &Handler{
		Title:     "shuttle",
		ScriptURL: DefaultScriptURL,
		Broker:    NewMemoryBroker(),
		factory:   factory,
		sessions:  newRegistry(),
		presence:  newPresence(),
	}

	return h
}

// mux builds the routes on first use, so Prefix can be set after New.
func (h *Handler) mux() *http.ServeMux {
	h.once.Do(func() {
		base := strings.TrimSuffix(h.Prefix, "/")

		m := http.NewServeMux()
		m.HandleFunc("GET "+base+routePrefix+"/live", private(h.serveStream))
		// The three routes that change something are the three that get the
		// origin check.
		m.HandleFunc("POST "+base+routePrefix+"/act/{node}/{aid}",
			private(h.sameOrigin(h.budgeted(h.serveAction))))
		m.HandleFunc("POST "+base+routePrefix+"/nav",
			private(h.sameOrigin(h.budgeted(h.serveNav))))
		m.HandleFunc("POST "+base+routePrefix+"/upload/{node}/{name}",
			private(h.sameOrigin(h.budgeted(h.serveUpload))))
		// Session-free on purpose: a page whose own session has died polls
		// this until the server answers, and must not mint a session per
		// attempt.
		m.HandleFunc("GET "+base+routePrefix+"/up", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
		})
		if h.Subtree {
			// Every path under the mount point is this handler's page, so one
			// session can be a whole site. Only safe because the app routed
			// here deliberately: mounted at the server root it would mint a
			// session for every /favicon.ico a crawler tries.
			m.HandleFunc("GET "+base+"/", h.metered(h.servePage))
		} else {
			// Exactly the mount point. A catch-all mints a session for every
			// stray GET, and each one costs a component tree until it is
			// collected.
			m.HandleFunc("GET "+base+"/{$}", h.metered(h.servePage))
		}
		h.routes = m
	})
	return h.routes
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Debug {
		h.log().Info("shuttle: request",
			"method", r.Method, "path", r.URL.Path, "length", r.ContentLength)
	}
	h.mux().ServeHTTP(w, r)
}

// Close ends the session with this id: its components unmount, its
// subscriptions, timers and presence stop, and its stream ends. It reports
// whether there was one.
//
// The page recovers by itself and that is the point. Datastar reconnects,
// finds a session this server no longer has, and gets the same answer a
// restart gives it - reload - which sends the browser back through
// whatever middleware fronts this handler. So logging out is Close plus the
// authentication you already have, rather than a second way to say no.
//
// Safe to call from anywhere, including from inside an action: closing does
// not wait on the session's goroutine.
func (h *Handler) Close(sid string) bool {
	if _, ok := h.sessions.get(sid); !ok {
		return false
	}
	h.sessions.remove(sid)
	return true
}

// CloseOwner ends every session labelled owner by [Session.SetOwner], and
// returns how many. It is what logging out calls: one person can have the
// same page open in four tabs, and each is its own session.
func (h *Handler) CloseOwner(owner string) int {
	if owner == "" {
		// Every unlabelled session matches "", which would log out the
		// pages of everyone who never called SetOwner.
		return 0
	}
	var n int
	for _, sid := range h.sessions.ownedBy(owner) {
		if h.Close(sid) {
			n++
		}
	}
	return n
}

// private marks a transport response as belonging to one session and no
// other.
//
// Every one of these routes answers for the session named in
// [SessionHeader], so two requests to the same URL are two different
// answers. A cache keying on the URL alone would be entitled to hand one
// page's response to another - Vary is what says it may not, and no-store
// says not to keep it at all.
func private(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", SessionHeader)
		next(w, r)
	}
}

// budgeted refuses a request the session has no tokens left for.
//
// It runs before the handler rather than inside it so that a refusal costs
// a map lookup and a mutex - nothing is decoded, no work is queued, and the
// session's goroutine never hears about it. A request naming no live
// session passes through to be refused as the 404 it is.
func (h *Handler) budgeted(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := h.sessionOf(r)
		if ok && !sess.allow(h.requestRate(), h.requestBurst(), time.Now()) {
			h.log().Warn("shuttle: request budget exhausted",
				"session", sess.Tag(), "path", r.URL.Path)
			// A second is the whole burst back at the default rate, and the
			// client is a browser that will not read it anyway - it is for
			// whatever is between us.
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// sameOrigin rejects a cross-origin request before it reaches a handler
// that changes something.
//
// It is defence in depth rather than the thing standing between a stranger
// and this page: the session id is an unguessable capability read out of
// the page by script, not a cookie the browser attaches by itself, so a
// forged cross-site POST has nothing to replay. What this guards is the
// app that adds its own cookie authentication alongside shuttle and
// reintroduces exactly the ambient authority the design does without.
func (h *Handler) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.originOK(r) {
			h.log().Warn("shuttle: cross-origin request refused",
				"origin", r.Header.Get("Origin"), "path", r.URL.Path)
			http.Error(w, "cross-origin request", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// originOK is the check itself, and follows gorilla/websocket's
// checkSameOrigin: compare hosts, and allow a request that sends no Origin
// at all.
//
// Allowing the empty case is deliberate. Browsers send Origin on every
// fetch that is not a plain GET, so a POST without one did not come from a
// page - it came from curl, a test, or a service - and cannot be a forgery
// on somebody's behalf. Refusing it would break every non-browser caller to
// prevent nothing.
//
// Only the host is compared, not the scheme: an app served over both ends
// up refusing itself otherwise. Set CheckOrigin to be stricter, or to allow
// the one other origin an embedded page is served from.
func (h *Handler) originOK(r *http.Request) bool {
	if h.CheckOrigin != nil {
		return h.CheckOrigin(r)
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// refusals are the errors whose text may be written into a response. Every
// one is a constant of this package's own describing a rule the client
// broke, so it tells the client something it needs and discloses nothing.
//
// Everything else gets a generic message. An error from application code -
// or worse, [ErrPanic], which carries the panic value - can name a table, a
// path on disk or whatever was in scope when it failed, and an HTTP body is
// the one place that must never end up. The log still gets all of it.
var refusals = []error{
	ErrNoSession, ErrNoAction, ErrNoUpload, ErrNoFiles,
	ErrTooManyFiles, ErrFileTooLarge, ErrFileType, ErrAlreadyAttached,
	ErrTooManySessions, errBadSignals,
}

// fail logs err and answers with a message that is safe to show.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, what string, err error, extra ...any) {
	h.log().Error("shuttle: "+what,
		append([]any{"path", r.URL.Path, "err", err}, extra...)...)
	http.Error(w, publicMessage(status, err), status)
}

// publicMessage is what the client is told: the refusal itself when the
// error is one, and otherwise nothing but the status.
func publicMessage(status int, err error) string {
	for _, refusal := range refusals {
		if errors.Is(err, refusal) {
			return refusal.Error()
		}
	}
	if status == http.StatusBadRequest {
		return "bad request"
	}
	return http.StatusText(status)
}

// SessionHeader carries the session id on every transport request.
//
// It is a header rather than a path segment because a URL is the most
// copied string in a system: access logs, proxy logs, APM spans and error
// trackers all record one, and the session id is the capability for the
// whole page. Redacting shuttle's own logs only ever fixed the half of that
// this process controls.
//
// It buys a second thing. A cross-origin request cannot set a custom header
// without a CORS preflight, and nothing here answers one - so this header
// can only be attached by a page served from the same origin, whatever an
// attacker knows.
const SessionHeader = "Shuttle-Session"

// sessionHeaderExpr is the headers option every request datastar makes on
// this session's behalf carries. Datastar merges it over its own defaults,
// for a GET stream as readily as for a POST.
//
// The id is a hex string from crypto/rand, so it needs no escaping to sit
// inside a single-quoted Datastar expression - but it is written through
// %q on the way in, so the day that stops being true this breaks loudly
// rather than quietly emitting a broken attribute.
func sessionHeaderExpr(s *Session) string {
	return fmt.Sprintf(`{%q: %q}`, SessionHeader, s.ID())
}

// sessionOf returns the session a transport request names, or false. The id
// arrives in [SessionHeader]; a request without one names nothing, which is
// the same answer as naming a session that has gone.
func (h *Handler) sessionOf(r *http.Request) (*Session, bool) {
	id := r.Header.Get(SessionHeader)
	if id == "" {
		return nil, false
	}
	return h.sessions.get(id)
}

// checkIDs reports duplicate element ids in a render. A duplicate is
// excluded from Datastar's persistent-id set, which drops that subtree to
// soft matching with no error anywhere - so in Debug it is worth saying so.
func (h *Handler) checkIDs(sess *Session, markup string) {
	if !h.Debug {
		return
	}
	if dupes := DuplicateIDs(markup); dupes != nil {
		h.log().Warn("shuttle: duplicate element ids will degrade morphing",
			"session", sess.Tag(), "ids", dupes)
	}
}

func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func (h *Handler) broker() Broker {
	if h.Broker != nil {
		return h.Broker
	}
	return NewMemoryBroker()
}

// servePage is the first phase: a complete document, rendered on the
// server, that works before any script runs.
func (h *Handler) servePage(w http.ResponseWriter, r *http.Request) {
	sess, err := h.sessions.create(h.factory(), h.broker(), h.presence, h.sessionError)
	if err != nil {
		h.fail(w, r, http.StatusServiceUnavailable, "cannot start a session", err)
		return
	}

	ctx := r.Context()
	sess.params = Params(r.URL.Query())
	sess.prefix = strings.TrimSuffix(h.Prefix, "/")

	// The address bar already says this, so record it as the baseline: a
	// component syncing its URL state should not move a browser that is
	// already where it belongs.
	sess.urlPath = r.URL.Path
	sess.url = sess.path(r.URL.Query())

	// Mount, HandleParams and the first render run on the session's own
	// goroutine, like every other piece of component work - Mount may
	// subscribe to a topic, and a message could arrive before the render
	// finishes.
	var body string
	err = sess.call(func() error {
		if m, ok := sess.Component().(Mounter); ok {
			if err := m.Mount(ctx, sess.Params()); err != nil {
				return fmt.Errorf("mount: %w", err)
			}
		}
		// HandleParams runs on the first render too, so a component reading
		// the URL needs only the one hook.
		if p, ok := sess.Component().(ParamsHandler); ok {
			if err := p.HandleParams(ctx, sess.Params()); err != nil {
				return fmt.Errorf("params: %w", err)
			}
		}
		var err error
		body, err = sess.Render(ctx)
		return err
	})
	if err != nil {
		h.sessions.remove(sess.ID())
		h.log().Error("shuttle: starting the page failed", "err", err)
		http.Error(w, "cannot render", http.StatusInternalServerError)
		return
	}

	h.checkIDs(sess, body)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The document embeds a session id, which is this page's capability. It
	// must never land in a shared cache.
	w.Header().Set("Cache-Control", "no-store")

	shell := h.Shell
	if shell == nil {
		shell = DefaultShell
	}
	page := Page{
		Title:     h.Title,
		Head:      h.Head,
		ScriptURL: h.ScriptURL,
		Attach:    attachExpr(sess, h.openWhenHidden()),
		Body:      body,
		Scripts:   clientScript(sess),
	}
	if err := shell(w, page); err != nil {
		h.log().Error("shuttle: writing page failed", "err", err)
	}
}

// DefaultShell writes a minimal document: the Datastar module, whatever
// Head carries, and the component inside a body that opens the stream.
func DefaultShell(w io.Writer, p Page) error {
	_, err := fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<script type="module" src="%s"></script>
%s%s</head>
<body data-init="%s">
%s
</body>
</html>
`,
		html.EscapeString(p.Title),
		html.EscapeString(p.ScriptURL),
		p.Head,
		p.Scripts,
		html.EscapeString(p.Attach),
		p.Body,
	)
	return err
}

// attachExpr is the expression that opens the page's persistent stream.
//
// retry: 'always' is not tuning. The default 'auto' retries transport
// errors but not a clean 200 close, so if a handler ever returns, the
// client silently stops reconnecting.
//
// openWhenHidden is a real choice rather than a fixed answer, and
// [Handler.OpenWhenHidden] carries the reasoning: a hidden tab that keeps
// its stream open holds one of the browser's ~6 connections per origin for
// as long as it exists.
//
// It hangs off data-init rather than a load event because Datastar v1 has
// no data-on:load; data-init runs when the attribute is initialised. It
// belongs on <body>, outside the component's morph target, or every patch
// would re-initialise it and open a second stream.
func attachExpr(s *Session, openWhenHidden bool) string {
	return fmt.Sprintf(`@get('%s/live', {openWhenHidden: %t, retry: 'always', headers: %s})`,
		s.prefix+routePrefix, openWhenHidden, sessionHeaderExpr(s))
}

// serveStream is the second phase: one persistent SSE stream per page,
// held open for the session's life. Every patch the client receives -
// action responses, server pushes, later pub/sub - comes down this one
// connection, which is also the only ordering guarantee available.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sessionOf(r)
	if !ok {
		// The page is holding a session this server has never heard of -
		// it was evicted, or the server restarted under it. A 404 would be
		// honest and useless: Datastar retries, and no amount of retrying
		// will conjure the session back, so the page would sit there
		// reconnecting to nothing until it gave up for good.
		//
		// Answer with a real stream that tells the page to start over.
		h.tellToReload(w, r)
		return
	}

	if err := sess.attach(); err != nil {
		http.Error(w, publicMessage(http.StatusConflict, err), http.StatusConflict)
		return
	}
	h.sessions.hold(sess.ID())

	defer func() {
		sess.detach()
		// Outlive the disconnect: Datastar reconnects on its own, and the
		// component's state should still be here when it does.
		h.sessions.expireIn(sess.ID(), h.grace())
	}()

	sse := datastar.NewSSE(w, r)
	if err := sess.stream(sse, h.heartbeat()); err != nil {
		h.log().Debug("shuttle: stream ended", "session", sess.Tag(), "err", err)
	}
}

// tellToReload answers a stream request for a session that no longer
// exists, by opening a stream just long enough to send the page home.
//
// Reloading is the whole recovery story for a lost session: the state was
// in memory and the memory is gone, so the only way back is a fresh mount
// from the URL. That is also why Mount has to be cheap and idempotent - it
// is what makes a rolling deploy survivable.
func (h *Handler) tellToReload(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	// A beat of delay, so a server that is failing every request cannot turn
	// this into a reload loop running as fast as the network allows.
	if err := sse.ExecuteScript(`setTimeout(() => location.reload(), 1000)`); err != nil {
		h.log().Debug("shuttle: could not send a reload", "err", err)
	}
}

// openWhenHidden reports the setting, defaulting to true.
func (h *Handler) openWhenHidden() bool {
	return h.OpenWhenHidden == nil || *h.OpenWhenHidden
}

func (h *Handler) requestRate() float64 {
	if h.RequestRate == 0 {
		return DefaultRequestRate
	}
	return h.RequestRate
}

func (h *Handler) requestBurst() float64 {
	if h.RequestBurst <= 0 {
		return DefaultRequestBurst
	}
	return h.RequestBurst
}

func (h *Handler) maxSignalBytes() int64 {
	if h.MaxSignalBytes <= 0 {
		return DefaultMaxSignalBytes
	}
	return h.MaxSignalBytes
}

func (h *Handler) grace() time.Duration {
	if h.Grace > 0 {
		return h.Grace
	}
	return DefaultGrace
}

func (h *Handler) heartbeat() time.Duration {
	if h.Heartbeat == 0 {
		return DefaultHeartbeat
	}
	return h.Heartbeat
}

// serveAction runs one action and pushes the re-render.
//
// The response itself carries nothing: the morph goes down the page's
// stream, because one writer is what gives patches a total order. Separate
// one-shot responses would have none.
func (h *Handler) serveAction(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sessionOf(r)
	if !ok {
		http.Error(w, ErrNoSession.Error(), http.StatusNotFound)
		return
	}

	// Signals are client-controlled and decoded into memory whole, so the
	// body needs a ceiling the way navigation and uploads already have one.
	// Without it a single POST is a way to exhaust the process.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxSignalBytes())

	n, ok := sess.node(r.PathValue("node"))
	if !ok {
		http.Error(w, ErrNoAction.Error(), http.StatusNotFound)
		return
	}

	// The action runs on the session's goroutine, so it cannot be inside the
	// component at the same time as a pub/sub message or a timer tick. The
	// re-render is marked in the same work item and happens right after it.
	err := sess.call(func() error {
		if err := h.invoke(n, r); err != nil {
			return err
		}
		// Only the component that handled the event re-renders. An event on
		// a child must not redraw its parent.
		return n.push(r.Context())
	})
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrNoAction):
			status = http.StatusNotFound
		case errors.Is(err, errBadSignals):
			// Signals are client-controlled input, so a payload that will not
			// decode is a bad request, not a server fault.
			status = http.StatusBadRequest
		}
		h.fail(w, r, status, "action failed", err,
			"session", sess.Tag(), "node", r.PathValue("node"),
			"action", r.PathValue("aid"))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// serveNav handles the back and forward buttons: the only part of
// navigation the server cannot see for itself. The page reports where
// history moved to, and the re-render comes back down the stream.
func (h *Handler) serveNav(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sessionOf(r)
	if !ok {
		http.Error(w, ErrNoSession.Error(), http.StatusNotFound)
		return
	}

	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		http.Error(w, "bad navigation payload", http.StatusBadRequest)
		return
	}

	u, err := url.Parse(body.URL)
	if err != nil {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}

	// The browser has already moved; record where, so the next render does
	// not try to move it back.
	sess.mu.Lock()
	sess.url = u.String()
	sess.mu.Unlock()

	if err := sess.call(func() error {
		return sess.applyLocation(r.Context(), u)
	}); err != nil {
		h.fail(w, r, http.StatusInternalServerError, "navigation failed", err,
			"session", sess.Tag())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// serveUpload receives files on their own request, since the stream only
// goes one way and cannot carry them.
//
// Everything here is checked server-side even though the picker was told
// the same rules: accept attributes and size limits in the page are a
// courtesy to the user, and nothing more.
func (h *Handler) serveUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sessionOf(r)
	if !ok {
		http.Error(w, ErrNoSession.Error(), http.StatusNotFound)
		return
	}
	n, ok := sess.node(r.PathValue("node"))
	if !ok {
		http.Error(w, ErrNoUpload.Error(), http.StatusNotFound)
		return
	}

	name := r.PathValue("name")
	spec, ok := uploadFor(n.cmp, name)
	if !ok {
		http.Error(w, ErrNoUpload.Error(), http.StatusNotFound)
		return
	}
	if _, ok := n.cmp.(UploadHandler); !ok {
		h.log().Error("shuttle: upload has nowhere to go",
			"session", sess.Tag(), "upload", name, "err", ErrNoUploadHandler)
		http.Error(w, ErrNoUploadHandler.Error(), http.StatusInternalServerError)
		return
	}

	files, err := h.receiveAll(w, r, spec)
	if err != nil {
		defer discard(files)
		status := http.StatusBadRequest
		if errors.Is(err, ErrFileTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.log().Info("shuttle: upload refused",
			"session", sess.Tag(), "upload", name, "err", err)
		http.Error(w, publicMessage(status, err), status)
		return
	}
	// The handler gets one look at the files; anything worth keeping is its
	// job to copy out.
	defer discard(files)

	err = sess.call(func() error {
		if err := n.cmp.(UploadHandler).HandleUpload(r.Context(), name, files); err != nil {
			return err
		}
		return n.push(r.Context())
	})
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "handling an upload", err,
			"session", sess.Tag(), "upload", name)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// receiveAll streams the request's parts to temp files.
func (h *Handler) receiveAll(w http.ResponseWriter, r *http.Request, spec Upload) ([]*UploadedFile, error) {
	// A hard ceiling on the request as a whole, so a client cannot send one
	// enormous body made of many small parts.
	limit := spec.maxSize()*int64(spec.maxFiles()) + (1 << 20)
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	parts, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("shuttle: not a file upload: %w", err)
	}

	var files []*UploadedFile
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, fmt.Errorf("shuttle: reading upload: %w", err)
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}

		if len(files) >= spec.maxFiles() {
			_ = part.Close()
			return files, fmt.Errorf("%w: at most %d", ErrTooManyFiles, spec.maxFiles())
		}

		// The declared type is recorded, never acted on: receive checks the
		// bytes. Refusing on the label here would reject a genuine file a
		// browser happened to send as application/octet-stream, and would
		// still pass anything an attacker chose to call an image.
		contentType := part.Header.Get("Content-Type")

		f, err := receive(part, part.FileName(), contentType, spec)
		_ = part.Close()
		if err != nil {
			return files, err
		}
		files = append(files, f)
	}

	if len(files) == 0 {
		return nil, ErrNoFiles
	}
	return files, nil
}

// sessionError reports a failure raised on a session's own goroutine, where
// there is no request left to return it to.
func (h *Handler) sessionError(what string, err error) {
	h.log().Error("shuttle: "+what+" failed", "err", err)
}

// invoke runs the action, turning a panicking handler into an error. One
// bad action should cost the client a failed click, not the whole session:
// the component tree is still here and the stream is still open.
func (h *Handler) invoke(n *node, r *http.Request) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%w: action: %v", ErrPanic, v)
		}
	}()

	fn, ok := n.lookup(r.PathValue("aid"))
	if !ok {
		return ErrNoAction
	}

	// Read the signals before running anything, and hand the action only the
	// values of the component that rendered it.
	values, err := readSignals(r, n.path)
	if err != nil {
		return fmt.Errorf("%w: %w", errBadSignals, err)
	}

	return fn(withSignals(r.Context(), values))
}
