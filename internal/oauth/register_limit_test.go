package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterRateLimitPerAddress(t *testing.T) {
	s := &Server{}
	for i := 0; i < registerRateMax; i++ {
		if !s.registerAllowed("203.0.113.5") {
			t.Fatalf("registration %d refused inside the budget", i+1)
		}
	}
	if s.registerAllowed("203.0.113.5") {
		t.Error("registration past the budget allowed")
	}
	if !s.registerAllowed("203.0.113.6") {
		t.Error("another address shares the budget")
	}

	// The handler answers 429 before reading the body.
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["https://x.example/cb"]}`))
	req.RemoteAddr = "203.0.113.5:4444"
	rec := httptest.NewRecorder()
	s.RegisterHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("over-budget register: code=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if got := clientAddr(req); got != "203.0.113.5" {
		t.Errorf("clientAddr = %q", got)
	}
}
