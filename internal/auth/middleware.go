package auth

import (
	"log/slog"
	"net/http"
	"strings"
)

// PublicPrefixes are URL paths that skip authentication.
var publicPrefixes = []string{
	"/static/",
	"/healthz",
	"/login",
	"/logout",
}

func isPublic(path string) bool {
	for _, p := range publicPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return true
		}
	}
	return false
}

// RequireUser wraps next with an auth check. Requests to public paths pass
// straight through; everything else must resolve to a user, otherwise the
// authenticator's HandleUnauthenticated decides the response shape.
//
// When act is non-nil and the resolved user is an active admin carrying a
// valid act_as cookie, the request's user is swapped to the target and
// the original admin is stashed under origUserKey for the banner.
func RequireUser(a Authenticator, act *ActAs, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		u, err := a.ResolveUser(r)
		if err != nil {
			slog.Warn("auth resolve failed", "path", r.URL.Path, "err", err)
			http.Error(w, "authentication error", http.StatusInternalServerError)
			return
		}
		if u == nil {
			a.HandleUnauthenticated(w, r)
			return
		}
		ctx := r.Context()
		u, ctx = act.Apply(ctx, r, u)
		next.ServeHTTP(w, r.WithContext(WithUser(ctx, u)))
	})
}

// RequireAdmin layers on top of RequireUser and returns 403 to non-admins.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil || !u.IsAdmin {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
