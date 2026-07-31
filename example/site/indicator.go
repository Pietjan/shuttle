package main

import (
	"context"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

// Saving is the one piece of state the server cannot drive, because the
// whole point of it is the window before the server has answered: whether
// the click is still in flight.
//
// Indicator marks the button with a signal Datastar sets true while a
// request that button started is running, so the pending state costs no
// round trip - there is nothing to ask, and nobody to ask it of.
type Saving struct {
	shuttle.Base
	Saved string
}

func (s *Saving) Render(ctx context.Context) templ.Component {
	// The reference has to be read from the render, not written by hand.
	// This component is a child of the site, so its indicator is
	// ind.c.<n>.saving - a hard-coded "$ind.c.saving" would work in a test
	// that mounted it alone and watch the wrong signal here.
	pending := shuttle.IndicatorRef(ctx, "saving")

	save := button.New(button.Primary,
		shuttle.OnClick(ctx, button.Attr, func(context.Context) error {
			// Faking the slow thing. A real one would be a query, a payment
			// or an API call; what matters is that the action POST does not
			// return until this does.
			time.Sleep(1200 * time.Millisecond)
			s.Saved = time.Now().Format("15:04:05")
			return nil
		}, shuttle.Indicator("saving")),
		button.Attr("data-attr:disabled", pending),
	)

	// data-show is the client answering its own question. The server never
	// learns this line was displayed, and never re-renders to hide it.
	working := text.New(text.Subtle, text.Small, text.Attr("data-show", pending))

	return group(
		inside(save, label("Save")),
		inside(working, label("Working — the server has not answered yet.")),
		inside(text.New(text.Subtle, text.Small),
			label("Last saved: "+orNothing(s.Saved))),
	)
}
