package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func testServer() *Server {
	// Trailing slash on purpose: every derived URL must come out clean.
	return &Server{Cfg: Config{Issuer: "https://trading.example.com/"}}
}

func TestDerivedURLs(t *testing.T) {
	s := testServer()
	want := map[string]string{
		s.Issuer():                         "https://trading.example.com",
		s.Resource():                       "https://trading.example.com/mcp",
		s.AuthorizationEndpoint():          "https://trading.example.com/oauth/authorize",
		s.TokenEndpoint():                  "https://trading.example.com/oauth/token",
		s.RegistrationEndpoint():           "https://trading.example.com/oauth/register",
		s.ProtectedResourceMetadataURL():   "https://trading.example.com/.well-known/oauth-protected-resource",
		s.AuthorizationServerMetadataURL(): "https://trading.example.com/.well-known/oauth-authorization-server",
	}
	for got, exp := range want {
		if got != exp {
			t.Errorf("derived URL %q, want %q", got, exp)
		}
	}
}

// Both metadata documents carry exactly the advertised field set, the
// no-store/CORS headers clients rely on, and refuse non-read methods.
func TestPRMHandler(t *testing.T) {
	h := testServer().PRMHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("headers: %v", rec.Header())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["resource"] != "https://trading.example.com/mcp" {
		t.Errorf("resource = %v", got["resource"])
	}
	if as, _ := got["authorization_servers"].([]any); len(as) != 1 || as[0] != "https://trading.example.com" {
		t.Errorf("authorization_servers = %v", got["authorization_servers"])
	}
	if bm, _ := got["bearer_methods_supported"].([]any); len(bm) != 1 || bm[0] != "header" {
		t.Errorf("bearer_methods_supported = %v", got["bearer_methods_supported"])
	}
	if sc, _ := got["scopes_supported"].([]any); len(sc) != 2 || sc[0] != ScopeMCP || sc[1] != ScopeOfflineAccess {
		t.Errorf("scopes_supported = %v", got["scopes_supported"])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/x", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status %d", rec.Code)
	}
}

func TestASMetadataHandler(t *testing.T) {
	s := testServer()
	h := s.ASMetadataHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("headers: %v", rec.Header())
	}
	var got ASMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := ASMetadata{
		Issuer:                            "https://trading.example.com",
		AuthorizationEndpoint:             "https://trading.example.com/oauth/authorize",
		TokenEndpoint:                     "https://trading.example.com/oauth/token",
		RegistrationEndpoint:              "https://trading.example.com/oauth/register",
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		ScopesSupported:                   []string{"mcp", "offline_access"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AS metadata = %+v, want %+v", got, want)
	}
	// No unexpected keys leak into the document (clients validate strictly).
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if len(raw) != 9 {
		t.Errorf("document has %d keys, want 9: %v", len(raw), raw)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/x", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD status %d body %d", rec.Code, rec.Body.Len())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status %d", rec.Code)
	}
}

func TestNewOpaque(t *testing.T) {
	full, sha, err := NewOpaque(PrefixAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, "tjo_") || len(full) != 4+43 {
		t.Errorf("token shape %q", full)
	}
	if strings.HasPrefix(full, "tj_") {
		t.Errorf("an OAuth token must never look like an API key: %q", full)
	}
	if sha != HashToken(full) || len(sha) != 64 {
		t.Errorf("hash mismatch")
	}
	full2, _, _ := NewOpaque(PrefixAccessToken)
	if full2 == full {
		t.Errorf("two mints must differ")
	}
}
