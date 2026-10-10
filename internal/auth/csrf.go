package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
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

// csrfExemptPaths bypass the CSRF check unconditionally. /login and /logout
// are the only mutating endpoints reachable before an authenticated session
// exists; they handle session fixation themselves (login regenerates the
// session id, logout destroys it). A stale cookie from a prior session must
// not be allowed to gate a fresh login.
var csrfExemptPaths = map[string]bool{
	"/login":  true,
	"/logout": true,
}

// Middleware returns an http.Handler that enforces CSRF on mutating requests.
// GET/HEAD/OPTIONS pass through; other methods must carry a matching token in
// the "csrf_token" form field (or X-CSRF-Token header for HTMX requests).
// /login and /logout are exempt (see csrfExemptPaths). Unauthenticated
// requests with no session cookie also skip the check - they can't have a
// token yet and the auth middleware will reject them anyway.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.PublicMethods[r.Method] {
			next.ServeHTTP(w, r)
			return
		}
		if csrfExemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		sid := CookieFromRequest(r)
		if sid == "" {
			// No session cookie. Either an unauthenticated call the auth
			// middleware rejects anyway, or proxy mode (Authelia), which
			// never issues one - yet the browser still holds the SSO cookie
			// and forward-auth does no origin checking, so a cross-site form
			// could POST here with full identity. Fall back to the browser's
			// own evidence: Sec-Fetch-Site, else Origin, else Referer must be
			// same-origin. A request carrying none of them is not a browser
			// form post (curl, scripts) and passes.
			if !sameOriginRequest(r) {
				slog.Warn("csrf rejected: cross-site request without session", "path", r.URL.Path, "method", r.Method,
					"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"), "origin", r.Header.Get("Origin"))
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
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

// sameOriginRequest reports whether the browser says this request came from
// our own origin. Sec-Fetch-Site is authoritative when present ("same-origin"
// or "none" for a typed URL / bookmark; "same-site" - a sibling subdomain
// behind the same SSO - is rejected on purpose). Without it, Origin and then
// Referer are compared against the request host (also the forwarded host
// behind a reverse proxy). "Origin: null" is cross-site by definition.
func sameOriginRequest(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	if o := r.Header.Get("Origin"); o != "" {
		if o == "null" {
			return false
		}
		return originHostMatches(o, r)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return originHostMatches(ref, r)
	}
	return true
}

func originHostMatches(raw string, r *http.Request) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	for _, h := range []string{r.Host, r.Header.Get("X-Forwarded-Host")} {
		if h != "" && strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

// SafeLocalPath reports whether s is a same-origin path safe to redirect to
// or link back to: a single leading "/", no scheme or host once parsed, and
// no backslash or control character - browsers read "/\evil.com" as
// "//evil.com", which http.Redirect would emit verbatim.
func SafeLocalPath(s string) bool {
	if s == "" || s[0] != '/' || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\\r\n\x00") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return false
	}
	return true
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
