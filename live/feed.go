package live

import (
	"context"
	"fmt"
	"strconv"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/skeleton"
	loomtext "github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

// feedStream is the name of the container items are streamed into. It is
// scoped to the component instance like every other id, so two feeds on one
// page do not collide.
const feedStream = "feed"

// Feed is an infinite-scrolling list over a set the server owns.
//
//	&live.Feed[Post]{
//	    Load: loadPosts,
//	    Item: func(p Post) templ.Component { return live.Text(p.Title) },
//	}
//
// It reads its pages through the same [Query] and [Page] a [Table] does, so
// one data source can back either - a table for the desk and a feed for the
// phone, over the same function.
//
// # What it costs, which is the whole design
//
// A feed is where server-held state gets expensive: scroll far enough and a
// component that re-renders its list has to hold the list, so a reader who
// keeps going costs the server everything they have seen, for as long as
// the tab is open. Feed holds **one page**, exactly as Table does. Pages
// after the first are streamed into the browser an item at a time and
// forgotten here, and the container carries data-ignore-morph so the
// component's own re-renders leave them alone.
//
// The trade is the one [shuttle.Stream] always makes, and it is worth
// saying plainly: the server does not know what the reader is looking at.
// Nothing can rebuild that list, so a page that reloads - or a session lost
// to a restart, which reloads - comes back to the first page and the reader
// scrolls again.
//
// # Why the first page is different
//
// The first page is rendered into the container rather than streamed, so
// the document a browser is first served is complete. That is shuttle's
// central promise, and a feed whose content only arrives after the stream
// opens would break it for the one component most likely to be the whole
// page.
//
// It also means [Feed.Reset] cannot simply re-render: markup inside an
// ignored container never reaches the browser. Reset clears the container
// and streams the new first page instead, which is why it is a method
// rather than something an app does by assigning to a field.
type Feed[T any] struct {
	shuttle.Base

	// Load fetches one page. It runs on the session's goroutine, and only
	// Offset and Limit are set - a feed has no sort and no filter of its
	// own. Filter by closing over your own state and calling Reset.
	Load func(ctx context.Context, q Query) (Page[T], error)

	// Item renders one row's content. The element around it, which carries
	// the id that makes the row addressable, is the feed's own.
	Item func(row T) templ.Component

	// PageSize is how many rows a page holds. Zero means DefaultPageSize.
	PageSize int

	// Empty is shown when the very first page comes back with nothing.
	Empty string

	// End is shown once the source is exhausted. Empty means show nothing,
	// which is the right choice for a feed long enough that nobody reaches
	// the bottom.
	End string

	// first is the page rendered into the container - the only rows this
	// component holds.
	first Page[T]
	// sent is how many rows the client has been given, and so the offset
	// the next page starts at.
	sent   int
	done   bool
	failed error
}

// Mount loads the first page.
func (f *Feed[T]) Mount(ctx context.Context, _ shuttle.Params) error {
	return f.reload(ctx)
}

// Loaded reports how many rows the reader has been given so far. The rows
// themselves are in the browser; this is the count the server still knows.
func (f *Feed[T]) Loaded() int { return f.sent }

// Done reports whether the source is exhausted.
func (f *Feed[T]) Done() bool { return f.done }

// Err returns the last load failure, if any.
func (f *Feed[T]) Err() error { return f.failed }

// Reset starts the feed over: the container is emptied and the first page
// loaded again. Call it after changing whatever Load closes over - a
// filter, a sort, a topic.
//
// It streams the new first page rather than leaving it to the re-render,
// because the container is ignored by morphs and markup written into it
// after the first paint never arrives.
func (f *Feed[T]) Reset(ctx context.Context) error {
	s := f.Stream(feedStream)
	if s == nil {
		return shuttle.ErrNotMounted
	}
	if err := s.Clear(ctx); err != nil {
		return err
	}
	if err := f.reload(ctx); err != nil {
		return err
	}
	_, err := f.stream(ctx, f.first.Rows, 0)
	return err
}

// reload fetches the first page into the component's own state.
func (f *Feed[T]) reload(ctx context.Context) error {
	f.failed, f.sent, f.done = nil, 0, false

	page, err := f.fetch(ctx, 0)
	if err != nil {
		f.first, f.failed = Page[T]{}, err
		return nil
	}
	f.first = page
	f.sent = len(page.Rows)
	f.done = f.exhausted(page)
	return nil
}

// more is what the sentinel calls: the next page, streamed.
//
// It advances a cursor rather than reading one out of the request, which
// matters because the sentinel can fire several times before the reader
// does anything - see [shuttle.OnIntersect]. Two calls in a row are two
// pages, not the same page twice.
func (f *Feed[T]) more(ctx context.Context) error {
	if f.done || f.failed != nil {
		return nil
	}

	page, err := f.fetch(ctx, f.sent)
	if err != nil {
		// Reported rather than returned: a source that fails is the feed's
		// to show, not the page's to die of. It also stops the sentinel
		// being rendered, which is what keeps a failing Load from being
		// retried as fast as the browser can ask.
		f.failed = err
		return nil
	}

	if f.sent == 0 {
		// This is the first page arriving late - a retry after Mount's own
		// load failed. It has to land in first as well as the stream, or a
		// fresh full render (a reconnect outside the grace window) would
		// show an empty feed over a healthy source, and Held would report a
		// page this component is in fact holding as zero.
		f.first = page
	}

	appended, err := f.stream(ctx, page.Rows, f.sent)
	// Advanced by what actually reached the container, not by the page: a
	// stream that failed partway must not count rows the client never got,
	// or the retry would re-append the ones it did under the same ids.
	f.sent += appended
	if err != nil {
		return err
	}
	f.done = f.exhausted(page)
	return nil
}

// Held reports how many rows this component is keeping in memory, which is
// one page however far the reader has scrolled. Loaded is how many they
// have; the difference between the two is the whole point of the design,
// and worth putting on screen while you are still deciding to believe it.
func (f *Feed[T]) Held() int { return len(f.first.Rows) }

// retry clears a failure and tries the page that failed again.
func (f *Feed[T]) retry(ctx context.Context) error {
	if f.failed == nil {
		return nil
	}
	f.failed = nil
	return f.more(ctx)
}

// stream appends rows to the container, numbered from at. It returns how
// many were appended before any failure, so the caller's cursor can follow
// what the client actually has.
func (f *Feed[T]) stream(ctx context.Context, rows []T, at int) (int, error) {
	s := f.Stream(feedStream)
	if s == nil {
		return 0, shuttle.ErrNotMounted
	}
	for i, row := range rows {
		if err := s.Append(ctx, key(at+i), f.row(at+i, row)); err != nil {
			return i, err
		}
	}
	return len(rows), nil
}

// fetch asks the source for the page starting at offset.
func (f *Feed[T]) fetch(ctx context.Context, offset int) (Page[T], error) {
	if f.Load == nil {
		return Page[T]{}, nil
	}
	return f.Load(ctx, Query{Offset: offset, Limit: f.limit()})
}

// exhausted reports whether page was the last one. Call it after sent has
// been advanced, since a source that knows its total is answered by
// comparing the two.
//
// A source that answers Total < 0 is telling us it does not know, and then
// a short page is the only signal there is - which means one whose last
// page happens to be exactly full costs an extra, empty, request. That is
// the price of not knowing, and it is cheaper than making every source
// count.
func (f *Feed[T]) exhausted(page Page[T]) bool {
	// An empty page is final whatever Total claims. Totals overstate for
	// mundane reasons - rows deleted between pages, a cached count, a query
	// filtered after counting - and trusting one past an empty answer is a
	// sentinel re-armed against a source with nothing left: a request loop
	// bounded only by the session's budget.
	if len(page.Rows) == 0 {
		return true
	}
	if page.Total >= 0 {
		return f.sent >= page.Total
	}
	return len(page.Rows) < f.limit()
}

func (f *Feed[T]) limit() int {
	if f.PageSize > 0 {
		return f.PageSize
	}
	return DefaultPageSize
}

// key numbers a streamed row by its position in the whole sequence, which
// is unique and stable for a list that only ever grows. Reset empties the
// container in the same breath as it restarts the numbering, so the two
// cannot overlap.
func key(i int) string { return strconv.Itoa(i) }

// row wraps one item in the element that carries its streamed id. Shuttle
// refuses an item without one, because an item that cannot be addressed can
// never be replaced or removed.
func (f *Feed[T]) row(i int, r T) templ.Component {
	var content templ.Component = empty
	if f.Item != nil {
		content = f.Item(r)
	}
	return seq(
		templ.Raw(fmt.Sprintf(`<li id=%q data-shuttle-feed-item>`,
			f.Stream(feedStream).ItemID(key(i)))),
		content,
		templ.Raw(`</li>`),
	)
}

func (f *Feed[T]) Render(ctx context.Context) templ.Component {
	s := f.Stream(feedStream)
	if s == nil {
		// Not mounted: rendering would dereference a stream that does not
		// exist. The guard the other stream users already have.
		return empty
	}

	items := make([]templ.Component, 0, len(f.first.Rows))
	for i, row := range f.first.Rows {
		items = append(items, f.row(i, row))
	}

	return seq(
		// Attrs carries the container's id and data-ignore-morph, which is
		// what stops this component's own re-renders from wiping the pages
		// it no longer holds.
		templ.Raw(fmt.Sprintf(`<ul%s data-shuttle-feed class="grid gap-2">`, s.Attrs())),
		seq(items...),
		templ.Raw(`</ul>`),
		f.footer(ctx),
	)
}

// footer is whatever comes after the list: the sentinel, an error, or the
// end.
//
// Exactly one of them, and the sentinel is absent in both of the other two
// cases. That is not tidiness - not rendering it is the only way to stop
// the loading, since an element on screen that keeps being re-rendered
// keeps asking for the next page.
func (f *Feed[T]) footer(ctx context.Context) templ.Component {
	switch {
	case f.failed != nil:
		return f.failure(ctx)
	case !f.done:
		return f.sentinel(ctx)
	case f.sent == 0 && f.Empty != "":
		return note(f.Empty)
	case f.End != "":
		return note(f.End)
	}
	return empty
}

// sentinel is the element that loads the next page when it scrolls into
// view.
//
// It is a skeleton row, which is the placeholder for the content it is
// about to fetch: the reader sees the shape of what is coming rather than
// the list simply stopping, and Loom already draws it - aria-hidden, so a
// screen reader is not told about a row that does not exist yet.
// The height is an inline style rather than only a class, and that is not
// a preference. This element's size is the difference between a feed that
// loads and one that does not, and a class resolves only in an app whose
// Tailwind build scans this package - the same bargain the pager's layout
// makes, but here the failure is silent rather than ugly. The class stays
// for an app that wants to restyle it.
func (f *Feed[T]) sentinel(ctx context.Context) templ.Component {
	return skeleton.New(
		skeleton.ID(shuttle.ElementID(ctx, "sentinel")),
		// The offset the next firing will ask for - and the reason the
		// sentinel keeps firing at all. Datastar re-applies a plugin when
		// its attribute value changes, and the observer's re-application is
		// what delivers the initial "still on screen" callback that walks a
		// short first page forward. Since generations are only minted when
		// markup changes, a feed whose own markup did not move would keep
		// its action id byte-for-byte and never re-arm; this attribute moves
		// with every page, so the render after each load always differs.
		skeleton.Attr("data-shuttle-sentinel", strconv.Itoa(f.sent)),
		skeleton.Attr("style", "min-height:2.5rem"),
		skeleton.Class("h-10"),
		shuttle.OnIntersect(ctx, skeleton.Attr, f.more),
	)
}

// failure shows what went wrong and offers the only sensible answer.
//
// A button rather than an automatic retry: the sentinel is on screen when a
// load fails, so retrying by itself would be a request loop bounded only by
// the session's budget, against a source that is already failing.
func (f *Feed[T]) failure(ctx context.Context) templ.Component {
	return seq(
		templ.Raw(`<div data-shuttle-feed-error class="mt-3 flex items-center gap-3">`),
		with(loomtext.New(loomtext.Subtle, loomtext.Small), text(f.failed.Error())),
		with(button.New(button.Outline, button.Small,
			shuttle.OnClick(ctx, button.Attr, f.retry)), text("Try again")),
		templ.Raw(`</div>`),
	)
}

// note is the muted line the feed ends on.
func note(s string) templ.Component {
	return seq(
		templ.Raw(`<div data-shuttle-feed-end class="mt-3">`),
		with(loomtext.New(loomtext.Subtle, loomtext.Small), text(s)),
		templ.Raw(`</div>`),
	)
}
