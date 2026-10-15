package auth

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// ---- API key kinds -------------------------------------------------------
//
// An API key is either an agent's (an AI tool or a script: it reads and
// appends, never overwrites the trader's own words) or a device's (the
// trader's phone, paired from Settings: it edits and deletes journal
// entries, changes settings and registers for push, as the trader). The
// middleware puts the key's kind and identity in the request context so a
// handler can refuse a write that needs the trader's own device.

// KeyKind is api_keys.kind.
type KeyKind string

const (
	KeyKindAgent  KeyKind = "agent"
	KeyKindDevice KeyKind = "device"
)

// APIKeyInfo identifies the key a request was authenticated with.
type APIKeyInfo struct {
	ID        int64
	Name      string
	Kind      KeyKind
	CreatedAt time.Time
}

type apiKeyCtxKey struct{}

// WithAPIKey stores the authenticating key in ctx.
func WithAPIKey(ctx context.Context, k APIKeyInfo) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, k)
}

// APIKeyFrom returns the authenticating key, ok false for a request that
// came through a session (the HTML app) or an OAuth token.
func APIKeyFrom(ctx context.Context) (APIKeyInfo, bool) {
	k, ok := ctx.Value(apiKeyCtxKey{}).(APIKeyInfo)
	return k, ok
}

// IsDeviceKey reports a request authenticated with the trader's own device
// key.
func IsDeviceKey(ctx context.Context) bool {
	k, ok := APIKeyFrom(ctx)
	return ok && k.Kind == KeyKindDevice
}

// normalizeKeyKind reads a stored kind: rows from before the column (and
// tests that leave it empty) are agent keys.
func normalizeKeyKind(s string) KeyKind {
	if strings.EqualFold(s, string(KeyKindDevice)) {
		return KeyKindDevice
	}
	return KeyKindAgent
}

// ---- Attempt limiting for unauthenticated endpoints -------------------------

// AttemptLimiter is the login throttle for other guessable endpoints (the
// pairing code exchange): a per-key lockout after a few consecutive
// failures that doubles on each further failure.
type AttemptLimiter struct{ l *loginLimiter }

// NewAttemptLimiter returns an empty limiter.
func NewAttemptLimiter() *AttemptLimiter { return &AttemptLimiter{l: newLoginLimiter()} }

// RetryAfter is how long key stays locked; 0 when it may try. Nil-safe.
func (a *AttemptLimiter) RetryAfter(key string) time.Duration {
	if a == nil {
		return 0
	}
	return a.l.retryAfter(key)
}

// Fail records a failed attempt under key. Nil-safe.
func (a *AttemptLimiter) Fail(key string) {
	if a != nil {
		a.l.fail(key)
	}
}

// Reset forgets key after a success. Nil-safe.
func (a *AttemptLimiter) Reset(key string) {
	if a != nil {
		a.l.reset(key)
	}
}

// ClientIP is the request's address without the port, for throttling.
func ClientIP(r *http.Request) string { return clientIP(r) }
