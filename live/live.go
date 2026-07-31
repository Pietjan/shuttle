// Package live provides the components Loom leaves out because they cannot
// be built without a stateful layer.
//
// Loom's README lists them under "Known limitations", left out rather than
// approximated: combobox/autocomplete ("there is nothing for the
// filter-as-you-type case"), and the patterns that need a script to be
// honest rather than merely present. Shuttle is that script, so these are
// the components it exists to make possible.
//
// They are ordinary shuttle components - state in fields, actions as
// closures - so they mount as children of yours:
//
//	shuttle.Child(ctx, "search", func() shuttle.Component {
//	    return &live.Combobox{Search: findUsers}
//	})
//
// Keyboard behaviour is real focus movement rather than ARIA roles painted
// onto rows. That follows Loom's own reasoning: role="option" on a link
// overrides the link role, so it announces "option" while activating it
// navigates - the wrong interaction, described confidently. Moving focus
// keeps Enter native and every row honest about what it is.
package live

import (
	"context"
	"io"

	"github.com/a-h/templ"
)

// seq renders components one after another.
func seq(parts ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		for _, p := range parts {
			if p == nil {
				continue
			}
			if err := p.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

// with renders parent wrapped around children, which is what a templ block
// does and what composing Loom parts in plain Go otherwise makes verbose.
func with(parent templ.Component, children ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return parent.Render(templ.WithChildren(ctx, seq(children...)), w)
	})
}

// empty renders nothing, for an optional part. seq skips a nil, but a
// component that has to return something needs a value.
var empty = templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })

// text is a plain text node.
func text(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, templ.EscapeString(s))
		return err
	})
}
