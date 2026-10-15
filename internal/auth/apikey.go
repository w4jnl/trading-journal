package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// keyPrefixLen is how many leading characters of the full key we persist + show
// in the settings UI so a user can recognize a key without revealing it.
const keyPrefixLen = 10

// GenerateAPIKey mints a new key: a 256-bit random secret rendered as
// "tj_<base64url>". It returns the full plaintext (shown to the user exactly
// once and never stored), a short non-secret display prefix, and the sha256 hex
// that is persisted for lookup. Mirrors newSessionID's crypto/rand approach.
func GenerateAPIKey() (full, prefix, sha string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	full = "tj_" + base64.RawURLEncoding.EncodeToString(buf)
	prefix = full
	if len(prefix) > keyPrefixLen {
		prefix = prefix[:keyPrefixLen]
	}
	return full, prefix, HashAPIKey(full), nil
}

// HashAPIKey returns the hex sha256 of the full presented token. The whole
// token (including the "tj_" prefix) is hashed so verification hashes exactly
// what the client sends. sha256 (not bcrypt) is correct here: the key is
// high-entropy, and a UNIQUE index on the hash gives O(1) lookup.
func HashAPIKey(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

// parseBearer extracts the token from "Authorization: Bearer <tok>" (preferred)
// or the "X-API-Key" header. Query-string keys are intentionally not accepted
// (they leak into logs and referrers).
func parseBearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// APIKeyAuth resolves a bearer / X-API-Key token to a *User and injects it via
// WithUser, mirroring RequireUser but for the stateless API. On any failure it
// writes a JSON 401 and does not call next. It never touches the session or
// CSRF machinery, so the /api subtree is exempt from both.
type APIKeyAuth struct {
	Queries *models.Queries

	touched touchGate // last_used_at writes, once per touchEvery per key
}

// Middleware wraps the API mux. Every downstream handler can then call the same
// userID(ctx)/MustUser helpers the session app uses.
func (a *APIKeyAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, info, reason := a.authenticate(r, parseBearer(r))
		if u == nil {
			slog.Debug("api auth rejected", "reason", reason, "method", r.Method, "path", r.URL.Path)
			writeAPIError(w, http.StatusUnauthorized, reason)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithAPIKey(WithUser(r.Context(), u), info)))
	})
}

// authenticate resolves a tj_ key to its user and the key itself, or
// returns nil and the reason for the 401. Shared by Middleware and the
// OAuth-aware /mcp middleware so the key rules live in exactly one place.
func (a *APIKeyAuth) authenticate(r *http.Request, tok string) (*User, APIKeyInfo, string) {
	if tok == "" || !strings.HasPrefix(tok, "tj_") {
		return nil, APIKeyInfo{}, "missing or malformed API key"
	}
	row, err := a.Queries.GetAPIKeyBySHA256(r.Context(), HashAPIKey(tok))
	if err != nil {
		// sql.ErrNoRows (unknown OR revoked key) and any other error -> 401.
		return nil, APIKeyInfo{}, "invalid API key"
	}
	if row.IsActive == 0 {
		return nil, APIKeyInfo{}, "account disabled"
	}
	info := APIKeyInfo{ID: row.KeyID, Name: row.KeyName, Kind: normalizeKeyKind(row.KeyKind), CreatedAt: row.KeyCreatedAt}
	u := &User{
		ID:          row.UserID,
		Username:    row.Username,
		DisplayName: row.DisplayName, // COALESCEd to username in the query
		Email:       row.Email,       // COALESCEd to "" in the query
		IsAdmin:     row.IsAdmin != 0,
	}
	// Best-effort last-used bump, once per touchEvery per key (a client
	// polling every few seconds must not turn into a write per request).
	// Never blocks the request; its own context so a slow disk write can't
	// wedge it.
	if keyID := row.KeyID; a.touched.allow(keyID, time.Now()) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = a.Queries.TouchAPIKeyLastUsed(ctx, keyID)
		}()
	}
	return u, info, ""
}

// writeAPIError writes a JSON {"error": ...} body. It lives in auth (which can't
// import the handlers package - import cycle) but matches the handlers'
// writeJSONError shape so the whole API uses one error envelope.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
