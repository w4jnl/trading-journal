package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
)

// ProxyAuth resolves identity from headers set by a trusted upstream reverse
// proxy (Authelia, oauth2-proxy, Caddy forward_auth, etc.). The flow:
//
//  1. Check r.RemoteAddr against TrustedProxies (CIDR allowlist). Direct
//     requests from outside the allowlist read as unauthenticated.
//  2. Read UserHeader. Empty -> unauthenticated.
//  3. Look up the user by username. If absent and AutoProvision is true,
//     create a row from the headers; otherwise unauthenticated.
//  4. Refresh email / display_name / is_admin from headers on every
//     request so config drift in the SSO propagates without manual
//     reconciliation.
//
// /login has no purpose in proxy mode; /logout redirects to LogoutURL if
// configured, else returns a stub page asking the user to sign out via SSO.
type ProxyAuth struct {
	DB      *sql.DB
	Queries *models.Queries

	// Header names. UserHeader is mandatory; the others are optional.
	UserHeader        string
	EmailHeader       string
	DisplayNameHeader string
	GroupsHeader      string

	// Group string (case-sensitive substring match against the
	// comma-separated GroupsHeader value) that grants admin.
	AdminGroup string

	// TrustedProxies are the parsed CIDR ranges. Requests from outside
	// the union are refused.
	TrustedProxies []*net.IPNet

	AutoProvision bool

	// LogoutURL: where to send the browser on /logout. Empty -> stub page.
	LogoutURL string
}

// NewProxyAuth parses raw CIDR strings into ranges and returns a ready
// authenticator. Returns an error if any CIDR is malformed.
func NewProxyAuth(db *sql.DB, q *models.Queries, opts ProxyAuthOptions) (*ProxyAuth, error) {
	nets := make([]*net.IPNet, 0, len(opts.TrustedProxies))
	for _, raw := range opts.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Allow bare-IP entries too — interpret as /32 or /128.
		if !strings.Contains(raw, "/") {
			ip := net.ParseIP(raw)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted_proxies entry %q", raw)
			}
			if ip.To4() != nil {
				raw += "/32"
			} else {
				raw += "/128"
			}
		}
		_, n, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted_proxies entry %q: %w", raw, err)
		}
		nets = append(nets, n)
	}
	if len(nets) == 0 {
		return nil, errors.New("proxy mode requires at least one trusted_proxies entry")
	}
	if opts.UserHeader == "" {
		return nil, errors.New("proxy mode requires user_header")
	}
	return &ProxyAuth{
		DB:                db,
		Queries:           q,
		UserHeader:        opts.UserHeader,
		EmailHeader:       opts.EmailHeader,
		DisplayNameHeader: opts.DisplayNameHeader,
		GroupsHeader:      opts.GroupsHeader,
		AdminGroup:        opts.AdminGroup,
		TrustedProxies:    nets,
		AutoProvision:     opts.AutoProvision,
		LogoutURL:         opts.LogoutURL,
	}, nil
}

// ProxyAuthOptions mirrors config.ProxyAuth so callers don't have to import
// internal/config from this package.
type ProxyAuthOptions struct {
	UserHeader        string
	EmailHeader       string
	DisplayNameHeader string
	GroupsHeader      string
	AdminGroup        string
	TrustedProxies    []string
	AutoProvision     bool
	LogoutURL         string
}

// ResolveUser implements Authenticator.
func (a *ProxyAuth) ResolveUser(r *http.Request) (*User, error) {
	if !a.trustedRemote(r) {
		slog.Warn("proxy auth: untrusted RemoteAddr; ignoring identity headers",
			"remote", r.RemoteAddr, "path", r.URL.Path)
		return nil, nil
	}
	username := strings.TrimSpace(r.Header.Get(a.UserHeader))
	if username == "" {
		return nil, nil
	}
	email := strings.TrimSpace(r.Header.Get(a.EmailHeader))
	display := strings.TrimSpace(r.Header.Get(a.DisplayNameHeader))
	if display == "" {
		display = username
	}
	isAdmin := a.isAdminFromHeaders(r)

	row, err := a.Queries.GetUserByUsername(r.Context(), username)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("lookup: %w", err)
		}
		if !a.AutoProvision {
			slog.Warn("proxy auth: unknown user and auto_provision is off; refusing",
				"username", username)
			return nil, nil
		}
		adminFlag := int64(0)
		if isAdmin {
			adminFlag = 1
		}
		var emailPtr, displayPtr *string
		if email != "" {
			emailPtr = &email
		}
		if display != "" {
			displayPtr = &display
		}
		created, err := a.Queries.CreateUser(r.Context(), models.CreateUserParams{
			Username:     username,
			Email:        emailPtr,
			PasswordHash: nil, // proxy users have no local password
			IsAdmin:      adminFlag,
			DisplayName:  displayPtr,
		})
		if err != nil {
			return nil, fmt.Errorf("auto-provision: %w", err)
		}
		slog.Info("proxy auth: provisioned user", "username", username, "id", created.ID, "admin", isAdmin)
		return &User{
			ID:          created.ID,
			Username:    created.Username,
			DisplayName: display,
			Email:       email,
			IsAdmin:     isAdmin,
		}, nil
	}
	if row.IsActive == 0 {
		slog.Warn("proxy auth: user is deactivated; refusing", "username", username)
		return nil, nil
	}

	// Refresh profile from headers. Cheap single-row UPDATE; skip if
	// nothing actually changed to avoid write churn.
	dbEmail := ""
	if row.Email != nil {
		dbEmail = *row.Email
	}
	dbDisplay := ""
	if row.DisplayName != nil {
		dbDisplay = *row.DisplayName
	}
	adminFlag := int64(0)
	if isAdmin {
		adminFlag = 1
	}
	if dbEmail != email || dbDisplay != display || row.IsAdmin != adminFlag {
		var emailPtr, displayPtr *string
		if email != "" {
			emailPtr = &email
		}
		if display != "" {
			displayPtr = &display
		}
		if err := a.Queries.UpdateUserProfileFromProxy(r.Context(), models.UpdateUserProfileFromProxyParams{
			Email:       emailPtr,
			DisplayName: displayPtr,
			IsAdmin:     adminFlag,
			ID:          row.ID,
		}); err != nil {
			slog.Warn("proxy auth: profile refresh failed", "username", username, "err", err)
		}
	}

	return &User{
		ID:          row.ID,
		Username:    row.Username,
		DisplayName: display,
		Email:       email,
		IsAdmin:     isAdmin,
	}, nil
}

// trustedRemote returns true when r.RemoteAddr falls within any
// TrustedProxies range.
func (a *ProxyAuth) trustedRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr // unix-socket case or already host-only
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range a.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isAdminFromHeaders parses GroupsHeader as a comma-separated list and
// looks for an exact (trimmed) match of AdminGroup. Empty GroupsHeader
// or empty AdminGroup -> not admin.
func (a *ProxyAuth) isAdminFromHeaders(r *http.Request) bool {
	if a.GroupsHeader == "" || a.AdminGroup == "" {
		return false
	}
	raw := r.Header.Get(a.GroupsHeader)
	if raw == "" {
		return false
	}
	for _, g := range strings.Split(raw, ",") {
		if strings.TrimSpace(g) == a.AdminGroup {
			return true
		}
	}
	return false
}

// Routes registers /logout. /login isn't applicable in proxy mode (the
// upstream SSO owns sign-in); a GET /login redirects to a stub or the
// configured LogoutURL so a stale bookmark doesn't 404.
func (a *ProxyAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /logout", a.handleLogout)
	mux.HandleFunc("POST /logout", a.handleLogout)
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

func (a *ProxyAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if a.LogoutURL != "" {
		http.Redirect(w, r, a.LogoutURL, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><body style="font-family:sans-serif;padding:32px;max-width:560px;margin:0 auto;">
<h1>Sign out via your SSO</h1>
<p>This deployment uses upstream authentication (Authelia, oauth2-proxy, or similar). To sign out, end the session at your identity provider; this page can only ask you to.</p>
</body></html>`))
}

// HandleUnauthenticated returns 401 when the proxy didn't pass an identity
// header. Browsers normally won't see this — a missing header means the
// proxy didn't forward the request — but it's a useful signal under
// misconfiguration.
func (a *ProxyAuth) HandleUnauthenticated(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "unauthenticated (no proxy identity header)", http.StatusUnauthorized)
}

var _ Authenticator = (*ProxyAuth)(nil)

// Allow callers to confirm at startup that we built a sane allowlist.
func (a *ProxyAuth) DescribeTrustedProxies() string {
	parts := make([]string, 0, len(a.TrustedProxies))
	for _, n := range a.TrustedProxies {
		parts = append(parts, n.String())
	}
	return strings.Join(parts, ",")
}

// context guard so unused imports don't trip the build before everything
// lands.
var _ = context.Background
