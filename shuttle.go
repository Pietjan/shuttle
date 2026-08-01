// Package shuttle is a stateful, server-driven UI layer for Go: a
// LiveView/Livewire developer experience built on Loom for rendering
// (github.com/pietjan/loom) and Datastar for client/server communication.
//
// A component is an ordinary Go struct. Its state lives in server memory
// for as long as the page is connected, its exported methods are the
// actions, and Render returns a templ.Component:
//
//	type Counter struct {
//	    shuttle.Base
//	    Count int
//	}
//
//	func (c *Counter) Render(ctx context.Context) templ.Component {
//	    return button.New(
//	        button.Primary,
//	        shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
//	            c.Count++
//	            return nil
//	        }),
//	    )
//	}
//
// Because the state never leaves the server, an action can be a closure
// registered per render rather than a named string looked up by
// reflection. That is the single most consequential property of the
// design: handlers are type-checked, they capture loop variables, and the
// client can only invoke actions that were rendered for it.
//
// Serve a component with New:
//
//	http.Handle("/", shuttle.New(func() shuttle.Component { return &Counter{} }))
package shuttle

import (
	"context"
	"errors"
	"net/url"

	"github.com/a-h/templ"
)

// Component is the unit of server-held state. Render is called once per
// render pass with a context carrying that pass's action table, so
// bindings created inside it (OnClick and friends) are registered against
// this session.
//
// Embed [Base] to get the session handle - Push and the rest.
type Component interface {
	Render(ctx context.Context) templ.Component
}

// Mounter is implemented by components that need to initialise state from
// the URL before their first render. Mount runs once per session; a
// reconnect outside the grace window mounts a fresh instance, so keep it
// cheap and idempotent.
type Mounter interface {
	Mount(ctx context.Context, p Params) error
}

// Unmounter is implemented by components that hold resources beyond their
// own memory. Unmount runs once, when the session is evicted.
type Unmounter interface {
	Unmount(ctx context.Context)
}

// Fallback is implemented by components that would rather show something
// than nothing when their own Render fails.
//
// It is the error boundary: without it a failed render is reported and the
// page keeps whatever markup it already had, which is fine on the first
// paint (the request carries a 500) and much less fine afterwards, when the
// only trace is a line in the log. With it, the component's own failure
// costs that component and nothing around it.
//
// RenderError is called with the error Render returned, or with the panic
// it raised.
type Fallback interface {
	RenderError(ctx context.Context, err error) templ.Component
}

// Params carries the query parameters the page was loaded with.
type Params url.Values

// Get returns the first value for key, or "" if there is none.
func (p Params) Get(key string) string { return url.Values(p).Get(key) }

// Has reports whether key is present.
func (p Params) Has(key string) bool { return url.Values(p).Has(key) }

// Action is one server-side event handler. Returning an error leaves the
// component's state as the action left it and reports the failure; it does
// not tear down the page.
type Action func(ctx context.Context) error

// Errors reported by the transport and the session registry.
var (
	// ErrNoSession means the session id is unknown - evicted, expired, or
	// never issued. The client's remedy is to reload the page.
	ErrNoSession = errors.New("shuttle: no such session")

	// ErrNoAction means the action id is not in the live table. Normally a
	// click against markup two or more renders stale.
	ErrNoAction = errors.New("shuttle: no such action")

	// ErrSessionClosed means the session has been evicted.
	ErrSessionClosed = errors.New("shuttle: session closed")

	// ErrAlreadyAttached means a second stream tried to attach to a
	// session that already has one. There is one stream per page.
	ErrAlreadyAttached = errors.New("shuttle: session already attached")

	// ErrNotMounted means a Base method was called on a component that no
	// session owns - usually a zero-value component in a unit test.
	ErrNotMounted = errors.New("shuttle: component not mounted")

	// ErrTooManySessions means the session cap was reached.
	ErrTooManySessions = errors.New("shuttle: too many sessions")

	// ErrNoReceiver means a component emitted an event and no ancestor
	// implements Receiver, so nothing would have handled it.
	ErrNoReceiver = errors.New("shuttle: no ancestor handles emitted events")

	// ErrNotAnInformer means a component subscribed to a topic without
	// implementing Informer, so it could never receive anything.
	ErrNotAnInformer = errors.New("shuttle: component does not implement Informer")

	// ErrBadInterval means a timer was asked for a non-positive interval.
	ErrBadInterval = errors.New("shuttle: interval must be positive")

	// ErrPanic wraps a panic that escaped component code. The session
	// survives it; the caller gets this instead of a dead page.
	ErrPanic = errors.New("shuttle: panic in component code")

	// ErrBadComponent means a component declared something the framework
	// cannot honour - a signal name Datastar would rewrite, a streamed item
	// with no id. It is a programming error, reported rather than guessed
	// around.
	ErrBadComponent = errors.New("shuttle: invalid component declaration")

	// ErrNoFiles means an upload request carried no files.
	ErrNoFiles = errors.New("shuttle: no files in the upload")

	// errBadSignals wraps a signal payload that would not decode.
	errBadSignals = errors.New("shuttle: bad signals")
)

// Attrs is the shape of every Loom package's Attr - button.Attr, card.Attr,
// input.Attr and the other 45 all have this type, instantiated at their own
// Option.
type Attrs[O any] func(key string, val ...string) O

// IDs is the shape of every Loom package's ID option.
type IDs[O any] func(id string) O

// Base gives a component its place in the tree. Embed it - the framework
// fills it in at mount, and the unexported method it carries is what
// distinguishes a shuttle component from any other struct with a Render
// method.
type Base struct {
	n *node
}

// bind is called by the framework at mount. Being unexported, it can only
// be satisfied by embedding Base.
func (b *Base) bind(n *node) { b.n = n }

// Session returns the live session this component belongs to, or nil
// before mount.
func (b *Base) Session() *Session {
	if b.n == nil {
		return nil
	}
	return b.n.sess
}

// Path returns the page's current path, for a component rendering a
// subtree of an app. Empty before mount.
func (b *Base) Path() string {
	if b.n == nil {
		return ""
	}
	return b.n.sess.Path()
}

// Push re-renders this component and sends the result to the page. Safe to
// call from any goroutine - that is the point of holding state on the
// server. Actions do not need it: the transport pushes automatically once
// a handler returns.
//
// Only this component's subtree is re-rendered and patched. A child pushing
// itself does not disturb its parent.
func (b *Base) Push(ctx context.Context) error {
	if b.n == nil {
		return ErrNotMounted
	}
	return b.n.push(ctx)
}

// Emit sends an event to the nearest ancestor that implements [Receiver],
// and re-renders that ancestor. This is how a child talks back to its
// parent without either holding a reference to the other.
//
// Call it from component code. Like [Base.Navigate] it runs the other
// component's handler - [Receiver.HandleEvent] - on the calling goroutine,
// so from anywhere else it races the session. Wrap it in [Base.Do] if you
// are not already inside a callback.
func (b *Base) Emit(ctx context.Context, name string, payload any) error {
	if b.n == nil {
		return ErrNotMounted
	}
	for a := b.n.parent; a != nil; a = a.parent {
		r, ok := a.cmp.(Receiver)
		if !ok {
			continue
		}
		if err := r.HandleEvent(ctx, name, payload); err != nil {
			return err
		}
		return a.push(ctx)
	}
	return ErrNoReceiver
}

// Receiver is implemented by components that handle events emitted by their
// descendants.
type Receiver interface {
	HandleEvent(ctx context.Context, name string, payload any) error
}

// binder is the seam Base implements.
type binder interface{ bind(*node) }
