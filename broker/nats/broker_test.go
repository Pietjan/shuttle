package nats_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natsclient "github.com/nats-io/nats.go"

	"github.com/pietjan/shuttle"
	natsbroker "github.com/pietjan/shuttle/broker/nats"
)

// note is a message type an application would publish. Registered once,
// the way every node must.
type note struct {
	From, Text string
}

func init() { natsbroker.Register(note{}) }

// cluster is an embedded NATS server and however many connections a test
// wants into it - each one standing in for a node. A real server rather
// than a fake: the tests are about messages surviving the wire.
func cluster(t *testing.T) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Port: -1})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server never came up")
	}
	return srv
}

func connect(t *testing.T, srv *natsserver.Server) *natsclient.Conn {
	t.Helper()
	nc, err := natsclient.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// TestAMessageCrossesNodesWithItsType. The whole point of the gob
// envelope: HandleInfo's type-switch on the far node must see the same
// concrete type the publisher sent.
func TestAMessageCrossesNodesWithItsType(t *testing.T) {
	srv := cluster(t)
	a := natsbroker.New(connect(t, srv))
	b := natsbroker.New(connect(t, srv))

	got := make(chan any, 1)
	unsubscribe, err := b.Subscribe("room", func(msg any) { got <- msg })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(unsubscribe)

	if err := a.Publish(context.Background(), "room", note{From: "ada", Text: "hi"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-got:
		n, ok := msg.(note)
		if !ok {
			t.Fatalf("arrived as %T, want note - the concrete type did not survive the wire", msg)
		}
		if n.From != "ada" || n.Text != "hi" {
			t.Errorf("arrived as %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the message never crossed")
	}
}

// TestPresenceEventsCrossWithoutRegistration - shuttle publishes them
// itself, so the broker registers them itself.
func TestPresenceEventsCrossWithoutRegistration(t *testing.T) {
	srv := cluster(t)
	a := natsbroker.New(connect(t, srv))
	b := natsbroker.New(connect(t, srv))

	got := make(chan any, 1)
	unsubscribe, err := b.Subscribe("lobby", func(msg any) { got <- msg })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(unsubscribe)

	ev := shuttle.PresenceEvent{
		Topic:  "lobby",
		Member: shuttle.Member{Tag: "abc123", Meta: "ada"},
		Joined: true,
	}
	if err := a.Publish(context.Background(), "lobby", ev); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-got:
		e, ok := msg.(shuttle.PresenceEvent)
		if !ok {
			t.Fatalf("arrived as %T, want PresenceEvent", msg)
		}
		if e.Member.Tag != "abc123" || e.Member.Meta != "ada" {
			t.Errorf("arrived as %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the presence event never crossed")
	}
}

// TestAnUnregisteredTypeFailsAtPublish, on the developer's machine, not in
// production's second region - the in-memory broker would have delivered
// it happily.
func TestAnUnregisteredTypeFailsAtPublish(t *testing.T) {
	type never struct{ X int }
	srv := cluster(t)
	a := natsbroker.New(connect(t, srv))

	err := a.Publish(context.Background(), "room", never{X: 1})
	if err == nil {
		t.Fatal("publishing an unregistered type succeeded")
	}
	if !strings.Contains(err.Error(), "Registered") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

// TestUnsubscribeStopsDelivery, and topics with subject syntax in them
// stay distinct topics rather than becoming NATS hierarchy.
func TestUnsubscribeStopsDeliveryAndTopicsAreOpaque(t *testing.T) {
	srv := cluster(t)
	a := natsbroker.New(connect(t, srv))
	b := natsbroker.New(connect(t, srv))

	var mu sync.Mutex
	var seen []string
	subscribe := func(topic string) func() {
		unsubscribe, err := b.Subscribe(topic, func(msg any) {
			mu.Lock()
			seen = append(seen, topic+":"+msg.(note).Text)
			mu.Unlock()
		})
		if err != nil {
			t.Fatalf("subscribe %q: %v", topic, err)
		}
		return unsubscribe
	}

	// "rooms.>" would be a wildcard as a raw subject; as a topic it is just
	// a name, and must not receive "rooms.1"'s message.
	stop := subscribe("rooms.>")
	subscribe("rooms.1")

	if err := a.Publish(context.Background(), "rooms.1", note{Text: "one"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	if fmt.Sprint(seen) != "[rooms.1:one]" {
		t.Errorf("delivered to %v, want only the exact topic", seen)
	}
	mu.Unlock()

	stop()
	if err := a.Publish(context.Background(), "rooms.>", note{Text: "late"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(seen) != 1 {
		t.Errorf("an unsubscribed topic still received: %v", seen)
	}
	mu.Unlock()
}

// echo is a component that shows what it hears - one per "node" in the
// cross-handler test.
type echo struct {
	shuttle.Base
	mu   sync.Mutex
	last string
}

func (e *echo) Mount(context.Context, shuttle.Params) error { return e.Subscribe("room") }

func (e *echo) HandleInfo(_ context.Context, msg any) error {
	n, ok := msg.(note)
	if !ok {
		return fmt.Errorf("heard a %T", msg)
	}
	e.mu.Lock()
	e.last = n.Text
	e.mu.Unlock()
	return nil
}

func (e *echo) say(ctx context.Context, text string) error {
	return e.Publish(ctx, "room", note{From: "me", Text: text})
}

func (e *echo) Render(ctx context.Context) templ.Component {
	e.mu.Lock()
	last := e.last
	e.mu.Unlock()
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p id="last">%s</p>`, templ.EscapeString(last))
		return err
	})
}

// TestTwoHandlersHearEachOther is the whole feature end to end: two
// Handlers - two nodes, separate registries, separate processes in
// everything but address space - sharing one NATS cluster. A component on
// one publishes; the component on the other re-renders down its own
// page's stream.
func TestTwoHandlersHearEachOther(t *testing.T) {
	srv := cluster(t)

	page := func() (*httptest.Server, *echo) {
		e := &echo{}
		h := shuttle.New(func() shuttle.Component { return e })
		h.Broker = natsbroker.New(connect(t, srv))
		ts := httptest.NewTestServer(t, h)
		t.Cleanup(ts.Close)
		if _, err := ts.Client().Get(ts.URL + "/"); err != nil {
			t.Fatalf("page: %v", err)
		}
		return ts, e
	}

	_, speaker := page()
	_, listener := page()

	if err := speaker.Do(func(ctx context.Context) error {
		return speaker.say(ctx, "across the wire")
	}); err != nil {
		t.Fatalf("say: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		listener.mu.Lock()
		last := listener.last
		listener.mu.Unlock()
		if last == "across the wire" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the other handler heard %q, want the message", last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// eventually polls cond against a real wire, where "it arrived" has no
// deadline the test can subscribe to.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func tags(members []shuttle.Member) string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.Tag
	}
	return strings.Join(out, ",")
}

// TestTheRosterSpansNodes: joins on any node appear in every node's
// Members - including a node that started after the join, whose sync
// request fills its mirror without waiting a heartbeat interval.
func TestTheRosterSpansNodes(t *testing.T) {
	srv := cluster(t)
	a := natsbroker.New(connect(t, srv))
	t.Cleanup(a.Close)

	if err := a.Join(context.Background(), "lobby", shuttle.Member{Tag: "aa", Meta: "ada"}); err != nil {
		t.Fatalf("join: %v", err)
	}
	// The joiner's own read is synchronous, like the in-memory roster.
	if got := tags(a.Members("lobby")); got != "aa" {
		t.Fatalf("own roster = %q immediately after joining, want aa", got)
	}

	// A node arriving later - its first read may race the sync answer, but
	// only briefly, not a heartbeat.
	b := natsbroker.New(connect(t, srv))
	t.Cleanup(b.Close)
	if err := b.Join(context.Background(), "lobby", shuttle.Member{Tag: "bb", Meta: "bob"}); err != nil {
		t.Fatalf("join: %v", err)
	}

	eventually(t, "the late node to see both", func() bool { return tags(b.Members("lobby")) == "aa,bb" })
	eventually(t, "the first node to see both", func() bool { return tags(a.Members("lobby")) == "aa,bb" })

	if err := a.Leave(context.Background(), "lobby", "aa"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	eventually(t, "the leave to reach the other node", func() bool { return tags(b.Members("lobby")) == "bb" })
}

// TestACrashedNodesMembersAgeOut. Nothing stores the roster, so a node
// that dies without leaving takes its members with it once its heartbeats
// stop - the self-cleaning a crashed node's ghost sessions need.
func TestACrashedNodesMembersAgeOut(t *testing.T) {
	srv := cluster(t)
	fast := natsbroker.PresenceInterval(30 * time.Millisecond)

	dying := natsbroker.New(connect(t, srv), fast)
	watcher := natsbroker.New(connect(t, srv), fast)
	t.Cleanup(watcher.Close)

	if err := dying.Join(context.Background(), "lobby", shuttle.Member{Tag: "dd", Meta: "doomed"}); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := watcher.Join(context.Background(), "lobby", shuttle.Member{Tag: "ww", Meta: "here"}); err != nil {
		t.Fatalf("join: %v", err)
	}
	eventually(t, "both on the roster", func() bool { return tags(watcher.Members("lobby")) == "dd,ww" })

	// The node dies: no Leave, heartbeats just stop.
	dying.Close()

	eventually(t, "the dead node's member to age out", func() bool {
		return tags(watcher.Members("lobby")) == "ww"
	})
}

// roomful joins on mount and renders the roster, one per handler in the
// cross-node test.
type roomful struct {
	shuttle.Base
	Who string
}

func (r *roomful) Mount(ctx context.Context, _ shuttle.Params) error {
	return r.Join(ctx, "lobby", r.Who)
}

func (r *roomful) HandleInfo(context.Context, any) error { return nil }

func (r *roomful) names() string {
	members := r.Presence("lobby")
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = fmt.Sprint(m.Meta)
	}
	return strings.Join(out, ",")
}

func (r *roomful) Render(ctx context.Context) templ.Component {
	names := r.names()
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p id="who">%s</p>`, templ.EscapeString(names))
		return err
	})
}

// TestPresenceSpansHandlers is the roster end to end: two Handlers on two
// broker instances, each page's component listing both members through
// nothing but Base.Presence.
func TestPresenceSpansHandlers(t *testing.T) {
	srv := cluster(t)

	page := func(who string) *roomful {
		r := &roomful{Who: who}
		h := shuttle.New(func() shuttle.Component { return r })
		h.Broker = natsbroker.New(connect(t, srv))
		ts := httptest.NewTestServer(t, h)
		t.Cleanup(ts.Close)
		if _, err := ts.Client().Get(ts.URL + "/"); err != nil {
			t.Fatalf("page: %v", err)
		}
		return r
	}

	ada := page("ada")
	bob := page("bob")

	eventually(t, "ada to see both", func() bool { return ada.names() == "ada,bob" })
	eventually(t, "bob to see both", func() bool { return bob.names() == "ada,bob" })
}
