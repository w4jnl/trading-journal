// Package livedata is the in-process change signal behind the pages' live
// refresh: every mutation path publishes "user X's trading data changed"
// AFTER its transaction commits, and the web layer reads a per-user version
// number that open pages compare against. The notifier is deliberately
// transport-agnostic - today the browser polls GET /data/version; a future
// SSE upgrade would add channel fan-out here without touching any publisher.
package livedata

import "sync"

// Notifier is a per-user monotonic data-version counter plus a global
// generation for reference-data changes (commission rates) that affect every
// user's derived trades at once.
type Notifier struct {
	mu     sync.Mutex
	global uint64
	users  map[int64]uint64
}

func NewNotifier() *Notifier {
	return &Notifier{users: map[int64]uint64{}}
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
