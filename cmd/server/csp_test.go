package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/handlers"
)

func TestCSPOrigin(t *testing.T) {
	cases := map[string]string{
		"https://auth.example.com/logout?rd=https%3A%2F%2Ftradingtest.example.com%2F": "https://auth.example.com",
		"http://auth.local:9091/logout":                                     "http://auth.local:9091",
		"":                                                                  "", // unset -> same-origin only
		"/logout":                                                           "", // relative -> covered by 'self'
		"not a url":                                                         "",
	}
	for in, want := range cases {
		if got := cspOrigin(in); got != want {
			t.Errorf("cspOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildContentSecurityPolicy(t *testing.T) {
	// No logout origin: form-action stays self-only. Scripts: self + nonce,
	// never 'unsafe-inline'.
	base := buildContentSecurityPolicy("", "n0nce")
	if !strings.Contains(base, "form-action 'self';") || strings.Contains(base, "form-action 'self' ") {
		t.Errorf("default form-action wrong: %q", base)
	}
	if !strings.Contains(base, "script-src 'self' 'nonce-n0nce';") {
		t.Errorf("script-src wrong: %q", base)
	}
	if i := strings.Index(base, "script-src"); strings.Contains(base[i:i+strings.Index(base[i:], ";")], "unsafe-inline") {
		t.Errorf("script-src allows inline: %q", base)
	}
	// With an origin, it is appended to form-action (and nowhere else).
	withSSO := buildContentSecurityPolicy("https://auth.example.com", "n0nce")
	if !strings.Contains(withSSO, "form-action 'self' https://auth.example.com;") {
		t.Errorf("SSO form-action missing: %q", withSSO)
	}
	if strings.Contains(withSSO, "default-src 'self' https://auth.example.com") {
		t.Errorf("origin leaked into another directive: %q", withSSO)
	}
}

// securityHeaders mints a nonce per request, puts it in the policy and in
// the request context (where renderPage reads it), and swaps to the
// stricter oiFileCSP under /oi/file/.
func TestSecurityHeadersUsesBuiltCSP(t *testing.T) {
	var seen string
	h := securityHeaders("https://auth.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = handlers.NonceFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard", nil))
	got := rec.Header().Get("Content-Security-Policy")
	if seen == "" || !strings.Contains(got, "script-src 'self' 'nonce-"+seen+"';") {
		t.Errorf("app CSP = %q, context nonce = %q", got, seen)
	}
	if got != buildContentSecurityPolicy("https://auth.example.com", seen) {
		t.Errorf("app CSP = %q, want the built policy", got)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("app X-Frame-Options = %q", rec.Header().Get("X-Frame-Options"))
	}
	first := seen
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard", nil))
	if seen == first {
		t.Errorf("nonce reused across requests: %q", seen)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/oi/file/dashboard-2026-09-04.html", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != oiFileCSP {
		t.Errorf("oi-file CSP = %q, want the strict policy", got)
	}
	if rec.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("oi-file X-Frame-Options = %q", rec.Header().Get("X-Frame-Options"))
	}
}
