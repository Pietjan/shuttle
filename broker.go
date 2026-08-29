package shuttle

import (
	"context"
	"sync"
)

// Broker carries messages between sessions. The default is in-memory,
// which is the right answer for a single process; a deployment across
// several nodes supplies one backed by Redis or NATS, and nothing above
// this interface changes.
//
// Publish must not block on delivery, and deliver is called on whichever
// goroutine the broker chooses - a subscriber hands the message to its own
// session rather than acting on it directly.
//
// # The contract is the distributed one, even in memory
//
// Write components against what any broker can promise, not against what
// the in-memory one happens to do - the gap between the two is exactly
// what breaks the day a real broker is swapped in, and by then the
// components relying on it are everywhere. Concretely:
//
//   - Delivery is at-most-once and asynchronous in principle. A message can
//     be lost to a network partition, and a subscriber can miss everything
//     published while its node - or its page, across a reconnect - was
//     away. So treat a message as an invalidation, not as the data: derive
//     the view from state that can be re-fetched, and let HandleInfo
//     trigger the re-fetch. A component that accumulates messages as its
//     only copy of the truth is quietly betting on delivery guarantees this
//     interface never made.
//   - Ordering is only what the transport gives. The in-memory broker
//     delivers in publish order because it is one process; nothing here
//     promises that across nodes, or across topics anywhere.
//   - Messages are values that may cross a process boundary. In one
//     process a published pointer works - and shares live memory between
//     sessions, each mutating it on its own goroutine. Publish plain data
//     a distributed broker could serialize (and PresenceEvent, which
//     shuttle publishes itself, is exactly that shape); a pointer meant as
//     shared state is a race today and a silent drop tomorrow.
type Broker interface {
	// Publish sends msg to everything subscribed to topic.
	Publish(ctx context.Context, topic string, msg any) error

	// Subscribe registers deliver for topic and returns the unsubscribe.
	Subscribe(topic string, deliver func(msg any)) (func(), error)
}

// PresenceBroker is a Broker that also holds the presence roster, so
// [Base.Presence] lists members across every node sharing it. Optional and
// discovered by type assertion: a Broker without it keeps the roster
// in-process, which is exactly right for the in-memory broker.
//
// Shuttle calls Join and Leave only on a session's first join and last
// leave of a topic - the per-component refcounting stays local, since a
// session lives on one node. Implementations must deliver a join to the
// wire before shuttle's own PresenceEvent for it follows on the same
// topic, so a component re-rendering on the event reads a roster that
// already contains the member; publishing both on one ordered connection
// is the usual way.
//
// Members returns the merged roster, ordered by tag. No error: a
// distributed implementation answers from a local mirror, because this is
// read during renders and a render must not wait on a network round trip.
type PresenceBroker interface {
	Broker

	// Join records member on topic's roster.
	Join(ctx context.Context, topic string, member Member) error

	// Leave removes the member with tag from topic's roster.
	Leave(ctx context.Context, topic, tag string) error

	// Members returns topic's roster across every node, ordered by tag.
	Members(topic string) []Member
}

// memoryBroker is the single-process Broker.
type memoryBroker struct {
	mu     sync.RWMutex
	next   uint64
	topics map[string]map[uint64]func(any)
}

// NewMemoryBroker returns an in-process Broker. It is what a Handler uses
// unless given another.
func NewMemoryBroker() Broker {
	return &memoryBroker{topics: map[string]map[uint64]func(any){}}
}

func (b *memoryBroker) Subscribe(topic string, deliver func(msg any)) (func(), error) {
	b.mu.Lock()
	b.next++
	id := b.next
	subs, ok := b.topics[topic]
	if !ok {
		subs = map[uint64]func(any){}
		b.topics[topic] = subs
	}
	subs[id] = deliver
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if subs, ok := b.topics[topic]; ok {
				delete(subs, id)
				if len(subs) == 0 {
					delete(b.topics, topic)
				}
			}
		})
	}, nil
}

func (b *memoryBroker) Publish(_ context.Context, topic string, msg any) error {
	// Copy under the read lock and deliver outside it: a subscriber that
	// publishes in response would otherwise deadlock on its own broker.
	b.mu.RLock()
	subs := make([]func(any), 0, len(b.topics[topic]))
	for _, deliver := range b.topics[topic] {
		subs = append(subs, deliver)
	}
	b.mu.RUnlock()

	for _, deliver := range subs {
		deliver(msg)
	}
	return nil
}
