package marketdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---- Exchange bars: the feed and the one-second layer ------------------
//
// The vendor's continuous series fills the cache by whole days. An
// ExchangeSource (a user's TopstepX session) can do better for the roots
// being traded: the exact contract, no delay, true volume for the recent
// days at one minute, and one-second bars around short trades, which one
// minute cannot judge (a trade that lasted forty seconds is one candle).
// Second bars are a layer of their own: stored in price_bars as '1s', listed
// in price_bar_windows by the stretch they cover, never rolled into a day
// read, and read by the excursion filler and the chart when a window covers
// the trade.

// Feed names recorded on a cached day.
const (
	SourceVendor   = "yahoo"
	SourceExchange = "topstepx"
)

// ExchangeSource fetches bars for a vendor symbol's underlying from the
// exchange feed: unitSeconds is the bar size (1 for a second, 60 for a
// minute). ErrUnsupported when the feed has no contract or session for it.
type ExchangeSource interface {
	FetchExchangeBars(ctx context.Context, yahooSymbol string, unitSeconds int, from, to time.Time) ([]Bar, error)
}

// FinePad is how far around a short trade the one-second window reaches.
const FinePad = 2 * time.Minute

// ShortTrade is how long a trade may last to earn one-second bars.
const ShortTrade = 10 * time.Minute

// FineBars returns the one-second bars covering [from, to) when a stored
// window covers that whole stretch; ok is false otherwise (the caller
// falls back to the day's bars).
func (c *CachingSource) FineBars(ctx context.Context, yahooSymbol string, from, to time.Time) ([]Bar, bool, error) {
	var n int
	if err := c.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM price_bar_windows WHERE yahoo_symbol = ? AND timeframe = '1s' AND from_time <= ? AND to_time >= ?`,
		yahooSymbol, from.Unix(), to.Unix()).Scan(&n); err != nil {
		return nil, false, err
	}
	if n == 0 {
		return nil, false, nil
	}
	rows, err := c.DB.QueryContext(ctx,
		`SELECT bar_time, open, high, low, close, volume FROM price_bars
		 WHERE yahoo_symbol = ? AND timeframe = '1s' AND bar_time >= ? AND bar_time < ? ORDER BY bar_time`,
		yahooSymbol, from.Unix(), to.Unix())
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var bars []Bar
	for rows.Next() {
		var b Bar
		if err := rows.Scan(&b.Time, &b.Open, &b.High, &b.Low, &b.Close, &b.Volume); err != nil {
			return nil, false, err
		}
		bars = append(bars, b)
	}
	return bars, len(bars) > 0, rows.Err()
}

// FetchFine fetches and stores the one-second bars for [from, to) from the
// exchange feed. A stretch already covered is not fetched again. ErrNoData
// when the feed answered nothing (the window is still recorded, so it is
// not retried forever); ErrUnsupported without a feed.
func (c *CachingSource) FetchFine(ctx context.Context, yahooSymbol string, from, to time.Time) error {
	if c.Exchange == nil {
		return ErrUnsupported
	}
	if _, ok, err := c.FineBars(ctx, yahooSymbol, from, to); err != nil || ok {
		return err
	}
	bars, err := c.Exchange.FetchExchangeBars(ctx, yahooSymbol, 1, from, to)
	if err != nil && !errors.Is(err, ErrNoData) {
		return err
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("marketdata: begin: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if len(bars) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR REPLACE INTO price_bars (yahoo_symbol, timeframe, bar_time, open, high, low, close, volume, fetched_at)
			 VALUES (?, '1s', ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("marketdata: prepare: %w", err)
		}
		defer stmt.Close()
		for _, b := range bars {
			if _, err := stmt.ExecContext(ctx, yahooSymbol, b.Time, b.Open, b.High, b.Low, b.Close, b.Volume, now); err != nil {
				return fmt.Errorf("marketdata: insert fine bar: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO price_bar_windows (yahoo_symbol, timeframe, from_time, to_time, source, fetched_at) VALUES (?, '1s', ?, ?, ?, ?)`,
		yahooSymbol, from.Unix(), to.Unix(), SourceExchange, now); err != nil {
		return fmt.Errorf("marketdata: mark window: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(bars) == 0 {
		return ErrNoData
	}
	return nil
}

// FineTrade is a short closed trade the fine fetcher serves (what a
// re-measurement needs, so the caller need not load it again).
type FineTrade struct {
	ID         int64
	SymbolRoot string
	Direction  string
	Qty        float64
	EntryPrice float64
	EntryTime  time.Time
	ExitTime   time.Time
}

// FineFetcher gives recent short trades their one-second bars: every trade
// shorter than ShortTrade that closed in the last days and has no window
// yet. AfterTrade, when set, is told each trade that gained bars (the
// excursion filler measures it again from them).
type FineFetcher struct {
	Cache      *CachingSource
	DB         *sql.DB
	RecentDays int // default 7
	AfterTrade func(ctx context.Context, t FineTrade)
}

// Run fetches the fine bars of every recent short trade without a window.
// Returns how many trades gained bars; a rate limit ends the round.
func (f *FineFetcher) Run(ctx context.Context) (int, error) {
	if f == nil || f.Cache == nil || f.Cache.Exchange == nil {
		return 0, nil
	}
	days := f.RecentDays
	if days <= 0 {
		days = 7
	}
	rows, err := f.DB.QueryContext(ctx, `
		SELECT t.id, t.symbol_root, t.direction, t.qty, t.entry_price, t.entry_time, t.exit_time
		FROM trades t
		WHERE t.status = 'closed' AND t.exit_time IS NOT NULL AND t.exit_time >= ?
		  AND (unixepoch(substr(t.exit_time, 1, 19)) - unixepoch(substr(t.entry_time, 1, 19))) < ?
		ORDER BY t.exit_time DESC LIMIT 500`,
		time.Now().AddDate(0, 0, -days).UTC(), int64(ShortTrade/time.Second))
	if err != nil {
		return 0, fmt.Errorf("marketdata: short trades: %w", err)
	}
	var trades []FineTrade
	for rows.Next() {
		var t FineTrade
		if err := rows.Scan(&t.ID, &t.SymbolRoot, &t.Direction, &t.Qty, &t.EntryPrice, &t.EntryTime, &t.ExitTime); err != nil {
			rows.Close()
			return 0, err
		}
		trades = append(trades, t)
	}
	rows.Close()
	done := 0
	for _, t := range trades {
		sym, ok := YahooSymbol(t.SymbolRoot)
		if !ok {
			continue
		}
		from, to := t.EntryTime.Add(-FinePad), t.ExitTime.Add(FinePad)
		if _, covered, err := f.Cache.FineBars(ctx, sym, from, to); err != nil || covered {
			continue
		}
		err := f.Cache.FetchFine(ctx, sym, from, to)
		switch {
		case errors.Is(err, ErrRateLimited):
			return done, err
		case err != nil:
			continue
		}
		done++
		if f.AfterTrade != nil {
			f.AfterTrade(ctx, t)
		}
	}
	return done, nil
}
