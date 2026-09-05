package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/oauth"
	"github.com/w4jnl/trading-journal/web"
)

const testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

func newOAuthHandlers(t *testing.T, consent bool) (*Handlers, *models.Queries, *auth.User, string) {
	t.Helper()
	conn, q := newAuthzTestDB(t)
	u, err := q.CreateUser(context.Background(), models.CreateUserParams{Username: "jana"})
	if err != nil {
		t.Fatal(err)
	}
	uris, _ := json.Marshal([]string{"https://claude.ai/api/mcp/auth_callback"})
	name := "Claude"
	if _, err := q.CreateOAuthClient(context.Background(), models.CreateOAuthClientParams{ClientID: "tjc_claude", ClientName: &name, RedirectUris: string(uris)}); err != nil {
		t.Fatal(err)
	}
	srv := &oauth.Server{Queries: q, Cfg: oauth.Config{Issuer: "https://trading.example.com", Consent: consent,
		AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, CodeTTL: time.Minute}}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, OAuth: srv}
	return h, q, &auth.User{ID: u.ID, Username: u.Username, DisplayName: "Jana"}, "tjc_claude"
}

func authorizeQuery(clientID string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://claude.ai/api/mcp/auth_callback"},
		"state": {"st8"}, "scope": {"mcp"}, "code_challenge": {testChallenge}, "code_challenge_method": {"S256"},
	}
}

// GET shows a consent page naming the client and the signed-in user, with
// the whole request carried as hidden fields; POST "allow" mints a code and
// redirects to the registered callback; "deny" reports access_denied.
func TestOAuthAuthorizeConsentFlow(t *testing.T) {
	h, q, user, clientID := newOAuthHandlers(t, true)
	ctx := auth.WithUser(context.Background(), user)

	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeQuery(clientID).Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.OAuthAuthorizePage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code = %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"<strong>Claude</strong>", "<strong>Jana</strong>", "claude.ai",
		`name="code_challenge" value="` + testChallenge + `"`, `name="state" value="st8"`, `name="decision" value="allow"`, `name="decision" value="deny"`} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page missing %q", want)
		}
	}

	form := authorizeQuery(clientID)
	form.Set("decision", "allow")
	req = httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.OAuthAuthorizeApprove(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("POST allow code = %d body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "claude.ai" || loc.Path != "/api/mcp/auth_callback" || loc.Query().Get("state") != "st8" || loc.Query().Get("code") == "" {
		t.Fatalf("redirect %q", rec.Header().Get("Location"))
	}
	code := loc.Query().Get("code")
	row, err := q.GetOAuthAuthCodeBySHA256(context.Background(), oauth.HashToken(code))
	if err != nil || row.UserID != user.ID || row.ClientID != clientID || row.CodeChallenge != testChallenge {
		t.Errorf("stored code %+v err=%v", row, err)
	}

	form.Set("decision", "deny")
	req = httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.OAuthAuthorizeApprove(rec, req)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || !strings.Contains(loc, "error=access_denied") || !strings.Contains(loc, "state=st8") {
		t.Errorf("deny: code=%d location=%q", rec.Code, loc)
	}
}

// Unknown client or unregistered redirect_uri: an error page with 400 and no
// redirect at all - the open-redirect guard. Parameter errors after those
// checks redirect back to the (verified) callback with an OAuth error.
func TestOAuthAuthorizeNeverRedirectsToUnverifiedURI(t *testing.T) {
	h, _, user, clientID := newOAuthHandlers(t, true)
	ctx := auth.WithUser(context.Background(), user)
	get := func(q url.Values) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.OAuthAuthorizePage(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil).WithContext(ctx))
		return rec
	}
	q := authorizeQuery("tjc_nobody")
	if rec := get(q); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), "Can't continue") {
		t.Errorf("unknown client: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	q = authorizeQuery(clientID)
	q.Set("redirect_uri", "https://evil.example/steal")
	if rec := get(q); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Errorf("unregistered redirect: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	// Same guard on the POST path: hidden fields are re-validated.
	form := authorizeQuery(clientID)
	form.Set("redirect_uri", "https://evil.example/steal")
	form.Set("decision", "allow")
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.OAuthAuthorizeApprove(rec, req)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Errorf("POST with tampered redirect: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	// After the client + redirect are verified, a bad parameter goes back to the client.
	q = authorizeQuery(clientID)
	q.Set("code_challenge_method", "plain")
	if rec := get(q); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://claude.ai/api/mcp/auth_callback?") || !strings.Contains(rec.Header().Get("Location"), "error=invalid_request") {
		t.Errorf("plain pkce: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// The consent page must let the browser follow the post-Allow redirect to the
// client: its CSP form-action gains the (verified) redirect origin, and only
// that. Without the security middleware's header nothing is added.
func TestOAuthConsentWidensFormActionToClientOrigin(t *testing.T) {
	h, _, user, clientID := newOAuthHandlers(t, true)
	ctx := auth.WithUser(context.Background(), user)
	const base = "default-src 'self'; form-action 'self' https://auth.example.com; object-src 'none'"

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy", base) // what securityHeaders sets before the handler runs
	h.OAuthAuthorizePage(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeQuery(clientID).Encode(), nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'self'; form-action 'self' https://claude.ai https://auth.example.com; object-src 'none'" {
		t.Errorf("CSP = %q", got)
	}

	// No middleware header -> untouched (tests, or a deployment without it).
	rec = httptest.NewRecorder()
	h.OAuthAuthorizePage(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeQuery(clientID).Encode(), nil).WithContext(ctx))
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("CSP without middleware = %q, want empty", got)
	}

	// The error page (unknown client) never widens anything.
	rec = httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy", base)
	h.OAuthAuthorizePage(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeQuery("tjc_nobody").Encode(), nil).WithContext(ctx))
	if got := rec.Header().Get("Content-Security-Policy"); got != base {
		t.Errorf("error page CSP = %q, want unchanged", got)
	}

	// Loopback clients (Claude Code) get their exact scheme://host:port.
	rec = httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy", "form-action 'self'")
	allowFormActionOrigin(rec, "http://localhost:52341")
	if got := rec.Header().Get("Content-Security-Policy"); got != "form-action 'self' http://localhost:52341" {
		t.Errorf("loopback CSP = %q", got)
	}
}

// With consent disabled the GET issues the code immediately; with the feature
// off entirely the routes are 404.
func TestOAuthAuthorizeAutoApproveAndDisabled(t *testing.T) {
	h, _, user, clientID := newOAuthHandlers(t, false)
	ctx := auth.WithUser(context.Background(), user)
	rec := httptest.NewRecorder()
	h.OAuthAuthorizePage(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeQuery(clientID).Encode(), nil).WithContext(ctx))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "code=") {
		t.Errorf("auto-approve: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	h.OAuth = nil
	for _, fn := range []http.HandlerFunc{h.OAuthAuthorizePage, h.OAuthAuthorizeApprove} {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize", nil).WithContext(ctx))
		if rec.Code != http.StatusNotFound {
			t.Errorf("disabled: code=%d", rec.Code)
		}
	}
}
