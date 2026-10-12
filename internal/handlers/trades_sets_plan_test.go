package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The set plan: one stop price on every leg with each leg's risk from its
// own size and contract (MES at 5 a point next to ES at 50), a total risk
// split by exposure into the same distance per leg, the edited side wins,
// an empty form clears, and a fill beyond the stop is flagged.
func TestTradeSetPlanSave(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := strconv.FormatInt(uid, 10)
	// A short across three accounts: 2 MES from 7863.25, 1 MES from 7863.75, 1 ES from 7863.25.
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'alpha', 'motivewave_csv', 'A'), (2, ` + u + `, 'bravo', 'motivewave_csv', 'B'), (3, ` + u + `, 'charlie', 'motivewave_csv', 'C')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl) VALUES
			(10, 1, 'MESZ6', 'MES', 'short', 2, 7863.25, 7862.25, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:01:00 +0000 UTC', 'closed', 10, 2, 8),
			(11, 2, 'MESZ6', 'MES', 'short', 1, 7863.75, 7862.25, '2026-10-09 14:00:01 +0000 UTC', '2026-10-09 14:01:05 +0000 UTC', 'closed', 7.5, 1, 6.5),
			(12, 3, 'ESZ6', 'ES', 'short', 1, 7863.25, 7862.25, '2026-10-09 14:00:01 +0000 UTC', '2026-10-09 14:01:02 +0000 UTC', 'closed', 50, 4, 46)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	q := models.New(conn)
	if n, err := copyset.Rebuild(context.Background(), q, uid); err != nil || n != 1 {
		t.Fatalf("rebuild sets = %d, %v", n, err)
	}
	var sid int64
	if err := conn.QueryRow(`SELECT id FROM trade_sets`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(sid, 10)
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/trades/sets/"+id+"/plan", strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.TradeSetPlanSave(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("plan %v -> %d: %s", form, rec.Code, rec.Body.String())
		}
		return rec
	}
	plan := func(tradeID int64) (stop, risk *float64) {
		t.Helper()
		if err := conn.QueryRow(`SELECT planned_stop, planned_risk FROM trades WHERE id = ?`, tradeID).Scan(&stop, &risk); err != nil {
			t.Fatal(err)
		}
		return
	}
	f := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}

	// Stop mode: 7865.75 on every leg. MES 2-lot: 2.5 pts x 5 x 2 = 25;
	// MES 1-lot from 7863.75: 2 pts x 5 = 10; ES: 2.5 x 50 = 125.
	rec := post(url.Values{"stop": {"7865.75"}})
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "Stop 7865.75 on 3 legs; risk 160.00 in total") {
		t.Errorf("stop toast = %q", rec.Header().Get("HX-Trigger"))
	}
	for _, c := range []struct {
		id         int64
		stop, risk float64
	}{{10, 7865.75, 25}, {11, 7865.75, 10}, {12, 7865.75, 125}} {
		if s, r := plan(c.id); f(s) != c.stop || f(r) != c.risk {
			t.Errorf("trade %d after stop: stop %v risk %v, want %v / %v", c.id, f(s), f(r), c.stop, c.risk)
		}
	}
	body := rec.Body.String()
	for _, want := range []string{`name="stop" value="7865.75"`, `name="risk" value="160.00"`, `data-plan-point-value="65"`, `<td class="tabular num">125.00</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet after stop missing %q", want)
		}
	}

	// Risk mode (stop untouched, risk changed): 325 over exposure 65 = 5 pts
	// on every leg, snapped to the tick: stops 7868.25 / 7868.75 / 7868.25.
	rec = post(url.Values{"stop": {"7865.75"}, "risk": {"325"}})
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "Risk 325.00 split by exposure: stops 7868.25 to 7868.75 (325.00 after tick rounding)") {
		t.Errorf("risk toast = %q", rec.Header().Get("HX-Trigger"))
	}
	for _, c := range []struct {
		id         int64
		stop, risk float64
	}{{10, 7868.25, 50}, {11, 7868.75, 25}, {12, 7868.25, 250}} {
		if s, r := plan(c.id); f(s) != c.stop || f(r) != c.risk {
			t.Errorf("trade %d after risk: stop %v risk %v, want %v / %v", c.id, f(s), f(r), c.stop, c.risk)
		}
	}
	// Mixed stops: the field is empty with the range as placeholder.
	if body := rec.Body.String(); !strings.Contains(body, `name="stop" value="" placeholder="7868.25 to 7868.75"`) {
		t.Error("mixed stops should show as a placeholder range")
	}

	// Stop edited while the risk is resent unchanged: the stop wins.
	post(url.Values{"stop": {"7870"}, "risk": {"325.00"}})
	if s, r := plan(12); f(s) != 7870 || f(r) != 337.5 {
		t.Errorf("stop edit: ES stop %v risk %v, want 7870 / 337.5", f(s), f(r))
	}

	// A straggler filled beyond the stop is flagged, not silently priced.
	if _, err := conn.Exec(`UPDATE trades SET entry_price = 7871 WHERE id = 11`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/trades/sets/"+id, nil).WithContext(ctx)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	h.TradeSetDetail(rec, req)
	if !strings.Contains(rec.Body.String(), `title="entry beyond the stop">!</span>`) {
		t.Error("sheet should flag the leg whose entry sits beyond its stop")
	}

	// Empty form clears every leg.
	rec = post(url.Values{})
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "Plan cleared on every leg") {
		t.Errorf("clear toast = %q", rec.Header().Get("HX-Trigger"))
	}
	for _, tid := range []int64{10, 11, 12} {
		if s, r := plan(tid); s != nil || r != nil {
			t.Errorf("trade %d not cleared: %v %v", tid, s, r)
		}
	}
}
