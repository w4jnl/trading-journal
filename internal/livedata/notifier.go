// Package livedata is the in-process change signal behind the pages' live
// refresh: every mutation path publishes "user X's trading data changed"
// AFTER its transaction commits, and the web layer reads a per-user version
// number that open pages compare against. The notifier is deliberately
// transport-agnostic: the browser polls GET /data/version, the phone app
// holds an SSE stream (GET /api/v1/events) fed by Subscribe; publishers
// never know which.
package livedata

import "sync"

// Notifier is a per-user monotonic data-version counter plus a global
// generation for reference-data changes (commission rates) that affect every
// user's derived trades at once.
type Notifier struct {
	mu     sync.Mutex
	global uint64
	users  map[int64]uint64
	subs   map[int64]map[*subscriber]struct{}
}

// subscriber is one open stream: a one-slot channel, so a burst of bumps
// collapses into one wake-up (the reader refetches whole state anyway).
type subscriber struct {
	userID int64
	ch     chan struct{}
}

func NewNotifier() *Notifier {
	return &Notifier{users: map[int64]uint64{}}
}

// Subscribe returns a channel that receives a value after every bump that
// concerns userID (its own or a global one), and a cancel that must be
// called when the stream ends. Nil-safe: a nil notifier never signals.
func (n *Notifier) Subscribe(userID int64) (<-chan struct{}, func()) {
	if n == nil {
		return nil, func() {}
	}
	s := &subscriber{userID: userID, ch: make(chan struct{}, 1)}
	n.mu.Lock()
	if n.subs == nil {
		n.subs = map[int64]map[*subscriber]struct{}{}
	}
	if n.subs[userID] == nil {
		n.subs[userID] = map[*subscriber]struct{}{}
	}
	n.subs[userID][s] = struct{}{}
	n.mu.Unlock()
	return s.ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if m := n.subs[userID]; m != nil {
			delete(m, s)
			if len(m) == 0 {
				delete(n.subs, userID)
			}
		}
	}
}

// wake signals every subscriber of userID; never blocks (the one-slot
// channel either takes the value or already holds one).
func (n *Notifier) wake(userID int64) {
	for s := range n.subs[userID] {
		select {
		case s.ch <- struct{}{}:
		default:
		}
	}
}

// Bump records a committed data change for one user. Nil-safe: callers are
// wired with an optional dependency, and tests construct bare structs.
func (n *Notifier) Bump(userID int64) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.users == nil {
		n.users = map[int64]uint64{}
	}
	n.users[userID]++
	n.wake(userID)
}

// BumpAll records a change that touches every user (admin-scoped reference
// data such as commission rates, whose rebuilds span accounts).
func (n *Notifier) BumpAll() {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.global++
	for uid := range n.subs {
		n.wake(uid)
	}
}

// Version returns the user's current data version. The sum of two monotonic
// counters is monotonic, so one number is enough for clients to compare.
// Values are process-local and restart at zero - clients must treat ANY
// difference as "changed", never only increases.
func (n *Notifier) Version(userID int64) uint64 {
	if n == nil {
		return 0
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.global + n.users[userID]
}
