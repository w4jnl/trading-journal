package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// GET /api/v1/stats/daily - daily P&L + trade stats for consumer apps. The
// contract is docs/api-handoff-001-daily-stats.md; the numbers are the
// Calendar's: the same daily_stats rows (ListDailyStatsForRange), the same
// account rule (accountAdmission), the same per-day merge keyed by the
// trading-TZ date of the trade's EXIT. No arithmetic lives here that the
// pages don't already do - minus the Calendar's Saturday-into-Friday fold,
// which is a grid-presentation fix, not data.
//
// Dates are inclusive on both ends (the analytics form's convention) and
// default to the last 30 days; a malformed range is a 400, because this is a
// programmatic endpoint - the pages silently ignore junk, an API must not.

const (
	apiStatsDefaultSpanDays = 30
	apiStatsMaxSpanDays     = 400
)

// apiDateRange is a validated inclusive day range at trading-TZ midnights.
type apiDateRange struct {
	From time.Time
	To   time.Time
}

// ToExclusive is the half-open upper bound the range queries take.
func (r apiDateRange) ToExclusive() time.Time { return r.To.AddDate(0, 0, 1) }

// Days counts calendar days in the range; the rounding absorbs the 23/25
// hour days around a DST transition.
func (r apiDateRange) Days() int {
	return int(r.ToExclusive().Sub(r.From).Round(24*time.Hour).Hours() / 24)
}

// parseAPIDateRange validates ?from=/?to= (YYYY-MM-DD in tz, inclusive).
// Missing to = today; missing from = to minus the default span. Errors are
// apiValidationError so wrappers map them to 400.
func parseAPIDateRange(fromStr, toStr string, tz *time.Location, today time.Time) (apiDateRange, error) {
	parse := func(name, v string) (time.Time, bool, error) {
		if v == "" {
			return time.Time{}, false, nil
		}
		t, err := time.ParseInLocation("2006-01-02", v, tz)
		if err != nil {
			return time.Time{}, false, badInput("bad %s; expected YYYY-MM-DD", name)
		}
		return t, true, nil
	}
	from, hasFrom, err := parse("from", fromStr)
	if err != nil {
		return apiDateRange{}, err
	}
	to, hasTo, err := parse("to", toStr)
	if err != nil {
		return apiDateRange{}, err
	}
	if !hasTo {
		y, m, d := today.In(tz).Date()
		to = time.Date(y, m, d, 0, 0, 0, 0, tz)
	}
	if !hasFrom {
		from = to.AddDate(0, 0, -(apiStatsDefaultSpanDays - 1))
	}
	rng := apiDateRange{From: from, To: to}
	if to.Before(from) {
		return apiDateRange{}, badInput("to is before from")
	}
	if n := rng.Days(); n > apiStatsMaxSpanDays {
		return apiDateRange{}, badInput("range too large: %d days (max %d)", n, apiStatsMaxSpanDays)
	}
	return rng, nil
}

// ---- projections (camelCase; apiSummary embedded so the stats vocabulary
// is byte-identical to the journal endpoints' "summary") ----

type apiDailyStatsItem struct {
	Date string `json:"date"` // YYYY-MM-DD, trading TZ
	apiSummary
	WinRate     *float64 `json:"winRate"`     // wins/(wins+losses); null when no decided trades
	LargestWin  *float64 `json:"largestWin"`  // null when no winning trade
	LargestLoss *float64 `json:"largestLoss"` // negative; null when no losing trade
}

type apiDayRef struct {
	Date string  `json:"date"`
	Net  float64 `json:"net"`
}

type apiDailyStatsTotals struct {
	apiSummary
	WinRate     *float64   `json:"winRate"`
	WinDays     int64      `json:"winDays"`  // days with net > 0 (Calendar month-total rule)
	LossDays    int64      `json:"lossDays"` // days with net < 0
	BestDay     *apiDayRef `json:"bestDay"`  // null when no items
	WorstDay    *apiDayRef `json:"worstDay"`
	LargestWin  *float64   `json:"largestWin"`
	LargestLoss *float64   `json:"largestLoss"`
}

type apiDailyStats struct {
	From     string              `json:"from"`
	To       string              `json:"to"`
	Timezone string              `json:"timezone"`
	Totals   apiDailyStatsTotals `json:"totals"`
	Items    []apiDailyStatsItem `json:"items"` // ascending; gap days omitted; never null
}

func winRatePtr(wins, losses int64) *float64 {
	if wins+losses == 0 {
		return nil
	}
	v := float64(wins) / float64(wins+losses)
	return &v
}

// maxPtr / minPtr fold a nullable value into a running extreme, copying the
// value so the result never aliases a source row.
func maxPtr(cur, v *float64) *float64 {
	if v == nil {
		return cur
	}
	if cur == nil || *v > *cur {
		x := *v
		return &x
	}
	return cur
}

func minPtr(cur, v *float64) *float64 {
	if v == nil {
		return cur
	}
	if cur == nil || *v < *cur {
		x := *v
		return &x
	}
	return cur
}

// aggregateDailyStats merges the admitted accounts' rows per trading-TZ day
// - the Calendar's loop (calendar.go), plus largest win/loss and win rate.
// NULL-net trades are already counted-but-zero in the cache rows, so no
// special casing is needed here.
func aggregateDailyStats(rows []models.DailyStat, admit func(int64) bool, tz *time.Location) []apiDailyStatsItem {
	byDate := map[string]*apiDailyStatsItem{}
	for _, row := range rows {
		if !admit(row.AccountID) {
			continue
		}
		key := row.StatDate.In(tz).Format("2006-01-02")
		it, ok := byDate[key]
		if !ok {
			it = &apiDailyStatsItem{Date: key}
			byDate[key] = it
		}
		it.Trades += row.TradesCount
		it.Wins += row.Wins
		it.Losses += row.Losses
		it.Net += row.NetPnl
		it.Gross += row.GrossPnl
		it.Commission += row.Commission
		it.LargestWin = maxPtr(it.LargestWin, row.LargestWin)
		it.LargestLoss = minPtr(it.LargestLoss, row.LargestLoss)
	}
	keys := make([]string, 0, len(byDate))
	for k := range byDate {
		keys = append(keys, k)
	}
	sort.Strings(keys) // ISO dates sort lexically
	out := make([]apiDailyStatsItem, 0, len(keys))
	for _, k := range keys {
		it := byDate[k]
		it.WinRate = winRatePtr(it.Wins, it.Losses)
		out = append(out, *it)
	}
	return out
}

func dailyStatsTotals(items []apiDailyStatsItem) apiDailyStatsTotals {
	var t apiDailyStatsTotals
	for i := range items {
		it := &items[i]
		t.Trades += it.Trades
		t.Wins += it.Wins
		t.Losses += it.Losses
		t.Net += it.Net
		t.Gross += it.Gross
		t.Commission += it.Commission
		switch {
		case it.Net > 0:
			t.WinDays++
		case it.Net < 0:
			t.LossDays++
		}
		if t.BestDay == nil || it.Net > t.BestDay.Net {
			t.BestDay = &apiDayRef{Date: it.Date, Net: it.Net}
		}
		if t.WorstDay == nil || it.Net < t.WorstDay.Net {
			t.WorstDay = &apiDayRef{Date: it.Date, Net: it.Net}
		}
		t.LargestWin = maxPtr(t.LargestWin, it.LargestWin)
		t.LargestLoss = minPtr(t.LargestLoss, it.LargestLoss)
	}
	t.WinRate = winRatePtr(t.Wins, t.Losses)
	return t
}

// dailyStats is the shared loader behind the REST endpoint and the MCP tool.
func (h *Handlers) dailyStats(ctx context.Context, af accountFilter, rng apiDateRange) (apiDailyStats, error) {
	tz := h.localTZ()
	rows, err := h.Queries.ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{
		UserID:     userID(ctx),
		StatDate:   rng.From,
		StatDate_2: rng.ToExclusive(),
	})
	if err != nil {
		return apiDailyStats{}, fmt.Errorf("list daily_stats: %w", err)
	}
	retired, err := h.retiredAccountSet(ctx)
	if err != nil {
		return apiDailyStats{}, fmt.Errorf("retired set: %w", err)
	}
	excluded, err := h.statsExcludedAccountSet(ctx)
	if err != nil {
		return apiDailyStats{}, fmt.Errorf("excluded set: %w", err)
	}
	items := aggregateDailyStats(rows, newAccountAdmission(af, retired, excluded).admits, tz)
	return apiDailyStats{
		From:     rng.From.Format("2006-01-02"),
		To:       rng.To.Format("2006-01-02"),
		Timezone: tz.String(),
		Totals:   dailyStatsTotals(items),
		Items:    items,
	}, nil
}

// statsRangeAndFilter parses the shared stats params. Dates are validated
// before any DB work so a 400 is cheap.
func (h *Handlers) statsRangeAndFilter(r *http.Request) (accountFilter, apiDateRange, error) {
	r = bridgeLegacyAccountID(r)
	tz := h.localTZ()
	q := r.URL.Query()
	// day=X is shorthand for from=to=X (the MCP tools' convention); keywords
	// (today|yesterday|last) resolve server-side so the caller never has to
	// know the date in the trading tz.
	fromStr, toStr := q.Get("from"), q.Get("to")
	if day := strings.TrimSpace(q.Get("day")); day != "" {
		fromStr, toStr = day, day
	}
	rng, err := parseAPIDateRange(h.resolveDayKeyword(r.Context(), fromStr, tz), h.resolveDayKeyword(r.Context(), toStr, tz), tz, todayLocal(tz))
	if err != nil {
		return accountFilter{}, apiDateRange{}, err
	}
	af, err := h.resolveAccountFilter(r)
	if err != nil {
		return accountFilter{}, apiDateRange{}, fmt.Errorf("resolve account filter: %w", err)
	}
	return af, rng, nil
}

// APIStatsDaily: GET /api/v1/stats/daily?from=&to=&filter=acc:N|type:slug
func (h *Handlers) APIStatsDaily(w http.ResponseWriter, r *http.Request) {
	af, rng, err := h.statsRangeAndFilter(r)
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.msg)
			return
		}
		h.logger().Warn("api stats: filter failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not resolve account filter")
		return
	}
	// A valid read keeps the broker data moving; refresh=1 also waits for
	// the sync so the answer is current, not the last poll's.
	h.syncForAPIRead(r.Context(), apiWantsRefresh(r))
	out, err := h.dailyStats(r.Context(), af, rng)
	if err != nil {
		h.logger().Warn("api stats: load failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not load daily stats")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
