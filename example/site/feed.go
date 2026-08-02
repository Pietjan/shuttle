package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/badge"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

// event is one line of an activity log - the kind of set that is too long to
// paginate and too long to hold.
type event struct {
	At   time.Time
	Kind string
	What string
}

// journal is the set behind the feed. Six hundred entries is not a lot for
// a real log and is plenty to make the point: the reader can scroll through
// all of them and the server will still be holding eight.
var journal = buildJournal(600)

func buildJournal(n int) []event {
	kinds := []string{"deploy", "merge", "alert", "rollback"}
	what := []string{
		"api gateway", "billing worker", "search index", "image pipeline",
		"session store", "edge cache", "report builder", "webhook relay",
	}

	start := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	out := make([]event, n)
	for i := range out {
		out[i] = event{
			// Newest first, which is the order a log is read in.
			At:   start.Add(-time.Duration(i) * 7 * time.Minute),
			Kind: kinds[i%len(kinds)],
			What: fmt.Sprintf("%s #%d", what[i%len(what)], n-i),
		}
	}
	return out
}

// Activity hosts a live.Feed and counts what it costs.
//
// The counter is the whole reason this example has a wrapper. Scrolling
// looks the same whichever way a feed is built; what is worth seeing is the
// number beside it, which says the browser is holding six hundred rows and
// this process is holding eight.
type Activity struct {
	shuttle.Base

	// feed is the child, kept so this component can read what it has done.
	// A parent holding a reference to a live child is the ordinary way to
	// talk to one - it is a Go struct in the same process.
	feed *live.Feed[event]
}

// load is the data source, and also where this component finds out that the
// feed has been asked for another page.
//
// There is no callback to register: Load is the application's own function,
// it runs on the session's goroutine like every other piece of component
// work, and Push marks this component for re-render so the count beside the
// feed keeps up. Pushing rather than rendering is what makes it safe to
// call from in here.
func (a *Activity) load(ctx context.Context, q live.Query) (live.Page[event], error) {
	if q.Offset >= len(journal) {
		return live.Page[event]{Total: len(journal)}, nil
	}
	end := min(q.Offset+q.Limit, len(journal))
	page := live.Page[event]{Rows: journal[q.Offset:end], Total: len(journal)}

	// Not on the first load: Mount is already going to render.
	if q.Offset > 0 {
		if err := a.Push(ctx); err != nil {
			return page, err
		}
	}
	return page, nil
}

func (a *Activity) Render(ctx context.Context) templ.Component {
	feed := shuttle.Child(ctx, "feed", func() shuttle.Component {
		a.feed = &live.Feed[event]{
			Load:     a.load,
			Item:     a.entry,
			PageSize: 8,
			End:      "That is the whole log.",
		}
		return a.feed
	})

	// The count is rendered after the feed and shown above it, which is not
	// a layout whim. A child mounts as it is rendered, so a count written
	// before it would be reading a component that has not loaded anything
	// yet - on the first paint it would say nothing at all. Rendering it
	// afterwards and moving it with order-first keeps both halves honest.
	return el(`<div class="flex flex-col gap-3">`, `</div>`,
		feed,
		el(`<div class="order-first">`, `</div>`,
			inside(badge.New(badge.Zinc), a.cost())),
	)
}

// cost is the line this example exists for. It is a component rather than a
// string so that it is read when it is written, which is after the feed
// above it has rendered.
func (a *Activity) cost() templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		if a.feed == nil {
			return nil
		}
		_, err := fmt.Fprintf(w, "%d rows in the browser, %d held on the server",
			a.feed.Loaded(), a.feed.Held())
		return err
	})
}

// entry renders one row. The element around it belongs to the feed, which
// is what carries the id a streamed item is addressed by - so this is only
// ever the content.
func (a *Activity) entry(e event) templ.Component {
	return el(`<div class="flex items-baseline gap-3">`, `</div>`,
		inside(text.New(text.Subtle, text.Small), label(e.At.Format("15:04"))),
		inside(badge.New(kindTone(e.Kind), badge.Small), label(e.Kind)),
		inside(text.New(text.Small), label(e.What)),
	)
}

func kindTone(kind string) badge.Option {
	switch kind {
	case "deploy":
		return badge.Emerald
	case "alert":
		return badge.Red
	case "rollback":
		return badge.Amber
	default:
		return badge.Blue
	}
}
