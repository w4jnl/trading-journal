package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// okHandler records whether the wrapped handler was reached.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// TestCSRFEnforcement covers the mutating-request guard: without a matching
// token a POST from an authenticated session must be rejected, and a correct
// token (in the form field or the HTMX header) must pass.
func TestCSRFEnforcement(t *testing.T) {
	c := NewCSRF([]byte("csrf-secret"))
	const sid = "session-id-abc"
	goodTok := c.Token(sid)

	sessionCookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	newPost := func(withCookie bool, form url.Values, header string) *http.Request {
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(http.MethodPost, "/journal/1", body)
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
		name   string
		req    *http.Request
		wantOK bool // handler reached (request allowed)
	}{
		{"safe GET bypasses", httptest.NewRequest(http.MethodGet, "/journal/1", nil), true},
		{"no session cookie passes to auth", newPost(false, nil, ""), true},
		{"session but no token rejected", newPost(true, nil, ""), false},
		{"session but wrong token rejected", newPost(true, url.Values{"csrf_token": {"wrong"}}, ""), false},
		{"correct token in form passes", newPost(true, url.Values{"csrf_token": {goodTok}}, ""), true},
		{"correct token in header passes", newPost(true, nil, goodTok), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			rec := httptest.NewRecorder()
			c.Middleware(okHandler(&reached)).ServeHTTP(rec, tc.req)
			if reached != tc.wantOK {
				t.Fatalf("reached=%v want %v (status %d)", reached, tc.wantOK, rec.Code)
			}
			if !tc.wantOK && rec.Code != http.StatusForbidden {
				t.Errorf("rejected request status = %d, want 403", rec.Code)
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
		var reached bool
		rec := httptest.NewRecorder()
		c.Middleware(okHandler(&reached)).ServeHTTP(rec, req)
		if !reached {
			t.Errorf("%s was blocked by CSRF (status %d); must be exempt", path, rec.Code)
		}
	}
}

// TestRequireAdmin rejects non-admins and missing users, admits admins.
func TestRequireAdmin(t *testing.T) {
	cases := []struct {
		name   string
		user   *User
		wantOK bool
	}{
		{"no user", nil, false},
		{"non-admin", &User{ID: 1, Username: "u", IsAdmin: false}, false},
		{"admin", &User{ID: 2, Username: "a", IsAdmin: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
			if tc.user != nil {
				req = req.WithContext(WithUser(req.Context(), tc.user))
			}
			rec := httptest.NewRecorder()
			RequireAdmin(okHandler(&reached)).ServeHTTP(rec, req)
			if reached != tc.wantOK {
				t.Fatalf("reached=%v want %v (status %d)", reached, tc.wantOK, rec.Code)
			}
			if !tc.wantOK && rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
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
			var reached bool
			req := httptest.NewRequest(http.MethodGet, "/api/v1/trades", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			a.Middleware(okHandler(&reached)).ServeHTTP(rec, req)
			if reached {
				t.Fatal("handler reached; malformed key must be rejected before dispatch")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}
