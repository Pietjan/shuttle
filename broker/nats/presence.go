package nats

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
	"github.com/pietjan/shuttle"
)

// The roster is a gossip mirror, not shared storage, because of where it
// is read: Base.Presence runs during renders, and a render must never wait
// on a network round trip. Every node keeps a full copy and answers
// Members from it instantly. The copy is kept honest three ways:
//
//   - a delta travels the moment a member joins or leaves, on the same
//     connection as everything else - and NATS preserves per-connection
//     order, which is what guarantees the delta lands before the
//     PresenceEvent shuttle publishes right after it, so a component
//     re-rendering on the event reads a roster that already holds the
//     member;
//   - a heartbeat carries each node's full state every interval, which is
//     both the repair for any missed delta and the liveness signal;
//   - a node starting up asks, and everyone answers with their state, so
//     a fresh node's mirror is complete after one round trip instead of
//     one heartbeat interval.
//
// A node that dies takes its members with it: nothing is stored anywhere,
// so when its heartbeats stop its entries age out of every mirror after
// presenceTTL. That is the self-cleaning the roster needs anyway - a
// crashed node's sessions are gone, and rows for them would be ghosts.

// DefaultPresenceInterval is how often a node gossips its full presence
// state when the Broker is not configured otherwise.
const DefaultPresenceInterval = 5 * time.Second

// PresenceInterval sets how often this node gossips its state, and with it
// how quickly a crashed node's members age out of the roster (at 3.5
// intervals). The default is DefaultPresenceInterval; every node should
// use the same value.
func PresenceInterval(d time.Duration) Option {
	return func(b *Broker) { b.interval = d }
}

// delta is one membership change, gossiped as it happens.
type delta struct {
	Node   string
	Topic  string
	Member shuttle.Member
	Joined bool
}

// state is one node's whole roster, gossiped every interval and on demand.
type state struct {
	Node    string
	Entries []entry
}

type entry struct {
	Topic  string
	Member shuttle.Member
}

// sync is a fresh node asking everyone for their state.
type syncRequest struct {
	Node string
}

// mirror is what this node believes the cluster's roster to be.
type mirror struct {
	mu    sync.Mutex
	nodes map[string]*nodeRoster
}

type nodeRoster struct {
	seen   time.Time
	topics map[string]map[string]shuttle.Member // topic -> tag -> member
}

func (m *mirror) node(id string) *nodeRoster {
	n, ok := m.nodes[id]
	if !ok {
		n = &nodeRoster{topics: map[string]map[string]shuttle.Member{}}
		m.nodes[id] = n
	}
	n.seen = time.Now()
	return n
}

func (m *mirror) apply(d delta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.node(d.Node)
	if d.Joined {
		members, ok := n.topics[d.Topic]
		if !ok {
			members = map[string]shuttle.Member{}
			n.topics[d.Topic] = members
		}
		members[d.Member.Tag] = d.Member
		return
	}
	if members, ok := n.topics[d.Topic]; ok {
		delete(members, d.Member.Tag)
		if len(members) == 0 {
			delete(n.topics, d.Topic)
		}
	}
}

// replace swaps in a node's authoritative state - the heartbeat repairing
// whatever deltas were missed.
func (m *mirror) replace(s state) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.node(s.Node)
	n.topics = map[string]map[string]shuttle.Member{}
	for _, e := range s.Entries {
		members, ok := n.topics[e.Topic]
		if !ok {
			members = map[string]shuttle.Member{}
			n.topics[e.Topic] = members
		}
		members[e.Member.Tag] = e.Member
	}
}

// members merges every live node's roster for one topic. self never ages
// out: this node's own entries are as alive as it is.
func (m *mirror) members(topic, self string, ttl time.Duration) []shuttle.Member {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-ttl)
	byTag := map[string]shuttle.Member{}
	for id, n := range m.nodes {
		if id != self && n.seen.Before(cutoff) {
			// Its heartbeats stopped: the node is gone and its sessions
			// with it. Dropped here rather than by a janitor - reads are
			// where staleness matters.
			delete(m.nodes, id)
			continue
		}
		for tag, member := range n.topics[topic] {
			byTag[tag] = member
		}
	}

	out := make([]shuttle.Member, 0, len(byTag))
	for _, member := range byTag {
		out = append(out, member)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// snapshot is this node's own entries, for a heartbeat or a sync answer.
func (m *mirror) snapshot(self string) state {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := state{Node: self}
	if n, ok := m.nodes[self]; ok {
		for topic, members := range n.topics {
			for _, member := range members {
				s.Entries = append(s.Entries, entry{Topic: topic, Member: member})
			}
		}
	}
	return s
}

// ensurePresence sets the gossip up on first use, so a Broker used only
// for messages pays none of it.
func (b *Broker) ensurePresence() error {
	b.presenceOnce.Do(func() { b.presenceErr = b.startPresence() })
	return b.presenceErr
}

func (b *Broker) startPresence() error {
	b.roster = &mirror{nodes: map[string]*nodeRoster{}}
	b.stop = make(chan struct{})

	subscribe := func(kind string, handle func([]byte)) error {
		sub, err := b.nc.Subscribe(b.prefix+".presence."+kind, func(m *nats.Msg) { handle(m.Data) })
		if err != nil {
			return fmt.Errorf("shuttle/nats: presence %s: %w", kind, err)
		}
		b.presenceSubs = append(b.presenceSubs, sub)
		return nil
	}

	if err := subscribe("delta", func(data []byte) {
		var d delta
		if decode(data, &d) == nil {
			b.roster.apply(d)
		}
	}); err != nil {
		return err
	}
	if err := subscribe("state", func(data []byte) {
		var s state
		if decode(data, &s) == nil && s.Node != b.node {
			b.roster.replace(s)
		}
	}); err != nil {
		return err
	}
	if err := subscribe("sync", func(data []byte) {
		var req syncRequest
		if decode(data, &req) == nil && req.Node != b.node {
			// A fresh node is asking; answer with our state now rather than
			// leaving its roster incomplete until the next heartbeat.
			b.publishState()
		}
	}); err != nil {
		return err
	}
	// Active before the sync request goes out, or an answer could be missed.
	if err := b.nc.Flush(); err != nil {
		return fmt.Errorf("shuttle/nats: presence flush: %w", err)
	}
	if err := b.publish("sync", syncRequest{Node: b.node}); err != nil {
		return err
	}

	go func() {
		t := time.NewTicker(b.interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				b.publishState()
			case <-b.stop:
				return
			}
		}
	}()
	return nil
}

func (b *Broker) publishState() {
	if err := b.publish("state", b.roster.snapshot(b.node)); err != nil {
		b.log.Error("shuttle/nats: presence state", "err", err)
	}
}

func (b *Broker) publish(kind string, v any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return fmt.Errorf("shuttle/nats: encoding presence %s: %w", kind, err)
	}
	return b.nc.Publish(b.prefix+".presence."+kind, buf.Bytes())
}

func decode(data []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(v)
}

// Join records member on topic's roster, cluster-wide. Part of
// [shuttle.PresenceBroker]; shuttle calls it on a session's first join.
func (b *Broker) Join(_ context.Context, topic string, member shuttle.Member) error {
	if err := b.ensurePresence(); err != nil {
		return err
	}
	d := delta{Node: b.node, Topic: topic, Member: member, Joined: true}
	// Applied locally first, so a Presence read on this node - the joiner
	// reading its own roster in Mount - sees the member synchronously, the
	// way the in-memory roster would.
	b.roster.apply(d)
	return b.publish("delta", d)
}

// Leave removes the member with tag from topic's roster, cluster-wide.
// Part of [shuttle.PresenceBroker].
func (b *Broker) Leave(_ context.Context, topic, tag string) error {
	if err := b.ensurePresence(); err != nil {
		return err
	}
	d := delta{Node: b.node, Topic: topic, Member: shuttle.Member{Tag: tag}, Joined: false}
	b.roster.apply(d)
	return b.publish("delta", d)
}

// Members returns topic's roster across every live node, ordered by tag.
// Part of [shuttle.PresenceBroker]. It answers from this node's mirror -
// no round trip - which is what lets renders read it freely.
func (b *Broker) Members(topic string) []shuttle.Member {
	if err := b.ensurePresence(); err != nil {
		b.log.Error("shuttle/nats: presence", "err", err)
		return nil
	}
	return b.roster.members(topic, b.node, b.ttl())
}

func (b *Broker) ttl() time.Duration {
	// Three missed heartbeats and half a one of slack: one lost message
	// must not flap a live node's members out of the roster.
	return b.interval*3 + b.interval/2
}

// Close stops the presence gossip - the heartbeat goroutine and its
// subscriptions. Call it before closing the connection on shutdown; a
// Broker only ever used for messages has nothing to stop, and Close is
// then a no-op. The connection itself stays the application's to close.
func (b *Broker) Close() {
	b.closeOnce.Do(func() {
		if b.stop != nil {
			close(b.stop)
		}
		for _, sub := range b.presenceSubs {
			_ = sub.Unsubscribe()
		}
	})
}

// nodeID names this broker instance in the gossip.
func nodeID() string { return nuid.Next() }
