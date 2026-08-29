package shuttle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Session lifetime defaults. Server-held state means a connected tab costs
// memory, so both the cap and the expiry are load-bearing rather than
// tuning knobs: without them a client that opens pages and never connects
// is a memory-exhaustion primitive.
const (
	// attachWindow is how long an unattached session waits for its page to
	// open a stream. A page that is loaded and closed, or crawled by a bot,
	// is collected after this.
	attachWindow = 30 * time.Second

	// maxSessions bounds the registry.
	maxSessions = 10_000
)

// registry owns session lifetime: creation, lookup, and eviction.
type registry struct {
	mu       sync.Mutex
	sessions map[string]*Session
	timers   map[string]*time.Timer
	// closed refuses new sessions once closeAll has run: a session created
	// after the sweep would be one nothing ever tears down.
	closed bool

	// grace, attach and max are fields rather than the constants directly
	// so tests can drive eviction without sleeping for half a minute.
	grace  time.Duration
	attach time.Duration
	max    int
}

func newRegistry() *registry {
	return &registry{
		sessions: map[string]*Session{},
		timers:   map[string]*time.Timer{},
		grace:    DefaultGrace,
		attach:   attachWindow,
		max:      maxSessions,
	}
}

// newID returns an unguessable session id. This is the client's capability
// for the whole page, so it comes from crypto/rand, not a counter.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tagFallback numbers tags when the entropy pool cannot be read, so that a
// tag is always unique even then.
var tagFallback atomic.Uint64

// newTag returns the short label a session is known by wherever naming it
// must not mean handing over the capability - logs, and the presence
// roster. It is deliberately not derived from newID's output: an id that
// never leaves this process in any form cannot leak in a derived one.
//
// Telling sessions apart is the whole requirement, so six bytes is plenty:
// at the registry's cap of 10,000 live sessions the chance of any collision
// is around one in five million.
//
// Uniqueness is load-bearing rather than cosmetic - presence keys its
// members by this - so an unreadable entropy pool falls back to a counter
// rather than to the empty string, which every session would then share.
func newTag() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "n" + strconv.FormatUint(tagFallback.Add(1), 10)
	}
	return hex.EncodeToString(b)
}

// create registers a new session for cmp and starts its attach timer.
func (r *registry) create(cmp Component, broker Broker, pres *presence, onError func(string, error)) (*Session, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrShutdown
	}
	if len(r.sessions) >= r.max {
		r.mu.Unlock()
		return nil, ErrTooManySessions
	}
	s := newSessionWith(id, cmp, broker, pres, onError)
	r.sessions[id] = s
	r.mu.Unlock()

	r.expireIn(id, r.attach)
	return s, nil
}

// ownedBy returns the ids of sessions labelled owner.
//
// The sessions are snapshotted under the registry's lock and asked for
// their owner afterwards, rather than while holding it: two locks held at
// once is a deadlock waiting for somebody to take them the other way round,
// and there is no reason to here.
func (r *registry) ownedBy(owner string) []string {
	r.mu.Lock()
	live := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		live = append(live, s)
	}
	r.mu.Unlock()

	var ids []string
	for _, s := range live {
		if s.Owner() == owner {
			ids = append(ids, s.ID())
		}
	}
	return ids
}

// get returns the session for id.
func (r *registry) get(id string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	return s, ok
}

// expireIn schedules eviction, replacing any pending timer.
func (r *registry) expireIn(id string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[id]; !ok {
		return
	}
	if t, ok := r.timers[id]; ok {
		t.Stop()
	}
	r.timers[id] = time.AfterFunc(d, func() { r.remove(id) })
}

// hold cancels a pending eviction, for the life of an attached stream.
func (r *registry) hold(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.timers[id]; ok {
		t.Stop()
		delete(r.timers, id)
	}
}

// remove evicts a session and unmounts its component. The unmount is begun
// rather than waited for - remove is what Handler.Close calls, and that is
// documented as safe from inside an action, where waiting on the session's
// own goroutine would be waiting on yourself.
func (r *registry) remove(id string) {
	r.mu.Lock()
	s, ok := r.sessions[id]
	delete(r.sessions, id)
	if t, ok := r.timers[id]; ok {
		t.Stop()
		delete(r.timers, id)
	}
	r.mu.Unlock()

	if ok {
		s.beginClose(context.Background())
	}
}

// closeAll evicts every session and waits for their teardowns, bounded by
// ctx. It marks the registry closed first, so nothing minted during the
// sweep can slip past it.
func (r *registry) closeAll(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	live := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		live = append(live, s)
	}
	r.sessions = map[string]*Session{}
	for id, t := range r.timers {
		t.Stop()
		delete(r.timers, id)
	}
	r.mu.Unlock()

	// Concurrently: each close waits behind whatever its session is doing,
	// and ten thousand of those in a row is a shutdown nobody waits out.
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, s := range live {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.close(ctx)
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// The teardowns keep running; only the waiting stops. A session
		// stuck in component code holds its goroutine either way.
		return ctx.Err()
	}
}

// len reports the number of live sessions.
func (r *registry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}
