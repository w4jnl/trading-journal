package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

// CSRF holds the HMAC key. Tokens are HMAC-SHA256(secret, sessionID); they
// stay constant for a session and so can be embedded in forms without a
// per-request rotation. On logout / new session, the token rotates because
// the session id changes.
type CSRF struct {
	Secret []byte
	// PublicMethods are HTTP methods that bypass CSRF (GET/HEAD/OPTIONS).
	// Default {"GET", "HEAD", "OPTIONS"}.
	PublicMethods map[string]bool
}

// NewCSRF returns a CSRF middleware with the default safe-method allowlist.
func NewCSRF(secret []byte) *CSRF {
	return &CSRF{
		Secret:        secret,
		PublicMethods: map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true},
	}
}

// Token returns the HMAC for this session id. Empty session id returns "".
func (c *CSRF) Token(sessionID string) string {
	if sessionID == "" || len(c.Secret) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, c.Secret)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify returns true if got matches the canonical token for sessionID.
func (c *CSRF) Verify(sessionID, got string) bool {
	want := c.Token(sessionID)
	if want == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(want), []byte(got))
}

// Middleware returns an http.Handler that enforces CSRF on mutating requests.
// GET/HEAD/OPTIONS pass through; other methods must carry a matching token in
// the "csrf_token" form field (or X-CSRF-Token header for HTMX requests).
// Unauthenticated requests (no session cookie) skip the check too - they
// can't carry the token until they log in, and the login form itself has
// nothing to forge.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.PublicMethods[r.Method] {
			next.ServeHTTP(w, r)
			return
		}
		sid := CookieFromRequest(r)
		if sid == "" {
			// No session cookie - either the login POST (which we want to
			// allow) or an unauthenticated mutating call that the auth
			// middleware will reject anyway.
			next.ServeHTTP(w, r)
			return
		}
		// Pull the token from the form first, then the header (HTMX path).
		got := r.Header.Get("X-CSRF-Token")
		if got == "" {
			// ParseForm is idempotent and cheap; safe to call here even if
			// the downstream handler also calls it. POST body is bounded
			// by the http.Server ReadTimeout already.
			_ = r.ParseForm()
			got = r.PostFormValue("csrf_token")
		}
		if !c.Verify(sid, got) {
			slog.Warn("csrf rejected", "path", r.URL.Path, "method", r.Method)
			http.Error(w, "csrf token mismatch", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SecretFromString turns a config string into the HMAC key. Accepts raw
// strings (UTF-8 bytes used directly) and base64-encoded strings (prefix
// "base64:"). Empty input returns nil, signaling "no CSRF".
func SecretFromString(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "base64:") {
		return base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "base64:"))
	}
	return []byte(s), nil
}
