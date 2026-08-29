package shuttle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// flooder publishes to the very topic its own session subscribes to - the
// shape that used to deadlock: the in-memory broker delivers on the
// publisher's goroutine, so each message came straight back as a submit to
// the mailbox this goroutine drains, and a bounded mailbox filled up under
// an action still holding it.
type flooder struct {
	Base
	got atomic.Int64
}

func (f *flooder) Mount(context.Context, Params) error {
	return f.Subscribe("flood")
}

func (f *flooder) HandleInfo(context.Context, any) error {
	f.got.Add(1)
	return nil
}

func (f *flooder) Render(context.Context) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<p id="flood">on</p>`)
		return err
	})
}

// TestPublishingToYourOwnTopicCannotWedgeTheSession pins the reason the
// mailbox is unbounded. The count is far past any sensible channel depth on
// purpose: with a bounded mailbox this test does not fail, it hangs.
func TestPublishingToYourOwnTopicCannotWedgeTheSession(t *testing.T) {
	broker, pres := NewMemoryBroker(), newPresence()
	f := &flooder{}
	sess := mountSession(t, "flood", f, broker, pres)

	const messages = 500
	if err := sess.call(context.Background(), func() error {
		for range messages {
			if err := f.Publish(context.Background(), "flood", "again"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("publishing from an action: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return f.got.Load() == messages
	}, "every self-published message to be handled")
}

// slowpoke lets a test hold the session goroutine inside an action.
type slowpoke struct {
	Base
	release  chan struct{}
	finished atomic.Bool
	closed   atomic.Bool
}

func (s *slowpoke) Unmount(context.Context) { s.closed.Store(true) }

func (s *slowpoke) Render(context.Context) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<p id="slow">on</p>`)
		return err
	})
}

// TestCloseWaitsForTheWorkInFlight: teardown runs on the session's own
// goroutine, queued behind whatever is already running - never alongside
// it. The old shape ran Unmount on the caller's goroutine, racing an action
// mid-execution, which is the one thing no component is written to survive.
func TestCloseWaitsForTheWorkInFlight(t *testing.T) {
	s := &slowpoke{release: make(chan struct{})}
	sess := newSession("test", s)

	started := make(chan struct{})
	if err := sess.submit(func() {
		close(started)
		<-s.release
		// Unmount must not have run while this action held the goroutine.
		if s.closed.Load() {
			t.Error("unmounted while an action was still running")
		}
		s.finished.Store(true)
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-started

	go func() {
		// Let close arrive first, then release the action it must wait for.
		time.Sleep(20 * time.Millisecond)
		close(s.release)
	}()

	sess.close(context.Background())
	if !s.finished.Load() {
		t.Error("close returned before the in-flight action finished")
	}
	if !s.closed.Load() {
		t.Error("close returned before Unmount ran")
	}
}

// TestGraceFollowsDeliveryNotMinting: renders coalesce, so generations can
// be minted faster than the stream ships them. The client clicks against
// the last markup it received - so that generation's action table must
// survive however many undelivered generations came and went after it.
func TestGraceFollowsDeliveryNotMinting(t *testing.T) {
	c := &counter{}
	sess := newSession("test", c)
	ctx := context.Background()

	first, err := sess.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Delivered: this is what the page's first paint does.
	sess.markTreeSent()
	staleReset := lastPathSegment(clickURLs(t, first)[1])

	// Two changed renders, neither delivered - a detached stream, or a
	// client the coalescing slot skipped past.
	c.Count = 5
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}
	c.Count = 7
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}

	// The click still resolves: the only generation the client ever held
	// keeps its grace, however many undelivered ones superseded it.
	if err := sess.Invoke(ctx, "c", staleReset); err != nil {
		t.Errorf("click against the last delivered markup: %v", err)
	}
	if c.Count != 0 {
		t.Errorf("count = %d, want 0", c.Count)
	}
}

// TestLateSubscriptionDiesWithItsComponent: a Do closure can reach a
// component after its key stopped being rendered. A Subscribe from there
// must tear down immediately - the unmount that would have done it has
// already run, and a subscription with no teardown lives, and delivers,
// for the rest of the session.
func TestLateSubscriptionDiesWithItsComponent(t *testing.T) {
	broker, pres := NewMemoryBroker(), newPresence()
	r := &room{Topic: "late"}
	sess := mountSession(t, "late", r, broker, pres)

	// Unmount the whole tree the way navigation would.
	sess.close(context.Background())

	if err := r.Subscribe("after"); !errors.Is(err, ErrNotMounted) && err != nil {
		t.Fatalf("subscribe after unmount: %v", err)
	}
	// Whether refused or torn down on the spot, nothing may be listening.
	if err := broker.Publish(context.Background(), "after", "anyone?"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	msgs, _ := r.seen()
	for _, m := range msgs {
		if strings.Contains(m, "anyone?") {
			t.Error("a subscription outlived its component")
		}
	}
}

// TestTwoJoinersOnOnePageLeaveOnce: presence is keyed by session but joined
// per component, so a page can be on a topic twice. The first component to
// unmount must not announce the whole page's departure while the second is
// still there.
func TestTwoJoinersOnOnePageLeaveOnce(t *testing.T) {
	pres := newPresence()

	if _, fresh := pres.join("lobby", "page1", "alice"); !fresh {
		t.Fatal("first join was not fresh")
	}
	if _, fresh := pres.join("lobby", "page1", "alice"); fresh {
		t.Error("second join of the same page reported fresh")
	}

	if _, left := pres.leave("lobby", "page1"); left {
		t.Error("first leave announced a departure with a joiner remaining")
	}
	if got := len(pres.list("lobby")); got != 1 {
		t.Fatalf("roster after first leave: %d members, want 1", got)
	}
	if _, left := pres.leave("lobby", "page1"); !left {
		t.Error("last leave did not announce the departure")
	}
	if got := len(pres.list("lobby")); got != 0 {
		t.Errorf("roster after last leave: %d members, want 0", got)
	}
}

// TestShutdownEndsEverySession: the graceful half of a deploy. Every
// session is torn down and waited for, the stream ends so the page falls
// into its reconnect-then-reload path, and a page load after it answers
// with a refusal rather than a session nothing would ever collect.
func TestShutdownEndsEverySession(t *testing.T) {
	h := New(func() Component { return &counter{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	getPage(t, srv)
	stream := openStream(t, srv, sid)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := h.sessions.len(); got != 0 {
		t.Errorf("%d sessions survived shutdown", got)
	}
	select {
	case <-stream.errs:
	case <-time.After(3 * time.Second):
		t.Error("the stream outlived shutdown")
	}

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("page after shutdown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("page after shutdown: status %d, want 503", resp.StatusCode)
	}
}
