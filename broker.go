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
type Broker interface {
	// Publish sends msg to everything subscribed to topic.
	Publish(ctx context.Context, topic string, msg any) error

	// Subscribe registers deliver for topic and returns the unsubscribe.
	Subscribe(topic string, deliver func(msg any)) (func(), error)
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
