package shuttle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
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

// create registers a new session for cmp and starts its attach timer.
func (r *registry) create(cmp Component, broker Broker, pres *presence, onError func(string, error)) (*Session, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
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

// remove evicts a session and unmounts its component.
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
		s.close(context.Background())
	}
}

// len reports the number of live sessions.
func (r *registry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}
