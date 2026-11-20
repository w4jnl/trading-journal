package handlers

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// Generate makes fills-<token>@<domain>, a second generate replaces it, clear
// removes it; without the listener the route answers 404.
func TestSettingsMWAddress(t *testing.T) {
	fx := newPositionFixture(t)
	post := func(action string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/settings/mw-address", strings.NewReader("action="+action)).WithContext(fx.ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		fx.h.SettingsMWAddressSave(rec, req)
		return rec
	}
	if rec := post("generate"); rec.Code != http.StatusNotFound {
		t.Fatalf("listener off: %d, want 404", rec.Code)
	}
	fx.h.MWFillDomain = "w4j.example"
	if rec := post("generate"); rec.Code != http.StatusSeeOther {
		t.Fatalf("generate = %d %s", rec.Code, rec.Body)
	}
	first := fx.h.userMWFillAddress(fx.ctx)
	if !regexp.MustCompile(`^fills-[a-z2-7]{10}@w4j\.example$`).MatchString(first) {
		t.Fatalf("address = %q", first)
	}
	post("generate")
	if second := fx.h.userMWFillAddress(fx.ctx); second == first || second == "" {
		t.Errorf("regenerate kept %q", second)
	}
	if rec := post("bogus"); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus action = %d", rec.Code)
	}
	post("clear")
	if got := fx.h.userMWFillAddress(fx.ctx); got != "" {
		t.Errorf("after clear = %q", got)
	}
}
