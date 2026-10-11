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
	"github.com/w4jnl/trading-journal/internal/excursion"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Opening the sheet of a closed trade measures it from cached bars and
// shows heat, best seen and the share the exit kept; the API rows carry
// the same numbers.
func TestTradeSheetMeasuresExcursion(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	sym, _ := marketdata.YahooSymbol("ES")
	entry := time.Date(2026, 10, 9, 14, 1, 30, 0, time.UTC)
	exit := entry.Add(100 * time.Second)
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + itoa64(uid) + `, 'mine', 'motivewave_csv', 'A-1')`); err != nil {
		t.Fatal(err)
	}
	// Long 2 ES from 7250 out at 7254: best 7255 (+5 pts), worst 7247 (-3 pts).
	if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl) VALUES
		(10, 1, 'ESZ6', 'ES', 'long', 2, 7250, 7254, ?, ?, 'closed', 400, 4, 396)`, entry, exit); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	for i, b := range [][4]float64{{7250, 7251, 7249, 7250}, {7250, 7253, 7248, 7252}, {7252, 7255, 7251, 7254}, {7254, 7254, 7247, 7248}} {
		if _, err := conn.Exec(`INSERT INTO price_bars VALUES (?, '1m', ?, ?, ?, ?, ?, 0, 0)`, sym, base.Add(time.Duration(i)*time.Minute).Unix(), b[0], b[1], b[2], b[3]); err != nil {
			t.Fatal(err)
		}
	}
	q := models.New(conn)
	cache := &marketdata.CachingSource{DB: conn, Loc: time.UTC}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, MarketData: cache,
		Excursions: &excursion.Service{DB: conn, Q: q, Bars: cache}}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})

	req := httptest.NewRequest(http.MethodGet, "/trades/10", nil).WithContext(ctx)
	req.SetPathValue("id", "10")
	rec := httptest.NewRecorder()
	h.TradeDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sheet -> %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Max heat", "−300.00", "3 pts", "Best seen", "+500.00", "5 pts", "Exit kept", "80%", "(1m bars)",
		`data-plan-entry="7250" data-plan-qty="2" data-plan-direction="long" data-plan-point-value="50" data-plan-tick="0.25"`} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet missing %q", want)
		}
	}
	var tf *string
	if err := conn.QueryRow(`SELECT excursion_tf FROM trades WHERE id = 10`).Scan(&tf); err != nil || tf == nil || *tf != "1m" {
		t.Errorf("sheet view did not persist the excursion: tf=%v err=%v", tf, err)
	}

	// API detail and list rows.
	d, err := h.tradeDetail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mae == nil || *d.Mae != 300 || d.Mfe == nil || *d.Mfe != 500 || d.MaePoints == nil || *d.MaePoints != 3 ||
		d.ExitEfficiency == nil || *d.ExitEfficiency != 0.8 || d.ExcursionBars == nil || *d.ExcursionBars != "1m" {
		b, _ := json.Marshal(d)
		t.Errorf("api detail excursion fields: %s", b)
	}
	rows, err := listTradesFiltered(ctx, conn, tradeFilters{Page: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v (%d rows)", err, len(rows))
	}
	if r := apiTradeRowFrom(rows[0]); r.Mfe == nil || *r.Mfe != 500 || r.ExitEfficiency == nil || *r.ExitEfficiency != 0.8 {
		t.Errorf("api list row excursion fields: %+v", r)
	}
}
