package excursion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Service fills trades.mae_* / mfe_* from the bar cache. It never fetches:
// the prefetcher (after an import, nightly) and the chart loader put bars
// in the cache, and the service reads what is there. A trade without bars
// stays unfilled and is retried on the next pass, up to MaxAge back.
type Service struct {
	DB     *sql.DB
	Q      *models.Queries
	Bars   *marketdata.CachingSource
	Logger *slog.Logger
	// MaxAge bounds how far back FillAccount looks; intraday bars older
	// than the cache retention never arrive. Default 120 days.
	MaxAge time.Duration
	// Changed, when set, is told the user whose trades gained excursions
	// (after the writes), so open pages refresh.
	Changed func(userID int64)
}

// Trade is what the measurement needs from a closed trade.
type Trade struct {
	ID         int64
	SymbolRoot string
	Direction  string
	Qty        float64
	EntryPrice float64
	EntryTime  time.Time
	ExitTime   time.Time
}

// DefaultMaxAge is how far back the fillers look when MaxAge is unset.
const DefaultMaxAge = 120 * 24 * time.Hour

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Service) maxAge() time.Duration {
	if s.MaxAge > 0 {
		return s.MaxAge
	}
	return DefaultMaxAge
}

// Fill measures one trade from cached bars and stores the result. ok is
// false, with no error, when the symbol has no market data, nothing is
// cached for the holding time, or the cached bars are too coarse.
func (s *Service) Fill(ctx context.Context, t Trade) (Excursion, bool, error) {
	inst, err := s.Q.GetInstrument(ctx, t.SymbolRoot)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Excursion{}, false, fmt.Errorf("load instrument: %w", err)
	}
	return s.fill(ctx, t, inst)
}

func (s *Service) fill(ctx context.Context, t Trade, inst models.Instrument) (Excursion, bool, error) {
	sym, ok := marketdata.YahooSymbol(t.SymbolRoot)
	if !ok || !t.ExitTime.After(t.EntryTime) {
		return Excursion{}, false, nil
	}
	// Pad the read so a bar that opened before the entry but overlaps it
	// is in the set: an hour covers hourly bars, a day covers daily ones.
	pad := time.Hour
	if t.ExitTime.Sub(t.EntryTime) >= 24*time.Hour {
		pad = 24 * time.Hour
	}
	// One-second bars around a short trade, when the exchange feed fetched
	// them (marketdata.FineFetcher): a trade that lasted a minute is one
	// candle at one minute and its heat is a guess there.
	bars, tf, _, err := s.Bars.BarsForWindow(ctx, sym, t.EntryTime.Add(-pad), t.ExitTime.Add(time.Second))
	if err != nil {
		return Excursion{}, false, fmt.Errorf("bars for %s: %w", sym, err)
	}
	if fine, ok, err := s.Bars.FineBars(ctx, sym, t.EntryTime.Add(-time.Second), t.ExitTime.Add(time.Second)); err == nil && ok {
		bars, tf = fine, marketdata.TF1s
	}
	ex, ok := Compute(t.Direction, t.EntryPrice, t.EntryTime, t.ExitTime, bars, tf)
	if !ok {
		return Excursion{}, false, nil
	}
	tfs := string(ex.Timeframe)
	if err := s.Q.SetTradeExcursion(ctx, models.SetTradeExcursionParams{
		MaePoints:   &ex.MAEPoints,
		MfePoints:   &ex.MFEPoints,
		MaePnl:      Money(ex.MAEPoints, t.Qty, inst),
		MfePnl:      Money(ex.MFEPoints, t.Qty, inst),
		ExcursionTf: &tfs,
		ID:          t.ID,
	}); err != nil {
		return Excursion{}, false, fmt.Errorf("store excursion: %w", err)
	}
	return ex, true, nil
}

// FillAccount measures every closed trade of the account that still lacks
// an excursion and has bars, newest first. Returns how many it filled.
func (s *Service) FillAccount(ctx context.Context, accountID int64) (int, error) {
	cut := time.Now().Add(-s.maxAge()).UTC()
	rows, err := s.Q.ListTradesMissingExcursion(ctx, models.ListTradesMissingExcursionParams{AccountID: accountID, ExitTime: &cut, Limit: 5000})
	if err != nil {
		return 0, fmt.Errorf("list trades: %w", err)
	}
	insts := map[string]models.Instrument{}
	filled := 0
	for _, r := range rows {
		if ctx.Err() != nil {
			return filled, ctx.Err()
		}
		if r.ExitTime == nil {
			continue
		}
		inst, seen := insts[r.SymbolRoot]
		if !seen {
			inst, err = s.Q.GetInstrument(ctx, r.SymbolRoot)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return filled, fmt.Errorf("load instrument %s: %w", r.SymbolRoot, err)
			}
			insts[r.SymbolRoot] = inst
		}
		_, ok, err := s.fill(ctx, Trade{
			ID: r.ID, SymbolRoot: r.SymbolRoot, Direction: r.Direction, Qty: r.Qty,
			EntryPrice: r.EntryPrice, EntryTime: r.EntryTime, ExitTime: *r.ExitTime,
		}, inst)
		if err != nil {
			return filled, err
		}
		if ok {
			filled++
		}
	}
	if filled > 0 && s.Changed != nil {
		var uid int64
		if err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id = ?`, accountID).Scan(&uid); err == nil {
			s.Changed(uid)
		}
	}
	return filled, nil
}

// FillAll runs FillAccount over every account with unfilled trades; errors
// are logged, not returned (boot-time and nightly background use).
func (s *Service) FillAll(ctx context.Context) {
	cut := time.Now().Add(-s.maxAge()).UTC()
	ids, err := s.Q.ListAccountsMissingExcursion(ctx, &cut)
	if err != nil {
		s.logger().Warn("excursion: list accounts", "err", err)
		return
	}
	total := 0
	for _, id := range ids {
		n, err := s.FillAccount(ctx, id)
		total += n
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger().Warn("excursion: fill account", "account_id", id, "err", err)
		}
	}
	if total > 0 {
		s.logger().Info("excursion: filled trades from cached bars", "trades", total, "accounts", len(ids))
	}
}
