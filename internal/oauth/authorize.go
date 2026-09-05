package oauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// AuthorizeRequest is the /oauth/authorize parameter set (RFC 6749 4.1.1 +
// PKCE + RFC 8707 resource). It is read identically from the GET query and
// from the consent form's hidden fields, and validated identically both
// times - the form is never trusted to have been validated before.
type AuthorizeRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	Resource            string
	CodeChallenge       string
	CodeChallengeMethod string
}

// ParseAuthorizeRequest reads the request from query or form values.
func ParseAuthorizeRequest(v url.Values) AuthorizeRequest {
	g := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	return AuthorizeRequest{
		ResponseType:        g("response_type"),
		ClientID:            g("client_id"),
		RedirectURI:         g("redirect_uri"),
		State:               g("state"),
		Scope:               g("scope"),
		Resource:            g("resource"),
		CodeChallenge:       g("code_challenge"),
		CodeChallengeMethod: g("code_challenge_method"),
	}
}

// Values renders the request back into form/query parameters (for the
// consent page's hidden fields).
func (a AuthorizeRequest) Values() url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("response_type", a.ResponseType)
	set("client_id", a.ClientID)
	set("redirect_uri", a.RedirectURI)
	set("state", a.State)
	set("scope", a.Scope)
	set("resource", a.Resource)
	set("code_challenge", a.CodeChallenge)
	set("code_challenge_method", a.CodeChallengeMethod)
	return v
}

// AuthorizeError carries an OAuth error code plus whether it may be reported
// to the client by redirect. RFC 6749 4.1.2.1: when the client or its
// redirect_uri cannot be trusted, the server MUST NOT redirect and must inform
// the user directly instead - those errors have Redirectable=false.
type AuthorizeError struct {
	Code         string
	Description  string
	Redirectable bool
}

func (e *AuthorizeError) Error() string { return e.Code + ": " + e.Description }

func fatal(code, desc string) *AuthorizeError { return &AuthorizeError{Code: code, Description: desc} }
func redirectable(code, desc string) *AuthorizeError {
	return &AuthorizeError{Code: code, Description: desc, Redirectable: true}
}

// NormalizeScope validates and canonicalizes a space-separated scope list.
// Empty means "mcp". Only the scopes this server advertises are accepted.
func NormalizeScope(raw string) (string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ScopeMCP, nil
	}
	seen := map[string]bool{}
	for _, f := range fields {
		switch f {
		case ScopeMCP, ScopeOfflineAccess:
			seen[f] = true
		default:
			return "", fmt.Errorf("unknown scope %q", f)
		}
	}
	// Always include mcp: offline_access alone would be a refresh token with
	// no capability behind it.
	seen[ScopeMCP] = true
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, " "), nil
}

// ScopeSubset reports whether every scope in requested is in granted.
func ScopeSubset(requested, granted string) bool {
	have := map[string]bool{}
	for _, g := range strings.Fields(granted) {
		have[g] = true
	}
	for _, r := range strings.Fields(requested) {
		if !have[r] {
			return false
		}
	}
	return true
}

// normalizeResource strips a trailing slash so "https://h/mcp/" and
// "https://h/mcp" compare equal; empty stays empty.
func normalizeResource(r string) string { return strings.TrimRight(strings.TrimSpace(r), "/") }

// ValidateAuthorize checks an authorization request against the registered
// client, in the order that keeps open redirects impossible: first establish
// that the client exists and that redirect_uri is one it registered (failures
// here are fatal - shown to the user, never redirected), and only then judge
// the remaining parameters (failures here go back to the now-trusted
// redirect_uri as OAuth error responses).
func (s *Server) ValidateAuthorize(ctx context.Context, req AuthorizeRequest) (*models.OauthClient, *AuthorizeError) {
	if req.ClientID == "" {
		return nil, fatal(ErrInvalidRequest, "client_id is required")
	}
	client, err := s.Queries.GetOAuthClientByClientID(ctx, req.ClientID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fatal(ErrInvalidClient, "unknown client_id; the app must register again")
		}
		s.logger().Error("oauth: client lookup", "err", err)
		return nil, fatal(ErrServerError, "could not look up the client")
	}
	if req.RedirectURI == "" {
		return nil, fatal(ErrInvalidRequest, "redirect_uri is required")
	}
	if !MatchesAnyRedirectURI(ClientRedirectURIs(client), req.RedirectURI) {
		return nil, fatal(ErrInvalidRequest, "redirect_uri is not registered for this client")
	}

	// From here on redirect_uri is trusted, so errors may travel back to it.
	if req.ResponseType != "code" {
		return &client, redirectable(ErrUnsupportedResponseType, "response_type must be \"code\"")
	}
	if req.CodeChallengeMethod != "S256" {
		return &client, redirectable(ErrInvalidRequest, "code_challenge_method must be S256 (PKCE is required)")
	}
	if !ValidPKCEString(req.CodeChallenge) {
		return &client, redirectable(ErrInvalidRequest, "code_challenge is missing or malformed")
	}
	if _, err := NormalizeScope(req.Scope); err != nil {
		return &client, redirectable(ErrInvalidScope, err.Error())
	}
	if r := normalizeResource(req.Resource); r != "" && r != s.Resource() {
		return &client, redirectable(ErrInvalidTarget, "resource must be "+s.Resource())
	}
	return &client, nil
}

// IssueCode records the user's approval (reusing a live grant for the same
// client so Settings shows one row per connection) and mints a single-use,
// PKCE-bound authorization code. Only the sha256 of the code is stored.
func (s *Server) IssueCode(ctx context.Context, userID int64, req AuthorizeRequest) (string, error) {
	scope, err := NormalizeScope(req.Scope)
	if err != nil {
		return "", err
	}
	resource := normalizeResource(req.Resource)
	if resource == "" {
		resource = s.Resource()
	}

	grant, err := s.Queries.GetActiveOAuthGrantForUserClient(ctx, models.GetActiveOAuthGrantForUserClientParams{
		UserID: userID, ClientID: req.ClientID,
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("grant lookup: %w", err)
		}
		grant, err = s.Queries.CreateOAuthGrant(ctx, models.CreateOAuthGrantParams{
			UserID: userID, ClientID: req.ClientID, Scope: &scope, Resource: &resource,
		})
		if err != nil {
			return "", fmt.Errorf("create grant: %w", err)
		}
		s.logger().Info("oauth: grant created", "user", userID, "client_id", req.ClientID[:min(12, len(req.ClientID))]+"...", "grant", grant.ID)
	}

	code, sha, err := NewOpaque("")
	if err != nil {
		return "", err
	}
	_, err = s.Queries.CreateOAuthAuthCode(ctx, models.CreateOAuthAuthCodeParams{
		CodeSha256:          sha,
		GrantID:             grant.ID,
		ClientID:            req.ClientID,
		UserID:              userID,
		RedirectUri:         req.RedirectURI,
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: "S256",
		Resource:            &resource,
		Scope:               &scope,
		ExpiresAt:           time.Now().UTC().Add(s.Cfg.CodeTTL),
	})
	if err != nil {
		return "", fmt.Errorf("create code: %w", err)
	}
	return code, nil
}

// SuccessRedirect appends code and state to the (trusted) redirect_uri,
// keeping any query component the client registered (RFC 6749 3.1.2.2).
func SuccessRedirect(redirectURI, code, state string) string {
	return appendQuery(redirectURI, url.Values{"code": {code}, "state": {state}})
}

// ErrorRedirect reports a redirectable authorization error to the client.
func ErrorRedirect(redirectURI string, e *AuthorizeError, state string) string {
	v := url.Values{"error": {e.Code}, "state": {state}}
	if e.Description != "" {
		v.Set("error_description", e.Description)
	}
	return appendQuery(redirectURI, v)
}

func appendQuery(rawURL string, add url.Values) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for k, vs := range add {
		for _, v := range vs {
			if v != "" || k == "code" {
				q.Set(k, v)
			}
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
