package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/w4jnl/trading-journal/internal/journal"
)

// analyticsSummary is the one analytics read for programs: the REST
// endpoint and the MCP tool both return it, so an analyst does not have to
// recompute stats from trade lists. Everything in it is what /analytics
// shows for the same filter.
type analyticsSummary struct {
	From       string                 `json:"from"`
	To         string                 `json:"to"`
	Timezone   string                 `json:"timezone"`
	Totals     analyticsTotals        `json:"totals"`
	R          analyticsR             `json:"r"`
	Excursion  analyticsExcursion     `json:"excursion"`
	Drawdown   drawdownMetrics        `json:"drawdown"`
	Streaks    streakMetrics          `json:"streaks"`
	Breakdowns map[string][]bucketOut `json:"breakdowns"` // bySymbol, byDirection, byStrategy, byTag, byMistake, byEmotion, byDayOfWeek, byHour
}

type analyticsTotals struct {
	Trades       int64    `json:"trades"`
	Wins         int64    `json:"wins"`
	Losses       int64    `json:"losses"`
	WinRate      float64  `json:"winRate"`
	Net          float64  `json:"net"`
	Gross        float64  `json:"gross"`
	Commission   float64  `json:"commission"`
	AvgWin       float64  `json:"avgWin"`
	AvgLoss      float64  `json:"avgLoss"` // negative
	PayoffRatio  *float64 `json:"payoffRatio"`
	ProfitFactor *float64 `json:"profitFactor"` // null when there are no losses
	Expectancy   float64  `json:"expectancy"`   // per trade, in money
	LargestWin   float64  `json:"largestWin"`
	LargestLoss  float64  `json:"largestLoss"`
}

type analyticsR struct {
	PlannedTrades int64   `json:"plannedTrades"` // trades carrying a planned risk
	AvgR          float64 `json:"avgR"`          // mean R = expectancy in risk units
	WinRate       float64 `json:"winRate"`
	AvgWinR       float64 `json:"avgWinR"`
	AvgLossR      float64 `json:"avgLossR"` // negative
}

// analyticsExcursion is the price-path view (internal/excursion) over the
// trades measured from cached bars; the other fields mean nothing when
// trades is 0.
type analyticsExcursion struct {
	Trades         int64   `json:"trades"`
	ExitEfficiency float64 `json:"exitEfficiency"` // total gross / total MFE
	AvgMae         float64 `json:"avgMae"`
	AvgMfe         float64 `json:"avgMfe"`
	WinnersAvgMae  float64 `json:"winnersAvgMae"` // heat a winner sat through
	LosersAvgMfe   float64 `json:"losersAvgMfe"`  // profit a loser had before turning
}

type bucketOut struct {
	Key    string  `json:"key"`
	Trades int64   `json:"trades"`
	Wins   int64   `json:"wins"`
	Losses int64   `json:"losses"`
	Net    float64 `json:"net"`
	Gross  float64 `json:"gross"`
	Best   float64 `json:"best"`
	Worst  float64 `json:"worst"`
}

func bucketsOut(bs []analyticsBucket) []bucketOut {
	out := make([]bucketOut, 0, len(bs))
	for _, b := range bs {
		out = append(out, bucketOut{Key: b.Key, Trades: b.Trades, Wins: b.Wins, Losses: b.Losses, Net: b.Net, Gross: b.Gross, Best: b.Best, Worst: b.Worst})
	}
	return out
}

// analyticsSummaryFor computes the summary for the user's trades that
// exited inside rng (trading-tz days) under the account filter.
func (h *Handlers) analyticsSummaryFor(ctx context.Context, af accountFilter, rng apiDateRange) (analyticsSummary, error) {
	tz := h.localTZ()
	from, to := rng.From.UTC(), rng.ToExclusive().UTC()
	trades, err := h.fetchAnalyticsTrades(ctx, af, &from, &to)
	if err != nil {
		return analyticsSummary{}, fmt.Errorf("fetch trades: %w", err)
	}
	hl := computeHeadline(trades)
	daily := dailySeriesFromTrades(trades, tz)
	totals := analyticsTotals{
		Trades: hl.Trades, Wins: hl.Wins, Losses: hl.Losses, WinRate: hl.WinRate,
		Net: hl.NetPnl, Gross: hl.GrossPnl, Commission: hl.Commission,
		AvgWin: hl.AvgWin, AvgLoss: hl.AvgLoss, Expectancy: hl.Expectancy,
		LargestWin: hl.LargestWin, LargestLoss: hl.LargestLoss,
	}
	if hl.AvgLoss < 0 {
		v := hl.AvgWin / -hl.AvgLoss
		totals.PayoffRatio = &v
	}
	if hl.Losses > 0 {
		v := hl.ProfitFactor
		totals.ProfitFactor = &v
	}
	return analyticsSummary{
		From: rng.From.Format("2006-01-02"), To: rng.To.Format("2006-01-02"), Timezone: tz.String(),
		Totals: totals,
		R:      analyticsR{PlannedTrades: hl.RTrades, AvgR: hl.AvgR, WinRate: hl.WinRateR, AvgWinR: hl.AvgWinR, AvgLossR: hl.AvgLossR},
		Excursion: analyticsExcursion{
			Trades: hl.ExcTrades, ExitEfficiency: hl.ExitEfficiency, AvgMae: hl.AvgMAE, AvgMfe: hl.AvgMFE,
			WinnersAvgMae: hl.Heat, LosersAvgMfe: hl.GaveBack,
		},
		Drawdown: computeDrawdown(trades),
		Streaks:  computeStreaks(trades, daily),
		Breakdowns: map[string][]bucketOut{
			"bySymbol":    bucketsOut(bucketBy(trades, func(t analyticsTrade) string { return t.SymbolRoot })),
			"byDirection": bucketsOut(bucketBy(trades, func(t analyticsTrade) string { return t.Direction })),
			"byStrategy":  bucketsOut(bucketBy(trades, func(t analyticsTrade) string { return orLabel(t.Strategy, "(no strategy)") })),
			"byTag":       bucketsOut(bucketBy(trades, func(t analyticsTrade) string { return orLabel(t.Tag, "(untagged)") })),
			"byMistake":   bucketsOut(bucketByMulti(trades, labelKeys(journal.KindMistake, func(t analyticsTrade) []string { return t.Mistakes }, "(none logged)"))),
			"byEmotion":   bucketsOut(bucketByMulti(trades, labelKeys(journal.KindEmotion, func(t analyticsTrade) []string { return t.Emotions }, "(none logged)"))),
			"byDayOfWeek": bucketsOut(bucketBy(trades, func(t analyticsTrade) string { return weekdayLabel(t.EntryTime.In(tz).Weekday()) })),
			"byHour":      bucketsOut(bucketBy(trades, func(t analyticsTrade) string { return fmt.Sprintf("%02d:00", t.EntryTime.In(tz).Hour()) })),
		},
	}, nil
}

// APIAnalyticsSummary: GET /api/v1/analytics/summary?from=&to=&day=&filter=
// Same parameters as /stats/daily.
func (h *Handlers) APIAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	af, rng, err := h.statsRangeAndFilter(r)
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
			return
		}
		h.logger().Warn("api analytics: filter failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not resolve account filter")
		return
	}
	h.syncForAPIRead(r.Context(), apiWantsRefresh(r))
	out, err := h.analyticsSummaryFor(r.Context(), af, rng)
	if err != nil {
		h.logger().Warn("api analytics: load failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not load analytics")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
