package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// okHandler is the terminal handler behind the middleware under test: reaching
// it yields 200, so rec.Code alone distinguishes "request allowed" (200) from
// "middleware rejected" (403/401 written by the middleware itself).
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// TestCSRFEnforcement covers the mutating-request guard: without a matching
// token a POST from an authenticated session must be rejected, and a correct
// token (in the form field or the HTMX header) must pass.
func TestCSRFEnforcement(t *testing.T) {
	c := NewCSRF([]byte("csrf-secret"))
	const sid = "session-id-abc"
	goodTok := c.Token(sid)

	sessionCookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	newPost := func(withCookie bool, form url.Values, header string) *http.Request {
		body := ""
		if form != nil {
			body = form.Encode()
		}
		req := httptest.NewRequest(http.MethodPost, "/journal/1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withCookie {
			req.AddCookie(sessionCookie)
		}
		if header != "" {
			req.Header.Set("X-CSRF-Token", header)
		}
		return req
	}

	cases := []struct {
		name string
		req  *http.Request
		want int // 200 = allowed through to the handler, 403 = CSRF-rejected
	}{
		{"safe GET bypasses", httptest.NewRequest(http.MethodGet, "/journal/1", nil), http.StatusOK},
		{"no session cookie passes to auth", newPost(false, nil, ""), http.StatusOK},
		{"session but no token rejected", newPost(true, nil, ""), http.StatusForbidden},
		{"session but wrong token rejected", newPost(true, url.Values{"csrf_token": {"wrong"}}, ""), http.StatusForbidden},
		{"correct token in form passes", newPost(true, url.Values{"csrf_token": {goodTok}}, ""), http.StatusOK},
		{"correct token in header passes", newPost(true, nil, goodTok), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.Middleware(okHandler).ServeHTTP(rec, tc.req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestCSRFExemptLoginLogout confirms /login and /logout mutate without a token
// (they own session fixation themselves; a stale cookie must not gate a fresh
// login).
func TestCSRFExemptLoginLogout(t *testing.T) {
	c := NewCSRF([]byte("csrf-secret"))
	for _, path := range []string{"/login", "/logout"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale"})
		rec := httptest.NewRecorder()
		c.Middleware(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s was blocked by CSRF (status %d); must be exempt", path, rec.Code)
		}
	}
}

// TestRequireAdmin rejects non-admins and missing users, admits admins.
func TestRequireAdmin(t *testing.T) {
	cases := []struct {
		name string
		user *User
		want int
	}{
		{"no user", nil, http.StatusForbidden},
		{"non-admin", &User{ID: 1, Username: "u", IsAdmin: false}, http.StatusForbidden},
		{"admin", &User{ID: 2, Username: "a", IsAdmin: true}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
			if tc.user != nil {
				req = req.WithContext(WithUser(req.Context(), tc.user))
			}
			rec := httptest.NewRecorder()
			RequireAdmin(okHandler).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestAPIKeyMissingOrMalformed covers the rejection paths that short-circuit
// before any DB lookup: a request with no bearer token, or a token lacking the
// "tj_" prefix, gets a JSON 401 and never reaches the handler. (A nil Queries
// proves no DB access happens on these paths.)
func TestAPIKeyMissingOrMalformed(t *testing.T) {
	a := &APIKeyAuth{Queries: nil}
	cases := []struct {
		name string
		auth string
	}{
		{"no header", ""},
		{"bearer non-tj token", "Bearer deadbeef"},
		{"raw non-tj token", "some-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/trades", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			a.Middleware(okHandler).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}
