package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

const actAsCookieName = "act_as"

// actAsTTL bounds how long a single act-as grant is valid. The expiry is baked
// into the signed token (not just the cookie Expires attribute) so a captured
// cookie value can't be replayed indefinitely until the signing secret rotates.
const actAsTTL = 24 * time.Hour

// ActAs lets an admin temporarily browse the app as another user. The
// admin's underlying session stays intact; a separate signed cookie
// carries the target user id. Every request consults this cookie AFTER
// the authenticator resolves the underlying user — if that user is an
// active admin and the cookie verifies, the resolved User is swapped to
// the target. The original admin identity is stashed in the request
// context (via OriginalUserFrom) so templates can render the "Acting as
// X" banner.
type ActAs struct {
	Queries *models.Queries
	// Secret signs the cookie payload. Reuse cfg.Auth.SessionSecret;
	// nil disables act-as entirely (Apply becomes a no-op).
	Secret []byte
	// Secure mirrors the session cookie's Secure flag.
	Secure bool
}

type origUserKey struct{}

// OriginalUserFrom returns the underlying admin when an act-as override
// is active; nil otherwise. Templates use this to render the banner.
func OriginalUserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(origUserKey{}).(*User)
	return u
}

// Apply consults the act-as cookie and, when valid, swaps the request's
// resolved user. Called from RequireUser AFTER the authenticator has
// produced a candidate User. Returns the (possibly-swapped) User and a
// context to use downstream.
//
// Rules:
//   - cookie absent / unsigned / expired → return original user as-is
//   - underlying user is not an active admin → ignore the cookie
//   - target user missing or inactive → ignore the cookie
//
// The swap never escalates privileges (target's IsAdmin/IsActive come
// from the DB, not from the cookie payload).
func (a *ActAs) Apply(ctx context.Context, r *http.Request, u *User) (*User, context.Context) {
	if a == nil || a.Secret == nil || u == nil {
		return u, ctx
	}
	if !u.IsAdmin {
		return u, ctx
	}
	c, err := r.Cookie(actAsCookieName)
	if err != nil || c.Value == "" {
		return u, ctx
	}
	targetID, ok := verifyActAsToken(c.Value, a.Secret)
	if !ok {
		return u, ctx
	}
	if targetID == u.ID {
		return u, ctx
	}
	target, err := a.Queries.GetUserByID(ctx, targetID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("act-as lookup failed", "target", targetID, "err", err)
		}
		return u, ctx
	}
	if target.IsActive == 0 {
		return u, ctx
	}
	swapped := &User{
		ID:          target.ID,
		Username:    target.Username,
		DisplayName: ptrOrSelf(target.DisplayName, target.Username),
		Email:       ptrOrEmpty(target.Email),
		IsAdmin:     target.IsAdmin != 0,
	}
	// Stash the admin under origUserKey so the template can render the
	// banner. Caller threads ctx onward.
	return swapped, context.WithValue(ctx, origUserKey{}, u)
}

// SetCookie writes the act-as cookie for the given target user id.
func (a *ActAs) SetCookie(w http.ResponseWriter, targetID int64) {
	if a == nil || a.Secret == nil {
		return
	}
	exp := time.Now().Add(actAsTTL)
	token := signActAsToken(targetID, exp, a.Secret)
	http.SetCookie(w, &http.Cookie{
		Name:     actAsCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.Secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	})
}

// ClearCookie removes the act-as cookie (user clicked "return").
func (a *ActAs) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     actAsCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a != nil && a.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// signActAsToken returns base64(userID|expUnix|hmac). The HMAC covers
// "userID|expUnix" so neither the target nor the expiry can be tampered with.
func signActAsToken(userID int64, exp time.Time, secret []byte) string {
	payload := strconv.FormatInt(userID, 10) + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	raw := payload + "|" + base64.RawURLEncoding.EncodeToString(sig)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// verifyActAsToken parses + verifies the token; returns (id, true) on success,
// (0, false) on any mismatch, malformed input, or expiry.
func verifyActAsToken(tok string, secret []byte) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, false
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	expUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	// Verify the signature over "userID|expUnix" before trusting the expiry.
	want := hmac.New(sha256.New, secret)
	want.Write([]byte(parts[0] + "|" + parts[1]))
	wantB64 := base64.RawURLEncoding.EncodeToString(want.Sum(nil))
	if !hmac.Equal([]byte(wantB64), []byte(parts[2])) {
		return 0, false
	}
	if time.Now().Unix() > expUnix {
		return 0, false
	}
	return id, true
}

func ptrOrSelf(p *string, fallback string) string {
	if p != nil && *p != "" {
		return *p
	}
	return fallback
}
func ptrOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// guard against unused-import noise on this small file
var _ = fmt.Sprintf
