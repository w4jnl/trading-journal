package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Generate makes fills-<token>@<domain>, a second generate replaces it, set
// stores a typed address (any domain, lower-cased, refused when another user
// has it or it is not an address), clear removes it; without the listener
// the route answers 404.
func TestSettingsMWAddress(t *testing.T) {
	fx := newPositionFixture(t)
	post := func(action string, extra ...string) *httptest.ResponseRecorder {
		body := "action=" + action
		for _, e := range extra {
			body += "&" + e
		}
		req := httptest.NewRequest(http.MethodPost, "/settings/mw-address", strings.NewReader(body)).WithContext(fx.ctx)
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

	// Set: the typed address is kept, lower-cased, whatever its domain.
	if rec := post("set", "address="+url.QueryEscape("  Fills-Jaro@Other.Example ")); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("set = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if got := fx.h.userMWFillAddress(fx.ctx); got != "fills-jaro@other.example" {
		t.Errorf("after set = %q", got)
	}
	for _, bad := range []string{"", "nobody", "two@at@x.example", "a b@x.example", "x@nodot"} {
		rec := post("set", "address="+url.QueryEscape(bad))
		if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "err=") {
			t.Errorf("set %q = %d %s, want an error redirect", bad, rec.Code, rec.Header().Get("Location"))
		}
	}
	if got := fx.h.userMWFillAddress(fx.ctx); got != "fills-jaro@other.example" {
		t.Errorf("a refused set changed the address to %q", got)
	}
	// Another user's address, in any case, is refused.
	other, err := fx.q.CreateUser(fx.ctx, models.CreateUserParams{Username: "second"})
	if err != nil {
		t.Fatal(err)
	}
	taken := "fills-taken@other.example"
	if err := fx.q.SetUserMWFillAddress(fx.ctx, models.SetUserMWFillAddressParams{MwFillAddress: &taken, ID: other.ID}); err != nil {
		t.Fatal(err)
	}
	rec := post("set", "address=Fills-TAKEN@other.example")
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Errorf("taken address accepted: %s", rec.Header().Get("Location"))
	}
}
