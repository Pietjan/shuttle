package shuttle

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/a-h/templ"
	"github.com/pietjan/loom"
	"github.com/starfederation/datastar-go/datastar"
)

// Stream patches one region of a component's markup an item at a time.
//
// It exists because server-held state has a bill attached: a component that
// re-renders a whole collection has to hold that collection, so a ten
// thousand row table costs ten thousand rows of memory for every connected
// tab. A stream lets the component forget the rows and send only the change
// - which is also what makes chat, logs, feeds and infinite scroll
// practical.
//
// The container carries data-ignore-morph, so the component's own
// re-renders leave the streamed items alone. That is the trade: the server
// no longer knows what the list contains, so nothing can rebuild it. A
// client that reconnects outside the grace window gets whatever the
// component renders into the container from scratch.
type Stream struct {
	n    *node
	name string
}

// Stream returns a handle to a container this component renders. The name
// is the same one [Base.Stream]'s container uses for its id, and is scoped
// to this component instance like every other id.
func (b *Base) Stream(name string) *Stream {
	if b.n == nil {
		return nil
	}
	return &Stream{n: b.n, name: name}
}

// ContainerID is the id of the element items are streamed into.
func (s *Stream) ContainerID() string {
	return s.n.path.elementID() + "-" + s.name
}

// ItemID is the element id of one item, derived from its key.
func (s *Stream) ItemID(key string) string {
	return s.ContainerID() + "-" + key
}

// Attrs are the attributes the container element must carry, ready to
// interpolate into markup the component writes itself:
//
//	fmt.Fprintf(w, `<ul%s></ul>`, s.Attrs())
//
// The id is the patch target. data-ignore-morph is what stops the
// component's own re-renders from wiping items it no longer remembers.
//
// For a Loom component as the container - which is most of them, since a
// stream is a list of something - use [Container] instead. A raw string
// cannot be handed to a component that takes options.
func (s *Stream) Attrs() string {
	return fmt.Sprintf(` id=%q data-ignore-morph`, s.ContainerID())
}

// Container makes a Loom component the element items are streamed into,
// carrying the same two attributes [Stream.Attrs] writes:
//
//	timeline.New(shuttle.Container(l.Stream("log"), timeline.Attr))
//
// It is a plain function rather than a method because Go does not allow
// type parameters on methods, and the component package's option type is
// what has to be inferred.
func Container[O ~func(T), T any](s *Stream, attr Attrs[O]) O {
	return bind(attr,
		pair{"id", s.ContainerID()},
		pair{"data-ignore-morph", ""},
	)
}

// Append adds an item to the end of the container.
//
// The rendered markup must carry the item's id - use [Stream.ItemID] - or
// this reports an error rather than sending markup that could never be
// replaced or removed afterwards.
func (s *Stream) Append(ctx context.Context, key string, c templ.Component) error {
	return s.insert(ctx, key, c, datastar.ElementPatchModeAppend)
}

// Prepend adds an item to the front of the container.
func (s *Stream) Prepend(ctx context.Context, key string, c templ.Component) error {
	return s.insert(ctx, key, c, datastar.ElementPatchModePrepend)
}

// Replace re-renders one item in place, leaving the rest of the collection
// untouched.
func (s *Stream) Replace(ctx context.Context, key string, c templ.Component) error {
	html, err := s.item(ctx, key, c)
	if err != nil {
		return err
	}
	return s.send(patch{
		target: s.ItemID(key),
		html:   html,
		mode:   datastar.ElementPatchModeOuter,
	})
}

// Remove deletes one item.
func (s *Stream) Remove(_ context.Context, key string) error {
	return s.send(patch{
		target: s.ItemID(key),
		mode:   datastar.ElementPatchModeRemove,
	})
}

// Clear empties the container.
func (s *Stream) Clear(context.Context) error {
	return s.send(patch{
		target: s.ContainerID(),
		mode:   datastar.ElementPatchModeInner,
	})
}

func (s *Stream) insert(ctx context.Context, key string, c templ.Component, mode datastar.ElementPatchMode) error {
	html, err := s.item(ctx, key, c)
	if err != nil {
		return err
	}
	return s.send(patch{target: s.ContainerID(), html: html, mode: mode})
}

// item renders one item and checks it can be addressed again later.
func (s *Stream) item(ctx context.Context, key string, c templ.Component) (string, error) {
	if c == nil {
		return "", fmt.Errorf("%w: stream %q: nil component for %q", ErrBadComponent, s.name, key)
	}

	var buf bytes.Buffer
	if err := c.Render(loom.NewContext(ctx), &buf); err != nil {
		return "", err
	}

	id := s.ItemID(key)
	if !strings.Contains(buf.String(), `id="`+id+`"`) {
		return "", fmt.Errorf(
			"%w: stream %q: item %q must render id=%q (use Stream.ItemID), or it can never be replaced or removed",
			ErrBadComponent, s.name, key, id)
	}
	return buf.String(), nil
}

// send queues one operation, unless the component has been unmounted.
//
// Stream operations do not go through the dirty set, so they need their own
// check: a timer tick already queued when the component was unmounted would
// otherwise patch a container that is no longer in the page, which Datastar
// reports as PatchElementsNoTargetsFound and the server never hears about.
func (s *Stream) send(p patch) error {
	if !s.n.sess.mounted(s.n) {
		return ErrNotMounted
	}
	return s.n.sess.enqueue(p)
}
