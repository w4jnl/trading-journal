package handlers

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// A position on an account hidden in Settings (excluded_from_stats) is no
// position to any view: it leaves the open-position lists and the badge,
// Today's unrealised P&L and the Session activity, and comes back when the
// account is shown again.
func TestHiddenAccountPositionsLeaveEveryView(t *testing.T) {
	fx := newPositionFixture(t)
	fx.h.quoteCache.Store("ESM6", cachedQuote{q: quote{Price: 7261, At: time.Now(), Source: "test"}, fetched: time.Now()})
	setHidden := func(hidden int64) {
		t.Helper()
		if err := fx.q.SetAccountExcluded(fx.ctx, models.SetAccountExcludedParams{ExcludedFromStats: hidden, ID: fx.acct.ID, UserID: fx.uid}); err != nil {
			t.Fatal(err)
		}
	}

	code, out := apiGet(fx.ctx, fx.h.APIToday, "/api/v1/today")
	if code != http.StatusOK || len(out["positions"].([]any)) != 1 || out["unrealized"].(map[string]any)["total"].(float64) <= 0 {
		t.Fatalf("visible account: today = %d %v", code, out)
	}
	if body := fx.trades(t, "?status=open"); !strings.Contains(body, "ESM6") {
		t.Fatal("visible account: the trades page does not list the position")
	}

	setHidden(1)
	if rows, err := fx.q.ListOpenPositions(fx.ctx, fx.uid); err != nil || len(rows) != 0 {
		t.Errorf("hidden account: open positions = %v %v", rows, err)
	}
	if n, err := fx.q.CountOpenPositions(fx.ctx, fx.uid); err != nil || n != 0 {
		t.Errorf("hidden account: badge = %d %v", n, err)
	}
	_, out = apiGet(fx.ctx, fx.h.APIToday, "/api/v1/today")
	unreal := out["unrealized"].(map[string]any)
	if len(out["positions"].([]any)) != 0 || unreal["total"] != 0.0 || unreal["priced"] != 0.0 || out["lastFillAt"] != nil {
		t.Errorf("hidden account: today = %v", out)
	}
	if st, err := fx.h.SessionState(fx.ctx, fx.uid); err != nil || st.OpenCount != 0 || st.Unrealized != 0 || len(st.Positions) != 0 {
		t.Errorf("hidden account: session = %+v %v", st, err)
	}
	if body := fx.trades(t, "?status=open"); strings.Contains(body, "ESM6") {
		t.Error("hidden account: the trades page still lists the position")
	}

	setHidden(0)
	if rows, err := fx.q.ListOpenPositions(fx.ctx, fx.uid); err != nil || len(rows) != 1 {
		t.Errorf("shown again: open positions = %v %v", rows, err)
	}
}
