// Package positions reconstructs trades from raw executions using a per
// (account, symbol) FIFO state machine. A "trade" is one open→close cycle of
// a position; multiple fills can contribute on either side.
//
// Correctness notes:
//   - FIFO is per (account, symbol); contract rolls (ESM6 → ESU6) close cleanly.
//   - A single fill that flips position (e.g. long 2 → sell 5 → short 3) closes
//     the existing trade and opens a new one. Both trades record that fill in
//     trade_executions, in different roles.
//   - P&L uses instruments.multiplier × price-diff × qty. If multiplier is NULL
//     (stub instrument awaiting setup), gross_pnl stays NULL — we never silently
//     substitute a price-difference shortcut.
//   - Commission lookups go (account.firm, instrument.symbol_root) → commission_rates.
//     Missing firm or missing rate row → commission NULL. net_pnl = gross_pnl
//     minus commission-or-zero, but stays NULL if gross_pnl is NULL.
//   - daily_stats is keyed on the trade exit date in the configured trading
//     timezone, not UTC, so a US trader's session doesn't get split at UTC midnight.
package positions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jaro/trading-journal/internal/models"
)

// Key identifies a (account, contract) FIFO queue.
type Key struct {
	AccountID int64
	Symbol    string
}

// Builder rebuilds trades and daily_stats from raw executions.
type Builder struct {
	Queries  *models.Queries
	Timezone *time.Location
}

// Result reports rebuild outcomes.
type Result struct {
	TradesCreated   int
	AccountsTouched int
}

// Rebuild deletes existing trades for each given key, replays the executions
// FIFO, writes fresh trade rows, and recomputes daily_stats for any account
// whose trades changed. All work runs in the *models.Queries provided to the
// Builder — pass a tx-bound Queries to share a transaction with the caller.
func (b *Builder) Rebuild(ctx context.Context, keys []Key) (Result, error) {
	res := Result{}
	if len(keys) == 0 {
		return res, nil
	}
	if b.Timezone == nil {
		return res, fmt.Errorf("Builder.Timezone is nil")
	}

	affectedAccounts := map[int64]bool{}
	for _, k := range keys {
		n, err := b.rebuildOne(ctx, k)
		if err != nil {
			return res, fmt.Errorf("rebuild %d/%s: %w", k.AccountID, k.Symbol, err)
		}
		res.TradesCreated += n
		affectedAccounts[k.AccountID] = true
	}

	for acc := range affectedAccounts {
		if err := b.rebuildDailyStats(ctx, acc); err != nil {
			return res, fmt.Errorf("rebuild daily_stats account %d: %w", acc, err)
		}
		res.AccountsTouched++
	}
	return res, nil
}

func (b *Builder) rebuildOne(ctx context.Context, k Key) (int, error) {
	if err := b.Queries.DeleteTradesForAccountSymbol(ctx, models.DeleteTradesForAccountSymbolParams{
		AccountID: k.AccountID,
		Symbol:    k.Symbol,
	}); err != nil {
		return 0, fmt.Errorf("delete prior trades: %w", err)
	}

	fills, err := b.Queries.ListExecutionsForAccountSymbol(ctx, models.ListExecutionsForAccountSymbolParams{
		AccountID: k.AccountID,
		Symbol:    k.Symbol,
	})
	if err != nil {
		return 0, fmt.Errorf("list executions: %w", err)
	}
	if len(fills) == 0 {
		return 0, nil
	}

	account, err := b.Queries.GetAccountByID(ctx, k.AccountID)
	if err != nil {
		return 0, fmt.Errorf("load account: %w", err)
	}

	instrument, err := b.Queries.GetInstrument(ctx, fills[0].SymbolRoot)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("load instrument: %w", err)
	}
	// If ErrNoRows, instrument is the zero value — multiplier is nil, P&L stays NULL.

	closed, err := b.replay(ctx, k, fills, account, instrument)
	if err != nil {
		return 0, err
	}
	return closed, nil
}

// lot is one open-position lot in the FIFO queue.
type lot struct {
	qtyRemaining float64
	price        float64
	time         time.Time
	execID       int64
}

// fillUse records that some quantity of an execution participated in a trade.
type fillUse struct {
	execID int64
	qty    float64
	price  float64
	time   time.Time
}

// tradeAccum accumulates state for one in-progress trade until it closes.
type tradeAccum struct {
	direction string // "long" | "short"
	entries   []fillUse
	exits     []fillUse
	maxQty    float64
}

func (b *Builder) replay(ctx context.Context, k Key, fills []models.Execution, account models.Account, inst models.Instrument) (int, error) {
	var openLots []lot
	var trade *tradeAccum
	closed := 0

	openTrade := func(fill models.Execution, dir string, qty float64) {
		trade = &tradeAccum{direction: dir, maxQty: qty}
		openLots = []lot{{qtyRemaining: qty, price: fill.Price, time: fill.ExecTime, execID: fill.ID}}
		trade.entries = append(trade.entries, fillUse{
			execID: fill.ID, qty: qty, price: fill.Price, time: fill.ExecTime,
		})
	}

	for _, fill := range fills {
		fillDir := "long"
		if fill.Side == "sell" {
			fillDir = "short"
		}

		if trade == nil {
			openTrade(fill, fillDir, fill.Qty)
			continue
		}

		if trade.direction == fillDir {
			openLots = append(openLots, lot{
				qtyRemaining: fill.Qty, price: fill.Price, time: fill.ExecTime, execID: fill.ID,
			})
			trade.entries = append(trade.entries, fillUse{
				execID: fill.ID, qty: fill.Qty, price: fill.Price, time: fill.ExecTime,
			})
			if total := totalLotQty(openLots); total > trade.maxQty {
				trade.maxQty = total
			}
			continue
		}

		// Opposite direction: close FIFO until lots exhausted or fill consumed.
		qtyLeft := fill.Qty
		consumedTotal := 0.0
		for qtyLeft > 0 && len(openLots) > 0 {
			head := &openLots[0]
			consumed := head.qtyRemaining
			if qtyLeft < consumed {
				consumed = qtyLeft
			}
			head.qtyRemaining -= consumed
			qtyLeft -= consumed
			consumedTotal += consumed
			if head.qtyRemaining <= 0 {
				openLots = openLots[1:]
			}
		}
		if consumedTotal > 0 {
			trade.exits = append(trade.exits, fillUse{
				execID: fill.ID, qty: consumedTotal, price: fill.Price, time: fill.ExecTime,
			})
		}

		if len(openLots) == 0 {
			if err := b.emitTrade(ctx, k, trade, account, inst); err != nil {
				return closed, err
			}
			closed++
			trade = nil

			if qtyLeft > 0 {
				// Flip: remainder opens a new trade. The same fill ID participates as
				// entry of this new trade; it's already in trade.exits of the previous
				// trade under role=exit, so the junction cleanly distinguishes the two.
				openTrade(fill, fillDir, qtyLeft)
			}
		}
	}
	// Open positions at end of replay don't produce trades in v1 — only closed
	// cycles materialize. Open positions can be derived from raw executions.
	return closed, nil
}

func totalLotQty(lots []lot) float64 {
	t := 0.0
	for _, l := range lots {
		t += l.qtyRemaining
	}
	return t
}

func (b *Builder) emitTrade(ctx context.Context, k Key, ta *tradeAccum, account models.Account, inst models.Instrument) error {
	entryAgg := aggregateByExec(ta.entries)
	exitAgg := aggregateByExec(ta.exits)

	entryTotal, entryWeighted, entryFirst := summarize(ta.entries, true)
	exitTotal, exitWeighted, exitLast := summarize(ta.exits, false)
	avgEntry := entryWeighted / entryTotal
	avgExit := exitWeighted / exitTotal

	grossPnl, commission, netPnl, err := b.computePnl(ctx, ta.direction, avgEntry, avgExit, exitTotal, account, inst)
	if err != nil {
		return err
	}

	exitTimePtr := exitLast
	exitPricePtr := avgExit

	row, err := b.Queries.CreateTrade(ctx, models.CreateTradeParams{
		AccountID:  k.AccountID,
		Symbol:     k.Symbol,
		SymbolRoot: inst.SymbolRoot, // safe: fills[0].SymbolRoot guarantees this
		Direction:  ta.direction,
		Qty:        ta.maxQty,
		EntryPrice: avgEntry,
		ExitPrice:  &exitPricePtr,
		EntryTime:  entryFirst,
		ExitTime:   &exitTimePtr,
		GrossPnl:   grossPnl,
		Commission: commission,
		NetPnl:     netPnl,
		Status:     "closed",
	})
	if err != nil {
		return fmt.Errorf("create trade: %w", err)
	}

	for execID, qty := range entryAgg {
		if err := b.Queries.LinkTradeExecution(ctx, models.LinkTradeExecutionParams{
			TradeID:     row.ID,
			ExecutionID: execID,
			Role:        "entry",
			QtyUsed:     qty,
		}); err != nil {
			return fmt.Errorf("link entry: %w", err)
		}
	}
	for execID, qty := range exitAgg {
		if err := b.Queries.LinkTradeExecution(ctx, models.LinkTradeExecutionParams{
			TradeID:     row.ID,
			ExecutionID: execID,
			Role:        "exit",
			QtyUsed:     qty,
		}); err != nil {
			return fmt.Errorf("link exit: %w", err)
		}
	}
	return nil
}

func aggregateByExec(uses []fillUse) map[int64]float64 {
	out := map[int64]float64{}
	for _, u := range uses {
		out[u.execID] += u.qty
	}
	return out
}

// summarize computes total qty, qty-weighted price sum, and first/last time.
// `wantFirst=true` returns the earliest time (entry); false returns the latest (exit).
func summarize(uses []fillUse, wantFirst bool) (totalQty, weighted float64, t time.Time) {
	for i, u := range uses {
		totalQty += u.qty
		weighted += u.qty * u.price
		if i == 0 {
			t = u.time
			continue
		}
		if wantFirst && u.time.Before(t) {
			t = u.time
		} else if !wantFirst && u.time.After(t) {
			t = u.time
		}
	}
	return
}

func (b *Builder) computePnl(ctx context.Context, direction string, avgEntry, avgExit, qty float64, account models.Account, inst models.Instrument) (gross, comm, net *float64, err error) {
	if inst.Multiplier == nil {
		return nil, nil, nil, nil
	}
	mult := *inst.Multiplier
	g := 0.0
	if direction == "long" {
		g = (avgExit - avgEntry) * mult * qty
	} else {
		g = (avgEntry - avgExit) * mult * qty
	}
	gross = &g

	if account.Firm != nil && inst.SymbolRoot != "" {
		rate, lookupErr := b.Queries.GetCommissionRate(ctx, models.GetCommissionRateParams{
			Firm:       *account.Firm,
			SymbolRoot: inst.SymbolRoot,
		})
		switch {
		case lookupErr == nil:
			c := rate.PerContractRt * qty
			comm = &c
			n := g - c
			net = &n
		case errors.Is(lookupErr, sql.ErrNoRows):
			// No rate configured for (firm, symbol_root): leave commission NULL,
			// net_pnl = gross_pnl (no commission charged in our records).
			n := g
			net = &n
		default:
			return nil, nil, nil, fmt.Errorf("commission lookup: %w", lookupErr)
		}
	} else {
		// firm unknown: no commission charged. net_pnl = gross_pnl.
		n := g
		net = &n
	}
	return gross, comm, net, nil
}

func (b *Builder) rebuildDailyStats(ctx context.Context, accountID int64) error {
	if err := b.Queries.DeleteDailyStatsForAccount(ctx, accountID); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	trades, err := b.Queries.ListTradesByDate(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list trades: %w", err)
	}
	if len(trades) == 0 {
		return nil
	}

	type bucket struct {
		count       int64
		wins        int64
		losses      int64
		gross       float64
		net         float64
		commission  float64
		largestWin  float64
		largestLoss float64
		hasWin      bool
		hasLoss     bool
	}

	groups := map[time.Time]*bucket{}
	dates := []time.Time{}

	for _, t := range trades {
		if t.ExitTime == nil {
			continue // not closed
		}
		// Compute date in trading timezone.
		y, mon, d := t.ExitTime.In(b.Timezone).Date()
		key := time.Date(y, mon, d, 0, 0, 0, 0, b.Timezone)

		bk, ok := groups[key]
		if !ok {
			bk = &bucket{}
			groups[key] = bk
			dates = append(dates, key)
		}
		bk.count++
		if t.GrossPnl != nil {
			bk.gross += *t.GrossPnl
		}
		if t.Commission != nil {
			bk.commission += *t.Commission
		}
		if t.NetPnl != nil {
			bk.net += *t.NetPnl
			switch {
			case *t.NetPnl > 0:
				bk.wins++
				if !bk.hasWin || *t.NetPnl > bk.largestWin {
					bk.largestWin = *t.NetPnl
					bk.hasWin = true
				}
			case *t.NetPnl < 0:
				bk.losses++
				if !bk.hasLoss || *t.NetPnl < bk.largestLoss {
					bk.largestLoss = *t.NetPnl
					bk.hasLoss = true
				}
			}
		}
	}

	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	for _, d := range dates {
		bk := groups[d]
		var lw, ll *float64
		if bk.hasWin {
			lw = &bk.largestWin
		}
		if bk.hasLoss {
			ll = &bk.largestLoss
		}
		if err := b.Queries.CreateDailyStat(ctx, models.CreateDailyStatParams{
			AccountID:   accountID,
			StatDate:    d,
			TradesCount: bk.count,
			Wins:        bk.wins,
			Losses:      bk.losses,
			GrossPnl:    bk.gross,
			NetPnl:      bk.net,
			Commission:  bk.commission,
			LargestWin:  lw,
			LargestLoss: ll,
		}); err != nil {
			return fmt.Errorf("insert daily_stat %s: %w", d.Format("2006-01-02"), err)
		}
	}
	return nil
}
