package oauth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// TokenResponse is the RFC 6749 section 5.1 success body.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// minted is a freshly generated secret: the plaintext that goes to the client
// and the hash that goes to the database.
type minted struct{ full, sha string }

func mint(prefix string) (minted, error) {
	full, sha, err := NewOpaque(prefix)
	return minted{full: full, sha: sha}, err
}

// TokenHandler implements the token endpoint for public clients:
// authorization_code (with mandatory PKCE) and refresh_token (rotating).
// Requests are application/x-www-form-urlencoded per RFC 6749 4.1.3.
func (s *Server) TokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if preflight(w, r, http.MethodPost) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidRequest, "body must be application/x-www-form-urlencoded")
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			s.exchangeCode(r.Context(), w, r.PostForm)
		case "refresh_token":
			s.refresh(r.Context(), w, r.PostForm)
		case "":
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidRequest, "grant_type is required")
		default:
			writeOAuthError(w, http.StatusBadRequest, ErrUnsupportedGrantType, "supported grant types: authorization_code, refresh_token")
		}
	})
}

// exchangeCode redeems a single-use code (RFC 6749 4.1.3 + RFC 7636 4.6).
// Every binding made at authorize time is re-checked: client, exact
// redirect_uri, PKCE, resource. A replayed code revokes the grant it came
// from - the legitimate client already has its tokens, so a second redemption
// can only be an attacker who captured the code.
func (s *Server) exchangeCode(ctx context.Context, w http.ResponseWriter, f url.Values) {
	code, redirectURI, clientID, verifier := f.Get("code"), f.Get("redirect_uri"), f.Get("client_id"), f.Get("code_verifier")
	if code == "" || clientID == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidRequest, "code, client_id and code_verifier are required")
		return
	}
	row, err := s.Queries.GetOAuthAuthCodeBySHA256(ctx, HashToken(code))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "unknown authorization code")
			return
		}
		s.serverErr(w, "code lookup", err)
		return
	}
	n, err := s.Queries.ConsumeOAuthAuthCode(ctx, row.ID)
	if err != nil {
		s.serverErr(w, "consume code", err)
		return
	}
	if n == 0 {
		s.logger().Warn("oauth: authorization code replayed; revoking grant", "grant", row.GrantID, "client_id", row.ClientID)
		_ = s.Queries.RevokeOAuthGrantByID(ctx, row.GrantID)
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "authorization code already used")
		return
	}
	if time.Now().UTC().After(row.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "authorization code expired")
		return
	}
	if row.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "client_id does not match the authorization")
		return
	}
	if row.RedirectUri != redirectURI {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "redirect_uri does not match the authorization")
		return
	}
	if !VerifyPKCES256(verifier, row.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "PKCE verification failed")
		return
	}
	if r := normalizeResource(f.Get("resource")); r != "" && r != deref(row.Resource) {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidTarget, "resource does not match the authorization")
		return
	}
	grant, err := s.Queries.GetOAuthGrantByID(ctx, row.GrantID)
	if err != nil || grant.RevokedAt != nil {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "the authorization has been revoked")
		return
	}
	s.issueTokens(ctx, w, grant, row.Resource, row.Scope, nil)
}

// refresh rotates a refresh token (OAuth 2.1 requirement for public clients):
// the presented token is retired and a new pair issued. A token that was
// already rotated is proof of a leak - the whole grant is revoked so neither
// party keeps access, and the user reconnects.
func (s *Server) refresh(ctx context.Context, w http.ResponseWriter, f url.Values) {
	rt, clientID := f.Get("refresh_token"), f.Get("client_id")
	if rt == "" || clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidRequest, "refresh_token and client_id are required")
		return
	}
	row, err := s.Queries.GetOAuthRefreshTokenBySHA256(ctx, HashToken(rt))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "unknown refresh token")
			return
		}
		s.serverErr(w, "refresh lookup", err)
		return
	}
	if row.GrantRevokedAt != nil {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "the authorization has been revoked")
		return
	}
	if row.RevokedAt != nil {
		s.logger().Warn("oauth: refresh token reuse detected; revoking grant", "grant", row.GrantID, "client_id", row.ClientID)
		_ = s.Queries.RevokeOAuthGrantByID(ctx, row.GrantID)
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "refresh token reuse detected; the authorization has been revoked")
		return
	}
	if row.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "client_id does not match the refresh token")
		return
	}
	if time.Now().UTC().After(row.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "refresh token expired")
		return
	}
	if r := normalizeResource(f.Get("resource")); r != "" && r != deref(row.Resource) {
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidTarget, "resource does not match the authorization")
		return
	}
	if sc := f.Get("scope"); sc != "" {
		norm, err := NormalizeScope(sc)
		if err != nil || !ScopeSubset(norm, deref(row.Scope)) {
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidScope, "requested scope exceeds the authorization")
			return
		}
	}

	next, err := mint(PrefixRefreshToken)
	if err != nil {
		s.serverErr(w, "mint refresh", err)
		return
	}
	n, err := s.Queries.RotateOAuthRefreshToken(ctx, models.RotateOAuthRefreshTokenParams{RotatedTo: &next.sha, ID: row.ID})
	if err != nil {
		s.serverErr(w, "rotate refresh", err)
		return
	}
	if n == 0 {
		// Lost a race with a concurrent redemption of the same token: that
		// is reuse too.
		s.logger().Warn("oauth: concurrent refresh token reuse; revoking grant", "grant", row.GrantID)
		_ = s.Queries.RevokeOAuthGrantByID(ctx, row.GrantID)
		writeOAuthError(w, http.StatusBadRequest, ErrInvalidGrant, "refresh token reuse detected; the authorization has been revoked")
		return
	}
	grant, err := s.Queries.GetOAuthGrantByID(ctx, row.GrantID)
	if err != nil {
		s.serverErr(w, "grant lookup", err)
		return
	}
	s.issueTokens(ctx, w, grant, row.Resource, row.Scope, &next)
}

// issueTokens stores a new access token (and the given or a fresh refresh
// token) under grant and writes the success response.
func (s *Server) issueTokens(ctx context.Context, w http.ResponseWriter, grant models.OauthGrant, resource, scope *string, refresh *minted) {
	now := time.Now().UTC()
	access, err := mint(PrefixAccessToken)
	if err != nil {
		s.serverErr(w, "mint access", err)
		return
	}
	if refresh == nil {
		m, err := mint(PrefixRefreshToken)
		if err != nil {
			s.serverErr(w, "mint refresh", err)
			return
		}
		refresh = &m
	}
	if _, err := s.Queries.CreateOAuthAccessToken(ctx, models.CreateOAuthAccessTokenParams{
		TokenSha256: access.sha, GrantID: grant.ID, ClientID: grant.ClientID, UserID: grant.UserID,
		Resource: resource, Scope: scope, ExpiresAt: now.Add(s.Cfg.AccessTokenTTL),
	}); err != nil {
		s.serverErr(w, "store access token", err)
		return
	}
	if _, err := s.Queries.CreateOAuthRefreshToken(ctx, models.CreateOAuthRefreshTokenParams{
		TokenSha256: refresh.sha, GrantID: grant.ID, ClientID: grant.ClientID, UserID: grant.UserID,
		Resource: resource, Scope: scope, ExpiresAt: now.Add(s.Cfg.RefreshTokenTTL),
	}); err != nil {
		s.serverErr(w, "store refresh token", err)
		return
	}
	_ = s.Queries.TouchOAuthGrantLastUsed(ctx, grant.ID)
	writeJSON(w, http.StatusOK, TokenResponse{
		AccessToken:  access.full,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.Cfg.AccessTokenTTL / time.Second),
		RefreshToken: refresh.full,
		Scope:        deref(scope),
	})
}

func (s *Server) serverErr(w http.ResponseWriter, what string, err error) {
	s.logger().Error("oauth: "+what, "err", err)
	writeOAuthError(w, http.StatusInternalServerError, ErrServerError, "internal error")
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
