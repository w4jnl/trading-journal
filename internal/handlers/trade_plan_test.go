package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The plan endpoint: risk derived from the stop through the instrument's
// ticks (ES: 2 points x 50/pt x qty 2 = 200), a typed risk wins, an empty
// form clears, and another user's trade is a 404.
func TestTradePlanSave(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO users (id, username) VALUES (2, 'other')`,
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES
			(1, ` + itoa64(uid) + `, 'mine', 'motivewave_csv', 'A-1'), (2, 2, 'theirs', 'motivewave_csv', 'A-2')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'ESU6', 'ES', 'long', 2, 7250, 7253, '2026-10-08 14:00:00 +0000 UTC', '2026-10-08 15:00:00 +0000 UTC', 'closed', 300),
			(20, 2, 'ESU6', 'ES', 'long', 1, 7250, 7253, '2026-10-08 14:00:00 +0000 UTC', '2026-10-08 15:00:00 +0000 UTC', 'closed', 150)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})
	post := func(id string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/trades/"+id+"/plan", strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.TradePlanSave(rec, req)
		return rec
	}
	plan := func() (stop, risk *float64) {
		t.Helper()
		if err := conn.QueryRow(`SELECT planned_stop, planned_risk FROM trades WHERE id = 10`).Scan(&stop, &risk); err != nil {
			t.Fatal(err)
		}
		return
	}

	if rec := post("10", url.Values{"stop": {"7248"}}); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("HX-Trigger"), "Plan saved") {
		t.Fatalf("stop only -> %d %q", rec.Code, rec.Header().Get("HX-Trigger"))
	}
	if stop, risk := plan(); stop == nil || *stop != 7248 || risk == nil || *risk != 200 {
		t.Fatalf("after stop: stop=%v risk=%v, want 7248 / 200", stop, risk)
	}
	// Risk alone derives the stop on the losing side, snapped to the tick:
	// 250 over 50/pt x qty 2 = 2.5 pts below the entry.
	if rec := post("10", url.Values{"risk": {"250"}}); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("HX-Trigger"), "stop set to 7247.5 from the risk") {
		t.Fatalf("risk only -> %d %q", rec.Code, rec.Header().Get("HX-Trigger"))
	}
	if stop, risk := plan(); stop == nil || *stop != 7247.5 || risk == nil || *risk != 250 {
		t.Fatalf("after risk only: stop=%v risk=%v, want 7247.5 / 250", stop, risk)
	}
	if rec := post("10", url.Values{"stop": {"7248"}, "risk": {"150"}}); rec.Code != http.StatusOK {
		t.Fatalf("typed risk -> %d", rec.Code)
	}
	if stop, risk := plan(); stop == nil || *stop != 7248 || risk == nil || *risk != 150 {
		t.Fatalf("typed both not kept as typed: stop=%v risk=%v", stop, risk)
	}
	// Both fields come back on every save: the one that changed wins and
	// the other follows. Stop moved to 7247 with the risk untouched -> 300.
	if rec := post("10", url.Values{"stop": {"7247"}, "risk": {"150"}}); rec.Code != http.StatusOK {
		t.Fatalf("edit stop -> %d", rec.Code)
	}
	if stop, risk := plan(); stop == nil || *stop != 7247 || risk == nil || *risk != 300 {
		t.Fatalf("after editing the stop: stop=%v risk=%v, want 7247 / 300", stop, risk)
	}
	// Risk edited to 100 with the stop untouched -> stop 1 pt below.
	if rec := post("10", url.Values{"stop": {"7247"}, "risk": {"100"}}); rec.Code != http.StatusOK {
		t.Fatalf("edit risk -> %d", rec.Code)
	}
	if stop, risk := plan(); stop == nil || *stop != 7249 || risk == nil || *risk != 100 {
		t.Fatalf("after editing the risk: stop=%v risk=%v, want 7249 / 100", stop, risk)
	}
	// Both changed at once: both kept.
	if rec := post("10", url.Values{"stop": {"7246"}, "risk": {"500"}}); rec.Code != http.StatusOK {
		t.Fatalf("edit both -> %d", rec.Code)
	}
	if stop, risk := plan(); stop == nil || *stop != 7246 || risk == nil || *risk != 500 {
		t.Fatalf("after editing both: stop=%v risk=%v, want 7246 / 500", stop, risk)
	}
	// Re-saving the form untouched changes nothing.
	if rec := post("10", url.Values{"stop": {"7246"}, "risk": {"500.00"}}); rec.Code != http.StatusOK {
		t.Fatalf("resave -> %d", rec.Code)
	}
	if stop, risk := plan(); *stop != 7246 || *risk != 500 {
		t.Fatalf("untouched resave moved the plan: stop=%v risk=%v", *stop, *risk)
	}
	if rec := post("10", url.Values{"stop": {"-1"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative stop -> %d, want 400", rec.Code)
	}
	if rec := post("10", url.Values{}); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("HX-Trigger"), "Plan cleared") {
		t.Fatalf("clear -> %d %q", rec.Code, rec.Header().Get("HX-Trigger"))
	}
	if stop, risk := plan(); stop != nil || risk != nil {
		t.Fatalf("not cleared: %v %v", stop, risk)
	}
	if rec := post("20", url.Values{"stop": {"7248"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("another user's trade -> %d, want 404", rec.Code)
	}
}
