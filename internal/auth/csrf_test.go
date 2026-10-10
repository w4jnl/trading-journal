package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Proxy mode (Authelia) never issues the local session cookie, so the token
// check cannot run; the middleware must then fall back to the browser's own
// origin evidence instead of waving every POST through.
func TestCSRFWithoutSessionUsesOriginEvidence(t *testing.T) {
	c := NewCSRF([]byte("secret"))
	var reached bool
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	post := func(hdr map[string]string) int {
		reached = false
		req := httptest.NewRequest(http.MethodPost, "https://trading.example/accounts/1/reassign", strings.NewReader("to=2"))
		req.Host = "trading.example"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cases := []struct {
		name string
		hdr  map[string]string
		ok   bool
	}{
		{"same-origin fetch metadata", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"typed URL / bookmark", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"sibling subdomain behind the same SSO", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.trading.example"}, false},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"origin matches host", map[string]string{"Origin": "https://trading.example"}, true},
		{"origin matches forwarded host", map[string]string{"Origin": "https://public.example", "X-Forwarded-Host": "public.example"}, true},
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, false},
		{"opaque origin", map[string]string{"Origin": "null"}, false},
		{"referer matches", map[string]string{"Referer": "https://trading.example/accounts"}, true},
		{"foreign referer", map[string]string{"Referer": "https://evil.example/x"}, false},
		{"no browser metadata at all (curl)", nil, true},
	}
	for _, tc := range cases {
		code := post(tc.hdr)
		if tc.ok && (code != http.StatusOK || !reached) {
			t.Errorf("%s: code=%d reached=%v, want pass", tc.name, code, reached)
		}
		if !tc.ok && (code != http.StatusForbidden || reached) {
			t.Errorf("%s: code=%d reached=%v, want 403 and not reached", tc.name, code, reached)
		}
	}

	// With a session cookie the token rule applies as before: a cross-site
	// request that somehow carries the cookie still needs the token.
	req := httptest.NewRequest(http.MethodPost, "https://trading.example/x", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "sess-1"})
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	reached = false
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || reached {
		t.Errorf("session without token: code=%d reached=%v, want 403", rec.Code, reached)
	}
	req.Header.Set("X-CSRF-Token", c.Token("sess-1"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reached {
		t.Errorf("session with token: code=%d reached=%v, want pass", rec.Code, reached)
	}
}

func TestSafeLocalPath(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"/", true},
		{"/trades?from=2026-01-01&to=2026-01-31", true},
		{"/journal/2026-07-31#notes", true},
		{"/a%2F%2Fb", true}, // percent-encoded slashes stay a path
		{"", false},
		{"trades", false},
		{"//evil.example/x", false},
		{`/\evil.example`, false}, // browsers read "/\" as "//"
		{`/trades\..\x`, false},
		{"http://evil.example", false},
		{"https://evil.example/", false},
		{"javascript:alert(1)", false},
		{"/x\r\nSet-Cookie: a=b", false},
		{"/x\x00", false},
	} {
		if got := SafeLocalPath(tc.in); got != tc.ok {
			t.Errorf("SafeLocalPath(%q) = %v, want %v", tc.in, got, tc.ok)
		}
	}
	if got := nextOr("/", `/\evil.example`); got != "/" {
		t.Errorf("nextOr accepted a backslash path: %q", got)
	}
	if got := nextOr("/", "/dashboard"); got != "/dashboard" {
		t.Errorf("nextOr rejected a plain path: %q", got)
	}
}
