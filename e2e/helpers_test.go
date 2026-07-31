//go:build e2e

package e2e

import (
	"context"
	"io"

	"github.com/a-h/templ"
)

// The same three combinators the live kit uses, because composing loom
// parts in plain Go is otherwise all boilerplate.

func seq(parts ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		for _, part := range parts {
			if part == nil {
				continue
			}
			if err := part.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

func with(parent templ.Component, children ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return parent.Render(templ.WithChildren(ctx, seq(children...)), w)
	})
}

func text(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, templ.EscapeString(s))
		return err
	})
}
