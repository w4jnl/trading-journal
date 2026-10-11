package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Risk on the trades overview: the risk filter (planned / past the stop),
// the footer aggregates over the filtered set, the past-stop dot and the
// path glyph on the rows.
func TestTradesRiskOverview(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + itoa64(uid) + `, 'mine', 'motivewave_csv', 'A-1')`,
		// A: planned risk 100, heat 150 (past the stop), best 500, won 396 net / 400 gross.
		// B: no plan, measured (heat 20, best 60), lost. C: nothing measured, no plan.
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl, planned_risk, mae_points, mfe_points, mae_pnl, mfe_pnl, excursion_tf) VALUES
			(10, 1, 'ESZ6', 'ES', 'long', 2, 7250, 7254, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:05:00 +0000 UTC', 'closed', 400, 4, 396, 100, 3, 5, 300, 500, '1m'),
			(11, 1, 'ESZ6', 'ES', 'short', 1, 7260, 7261, '2026-10-09 15:00:00 +0000 UTC', '2026-10-09 15:05:00 +0000 UTC', 'closed', -50, 2, -52, NULL, 1, 1.2, 50, 60, '1m'),
			(12, 1, 'NQZ6', 'NQ', 'long', 1, 20000, 20010, '2026-10-09 16:00:00 +0000 UTC', '2026-10-09 16:05:00 +0000 UTC', 'closed', 200, 2, 198, NULL, NULL, NULL, NULL, NULL, NULL)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	filters := func(qs string) tradeFilters {
		t.Helper()
		f, err := h.parseTradeFilters(httptest.NewRequest(http.MethodGet, "/trades"+qs, nil).WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	ids := func(f tradeFilters) []int64 {
		t.Helper()
		rows, err := listTradesFiltered(ctx, conn, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}

	if got := ids(filters("?risk=planned")); len(got) != 1 || got[0] != 10 {
		t.Errorf("risk=planned -> %v, want [10]", got)
	}
	if got := ids(filters("?risk=past-stop")); len(got) != 1 || got[0] != 10 {
		t.Errorf("risk=past-stop -> %v, want [10]", got)
	}
	if got := ids(filters("?risk=bogus")); len(got) != 3 {
		t.Errorf("unknown risk value should not filter, got %v", got)
	}
	if f := filters("?risk=planned"); filtersAsForm(f)["Risk"] != "planned" {
		t.Error("filtersAsForm does not echo the risk filter")
	}

	tt, err := totalsTradesFiltered(ctx, conn, filters(""))
	if err != nil {
		t.Fatal(err)
	}
	// Planned 1 (A, R = 396/100), past stop 1 (heat 300 > 100); measured A
	// and B: gross 400 - 50 over best 500 + 60 = 62.5% kept.
	if tt.Planned != 1 || tt.RCount != 1 || tt.AvgR() != 3.96 || tt.PastStop != 1 || tt.Measured != 2 || tt.MeasuredGross != 350 || tt.MeasuredMfe != 560 || tt.ExitKept() != 350.0/560 {
		t.Errorf("totals = %+v (avgR %v, kept %v)", tt, tt.AvgR(), tt.ExitKept())
	}

	rows, _ := listTradesFiltered(ctx, conn, filters("?sort=entry&dir=asc"))
	a, b, c := rows[0], rows[1], rows[2]
	if !a.PastStop() || b.PastStop() || c.PastStop() {
		t.Errorf("past stop: a=%v b=%v c=%v, want only a", a.PastStop(), b.PastStop(), c.PastStop())
	}
	// A: scale 5 pts; heat 3/5 of the half = 30%, gain 50%, exit +4 pts = 50+40 = 90%.
	if p := a.Path(); p == nil || p.HeatPct != 30 || p.GainPct != 50 || p.ExitPct != 90 || p.Sign != "pnl-pos" || !strings.Contains(p.Title, "exit kept 80%") || !strings.Contains(p.Title, "(1m bars)") {
		t.Errorf("path a = %+v", p)
	}
	// B: short from 7260 out at 7261 lost 1 pt; scale 1.2: exit 50 - 41.7 = 8%.
	if p := b.Path(); p == nil || p.ExitPct != 8 || p.Sign != "pnl-neg" || p.GainPct != 50 {
		t.Errorf("path b = %+v", p)
	}
	if c.Path() != nil {
		t.Error("unmeasured trade should have no path")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /trades", h.TradesPage)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/trades?risk=planned", nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /trades -> %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<option value="past-stop" >Past stop</option>`,
		`<option value="planned" selected>Planned</option>`,
		`<td class="tabular num c-r" data-label="Risk" title="R: net P&amp;L over the planned risk · path: heat left, best seen right, exit as the dot"><span class="past-stop-dot past-stop-dot--lead" title="Past the stop: heat −300.00 against a planned risk of 100.00"></span>`,
		// html/template writes + as &#43; in attributes and text.
		`<span class="path" title="Heat −300.00 · best seen &#43;500.00 · exit kept 80% (1m bars)">`,
		`style="width: 30%"`, `style="left: 90%"`,
		`planned <span class="num">1</span>`, `&#43;3.96R`, `past stop <span class="num pnl-neg">1</span>`,
		`exit kept <span class="num">80%</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Error("template sanitizer rejected an inline style")
	}
}
