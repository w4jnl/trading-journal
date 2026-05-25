// Package auth wires authentication and request-scoped user identity for the
// trading journal. v1 of multi-user support ships with a "stub" authenticator
// that always resolves the configured bootstrap user; real local-password and
// reverse-proxy modes layer on top of the same Authenticator interface in
// later phases.
package auth

import (
	"context"
	"net/http"
)

// User is the in-request identity. It carries only the fields handlers care
// about; the persistent row is in the `users` table.
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Email       string
	IsAdmin     bool
}

type Authenticator interface {
	// ResolveUser inspects the request and returns the authenticated user,
	// or nil if the request is unauthenticated. It must not write to the
	// response; the middleware handles redirect/401 behavior.
	ResolveUser(r *http.Request) (*User, error)
	// Routes registers auth-mode-specific endpoints (login/logout in local
	// mode; nothing in proxy mode).
	Routes(mux *http.ServeMux)
	// HandleUnauthenticated decides what happens when ResolveUser returns
	// nil (redirect to /login vs. 401).
	HandleUnauthenticated(w http.ResponseWriter, r *http.Request)
}

type ctxKey struct{}

// UserFrom returns the request-scoped user, or nil if the middleware did not
// run for this request.
func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// MustUser returns the request-scoped user or panics. Use in handlers behind
// RequireUser middleware where a nil here indicates a wiring bug.
func MustUser(ctx context.Context) *User {
	u := UserFrom(ctx)
	if u == nil {
		panic("auth.MustUser: no user in context (middleware not wired?)")
	}
	return u
}

// WithUser returns a child context carrying u. Exported so handlers and tests
// can construct a scoped context directly.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}
