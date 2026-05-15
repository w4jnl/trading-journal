package positions

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/models"
)

// FIFO state-machine tests use synthetic fills inserted directly into a fresh
// DB. Each test builds a one-symbol scenario and asserts trade count, qty,
// VWAP entry/exit, direction, and P&L.

type synthFill struct {
	side  string
	qty   float64
	price float64
	when  time.Time
}

func setupBuilder(t *testing.T) (*Builder, *models.Queries, *sql.DB, int64) {
	t.Helper()
	tmp := t.TempDir()
	conn, err := db.Open(filepath.Join(tmp, "t.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.RunMigrations(conn); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	q := models.New(conn)

	// Create a test account with firm=apex (so commission lookups have a match).
	apex := "apex"
	acct, err := q.CreateAccount(context.Background(), models.CreateAccountParams{
		Name:             "test",
		Firm:             &apex,
		ImportSource:     "motivewave_csv",
		AccountRef:       "TEST",
		Currency:         "USD",
		NeedsSetup:       1,
		CommissionSource: "rate",
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	loc, _ := time.LoadLocation("America/Chicago")
	return &Builder{Queries: q, Timezone: loc}, q, conn, acct.ID
}

func insertSynthExecutions(t *testing.T, q *models.Queries, accountID int64, symbol, root string, fills []synthFill) {
	t.Helper()
	ctx := context.Background()
	for i, f := range fills {
		if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{
			AccountID:    accountID,
			BrokerExecID: synthExecID(symbol, i),
			Symbol:       symbol,
			SymbolRoot:   root,
			Side:         f.side,
			Qty:          f.qty,
			Price:        f.price,
			ExecTime:     f.when,
		}); err != nil {
			t.Fatalf("insert synth %d: %v", i, err)
		}
	}
}

func synthExecID(symbol string, i int) string {
	return symbol + ":" + time.Now().Format("150405.000000") + ":" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// at builds a fixed-base time + offset.
func at(min int) time.Time {
	base := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(min) * time.Minute)
}

// ES tick specs from the seeded migration: tick=0.25, value=12.50, multiplier=50.
const esMultiplier = 50.0

// Apex commission for ES is $1.99 RT.
const apexESRate = 1.99

func TestRebuild_SimpleLongClose(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0)},
		{"sell", 1, 7252, at(5)},
	})

	res, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if res.TradesCreated != 1 {
		t.Fatalf("TradesCreated = %d, want 1", res.TradesCreated)
	}

	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Direction != "long" {
		t.Errorf("direction = %q, want long", tr.Direction)
	}
	if tr.Qty != 1 {
		t.Errorf("qty = %v, want 1", tr.Qty)
	}
	if tr.EntryPrice != 7250 {
		t.Errorf("entry = %v, want 7250", tr.EntryPrice)
	}
	if tr.ExitPrice == nil || *tr.ExitPrice != 7252 {
		t.Errorf("exit = %v, want 7252", tr.ExitPrice)
	}
	wantGross := 2.0 * esMultiplier // 2 points × $50 = $100
	if tr.GrossPnl == nil || *tr.GrossPnl != wantGross {
		t.Errorf("gross_pnl = %v, want %v", tr.GrossPnl, wantGross)
	}
	wantComm := apexESRate * 1
	if tr.Commission == nil || *tr.Commission != wantComm {
		t.Errorf("commission = %v, want %v", tr.Commission, wantComm)
	}
	wantNet := wantGross - wantComm
	if tr.NetPnl == nil || *tr.NetPnl != wantNet {
		t.Errorf("net_pnl = %v, want %v", tr.NetPnl, wantNet)
	}
}

func TestRebuild_SimpleShortClose(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"sell", 1, 7252, at(0)},
		{"buy", 1, 7250, at(5)},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Direction != "short" {
		t.Errorf("direction = %q, want short", tr.Direction)
	}
	wantGross := 2.0 * esMultiplier
	if tr.GrossPnl == nil || *tr.GrossPnl != wantGross {
		t.Errorf("gross_pnl = %v, want %v", tr.GrossPnl, wantGross)
	}
}

func TestRebuild_ScaleInScaleOut(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	// Buy 1 @ 7250, buy 2 @ 7252 → avg entry = (7250 + 14504) / 3 = 7251.333...
	// Sell 1 @ 7256, sell 2 @ 7258 → avg exit = (7256 + 14516) / 3 = 7257.333...
	// Gross = (7257.333 - 7251.333) * 50 * 3 = 6 * 50 * 3 = 900
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0)},
		{"buy", 2, 7252, at(1)},
		{"sell", 1, 7256, at(2)},
		{"sell", 2, 7258, at(3)},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Qty != 3 {
		t.Errorf("qty (max position) = %v, want 3", tr.Qty)
	}
	if !approxEqual(tr.EntryPrice, 7251+1.0/3) {
		t.Errorf("entry = %v, want 7251.333", tr.EntryPrice)
	}
	if tr.ExitPrice == nil || !approxEqual(*tr.ExitPrice, 7257+1.0/3) {
		t.Errorf("exit = %v, want 7257.333", tr.ExitPrice)
	}
	if tr.GrossPnl == nil || !approxEqual(*tr.GrossPnl, 900) {
		t.Errorf("gross_pnl = %v, want 900", tr.GrossPnl)
	}
}

func TestRebuild_PositionFlip(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	// Long 2 @ 7250, sell 5 @ 7252 → close long 2 (gross +200), open short 3 @ 7252.
	// Buy 3 @ 7251 → close short 3 (gross +50). Total: 2 trades.
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 2, 7250, at(0)},
		{"sell", 5, 7252, at(5)},
		{"buy", 3, 7251, at(10)},
	})
	res, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if res.TradesCreated != 2 {
		t.Fatalf("TradesCreated = %d, want 2", res.TradesCreated)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 2 {
		t.Fatalf("len(trades) = %d, want 2", len(trades))
	}

	// First trade: long 2, entry 7250, exit 7252, gross = 2 * 50 * 2 = 200
	long := trades[0]
	if long.Direction != "long" || long.Qty != 2 ||
		long.EntryPrice != 7250 || (long.ExitPrice == nil || *long.ExitPrice != 7252) ||
		(long.GrossPnl == nil || *long.GrossPnl != 200) {
		t.Errorf("first trade %+v not the expected long 2 @ 7250→7252 (gross 200)", long)
	}

	// Second trade: short 3, entry 7252, exit 7251, gross = 1 * 50 * 3 = 150
	short := trades[1]
	if short.Direction != "short" || short.Qty != 3 ||
		short.EntryPrice != 7252 || (short.ExitPrice == nil || *short.ExitPrice != 7251) ||
		(short.GrossPnl == nil || *short.GrossPnl != 150) {
		t.Errorf("second trade %+v not the expected short 3 @ 7252→7251 (gross 150)", short)
	}

	// The flip fill (sell 5 @ 7252) should appear in BOTH trades:
	//   - role=exit on trade 1 with qty_used=2
	//   - role=entry on trade 2 with qty_used=3
	flipExec := getExecBySide(t, q, accID, "sell", 5)
	long1Junctions := junctionsFor(t, q, long.ID)
	short2Junctions := junctionsFor(t, q, short.ID)

	if got := long1Junctions["exit"][flipExec.ID]; got != 2 {
		t.Errorf("flip fill participation in long trade as exit = %v, want 2", got)
	}
	if got := short2Junctions["entry"][flipExec.ID]; got != 3 {
		t.Errorf("flip fill participation in short trade as entry = %v, want 3", got)
	}
}

func TestRebuild_OpenPositionDoesNotEmitTrade(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0)},
		{"buy", 1, 7251, at(5)},
		// Position still open — never sold.
	})
	res, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if res.TradesCreated != 0 {
		t.Fatalf("TradesCreated = %d, want 0 (position still open)", res.TradesCreated)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 0 {
		t.Errorf("len(trades) = %d, want 0", len(trades))
	}
}

func TestRebuild_StubInstrumentLeavesPnLNull(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	// Manually create a stub instrument with NULL multiplier.
	if _, err := conn.Exec(`INSERT INTO instruments (symbol_root, currency, needs_setup) VALUES ('XYZ', 'USD', 1)`); err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	insertSynthExecutions(t, q, accID, "XYZM6", "XYZ", []synthFill{
		{"buy", 1, 100, at(0)},
		{"sell", 1, 102, at(5)},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "XYZM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	if trades[0].GrossPnl != nil {
		t.Errorf("gross_pnl = %v, want nil (stub instrument)", trades[0].GrossPnl)
	}
	if trades[0].NetPnl != nil {
		t.Errorf("net_pnl = %v, want nil (stub)", trades[0].NetPnl)
	}
}

func TestRebuild_DailyStats(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	// Two trades on the same day, both wins.
	day := func(h int) time.Time {
		return time.Date(2026, 5, 10, h, 0, 0, 0, time.UTC)
	}
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, day(14)}, {"sell", 1, 7252, day(15)}, // gross 100, net ~98.01
		{"buy", 1, 7253, day(16)}, {"sell", 1, 7251, day(17)}, // gross -100, net ~-101.99
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	stats, _ := q.ListDailyStatsForAccount(context.Background(), accID)
	if len(stats) != 1 {
		t.Fatalf("len(stats) = %d, want 1", len(stats))
	}
	s := stats[0]
	if s.TradesCount != 2 {
		t.Errorf("trades_count = %d, want 2", s.TradesCount)
	}
	if s.Wins != 1 || s.Losses != 1 {
		t.Errorf("wins=%d losses=%d, want 1/1", s.Wins, s.Losses)
	}
	if !approxEqual(s.GrossPnl, 0) {
		t.Errorf("gross_pnl = %v, want 0", s.GrossPnl)
	}
	wantNet := -2 * apexESRate
	if !approxEqual(s.NetPnl, wantNet) {
		t.Errorf("net_pnl = %v, want %v", s.NetPnl, wantNet)
	}
	wantComm := 2 * apexESRate
	if !approxEqual(s.Commission, wantComm) {
		t.Errorf("commission = %v, want %v", s.Commission, wantComm)
	}
}

func TestRebuild_PreservesJournalLinkAcrossRebuild(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	// First import: long 1 → out, produces one trade.
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0)},
		{"sell", 1, 7252, at(5)},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("setup expected 1 trade, got %d", len(trades))
	}
	origID := trades[0].ID
	_ = origID // SQLite may reuse the autoincrement value; we don't assert on it.
	origEntry := trades[0].EntryTime

	// Attach a journal entry to that trade. journal_entries.trade_id is
	// NO ACTION, so a naive rebuild would trip the FK on DELETE.
	if _, err := conn.Exec(
		`INSERT INTO journal_entries (trade_id, account_id, entry_date, body, scope) VALUES (?, ?, ?, ?, 'trade')`,
		origID, accID, "2026-05-10", "post-mortem on this trade",
	); err != nil {
		t.Fatalf("seed journal entry: %v", err)
	}

	// Second rebuild over the same executions. Should not error, and the
	// reconstructed trade should pick up the same journal entry by signal.
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("rebuild with journal link: %v", err)
	}
	trades, _ = q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("post-rebuild len(trades) = %d, want 1", len(trades))
	}
	if !trades[0].EntryTime.Equal(origEntry) {
		t.Fatalf("entry_time drift: %v vs %v", trades[0].EntryTime, origEntry)
	}
	var linkedID int64
	if err := conn.QueryRow(
		`SELECT trade_id FROM journal_entries WHERE body = 'post-mortem on this trade'`,
	).Scan(&linkedID); err != nil {
		t.Fatalf("journal link lookup: %v", err)
	}
	if linkedID != trades[0].ID {
		t.Errorf("journal_entries.trade_id = %d, want %d (the new trade)", linkedID, trades[0].ID)
	}
}

func TestRebuild_IdempotentReplay(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0)},
		{"sell", 1, 7252, at(5)},
	})
	for i := 0; i < 3; i++ {
		if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
			t.Fatalf("Rebuild #%d: %v", i, err)
		}
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Errorf("after 3 rebuilds, len(trades) = %d, want 1", len(trades))
	}
}

func approxEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-6
}

func getExecBySide(t *testing.T, q *models.Queries, accountID int64, side string, qty float64) models.Execution {
	t.Helper()
	execs, err := q.ListExecutionsByAccount(context.Background(), accountID)
	if err != nil {
		t.Fatalf("list execs: %v", err)
	}
	for _, e := range execs {
		if e.Side == side && e.Qty == qty {
			return e
		}
	}
	t.Fatalf("execution side=%s qty=%v not found", side, qty)
	return models.Execution{}
}

// junctionsFor returns trade_executions[role][exec_id] = qty_used for the trade.
// We query trade_executions directly because there's no sqlc query that returns
// the role + qty_used + execution joined for a single trade in one call yet.
func junctionsFor(t *testing.T, q *models.Queries, tradeID int64) map[string]map[int64]float64 {
	t.Helper()
	rows, err := q.ListExecutionsForTrade(context.Background(), tradeID)
	if err != nil {
		t.Fatalf("ListExecutionsForTrade: %v", err)
	}
	out := map[string]map[int64]float64{"entry": {}, "exit": {}}
	for _, r := range rows {
		out[r.Role][r.ID] = r.QtyUsed
	}
	return out
}
