package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// analyticsTrade is the minimal projection of a closed trade we need for
// computing the analytics page. Pulling from raw SQL keeps the handler in
// charge of the type so we can avoid a full models.Trade allocation per row.
type analyticsTrade struct {
	ID          int64
	AccountID   int64
	Symbol      string
	SymbolRoot  string
	Direction   string
	Qty         float64
	EntryPrice  float64
	ExitPrice   *float64 // NULL until trade closes; we only fetch closed trades but keep nullable
	GrossPnl    *float64
	NetPnl      *float64
	Commission  *float64
	EntryTime   time.Time
	ExitTime    time.Time
	AccountName string
}

// analyticsBucket aggregates trades by some grouping key (symbol, direction,
// day-of-week, etc).
type analyticsBucket struct {
	Key       string
	Trades    int64
	Wins      int64
	Losses    int64
	Net       float64
	Gross     float64
	Best      float64
	Worst     float64
	HasBW     bool // whether Best/Worst have valid values
}

// AnalyticsPage renders /analytics — aggregate metrics plus per-symbol /
// per-direction / per-day-of-week / per-hour breakdowns, filterable by
// account and date range. No chart yet — the headline metrics give the
// trader the numbers they need; equity curve is a later addition.
func (h *Handlers) AnalyticsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()

	q := r.URL.Query()
	// Bridge legacy ?account_id=N → ?filter=acc:N for older bookmarks.
	if legacy := q.Get("account_id"); legacy != "" {
		newQ := q
		newQ.Add("filter", "acc:"+legacy)
		newR := *r
		newR.URL = cloneURL(r.URL)
		newR.URL.RawQuery = newQ.Encode()
		r = &newR
		q = r.URL.Query()
	}
	accountFilt, err := h.resolveAccountFilter(r)
	if err != nil {
		h.serverError(w, fmt.Errorf("resolve filter: %w", err))
		return
	}

	var from, to *time.Time
	if v := q.Get("from"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, tz); err == nil {
			from = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, tz); err == nil {
			t = t.AddDate(0, 0, 1)
			to = &t
		}
	}

	trades, err := h.fetchAnalyticsTrades(ctx, accountFilt, from, to)
	if err != nil {
		h.serverError(w, fmt.Errorf("fetch trades: %w", err))
		return
	}

	headline := computeHeadline(trades)
	bySymbol := bucketBy(trades, func(t analyticsTrade) string { return t.SymbolRoot })
	byDirection := bucketBy(trades, func(t analyticsTrade) string { return t.Direction })
	byDOW := bucketBy(trades, func(t analyticsTrade) string {
		return weekdayLabel(t.EntryTime.In(tz).Weekday())
	})
	byHour := bucketBy(trades, func(t analyticsTrade) string {
		return fmt.Sprintf("%02d:00", t.EntryTime.In(tz).Hour())
	})

	// Daily series — prefer the daily_stats rollup so the local-tz bucketing
	// matches the calendar view. Falls back to per-trade derivation if no
	// stats rows fell in the range (e.g., before the position builder has
	// run for fresh imports).
	dailyRollup := h.dailyRollupForFilter(ctx, accountFilt, from, to, tz)
	dash := computeDashboard(trades, byDOW, dailyRollup, tz)

	// Chart data is shipped to the page as inline JSON so uPlot can read it
	// without a second round-trip. {xs, ys, cum} arrays parallel each other.
	chartDates := make([]int64, len(dash.DailySeries))
	chartNets := make([]float64, len(dash.DailySeries))
	chartCum := make([]float64, len(dash.DailySeries))
	for i, p := range dash.DailySeries {
		chartDates[i] = p.Date.Unix()
		chartNets[i] = p.NetPnl
		chartCum[i] = p.Cumulative
	}
	chartJSON, _ := json.Marshal(map[string]any{
		"dates": chartDates,
		"nets":  chartNets,
		"cum":   chartCum,
	})

	accounts, err := h.Queries.ListAccounts(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}

	formFrom, formTo := "", ""
	if from != nil {
		formFrom = from.Format("2006-01-02")
	}
	if to != nil {
		formTo = to.AddDate(0, 0, -1).Format("2006-01-02")
	}

	h.renderPage(w, "analytics.html", map[string]any{
		"NavActive":        "analytics",
		"BackHref":         validBackHref(r),
		"Headline":         headline,
		"Dash":             dash,
		"ChartJSON":        template.JS(chartJSON),
		"BySymbol":         bySymbol,
		"ByDirection":      byDirection,
		"ByDOW":            byDOW,
		"ByHour":           byHour,
		"Accounts":         accounts,
		"FilterSelections": accountFilt.Selected,
		"FilterCount":      accountFilt.SelectedCount,
		"From":             formFrom,
		"To":               formTo,
		"TradeCount":       int64(len(trades)),
	})
}

// dailyRollupForFilter returns a daily series rolled up from daily_stats for
// the active account filter and date range. Empty slice (not nil) on miss so
// the caller can still build an empty chart.
func (h *Handlers) dailyRollupForFilter(ctx context.Context, af accountFilter, from, to *time.Time, tz *time.Location) []dailyPoint {
	// We need [from, to). When the user didn't set bounds, default to the
	// last 30 days ending today — keeps the chart bounded and matches the
	// "Last month" preset's spirit.
	end := time.Now().In(tz).AddDate(0, 0, 1) // tomorrow midnight, exclusive
	start := end.AddDate(0, 0, -30)
	if from != nil {
		start = *from
	}
	if to != nil {
		end = *to
	}
	rows, err := h.Queries.ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{
		StatDate:   start,
		StatDate_2: end,
	})
	if err != nil {
		return nil
	}
	allowed := map[int64]bool{}
	for _, id := range af.AccountIDs {
		allowed[id] = true
	}
	type key struct{ y, m, d int }
	bucket := map[key]float64{}
	keys := []key{}
	for _, r := range rows {
		if af.Active && !allowed[r.AccountID] {
			continue
		}
		ld := r.StatDate.In(tz)
		k := key{ld.Year(), int(ld.Month()), ld.Day()}
		if _, ok := bucket[k]; !ok {
			keys = append(keys, k)
		}
		bucket[k] += r.NetPnl
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].y != keys[j].y {
			return keys[i].y < keys[j].y
		}
		if keys[i].m != keys[j].m {
			return keys[i].m < keys[j].m
		}
		return keys[i].d < keys[j].d
	})
	out := make([]dailyPoint, 0, len(keys))
	var cum float64
	for _, k := range keys {
		net := bucket[k]
		cum += net
		out = append(out, dailyPoint{
			Date:       time.Date(k.y, time.Month(k.m), k.d, 0, 0, 0, 0, tz),
			NetPnl:     net,
			Cumulative: cum,
		})
	}
	return out
}

// fetchAnalyticsTrades pulls the slice with optional account / date filters
// applied at the SQL level (cheaper than filtering 500+ trades in Go).
func (h *Handlers) fetchAnalyticsTrades(ctx context.Context, af accountFilter, from, to *time.Time) ([]analyticsTrade, error) {
	clauses := []string{"status='closed'", "exit_time IS NOT NULL"}
	var args []any
	if af.Active {
		if len(af.AccountIDs) == 0 {
			clauses = append(clauses, "1 = 0")
		} else {
			placeholders := strings.Repeat("?,", len(af.AccountIDs))
			placeholders = placeholders[:len(placeholders)-1]
			clauses = append(clauses, "account_id IN ("+placeholders+")")
			for _, id := range af.AccountIDs {
				args = append(args, id)
			}
		}
	}
	if from != nil {
		clauses = append(clauses, "exit_time >= ?")
		args = append(args, *from)
	}
	if to != nil {
		clauses = append(clauses, "exit_time < ?")
		args = append(args, *to)
	}
	where := ""
	for i, c := range clauses {
		if i == 0 {
			where = c
		} else {
			where += " AND " + c
		}
	}
	query := `SELECT t.id, t.account_id, t.symbol, t.symbol_root, t.direction, t.qty,
	                 t.entry_price, t.exit_price,
	                 t.gross_pnl, t.net_pnl, t.commission,
	                 t.entry_time, t.exit_time,
	                 a.name
	          FROM trades t
	          JOIN accounts a ON a.id = t.account_id
	          WHERE ` + where + `
	          ORDER BY t.exit_time`
	rows, err := h.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []analyticsTrade
	for rows.Next() {
		var t analyticsTrade
		var exit sql.NullTime // exit_time IS NOT NULL filter above, but be defensive
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Symbol, &t.SymbolRoot, &t.Direction, &t.Qty,
			&t.EntryPrice, &t.ExitPrice,
			&t.GrossPnl, &t.NetPnl, &t.Commission,
			&t.EntryTime, &exit,
			&t.AccountName); err != nil {
			return nil, err
		}
		if exit.Valid {
			t.ExitTime = exit.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// headlineMetrics is the numbers shown in big stat cards at the top.
type headlineMetrics struct {
	Trades             int64
	Wins               int64
	Losses             int64
	WinRate            float64
	NetPnl             float64
	GrossPnl           float64
	Commission         float64
	AvgWin             float64
	AvgLoss            float64 // negative number
	LargestWin         float64
	LargestLoss        float64 // negative number
	LargestWinTradeID  int64   // 0 if none
	LargestLossTradeID int64   // 0 if none
	ProfitFactor       float64
	Expectancy         float64
}

func computeHeadline(trades []analyticsTrade) headlineMetrics {
	var m headlineMetrics
	if len(trades) == 0 {
		return m
	}
	var sumWins, sumLosses float64 // sumLosses stored as positive magnitude
	first := true
	for _, t := range trades {
		m.Trades++
		if t.GrossPnl != nil {
			m.GrossPnl += *t.GrossPnl
		}
		if t.Commission != nil {
			m.Commission += *t.Commission
		}
		if t.NetPnl == nil {
			continue
		}
		m.NetPnl += *t.NetPnl
		switch {
		case *t.NetPnl > 0:
			m.Wins++
			sumWins += *t.NetPnl
			if m.LargestWinTradeID == 0 || *t.NetPnl > m.LargestWin {
				m.LargestWin = *t.NetPnl
				m.LargestWinTradeID = t.ID
			}
		case *t.NetPnl < 0:
			m.Losses++
			sumLosses += -*t.NetPnl
			if m.LargestLossTradeID == 0 || *t.NetPnl < m.LargestLoss {
				m.LargestLoss = *t.NetPnl
				m.LargestLossTradeID = t.ID
			}
		}
		first = false
	}
	_ = first // retained for future shape-of-edge-case checks
	decided := m.Wins + m.Losses
	if decided > 0 {
		m.WinRate = float64(m.Wins) / float64(decided)
	}
	if m.Wins > 0 {
		m.AvgWin = sumWins / float64(m.Wins)
	}
	if m.Losses > 0 {
		m.AvgLoss = -sumLosses / float64(m.Losses) // negative
	}
	if sumLosses > 0 {
		m.ProfitFactor = sumWins / sumLosses
	} else if sumWins > 0 {
		m.ProfitFactor = math.Inf(1)
	}
	if decided > 0 {
		lossRate := float64(m.Losses) / float64(decided)
		m.Expectancy = (m.AvgWin * m.WinRate) + (m.AvgLoss * lossRate)
	}
	return m
}

// bucketBy groups trades by a string key and returns buckets sorted by net
// P&L descending (most-profitable group first).
func bucketBy(trades []analyticsTrade, key func(analyticsTrade) string) []analyticsBucket {
	by := map[string]*analyticsBucket{}
	for _, t := range trades {
		k := key(t)
		b, ok := by[k]
		if !ok {
			b = &analyticsBucket{Key: k}
			by[k] = b
		}
		b.Trades++
		if t.GrossPnl != nil {
			b.Gross += *t.GrossPnl
		}
		if t.NetPnl == nil {
			continue
		}
		b.Net += *t.NetPnl
		switch {
		case *t.NetPnl > 0:
			b.Wins++
		case *t.NetPnl < 0:
			b.Losses++
		}
		if !b.HasBW {
			b.Best = *t.NetPnl
			b.Worst = *t.NetPnl
			b.HasBW = true
		} else {
			if *t.NetPnl > b.Best {
				b.Best = *t.NetPnl
			}
			if *t.NetPnl < b.Worst {
				b.Worst = *t.NetPnl
			}
		}
	}
	out := make([]analyticsBucket, 0, len(by))
	for _, b := range by {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Net > out[j].Net })
	return out
}

// weekdayLabel returns "Mon", "Tue", … so DOW buckets sort and display nicely.
func weekdayLabel(d time.Weekday) string {
	return d.String()[:3]
}

// dailyPoint is one date on the equity curve / daily bar chart.
type dailyPoint struct {
	Date       time.Time
	NetPnl     float64
	Cumulative float64
}

// durationBucket counts trades whose duration falls into [LoSec, HiSec).
// HiSec == -1 means "+∞" (the open-ended top bucket).
type durationBucket struct {
	Label   string
	LoSec   int
	HiSec   int
	Trades  int64
	Wins    int64
	Losses  int64
	WinRate float64
}

// dowStat is the day-of-week summary shown in the "Most Active" / "Most
// Profitable" / "Least Profitable" tiles.
type dowStat struct {
	DOW           string
	ActiveDates   int64
	TotalTrades   int64
	AvgPerDay     float64
	Net           float64
}

// tradeBlurb is the small payload rendered in the Best / Worst trade tiles.
type tradeBlurb struct {
	ID          int64
	Direction   string
	Qty         float64
	Symbol      string
	EntryPrice  float64
	ExitPrice   float64
	ExitTime    time.Time
	AccountName string
	NetPnl      float64
}

// dashboardMetrics carries everything the TopstepX-style dashboard needs that
// isn't already in headlineMetrics. Held in its own struct so the handler can
// pass it as one template variable.
type dashboardMetrics struct {
	DayWinPct          float64       // % of active days with net > 0
	BestDayPctOfProfit float64       // max(positive_day_net) / sum(positive_day_nets)
	TotalLots          float64       // sum(trade.qty)
	AvgTradeDuration   time.Duration
	AvgWinDuration     time.Duration
	AvgLossDuration    time.Duration
	LongCount          int64
	ShortCount         int64
	LongPct            float64
	MostActive         *dowStat
	MostProfitable     *dowStat
	LeastProfitable    *dowStat
	DailySeries        []dailyPoint
	DurationBuckets    []durationBucket
	Best               *tradeBlurb
	Worst              *tradeBlurb
}

// computeDashboard computes everything in dashboardMetrics from the filtered
// trades + an optional pre-aggregated daily series rolled up from daily_stats.
// dailyRollup may be nil if the caller doesn't have the filtered daily_stats
// — the function then falls back to deriving daily totals from `trades`
// (which is slightly less faithful to the trading-tz rollup but works).
func computeDashboard(trades []analyticsTrade, byDOW []analyticsBucket, dailyRollup []dailyPoint, tz *time.Location) dashboardMetrics {
	var m dashboardMetrics
	if len(trades) == 0 {
		return m
	}

	// Lots + direction split.
	for _, t := range trades {
		m.TotalLots += t.Qty
		if t.Direction == "long" {
			m.LongCount++
		} else {
			m.ShortCount++
		}
	}
	if m.LongCount+m.ShortCount > 0 {
		m.LongPct = float64(m.LongCount) / float64(m.LongCount+m.ShortCount) * 100
	}

	// Durations.
	var totalDur, winDur, lossDur time.Duration
	var totalN, winN, lossN int64
	for _, t := range trades {
		if t.ExitTime.IsZero() {
			continue
		}
		d := t.ExitTime.Sub(t.EntryTime)
		if d < 0 {
			continue
		}
		totalDur += d
		totalN++
		if t.NetPnl == nil {
			continue
		}
		if *t.NetPnl > 0 {
			winDur += d
			winN++
		} else if *t.NetPnl < 0 {
			lossDur += d
			lossN++
		}
	}
	if totalN > 0 {
		m.AvgTradeDuration = totalDur / time.Duration(totalN)
	}
	if winN > 0 {
		m.AvgWinDuration = winDur / time.Duration(winN)
	}
	if lossN > 0 {
		m.AvgLossDuration = lossDur / time.Duration(lossN)
	}

	// Daily series — prefer rollup if provided, else derive from trades.
	if dailyRollup != nil {
		m.DailySeries = dailyRollup
	} else {
		m.DailySeries = dailySeriesFromTrades(trades, tz)
	}

	// Day Win % + Best Day % of Total Profit.
	var positiveDays []float64
	var activeDays, winDays int64
	for _, p := range m.DailySeries {
		if p.NetPnl == 0 {
			continue
		}
		activeDays++
		if p.NetPnl > 0 {
			winDays++
			positiveDays = append(positiveDays, p.NetPnl)
		}
	}
	if activeDays > 0 {
		m.DayWinPct = float64(winDays) / float64(activeDays) * 100
	}
	if len(positiveDays) > 0 {
		var sum, best float64
		for _, v := range positiveDays {
			sum += v
			if v > best {
				best = v
			}
		}
		if sum > 0 {
			m.BestDayPctOfProfit = best / sum * 100
		}
	}

	// DOW stats — enrich existing byDOW buckets with distinct active dates.
	activeDatesByDOW := map[string]map[string]struct{}{}
	for _, t := range trades {
		dow := weekdayLabel(t.EntryTime.In(tz).Weekday())
		key := t.EntryTime.In(tz).Format("2006-01-02")
		set, ok := activeDatesByDOW[dow]
		if !ok {
			set = map[string]struct{}{}
			activeDatesByDOW[dow] = set
		}
		set[key] = struct{}{}
	}
	for _, b := range byDOW {
		s := &dowStat{
			DOW:         b.Key,
			TotalTrades: b.Trades,
			Net:         b.Net,
			ActiveDates: int64(len(activeDatesByDOW[b.Key])),
		}
		if s.ActiveDates > 0 {
			s.AvgPerDay = float64(s.TotalTrades) / float64(s.ActiveDates)
		}
		if m.MostActive == nil || s.TotalTrades > m.MostActive.TotalTrades {
			m.MostActive = s
		}
		if m.MostProfitable == nil || s.Net > m.MostProfitable.Net {
			m.MostProfitable = s
		}
		if m.LeastProfitable == nil || s.Net < m.LeastProfitable.Net {
			m.LeastProfitable = s
		}
	}

	// Duration buckets — fixed bins matching the reference dashboard.
	m.DurationBuckets = durationBuckets(trades)

	// Best / worst trade blurbs.
	var best, worst *analyticsTrade
	for i := range trades {
		t := &trades[i]
		if t.NetPnl == nil {
			continue
		}
		if best == nil || *t.NetPnl > derefF(best.NetPnl) {
			best = t
		}
		if worst == nil || *t.NetPnl < derefF(worst.NetPnl) {
			worst = t
		}
	}
	m.Best = blurbFor(best)
	m.Worst = blurbFor(worst)

	return m
}

func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func blurbFor(t *analyticsTrade) *tradeBlurb {
	if t == nil {
		return nil
	}
	tb := &tradeBlurb{
		ID:          t.ID,
		Direction:   t.Direction,
		Qty:         t.Qty,
		Symbol:      t.Symbol,
		EntryPrice:  t.EntryPrice,
		ExitTime:    t.ExitTime,
		AccountName: t.AccountName,
	}
	if t.ExitPrice != nil {
		tb.ExitPrice = *t.ExitPrice
	}
	if t.NetPnl != nil {
		tb.NetPnl = *t.NetPnl
	}
	return tb
}

// dailySeriesFromTrades rolls trades up to per-date net P&L (local tz) and
// computes a running cumulative. Used when daily_stats isn't available
// pre-aggregated for the filter.
func dailySeriesFromTrades(trades []analyticsTrade, tz *time.Location) []dailyPoint {
	type key struct{ y, m, d int }
	bucket := map[key]float64{}
	keys := []key{}
	for _, t := range trades {
		if t.ExitTime.IsZero() || t.NetPnl == nil {
			continue
		}
		ld := t.ExitTime.In(tz)
		k := key{ld.Year(), int(ld.Month()), ld.Day()}
		if _, ok := bucket[k]; !ok {
			keys = append(keys, k)
		}
		bucket[k] += *t.NetPnl
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].y != keys[j].y {
			return keys[i].y < keys[j].y
		}
		if keys[i].m != keys[j].m {
			return keys[i].m < keys[j].m
		}
		return keys[i].d < keys[j].d
	})
	out := make([]dailyPoint, 0, len(keys))
	var cum float64
	for _, k := range keys {
		net := bucket[k]
		cum += net
		out = append(out, dailyPoint{
			Date:       time.Date(k.y, time.Month(k.m), k.d, 0, 0, 0, 0, tz),
			NetPnl:     net,
			Cumulative: cum,
		})
	}
	return out
}

// durationBuckets assigns each closed trade to its [lo, hi) bucket.
func durationBuckets(trades []analyticsTrade) []durationBucket {
	defs := []durationBucket{
		{Label: "Under 15 sec", LoSec: 0, HiSec: 15},
		{Label: "15-45 sec", LoSec: 15, HiSec: 45},
		{Label: "45 sec - 1 min", LoSec: 45, HiSec: 60},
		{Label: "1 min - 2 min", LoSec: 60, HiSec: 120},
		{Label: "2 min - 5 min", LoSec: 120, HiSec: 300},
		{Label: "5 min - 10 min", LoSec: 300, HiSec: 600},
		{Label: "10 min - 30 min", LoSec: 600, HiSec: 1800},
		{Label: "30 min - 1 hour", LoSec: 1800, HiSec: 3600},
		{Label: "1 hour - 2 hours", LoSec: 3600, HiSec: 7200},
		{Label: "2 hours - 4 hours", LoSec: 7200, HiSec: 14400},
		{Label: "4 hours and up", LoSec: 14400, HiSec: -1},
	}
	for _, t := range trades {
		if t.ExitTime.IsZero() {
			continue
		}
		sec := int(t.ExitTime.Sub(t.EntryTime).Seconds())
		if sec < 0 {
			continue
		}
		for i := range defs {
			b := &defs[i]
			if (sec >= b.LoSec) && (b.HiSec == -1 || sec < b.HiSec) {
				b.Trades++
				if t.NetPnl != nil {
					if *t.NetPnl > 0 {
						b.Wins++
					} else if *t.NetPnl < 0 {
						b.Losses++
					}
				}
				break
			}
		}
	}
	for i := range defs {
		b := &defs[i]
		decided := b.Wins + b.Losses
		if decided > 0 {
			b.WinRate = float64(b.Wins) / float64(decided) * 100
		}
	}
	return defs
}
