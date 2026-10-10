package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiterLocksAfterFreeFailures(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	key := "alice|10.0.0.9"

	for i := 0; i < loginFreeFailures-1; i++ {
		l.fail(key)
		if w := l.retryAfter(key); w != 0 {
			t.Fatalf("locked after %d failures (%v), want free allowance of %d", i+1, w, loginFreeFailures)
		}
	}
	l.fail(key) // the 5th failure locks
	if w := l.retryAfter(key); w != loginBaseLock {
		t.Fatalf("first lock = %v, want %v", w, loginBaseLock)
	}
	// Each further failure doubles the lock, capped.
	l.fail(key)
	if w := l.retryAfter(key); w != 2*loginBaseLock {
		t.Errorf("second lock = %v, want %v", w, 2*loginBaseLock)
	}
	for i := 0; i < 12; i++ {
		l.fail(key)
	}
	if w := l.retryAfter(key); w != loginMaxLock {
		t.Errorf("capped lock = %v, want %v", w, loginMaxLock)
	}
	// Time passes: the lock expires on its own.
	now = now.Add(loginMaxLock + time.Second)
	if w := l.retryAfter(key); w != 0 {
		t.Errorf("lock did not expire: %v", w)
	}
	// Another user / address is independent; success clears the key.
	if w := l.retryAfter("bob|10.0.0.9"); w != 0 {
		t.Errorf("unrelated key locked: %v", w)
	}
	l.reset(key)
	l.fail(key)
	if w := l.retryAfter(key); w != 0 {
		t.Errorf("reset did not clear the failure count: %v", w)
	}
}

// PostLogin consults the limiter before bcrypt: a locked key gets 429 with a
// Retry-After and never reaches the password check (no DB needed here).
func TestPostLoginThrottled(t *testing.T) {
	a := &LocalAuth{}
	key := "alice|192.0.2.10"
	for i := 0; i < loginFreeFailures; i++ {
		a.limits().fail(key)
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=Alice&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.10:51234"
	rec := httptest.NewRecorder()
	a.PostLogin(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "Too many failed attempts") {
		t.Errorf("throttled response lacks Retry-After / message: %q %q", rec.Header().Get("Retry-After"), rec.Body.String())
	}
	// A different address for the same username is not locked out.
	req2 := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=alice&password=x"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.RemoteAddr = "192.0.2.11:51234"
	if w := a.limits().retryAfter("alice|192.0.2.11"); w != 0 {
		t.Errorf("other address locked: %v", w)
	}
}
