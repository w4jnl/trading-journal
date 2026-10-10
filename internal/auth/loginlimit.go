package auth

import (
	"sync"
	"time"
)

// Local-login throttle. bcrypt alone bounds a password-guessing loop to a
// few tens of attempts per second per core, which is not a limit at all
// over a weekend; this adds a per-(username, client IP) lockout that starts
// after a handful of consecutive failures and doubles on each further
// failure. Production runs behind Authelia in proxy mode where /login is
// not used, but auth.mode=local is the default and the dev/test path.
const (
	loginFreeFailures = 5                // consecutive failures before the first lock
	loginBaseLock     = 30 * time.Second // first lock; doubles per further failure
	loginMaxLock      = 15 * time.Minute
	loginForgetAfter  = time.Hour // idle entries are dropped on the next prune
	loginPruneAt      = 1024      // prune when the map grows past this
)

type loginAttempts struct {
	failures    int
	lockedUntil time.Time
	last        time.Time
}

type loginLimiter struct {
	mu  sync.Mutex
	m   map[string]*loginAttempts
	now func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{m: map[string]*loginAttempts{}, now: time.Now}
}

// retryAfter returns how long key stays locked (0 = not locked).
func (l *loginLimiter) retryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[key]
	if a == nil {
		return 0
	}
	if wait := a.lockedUntil.Sub(l.now()); wait > 0 {
		return wait
	}
	return 0
}

// fail records a failed attempt and, past the free allowance, locks the key
// for loginBaseLock doubled per extra failure, capped at loginMaxLock.
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.m) > loginPruneAt {
		for k, a := range l.m {
			if now.Sub(a.last) > loginForgetAfter && !a.lockedUntil.After(now) {
				delete(l.m, k)
			}
		}
	}
	a := l.m[key]
	if a == nil {
		a = &loginAttempts{}
		l.m[key] = a
	}
	a.last = now
	a.failures++
	if extra := a.failures - loginFreeFailures; extra >= 0 {
		lock := loginBaseLock << uint(min(extra, 10))
		if lock > loginMaxLock {
			lock = loginMaxLock
		}
		a.lockedUntil = now.Add(lock)
	}
}

// reset clears the key after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}
