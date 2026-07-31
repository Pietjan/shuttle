package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/badge"
	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/icon"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/loom/timeline"
	"github.com/pietjan/shuttle"
)

// Log streams its entries instead of holding them. The component keeps a
// counter, not a collection, so a page open all day costs what a page just
// opened does.
//
// The container is Loom's timeline - a log is a sequence of events, which is
// what it is for - bound with shuttle.Container. That binding is the only
// thing that makes a component usable here: the container has to carry the
// patch target's id and data-ignore-morph, and a component takes options,
// not a string of attributes.
type Log struct {
	shuttle.Base
	Sent int
}

// Mount starts a timer, torn down with the session.
func (l *Log) Mount(context.Context, shuttle.Params) error {
	return l.Every(2*time.Second, func(ctx context.Context) error {
		return l.append(ctx, "tick at "+time.Now().Format("15:04:05"))
	})
}

func (l *Log) append(ctx context.Context, line string) error {
	l.Sent++
	key := strconv.Itoa(l.Sent)
	return l.Stream("log").Append(ctx, key, l.entry(key, line))
}

// entry must render the item's id, or the entry could never be replaced or
// removed afterwards. Shuttle checks, rather than letting it fail silently -
// and timeline.ID is how a Loom component takes one.
func (l *Log) entry(key, line string) templ.Component {
	return inside(timeline.Item(timeline.ID(l.Stream("log").ItemID(key))),
		timeline.Indicator(),
		inside(timeline.Content(), inside(text.New(), label(line))),
	)
}

func (l *Log) Render(ctx context.Context) templ.Component {
	add := button.New(button.Primary,
		shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
			return l.append(actx, "added by hand")
		}))
	clear := button.New(button.Outline,
		shuttle.OnClick(ctx, button.Attr, func(actx context.Context) error {
			return l.Stream("log").Clear(actx)
		}))

	return el(`<div class="grid gap-2">`, `</div>`,
		el(`<div class="grid grid-flow-col items-center justify-start gap-2">`, `</div>`,
			inside(add, icon.New(icon.Plus), label("Append")),
			inside(clear, icon.New(icon.Trash), label("Clear")),
			// The count is state; the entries it counts are not held at all.
			inside(badge.New(badge.Zinc), label(fmt.Sprintf("%d sent, none held", l.Sent))),
		),
		// data-ignore-morph is on the timeline, so this component's own
		// re-renders leave the streamed items where they are.
		inside(timeline.New(shuttle.Container(l.Stream("log"), timeline.Attr))),
	)
}
