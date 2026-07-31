package main

import (
	"context"
	"net/url"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/badge"
	"github.com/pietjan/loom/button"
	"github.com/pietjan/shuttle"
)

// Filters keeps its state in the URL, so the view can be shared and the
// back button works. HandleParams runs on the first render too, so reading
// the URL needs only the one hook.
type Filters struct {
	shuttle.Base
	Team string
}

func (f *Filters) HandleParams(_ context.Context, p shuttle.Params) error {
	f.Team = p.Get("team")
	return nil
}

func (f *Filters) QueryParams() url.Values {
	if f.Team == "" {
		return nil
	}
	return url.Values{"team": {f.Team}}
}

func (f *Filters) Render(ctx context.Context) templ.Component {
	teams := []string{"core", "tools", "docs"}
	buttons := make([]templ.Component, 0, len(teams)+1)

	for _, team := range teams {
		pick := team // captured per row
		opt := button.Outline
		if f.Team == team {
			opt = button.Primary
		}
		buttons = append(buttons, button.New(opt,
			// Replace changes the URL without adding a history entry, the
			// right choice for a filter someone is still adjusting.
			shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
				f.Team = pick
				return nil
			})))
	}
	// Navigate pushes an entry, so the back button undoes this one.
	jump := button.New(button.Outline,
		shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
			return f.Navigate(actx, "/e/navigation/?team=docs")
		}))

	labelled := make([]templ.Component, 0, len(buttons))
	for i, b := range buttons {
		labelled = append(labelled, inside(b, label(teams[i])))
	}

	return group(
		el(`<div class="grid grid-flow-col items-center justify-start gap-2">`, `</div>`, labelled...),
		inside(jump, label("Navigate to docs")),
		inside(badge.New(badge.Blue), label("Showing "+orAll(f.Team))),
	)
}
