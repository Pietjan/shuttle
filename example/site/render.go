package main

import (
	"context"
	"io"

	"github.com/a-h/templ"
)

// The plumbing every example composes with, kept out of the example files
// themselves: each of those is shown in the site's source panel, and what
// belongs there is the example, not the glue underneath it.
//
// templ's `{ ... }` block compiles to children in the context, which is what
// a Loom component renders inside itself. In plain Go that is
// templ.WithChildren, and these are the shapes worth naming.

// label is a bare text node, for a component's children.
func label(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, templ.EscapeString(s))
		return err
	})
}

// group renders several components in order, as one.
func group(parts ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		for _, part := range parts {
			if err := part.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

// inside renders parts as the children of parent.
func inside(parent templ.Component, parts ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return parent.Render(templ.WithChildren(ctx, group(parts...)), w)
	})
}

// el wraps parts in a plain element, for the page's own layout - the bits
// Loom deliberately has no component for.
func el(open, close string, parts ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, open); err != nil {
			return err
		}
		if err := group(parts...).Render(ctx, w); err != nil {
			return err
		}
		_, err := io.WriteString(w, close)
		return err
	})
}

// nothing renders nothing, for an optional part.
var nothing = templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })

// orNothing and orAll are the two "nothing chosen yet" labels the examples
// share.
func orNothing(s string) string {
	if s == "" {
		return "nothing yet"
	}
	return s
}

func orAll(s string) string {
	if s == "" {
		return "everything"
	}
	return s
}
