package shuttle

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Informer is implemented by components that receive messages from pub/sub,
// from a timer, or from an application goroutine. HandleInfo runs on the
// session's own goroutine, so it can touch component state directly.
//
// Treat the message as a trigger, not as the data: delivery is
// at-most-once, so a component whose only copy of the truth is the
// messages it has seen shows a hole for every one it missed - see the
// contract on [Broker]. The durable pattern reads its state back from
// somewhere it can always re-fetch, and uses HandleInfo to know when.
type Informer interface {
	HandleInfo(ctx context.Context, msg any) error
}

// Subscribe delivers everything published to topic to this component's
// HandleInfo, and re-renders it afterwards. The subscription is torn down
// when this component is unmounted - not merely when the session ends,
// which for a child would mean delivering into a component that is no
// longer on the page.
//
// Call it from Mount. Subscribing during a render would add a subscription
// per render.
func (b *Base) Subscribe(topic string) error {
	if b.n == nil {
		return ErrNotMounted
	}

	n := b.n
	inf, ok := n.cmp.(Informer)
	if !ok {
		return ErrNotAnInformer
	}

	unsubscribe, err := n.sess.broker.Subscribe(topic, func(msg any) {
		// The broker delivers on whichever goroutine published. Hand the
		// message to the session rather than touching the component here,
		// so it can never run alongside an action.
		_ = n.sess.submit(func() {
			if err := inf.HandleInfo(context.Background(), msg); err != nil {
				n.sess.fail("handle info", err)
				return
			}
			_ = n.push(context.Background())
		})
	})
	if err != nil {
		return err
	}

	n.onClose(unsubscribe)
	return nil
}

// Publish sends a message to every component subscribed to topic, in this
// process or across the cluster, depending on the Broker.
func (b *Base) Publish(ctx context.Context, topic string, msg any) error {
	if b.n == nil {
		return ErrNotMounted
	}
	return b.n.sess.broker.Publish(ctx, topic, msg)
}

// Every calls fn on the session's goroutine at each interval, and
// re-renders the component afterwards. The timer stops when this component
// is unmounted, which for a child means when its parent stops rendering its
// key - otherwise navigating away from a component leaves its timer firing
// at a node that no longer exists.
//
//	func (c *Clock) Mount(ctx context.Context, _ shuttle.Params) error {
//	    return c.Every(time.Second, c.tick)
//	}
func (b *Base) Every(d time.Duration, fn func(context.Context) error) error {
	if b.n == nil {
		return ErrNotMounted
	}
	if d <= 0 {
		return ErrBadInterval
	}

	n := b.n
	stop := make(chan struct{})
	var once sync.Once

	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = n.sess.submit(func() {
					if err := fn(context.Background()); err != nil {
						n.sess.fail("timer", err)
						return
					}
					_ = n.push(context.Background())
				})
			case <-stop:
				return
			case <-n.sess.done:
				return
			}
		}
	}()

	n.onClose(func() { once.Do(func() { close(stop) }) })
	return nil
}

// Do runs fn on the session's own goroutine, where it can touch component
// state without racing an action or a message, and re-renders afterwards.
// This is how application code outside the session mutates a component.
//
// It returns once the work is queued, not once it has run, so it is safe to
// call from anywhere - including from inside an action, where waiting would
// be waiting on itself. The trade is that fn's error has nowhere to go but
// the handler's logger, since the caller is long past by the time it runs.
//
// It is also the wrapper for the three methods that must not be called off
// the session's goroutine - [Base.Navigate], [Base.Replace] and [Base.Emit],
// each of which runs another component's handler inline.
func (b *Base) Do(fn func(context.Context) error) error {
	if b.n == nil {
		return ErrNotMounted
	}

	n := b.n
	return n.sess.submit(func() {
		if err := fn(context.Background()); err != nil {
			n.sess.fail("do", err)
			return
		}
		_ = n.push(context.Background())
	})
}

// --- presence --------------------------------------------------------------

// Member is one participant on a presence topic.
type Member struct {
	// Tag identifies the page the member is connected on, and is the
	// session's [Session.Tag] rather than its id.
	//
	// That distinction is the whole reason this field is named as it is. A
	// roster goes to every subscriber on the topic, so a member carrying the
	// session id would hand each page the capability for all the others -
	// and rendering the roster, which is what a roster is for, would print
	// them into the markup. A tag names a page without unlocking it.
	Tag string
	// Meta is whatever the caller attached at join - a user id, a name.
	Meta any
}

// PresenceEvent is delivered to a topic's subscribers when someone joins or
// leaves it. Components handle it in HandleInfo alongside their own
// messages.
type PresenceEvent struct {
	Topic  string
	Member Member
	// Joined is true for an arrival, false for a departure.
	Joined bool
}

// presence tracks who is on each topic. It is per-Broker rather than
// global, and in-process: a multi-node deployment needs the members shared
// the same way messages are, which is a job for the Broker that owns them.
//
// Membership is counted, not flagged: presence is keyed by session, but
// joining happens per component, and two components on one page can join
// the same topic. Without the count, the first of them to unmount would
// announce the whole page's departure while the other was still listening.
type presence struct {
	mu     sync.RWMutex
	topics map[string]map[string]presenceEntry
}

type presenceEntry struct {
	member Member
	refs   int
}

func newPresence() *presence {
	return &presence{topics: map[string]map[string]presenceEntry{}}
}

func (p *presence) join(topic, tag string, meta any) (Member, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	members, ok := p.topics[topic]
	if !ok {
		members = map[string]presenceEntry{}
		p.topics[topic] = members
	}
	e, rejoin := members[tag]
	e.member = Member{Tag: tag, Meta: meta}
	e.refs++
	members[tag] = e
	return e.member, !rejoin
}

func (p *presence) leave(topic, tag string) (Member, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	members, ok := p.topics[topic]
	if !ok {
		return Member{}, false
	}
	e, ok := members[tag]
	if !ok {
		return Member{}, false
	}
	e.refs--
	if e.refs > 0 {
		members[tag] = e
		return Member{}, false
	}
	delete(members, tag)
	if len(members) == 0 {
		delete(p.topics, topic)
	}
	return e.member, true
}

func (p *presence) list(topic string) []Member {
	p.mu.RLock()
	defer p.mu.RUnlock()

	members := make([]Member, 0, len(p.topics[topic]))
	for _, e := range p.topics[topic] {
		members = append(members, e.member)
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].Tag < members[j].Tag
	})
	return members
}

// Join announces this page on a presence topic and subscribes it to the
// topic's messages, so it hears about everyone who arrives and leaves after
// it. It leaves automatically when this component is unmounted.
//
// It subscribes before announcing, so a joiner hears its own arrival. That
// is deliberate: a component can then build its view of the room from the
// events alone, with no special case for itself.
//
// Call it from Mount, and read the current roster with [Base.Presence].
func (b *Base) Join(ctx context.Context, topic string, meta any) error {
	if b.n == nil {
		return ErrNotMounted
	}
	if err := b.Subscribe(topic); err != nil {
		return err
	}

	sess := b.n.sess
	member, fresh := sess.presence().join(topic, sess.Tag(), meta)
	// A broker that holds the roster is told on the first join only - the
	// refcounting above is local on purpose, since a session lives on one
	// node - and told *before* the PresenceEvent below goes out, so a
	// component re-rendering on the event reads a roster that already
	// holds the member.
	if fresh {
		if pb, ok := sess.broker.(PresenceBroker); ok {
			if err := pb.Join(ctx, topic, member); err != nil {
				sess.presence().leave(topic, sess.Tag())
				return err
			}
		}
	}
	b.n.onClose(func() {
		if m, ok := sess.presence().leave(topic, sess.Tag()); ok {
			if pb, ok := sess.broker.(PresenceBroker); ok {
				if err := pb.Leave(context.Background(), topic, sess.Tag()); err != nil {
					sess.fail("presence leave", err)
				}
			}
			_ = sess.broker.Publish(context.Background(), topic,
				PresenceEvent{Topic: topic, Member: m, Joined: false})
		}
	})

	if !fresh {
		return nil
	}
	return sess.broker.Publish(ctx, topic,
		PresenceEvent{Topic: topic, Member: member, Joined: true})
}

// Presence returns who is currently on a topic, ordered by member tag -
// across every node, when the Broker is a [PresenceBroker]; otherwise this
// process's roster, which on one node is the same thing.
func (b *Base) Presence(topic string) []Member {
	if b.n == nil {
		return nil
	}
	if pb, ok := b.n.sess.broker.(PresenceBroker); ok {
		return pb.Members(topic)
	}
	return b.n.sess.presence().list(topic)
}
