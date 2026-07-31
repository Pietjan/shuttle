package shuttle

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"
)

// Session is one connected page: the component tree, the action tables its
// markup refers to, and the renders waiting for the page's stream.
//
// Everything a client can reach is keyed by the session id, which is
// unguessable. That is what makes action ids safe to number per component
// and per render - see [scope.register].
type Session struct {
	id     string
	root   *node
	params Params
	broker Broker
	pres   *presence
	// prefix is where the handler serving this session is mounted, so the
	// action URLs it renders point back at it rather than at the server
	// root.
	prefix string
	// url is the address bar's current contents, as far as the server knows,
	// so a re-render only moves it when something actually changed.
	url string
	// urlPath is the page's path. A handler serving a subtree owns more than
	// one, and the component reading it is how a single session can be a
	// whole site.
	urlPath string

	// onError reports failures raised on the session's own goroutine, where
	// there is no request to return them to. Set by the handler.
	onError func(what string, err error)

	mu sync.Mutex
	// nodes indexes every mounted component by its element id, so an action
	// request can find the one that rendered it.
	nodes map[string]*node
	// pending holds the newest unsent render per component. One slot each
	// rather than a queue: every patch carries that component's complete
	// state, so a slow client should skip intermediate renders instead of
	// accumulating them. This is the fat-morph idiom paying for
	// backpressure.
	pending map[string]string
	// ops are stream operations, kept in order because each one is a
	// distinct change rather than a newer version of the same state.
	ops      []patch
	attached bool
	closed   bool

	// dirty holds components needing a re-render. Marking rather than
	// queueing renders is what makes Push safe to call from anywhere: the
	// set coalesces on its own and can never grow past one entry per
	// component.
	dirty map[string]*node

	// wake has capacity 1 and coalesces: it signals "something pending".
	wake chan struct{}
	done chan struct{}

	// mailbox serialises everything that touches component state. One
	// goroutine per session runs it, so a pub/sub message, a timer tick and
	// a click can never be inside the same component at once - which is why
	// a component's fields need no locks of their own.
	mailbox chan func()
	// nudge has capacity 1 and tells the session goroutine that something
	// was marked dirty without any work being queued.
	nudge chan struct{}
}

// newSession builds a standalone session with its own broker and presence.
// The registry uses newSessionWith to share the handler's instead.
func newSession(id string, cmp Component) *Session {
	return newSessionWith(id, cmp, NewMemoryBroker(), newPresence(), nil)
}

func newSessionWith(id string, cmp Component, broker Broker, pres *presence, onError func(string, error)) *Session {
	s := &Session{
		id:      id,
		broker:  broker,
		pres:    pres,
		onError: onError,
		nodes:   map[string]*node{},
		pending: map[string]string{},
		dirty:   map[string]*node{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		mailbox: make(chan func(), 64),
		nudge:   make(chan struct{}, 1),
	}
	s.root = newNode(s, nil, nil, "", cmp)
	s.nodes[s.root.path.elementID()] = s.root

	go s.run()
	return s
}

// run is the session's own goroutine: it executes queued work one item at a
// time and re-renders whatever that work dirtied.
func (s *Session) run() {
	for {
		select {
		case fn := <-s.mailbox:
			s.safely("work", fn)
		case <-s.nudge:
		case <-s.done:
			return
		}
		s.safely("render", s.renderDirty)
	}
}

// safely runs fn on the session's goroutine, turning a panic into a report.
//
// Without this a panicking component takes the goroutine with it and the
// session becomes a zombie: its mailbox is never drained again, so every
// later click hangs rather than failing. One bad render should cost the
// client that render.
func (s *Session) safely(what string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			s.fail(what, fmt.Errorf("%w: %v", ErrPanic, v))
		}
	}()
	fn()
}

// submit queues work for the session's goroutine. It does not wait, so it
// is safe to call from inside other work - including from an action, where
// waiting would be waiting on itself.
func (s *Session) submit(fn func()) error {
	select {
	case <-s.done:
		return ErrSessionClosed
	default:
	}

	select {
	case s.mailbox <- fn:
		return nil
	case <-s.done:
		return ErrSessionClosed
	}
}

// call queues work and waits for it. Only ever called from a request
// goroutine: calling it from the session's own goroutine would deadlock.
func (s *Session) call(fn func() error) error {
	result := make(chan error, 1)
	// The result is sent even if fn panics, or the caller would wait on a
	// goroutine that has already given up on it.
	err := s.submit(func() {
		defer func() {
			if v := recover(); v != nil {
				result <- fmt.Errorf("%w: %v", ErrPanic, v)
			}
		}()
		result <- fn()
	})
	if err != nil {
		return err
	}

	select {
	case err := <-result:
		return err
	case <-s.done:
		return ErrSessionClosed
	}
}

// markDirty schedules a component for re-render.
func (s *Session) markDirty(n *node) error {
	id := n.path.elementID()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	// An unmounted component has no element left in the page, and patching
	// one that is not there is a console error the server never hears about.
	// Anything still holding a reference to it - a timer mid-tick, a message
	// already in flight - lands here.
	if _, mounted := s.nodes[id]; !mounted {
		s.mu.Unlock()
		return ErrNotMounted
	}
	s.dirty[id] = n
	s.mu.Unlock()

	select {
	case s.nudge <- struct{}{}:
	default: // already nudged; the set holds the work
	}
	return nil
}

// renderDirty renders everything marked since the last pass, parents first.
// A parent's markup already contains its children, so rendering the parent
// first means a child dirtied by the same work is usually already covered.
func (s *Session) renderDirty() {
	s.mu.Lock()
	if len(s.dirty) == 0 {
		s.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(s.dirty))
	for id := range s.dirty {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	nodes := make([]*node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, s.dirty[id])
		delete(s.dirty, id)
	}
	s.mu.Unlock()

	for _, n := range nodes {
		html, err := n.render(context.Background())
		if err != nil {
			s.fail("render", err)
			continue
		}
		if err := s.queue(n.path.elementID(), html); err != nil {
			return
		}
	}

	// The root's state may name a URL now. Sync after rendering, so the
	// address bar and the markup move together.
	s.syncURL()
}

// fail reports an error raised on the session's own goroutine, where there
// is no request to return it to.
func (s *Session) fail(what string, err error) {
	if s.onError != nil {
		s.onError(what, err)
	}
}

// ID returns the session id. It is the client's capability for this page:
// unguessable, and the only thing standing between a stranger and this
// page's actions.
func (s *Session) ID() string { return s.id }

// RootID returns the id of the element the root component renders into.
func (s *Session) RootID() string { return s.root.path.elementID() }

// Component returns the root component instance. Useful in tests; racy if
// you touch its fields while a stream is running.
func (s *Session) Component() Component { return s.root.cmp }

// Params returns the query parameters the page was loaded with.
func (s *Session) Params() Params { return s.params }

// Path returns the page's current path.
func (s *Session) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.urlPath == "" {
		return s.prefix + "/"
	}
	return s.urlPath
}

// presence returns the roster this session's pages share.
func (s *Session) presence() *presence { return s.pres }

// Broker returns the pub/sub backend this session publishes through.
func (s *Session) Broker() Broker { return s.broker }

// currentURL is where the server believes the address bar points.
func (s *Session) currentURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Render renders the whole tree from the root.
func (s *Session) Render(ctx context.Context) (string, error) {
	return s.root.render(ctx)
}

// Push re-renders the whole tree and hands it to the page's stream.
// Components normally push themselves, which patches only their own
// subtree; this is the page-wide version.
func (s *Session) Push(ctx context.Context) error {
	return s.root.push(ctx)
}

// register indexes a newly mounted component.
func (s *Session) register(n *node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.path.elementID()] = n
}

// unregister drops an unmounted component, along with any render of it
// still waiting to be sent.
func (s *Session) unregister(n *node) {
	id := n.path.elementID()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	delete(s.pending, id)
}

// node finds a mounted component by its node id ("c", "c-1").
func (s *Session) node(nodeID string) (*node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[idPrefix+nodeID]
	return n, ok
}

// queue holds a render for the stream, replacing any older one for the same
// component.
func (s *Session) queue(elementID, html string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	s.pending[elementID] = html
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default: // already signalled; the slot holds the newer render
	}
	return nil
}

// take removes every pending render, parents before children.
//
// The order matters when both are waiting: a parent's markup already
// contains its children, so applying the parent first and the child second
// converges either way, but patching a child into a subtree its parent is
// about to replace is wasted work.
func (s *Session) take() []patch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 && len(s.ops) == 0 {
		return nil
	}

	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	// Lexicographic order puts "shuttle-c" before "shuttle-c-1".
	sort.Strings(ids)

	out := make([]patch, 0, len(ids)+len(s.ops))
	for _, id := range ids {
		out = append(out, patch{
			target: id,
			html:   s.pending[id],
			mode:   datastar.ElementPatchModeOuter,
		})
		delete(s.pending, id)
	}

	// Stream operations after the re-renders. A container carries
	// data-ignore-morph, so a component's re-render cannot disturb what was
	// streamed into it and the two are independent.
	out = append(out, s.ops...)
	s.ops = nil

	return out
}

// patch is one piece of markup and what to do with it, or a script to run.
type patch struct {
	target string
	html   string
	mode   datastar.ElementPatchMode
	// script, when set, is JavaScript to run on the page instead of markup
	// to patch. It is how navigation works: Datastar's own URL attributes
	// are paid, so the history calls travel down the stream as a script.
	script string
}

// mounted reports whether a component is still part of the tree.
func (s *Session) mounted(n *node) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.nodes[n.path.elementID()]
	return ok
}

// enqueue adds a stream operation.
//
// Ordered rather than coalesced, unlike a component's re-render: two
// appends to the same container are two different items, not one state
// superseding another.
func (s *Session) enqueue(p patch) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	s.ops = append(s.ops, p)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// Invoke runs an action on the component that rendered it.
func (s *Session) Invoke(ctx context.Context, nodeID, actionID string) error {
	n, ok := s.node(nodeID)
	if !ok {
		return ErrNoAction
	}
	fn, ok := n.lookup(actionID)
	if !ok {
		return ErrNoAction
	}
	return fn(ctx)
}

// attach claims the session's single stream slot.
func (s *Session) attach() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionClosed
	}
	if s.attached {
		return ErrAlreadyAttached
	}
	s.attached = true
	return nil
}

// isAttached reports whether a stream currently holds the session.
func (s *Session) isAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached
}

// detach releases the stream slot.
func (s *Session) detach() {
	s.mu.Lock()
	s.attached = false
	s.mu.Unlock()
}

// stream writes pending renders to the page until the connection or the
// session ends. It is the session's only writer, which is what gives
// patches a total order - separate one-shot requests have none.
func (s *Session) stream(sse *datastar.ServerSentEventGenerator, heartbeat time.Duration) error {
	var beat <-chan time.Time
	if heartbeat > 0 {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		beat = t.C
	}

	// Say hello before waiting for anything to say. The client cannot tell a
	// live stream from a stalled one until bytes arrive, so a page that has
	// just reconnected goes on showing "connection lost" over a connection
	// that is already working - for up to a whole heartbeat, which is 25
	// seconds by default. One write closes that gap: the shim treats a patch
	// as proof the stream is alive, and this is the first one.
	//
	// It also gets bytes past any intermediary that would otherwise sit on
	// the response until something arrives.
	if err := sse.PatchSignals([]byte("{}")); err != nil {
		return err
	}

	for {
		// Drain first: a render may already be waiting from before the
		// stream attached, or from the reconnect gap.
		for _, p := range s.take() {
			var err error
			switch {
			case p.script != "":
				err = sse.ExecuteScript(p.script)
			case p.mode == datastar.ElementPatchModeRemove:
				err = sse.RemoveElementByID(p.target)
			default:
				err = sse.PatchElements(p.html,
					datastar.WithSelectorID(p.target),
					datastar.WithMode(p.mode),
				)
			}
			if err != nil {
				return err
			}
		}

		select {
		case <-s.wake:
		case <-beat:
			// An empty signal patch changes nothing on the client. The point
			// is the write itself: it keeps intermediaries from closing an
			// idle stream, and it is the only way this end finds out the
			// client has gone.
			if err := sse.PatchSignals([]byte("{}")); err != nil {
				return err
			}
		case <-s.done:
			return nil
		case <-sse.Context().Done():
			return nil
		}
	}
}

// close tears the session down, unmounting the whole tree and stopping its
// subscriptions and timers. Idempotent.
func (s *Session) close(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	close(s.done)

	// Every component's own teardown - its subscriptions, timers and
	// presence - runs as the tree unmounts, so there is nothing session-wide
	// left to stop.
	s.root.close(ctx)
}
