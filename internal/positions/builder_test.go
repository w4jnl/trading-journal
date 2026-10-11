package positions

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
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
	comm  *float64 // broker-reported total commission for the fill (API sync); nil = rate lookup
}

func fptr(v float64) *float64 { return &v }

func setupBuilder(t *testing.T) (*Builder, *models.Queries, *sql.DB, int64) {
	t.Helper()
	conn := dbtest.Open(t, "test")
	q := models.New(conn)

	// Look up the bootstrap user the migration created.
	bootstrapID := dbtest.BootstrapUserID(t, conn, "test")

	// Create a test account with firm=apex (so commission lookups have a match).
	apex := "apex"
	acct, err := q.CreateAccount(context.Background(), models.CreateAccountParams{
		UserID:           bootstrapID,
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
	return &Builder{Queries: q, Timezone: loc, UserID: bootstrapID}, q, conn, acct.ID
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
			Commission:   f.comm,
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

// Apex commission for ES is $1.99 per side, so a 1-contract round-turn
// costs $3.98.
const apexESPerSide = 1.99
const apexESRoundTurn = apexESPerSide * 2

func TestRebuild_SimpleLongClose(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(5), nil},
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
	wantComm := apexESRoundTurn * 1 // 1 contract × per-side × 2 legs
	if tr.Commission == nil || *tr.Commission != wantComm {
		t.Errorf("commission = %v, want %v", tr.Commission, wantComm)
	}
	wantNet := wantGross - wantComm
	if tr.NetPnl == nil || *tr.NetPnl != wantNet {
		t.Errorf("net_pnl = %v, want %v", tr.NetPnl, wantNet)
	}
}

// Confirms commission_rates.per_contract_per_side is treated as per-side
// (charged twice per closed trade), not as a round-turn rate.
func TestRebuild_PerSideCommissionScalesWithQty(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 3, 7250, at(0), nil},
		{"sell", 3, 7252, at(5), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	want := apexESPerSide * 3 * 2 // 3 contracts × per-side × 2 legs
	if trades[0].Commission == nil || *trades[0].Commission != want {
		t.Errorf("commission = %v, want %v (per-side × qty × 2)", trades[0].Commission, want)
	}
}

// TestRebuild_TimeVersionedCommission proves each fill is charged the per-side
// rate in effect at its own exec_time. A rate change at 14:03 (1.99 -> 3.00)
// splits three trades: one wholly before (both legs old), one wholly after
// (both legs new), and one straddling the cutover (old entry leg, new exit leg).
func TestRebuild_TimeVersionedCommission(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	ctx := context.Background()

	// New version for the ES cell (equity_futures/apex/mini) effective 14:03.
	const newRate = 3.00
	if err := q.UpsertCommissionRate(ctx, models.UpsertCommissionRateParams{
		AssetClass: "equity_futures", Firm: "apex", SizeClass: "mini",
		EffectiveFrom: at(3), PerContractPerSide: newRate,
	}); err != nil {
		t.Fatalf("UpsertCommissionRate: %v", err)
	}

	// Disjoint cycles so each opens and fully closes before the next:
	//   before:   buy 14:00, sell 14:01
	//   straddle: buy 14:02, sell 14:04
	//   after:    buy 14:05, sell 14:06
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil}, {"sell", 1, 7252, at(1), nil},
		{"buy", 1, 7250, at(2), nil}, {"sell", 1, 7252, at(4), nil},
		{"buy", 1, 7250, at(5), nil}, {"sell", 1, 7252, at(6), nil},
	})
	if _, err := b.Rebuild(ctx, []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	trades, _ := q.ListTradesForAccount(ctx, accID)
	if len(trades) != 3 {
		t.Fatalf("len(trades) = %d, want 3", len(trades))
	}
	sort.Slice(trades, func(i, j int) bool { return trades[i].EntryTime.Before(trades[j].EntryTime) })

	want := []float64{
		apexESPerSide + apexESPerSide, // before: old both legs = 3.98
		apexESPerSide + newRate,       // straddle: old entry + new exit = 4.99
		newRate + newRate,             // after: new both legs = 6.00
	}
	for i, tr := range trades {
		if tr.Commission == nil {
			t.Fatalf("trade %d commission is nil", i)
		}
		if *tr.Commission != want[i] {
			t.Errorf("trade %d (entry %s) commission = %.2f, want %.2f",
				i, tr.EntryTime.Format("15:04"), *tr.Commission, want[i])
		}
	}
}

// Broker-reported (actual) commission on an execution takes precedence over
// the rate table, per leg. Covers the TopstepX API sync path, which stores
// fees+commissions on executions.commission.
func TestRebuild_ActualCommission_AllLegs(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), fptr(3.10)},
		{"sell", 1, 7252, at(5), fptr(3.30)},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	// 3.10 + 3.30, NOT the apex rate 1.99*2 = 3.98.
	if trades[0].Commission == nil || *trades[0].Commission != 6.40 {
		t.Errorf("commission = %v, want 6.40 (actual, not rate)", trades[0].Commission)
	}
}

// A trade whose entry fill carries an actual commission but whose exit fill
// does not combines actual (entry) + rate lookup (exit) per leg.
func TestRebuild_ActualCommission_MixedLegs(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), fptr(3.10)},
		{"sell", 1, 7252, at(5), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	want := 3.10 + apexESPerSide
	if trades[0].Commission == nil || *trades[0].Commission != want {
		t.Errorf("commission = %v, want %.2f (actual entry + rate exit)", trades[0].Commission, want)
	}
}

// An entry fill consumed across two trades (partial closes) pro-rates its
// actual commission by consumed quantity: qty 5 with commission 10.00 is
// 2.00/contract, so a 2-lot close carries 4.00 of it and the 3-lot close 6.00.
func TestRebuild_ActualCommission_ProRatedPartial(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 5, 7250, at(0), fptr(10.00)},
		{"sell", 2, 7252, at(5), nil},
		{"sell", 3, 7254, at(10), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1 (one FIFO cycle, two partial exits)", len(trades))
	}
	// One cycle: entry leg actual 10.00 + exit legs via rate (2+3 contracts).
	want := 10.00 + apexESPerSide*5
	if trades[0].Commission == nil || *trades[0].Commission != want {
		t.Errorf("commission = %v, want %.2f", trades[0].Commission, want)
	}
}

// A flip fill (sell 5 against long 2 -> short 3) splits its actual commission
// across the closing trade's exit and the new trade's entry in 2:3 proportion.
func TestRebuild_ActualCommission_FlipSplit(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 2, 7250, at(0), fptr(4.00)},
		{"sell", 5, 7252, at(5), fptr(10.00)}, // 2.00/contract
		{"buy", 3, 7250, at(10), fptr(6.00)},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 2 {
		t.Fatalf("len(trades) = %d, want 2 (flip)", len(trades))
	}
	sort.Slice(trades, func(i, j int) bool { return trades[i].EntryTime.Before(trades[j].EntryTime) })
	// Long 2: entry 4.00 + 2/5 of the flip fill's 10.00 = 8.00.
	if trades[0].Commission == nil || *trades[0].Commission != 8.00 {
		t.Errorf("long trade commission = %v, want 8.00", trades[0].Commission)
	}
	// Short 3: 3/5 of the flip fill's 10.00 + exit 6.00 = 12.00.
	if trades[1].Commission == nil || *trades[1].Commission != 12.00 {
		t.Errorf("short trade commission = %v, want 12.00", trades[1].Commission)
	}
}

// noRateAccount creates a second account with no firm, so loadRateVersions
// returns zero versions and only actual per-execution commissions can apply.
func noRateAccount(t *testing.T, q *models.Queries, userID int64) int64 {
	t.Helper()
	acct, err := q.CreateAccount(context.Background(), models.CreateAccountParams{
		UserID:           userID,
		Name:             "no-rate",
		Firm:             nil,
		ImportSource:     "motivewave_csv",
		AccountRef:       "NORATE",
		Currency:         "USD",
		NeedsSetup:       1,
		CommissionSource: "rate",
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return acct.ID
}

// All-actual trades must compute a commission even when the cell has ZERO rate
// versions (regression: an earlier commissionForLegs returned nil whenever the
// versions slice was empty, which would wrongly NULL API-synced trades on
// accounts with no configured rates).
func TestRebuild_ActualCommission_NoRateCell(t *testing.T) {
	b, q, _, _ := setupBuilder(t)
	accID := noRateAccount(t, q, b.UserID)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), fptr(3.10)},
		{"sell", 1, 7252, at(5), fptr(3.30)},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	if trades[0].Commission == nil || *trades[0].Commission != 6.40 {
		t.Errorf("commission = %v, want 6.40 despite no rate cell", trades[0].Commission)
	}
}

// No actual commission and no rate cell -> commission NULL, net = gross
// (the pre-existing "unconfigured -> NULL" honesty is preserved).
func TestRebuild_NoActualNoRate_NullCommission(t *testing.T) {
	b, q, _, _ := setupBuilder(t)
	accID := noRateAccount(t, q, b.UserID)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(5), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Commission != nil {
		t.Errorf("commission = %v, want nil", *tr.Commission)
	}
	if tr.GrossPnl == nil || tr.NetPnl == nil || *tr.NetPnl != *tr.GrossPnl {
		t.Errorf("net (%v) should equal gross (%v) when commission is NULL", tr.NetPnl, tr.GrossPnl)
	}
}

func TestRebuild_SimpleShortClose(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"sell", 1, 7252, at(0), nil},
		{"buy", 1, 7250, at(5), nil},
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
		{"buy", 1, 7250, at(0), nil},
		{"buy", 2, 7252, at(1), nil},
		{"sell", 1, 7256, at(2), nil},
		{"sell", 2, 7258, at(3), nil},
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
		{"buy", 2, 7250, at(0), nil},
		{"sell", 5, 7252, at(5), nil},
		{"buy", 3, 7251, at(10), nil},
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
		{"buy", 1, 7250, at(0), nil},
		{"buy", 1, 7251, at(5), nil},
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
		{"buy", 1, 100, at(0), nil},
		{"sell", 1, 102, at(5), nil},
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
		{"buy", 1, 7250, day(14), nil}, {"sell", 1, 7252, day(15), nil}, // gross 100, net ~96.02
		{"buy", 1, 7253, day(16), nil}, {"sell", 1, 7251, day(17), nil}, // gross -100, net ~-103.98
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
	wantNet := -2 * apexESRoundTurn // gross 0 minus 2 trades worth of round-turn commission
	if !approxEqual(s.NetPnl, wantNet) {
		t.Errorf("net_pnl = %v, want %v", s.NetPnl, wantNet)
	}
	wantComm := 2 * apexESRoundTurn
	if !approxEqual(s.Commission, wantComm) {
		t.Errorf("commission = %v, want %v", s.Commission, wantComm)
	}
}

func TestRebuild_PreservesJournalLinkAcrossRebuild(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	// First import: long 1 → out, produces one trade.
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(5), nil},
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

// TestRebuild_PreservesGroupJournalLinksAcrossRebuild guards the M:N
// group-journal table (journal_entry_trades). The FK has ON DELETE
// CASCADE, so a naive rebuild wipes every link to a deleted trade.
// The builder must snapshot pre-delete and re-insert post-replay.
func TestRebuild_PreservesGroupJournalLinksAcrossRebuild(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	// Two trades on the same account/symbol so we can verify multiple
	// links survive (not just the first one).
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(5), nil},
		{"sell", 1, 7260, at(20), nil},
		{"buy", 1, 7258, at(25), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 2 {
		t.Fatalf("setup expected 2 trades, got %d", len(trades))
	}

	// Create a group journal entry and link both trades to it.
	res, err := conn.Exec(
		`INSERT INTO journal_entries (account_id, entry_date, body, scope) VALUES (?, ?, ?, 'group')`,
		accID, "2026-05-10", "group debrief",
	)
	if err != nil {
		t.Fatalf("seed group entry: %v", err)
	}
	groupID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	for _, tr := range trades {
		if _, err := conn.Exec(
			`INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)`,
			groupID, tr.ID,
		); err != nil {
			t.Fatalf("seed link: %v", err)
		}
	}
	origEntryTimes := []time.Time{trades[0].EntryTime, trades[1].EntryTime}

	// Second rebuild — same executions, same resulting entry_times. The
	// CASCADE wipes both link rows; the builder must re-insert them.
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("rebuild with group links: %v", err)
	}
	trades, _ = q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 2 {
		t.Fatalf("post-rebuild len(trades) = %d, want 2", len(trades))
	}
	// Sanity: entry_times survived the rebuild.
	for i, want := range origEntryTimes {
		if !trades[i].EntryTime.Equal(want) {
			t.Fatalf("trade[%d] entry_time drift: %v vs %v", i, trades[i].EntryTime, want)
		}
	}

	var linkCount int64
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM journal_entry_trades WHERE journal_entry_id = ?`, groupID,
	).Scan(&linkCount); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if linkCount != 2 {
		t.Fatalf("links after rebuild = %d, want 2 (group links must survive replay)", linkCount)
	}

	// Each link should point at the new trade with the matching entry_time.
	for _, tr := range trades {
		var hits int64
		if err := conn.QueryRow(
			`SELECT COUNT(*) FROM journal_entry_trades WHERE journal_entry_id = ? AND trade_id = ?`,
			groupID, tr.ID,
		).Scan(&hits); err != nil {
			t.Fatalf("link lookup: %v", err)
		}
		if hits != 1 {
			t.Errorf("expected exactly 1 link to trade id %d, got %d", tr.ID, hits)
		}
	}
}

// TestRebuild_GroupLinkDropsWhenSignalShifts is the symmetric fallback
// case: when the underlying executions change enough that no trade
// with the original entry_time exists anymore, the link is dropped
// silently - same behavior as the trade-scope (single trade_id) FK.
func TestRebuild_GroupLinkDropsWhenSignalShifts(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(5), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("setup expected 1 trade, got %d", len(trades))
	}

	res, err := conn.Exec(
		`INSERT INTO journal_entries (account_id, entry_date, body, scope) VALUES (?, ?, 'g', 'group')`,
		accID, "2026-05-10",
	)
	if err != nil {
		t.Fatalf("seed group entry: %v", err)
	}
	groupID, _ := res.LastInsertId()
	if _, err := conn.Exec(
		`INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)`,
		groupID, trades[0].ID,
	); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// Wipe executions, insert a new pair with a different entry_time so
	// the rebuilt trade has no signal-match against the original.
	if _, err := conn.Exec(`DELETE FROM executions WHERE account_id = ?`, accID); err != nil {
		t.Fatalf("clear execs: %v", err)
	}
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7300, at(120), nil}, // 2h later → different entry_time
		{"sell", 1, 7302, at(125), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("rebuild after shift: %v", err)
	}

	var linkCount int64
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM journal_entry_trades WHERE journal_entry_id = ?`, groupID,
	).Scan(&linkCount); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if linkCount != 0 {
		t.Errorf("links after signal shift = %d, want 0 (dropped fallback)", linkCount)
	}
}

func TestRebuild_IdempotentReplay(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(5), nil},
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

// TestRebuild_PreservesTradePlanAcrossRebuild: the planned stop survives a
// rebuild (keyed on the opening fill, like journal links) and the risk is
// recomputed from it against the rebuilt trade: 2 points on ES at 50/pt.
func TestRebuild_PreservesTradePlanAcrossRebuild(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(5), nil},
	})
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 {
		t.Fatalf("setup expected 1 trade, got %d", len(trades))
	}
	if _, err := conn.Exec(`UPDATE trades SET planned_stop = 7248, planned_risk = 1 WHERE id = ?`, trades[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rebuild(context.Background(), []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("rebuild with plan: %v", err)
	}
	trades, _ = q.ListTradesForAccount(context.Background(), accID)
	if len(trades) != 1 || trades[0].PlannedStop == nil || *trades[0].PlannedStop != 7248 {
		t.Fatalf("plan lost across rebuild: %+v", trades)
	}
	if trades[0].PlannedRisk == nil || *trades[0].PlannedRisk != 100 {
		t.Errorf("planned_risk = %v, want 100 (2 pts x 50/pt x 1), recomputed from the stop", trades[0].PlannedRisk)
	}
}

func TestPlannedRisk(t *testing.T) {
	ts, tv := 0.25, 12.5
	es := models.Instrument{TickSize: &ts, TickValue: &tv}
	if r := PlannedRisk(7250, 7248, 2, es); r == nil || *r != 200 {
		t.Errorf("ES 2 pts x 2 = %v, want 200", r)
	}
	if r := PlannedRisk(7250, 7252, 1, es); r == nil || *r != 100 {
		t.Errorf("stop above entry still measures the distance: %v", r)
	}
	if r := PlannedRisk(7250, 7250, 1, es); r != nil {
		t.Errorf("stop on the entry = %v, want nil", r)
	}
	if r := PlannedRisk(7250, 7248, 1, models.Instrument{}); r != nil {
		t.Errorf("stub instrument = %v, want nil", r)
	}
	net, risk := 150.0, 100.0
	if r := RMultiple(&net, &risk); r == nil || *r != 1.5 {
		t.Errorf("R = %v, want 1.5", r)
	}
	zero := 0.0
	if RMultiple(&net, &zero) != nil || RMultiple(nil, &risk) != nil {
		t.Error("R with zero risk or no net must be nil")
	}
}
