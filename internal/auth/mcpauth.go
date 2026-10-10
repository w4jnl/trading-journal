package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// MCPAuth guards /mcp when the OAuth server is enabled. It accepts the
// per-user tj_ API keys exactly as APIKeyAuth does, and additionally the
// opaque tjo_ access tokens the app's own authorization server issues.
// Every rejection carries the RFC 9728 challenge header so an OAuth-capable
// client (ChatGPT, claude.ai, Claude Code) can discover how to sign in; the
// JSON body stays the API's usual {"error": ...} envelope.
type MCPAuth struct {
	Keys    *APIKeyAuth
	Queries *models.Queries
	// Resource is the audience OAuth tokens must have been minted for (the
	// canonical /mcp URL); a token issued for anything else is refused.
	Resource string
	// ResourceMetadataURL is advertised in the WWW-Authenticate challenge.
	ResourceMetadataURL string

	touched touchGate // token/grant last_used_at writes, once per touchEvery per token
}

// Middleware resolves the bearer token to a *User and injects it via WithUser,
// or answers 401 with the discovery challenge.
func (a *MCPAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := parseBearer(r)
		var (
			u      *User
			reason string
		)
		switch {
		case tok == "":
			reason = "missing bearer token"
		case strings.HasPrefix(tok, "tj_"):
			u, reason = a.Keys.authenticate(r, tok)
		case strings.HasPrefix(tok, "tjo_"):
			u, reason = a.authenticateOAuth(r, tok)
		default:
			reason = "unrecognized token"
		}
		if u == nil {
			slog.Debug("mcp auth rejected", "reason", reason, "method", r.Method, "path", r.URL.Path)
			a.challenge(w, tok != "", reason)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// authenticateOAuth resolves a tjo_ access token: it must exist, be unrevoked
// (token and grant), unexpired, minted for this resource, and belong to an
// active user. The lookup mirrors the API-key one so the resulting User is
// identical in shape.
func (a *MCPAuth) authenticateOAuth(r *http.Request, tok string) (*User, string) {
	row, err := a.Queries.GetOAuthAccessTokenBySHA256(r.Context(), HashAPIKey(tok))
	if err != nil {
		return nil, "invalid or expired access token"
	}
	if row.IsActive == 0 {
		return nil, "account disabled"
	}
	if row.Resource != nil && *row.Resource != a.Resource {
		return nil, "access token was not issued for this resource"
	}
	u := &User{
		ID:          row.UserID,
		Username:    row.Username,
		DisplayName: row.DisplayName,
		Email:       row.Email,
		IsAdmin:     row.IsAdmin != 0,
	}
	// Last-used bumps once per touchEvery per token (see touchGate).
	if tokenID, grantID := row.TokenID, row.GrantID; a.touched.allow(tokenID, time.Now()) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = a.Queries.TouchOAuthAccessTokenLastUsed(ctx, tokenID)
			_ = a.Queries.TouchOAuthGrantLastUsed(ctx, grantID)
		}()
	}
	return u, ""
}

// challenge writes the 401. With no token at all the header only points at
// the metadata (the client has not tried anything yet); with a bad token it
// also names the RFC 6750 error so the client knows to re-authenticate
// rather than retry.
func (a *MCPAuth) challenge(w http.ResponseWriter, hadToken bool, reason string) {
	v := fmt.Sprintf(`Bearer resource_metadata=%q`, a.ResourceMetadataURL)
	if hadToken {
		v = fmt.Sprintf(`Bearer error="invalid_token", error_description=%q, resource_metadata=%q`, reason, a.ResourceMetadataURL)
	}
	w.Header().Set("WWW-Authenticate", v)
	writeAPIError(w, http.StatusUnauthorized, reason)
}
