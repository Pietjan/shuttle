package shuttle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/a-h/templ"
	"github.com/pietjan/loom"
)

// node is one mounted component instance: its own state, its own action
// table, its own morph target, its own children.
//
// The per-node action table is what makes a scoped re-render possible. A
// session-wide table would be replaced wholesale on every render, so a
// child re-rendering would invalidate every action its parent had just
// handed the client.
type node struct {
	sess   *Session
	parent *node
	path   path
	key    string
	cmp    Component

	mu sync.Mutex
	// gen counts this node's shipped renders, and is part of every action
	// id it hands out, so a click against markup a morph has replaced runs
	// the closure it was rendered for rather than whatever now sits in
	// that position. It advances only when a render produced new bytes -
	// an unchanged render keeps the generation, which is what lets it keep
	// the bytes.
	gen       uint64
	cur, prev map[string]Action
	// curSent records whether gen's markup was actually handed to the
	// client. Renders coalesce, so a generation minted while the stream was
	// behind can be superseded before anyone saw it - and granting such a
	// generation grace would push out the one the client is really holding.
	// prev only ever holds the last generation that was sent.
	curSent bool
	// last is the markup of the last adopted render, the bytes the client
	// holds. A re-render is tried against it under the same generation
	// first: equal bytes mean nothing changed, so nothing needs pushing
	// and no new generation needs minting.
	last string

	// closed marks a node whose teardown has run, so anything registering
	// cleanup after that runs it immediately instead of appending to a list
	// nobody will ever read again.
	closed bool

	// children are keyed by the caller's key; order fixes each key's path
	// index so a child keeps its ids when its siblings come and go.
	children map[string]*node
	order    map[string]int
	next     int

	// cleanup stops what this component started - subscriptions, timers,
	// presence - when it is unmounted. On the session rather than the node
	// it would outlive the component: a child's timer would keep firing
	// after its key stopped being rendered, pushing renders at a node that
	// no longer exists.
	cleanup []func()
}

// onClose registers teardown to run when this component is unmounted. On a
// node already unmounted - a Do closure subscribing after the component's
// key stopped being rendered - the teardown runs immediately, because the
// list it would join has already been drained and the subscription or timer
// would otherwise outlive its component for the rest of the session.
func (n *node) onClose(fn func()) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		fn()
		return
	}
	n.cleanup = append(n.cleanup, fn)
	n.mu.Unlock()
}

func newNode(sess *Session, parent *node, p path, key string, cmp Component) *node {
	n := &node{
		sess:     sess,
		parent:   parent,
		path:     p,
		key:      key,
		cmp:      cmp,
		cur:      map[string]Action{},
		prev:     map[string]Action{},
		children: map[string]*node{},
		order:    map[string]int{},
	}
	if b, ok := cmp.(binder); ok {
		b.bind(n)
	}
	return n
}

// render renders this component into its wrapper and installs the action
// table its markup refers to.
//
// The two steps belong together: bindings register themselves while the
// tree is being built, so a table is only correct for the markup produced
// by the same pass.
func (n *node) render(ctx context.Context) (string, error) {
	n.mu.Lock()
	gen, last := n.gen, n.last
	n.mu.Unlock()

	// First, a pass under the generation the client already holds. Equal
	// bytes mean the state did not move: the fresh table is adopted - same
	// ids, closures over the same state - and the cached markup goes back
	// so the stream can recognise it and drop the patch. The generation
	// stays put, which is what keeps the client's action ids resolving for
	// as long as nothing changes.
	if last != "" {
		html, sc, err := n.renderPass(ctx, gen)
		if err != nil {
			return "", err
		}
		if html == last {
			n.mu.Lock()
			n.cur = sc.table
			n.mu.Unlock()
			n.prune(ctx, sc.seen)
			return last, nil
		}
	}

	// Something changed, so this render ships - under a new generation,
	// because position n of the new markup is not necessarily the same
	// button as position n of the old, and a click already in flight must
	// run the closure it was rendered for. That costs a second pass, paid
	// only on change; the table the client clicked against until now moves
	// to prev, so a stale click still resolves however long the markup sat
	// unchanged before this.
	n.mu.Lock()
	n.gen++
	gen = n.gen
	n.mu.Unlock()

	html, sc, err := n.renderPass(ctx, gen)
	if err != nil {
		return "", err
	}

	n.mu.Lock()
	// Grace goes to the generation the client saw, not to the newest one
	// minted. A cur that was never sent - superseded while the stream was
	// behind - is discarded outright, and prev keeps covering whatever was
	// last on the wire.
	if n.curSent {
		n.prev = n.cur
	}
	n.cur = sc.table
	n.curSent = false
	n.last = html
	n.mu.Unlock()

	// A child the parent stopped rendering is gone: unmount it rather than
	// leaving its state, and its action table, alive for the session.
	n.prune(ctx, sc.seen)

	return html, nil
}

// renderPass builds this component's markup once under the given
// generation, returning it with the pass's scope. It touches none of the
// node's tables - adopting a pass is render's decision, not the pass's.
func (n *node) renderPass(ctx context.Context, gen uint64) (string, *scope, error) {
	sc := &scope{node: n, gen: gen, table: map[string]Action{}, seen: map[string]bool{}}

	// A fresh Loom ID counter per node, so a component rendered on its own
	// produces the ids it had inside the page.
	rctx := withScope(loom.NewContext(ctx), sc)

	// The body renders into its own buffer, so a component that fails
	// halfway does not leave half its markup in the patch.
	body, err := n.renderBody(rctx)
	if err != nil {
		fb, ok := n.cmp.(Fallback)
		if !ok {
			return "", nil, err
		}
		// The boundary: one component's failure costs that component, not
		// the page. A fallback that also fails is not worth a second chance.
		body, err = renderSafely(rctx, fb.RenderError(rctx, err))
		if err != nil {
			return "", nil, err
		}
	}

	// After the body, because the body is what says which indicators this
	// render used - and before it in the output, which is the point.
	var buf bytes.Buffer
	if err := n.writeRoot(&buf, sc); err != nil {
		return "", nil, err
	}
	buf.WriteString(body)
	buf.WriteString(`</div>`)

	return buf.String(), sc, nil
}

// renderBody renders the component itself, without its wrapper.
func (n *node) renderBody(ctx context.Context) (string, error) {
	c, err := build(ctx, n.cmp)
	if err != nil {
		return "", err
	}
	return renderSafely(ctx, c)
}

// build calls Render, turning a panic into an error.
func build(ctx context.Context, cmp Component) (c templ.Component, err error) {
	defer func() {
		if v := recover(); v != nil {
			c, err = nil, fmt.Errorf("%w: Render: %v", ErrPanic, v)
		}
	}()
	return cmp.Render(ctx), nil
}

// renderSafely renders a component to a string, turning a panic into an
// error. templ components run arbitrary application code, so a panic here
// is a bug in a component rather than in the framework - but it must not
// take the session's goroutine with it.
func renderSafely(ctx context.Context, c templ.Component) (s string, err error) {
	if c == nil {
		return "", nil
	}
	defer func() {
		if v := recover(); v != nil {
			s, err = "", fmt.Errorf("%w: rendering: %v", ErrPanic, v)
		}
	}()

	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// writeRoot opens the component's wrapper: its morph target, and where its
// signals are declared.
func (n *node) writeRoot(buf *bytes.Buffer, sc *scope) error {
	attrs := ""
	if sig, ok := n.cmp.(Signaller); ok {
		var err error
		if attrs, err = signalAttrs(n.path.namespace(), sig.Signals()); err != nil {
			return err
		}
	}
	attrs += sc.indicatorDecls()

	buf.WriteString(`<div id="`)
	buf.WriteString(n.path.elementID())
	buf.WriteString(`" data-shuttle="component"`)
	buf.WriteString(attrs)
	buf.WriteString(`>`)
	return nil
}

// push marks this component for re-render on the session's goroutine. The
// render itself happens there, after the current work item - push never
// renders or sends anything on the caller's goroutine.
//
// Only this component's subtree: an event on a child must not re-render its
// parent, which is the whole point of tracking dirtiness per component
// rather than per page.
//
// It marks rather than renders so that it is safe to call from any
// goroutine, and so that ten pushes in a row cost one render.
func (n *node) push(context.Context) error {
	return n.sess.markDirty(n)
}

// markSent records that this node's current markup reached the client -
// via the stream, the page's first paint, or the testing kit. From here on
// gen is a generation the client may click against, so the next new
// generation moves its table to prev rather than discarding it.
func (n *node) markSent() {
	n.mu.Lock()
	n.curSent = true
	n.mu.Unlock()
}

// lookup finds a registered action, falling back to the previous render's
// table so a click already in flight when the morph landed still resolves.
func (n *node) lookup(actionID string) (Action, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn, ok := n.cur[actionID]
	if !ok {
		fn, ok = n.prev[actionID]
	}
	return fn, ok
}

// child returns the instance mounted under key, creating it on first sight.
//
// The key is the identity rule, and it is the whole rule: the same key
// keeps the same instance and its state across the parent's re-renders, and
// a different key is a different component that mounts fresh. factory runs
// only on that first mount, so props captured in it are mount-time props -
// a parent wanting to update a live child holds a reference and sets its
// fields, which costs nothing when the child is a Go struct in the same
// process.
func (n *node) child(ctx context.Context, key string, factory func() Component) (*node, error) {
	n.mu.Lock()
	if c, ok := n.children[key]; ok {
		n.mu.Unlock()
		return c, nil
	}

	idx, ok := n.order[key]
	if !ok {
		n.next++
		idx = n.next
		n.order[key] = idx
	}
	c := newNode(n.sess, n, n.path.child(idx), key, factory())
	n.children[key] = c
	n.mu.Unlock()

	n.sess.register(c)

	drop := func() {
		n.sess.unregister(c)
		n.mu.Lock()
		delete(n.children, key)
		n.mu.Unlock()
	}

	if m, ok := c.cmp.(Mounter); ok {
		if err := m.Mount(ctx, n.sess.Params()); err != nil {
			drop()
			return nil, err
		}
	}
	// A child sees the URL too. Without this a component whose state comes
	// from its parameters - a table reading its filter and page - would work
	// as a root and quietly render nothing as a child.
	if h, ok := c.cmp.(ParamsHandler); ok {
		if err := h.HandleParams(ctx, n.sess.Params()); err != nil {
			drop()
			return nil, err
		}
	}
	return c, nil
}

// prune unmounts children the latest render did not include.
func (n *node) prune(ctx context.Context, seen map[string]bool) {
	n.mu.Lock()
	var gone []*node
	for key, c := range n.children {
		if !seen[key] {
			gone = append(gone, c)
			delete(n.children, key)
			// The index too: it only existed to keep a mounted child's ids
			// stable, and this child's state is gone with it. Keeping every
			// index ever handed out would grow the map by one entry per key
			// the parent has ever rendered - a feed mounting a child per
			// item makes that the session's memory ceiling.
			delete(n.order, key)
		}
	}
	n.mu.Unlock()

	for _, c := range gone {
		c.close(ctx)
	}
}

// close unmounts this component and everything below it.
func (n *node) close(ctx context.Context) {
	n.mu.Lock()
	n.closed = true
	children := make([]*node, 0, len(n.children))
	for _, c := range n.children {
		children = append(children, c)
	}
	n.children = map[string]*node{}
	n.order = map[string]int{}
	cleanup := n.cleanup
	n.cleanup = nil
	n.mu.Unlock()

	for _, c := range children {
		c.close(ctx)
	}

	// Subscriptions and timers first: neither should be able to deliver
	// into a component that is being unmounted.
	for _, c := range slices.Backward(cleanup) {
		c()
	}

	n.sess.unregister(n)
	if u, ok := n.cmp.(Unmounter); ok {
		// Guarded like a render: session teardown runs a whole tree of
		// these, and one panicking Unmount must not abort the rest or take
		// the session's goroutine with it.
		func() {
			defer func() {
				if v := recover(); v != nil {
					n.sess.fail("unmount", fmt.Errorf("%w: Unmount: %v", ErrPanic, v))
				}
			}()
			u.Unmount(ctx)
		}()
	}
}

// walk visits this node and every node below it.
func (n *node) walk(fn func(*node)) {
	fn(n)
	n.mu.Lock()
	children := make([]*node, 0, len(n.children))
	for _, c := range n.children {
		children = append(children, c)
	}
	n.mu.Unlock()

	for _, c := range children {
		c.walk(fn)
	}
}

// Child mounts a component under the parent's key and renders it in place.
//
//	shuttle.Child(ctx, item.ID, func() shuttle.Component {
//	    return &Row{Item: item}
//	})
//
// The child gets its own state, its own action table and its own morph
// target, so an event on it re-renders it alone. See [node.child] for the
// identity rule the key sets.
func Child(ctx context.Context, key string, factory func() Component) templ.Component {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return templ.NopComponent
	}
	parent := sc.node

	return templ.ComponentFunc(func(rctx context.Context, w io.Writer) error {
		child, err := parent.child(rctx, key, factory)
		if err != nil {
			return err
		}
		sc.saw(key)

		html, err := child.render(rctx)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, html)
		return err
	})
}
