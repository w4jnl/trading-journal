package excursion

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
)

// FillAccount measures the trades whose bars are cached (ES at 50/pt: 3
// points of heat is 150), leaves the ones without bars for a later pass,
// and tells the notifier whose data changed.
func TestServiceFillAccount(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	sym, ok := marketdata.YahooSymbol("ES")
	if !ok {
		t.Fatal("ES has no yahoo symbol")
	}
	entry := time.Date(2026, 10, 9, 14, 1, 30, 0, time.UTC)
	exit := entry.Add(100 * time.Second) // 14:03:10
	stmts := []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + itoa(uid) + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'ESZ6', 'ES', 'long', 2, 7250, 7254, ?, ?, 'closed', 400),
			(11, 1, 'NQZ6', 'NQ', 'short', 1, 20000, 19990, ?, ?, 'closed', 200)`,
	}
	for _, st := range stmts[:1] {
		if _, err := conn.Exec(st); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(stmts[1], entry, exit, entry, exit); err != nil {
		t.Fatal(err)
	}
	// 1m bars around the ES trade; nothing for NQ.
	base := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	for i, b := range [][4]float64{{7250, 7251, 7249, 7250}, {7250, 7253, 7248, 7252}, {7252, 7255, 7251, 7254}, {7254, 7254, 7247, 7248}, {7248, 7260, 7240, 7250}} {
		if _, err := conn.Exec(`INSERT INTO price_bars VALUES (?, '1m', ?, ?, ?, ?, ?, 0, 0)`, sym, base.Add(time.Duration(i)*time.Minute).Unix(), b[0], b[1], b[2], b[3]); err != nil {
			t.Fatal(err)
		}
	}

	var changed []int64
	s := &Service{DB: conn, Q: models.New(conn), Bars: &marketdata.CachingSource{DB: conn, Loc: time.UTC}, Changed: func(u int64) { changed = append(changed, u) }}
	n, err := s.FillAccount(context.Background(), 1)
	if err != nil || n != 1 {
		t.Fatalf("FillAccount = %d, %v; want 1 filled", n, err)
	}
	var maeP, mfeP, maeM, mfeM *float64
	var tf *string
	if err := conn.QueryRow(`SELECT mae_points, mfe_points, mae_pnl, mfe_pnl, excursion_tf FROM trades WHERE id = 10`).Scan(&maeP, &mfeP, &maeM, &mfeM, &tf); err != nil {
		t.Fatal(err)
	}
	if maeP == nil || *maeP != 3 || mfeP == nil || *mfeP != 5 || maeM == nil || *maeM != 300 || mfeM == nil || *mfeM != 500 || tf == nil || *tf != "1m" {
		t.Errorf("ES trade: mae %v/%v mfe %v/%v tf %v; want 3 pts / 300, 5 pts / 500, 1m", maeP, maeM, mfeP, mfeM, tf)
	}
	if err := conn.QueryRow(`SELECT excursion_tf FROM trades WHERE id = 11`).Scan(&tf); err != nil || tf != nil {
		t.Errorf("NQ trade without bars should stay unmeasured, got tf=%v err=%v", tf, err)
	}
	if len(changed) != 1 || changed[0] != uid {
		t.Errorf("Changed called with %v, want [%d]", changed, uid)
	}
	// A second pass finds nothing new and stays quiet.
	if n, err := s.FillAccount(context.Background(), 1); err != nil || n != 0 || len(changed) != 1 {
		t.Errorf("second pass = %d, %v, changed=%v", n, err, changed)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// One-second bars around a short trade, when a window covers it, measure
// the excursion instead of the minute bars: the minute candle hides the
// path inside it.
func TestFillPrefersFineBars(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ?, 'a', 'motivewave_csv', 'A')`, uid); err != nil {
		t.Fatal(err)
	}
	entry := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	exit := entry.Add(45 * time.Second)
	// The minute bar: low 6990 (ten points of heat). The seconds inside the
	// trade never go below 6998 (two points).
	minute := entry.Unix()
	if _, err := conn.Exec(`INSERT INTO price_bars (yahoo_symbol, timeframe, bar_time, open, high, low, close, volume, fetched_at) VALUES ('ES=F', '1m', ?, 7000, 7004, 6990, 7001, 10, 0)`, minute); err != nil {
		t.Fatal(err)
	}
	for s := int64(0); s < 60; s++ {
		low := 7000.0
		if s == 20 {
			low = 6998
		}
		if _, err := conn.Exec(`INSERT INTO price_bars (yahoo_symbol, timeframe, bar_time, open, high, low, close, volume, fetched_at) VALUES ('ES=F', '1s', ?, 7000, 7001, ?, 7000.5, 1, 0)`, minute+s, low); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO price_bar_windows (yahoo_symbol, timeframe, from_time, to_time, source, fetched_at) VALUES ('ES=F', '1s', ?, ?, 'topstepx', 0)`, minute-120, minute+180); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status)
		VALUES (5, 1, 'ESZ6', 'ES', 'long', 1, 7000, 7000.5, ?, ?, 'closed')`, entry, exit); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: conn, Q: models.New(conn), Bars: &marketdata.CachingSource{DB: conn, Loc: time.UTC}}
	ex, ok, err := s.Fill(context.Background(), Trade{ID: 5, SymbolRoot: "ES", Direction: "long", Qty: 1, EntryPrice: 7000, EntryTime: entry, ExitTime: exit})
	if err != nil || !ok {
		t.Fatalf("fill: ok=%v err=%v", ok, err)
	}
	if ex.Timeframe != marketdata.TF1s || ex.MAEPoints != 2 {
		t.Errorf("excursion = %+v, want 2 points of heat from the one-second bars", ex)
	}
}
