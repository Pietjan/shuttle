// Package nats is a [shuttle.Broker] over NATS, for a deployment that runs
// more than one node: a Publish on any node reaches subscribers on all of
// them, and nothing above the Broker interface changes.
//
// It implements the contract documented on [shuttle.Broker], not the
// accidental extras the in-memory broker has: delivery is at-most-once,
// ordering is per publisher per topic (NATS's own guarantee), and messages
// are values that cross a process boundary. Every node's delivery goes
// through the wire - a publisher's own subscribers included - so one node
// behaves exactly like five, and a message that cannot survive
// serialization fails at Publish on the developer's machine rather than in
// production's second region.
//
// # Message types must be registered
//
// Messages travel as gob, which needs the concrete type known on both
// sides to hand HandleInfo the same type the publisher sent - a
// type-switch on a component must keep working across nodes. Register
// every type an application publishes, once, at startup:
//
//	natsbroker.Register(RoomMessage{})
//
// [shuttle.PresenceEvent] is registered already, since shuttle publishes
// it itself. Publishing an unregistered type fails loudly at Publish;
// receiving one (a node running older code) is reported to the handler and
// dropped.
//
// # What this does not distribute
//
// Presence. The roster is in-process per node even with a shared broker:
// join and leave events cross nodes, but [shuttle.Base.Presence] lists
// only the members this node has seen join since it subscribed. A shared
// roster is a later, separate piece.
package nats

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/pietjan/shuttle"
)

func init() {
	// Shuttle publishes these itself, so they are always registered.
	gob.Register(shuttle.PresenceEvent{})
	gob.Register(shuttle.Member{})
}

// Register makes a message type encodable, exactly like [gob.Register]:
// call it once per concrete type the application publishes, from an init
// or early in main, identically on every node.
func Register(value any) { gob.Register(value) }

// Option configures a Broker.
type Option func(*Broker)

// Prefix sets the subject prefix topics are published under, for one NATS
// cluster carrying more than one application. The default is "shuttle".
func Prefix(p string) Option { return func(b *Broker) { b.prefix = p } }

// Logger reports messages that arrive but cannot be decoded - a node
// running older code than its publisher. Defaults to slog.Default().
func Logger(l *slog.Logger) Option { return func(b *Broker) { b.log = l } }

// Broker is a [shuttle.Broker] over a NATS connection.
type Broker struct {
	nc     *nats.Conn
	prefix string
	log    *slog.Logger
}

// New returns a Broker over nc. The connection is the application's: it
// owns connecting, reconnect options and closing, and one connection can
// back many handlers.
//
//	nc, _ := nats.Connect(natsURL)
//	handler.Broker = natsbroker.New(nc)
func New(nc *nats.Conn, opts ...Option) *Broker {
	b := &Broker{nc: nc, prefix: "shuttle", log: slog.Default()}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// envelope carries the message as a gob interface value, which is what
// brings the concrete type back out on the far side.
type envelope struct {
	Msg any
}

// subject maps a topic onto a NATS subject. Topics are arbitrary strings
// and subjects are not - dots are hierarchy, spaces and wildcards are
// syntax - so the topic travels base64url-encoded rather than sanitized:
// sanitizing collides distinct topics, and a collision here is one page's
// messages delivered to another's component.
func (b *Broker) subject(topic string) string {
	return b.prefix + "." + base64.RawURLEncoding.EncodeToString([]byte(topic))
}

// Publish sends msg to every subscriber on topic, on every node sharing
// the cluster. It buffers rather than waits - NATS writes are async - so
// it keeps the Broker contract of never blocking on delivery.
func (b *Broker) Publish(_ context.Context, topic string, msg any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(envelope{Msg: msg}); err != nil {
		// Almost always an unregistered type. Failing here is the point:
		// the same Publish on the in-memory broker would have worked, and
		// this is the developer's machine finding out instead of the
		// deployment.
		return fmt.Errorf("shuttle/nats: encoding %T (is it Registered?): %w", msg, err)
	}
	return b.nc.Publish(b.subject(topic), buf.Bytes())
}

// Subscribe registers deliver for topic and returns the unsubscribe.
// Delivery happens on the NATS client's goroutines; shuttle's subscribers
// already hand messages to their own sessions, which is the Broker
// contract's other half.
func (b *Broker) Subscribe(topic string, deliver func(msg any)) (func(), error) {
	sub, err := b.nc.Subscribe(b.subject(topic), func(m *nats.Msg) {
		var env envelope
		if err := gob.NewDecoder(bytes.NewReader(m.Data)).Decode(&env); err != nil {
			// A publisher we cannot understand - usually a newer node
			// publishing a type this one does not Register. Dropped and
			// said so; delivering a half-decoded value would be worse.
			b.log.Error("shuttle/nats: dropping undecodable message",
				"topic", topic, "err", err)
			return
		}
		deliver(env.Msg)
	})
	if err != nil {
		return nil, fmt.Errorf("shuttle/nats: subscribe: %w", err)
	}
	// Flushed, so the subscription is active on the server before this
	// returns. NATS buffers SUB like everything else, and shuttle's Join
	// deliberately subscribes before announcing so a joiner hears its own
	// arrival - without the round trip, the announcement can outrun the
	// subscription and that promise quietly breaks. Subscribing happens at
	// Mount, never on a hot path, so the round trip is cheap where it is
	// paid.
	if err := b.nc.Flush(); err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("shuttle/nats: subscribe flush: %w", err)
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
