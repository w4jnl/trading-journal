package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	// No logout origin: form-action stays self-only.
	base := buildContentSecurityPolicy("")
	if !strings.Contains(base, "form-action 'self';") || strings.Contains(base, "form-action 'self' ") {
		t.Errorf("default form-action wrong: %q", base)
	}
	// With an origin, it is appended to form-action (and nowhere else).
	withSSO := buildContentSecurityPolicy("https://auth.example.com")
	if !strings.Contains(withSSO, "form-action 'self' https://auth.example.com;") {
		t.Errorf("SSO form-action missing: %q", withSSO)
	}
	if strings.Contains(withSSO, "default-src 'self' https://auth.example.com") {
		t.Errorf("origin leaked into another directive: %q", withSSO)
	}
}

// securityHeaders stamps the built policy on app responses and swaps to the
// stricter oiFileCSP under /oi/file/.
func TestSecurityHeadersUsesBuiltCSP(t *testing.T) {
	appCSP := buildContentSecurityPolicy("https://auth.example.com")
	h := securityHeaders(appCSP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != appCSP {
		t.Errorf("app CSP = %q, want the built policy", got)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("app X-Frame-Options = %q", rec.Header().Get("X-Frame-Options"))
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
