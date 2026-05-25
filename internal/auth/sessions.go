package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

const (
	sessionCookieName = "session"
	sessionIDBytes    = 32 // 256 bits → 64 hex chars
)

// SessionStore creates, looks up, and destroys sqlite-backed sessions. The
// cookie value is the session id (random 32 bytes hex); the row holds the
// owning user_id and expiry.
type SessionStore struct {
	DB      *sql.DB
	Queries *models.Queries
	TTL     time.Duration
	// Secure controls the Secure attribute on the session cookie. Default
	// true; tests / local-only HTTP deploys can flip this off.
	Secure bool
}

// newSessionID returns a fresh random session id encoded as hex.
func newSessionID() (string, error) {
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read entropy: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Create writes a new session row and returns its id. The caller is
// responsible for setting the cookie on the response.
func (s *SessionStore) Create(ctx context.Context, userID int64, ua, ip string) (string, time.Time, error) {
	id, err := newSessionID()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(s.TTL)
	if err := s.Queries.CreateSession(ctx, models.CreateSessionParams{
		ID:        id,
		UserID:    userID,
		ExpiresAt: expires,
		UserAgent: nilIfEmpty(ua),
		Ip:        nilIfEmpty(ip),
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("insert session: %w", err)
	}
	return id, expires, nil
}

// Lookup returns the user for the given session id, or nil if the session is
// missing / expired. Expired sessions are returned as nil; the janitor sweeps
// the row on its next pass.
func (s *SessionStore) Lookup(ctx context.Context, sessionID string) (*User, error) {
	if sessionID == "" {
		return nil, nil
	}
	row, err := s.Queries.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	if row.IsActive == 0 {
		return nil, nil
	}
	return &User{
		ID:          row.UserID,
		Username:    row.Username,
		DisplayName: row.DisplayName,
		Email:       row.Email,
		IsAdmin:     row.IsAdmin != 0,
	}, nil
}

// Destroy removes a session row. Safe to call with an unknown id.
func (s *SessionStore) Destroy(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return s.Queries.DeleteSession(ctx, sessionID)
}

// SetCookie writes the session cookie to w. expires is the cookie expiry
// (matches the row's expires_at so the browser drops the cookie when the row
// goes stale).
func (s *SessionStore) SetCookie(w http.ResponseWriter, id string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie removes the session cookie on logout. We send Max-Age = -1 so
// the browser drops it immediately rather than waiting for the prior Expires.
func (s *SessionStore) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// CookieFromRequest reads the session id from the request cookie, or "" if
// absent.
func CookieFromRequest(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// StartJanitor purges expired sessions every interval until ctx is canceled.
// Run as a goroutine from main.
func (s *SessionStore) StartJanitor(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.Queries.PurgeExpiredSessions(ctx)
			if err != nil {
				slog.Warn("session janitor", "err", err)
				continue
			}
			if n > 0 {
				slog.Debug("session janitor purged", "count", n)
			}
		}
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
