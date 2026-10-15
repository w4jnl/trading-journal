package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

var pairCodeRe = regexp.MustCompile(`data-copy="([A-Z0-9]{4}-[A-Z0-9]{4})"`)

// pairPost posts a pairing body without any user in the context, as the
// phone does.
func pairPost(h *Handlers, body string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pair", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:4444"
	rec := httptest.NewRecorder()
	h.APIPair(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// Settings makes a code and shows it as a QR; the phone exchanges it once
// for a device key; the key authenticates as the user with kind device;
// the code cannot be used again; the settings page lists the phone apart
// from the API keys.
func TestPairFlow(t *testing.T) {
	fx := newPositionFixture(t)
	fx.h.PairLimiter = auth.NewAttemptLimiter()
	fx.h.ExternalURL = "https://journal.example.com/"
	req := httptest.NewRequest(http.MethodPost, "/settings/devices/pair", nil).WithContext(WithNonce(fx.ctx, "n"))
	rec := httptest.NewRecorder()
	fx.h.SettingsPairStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pair start = %d: %s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	m := pairCodeRe.FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "<svg") || !strings.Contains(page, "tj://pair?server=https://journal.example.com&amp;code="+m[1]) {
		t.Fatalf("settings page lacks the code, the QR or the URL")
	}
	code := m[1]

	// Lower case without the dash is accepted.
	status, out := pairPost(fx.h, `{"code": "`+strings.ToLower(strings.ReplaceAll(code, "-", ""))+`", "deviceName": "the trader's iPhone"}`)
	if status != http.StatusCreated {
		t.Fatalf("pair = %d: %v", status, out)
	}
	key, _ := out["key"].(string)
	if !strings.HasPrefix(key, "tj_") || out["server"].(map[string]any)["url"] != "https://journal.example.com" || out["user"].(map[string]any)["username"] != "test" {
		t.Fatalf("pair response = %v", out)
	}
	if status, _ := pairPost(fx.h, `{"code": "`+code+`"}`); status != http.StatusBadRequest {
		t.Errorf("a used code paired again: %d", status)
	}

	// The key works through the API middleware and reads as a device key.
	mw := (&auth.APIKeyAuth{Queries: fx.q}).Middleware(http.HandlerFunc(fx.h.APIMe))
	me := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	me.Header.Set("Authorization", "Bearer "+key)
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, me)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}
	var meOut map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &meOut)
	k := meOut["key"].(map[string]any)
	if k["kind"] != "device" || k["name"] != "the trader's iPhone" || meOut["settings"].(map[string]any)["autoPlan"] != 0.0 {
		t.Errorf("me = %v", meOut)
	}

	// The settings page lists the phone under Phone app, not under API keys.
	req = httptest.NewRequest(http.MethodGet, "/settings", nil).WithContext(WithNonce(fx.ctx, "n"))
	rec = httptest.NewRecorder()
	fx.h.SettingsPage(rec, req)
	page = rec.Body.String()
	if !strings.Contains(page, "the trader&#39;s iPhone") || !strings.Contains(page, ">paired<") || !strings.Contains(page, "No API keys yet.") {
		t.Errorf("settings page does not separate the phone from the API keys")
	}
}

// A wrong code, an expired code and a disabled user all fail alike, and
// an address that keeps failing is told to wait.
func TestPairRefusals(t *testing.T) {
	fx := newPositionFixture(t)
	fx.h.PairLimiter = auth.NewAttemptLimiter()
	ctx := context.Background()
	if _, err := fx.q.CreatePairCode(ctx, models.CreatePairCodeParams{UserID: fx.uid, CodeSha256: auth.HashAPIKey("OLDC-ODE1"), ExpiresAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{`{"code": "OLDC-ODE1"}`, `{"code": "NOPE-NOPE"}`, `{"code": "x"}`, `not json`, `{"code": "NOPE-NOPE"}`} {
		if status, out := pairPost(fx.h, body); status != http.StatusBadRequest {
			t.Errorf("attempt %d = %d %v", i, status, out)
		}
	}
	// The sixth failure in a row locks the address.
	if status, _ := pairPost(fx.h, `{"code": "NOPE-NOPE"}`); status != http.StatusBadRequest {
		t.Fatalf("sixth attempt should still be judged")
	}
	if status, _ := pairPost(fx.h, `{"code": "NOPE-NOPE"}`); status != http.StatusTooManyRequests {
		t.Errorf("after six failures the address is not throttled: %d", status)
	}
}

// Settings are the device's to change: an agent key is refused; the
// switches change and come back; a bad mode is refused.
func TestAPISettingsSave(t *testing.T) {
	fx := newPositionFixture(t)
	put := func(ctx context.Context, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		fx.h.APISettingsSave(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	agent := auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindAgent})
	device := auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindDevice})
	if status, _ := put(agent, `{"guardrail": true}`); status != http.StatusForbidden {
		t.Errorf("agent key = %d, want 403", status)
	}
	if status, _ := put(fx.ctx, `{"guardrail": true}`); status != http.StatusForbidden {
		t.Errorf("no key info = %d, want 403", status)
	}
	status, out := put(device, `{"guardrail": true, "autoPlan": 2}`)
	if status != http.StatusOK || out["settings"].(map[string]any)["guardrail"] != true || out["settings"].(map[string]any)["autoPlan"] != 2.0 {
		t.Fatalf("device put = %d %v", status, out)
	}
	u, _ := fx.q.GetUserByID(context.Background(), fx.uid)
	if u.Guardrail != 1 || u.AutoPlan != 2 {
		t.Errorf("stored = guardrail %d autoPlan %d", u.Guardrail, u.AutoPlan)
	}
	if status, _ := put(device, `{"autoPlan": 3}`); status != http.StatusBadRequest {
		t.Errorf("autoPlan 3 = %d", status)
	}
	if status, out := put(device, `{"guardrail": false}`); status != http.StatusOK || out["settings"].(map[string]any)["autoPlan"] != 2.0 {
		t.Errorf("absent field changed: %d %v", status, out)
	}
}

// A code as typed is normalised; anything else is rejected before lookup.
func TestNormalizePairCode(t *testing.T) {
	for in, want := range map[string]string{"ab7q-k2mn": "AB7Q-K2MN", " AB7Q K2MN ": "AB7Q-K2MN", "ab7qk2mn": "AB7Q-K2MN", "AB7Q-K2M": "", "AB7Q-K2MN1": "", "ab7q-k2m!": ""} {
		if got := normalizePairCode(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
