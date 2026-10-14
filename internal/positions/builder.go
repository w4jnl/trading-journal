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
//   - Commission per leg: a fill carrying a broker-reported commission
//     (executions.commission, populated by the TopstepX API sync) is charged
//     that actual amount, pro-rated by the quantity consumed in the trade.
//     Fills without one (CSV / MotiveWave sources) fall back to the rate table:
//     (instrument.asset_class, account.firm, instrument.size_class) ->
//     commission_rates, effective-dated, picking the per-side version in effect
//     at the fill's own exec_time. Mixed trades combine actual + rate per leg.
//     A leg with neither (missing firm, uncategorized instrument, or no rate
//     versions) -> commission NULL. net_pnl = gross_pnl minus
//     commission-or-zero, but stays NULL if gross_pnl is NULL.
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

	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/models"
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
	// UserID owns the accounts this builder rebuilds. Set by the importer
	// or admin handler that constructs the Builder; used to scope account
	// lookups so a user can't trigger a rebuild against someone else's data.
	UserID int64
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
	// Copy sets span accounts, so they are recomputed for the whole user
	// once the trades of every touched key are back.
	if b.UserID != 0 {
		if _, err := copyset.Rebuild(ctx, b.Queries, b.UserID); err != nil {
			return res, err
		}
		// Legs that joined a copied decision after it was planned inherit
		// the set's plan (open sets and the closed sets just rebuilt).
		if err := InheritSetPlans(ctx, b.Queries, b.UserID, nil); err != nil {
			return res, fmt.Errorf("inherit set plans: %w", err)
		}
	}
	return res, nil
}

func (b *Builder) rebuildOne(ctx context.Context, k Key) (int, error) {
	// Snapshot journal entries that point to trades we're about to delete so
	// we can re-link them after the replay creates fresh trade rows. The
	// journal_entries.trade_id FK is NO ACTION, so failing to detach first
	// would make the DELETE trip the constraint. Trades reconstructed from
	// the same executions land on the same entry_time, which is the signal
	// we re-link on.
	links, err := b.Queries.ListJournalLinksForAccountSymbol(ctx, models.ListJournalLinksForAccountSymbolParams{
		AccountID: k.AccountID,
		Symbol:    k.Symbol,
	})
	if err != nil {
		return 0, fmt.Errorf("list journal links: %w", err)
	}
	if len(links) > 0 {
		if err := b.Queries.ClearJournalLinksForAccountSymbol(ctx, models.ClearJournalLinksForAccountSymbolParams{
			AccountID: k.AccountID,
			Symbol:    k.Symbol,
		}); err != nil {
			return 0, fmt.Errorf("clear journal links: %w", err)
		}
	}

	// Snapshot group-journal links (M:N journal_entry_trades) for this
	// (account, symbol) before the DELETE+CASCADE wipes them. No manual
	// clear needed - the FK CASCADE handles that; we just need to know
	// what was there so we can re-insert after the replay produces fresh
	// trades with the same entry_time. Same fallback as trade-scope
	// entries: if entry_time shifts, the link is silently dropped.
	groupLinks, err := b.Queries.ListGroupLinksForAccountSymbol(ctx, models.ListGroupLinksForAccountSymbolParams{
		AccountID: k.AccountID,
		Symbol:    k.Symbol,
	})
	if err != nil {
		return 0, fmt.Errorf("list group links: %w", err)
	}

	// Remember each linked trade's opening fill while the junction rows still
	// exist; the relink below keys on it (see there).
	entryExecByTrade := map[int64]int64{}
	for _, link := range links {
		if _, seen := entryExecByTrade[link.TradeID]; !seen {
			entryExecByTrade[link.TradeID] = b.firstEntryExec(ctx, link.TradeID)
		}
	}
	for _, link := range groupLinks {
		if _, seen := entryExecByTrade[link.TradeID]; !seen {
			entryExecByTrade[link.TradeID] = b.firstEntryExec(ctx, link.TradeID)
		}
	}
	// Planned stops, risks and targets ride along the same way: a trade's plan is
	// the trader's, not the replay's, and the delete below would lose it.
	plans, err := b.Queries.ListTradePlansForAccountSymbol(ctx, models.ListTradePlansForAccountSymbolParams{
		AccountID: k.AccountID,
		Symbol:    k.Symbol,
	})
	if err != nil {
		return 0, fmt.Errorf("list trade plans: %w", err)
	}
	for _, p := range plans {
		if _, seen := entryExecByTrade[p.ID]; !seen {
			entryExecByTrade[p.ID] = b.firstEntryExec(ctx, p.ID)
		}
	}
	// Excursions (MAE/MFE off cached bars) are derived, but re-reading
	// bars for every rebuilt trade is work the replay need not repeat:
	// keep them where the trade comes back with the same boundaries and
	// let the filler recompute the rest.
	excursions, err := b.Queries.ListTradeExcursionsForAccountSymbol(ctx, models.ListTradeExcursionsForAccountSymbolParams{
		AccountID: k.AccountID,
		Symbol:    k.Symbol,
	})
	if err != nil {
		return 0, fmt.Errorf("list trade excursions: %w", err)
	}
	for _, e := range excursions {
		if _, seen := entryExecByTrade[e.ID]; !seen {
			entryExecByTrade[e.ID] = b.firstEntryExec(ctx, e.ID)
		}
	}
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

	account, err := b.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{
		ID:     k.AccountID,
		UserID: b.UserID,
	})
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

	// Re-link the journal entries we detached up top. The identity that
	// survives a rebuild is the fill that OPENED the trade: execution ids are
	// stable while trade ids are not, and a fill is the entry of exactly one
	// trade. entry_time alone is ambiguous whenever two trades open in the
	// same second (one-second CSV timestamps; a sell@T that flips into a new
	// position at the same T) - it used to attach a note to whichever trade
	// happened to sort first. When the opening fill no longer opens a trade
	// (fills were removed and the FIFO re-paired), fall back to entry_time;
	// when that misses too, leave trade_id NULL — the entry becomes a
	// day-level note, the closest sensible fallback.
	for _, link := range links {
		newID, err := b.findRebuiltTrade(ctx, k, entryExecByTrade[link.TradeID], link.EntryTime)
		switch {
		case err == nil:
			if err := b.Queries.RelinkJournalEntry(ctx, models.RelinkJournalEntryParams{
				TradeID: &newID,
				ID:      link.JournalID,
			}); err != nil {
				return closed, fmt.Errorf("relink journal entry %d: %w", link.JournalID, err)
			}
		case errors.Is(err, sql.ErrNoRows):
			// boundaries shifted; entry stays detached.
		default:
			return closed, fmt.Errorf("find trade by signal: %w", err)
		}
	}

	// Re-link group entries (M:N) the same way. The CASCADE wiped the
	// link rows when DeleteTradesForAccountSymbol ran; re-insert now.
	for _, link := range groupLinks {
		newID, err := b.findRebuiltTrade(ctx, k, entryExecByTrade[link.TradeID], link.EntryTime)
		switch {
		case err == nil:
			if err := b.Queries.InsertGroupLink(ctx, models.InsertGroupLinkParams{
				JournalEntryID: link.JournalEntryID,
				TradeID:        newID,
			}); err != nil {
				return closed, fmt.Errorf("relink group entry %d: %w", link.JournalEntryID, err)
			}
		case errors.Is(err, sql.ErrNoRows):
			// boundaries shifted; group link silently drops (same fallback).
		default:
			return closed, fmt.Errorf("find trade by signal (group): %w", err)
		}
	}

	// Restore the plans. When the stop is known the risk is recomputed
	// against the rebuilt trade (a re-paired trade can open at a different
	// average or size); a typed-in risk without a stop is kept as is.
	for _, p := range plans {
		newID, err := b.findRebuiltTrade(ctx, k, entryExecByTrade[p.ID], p.EntryTime)
		switch {
		case err == nil:
			risk := p.PlannedRisk
			if p.PlannedStop != nil {
				if t, err := b.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: newID, UserID: b.UserID}); err == nil {
					if r := PlannedRisk(t.EntryPrice, *p.PlannedStop, t.Qty, instrument); r != nil {
						risk = r
					}
				}
			}
			if err := b.Queries.RestoreTradePlan(ctx, models.RestoreTradePlanParams{
				PlannedStop: p.PlannedStop, PlannedRisk: risk, PlannedTarget: p.PlannedTarget, ID: newID,
			}); err != nil {
				return closed, fmt.Errorf("restore trade plan %d: %w", newID, err)
			}
		case errors.Is(err, sql.ErrNoRows):
			// the trade is gone with its boundaries; so is its plan.
		default:
			return closed, fmt.Errorf("find trade by signal (plan): %w", err)
		}
	}

	// Restore the excursions whose trade came back unchanged: same entry
	// price, size and exit, so the bars it was read from still apply. A
	// re-paired trade drops it and is measured again from the cache.
	for _, e := range excursions {
		newID, err := b.findRebuiltTrade(ctx, k, entryExecByTrade[e.ID], e.EntryTime)
		switch {
		case err == nil:
			t, err := b.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: newID, UserID: b.UserID})
			if err != nil || t.ExitTime == nil || e.ExitTime == nil || !t.ExitTime.Equal(*e.ExitTime) ||
				t.EntryPrice != e.EntryPrice || t.Qty != e.Qty {
				continue
			}
			if err := b.Queries.SetTradeExcursion(ctx, models.SetTradeExcursionParams{
				MaePoints: e.MaePoints, MfePoints: e.MfePoints, MaePnl: e.MaePnl, MfePnl: e.MfePnl,
				ExcursionTf: e.ExcursionTf, ID: newID,
			}); err != nil {
				return closed, fmt.Errorf("restore trade excursion %d: %w", newID, err)
			}
		case errors.Is(err, sql.ErrNoRows):
			// boundaries shifted; measured again later.
		default:
			return closed, fmt.Errorf("find trade by signal (excursion): %w", err)
		}
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
	// commPerUnit is the execution's broker-reported commission divided by its
	// full quantity (see commissionPerUnit). Non-nil means "actual commission
	// known for this fill" and takes precedence over the rate-table lookup;
	// partial consumption and flips pro-rate naturally as qty * commPerUnit.
	commPerUnit *float64
}

// commissionPerUnit converts an execution's broker-reported total commission
// into a per-contract figure. nil when the execution carries no commission
// (CSV / MotiveWave sources) or has a non-positive quantity.
func commissionPerUnit(fill models.Execution) *float64 {
	if fill.Commission == nil || fill.Qty <= 0 {
		return nil
	}
	v := *fill.Commission / fill.Qty
	return &v
}

// tradeAccum accumulates state for one in-progress trade until it closes.
type tradeAccum struct {
	direction string // "long" | "short"
	entries   []fillUse
	exits     []fillUse
	maxQty    float64
	// opened is the fill that started the cycle (the first entry; on a flip,
	// the flip fill): the anchor of the position's plan (HandOffPlan).
	opened models.Execution
}

func (b *Builder) replay(ctx context.Context, k Key, fills []models.Execution, account models.Account, inst models.Instrument) (int, error) {
	var openLots []lot
	var trade *tradeAccum
	closed := 0
	// openQty mirrors sum(openLots[*].qtyRemaining) — kept in sync as lots
	// are added/consumed so the same-direction branch doesn't need to scan
	// every lot on every fill (was O(n^2) for positions built up over many
	// same-side fills).
	var openQty float64

	// Load the commission-rate versions once for this (account, symbol) replay.
	// The cell (asset_class, firm, size_class) is constant across every trade we
	// emit; each trade then picks, per fill, the version in effect at that fill's
	// exec_time (rateAt) in memory - one DB round-trip regardless of trade count.
	versions, err := b.loadRateVersions(ctx, account, inst)
	if err != nil {
		return 0, err
	}

	openTrade := func(fill models.Execution, dir string, qty float64) {
		trade = &tradeAccum{direction: dir, maxQty: qty, opened: fill}
		openLots = []lot{{qtyRemaining: qty, price: fill.Price, time: fill.ExecTime, execID: fill.ID}}
		openQty = qty
		trade.entries = append(trade.entries, fillUse{
			execID: fill.ID, qty: qty, price: fill.Price, time: fill.ExecTime,
			commPerUnit: commissionPerUnit(fill),
		})
	}

	for _, fill := range fills {
		if fill.Qty <= 0 {
			// A zero or negative quantity would divide the VWAP by zero and
			// store NaN prices; the parsers reject such rows, this is the
			// backstop for hand-edited or API-sourced executions.
			continue
		}
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
				commPerUnit: commissionPerUnit(fill),
			})
			openQty += fill.Qty
			if openQty > trade.maxQty {
				trade.maxQty = openQty
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
		openQty -= consumedTotal
		if openQty < 0 {
			openQty = 0
		}
		if consumedTotal > 0 {
			trade.exits = append(trade.exits, fillUse{
				execID: fill.ID, qty: consumedTotal, price: fill.Price, time: fill.ExecTime,
				commPerUnit: commissionPerUnit(fill),
			})
		}

		if len(openLots) == 0 {
			if err := b.emitTrade(ctx, k, trade, inst, versions); err != nil {
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

// rateVersion is one effective-dated per-side commission rate for a cell.
// loadRateVersions returns them sorted by EffectiveFrom ASC.
type rateVersion struct {
	EffectiveFrom time.Time
	PerSide       float64
}

// loadRateVersions returns every effective-dated per-side rate for this
// (account, instrument) commission cell, oldest first. nil when firm/asset/size
// is missing or no rows exist -> commission stays NULL. Loaded ONCE per replay;
// the per-fill pick is done in Go (rateAt), so a many-trade replay still does
// exactly one DB round-trip - mirroring the balance-anchor as-of pattern.
func (b *Builder) loadRateVersions(ctx context.Context, account models.Account, inst models.Instrument) ([]rateVersion, error) {
	if account.Firm == nil || inst.AssetClass == nil || inst.SizeClass == nil {
		return nil, nil
	}
	rows, err := b.Queries.ListCommissionRateVersionsForCell(ctx, models.ListCommissionRateVersionsForCellParams{
		AssetClass: *inst.AssetClass,
		Firm:       *account.Firm,
		SizeClass:  *inst.SizeClass,
	})
	if err != nil {
		return nil, fmt.Errorf("commission rate versions: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	versions := make([]rateVersion, len(rows))
	for i, r := range rows {
		versions[i] = rateVersion{EffectiveFrom: r.EffectiveFrom, PerSide: r.PerContractPerSide}
	}
	// Sort in Go so rateAt's "ASC" assumption holds authoritatively, independent
	// of how the sqlite driver text-orders DATETIME values written in different
	// formats (migration string literal vs driver-written time.Time).
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].EffectiveFrom.Before(versions[j].EffectiveFrom)
	})
	return versions, nil
}

// rateAt returns the per-side rate in effect at time t: the version with the
// greatest EffectiveFrom <= t. A fill before the earliest version clamps to the
// earliest (matches loadBalances' "all future -> earliest" back-fill). ok is
// false only when there are no versions. versions must be sorted ASC.
func rateAt(versions []rateVersion, t time.Time) (float64, bool) {
	if len(versions) == 0 {
		return 0, false
	}
	rate := versions[0].PerSide // clamp: a fill before the first version uses it
	for _, v := range versions {
		if v.EffectiveFrom.After(t) {
			break // sorted ASC: nothing later can apply either
		}
		rate = v.PerSide
	}
	return rate, true
}

// commissionForLegs sums the commission across every entry and exit fill.
// Per leg, precedence is: the fill's actual broker-reported commission
// (commPerUnit, pro-rated by consumed qty) when known, else the rate-table
// rate in effect at that fill's exec_time. A leg with neither an actual
// commission nor an applicable rate version makes the whole trade's
// commission NULL - never a silent partial sum. All-actual trades therefore
// work even on cells with zero configured rate versions; with a single
// constant rate and no actuals this equals rate * cycleQty * 2 (both legs
// of the round-turn), so CSV-only history is unchanged.
func commissionForLegs(entries, exits []fillUse, versions []rateVersion) *float64 {
	total, unknown := 0.0, false
	add := func(u fillUse) {
		switch {
		case u.commPerUnit != nil:
			total += u.qty * *u.commPerUnit // actual, pro-rated by consumed qty
		default:
			r, ok := rateAt(versions, u.time)
			if !ok {
				unknown = true
				return
			}
			total += r * u.qty
		}
	}
	for _, u := range entries {
		add(u)
	}
	for _, u := range exits {
		add(u)
	}
	if unknown {
		return nil
	}
	return &total
}

func (b *Builder) emitTrade(ctx context.Context, k Key, ta *tradeAccum, inst models.Instrument, versions []rateVersion) error {
	entryAgg := aggregateByExec(ta.entries)
	exitAgg := aggregateByExec(ta.exits)

	entryTotal, entryWeighted, entryFirst := summarize(ta.entries, true)
	exitTotal, exitWeighted, exitLast := summarize(ta.exits, false)
	avgEntry := entryWeighted / entryTotal
	avgExit := exitWeighted / exitTotal

	// Per-side commission, each leg charged at the rate in effect at its own
	// exec_time (so a trade straddling a rate change pays old rate on entry,
	// new rate on exit).
	legCommission := commissionForLegs(ta.entries, ta.exits, versions)
	grossPnl, commission, netPnl := computePnl(ta.direction, avgEntry, avgExit, exitTotal, inst, legCommission)

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
	// The plan the trader set while the position was open follows it into
	// the trade (first close), or is re-linked to the rebuilt row.
	if err := HandOffPlan(ctx, b.Queries, ta.opened, row, inst); err != nil {
		return err
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

// firstEntryExec returns the execution id that opened tradeID, or 0 when the
// trade has no entry-role junction row (never for a builder-made trade).
func (b *Builder) firstEntryExec(ctx context.Context, tradeID int64) int64 {
	id, err := b.Queries.FirstEntryExecutionForTrade(ctx, tradeID)
	if err != nil {
		return 0
	}
	return id
}

// findRebuiltTrade resolves the trade a journal link should attach to after
// the rebuild: the trade opened by execID when known, else the trade with
// the original entry_time. sql.ErrNoRows means neither matched.
func (b *Builder) findRebuiltTrade(ctx context.Context, k Key, execID int64, entryTime time.Time) (int64, error) {
	if execID != 0 {
		id, err := b.Queries.FindTradeByEntryExecution(ctx, models.FindTradeByEntryExecutionParams{
			ExecutionID: execID, AccountID: k.AccountID, Symbol: k.Symbol,
		})
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	return b.Queries.FindTradeBySignal(ctx, models.FindTradeBySignalParams{
		AccountID: k.AccountID, Symbol: k.Symbol, EntryTime: entryTime,
	})
}

// computePnl computes a closed trade's gross and net given a pre-computed
// commission (from commissionForLegs). Futures P&L is price distance in
// ticks x tick value x qty (CLAUDE.md invariant; the same math the open
// positions' unrealized P&L uses), so a complete tick spec is all an
// instrument needs. multiplier - which IS tick_value / tick_size - is kept
// only as the fallback for an instrument that was configured before tick
// specs were required. gross is NULL when neither is set (a stub awaiting
// setup) - and then commission/net are NULL too, pending the user
// configuring the symbol. A nil commission (no rate configured) leaves
// net = gross. User fixes missing rates via /commissions.
func computePnl(direction string, avgEntry, avgExit, qty float64, inst models.Instrument, commission *float64) (gross, comm, net *float64) {
	var mult float64
	switch {
	case inst.TickSize != nil && inst.TickValue != nil && *inst.TickSize > 0:
		mult = *inst.TickValue / *inst.TickSize
	case inst.Multiplier != nil:
		mult = *inst.Multiplier
	default:
		return nil, nil, nil
	}
	g := 0.0
	if direction == "long" {
		g = (avgExit - avgEntry) * mult * qty
	} else {
		g = (avgEntry - avgExit) * mult * qty
	}
	gross = &g
	comm = commission
	n := g
	if commission != nil {
		n = g - *commission
	}
	net = &n
	return gross, comm, net
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
