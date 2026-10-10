package auth

import (
	"sync"
	"time"
)

// touchEvery is how often a credential's last_used_at is actually written.
// The column answers "is this key still in use", which a 5-minute
// resolution serves; writing it on every request put one UPDATE per API
// call in line with page renders on the single writer connection.
const touchEvery = 5 * time.Minute

// touchGate remembers when each id was last touched and lets a write
// through once per touchEvery. Zero value ready; safe for concurrent use.
type touchGate struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

// allow reports whether id is due for a write at now, and records it.
func (g *touchGate) allow(id int64, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[int64]time.Time{}
	}
	if t, ok := g.last[id]; ok && now.Sub(t) < touchEvery {
		return false
	}
	g.last[id] = now
	return true
}
