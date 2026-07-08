package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/w4jnl/trading-journal/internal/models"
)

// LocalAuth implements Authenticator using a username + bcrypt password
// stored in the users table, with sqlite-backed sessions identifying the
// authenticated user across requests.
type LocalAuth struct {
	DB       *sql.DB
	Queries  *models.Queries
	Sessions *SessionStore
	// LoginRedirect is the path to send unauthenticated browsers to. Default
	// "/login". Tests can override.
	LoginRedirect string
	// LoginTemplate renders the login page. Provided by main so the existing
	// renderPage / template machinery is reused. Receives template data
	// like {"Error": "...", "Username": "alice"}.
	LoginTemplate func(w http.ResponseWriter, data map[string]any)
}

// ResolveUser reads the session cookie and returns the user, or nil if the
// caller is unauthenticated.
func (a *LocalAuth) ResolveUser(r *http.Request) (*User, error) {
	id := CookieFromRequest(r)
	if id == "" {
		return nil, nil
	}
	return a.Sessions.Lookup(r.Context(), id)
}

// HandleUnauthenticated redirects browsers to /login with ?next=<current>
// so the user lands back where they were trying to go.
func (a *LocalAuth) HandleUnauthenticated(w http.ResponseWriter, r *http.Request) {
	target := a.LoginRedirect
	if target == "" {
		target = "/login"
	}
	next := r.URL.RequestURI()
	if next != "" && next != "/" {
		target += "?next=" + safeNext(next)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// Routes registers /login (GET + POST) and /logout (POST + GET).
func (a *LocalAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.GetLogin)
	mux.HandleFunc("POST /login", a.PostLogin)
	mux.HandleFunc("POST /logout", a.PostLogout)
	// GET /logout: convenience for "click logout link" flows. POST is the
	// canonical CSRF-protected version.
	mux.HandleFunc("GET /logout", a.PostLogout)
}

// GetLogin renders the login form. If ?next=<safe> is set, the form
// preserves it so the POST handler can redirect there.
func (a *LocalAuth) GetLogin(w http.ResponseWriter, r *http.Request) {
	// If already authenticated, bounce to home rather than re-show the form.
	if u, _ := a.ResolveUser(r); u != nil {
		http.Redirect(w, r, nextOr("/", r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	a.renderLogin(w, "", "", r.URL.Query().Get("next"))
}

// PostLogin verifies username + password, opens a session, sets the cookie,
// and redirects.
func (a *LocalAuth) PostLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")

	user, err := a.verify(r.Context(), username, password)
	if err != nil {
		// Don't reveal whether it was username or password that failed —
		// constant-time-ish equivalence at the UX layer.
		slog.Info("login failed", "username", username, "err", err)
		a.renderLogin(w, username, "Invalid username or password.", next)
		return
	}

	id, expires, err := a.Sessions.Create(r.Context(), user.ID,
		r.UserAgent(), clientIP(r))
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	a.Sessions.SetCookie(w, id, expires)
	_ = a.Queries.TouchUserLogin(r.Context(), user.ID)
	http.Redirect(w, r, nextOr("/", next), http.StatusSeeOther)
}

// PostLogout destroys the session and redirects to /login.
func (a *LocalAuth) PostLogout(w http.ResponseWriter, r *http.Request) {
	id := CookieFromRequest(r)
	if id != "" {
		_ = a.Sessions.Destroy(r.Context(), id)
	}
	a.Sessions.ClearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// dummyHash is compared against when the username is unknown so that a missing
// user and a wrong password take the same time - defeating username enumeration
// by timing. It is generated at bcrypt.DefaultCost, the SAME cost HashPassword
// uses, on purpose: a mismatched cost (e.g. the old hardcoded $2a$12$ string
// against cost-10 real hashes) makes the "no such user" branch measurably slower
// and reintroduces the very signal this defends against. If the password cost is
// ever raised, do it as a coordinated rehash-on-login migration so real hashes
// and this dummy move together.
//
// Computed lazily via sync.OnceValue: bcrypt at DefaultCost costs ~50ms, which
// a package-level init would charge to EVERY process start (server, CLI
// subcommands, all test binaries) even when verify() never runs. A generation
// error panics rather than being discarded - it can only mean crypto/rand is
// broken (session-ID minting is equally dead then), and the silent alternative
// is a nil hash whose compare fails instantly, quietly re-opening the timing
// side channel.
var dummyHash = sync.OnceValue(func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("timing-defense-placeholder"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Sprintf("auth: generate timing-defense dummy hash: %v", err))
	}
	return h
})

// verify looks up the user by username, checks the bcrypt hash, and returns
// the user on success.
func (a *LocalAuth) verify(ctx context.Context, username, password string) (*User, error) {
	if username == "" || password == "" {
		return nil, errors.New("missing credentials")
	}
	row, err := a.Queries.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Run a bcrypt compare against a same-cost dummy hash so the timing
			// of a missing username matches a wrong-password attempt.
			_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
			return nil, errors.New("no such user")
		}
		return nil, err
	}
	if row.IsActive == 0 {
		return nil, errors.New("user deactivated")
	}
	if row.PasswordHash == nil || *row.PasswordHash == "" {
		return nil, errors.New("no password set; use the users CLI to set one")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*row.PasswordHash), []byte(password)); err != nil {
		return nil, fmt.Errorf("password mismatch: %w", err)
	}
	display := row.Username
	if row.DisplayName != nil && *row.DisplayName != "" {
		display = *row.DisplayName
	}
	email := ""
	if row.Email != nil {
		email = *row.Email
	}
	return &User{
		ID:          row.ID,
		Username:    row.Username,
		DisplayName: display,
		Email:       email,
		IsAdmin:     row.IsAdmin != 0,
	}, nil
}

func (a *LocalAuth) renderLogin(w http.ResponseWriter, username, errMsg, next string) {
	if a.LoginTemplate == nil {
		// Fallback for tests / before main wires the template.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<html><body><form method=post><input name=username value=%q><input type=password name=password><input type=hidden name=next value=%q><button>Sign in</button></form><p>%s</p></body></html>`, username, next, errMsg)
		return
	}
	a.LoginTemplate(w, map[string]any{
		"Username": username,
		"Error":    errMsg,
		"Next":     next,
	})
}

// HashPassword returns a bcrypt hash suitable for users.password_hash. Used
// by the users CLI and the admin reset-password handler.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// safeNext URL-escapes the next path so it can be embedded in a redirect
// query string without splitting on the first '?'.
func safeNext(s string) string {
	// Keep it minimal — `?next=` only needs to survive a redirect, and the
	// PostLogin handler trusts it only when it starts with "/".
	out := strings.ReplaceAll(s, "&", "%26")
	out = strings.ReplaceAll(out, " ", "%20")
	out = strings.ReplaceAll(out, "#", "%23")
	return out
}

// nextOr returns next if it's a safe relative path, else fallback.
func nextOr(fallback, next string) string {
	if next == "" {
		return fallback
	}
	// Only same-origin paths. Reject scheme/host or protocol-relative URLs.
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return fallback
	}
	return next
}

// clientIP returns the request's RemoteAddr stripped of the port, for
// session bookkeeping (informational; the auth check doesn't trust it).
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
