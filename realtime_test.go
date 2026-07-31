package shuttle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// room is the payoff for holding state on the server: it subscribes at
// mount, and anything published reaches every connected page without the
// client asking.
type room struct {
	Base
	Topic string
	Who   string

	mu       sync.Mutex
	messages []string
	joins    []string
}

func (r *room) Mount(ctx context.Context, _ Params) error {
	return r.Join(ctx, r.Topic, r.Who)
}

func (r *room) HandleInfo(_ context.Context, msg any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch m := msg.(type) {
	case PresenceEvent:
		verb := "left"
		if m.Joined {
			verb = "joined"
		}
		r.joins = append(r.joins, fmt.Sprintf("%v %s", m.Member.Meta, verb))
	default:
		r.messages = append(r.messages, fmt.Sprint(msg))
	}
	return nil
}

func (r *room) seen() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messages...), append([]string(nil), r.joins...)
}

func (r *room) Render(ctx context.Context) templ.Component {
	msgs, joins := r.seen()
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<ul id="log">%s|%s</ul>`,
			strings.Join(msgs, ","), strings.Join(joins, ","))
		return err
	})
}

// clock exercises timers.
type clock struct {
	Base
	mu    sync.Mutex
	ticks int
}

func (c *clock) Mount(context.Context, Params) error {
	return c.Every(5*time.Millisecond, c.tick)
}

func (c *clock) tick(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ticks++
	return nil
}

func (c *clock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ticks
}

func (c *clock) Render(ctx context.Context) templ.Component {
	n := c.count()
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p id="ticks">%d</p>`, n)
		return err
	})
}

// deaf subscribes without implementing Informer.
type deaf struct{ counter }

// mountSession builds a session sharing a broker and roster, the way a
// handler's sessions do.
func mountSession(t *testing.T, id string, cmp Component, broker Broker, pres *presence) *Session {
	t.Helper()
	sess := newSessionWith(id, cmp, broker, pres, func(what string, err error) {
		t.Errorf("session error in %s: %v", what, err)
	})
	t.Cleanup(func() { sess.close(context.Background()) })

	if err := sess.call(func() error {
		if m, ok := cmp.(Mounter); ok {
			if err := m.Mount(context.Background(), nil); err != nil {
				return err
			}
		}
		_, err := sess.Render(context.Background())
		return err
	}); err != nil {
		t.Fatalf("mount %s: %v", id, err)
	}
	return sess
}

// TestPublishReachesEverySubscriber. The stream is already open, so server
// push is nearly free - and it is the reason to hold state on the server at
// all.
func TestPublishReachesEverySubscriber(t *testing.T) {
	broker, pres := NewMemoryBroker(), newPresence()
	a, b := &room{Topic: "lobby"}, &room{Topic: "lobby"}
	mountSession(t, "a", a, broker, pres)
	sessB := mountSession(t, "b", b, broker, pres)

	if err := broker.Publish(context.Background(), "lobby", "hello"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	for _, r := range []*room{a, b} {
		waitFor(t, time.Second, func() bool {
			msgs, _ := r.seen()
			return len(msgs) == 1 && msgs[0] == "hello"
		}, "the message to arrive")
	}

	// And the subscriber re-renders itself, so the page sees it.
	waitFor(t, time.Second, func() bool {
		for _, p := range sessB.take() {
			if strings.Contains(p.html, "hello") {
				return true
			}
		}
		return false
	}, "the patch carrying the message")
}

// TestPresenceTracksWhoIsHere, and tells the room who comes and goes.
//
// A joiner hears its own arrival, because Join subscribes before it
// announces. That is deliberate: it means a component can render the roster
// from the events alone, without a special case for itself.
func TestPresenceTracksWhoIsHere(t *testing.T) {
	broker, pres := NewMemoryBroker(), newPresence()

	first := &room{Topic: "lobby", Who: "ana"}
	mountSession(t, "a", first, broker, pres)

	if got := first.Presence("lobby"); len(got) != 1 || got[0].Session != "a" {
		t.Fatalf("roster after one join = %v", got)
	}
	waitFor(t, time.Second, func() bool {
		_, joins := first.seen()
		return fmt.Sprint(joins) == "[ana joined]"
	}, "the joiner to hear itself")

	second := &room{Topic: "lobby", Who: "bo"}
	sessB := mountSession(t, "b", second, broker, pres)

	if got := first.Presence("lobby"); len(got) != 2 {
		t.Errorf("roster after two joins = %v", got)
	}
	waitFor(t, time.Second, func() bool {
		_, joins := first.seen()
		return fmt.Sprint(joins) == "[ana joined bo joined]"
	}, "the arrival to be announced")
	// The newcomer hears only itself; it missed what happened before it.
	waitFor(t, time.Second, func() bool {
		_, joins := second.seen()
		return fmt.Sprint(joins) == "[bo joined]"
	}, "the newcomer to hear itself")

	// Leaving is announced too, and drops the member from the roster.
	sessB.close(context.Background())
	waitFor(t, time.Second, func() bool {
		return len(first.Presence("lobby")) == 1
	}, "the roster to shrink")
	waitFor(t, time.Second, func() bool {
		_, joins := first.seen()
		return fmt.Sprint(joins) == "[ana joined bo joined bo left]"
	}, "the departure to be announced")
}

// TestSubscriptionsStopWithTheSession: a message arriving after eviction
// must not reach a component that has been unmounted.
func TestSubscriptionsStopWithTheSession(t *testing.T) {
	broker, pres := NewMemoryBroker(), newPresence()
	r := &room{Topic: "lobby"}
	sess := mountSession(t, "a", r, broker, pres)

	sess.close(context.Background())
	if err := broker.Publish(context.Background(), "lobby", "after"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	msgs, _ := r.seen()
	for _, m := range msgs {
		if m == "after" {
			t.Error("a closed session still received a message")
		}
	}
}

// TestSubscribeNeedsAnInformer, rather than subscribing to nothing.
func TestSubscribeNeedsAnInformer(t *testing.T) {
	sess := newSession("test", &deaf{})
	t.Cleanup(func() { sess.close(context.Background()) })

	d := sess.Component().(*deaf)
	if err := d.Subscribe("lobby"); !errors.Is(err, ErrNotAnInformer) {
		t.Errorf("Subscribe = %v, want ErrNotAnInformer", err)
	}
}

// TestEveryTicksAndStops.
func TestEveryTicksAndStops(t *testing.T) {
	c := &clock{}
	sess := newSession("test", c)
	if err := sess.call(func() error {
		if err := c.Mount(context.Background(), nil); err != nil {
			return err
		}
		_, err := sess.Render(context.Background())
		return err
	}); err != nil {
		t.Fatalf("mount: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return c.count() >= 3 }, "the timer to tick")

	sess.close(context.Background())
	settled := c.count()
	time.Sleep(60 * time.Millisecond)

	if got := c.count(); got != settled {
		t.Errorf("timer kept running after close: %d then %d", settled, got)
	}
}

// TestEveryRejectsANonPositiveInterval, which would spin.
func TestEveryRejectsANonPositiveInterval(t *testing.T) {
	sess := newSession("test", &clock{})
	t.Cleanup(func() { sess.close(context.Background()) })

	c := sess.Component().(*clock)
	if err := c.Every(0, c.tick); !errors.Is(err, ErrBadInterval) {
		t.Errorf("Every(0) = %v, want ErrBadInterval", err)
	}
}

// TestRealtimeMethodsNeedAMount: every one of them has to fail cleanly on a
// component nobody mounted, which is the shape of a unit test.
func TestRealtimeMethodsNeedAMount(t *testing.T) {
	var r room
	ctx := context.Background()

	for name, err := range map[string]error{
		"Subscribe": r.Subscribe("t"),
		"Publish":   r.Publish(ctx, "t", "m"),
		"Join":      r.Join(ctx, "t", nil),
		"Every":     r.Every(time.Second, func(context.Context) error { return nil }),
		"Do":        r.Do(func(context.Context) error { return nil }),
	} {
		if !errors.Is(err, ErrNotMounted) {
			t.Errorf("%s = %v, want ErrNotMounted", name, err)
		}
	}
	if got := r.Presence("t"); got != nil {
		t.Errorf("Presence on an unmounted component = %v", got)
	}
}

// TestWorkIsSerialised is the property the session's goroutine exists for:
// a pub/sub message, a timer tick and an action can never be inside the
// same component at once, so its fields need no locks of their own.
func TestWorkIsSerialised(t *testing.T) {
	sess := newSession("test", &counter{})
	t.Cleanup(func() { sess.close(context.Background()) })

	var (
		inside int
		clash  bool
		mu     sync.Mutex
		wg     sync.WaitGroup
	)

	for range 50 {
		wg.Go(func() {
			_ = sess.call(func() error {
				mu.Lock()
				inside++
				if inside > 1 {
					clash = true
				}
				mu.Unlock()

				time.Sleep(time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		})
	}
	wg.Wait()

	if clash {
		t.Error("two pieces of session work ran at the same time")
	}
}

// TestBrokerUnsubscribeIsIdempotent, since teardown can run more than once.
func TestBrokerUnsubscribeIsIdempotent(t *testing.T) {
	b := NewMemoryBroker()
	var got int

	cancel, err := b.Subscribe("t", func(any) { got++ })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	cancel()
	cancel()

	if err := b.Publish(context.Background(), "t", "x"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got != 0 {
		t.Errorf("delivered %d messages after unsubscribe", got)
	}
}

// TestPublishFromASubscriberDoesNotDeadlock: the broker delivers outside
// its own lock, so a component answering a message by publishing one is
// fine.
func TestPublishFromASubscriberDoesNotDeadlock(t *testing.T) {
	b := NewMemoryBroker()
	done := make(chan struct{})

	if _, err := b.Subscribe("ping", func(any) {
		_ = b.Publish(context.Background(), "pong", "reply")
	}); err != nil {
		t.Fatalf("subscribe ping: %v", err)
	}
	if _, err := b.Subscribe("pong", func(any) { close(done) }); err != nil {
		t.Fatalf("subscribe pong: %v", err)
	}

	go func() { _ = b.Publish(context.Background(), "ping", "hello") }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publishing from a subscriber deadlocked")
	}
}

// TestUnmountStopsATimer is the bug that navigating between components
// exposed: a timer registered against the session outlives the component
// that started it, so it keeps firing at a node that is no longer in the
// page. Datastar reports that as PatchElementsNoTargetsFound and the server
// never hears about it.
func TestUnmountStopsATimer(t *testing.T) {
	l := &ticking{Labels: []string{"a"}}
	sess := newSession("test", l)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}

	n, ok := sess.node("c-1")
	if !ok {
		t.Fatal("child not mounted")
	}
	child := n.cmp.(*ticker)
	waitFor(t, time.Second, func() bool { return child.count() > 0 }, "the timer to tick")

	// Stop rendering its key: the child is unmounted.
	l.Labels = nil
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if _, still := sess.node("c-1"); still {
		t.Fatal("child was not unmounted")
	}

	settled := child.count()
	time.Sleep(80 * time.Millisecond)
	if got := child.count(); got != settled {
		t.Errorf("timer kept firing after unmount: %d then %d", settled, got)
	}
}

// TestUnmountStopsASubscription, the same way.
func TestUnmountStopsASubscription(t *testing.T) {
	broker := NewMemoryBroker()
	l := &ticking{Labels: []string{"a"}, Broker: broker}
	sess := newSessionWith("test", l, broker, newPresence(), nil)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	n, _ := sess.node("c-1")
	child := n.cmp.(*ticker)

	l.Labels = nil
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}

	if err := broker.Publish(ctx, "ticks", "after"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	if child.heard() != 0 {
		t.Errorf("an unmounted child still received %d messages", child.heard())
	}
}

// TestAnUnmountedComponentCannotBeMarkedDirty is the backstop: whatever
// still holds a reference - a message already in flight, a timer mid-tick -
// must not produce a patch for an element that is gone.
func TestAnUnmountedComponentCannotBeMarkedDirty(t *testing.T) {
	l := &ticking{Labels: []string{"a"}}
	sess := newSession("test", l)
	t.Cleanup(func() { sess.close(context.Background()) })
	ctx := context.Background()

	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	n, _ := sess.node("c-1")

	l.Labels = nil
	if _, err := sess.Render(ctx); err != nil {
		t.Fatalf("re-render: %v", err)
	}
	sess.take() // drop the parent's re-render

	if err := n.push(ctx); !errors.Is(err, ErrNotMounted) {
		t.Errorf("push on an unmounted component = %v, want ErrNotMounted", err)
	}
	if got := sess.take(); got != nil {
		t.Errorf("an unmounted component produced patches: %v", got)
	}
}

// ticking renders a child per label, so dropping a label unmounts it.
type ticking struct {
	Base
	Labels []string
	Broker Broker
}

func (l *ticking) Render(ctx context.Context) templ.Component {
	labels := append([]string(nil), l.Labels...)
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		for _, label := range labels {
			c := Child(ctx, label, func() Component { return &ticker{} })
			if err := c.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

// ticker starts a timer and a subscription at mount, both of which must
// stop when it is unmounted.
type ticker struct {
	Base
	mu    sync.Mutex
	ticks int
	msgs  int
}

func (c *ticker) Mount(context.Context, Params) error {
	if err := c.Subscribe("ticks"); err != nil {
		return err
	}
	return c.Every(5*time.Millisecond, func(context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.ticks++
		return nil
	})
}

func (c *ticker) HandleInfo(context.Context, any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs++
	return nil
}

func (c *ticker) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ticks
}

func (c *ticker) heard() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.msgs
}

func (c *ticker) Render(context.Context) templ.Component {
	return templ.Raw(`<p>tick</p>`)
}
