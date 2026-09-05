package oauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

const testIssuer = "https://trading.example.com"

func newFlowServer(t *testing.T) (*Server, *models.Queries, int64) {
	t.Helper()
	conn := dbtest.Open(t, "bootstrap")
	q := models.New(conn)
	u, err := q.CreateUser(context.Background(), models.CreateUserParams{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Queries: q, Cfg: Config{
		Issuer: testIssuer, Consent: true,
		AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour, CodeTTL: time.Minute,
	}}
	return s, q, u.ID
}

func register(t *testing.T, s *Server, body string) (int, RegisterResponse, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.RegisterHandler().ServeHTTP(rec, req)
	var resp RegisterResponse
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	return rec.Code, resp, raw
}

func pkcePair() (verifier, challenge string) {
	verifier = strings.Repeat("v", 20) + "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"[:23]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func tokenPost(t *testing.T, s *Server, form url.Values) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.TokenHandler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// Three very different clients register independently: ChatGPT and claude.ai
// with fixed https callbacks, Claude Code with a loopback callback. Each gets
// its own public client_id and no secret; bad metadata is refused.
func TestRegisterClients(t *testing.T) {
	s, _, _ := newFlowServer(t)
	ids := map[string]bool{}
	for name, body := range map[string]string{
		"ChatGPT":     `{"client_name":"ChatGPT","redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"none","client_uri":"https://chatgpt.com"}`,
		"Claude":      `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`,
		"Claude Code": `{"client_name":"Claude Code","redirect_uris":["http://localhost:52341/callback","http://127.0.0.1:52341/callback"]}`,
	} {
		code, resp, raw := register(t, s, body)
		if code != http.StatusCreated {
			t.Fatalf("%s: status %d body %v", name, code, raw)
		}
		if !strings.HasPrefix(resp.ClientID, PrefixClientID) || ids[resp.ClientID] {
			t.Errorf("%s: client_id %q", name, resp.ClientID)
		}
		ids[resp.ClientID] = true
		if _, has := raw["client_secret"]; has {
			t.Errorf("%s: public clients must not get a secret", name)
		}
		if resp.TokenEndpointAuthMethod != "none" || resp.ClientName != name || len(resp.GrantTypes) != 2 || resp.ResponseTypes[0] != "code" {
			t.Errorf("%s: response %+v", name, resp)
		}
	}
	for name, body := range map[string]string{
		"no redirects":   `{"client_name":"x"}`,
		"http real host": `{"redirect_uris":["http://example.com/cb"]}`,
		"secret auth":    `{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
		"implicit grant": `{"redirect_uris":["https://a.example/cb"],"grant_types":["implicit"]}`,
		"token response": `{"redirect_uris":["https://a.example/cb"],"response_types":["token"]}`,
		"not json":       `redirect_uris=x`,
	} {
		if code, _, raw := register(t, s, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %v", name, code, raw)
		}
	}
	// Allow-list applies to https hosts only; loopback stays open for Claude Code.
	s.Cfg.AllowedRedirectHosts = []string{"claude.ai"}
	if code, _, _ := register(t, s, `{"redirect_uris":["https://chatgpt.com/cb"]}`); code != http.StatusBadRequest {
		t.Errorf("allow-list must reject chatgpt.com, got %d", code)
	}
	if code, _, _ := register(t, s, `{"redirect_uris":["http://localhost:9/cb"]}`); code != http.StatusCreated {
		t.Errorf("allow-list must not block loopback, got %d", code)
	}
}

// The full code flow for a Claude-Code-style client (loopback callback on an
// ephemeral port), then every way it must fail.
func TestAuthorizationCodeFlow(t *testing.T) {
	s, q, uid := newFlowServer(t)
	ctx := context.Background()
	_, reg, _ := register(t, s, `{"client_name":"Claude Code","redirect_uris":["http://localhost/callback"]}`)
	verifier, challenge := pkcePair()
	base := AuthorizeRequest{
		ResponseType: "code", ClientID: reg.ClientID, RedirectURI: "http://localhost:4242/callback",
		State: "xyz", Scope: "offline_access mcp", CodeChallenge: challenge, CodeChallengeMethod: "S256",
	}

	client, aerr := s.ValidateAuthorize(ctx, base)
	if aerr != nil || client == nil || ClientDisplayName(*client) != "Claude Code" {
		t.Fatalf("validate: %v", aerr)
	}
	code, err := s.IssueCode(ctx, uid, base)
	if err != nil {
		t.Fatal(err)
	}
	loc := SuccessRedirect(base.RedirectURI, code, base.State)
	if !strings.HasPrefix(loc, "http://localhost:4242/callback?") || !strings.Contains(loc, "code="+code) || !strings.Contains(loc, "state=xyz") {
		t.Errorf("redirect %q", loc)
	}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {base.RedirectURI},
		"client_id": {reg.ClientID}, "code_verifier": {verifier}}
	status, body := tokenPost(t, s, form)
	if status != http.StatusOK {
		t.Fatalf("token: %d %v", status, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)
	if !strings.HasPrefix(access, PrefixAccessToken) || !strings.HasPrefix(refresh, PrefixRefreshToken) || body["token_type"] != "Bearer" || body["scope"] != "mcp offline_access" {
		t.Errorf("token body %v", body)
	}
	if exp, _ := body["expires_in"].(float64); exp != 3600 {
		t.Errorf("expires_in %v", body["expires_in"])
	}
	row, err := q.GetOAuthAccessTokenBySHA256(ctx, HashToken(access))
	if err != nil || row.UserID != uid || row.Username != "alice" || deref(row.Resource) != testIssuer+"/mcp" {
		t.Fatalf("access token lookup: %v %+v", err, row)
	}

	// Replaying the code is theft: refused, and the grant (and its tokens) die.
	if status, body := tokenPost(t, s, form); status != http.StatusBadRequest || body["error"] != ErrInvalidGrant {
		t.Errorf("code replay: %d %v", status, body)
	}
	if _, err := q.GetOAuthAccessTokenBySHA256(ctx, HashToken(access)); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("access token must be dead after code replay, err=%v", err)
	}

	// Each failure gets a fresh code.
	fresh := func() string {
		c, err := s.IssueCode(ctx, uid, base)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cases := map[string]struct {
		mutate  func(url.Values)
		wantErr string
	}{
		"wrong verifier":    {func(f url.Values) { f.Set("code_verifier", strings.Repeat("x", 50)) }, ErrInvalidGrant},
		"different port":    {func(f url.Values) { f.Set("redirect_uri", "http://localhost:9999/callback") }, ErrInvalidGrant},
		"wrong client":      {func(f url.Values) { f.Set("client_id", "tjc_other") }, ErrInvalidGrant},
		"resource mismatch": {func(f url.Values) { f.Set("resource", "https://other.example/mcp") }, ErrInvalidTarget},
		"unknown code":      {func(f url.Values) { f.Set("code", "nope") }, ErrInvalidGrant},
		"missing verifier":  {func(f url.Values) { f.Del("code_verifier") }, ErrInvalidRequest},
		"unsupported grant": {func(f url.Values) { f.Set("grant_type", "client_credentials") }, ErrUnsupportedGrantType},
	}
	for name, c := range cases {
		f := url.Values{"grant_type": {"authorization_code"}, "code": {fresh()}, "redirect_uri": {base.RedirectURI},
			"client_id": {reg.ClientID}, "code_verifier": {verifier}}
		c.mutate(f)
		if status, body := tokenPost(t, s, f); status != http.StatusBadRequest || body["error"] != c.wantErr {
			t.Errorf("%s: %d %v (want %s)", name, status, body, c.wantErr)
		}
	}

	// Expired code.
	s.Cfg.CodeTTL = -time.Minute
	expired := fresh()
	s.Cfg.CodeTTL = time.Minute
	f := url.Values{"grant_type": {"authorization_code"}, "code": {expired}, "redirect_uri": {base.RedirectURI},
		"client_id": {reg.ClientID}, "code_verifier": {verifier}}
	if status, body := tokenPost(t, s, f); status != http.StatusBadRequest || body["error"] != ErrInvalidGrant {
		t.Errorf("expired code: %d %v", status, body)
	}

	// Re-approving reuses the live grant: one row per connection.
	grants, _ := q.ListOAuthGrantsForUser(ctx, uid)
	live := 0
	for _, g := range grants {
		if g.RevokedAt == nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("live grants = %d, want 1 (%d total)", live, len(grants))
	}
}

// Authorize-time validation: the two checks that protect against open
// redirects are fatal (never redirect); everything after them is reported to
// the trusted redirect_uri.
func TestValidateAuthorizeOrder(t *testing.T) {
	s, _, _ := newFlowServer(t)
	ctx := context.Background()
	_, reg, _ := register(t, s, `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`)
	_, challenge := pkcePair()
	good := AuthorizeRequest{ResponseType: "code", ClientID: reg.ClientID, RedirectURI: "https://claude.ai/api/mcp/auth_callback",
		CodeChallenge: challenge, CodeChallengeMethod: "S256"}
	if _, aerr := s.ValidateAuthorize(ctx, good); aerr != nil {
		t.Fatalf("good request rejected: %v", aerr)
	}
	cases := map[string]struct {
		mutate       func(*AuthorizeRequest)
		code         string
		redirectable bool
	}{
		"unknown client":        {func(r *AuthorizeRequest) { r.ClientID = "tjc_nobody" }, ErrInvalidClient, false},
		"missing client":        {func(r *AuthorizeRequest) { r.ClientID = "" }, ErrInvalidRequest, false},
		"unregistered redirect": {func(r *AuthorizeRequest) { r.RedirectURI = "https://evil.example/cb" }, ErrInvalidRequest, false},
		"redirect with extra":   {func(r *AuthorizeRequest) { r.RedirectURI += "?x=1" }, ErrInvalidRequest, false},
		"missing redirect":      {func(r *AuthorizeRequest) { r.RedirectURI = "" }, ErrInvalidRequest, false},
		"response_type token":   {func(r *AuthorizeRequest) { r.ResponseType = "token" }, ErrUnsupportedResponseType, true},
		"plain pkce":            {func(r *AuthorizeRequest) { r.CodeChallengeMethod = "plain" }, ErrInvalidRequest, true},
		"no challenge":          {func(r *AuthorizeRequest) { r.CodeChallenge = "" }, ErrInvalidRequest, true},
		"bad scope":             {func(r *AuthorizeRequest) { r.Scope = "admin" }, ErrInvalidScope, true},
		"wrong resource":        {func(r *AuthorizeRequest) { r.Resource = "https://other.example/mcp" }, ErrInvalidTarget, true},
	}
	for name, c := range cases {
		r := good
		c.mutate(&r)
		_, aerr := s.ValidateAuthorize(ctx, r)
		if aerr == nil || aerr.Code != c.code || aerr.Redirectable != c.redirectable {
			t.Errorf("%s: got %v, want %s redirectable=%v", name, aerr, c.code, c.redirectable)
		}
	}
	// The resource with a trailing slash is the same resource.
	r := good
	r.Resource = testIssuer + "/mcp/"
	if _, aerr := s.ValidateAuthorize(ctx, r); aerr != nil {
		t.Errorf("trailing-slash resource rejected: %v", aerr)
	}
	e := &AuthorizeError{Code: ErrAccessDenied, Description: "no", Redirectable: true}
	if got := ErrorRedirect("https://claude.ai/cb?keep=1", e, "s1"); !strings.Contains(got, "keep=1") || !strings.Contains(got, "error=access_denied") || !strings.Contains(got, "state=s1") {
		t.Errorf("error redirect %q", got)
	}
}

// Refresh tokens rotate; reusing a rotated one revokes the whole grant; a
// token cannot be refreshed under another client's id.
func TestRefreshRotation(t *testing.T) {
	s, q, uid := newFlowServer(t)
	ctx := context.Background()
	_, reg, _ := register(t, s, `{"client_name":"ChatGPT","redirect_uris":["https://chatgpt.com/cb"]}`)
	_, other, _ := register(t, s, `{"client_name":"Other","redirect_uris":["https://other.example/cb"]}`)
	verifier, challenge := pkcePair()
	req := AuthorizeRequest{ResponseType: "code", ClientID: reg.ClientID, RedirectURI: "https://chatgpt.com/cb",
		CodeChallenge: challenge, CodeChallengeMethod: "S256"}
	code, err := s.IssueCode(ctx, uid, req)
	if err != nil {
		t.Fatal(err)
	}
	_, body := tokenPost(t, s, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {req.RedirectURI},
		"client_id": {reg.ClientID}, "code_verifier": {verifier}})
	access1, refresh1 := body["access_token"].(string), body["refresh_token"].(string)

	// Wrong client id: refused, but not treated as reuse.
	if status, b := tokenPost(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}, "client_id": {other.ClientID}}); status != http.StatusBadRequest || b["error"] != ErrInvalidGrant {
		t.Errorf("cross-client refresh: %d %v", status, b)
	}
	// Scope may only narrow.
	if status, b := tokenPost(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}, "client_id": {reg.ClientID}, "scope": {"mcp offline_access"}}); status != http.StatusBadRequest || b["error"] != ErrInvalidScope {
		t.Errorf("scope escalation: %d %v", status, b)
	}

	status, b := tokenPost(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}, "client_id": {reg.ClientID}})
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %v", status, b)
	}
	access2, refresh2 := b["access_token"].(string), b["refresh_token"].(string)
	if access2 == access1 || refresh2 == refresh1 || !strings.HasPrefix(refresh2, PrefixRefreshToken) {
		t.Errorf("rotation must issue a new pair")
	}
	for _, tok := range []string{access1, access2} {
		if _, err := q.GetOAuthAccessTokenBySHA256(ctx, HashToken(tok)); err != nil {
			t.Errorf("access token %s... must still resolve: %v", tok[:8], err)
		}
	}

	// Reuse of the rotated token: grant revoked, every token dead.
	if status, b := tokenPost(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}, "client_id": {reg.ClientID}}); status != http.StatusBadRequest || b["error"] != ErrInvalidGrant {
		t.Errorf("reuse: %d %v", status, b)
	}
	for _, tok := range []string{access1, access2} {
		if _, err := q.GetOAuthAccessTokenBySHA256(ctx, HashToken(tok)); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("access token %s... must be dead after reuse, err=%v", tok[:8], err)
		}
	}
	if status, b := tokenPost(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh2}, "client_id": {reg.ClientID}}); status != http.StatusBadRequest || b["error"] != ErrInvalidGrant {
		t.Errorf("successor after family revocation: %d %v", status, b)
	}
}

func TestNormalizeScope(t *testing.T) {
	for in, want := range map[string]string{"": "mcp", "mcp": "mcp", "offline_access": "mcp offline_access", "offline_access mcp mcp": "mcp offline_access"} {
		if got, err := NormalizeScope(in); err != nil || got != want {
			t.Errorf("NormalizeScope(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeScope("mcp admin"); err == nil {
		t.Errorf("unknown scope must fail")
	}
	if !ScopeSubset("mcp", "mcp offline_access") || ScopeSubset("mcp offline_access", "mcp") {
		t.Errorf("ScopeSubset wrong")
	}
}
