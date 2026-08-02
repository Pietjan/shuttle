//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/a-h/templ"
	"github.com/playwright-community/playwright-go"

	"github.com/pietjan/shuttle"
	"github.com/pietjan/shuttle/live"
)

// journal is the set behind the feed under test.
func journal(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("row-%d", i)
	}
	return out
}

func loadJournal(all []string) func(context.Context, live.Query) (live.Page[string], error) {
	return func(_ context.Context, q live.Query) (live.Page[string], error) {
		if q.Offset >= len(all) {
			return live.Page[string]{Total: len(all)}, nil
		}
		end := min(q.Offset+q.Limit, len(all))
		return live.Page[string]{Rows: all[q.Offset:end], Total: len(all)}, nil
	}
}

// reader hosts a feed and prints what it costs, so the test can assert on
// the split between what the browser holds and what the server does without
// reaching into the component from the test's goroutine.
type reader struct {
	shuttle.Base

	rows   int
	size   int
	height int // px per row, so a test can decide whether the page scrolls

	feed *live.Feed[string]
}

func (r *reader) load(ctx context.Context, q live.Query) (live.Page[string], error) {
	page, err := loadJournal(journal(r.rows))(ctx, q)
	if err != nil {
		return page, err
	}
	// The count beside the feed is this component's, so it has to ask to be
	// re-rendered when the child loads. Push rather than render: this runs
	// inside the child's action, on the session's goroutine.
	if q.Offset > 0 {
		if err := r.Push(ctx); err != nil {
			return page, err
		}
	}
	return page, nil
}

func (r *reader) item(row string) templ.Component {
	return templ.Raw(fmt.Sprintf(
		`<div class="row" style="height:%dpx">%s</div>`, r.height, row))
}

func (r *reader) Render(ctx context.Context) templ.Component {
	feed := shuttle.Child(ctx, "feed", func() shuttle.Component {
		r.feed = &live.Feed[string]{
			Load:     r.load,
			Item:     r.item,
			PageSize: r.size,
			End:      "the end",
		}
		return r.feed
	})

	// After the feed, and written rather than formatted up front: a child
	// mounts as it renders, so anything read from it before that point is a
	// render behind.
	counts := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p id="loaded">%d</p><p id="held">%d</p>`,
			r.feed.Loaded(), r.feed.Held())
		return err
	})

	return seq(feed, counts)
}

// TestScrollingLoadsMorePages is infinite scroll doing the one thing it is
// named after.
func TestScrollingLoadsMorePages(t *testing.T) {
	// Tall rows, so the first page more than fills the window and nothing
	// arrives until the reader actually scrolls.
	p := open(t, func() shuttle.Component {
		return &reader{rows: 60, size: 5, height: 200}
	})

	if err := expect(p.Locator("#loaded")).ToHaveText("5"); err != nil {
		t.Fatalf("first page: %v", err)
	}
	if err := expect(p.Locator("[data-shuttle-sentinel]")).ToBeAttached(); err != nil {
		t.Fatalf("no sentinel: %v", err)
	}

	scrollToBottom(t, p)

	if err := expect(p.Locator("#loaded")).ToHaveText("10"); err != nil {
		t.Fatalf("scrolling loaded nothing: %v", err)
	}

	// And the reader's scrollback is the browser's, not the server's.
	if err := expect(p.Locator("#held")).ToHaveText("5"); err != nil {
		t.Fatalf("the server is holding more than a page: %v", err)
	}
	count, err := p.Locator(".row").Count()
	if err != nil || count != 10 {
		t.Fatalf("%d rows in the document (err %v), want 10", count, err)
	}
}

// TestAShortFirstPageDoesNotStall is the mechanism this whole feature rests
// on, and the one that is invisible outside a browser.
//
// An IntersectionObserver reports a transition, so a sentinel that is
// already visible and stays visible fires once - and a first page too short
// to push it off screen would leave the feed stopped, in plain view, with
// more to give. What saves it is that every render writes a new action id,
// which makes Datastar re-apply the plugin, which builds a fresh observer,
// which reports the element's current state as soon as it observes.
//
// So: a window taller than the whole list, no scrolling at all, and the
// feed should still walk itself to the end.
func TestAShortFirstPageDoesNotStall(t *testing.T) {
	p := open(t, func() shuttle.Component {
		return &reader{rows: 12, size: 3, height: 20}
	})

	if err := expect(p.Locator("#loaded")).ToHaveText("12"); err != nil {
		height, _ := p.Evaluate(`() => [document.body.scrollHeight, window.innerHeight]`)
		t.Fatalf("the feed stalled with the sentinel on screen (page %v): %v", height, err)
	}
	if err := expect(p.Locator("[data-shuttle-feed-end]")).ToHaveText("the end"); err != nil {
		t.Fatalf("no end marker: %v", err)
	}
}

// TestTheFeedStopsAtTheEnd. The sentinel is what asks for more, so once
// there is no more it has to be gone - otherwise a page that keeps
// re-rendering keeps asking, for as long as the tab is open.
func TestTheFeedStopsAtTheEnd(t *testing.T) {
	p := open(t, func() shuttle.Component {
		return &reader{rows: 12, size: 3, height: 20}
	})

	if err := expect(p.Locator("#loaded")).ToHaveText("12"); err != nil {
		t.Fatalf("the feed did not reach the end: %v", err)
	}
	if err := expect(p.Locator("[data-shuttle-sentinel]")).ToHaveCount(0); err != nil {
		t.Fatalf("the sentinel outlived the last page: %v", err)
	}

	// Nothing further arrives, however much the page is scrolled.
	scrollToBottom(t, p)
	if err := expect(p.Locator("#loaded")).ToHaveText("12"); err != nil {
		t.Errorf("the feed loaded past the end of its source: %v", err)
	}
}

// scrollToBottom scrolls the window and waits for the browser to have
// settled there, since the observer reports after layout rather than during
// it.
func scrollToBottom(t *testing.T, p playwright.Page) {
	t.Helper()
	if _, err := p.Evaluate(`() => window.scrollTo(0, document.body.scrollHeight)`); err != nil {
		t.Fatalf("scroll: %v", err)
	}
}
