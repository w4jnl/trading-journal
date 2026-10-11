package handlers

import (
	"context"
	"encoding/json"
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

// The analytics summary (API and MCP share it) and the page's new sections
// over a small seeded book: three trades, one with a plan, a strategy on a
// trade entry, a tag through a group entry.
func TestAnalyticsSummaryAndPage(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + itoa64(uid) + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO strategies (id, user_id, name, body) VALUES (5, ` + itoa64(uid) + `, 'ORB', 'opening range')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl, gross_pnl, commission, planned_risk) VALUES
			(10, 1, 'ESU6', 'ES', 'long', 1, 100, 102, '2026-10-06 14:00:00 +0000 UTC', '2026-10-06 15:00:00 +0000 UTC', 'closed', 100, 104, 4, 50),
			(11, 1, 'ESU6', 'ES', 'short', 1, 100, 103, '2026-10-07 14:00:00 +0000 UTC', '2026-10-07 15:00:00 +0000 UTC', 'closed', -150, -146, 4, NULL),
			(12, 1, 'NQU6', 'NQ', 'long', 1, 100, 101, '2026-10-08 14:00:00 +0000 UTC', '2026-10-08 15:00:00 +0000 UTC', 'closed', 20, 24, 4, NULL)`,
		`INSERT INTO journal_entries (id, user_id, trade_id, entry_date, body, scope, strategy_id) VALUES (1, ` + itoa64(uid) + `, 10, '2026-10-06', 'x', 'trade', 5)`,
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope, tag) VALUES (2, ` + itoa64(uid) + `, '2026-10-07', 'g', 'group', 'bad')`,
		`INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (2, 11)`,
		// Measured price paths: the winner took 20 of heat with 150 on the
		// table, the loser had 60 of profit before turning; 12 is unmeasured.
		`UPDATE trades SET mae_pnl = 20, mfe_pnl = 150, mae_points = 0.4, mfe_points = 3, excursion_tf = '1m' WHERE id = 10`,
		`UPDATE trades SET mae_pnl = 200, mfe_pnl = 60, mae_points = 4, mfe_points = 1.2, excursion_tf = '1m' WHERE id = 11`,
		// Labels reach a trade through its own entry (10), a group entry (11)
		// or the day entry of the day it closed (12 via the 2026-10-08 day).
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope) VALUES (3, ` + itoa64(uid) + `, '2026-10-08', 'd', 'day')`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label) VALUES (1, 'mistake', 'chased'), (2, 'mistake', 'chased'), (2, 'emotion', 'tilted'), (3, 'mistake', 'overtraded')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/summary?from=2026-10-01&to=2026-10-10", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.APIAnalyticsSummary(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary -> %d: %s", rec.Code, rec.Body.String())
	}
	var out analyticsSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.Trades != 3 || out.Totals.Net != -30 || out.Totals.Wins != 2 || out.Totals.Losses != 1 {
		t.Errorf("totals = %+v", out.Totals)
	}
	if out.R.PlannedTrades != 1 || out.R.AvgR != 2 {
		t.Errorf("r = %+v, want one planned trade at +2R", out.R)
	}
	// Gross 104 + -146 over MFE 150 + 60: the exits kept -20% of what was there.
	if ex := out.Excursion; ex.Trades != 2 || ex.ExitEfficiency != -0.2 || ex.AvgMae != 110 || ex.AvgMfe != 105 || ex.WinnersAvgMae != 20 || ex.LosersAvgMfe != 60 {
		t.Errorf("excursion = %+v", ex)
	}
	// Cumulative 100, -50, -30: the fall from 100 to -50 is 150 and still open.
	if out.Drawdown.MaxDD != 150 || out.Drawdown.MaxDDRecover != nil || out.Drawdown.CurrentDD != 130 {
		t.Errorf("drawdown = %+v", out.Drawdown)
	}
	if out.Streaks.LongestWinTrades != 1 || out.Streaks.LongestLossTrades != 1 || out.Streaks.CurrentTrades != 1 {
		t.Errorf("streaks = %+v", out.Streaks)
	}
	keys := func(name string) []string {
		var ks []string
		for _, b := range out.Breakdowns[name] {
			ks = append(ks, b.Key)
		}
		return ks
	}
	if ks := keys("byStrategy"); len(ks) != 2 || ks[0] != "ORB" {
		t.Errorf("byStrategy keys = %v, want ORB first (net 100) then (no strategy)", ks)
	}
	if ks := keys("byTag"); len(ks) != 2 || ks[0] != "(untagged)" || ks[1] != "bad" {
		t.Errorf("byTag keys = %v", ks)
	}
	// Chased: trades 10 (+100) and 11 (-150) = -50 over 2 trades; Overtraded
	// reaches trade 12 (+20) from its day entry, so no trade is unlabelled.
	if bs := out.Breakdowns["byMistake"]; len(bs) != 2 || bs[0].Key != "Overtraded" || bs[0].Trades != 1 || bs[0].Net != 20 || bs[1].Key != "Chased the entry" || bs[1].Trades != 2 || bs[1].Net != -50 {
		t.Errorf("byMistake = %+v", bs)
	}
	if bs := out.Breakdowns["byEmotion"]; len(bs) != 2 || bs[0].Key != "(none logged)" || bs[0].Trades != 2 || bs[1].Key != "Tilted" || bs[1].Trades != 1 || bs[1].Net != -150 {
		t.Errorf("byEmotion = %+v", bs)
	}
	if rec := httptest.NewRecorder(); true {
		h.APIAnalyticsSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/summary?from=2026-13-01", nil).WithContext(ctx))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad date -> %d, want 400", rec.Code)
		}
	}

	// The page renders the new sections without template errors.
	rec = httptest.NewRecorder()
	h.AnalyticsPage(rec, httptest.NewRequest(http.MethodGet, "/analytics?from=2026-10-01&to=2026-10-10", nil).WithContext(ctx))
	body := rec.Body.String()
	for _, want := range []string{"Max drawdown", "still open", "Trade streaks", "By strategy", "By tag", "By mistake", "By emotion", "<code>ORB</code>", "<code>bad</code>", "<code>Chased the entry</code>", "<code>Overtraded</code>", "<code>Tilted</code>", "Avg R", "Exit efficiency", "-20%", "Heat on winners", "−20.00", "Given back by losers", "+60.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("analytics page missing %q", want)
		}
	}
	if strings.Contains(body, "<no value>") {
		t.Error("analytics page rendered <no value>")
	}
}
