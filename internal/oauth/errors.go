package oauth

import (
	"encoding/json"
	"net/http"
)

// OAuth error codes used by this server (RFC 6749 section 5.2 / 4.1.2.1,
// RFC 7591 section 3.2.2, RFC 8707 section 2).
const (
	ErrInvalidRequest          = "invalid_request"
	ErrInvalidClient           = "invalid_client"
	ErrInvalidGrant            = "invalid_grant"
	ErrInvalidScope            = "invalid_scope"
	ErrInvalidTarget           = "invalid_target"
	ErrUnsupportedGrantType    = "unsupported_grant_type"
	ErrUnsupportedResponseType = "unsupported_response_type"
	ErrAccessDenied            = "access_denied"
	ErrServerError             = "server_error"
	ErrInvalidRedirectURI      = "invalid_redirect_uri"
	ErrInvalidClientMetadata   = "invalid_client_metadata"
)

// errorBody is the JSON envelope every OAuth endpoint uses for failures.
type errorBody struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// writeJSON emits v with the headers the OAuth specs require of token and
// registration responses: JSON, and never cached.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, errorBody{Error: code, Description: description})
}

// setCORS allows browser-based OAuth clients to call the machine endpoints.
// Nothing here is cookie-authenticated, so a wildcard origin is safe.
func setCORS(w http.ResponseWriter, methods string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", methods)
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

// preflight answers OPTIONS and rejects methods other than allowed. Returns
// true when the caller should stop.
func preflight(w http.ResponseWriter, r *http.Request, allowed string) bool {
	setCORS(w, allowed+", OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if r.Method != allowed {
		w.Header().Set("Allow", allowed+", OPTIONS")
		writeOAuthError(w, http.StatusMethodNotAllowed, ErrInvalidRequest, "method not allowed")
		return true
	}
	return false
}
