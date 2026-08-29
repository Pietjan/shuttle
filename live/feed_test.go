package live_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

// posts is the set behind the feed: bigger than any page of it, which is
// the only interesting case.
func posts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("post-%d", i)
	}
	return out
}

// loadPosts is an ordinary offset/limit source. total < 0 makes it one that
// does not know how many rows it has, which is a different code path.
func loadPosts(all []string, total int) func(context.Context, live.Query) (live.Page[string], error) {
	return func(_ context.Context, q live.Query) (live.Page[string], error) {
		if q.Offset >= len(all) {
			return live.Page[string]{Total: total}, nil
		}
		end := min(q.Offset+q.Limit, len(all))
		return live.Page[string]{Rows: all[q.Offset:end], Total: total}, nil
	}
}

func newFeed(all []string, total int) *live.Feed[string] {
	return &live.Feed[string]{
		Load:     loadPosts(all, total),
		Item:     func(s string) templ.Component { return live.Text(s) },
		PageSize: 3,
		End:      "That is everything.",
	}
}

// TestFeedRendersItsFirstPageIntoTheDocument. The first page is markup, not
// a patch, so the document a browser is first served is already complete -
// which is the promise the rest of this framework makes and the one a feed
// is most likely to be the whole of.
func TestFeedRendersItsFirstPageIntoTheDocument(t *testing.T) {
	f := newFeed(posts(10), 10)
	l := shuttle.Test(t, f)

	html := l.HTML()
	for _, want := range []string{"post-0", "post-1", "post-2"} {
		if !strings.Contains(html, want) {
			t.Errorf("%q missing from the first render", want)
		}
	}
	if strings.Contains(html, "post-3") {
		t.Error("the first render carries more than one page")
	}
	if f.Loaded() != 3 {
		t.Errorf("Loaded() = %d, want one page of 3", f.Loaded())
	}
	l.Assert().NoDuplicateIDs()
}

// TestFeedStreamsEveryPageAfterTheFirst, and holds none of them. This is
// what the whole design is for: the reader's scrollback lives in the
// browser, so how far somebody scrolls does not decide what the tab costs.
func TestFeedStreamsEveryPageAfterTheFirst(t *testing.T) {
	f := newFeed(posts(10), 10)
	l := shuttle.Test(t, f)

	l.Intersect("[data-shuttle-sentinel]")

	patches := strings.Join(l.Patches(), "\n")
	for _, want := range []string{"post-3", "post-4", "post-5"} {
		if !strings.Contains(patches, want) {
			t.Errorf("%q was not streamed; patches were:\n%s", want, patches)
		}
	}
	if f.Loaded() != 6 {
		t.Errorf("Loaded() = %d, want 6", f.Loaded())
	}

	// The browser has six rows and the server is holding three of them -
	// the first page, and nothing the reader has scrolled past.
	if f.Held() != 3 {
		t.Errorf("Held() = %d, want one page of 3 however far the reader gets", f.Held())
	}
}

// TestFeedKeepsAskingUntilTheSourceRunsOut, one page per firing.
func TestFeedKeepsAskingUntilTheSourceRunsOut(t *testing.T) {
	f := newFeed(posts(10), 10)
	l := shuttle.Test(t, f)

	for range 3 {
		l.Intersect("[data-shuttle-sentinel]")
	}
	if f.Loaded() != 10 {
		t.Fatalf("Loaded() = %d, want all 10", f.Loaded())
	}
	if !f.Done() {
		t.Error("the feed does not know it reached the end")
	}
}

// TestTheSentinelGoesAwayAtTheEnd. Nothing else stops the loading: an
// element on screen that keeps being re-rendered keeps being re-armed, so
// exhaustion has to be expressed by not rendering it.
func TestTheSentinelGoesAwayAtTheEnd(t *testing.T) {
	l := shuttle.Test(t, newFeed(posts(4), 4))

	l.Intersect("[data-shuttle-sentinel]")

	if html := l.HTML(); strings.Contains(html, "data-shuttle-sentinel") {
		t.Error("the sentinel outlived the last page, so the feed will keep asking")
	}
	l.Assert().TextContains("[data-shuttle-feed-end]", "That is everything.")
}

// TestAFeedThatDoesNotKnowItsTotal. A source answering Total < 0 is saying
// it does not know, and then a short page is the only end-of-set signal
// there is.
func TestAFeedThatDoesNotKnowItsTotal(t *testing.T) {
	f := newFeed(posts(5), -1)
	l := shuttle.Test(t, f)

	l.Intersect("[data-shuttle-sentinel]") // rows 3 and 4: a short page
	if !f.Done() {
		t.Fatalf("Loaded() = %d and not done; a short page is the end", f.Loaded())
	}
	if f.Loaded() != 5 {
		t.Errorf("Loaded() = %d, want 5", f.Loaded())
	}
}

// TestAFullLastPageCostsOneEmptyRequest, when the source does not know its
// total. Worth pinning rather than discovering: it is the price of not
// knowing, and the alternative is asking every source for a count.
func TestAFullLastPageCostsOneEmptyRequest(t *testing.T) {
	f := newFeed(posts(6), -1) // exactly two pages of 3
	l := shuttle.Test(t, f)

	l.Intersect("[data-shuttle-sentinel]")
	if f.Done() {
		t.Fatal("a full page cannot be known to be the last one")
	}

	l.Intersect("[data-shuttle-sentinel]")
	if !f.Done() {
		t.Error("the empty page did not end the feed")
	}
	if f.Loaded() != 6 {
		t.Errorf("Loaded() = %d, want 6", f.Loaded())
	}
}

// TestAFailingSourceStopsRatherThanRetryingItself is the one that would
// otherwise be found in production.
//
// The sentinel is on screen when the load fails, and every render re-arms
// it - so a feed that kept rendering it would retry as fast as the browser
// could ask, against a source that is already failing, until the session's
// request budget cut it off. Failing means the sentinel goes away and a
// button takes its place.
func TestAFailingSourceStopsRatherThanRetryingItself(t *testing.T) {
	boom := errors.New("the database is having a moment")
	var calls int

	f := &live.Feed[string]{
		Item:     func(s string) templ.Component { return live.Text(s) },
		PageSize: 3,
		Load: func(_ context.Context, q live.Query) (live.Page[string], error) {
			calls++
			if q.Offset > 0 {
				return live.Page[string]{}, boom
			}
			return live.Page[string]{Rows: posts(3), Total: 100}, nil
		},
	}
	l := shuttle.Test(t, f)

	l.Intersect("[data-shuttle-sentinel]")

	if !errors.Is(f.Err(), boom) {
		t.Fatalf("Err() = %v, want the load failure", f.Err())
	}
	if html := l.HTML(); strings.Contains(html, "data-shuttle-sentinel") {
		t.Error("the sentinel survived a failure, so the failing source gets hammered")
	}
	l.Assert().TextContains("[data-shuttle-feed-error]", "having a moment")

	// And the retry is a decision somebody makes, not one the page makes
	// for them.
	before := calls
	l.Click("[data-shuttle-feed-error] button")
	if calls != before+1 {
		t.Errorf("the source was called %d times, want exactly one retry", calls-before)
	}
}

// TestResetStreamsTheFirstPageAgain, because it cannot re-render it: the
// container is ignored by morphs, so markup written into it after the first
// paint never reaches the browser. That is the trap this method exists to
// keep an app out of.
func TestResetStreamsTheFirstPageAgain(t *testing.T) {
	all := posts(10)
	f := newFeed(all, 10)
	l := shuttle.Test(t, f)

	l.Intersect("[data-shuttle-sentinel]")
	if f.Loaded() != 6 {
		t.Fatalf("Loaded() = %d, want 6 before the reset", f.Loaded())
	}
	l.Patches()

	// The source changes under the feed, the way a filter would change it.
	copy(all, []string{"fresh-0", "fresh-1", "fresh-2"})
	if err := f.Do(func(ctx context.Context) error { return f.Reset(ctx) }); err != nil {
		t.Fatalf("reset: %v", err)
	}

	patches := strings.Join(l.Patches(), "\n")
	for _, want := range []string{"fresh-0", "fresh-1", "fresh-2"} {
		if !strings.Contains(patches, want) {
			t.Errorf("%q was not streamed after the reset; patches were:\n%s", want, patches)
		}
	}
	if f.Loaded() != 3 {
		t.Errorf("Loaded() = %d, want the first page again", f.Loaded())
	}
	if f.Done() {
		t.Error("the reset feed thinks it is already finished")
	}
}

// TestFeedItemsCarryTheirStreamID. Shuttle refuses an item without one, so
// this is really a test that the feed builds the element rather than
// leaving it to the caller's Item - which would make the rule everybody
// else's problem.
func TestFeedItemsCarryTheirStreamID(t *testing.T) {
	l := shuttle.Test(t, newFeed(posts(10), 10))

	l.Intersect("[data-shuttle-sentinel]")
	patches := strings.Join(l.Patches(), "\n")

	if !strings.Contains(patches, `id="shuttle-c-feed-3"`) {
		t.Errorf("a streamed row carries no addressable id:\n%s", patches)
	}
}

// TestTwoFeedsOnOnePageDoNotShareAContainer: every id here comes from the
// component's position in the tree, so a second instance is a second
// container and not a collision nobody would be told about.
func TestTwoFeedsOnOnePageDoNotShareAContainer(t *testing.T) {
	l := shuttle.Test(t, &pair{})
	l.Assert().NoDuplicateIDs()

	html := l.HTML()
	for _, want := range []string{`id="shuttle-c-1-feed"`, `id="shuttle-c-2-feed"`} {
		if !strings.Contains(html, want) {
			t.Errorf("no container %s in:\n%s", want, html)
		}
	}
}

// pair mounts two feeds as children.
type pair struct{ shuttle.Base }

func (p *pair) Render(ctx context.Context) templ.Component {
	a := shuttle.Child(ctx, "a", func() shuttle.Component { return newFeed(posts(10), 10) })
	b := shuttle.Child(ctx, "b", func() shuttle.Component { return newFeed(posts(10), 10) })

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := a.Render(ctx, w); err != nil {
			return err
		}
		return b.Render(ctx, w)
	})
}

// TestAnOverstatedTotalStopsAtTheEmptyPage. Totals overstate for mundane
// reasons - rows deleted between pages, a cached count - and a feed that
// kept trusting one past an empty answer would re-arm its sentinel against
// a source with nothing left: a request loop bounded only by the session's
// budget.
func TestAnOverstatedTotalStopsAtTheEmptyPage(t *testing.T) {
	f := newFeed(posts(5), 100)
	l := shuttle.Test(t, f)

	l.Intersect("[data-shuttle-sentinel]") // rows 3-4: a short page, but Total says go on
	l.Intersect("[data-shuttle-sentinel]") // the empty page, which is final

	if !f.Done() {
		t.Error("an empty page did not end the feed")
	}
	l.Assert().Missing("[data-shuttle-sentinel]")
	if f.Loaded() != 5 {
		t.Errorf("Loaded() = %d, want the 5 that exist", f.Loaded())
	}
}

// TestRetryAfterAFirstPageFailureRecoversTheFirstPage. The recovered page
// has to land in the component as well as the stream: first is what a
// fresh full render shows, and a reconnect outside the grace window is
// exactly such a render - an empty first over a healthy source would make
// that page come back blank.
func TestRetryAfterAFirstPageFailureRecoversTheFirstPage(t *testing.T) {
	calls := 0
	f := &live.Feed[string]{
		Load: func(ctx context.Context, q live.Query) (live.Page[string], error) {
			if calls++; calls == 1 {
				return live.Page[string]{}, errors.New("database asleep")
			}
			return loadPosts(posts(5), 5)(ctx, q)
		},
		Item:     func(s string) templ.Component { return live.Text(s) },
		PageSize: 3,
	}
	l := shuttle.Test(t, f)

	if f.Err() == nil {
		t.Fatal("the first load did not fail")
	}
	l.Click("[data-shuttle-feed-error] button")

	if f.Err() != nil {
		t.Fatalf("retry did not clear the failure: %v", f.Err())
	}
	if f.Held() != 3 {
		t.Errorf("Held() = %d, want the recovered first page of 3", f.Held())
	}
	if f.Loaded() != 3 {
		t.Errorf("Loaded() = %d, want 3", f.Loaded())
	}
}

// TestPrependShowsARowAtTheTop - the pub/sub pairing. The row goes down the
// stream as a prepend with a key that cannot collide with the appended
// ones, and the feed's offset cursor shifts so the next page from an
// offset source does not re-serve a row the reader already has.
func TestPrependShowsARowAtTheTop(t *testing.T) {
	all := posts(6)
	f := newFeed(all, 6)
	l := shuttle.Test(t, f)

	// Something new lands at the source's head, and the feed is told.
	all2 := append([]string{"post-new"}, all...)
	if err := f.Do(func(ctx context.Context) error {
		f.Load = loadPosts(all2, 7)
		return f.Prepend(ctx, "post-new")
	}); err != nil {
		t.Fatalf("prepend: %v", err)
	}
	l.Settle()

	patches := strings.Join(l.Patches(), "\n")
	if !strings.Contains(patches, "post-new") {
		t.Fatalf("the prepended row was not streamed: %s", patches)
	}
	if f.Loaded() != 4 {
		t.Errorf("Loaded() = %d, want 3 fetched + 1 prepended", f.Loaded())
	}

	// The next page starts where it would have without the prepend: rows
	// 3-5 of the old sequence, not a duplicate of anything on screen.
	l.Intersect("[data-shuttle-sentinel]")
	patches = strings.Join(l.Patches(), "\n")
	for _, want := range []string{"post-3", "post-4", "post-5"} {
		if !strings.Contains(patches, want) {
			t.Errorf("%q missing from the next page", want)
		}
	}
	if got := strings.Count(patches, ">post-2<"); got > 1 {
		t.Errorf("post-2 was served twice - the prepend did not shift the offset")
	}
	if !f.Done() {
		t.Error("the feed does not know it reached the end")
	}
}

// TestPrependFillsAnEmptyFeed: the Empty note has to give way to the row.
func TestPrependFillsAnEmptyFeed(t *testing.T) {
	f := &live.Feed[string]{
		Load:     loadPosts(nil, 0),
		Item:     func(s string) templ.Component { return live.Text(s) },
		PageSize: 3,
		Empty:    "Nothing yet.",
	}
	l := shuttle.Test(t, f)
	l.Assert().TextContains("[data-shuttle-feed-end]", "Nothing yet.")

	if err := f.Do(func(ctx context.Context) error {
		return f.Prepend(ctx, "post-new")
	}); err != nil {
		t.Fatalf("prepend: %v", err)
	}
	l.Settle()

	if !strings.Contains(strings.Join(l.Patches(), "\n"), "post-new") {
		t.Fatal("the prepended row was not streamed")
	}
	l.Assert().Missing("[data-shuttle-feed-end]")
	if f.Loaded() != 1 {
		t.Errorf("Loaded() = %d, want 1", f.Loaded())
	}
}

// loadByCursor paginates by key and ignores Offset entirely, the way a
// database seeking on an indexed column does. Next is the index of the row
// after this page, "" on the last.
func loadByCursor(all []string) func(context.Context, live.Query) (live.Page[string], error) {
	return func(_ context.Context, q live.Query) (live.Page[string], error) {
		at := 0
		if q.Cursor != "" {
			at, _ = strconv.Atoi(q.Cursor)
		}
		if at >= len(all) {
			return live.Page[string]{Total: -1}, nil
		}
		end := min(at+q.Limit, len(all))
		p := live.Page[string]{Rows: all[at:end], Total: -1}
		if end < len(all) {
			p.Next = strconv.Itoa(end)
		}
		return p, nil
	}
}

// TestFeedWalksAKeysetSource. Setting Page.Next is the whole opt-in: the
// feed carries it back as Query.Cursor, is not exhausted while it is set,
// and stops the moment the source leaves it empty.
func TestFeedWalksAKeysetSource(t *testing.T) {
	f := &live.Feed[string]{
		Load:     loadByCursor(posts(7)),
		Item:     func(s string) templ.Component { return live.Text(s) },
		PageSize: 3,
	}
	l := shuttle.Test(t, f)

	for range 2 {
		l.Intersect("[data-shuttle-sentinel]")
	}
	if f.Loaded() != 7 {
		t.Fatalf("Loaded() = %d, want all 7", f.Loaded())
	}
	if !f.Done() {
		t.Error("the feed does not know the cursor ran out")
	}
	l.Assert().Missing("[data-shuttle-sentinel]")

	patches := strings.Join(l.Patches(), "\n")
	for _, want := range []string{"post-3", "post-6"} {
		if !strings.Contains(patches, want) {
			t.Errorf("%q never arrived", want)
		}
	}
}

// TestPrependLeavesAKeysetCursorAlone: a cursor points into the sequence
// below, so a row arriving at the head must not disturb it - no
// compensation, no skipped rows.
func TestPrependLeavesAKeysetCursorAlone(t *testing.T) {
	f := &live.Feed[string]{
		Load:     loadByCursor(posts(5)),
		Item:     func(s string) templ.Component { return live.Text(s) },
		PageSize: 3,
	}
	l := shuttle.Test(t, f)

	if err := f.Do(func(ctx context.Context) error {
		return f.Prepend(ctx, "post-new")
	}); err != nil {
		t.Fatalf("prepend: %v", err)
	}
	l.Settle()
	l.Intersect("[data-shuttle-sentinel]")

	patches := strings.Join(l.Patches(), "\n")
	for _, want := range []string{"post-3", "post-4"} {
		if !strings.Contains(patches, want) {
			t.Errorf("%q was skipped after a prepend", want)
		}
	}
	if !f.Done() {
		t.Error("the feed did not finish")
	}
}
