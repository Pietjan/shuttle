package main

import (
	"context"
	"strconv"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/stat"
	"github.com/pietjan/shuttle"
)

// Counter is the whole idea in one component: state is a struct field, and
// an action is a closure over it. Nothing is serialised to the client and
// nothing comes back - the click carries an opaque action id, the closure
// runs here, and the new markup is morphed in.
type Counter struct {
	shuttle.Base
	Count int
}

func (c *Counter) Render(ctx context.Context) templ.Component {
	inc := button.New(button.Primary,
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Count++
			return nil
		}))
	dec := button.New(button.Outline,
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			c.Count--
			return nil
		}))

	return el(`<div class="grid gap-2">`, `</div>`,
		stat.New(stat.Label("Count"), stat.Value(strconv.Itoa(c.Count))),
		el(`<div class="grid grid-flow-col items-center justify-start gap-2">`, `</div>`,
			inside(dec, label("−1")),
			inside(inc, label("+1")),
		),
	)
}
