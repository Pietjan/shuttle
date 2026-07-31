package shuttle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/timeline"
)

// feed streams its entries rather than holding them: the component keeps a
// counter, not a collection, which is the whole point.
type feed struct {
	Base
	Sent int
}

func (f *feed) entry(key, text string) templ.Component {
	id := f.Stream("log").ItemID(key)
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<li id=%q>%s</li>`, id, templ.EscapeString(text))
		return err
	})
}

// add is what an action or a pub/sub message would call.
func (f *feed) add(ctx context.Context, text string) error {
	f.Sent++
	return f.Stream("log").Append(ctx, fmt.Sprint(f.Sent), f.entry(fmt.Sprint(f.Sent), text))
}

func (f *feed) Render(ctx context.Context) templ.Component {
	post := button.New(
		OnClick(ctx, button.Attr, func(actx context.Context) error {
			return f.add(actx, "posted")
		}),
	)
	s := f.Stream("log")

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := fmt.Fprintf(w, `<p id="sent">%d</p><ul%s></ul>`, f.Sent, s.Attrs()); err != nil {
			return err
		}
		return post.Render(templ.WithChildren(ctx, templ.Raw("post")), w)
	})
}

// mounted returns a session whose component has rendered once.
func streamSession(t *testing.T, cmp Component) *Session {
	t.Helper()
	sess := newSession("test", cmp)
	t.Cleanup(func() { sess.close(context.Background()) })
	if _, err := sess.Render(context.Background()); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sess
}

// TestStreamContainerSurvivesARerender: the container carries
// data-ignore-morph, without which the component's next re-render would
// wipe every item it no longer remembers holding.
func TestStreamContainerSurvivesARerender(t *testing.T) {
	f := &feed{}
	sess := streamSession(t, f)

	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(markup, `<ul id="shuttle-c-log" data-ignore-morph>`) {
		t.Errorf("container is not protected from morphing: %q", markup)
	}
}

// TestStreamAppendSendsOnlyTheItem. A component that re-rendered the whole
// list would have to hold the whole list.
func TestStreamAppendSendsOnlyTheItem(t *testing.T) {
	f := &feed{}
	sess := streamSession(t, f)
	ctx := context.Background()

	for _, text := range []string{"first", "second"} {
		if err := f.add(ctx, text); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got := sess.take()
	if len(got) != 2 {
		t.Fatalf("two appends produced %d patches, want 2", len(got))
	}
	for i, want := range []string{"first", "second"} {
		if got[i].target != "shuttle-c-log" {
			t.Errorf("patch %d targets %q, want the container", i, got[i].target)
		}
		if got[i].mode != "append" {
			t.Errorf("patch %d mode = %q, want append", i, got[i].mode)
		}
		if !strings.Contains(got[i].html, want) {
			t.Errorf("patch %d = %q, want it to contain %q", i, got[i].html, want)
		}
		// Only the item travels, not the collection.
		if strings.Contains(got[i].html, "<ul") {
			t.Errorf("patch %d carried the container: %q", i, got[i].html)
		}
	}
}

// TestStreamOpsKeepTheirOrder: two appends are two items, not one state
// superseding another, so they must not coalesce the way re-renders do.
func TestStreamOpsKeepTheirOrder(t *testing.T) {
	f := &feed{}
	sess := streamSession(t, f)
	ctx := context.Background()

	for i := range 5 {
		if err := f.add(ctx, fmt.Sprintf("item %d", i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got := sess.take()
	if len(got) != 5 {
		t.Fatalf("five appends produced %d patches, want 5", len(got))
	}
	for i, p := range got {
		if !strings.Contains(p.html, fmt.Sprintf("item %d", i)) {
			t.Errorf("patch %d out of order: %q", i, p.html)
		}
	}
}

// TestStreamReplaceAndRemoveAddressTheItem.
func TestStreamReplaceAndRemoveAddressTheItem(t *testing.T) {
	f := &feed{}
	sess := streamSession(t, f)
	ctx := context.Background()
	s := f.Stream("log")

	if err := f.add(ctx, "original"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Replace(ctx, "1", f.entry("1", "edited")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := s.Remove(ctx, "1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := sess.take()
	if len(got) != 4 {
		t.Fatalf("got %d patches, want 4", len(got))
	}
	for i, want := range []struct {
		target, mode string
	}{
		{"shuttle-c-log", "append"},
		{"shuttle-c-log-1", "outer"},
		{"shuttle-c-log-1", "remove"},
		{"shuttle-c-log", "inner"},
	} {
		if got[i].target != want.target || string(got[i].mode) != want.mode {
			t.Errorf("patch %d = %s/%s, want %s/%s",
				i, got[i].target, got[i].mode, want.target, want.mode)
		}
	}
	if !strings.Contains(got[1].html, "edited") {
		t.Errorf("replace did not carry the new markup: %q", got[1].html)
	}
}

// TestStreamItemMustCarryItsID. An item without the id can never be
// replaced or removed, and nothing downstream would report it - Datastar
// drops a patch whose target is missing with only a console warning.
func TestStreamItemMustCarryItsID(t *testing.T) {
	f := &feed{}
	streamSession(t, f)

	naked := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<li>no id here</li>`)
		return err
	})

	err := f.Stream("log").Append(context.Background(), "1", naked)
	if err == nil {
		t.Fatal("append accepted an item with no id")
	}
	if !strings.Contains(err.Error(), "shuttle-c-log-1") {
		t.Errorf("error does not name the missing id: %v", err)
	}
}

// TestStreamOnAnUnmountedComponent.
func TestStreamOnAnUnmountedComponent(t *testing.T) {
	var f feed
	if s := f.Stream("log"); s != nil {
		t.Errorf("Stream on an unmounted component = %v, want nil", s)
	}
}

// TestStreamIDsAreScopedToTheComponent, so two components streaming a "log"
// do not patch into each other.
func TestStreamIDsAreScopedToTheComponent(t *testing.T) {
	sess := newSession("test", &list{Labels: []string{"a"}})
	t.Cleanup(func() { sess.close(context.Background()) })
	if _, err := sess.Render(context.Background()); err != nil {
		t.Fatalf("render: %v", err)
	}

	parent, _ := sess.node("c")
	child, _ := sess.node("c-1")

	p := (&Base{n: parent}).Stream("log")
	c := (&Base{n: child}).Stream("log")

	if p.ContainerID() != "shuttle-c-log" {
		t.Errorf("parent container = %q", p.ContainerID())
	}
	if c.ContainerID() != "shuttle-c-1-log" {
		t.Errorf("child container = %q", c.ContainerID())
	}
	if p.ItemID("7") == c.ItemID("7") {
		t.Errorf("two components share an item id: %q", p.ItemID("7"))
	}
}

// TestStreamOverTheTransport drives it end to end: a click appends one item
// and the page receives an append patch, not a re-render of the list.
func TestStreamOverTheTransport(t *testing.T) {
	h := New(func() Component { return &feed{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	if code := post(t, srv, clickURLs(t, fragment(t, page))[0]); code != http.StatusNoContent {
		t.Fatalf("post: status %d", code)
	}

	// The component re-renders (its counter changed) and the item is
	// appended. Both arrive; only the order between them is fixed.
	var appended, rerendered bool
	deadline := time.After(3 * time.Second)
	for !appended || !rerendered {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for both patches")
		default:
		}

		evt := stream.event(t)
		switch {
		case strings.Contains(evt, "data: mode append"):
			appended = true
			if !strings.Contains(evt, `<li id="shuttle-c-log-1">posted</li>`) {
				t.Errorf("append patch = %q", evt)
			}
			if !strings.Contains(evt, "data: selector #shuttle-c-log") {
				t.Errorf("append did not target the container: %q", evt)
			}
		case strings.Contains(evt, `<p id="sent">1</p>`):
			rerendered = true
		}
	}
}

// TestUnmountedComponentStreamsNothing is the bug navigating between
// components exposed, and it is invisible from the server: a stream
// operation queued by a timer that fired just as its component was
// unmounted patches a container that is no longer in the page. Datastar
// reports PatchElementsNoTargetsFound in the console and nothing reaches
// the server at all.
//
// Stream operations need their own check because they never go through the
// dirty set, so guarding renders alone does not cover them.
func TestUnmountedComponentStreamsNothing(t *testing.T) {
	p := &switcher{Shown: "ticker"}
	sess := newSession("test", p)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	// Show it, let it stream, then switch away - twice, because coming back
	// to a component mounts a second instance under the same key and it is
	// the second unmount that used to leak.
	for round := range 2 {
		p.Shown = "ticker"
		if _, err := sess.Render(ctx); err != nil {
			t.Fatalf("round %d: render: %v", round, err)
		}
		waitFor(t, time.Second, func() bool {
			return len(sess.take()) > 0
		}, "the stream to produce something")

		p.Shown = "quiet"
		if _, err := sess.Render(ctx); err != nil {
			t.Fatalf("round %d: switch away: %v", round, err)
		}
		sess.take()

		time.Sleep(80 * time.Millisecond)
		if got := sess.take(); len(got) > 0 {
			t.Errorf("round %d: %d patches after unmount, first targeting %q",
				round, len(got), got[0].target)
		}
	}
}

// switcher renders exactly one child, and which one changes - the shape of
// any app that navigates between views.
type switcher struct {
	Base
	Shown string
}

func (s *switcher) Render(ctx context.Context) templ.Component {
	shown := s.Shown
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return Child(ctx, shown, func() Component {
			if shown == "ticker" {
				return &tickingStream{}
			}
			return &quiet{}
		}).Render(ctx, w)
	})
}

// tickingStream appends to a stream on a timer, like a log or a feed.
type tickingStream struct {
	Base
	n int
}

func (t *tickingStream) Mount(context.Context, Params) error {
	return t.Every(10*time.Millisecond, func(ctx context.Context) error {
		t.n++
		key := strconv.Itoa(t.n)
		return t.Stream("log").Append(ctx, key,
			templ.Raw(`<li id="`+t.Stream("log").ItemID(key)+`">tick</li>`))
	})
}

func (t *tickingStream) Render(context.Context) templ.Component {
	return templ.Raw(`<ul` + t.Stream("log").Attrs() + `></ul>`)
}

type quiet struct{ Base }

func (q *quiet) Render(context.Context) templ.Component { return templ.Raw(`<p>quiet</p>`) }

// loomFeed streams into a Loom component rather than into markup it wrote
// itself, which is what Container exists for: Attrs returns a string, and a
// component takes options.
type loomFeed struct {
	Base
	Sent int
}

func (f *loomFeed) Render(ctx context.Context) templ.Component {
	s := f.Stream("log")
	return timeline.New(Container(s, timeline.Attr))
}

// TestStreamContainerBindsALoomComponent. The same two attributes Attrs
// writes, arriving as an option - without this the container can only ever
// be markup the component wrote itself.
func TestStreamContainerBindsALoomComponent(t *testing.T) {
	cmp := &loomFeed{}
	sess := streamSession(t, cmp)

	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// The patch target, and the reason a re-render does not wipe the items.
	want := `id="` + cmp.Stream("log").ContainerID() + `"`
	if !strings.Contains(markup, want) {
		t.Errorf("missing %q in %q", want, markup)
	}
	if !strings.Contains(markup, "data-ignore-morph") {
		t.Errorf("the container does not ignore morphs: %q", markup)
	}
	// On the component itself, not on a wrapper around it.
	if !strings.Contains(markup, `data-ui="timeline"`) {
		t.Errorf("the loom component did not render: %q", markup)
	}

	// And an item still lands in it.
	if err := cmp.Stream("log").Append(context.Background(), "1",
		templ.Raw(`<div id="`+cmp.Stream("log").ItemID("1")+`">tick</div>`)); err != nil {
		t.Fatalf("append: %v", err)
	}
}
