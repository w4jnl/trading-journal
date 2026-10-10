// Package oauth is the journal's built-in OAuth 2.1 authorization server for
// the MCP endpoint. AI connectors that cannot hold a static API key (ChatGPT,
// claude.ai, Claude Code) register themselves dynamically (RFC 7591), send the
// signed-in user to /oauth/authorize for a one-click approval, and exchange a
// PKCE-protected code for opaque tokens that the /mcp middleware accepts
// alongside the existing tj_ keys.
//
// Discovery documents (RFC 9728 protected-resource metadata and RFC 8414
// authorization-server metadata) live here too. Everything is derived from a
// single configured issuer origin so the URLs the clients fetch and the URLs
// the documents advertise can never drift apart.
package oauth

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Scopes the server understands. "mcp" is the only real capability; Claude
// clients also ask for "offline_access" when it is advertised, meaning "please
// give me a refresh token" - which we always do, so accepting it is harmless.
const (
	ScopeMCP           = "mcp"
	ScopeOfflineAccess = "offline_access"
)

// Config is the resolved OAuth configuration (from config.MCP.OAuth).
type Config struct {
	// Issuer is the public origin, e.g. "https://trading.example.com". Trailing
	// slashes are tolerated and stripped.
	Issuer string
	// Consent shows the approval page before a code is issued; false means
	// auto-approve for the signed-in user.
	Consent bool
	// TTLs for the three secret kinds.
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	CodeTTL         time.Duration
	// AllowedRedirectHosts, when non-empty, restricts https redirect URIs that
	// clients may register. Loopback http (Claude Code) is always permitted.
	AllowedRedirectHosts []string
}

// Server holds the dependencies the OAuth handlers share.
type Server struct {
	Queries *models.Queries
	Cfg     Config
	Logger  *slog.Logger

	// Dynamic client registration is public and unauthenticated; this bounds
	// how many registrations one client address may make per window so an
	// unauthenticated loop cannot fill the clients table (see register.go).
	regMu   sync.Mutex
	regHits map[string][]time.Time
}

// Issuer returns the configured origin without a trailing slash.
func (s *Server) Issuer() string {
	return strings.TrimRight(strings.TrimSpace(s.Cfg.Issuer), "/")
}

// Resource is the protected resource identifier (the MCP endpoint URL) that
// access tokens are bound to. It is also the RFC 8707 audience value.
func (s *Server) Resource() string { return s.Issuer() + "/mcp" }

// AuthorizationEndpoint is where the human approves a client (browser; sits
// behind the normal proxy/session login).
func (s *Server) AuthorizationEndpoint() string { return s.Issuer() + "/oauth/authorize" }

// TokenEndpoint exchanges codes and refresh tokens (public, machine-to-machine).
func (s *Server) TokenEndpoint() string { return s.Issuer() + "/oauth/token" }

// RegistrationEndpoint is the RFC 7591 dynamic client registration URL (public).
func (s *Server) RegistrationEndpoint() string { return s.Issuer() + "/oauth/register" }

// ProtectedResourceMetadataURL is what the /mcp 401 challenge points at.
func (s *Server) ProtectedResourceMetadataURL() string {
	return s.Issuer() + "/.well-known/oauth-protected-resource"
}

// AuthorizationServerMetadataURL is the RFC 8414 document location.
func (s *Server) AuthorizationServerMetadataURL() string {
	return s.Issuer() + "/.well-known/oauth-authorization-server"
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
