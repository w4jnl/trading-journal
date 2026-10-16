package handlers

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Hiding or showing an account in the stats moves every default aggregate
// without a trade moving, so the save must bump the live version; a save
// that changes nothing must not.
func TestAccountVisibilitySaveBumpsLiveVersion(t *testing.T) {
	fx := newPositionFixture(t)
	post := func(form string) int {
		req := httptest.NewRequest(http.MethodPost, "/settings/accounts/visibility", strings.NewReader(form)).WithContext(fx.ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		fx.h.SettingsAccountVisibilitySave(rec, req)
		return rec.Code
	}
	id := strconv.FormatInt(fx.acct.ID, 10)

	before := fx.h.LiveData.Version(fx.uid)
	if code := post(""); code != http.StatusSeeOther { // nothing included: the account is hidden
		t.Fatalf("hide = %d", code)
	}
	if v := fx.h.LiveData.Version(fx.uid); v != before+1 {
		t.Fatalf("hiding the account: version %d, want %d", v, before+1)
	}
	if a, err := fx.q.GetAccountByID(fx.ctx, models.GetAccountByIDParams{ID: fx.acct.ID, UserID: fx.uid}); err != nil || a.ExcludedFromStats != 1 {
		t.Fatalf("account not hidden: %+v %v", a, err)
	}

	if code := post(""); code != http.StatusSeeOther {
		t.Fatalf("hide again = %d", code)
	}
	if v := fx.h.LiveData.Version(fx.uid); v != before+1 {
		t.Fatalf("an unchanged save bumped the version to %d", v)
	}

	if code := post("include=" + id); code != http.StatusSeeOther {
		t.Fatalf("show = %d", code)
	}
	if v := fx.h.LiveData.Version(fx.uid); v != before+2 {
		t.Fatalf("showing the account: version %d, want %d", v, before+2)
	}
}
