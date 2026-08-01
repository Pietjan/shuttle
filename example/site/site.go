package main

import (
	"context"
	"strings"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/accordion"
	"github.com/pietjan/loom/callout"
	"github.com/pietjan/loom/heading"
	"github.com/pietjan/loom/icon"
	"github.com/pietjan/loom/navlist"
	"github.com/pietjan/loom/separator"
	"github.com/pietjan/loom/sidebar"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

// Site is the whole examples site as one component, and therefore one
// session and one stream.
//
// A handler per example with href links between them does not survive being
// browsed: every link is a full page load, every page holds one of the
// browser's ~6 connections per origin for its stream, and once enough are
// open the next page simply never loads.
//
// Here a link is an action. It calls Navigate, which pushes the path into
// history from the server and re-renders; the example being shown is a
// keyed child, so switching swaps one child for another and the connection
// is never touched.
//
// The chrome around the examples is Loom's - sidebar, navlist, heading,
// text, callout, accordion, separator, code - because a site demonstrating
// the stateful layer over Loom that hand-wrote its own navigation would be
// arguing against itself. What is left in raw markup is the page's grid and
// the boxes that position things, which is the split Loom asks for: it
// styles components, not your document.
type Site struct {
	shuttle.Base
	All []Example

	slug string
}

// HandleParams reads the example out of the path, so a deep link, a reload
// and the back button all arrive at the same place. It runs on the first
// render too, which is what makes /e/table/ work as a bookmark.
func (s *Site) HandleParams(context.Context, shuttle.Params) error {
	s.slug = strings.Trim(strings.TrimPrefix(s.Path(), "/e"), "/")
	if s.current() == nil && len(s.All) > 0 {
		s.slug = s.All[0].Slug
	}
	return nil
}

func (s *Site) current() *Example {
	for i := range s.All {
		if s.All[i].Slug == s.slug {
			return &s.All[i]
		}
	}
	return nil
}

func (s *Site) Render(ctx context.Context) templ.Component {
	ex := s.current()
	if ex == nil {
		return el(`<main class="max-w-[60rem] px-10 py-8">`, `</main>`, inside(text.New(), label("No such example.")))
	}

	// One action per link. Navigate pushes the path and tells the component
	// about it, so the address bar, the history and the markup all move
	// together - without a page load, and without a second stream.
	//
	// navlist.Item is a real <a href>, and the path it names really does
	// serve this example on a fresh load, so the link stays copyable and
	// bookmarkable. __prevent is what stops an ordinary click from doing
	// that the slow way as well.
	items := make([]templ.Component, 0, len(s.All))
	for _, entry := range s.All {
		slug := entry.Slug // captured per link
		options := []navlist.Option{
			shuttle.OnEvent(ctx, navlist.Attr, "click__prevent", func(actx context.Context) error {
				return s.Navigate(actx, "/e/"+slug+"/")
			}),
		}
		// aria-current="page", which is also what navlist styles the
		// current item off.
		if slug == s.slug {
			options = append(options, navlist.Current())
		}
		items = append(items, inside(navlist.Item("/e/"+slug+"/", options...),
			icon.New(entry.Icon), label(entry.Title)))
	}

	// The sidebar is a popover: an overlay with light dismiss and Esc below
	// 64rem, statically visible above it, and its Toggle hides itself once
	// it is. All of that is the platform and Loom's structural CSS - the
	// morph swaps the markup underneath and none of it is state this
	// component has to keep.
	nav := inside(sidebar.New(),
		inside(heading.New(heading.Large), label("shuttle")),
		inside(navlist.New(navlist.Label("Examples")), items...),
	)

	// The example itself is a keyed child: its own state, its own action
	// table, its own morph target. Switching examples unmounts the old one
	// because its key stops being rendered.
	child := shuttle.Child(ctx, ex.Slug, ex.New)

	return el(`<div class="grid min-h-dvh grid-cols-[auto_minmax(0,1fr)]">`, `</div>`,
		nav,
		el(`<main class="max-w-[60rem] px-10 py-8">`, `</main>`,
			el(`<header class="flex flex-col items-start gap-2">`, `</header>`,
				sidebar.Toggle(),
				inside(heading.New(heading.Level(1), heading.XL), label(ex.Title)),
				inside(text.New(text.Class("max-w-[46ch]")), label(ex.Blurb)),
				hint(*ex),
			),
			el(demoClasses(*ex), `</section>`, child),
			separator.New(separator.Class("mt-10 mb-4")),
			// A disclosure, which is what accordion is - on native <details>,
			// so opening it costs no script and survives a morph.
			inside(accordion.Root(),
				inside(accordion.Item(accordion.Title(ex.File)), sourceBlock(ex.File)),
			),
		),
	)
}

// demoClasses opens the section the example renders into. It hugs its
// content, so a button is button-sized rather than page-wide - except for an
// example that says it wants the room.
func demoClasses(ex Example) string {
	if ex.Wide {
		return `<section class="my-8 flex flex-col items-stretch gap-4">`
	}
	return `<section class="my-8 flex flex-col items-start gap-4">`
}

// hint is the instruction an example needs to make sense - open a second
// tab, press back - which is exactly what a callout is for.
func hint(ex Example) templ.Component {
	if ex.Hint == "" {
		return nothing
	}
	return inside(callout.New(callout.Info, callout.Class("max-w-[46ch]")),
		icon.New(icon.Info),
		inside(callout.Text(), label(ex.Hint)),
	)
}
