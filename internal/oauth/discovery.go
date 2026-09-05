package oauth

import (
	"encoding/json"
	"net/http"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// PRMHandler serves the RFC 9728 protected-resource metadata for /mcp. It is
// mounted at both /.well-known/oauth-protected-resource (what the WWW-Authenticate
// challenge points at, and what older clients probe) and
// /.well-known/oauth-protected-resource/mcp (what clients on the 2025-06-18 MCP
// spec derive by inserting the resource path). Same body either way.
//
// mcp-go's handler is reused for the body/headers (CORS, no-store, method
// switch); it is registered by main.go on the root mux because the MCP server
// itself is mounted at the exact path /mcp and would never see a well-known URL.
func (s *Server) PRMHandler() http.Handler {
	return mcpserver.NewProtectedResourceMetadataHandler(mcpserver.ProtectedResourceMetadataConfig{
		Resource:               s.Resource(),
		AuthorizationServers:   []string{s.Issuer()},
		ScopesSupported:        []string{ScopeMCP, ScopeOfflineAccess},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Trading Journal MCP",
	})
}

// ASMetadata is the RFC 8414 authorization-server metadata document. Only the
// fields this server actually implements are advertised: authorization-code +
// refresh grants, PKCE S256 (the only accepted method), public clients only
// (token_endpoint_auth_methods_supported = none), and dynamic registration.
type ASMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
}

// ASMetadata builds the document from the configured issuer.
func (s *Server) ASMetadata() ASMetadata {
	return ASMetadata{
		Issuer:                            s.Issuer(),
		AuthorizationEndpoint:             s.AuthorizationEndpoint(),
		TokenEndpoint:                     s.TokenEndpoint(),
		RegistrationEndpoint:              s.RegistrationEndpoint(),
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		ScopesSupported:                   []string{ScopeMCP, ScopeOfflineAccess},
	}
}

// ASMetadataHandler serves /.well-known/oauth-authorization-server with the
// same conventions as the PRM handler: CORS for browser-based clients,
// no-store so a changed issuer is picked up, GET/HEAD/OPTIONS only.
func (s *Server) ASMetadataHandler() http.Handler {
	body, err := json.Marshal(s.ASMetadata())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet, http.MethodHead:
		default:
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err != nil {
			http.Error(w, "failed to marshal authorization server metadata", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}
