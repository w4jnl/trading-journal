package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
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
	PlannedRisk *float64 // money at risk per the trade's plan; nil without one
	EntryTime   time.Time
	ExitTime    time.Time
	AccountName string
	Strategy    string // from the trade's own entry, else a group entry it is in; "" = none
	Tag         string // good | neutral | bad from the same entries; "" = untagged
}

// analyticsBucket aggregates trades by some grouping key (symbol, direction,
// day-of-week, etc).
type analyticsBucket struct {
	Key    string
	Trades int64
	Wins   int64
	Losses int64
	Net    float64
	Gross  float64
	Best   float64
	Worst  float64
	HasBW  bool // whether Best/Worst have valid values
}

// AnalyticsPage renders /analytics — aggregate metrics plus per-symbol /
// per-direction / per-day-of-week / per-hour breakdowns, filterable by
// account and date range. No chart yet — the headline metrics give the
// trader the numbers they need; equity curve is a later addition.
func (h *Handlers) AnalyticsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isFragment := liveFragment(r)
	h.TopstepxSync.MaybeEnqueueUser(userID(ctx)) // throttled background API sync; nil-safe
	h.IbkrSync.MaybeEnqueueUser(userID(ctx))     // throttled IBKR gateway poll; nil-safe
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

	// Trading-tz days, sent to SQL as UTC instants (see daybounds.go).
	var from, to *time.Time
	if v := q.Get("from"); v != "" {
		if t, ok := parseLocalDay(v, tz); ok {
			u := t.UTC()
			from = &u
		}
	}
	if v := q.Get("to"); v != "" {
		if t, ok := parseLocalDay(v, tz); ok {
			u := t.AddDate(0, 0, 1).UTC()
			to = &u
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
	byStrategy := bucketBy(trades, func(t analyticsTrade) string { return orLabel(t.Strategy, "(no strategy)") })
	byTag := bucketBy(trades, func(t analyticsTrade) string { return orLabel(t.Tag, "(untagged)") })
	insights := computeInsights(headline, bySymbol, byDOW, byHour)

	// Daily series — prefer the daily_stats rollup so the local-tz bucketing
	// matches the calendar view. Falls back to per-trade derivation if no
	// stats rows fell in the range (e.g., before the position builder has
	// run for fresh imports).
	dailyRollup := h.dailyRollupForFilter(ctx, accountFilt, from, to, tz)
	dash := computeDashboard(trades, byDOW, dailyRollup, tz)
	drawdown := computeDrawdown(trades)
	streaks := computeStreaks(trades, dash.DailySeries)

	// Balance series — running equity over the same date range, walked
	// forward from each included account's balance anchor.
	balXs, balYs, equityNow, equityByType, balIncluded, balExcluded, err := h.balanceSeriesForFilter(ctx, accountFilt, from, to, tz)
	if err != nil {
		h.serverError(w, fmt.Errorf("balance series: %w", err))
		return
	}

	// Chart data is shipped to the page as inline JSON so uPlot can read it
	// without a second round-trip. {dates, nets, cum} parallel each other for
	// the cumulative chart; {balDates, balance} parallel each other for the
	// equity chart (its own axis because excluded accounts narrow the date set).
	chartDates := make([]int64, len(dash.DailySeries))
	chartNets := make([]float64, len(dash.DailySeries))
	chartCum := make([]float64, len(dash.DailySeries))
	for i, p := range dash.DailySeries {
		chartDates[i] = p.Date.Unix()
		chartNets[i] = p.NetPnl
		chartCum[i] = p.Cumulative
	}
	chartJSON, _ := json.Marshal(map[string]any{
		"dates":    chartDates,
		"nets":     chartNets,
		"cum":      chartCum,
		"balDates": balXs,
		"balance":  balYs,
	})

	accounts, err := h.rq().ListAccounts(ctx, auth.MustUser(ctx).ID)
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

	data := map[string]any{
		"NavActive":        "analytics",
		"BackHref":         validBackHref(r),
		"Headline":         headline,
		"Insights":         insights,
		"Dash":             dash,
		"ChartJSON":        template.JS(chartJSON),
		"BySymbol":         bySymbol,
		"ByDirection":      byDirection,
		"ByStrategy":       byStrategy,
		"ByTag":            byTag,
		"Drawdown":         drawdown,
		"Streaks":          streaks,
		"ByDOW":            byDOW,
		"ByHour":           byHour,
		"Accounts":         accountsForFilter(accounts, false),
		"FilterSelections": accountFilt.Selected,
		"FilterCount":      accountFilt.SelectedCount,
		"From":             formFrom,
		"To":               formTo,
		"TradeCount":       int64(len(trades)),
		"EquityNow":        equityNow,
		"EquityByType":     equityByType,
		"EquityIncluded":   balIncluded,
		"EquityExcluded":   balExcluded,
	}
	if isFragment {
		h.renderPageFragment(w, r, "analytics.html", "analytics_fragment", data)
		return
	}
	h.renderPage(w, r, "analytics.html", data)
}

// balanceSeriesForFilter builds a per-day running-equity series across the
// accounts in the filter that have at least one balance anchor. Returns:
//   - xs/ys: parallel Unix-seconds and balance arrays for the chart.
//   - equityNow: the total balance at the end of the range (last ys value, or
//     the starting total if the range had no daily_stats activity).
//   - byType: per-account-type breakdown of the end-of-range equity, in
//     equityTypeOrder so real capital shows above eval / paper.
//   - included/excluded: account counts for the "N excluded — no anchor" notice.
//
// Equity at the start of the range is sourced from loadBalances called for
// (start − 1 day) — that helper already handles the "no anchor in the past,
// pick the earliest future anchor and back-fill" edge case. From there each
// day's daily_stats row applies a per-account delta to the running balance,
// summed across included accounts.
func (h *Handlers) balanceSeriesForFilter(ctx context.Context, af accountFilter, from, to *time.Time, tz *time.Location) (xs []int64, ys []float64, equityNow float64, byType []equityByType, included int64, excluded int64, err error) {
	// Resolve range. Upper bound: explicit `to`, else tomorrow midnight.
	end := time.Now().In(tz).AddDate(0, 0, 1)
	if to != nil {
		end = *to
	}
	// Lower bound: explicit `from` wins. When omitted, walk back to the
	// earliest closed trade in the filter so an anchored account immediately
	// shows its full equity history — the back-fill in loadBalances does the
	// rest. Falls back to 30 days if we can't determine an earliest exit.
	var start time.Time
	if from != nil {
		start = *from
	} else {
		earliest, derr := h.earliestExitForFilter(ctx, af)
		switch {
		case derr != nil:
			err = derr
			return
		case !earliest.IsZero():
			start = earliest
		default:
			start = end.AddDate(0, 0, -30)
		}
	}
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, tz)
	priorDay := startDay.AddDate(0, 0, -1)

	// Resolve account set in the filter and capture each account's type slug
	// so we can break the end-of-range equity down per type at the end.
	accounts, listErr := h.rq().ListAccounts(ctx, auth.MustUser(ctx).ID)
	if listErr != nil {
		err = listErr
		return
	}
	typeByID := map[int64]string{}
	for _, a := range accounts {
		t := ""
		if a.Type != nil {
			t = *a.Type
		}
		typeByID[a.ID] = t
	}
	afSet := map[int64]bool{}
	if af.Active {
		for _, id := range af.AccountIDs {
			afSet[id] = true
		}
	} else {
		// Implicit "all accounts" means all non-retired, non-stats-excluded
		// accounts, so the equity curve reflects only capital the user wants
		// counted (Eval accounts flagged out don't distort it).
		for _, a := range accounts {
			if a.Active != 1 || a.ExcludedFromStats != 0 {
				continue
			}
			afSet[a.ID] = true
		}
	}

	// Starting balance per account = balance at end of (start - 1 day).
	initBalances, err := h.loadBalances(ctx, priorDay)
	if err != nil {
		return
	}
	state := map[int64]float64{}
	var startingTotal float64
	for aid := range afSet {
		b, ok := initBalances[aid]
		if !ok || !b.Known {
			excluded++
			continue
		}
		state[aid] = b.Balance
		startingTotal += b.Balance
		included++
	}
	if included == 0 {
		return nil, nil, 0, nil, 0, excluded, nil
	}

	// Walk daily_stats inside [start, end). Group rows by date in trading tz;
	// apply each day's deltas to the running balances, then snapshot the total.
	rows, err := h.rq().ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{
		UserID:     userID(ctx),
		StatDate:   startDay,
		StatDate_2: end,
	})
	if err != nil {
		return
	}
	byDate := map[time.Time]map[int64]float64{}
	for _, r := range rows {
		if _, ok := state[r.AccountID]; !ok {
			continue // excluded or out of filter
		}
		y, m, d := r.StatDate.In(tz).Date()
		key := time.Date(y, m, d, 0, 0, 0, 0, tz)
		m2, exists := byDate[key]
		if !exists {
			m2 = map[int64]float64{}
			byDate[key] = m2
		}
		m2[r.AccountID] += r.NetPnl
	}
	dates := make([]time.Time, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })

	xs = make([]int64, 0, len(dates))
	ys = make([]float64, 0, len(dates))
	for _, d := range dates {
		for aid, delta := range byDate[d] {
			state[aid] += delta
		}
		var sum float64
		for _, b := range state {
			sum += b
		}
		xs = append(xs, d.Unix())
		ys = append(ys, sum)
	}

	if len(ys) > 0 {
		equityNow = ys[len(ys)-1]
	} else {
		equityNow = startingTotal
	}

	// Per-type breakdown of the end-of-range equity. state already holds each
	// included account's final balance, so we just group by its type slug.
	byTypeMap := map[string]*equityByType{}
	for aid, bal := range state {
		slug := typeByID[aid]
		row, exists := byTypeMap[slug]
		if !exists {
			row = &equityByType{Type: slug, Label: equityTypeLabel(slug)}
			byTypeMap[slug] = row
		}
		row.Total += bal
		row.Accounts++
	}
	byType = orderedEquityBreakdown(byTypeMap)

	return xs, ys, equityNow, byType, included, excluded, nil
}

// earliestExitForFilter returns the earliest closed trade's exit_time scoped
// to the active account filter. Used by balanceSeriesForFilter to extend the
// equity curve back to the start of trading when no explicit `from` was
// supplied — so adding the first anchor on /accounts immediately reveals the
// full history.
//
// Returns a zero Time when the filter resolves to "no accounts" (e.g., the
// filter is active but its ID list is empty) or when no closed trades exist.
//
// Note the ORDER BY ... LIMIT 1 dance: aggregates like MIN(exit_time) lose
// the column type in modernc.org/sqlite and can't be scanned into time.Time.
// Same workaround as pickDashboardDate.
func (h *Handlers) earliestExitForFilter(ctx context.Context, af accountFilter) (time.Time, error) {
	if af.Active && len(af.AccountIDs) == 0 {
		return time.Time{}, nil
	}
	w := whereUser(ownsTradesSQL, userID(ctx)).and("t.status = 'closed'").and("t.exit_time IS NOT NULL").and("t." + retiredAccountFilterSQL)
	if af.Active {
		w.andIn("t.account_id", af.AccountIDs)
	} else {
		w.and("t.account_id IN (SELECT id FROM accounts WHERE excluded_from_stats = 0)")
	}
	var earliest time.Time
	row := h.rdb().QueryRowContext(ctx, "SELECT t.exit_time FROM trades t WHERE "+w.sql()+" ORDER BY t.exit_time ASC LIMIT 1", w.args...)
	if err := row.Scan(&earliest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return earliest, nil
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
	rows, err := h.rq().ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{
		UserID:     userID(ctx),
		StatDate:   start,
		StatDate_2: end,
	})
	if err != nil {
		return nil
	}
	retired, err := h.retiredAccountSet(ctx)
	if err != nil {
		return nil
	}
	excluded, err := h.statsExcludedAccountSet(ctx)
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
		if retired[r.AccountID] {
			continue
		}
		if af.Active {
			if !allowed[r.AccountID] {
				continue
			}
		} else if excluded[r.AccountID] {
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
// Trades from retired accounts (accounts.active = 0) are always excluded —
// analytics is one of the surfaces the user asked us to keep clean.
func (h *Handlers) fetchAnalyticsTrades(ctx context.Context, af accountFilter, from, to *time.Time) ([]analyticsTrade, error) {
	w := whereUser(ownsAccountsSQL, userID(ctx)).and("t.status = 'closed'").and("t.exit_time IS NOT NULL").and("a.active = 1")
	if af.Active {
		w.andIn("t.account_id", af.AccountIDs)
	} else {
		// No explicit selection: drop stats-excluded accounts from the default.
		w.and("a.excluded_from_stats = 0")
	}
	if from != nil {
		w.and("t.exit_time >= ?", *from)
	}
	if to != nil {
		w.and("t.exit_time < ?", *to)
	}
	where, args := w.sql(), w.args
	query := `SELECT t.id, t.account_id, t.symbol, t.symbol_root, t.direction, t.qty,
	                 t.entry_price, t.exit_price,
	                 t.gross_pnl, t.net_pnl, t.commission, t.planned_risk,
	                 t.entry_time, t.exit_time,
	                 a.name,
	                 COALESCE((SELECT s.name FROM journal_entries je JOIN strategies s ON s.id = je.strategy_id WHERE je.trade_id = t.id LIMIT 1),
	                          (SELECT s.name FROM journal_entry_trades jet JOIN journal_entries je ON je.id = jet.journal_entry_id
	                           JOIN strategies s ON s.id = je.strategy_id WHERE jet.trade_id = t.id LIMIT 1), '') AS strategy,
	                 COALESCE((SELECT je.tag FROM journal_entries je WHERE je.trade_id = t.id AND je.tag IS NOT NULL LIMIT 1),
	                          (SELECT je.tag FROM journal_entry_trades jet JOIN journal_entries je ON je.id = jet.journal_entry_id
	                           WHERE jet.trade_id = t.id AND je.tag IS NOT NULL LIMIT 1), '') AS tag
	          FROM trades t
	          JOIN accounts a ON a.id = t.account_id
	          WHERE ` + where + `
	          ORDER BY t.exit_time`
	rows, err := h.rdb().QueryContext(ctx, query, args...)
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
			&t.GrossPnl, &t.NetPnl, &t.Commission, &t.PlannedRisk,
			&t.EntryTime, &exit,
			&t.AccountName, &t.Strategy, &t.Tag); err != nil {
			return nil, err
		}
		if exit.Valid {
			t.ExitTime = exit.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// insight is one auto-surfaced finding rendered in the "Notable" panel at
// the top of /analytics. The intent is to draw the trader's eye to the most
// actionable signal in the current filter without forcing them to scan the
// KPI grid and four breakdown tables themselves.
//
// Tone drives the card's left-border color: "pos" / "neg" / "warn" / "info".
type insight struct {
	Tone   string // "pos" | "neg" | "warn" | "info"
	Title  string // short headline
	Detail string // one supporting line
	Href   string // optional link (target page already filtered)
}

// computeInsights derives 4–6 insights from the filtered headline + buckets.
// Each branch contributes at most one insight and is gated so that weak
// signals (one trade per bucket, ties, etc.) don't dominate the panel.
//
// Order is loose hierarchy: profit factor / R-multiple anomalies first
// because they apply across the whole filter, then symbol focus, then
// time-of-day, then day-of-week.
func computeInsights(h headlineMetrics, bySymbol, byDOW, byHour []analyticsBucket) []insight {
	var out []insight
	if h.Trades == 0 {
		return out
	}

	// Profit factor — flag the two ends.
	switch {
	case math.IsInf(h.ProfitFactor, 0) && h.Wins > 0:
		out = append(out, insight{
			Tone:   "pos",
			Title:  "No losses in this range",
			Detail: fmt.Sprintf("%d wins, 0 losses — sample is small; widen the range to stress-test.", h.Wins),
		})
	case h.ProfitFactor > 0 && h.ProfitFactor < 1 && h.Losses >= 5:
		out = append(out, insight{
			Tone:   "neg",
			Title:  "Profit factor below 1",
			Detail: fmt.Sprintf("Wins cover only %.2fx losses. Total losses exceed total wins in this filter.", h.ProfitFactor),
		})
	case h.ProfitFactor >= 2 && h.Wins+h.Losses >= 10:
		out = append(out, insight{
			Tone:   "pos",
			Title:  "Strong profit factor",
			Detail: fmt.Sprintf("Wins are %.2fx losses across %d decided trades.", h.ProfitFactor, h.Wins+h.Losses),
		})
	}

	// Win/loss R-multiple — flag asymmetry that would erode an OK win rate.
	if h.AvgLoss < 0 && h.AvgWin > 0 {
		r := h.AvgWin / -h.AvgLoss
		switch {
		case r < 0.75 && h.WinRate < 0.6:
			out = append(out, insight{
				Tone:   "warn",
				Title:  "Avg loss exceeds avg win",
				Detail: fmt.Sprintf("Avg win %.2f vs avg loss %.2f (R %.2f). At %.0f%% win rate the math is tight.", h.AvgWin, h.AvgLoss, r, h.WinRate*100),
			})
		case r >= 2.0:
			out = append(out, insight{
				Tone:   "pos",
				Title:  "Wins are bigger than losses",
				Detail: fmt.Sprintf("Avg win %.2f vs avg loss %.2f (R %.2f).", h.AvgWin, h.AvgLoss, r),
			})
		}
	}

	// Symbol focus — when the most-traded root isn't the most-profitable.
	if len(bySymbol) >= 2 {
		mostTraded := pickMaxBucket(bySymbol, func(b analyticsBucket) float64 { return float64(b.Trades) })
		mostProfit := pickMaxBucket(bySymbol, func(b analyticsBucket) float64 { return b.Net })
		if mostTraded.Key != "" && mostProfit.Key != "" && mostTraded.Key != mostProfit.Key && mostProfit.Net > 0 {
			out = append(out, insight{
				Tone:   "info",
				Title:  "Most-traded ≠ most-profitable",
				Detail: fmt.Sprintf("%s has the most trades (%d) but %s makes the most money (%+.2f).", mostTraded.Key, mostTraded.Trades, mostProfit.Key, mostProfit.Net),
				Href:   "/trades?symbol_root=" + mostProfit.Key,
			})
		}
	}

	// Best / worst hour — when the spread is wide enough to be actionable
	// (best ≥ +X and worst ≤ -X, where X scales with trade count).
	if len(byHour) >= 2 {
		bestH := pickMaxBucket(byHour, func(b analyticsBucket) float64 { return b.Net })
		worstH := pickMinBucket(byHour, func(b analyticsBucket) float64 { return b.Net })
		if bestH.Key != "" && bestH.Net > 0 && bestH.Trades >= 3 {
			out = append(out, insight{
				Tone:   "pos",
				Title:  "Best hour: " + bestH.Key,
				Detail: fmt.Sprintf("%+.2f across %d trades · %s avg per trade", bestH.Net, bestH.Trades, money(bestH.Net/float64(bestH.Trades))),
			})
		}
		if worstH.Key != "" && worstH.Net < 0 && worstH.Trades >= 3 && worstH.Key != bestH.Key {
			out = append(out, insight{
				Tone:   "neg",
				Title:  "Worst hour: " + worstH.Key,
				Detail: fmt.Sprintf("%+.2f across %d trades · %s avg per trade", worstH.Net, worstH.Trades, money(worstH.Net/float64(worstH.Trades))),
			})
		}
	}

	// Best / worst day-of-week — same idea, but only when distinct.
	if len(byDOW) >= 2 {
		bestD := pickMaxBucket(byDOW, func(b analyticsBucket) float64 { return b.Net })
		worstD := pickMinBucket(byDOW, func(b analyticsBucket) float64 { return b.Net })
		if bestD.Key != "" && bestD.Net > 0 && bestD.Trades >= 3 {
			out = append(out, insight{
				Tone:   "pos",
				Title:  "Best day: " + bestD.Key,
				Detail: fmt.Sprintf("%+.2f across %d trades", bestD.Net, bestD.Trades),
			})
		}
		if worstD.Key != "" && worstD.Net < 0 && worstD.Trades >= 3 && worstD.Key != bestD.Key {
			out = append(out, insight{
				Tone:   "neg",
				Title:  "Worst day: " + worstD.Key,
				Detail: fmt.Sprintf("%+.2f across %d trades", worstD.Net, worstD.Trades),
			})
		}
	}

	// Cap the panel so it doesn't dominate the page. Six is the visual
	// budget — two rows of three cards on desktop, scrolls on mobile.
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// pickMaxBucket / pickMinBucket return the bucket with the largest / smallest
// score from `score`. Returns the zero-value analyticsBucket when input is
// empty so callers can check Key=="" for "no signal".
func pickMaxBucket(bs []analyticsBucket, score func(analyticsBucket) float64) analyticsBucket {
	var best analyticsBucket
	first := true
	for _, b := range bs {
		s := score(b)
		if first || s > score(best) {
			best = b
			first = false
		}
	}
	return best
}
func pickMinBucket(bs []analyticsBucket, score func(analyticsBucket) float64) analyticsBucket {
	var worst analyticsBucket
	first := true
	for _, b := range bs {
		s := score(b)
		if first || s < score(worst) {
			worst = b
			first = false
		}
	}
	return worst
}

// money is a tiny helper for embedding signed currency values in detail
// strings. Always 2dp, no symbol — the surrounding copy carries context.
func money(v float64) string {
	return fmt.Sprintf("%+.2f", v)
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
	// R metrics over the trades that carry a planned risk (trade_plan.go):
	// R = net / planned risk. RTrades is how many qualify; the rest is
	// meaningless (and rendered as a dash) when it is zero.
	RTrades     int64
	AvgR        float64
	AvgWinR     float64
	AvgLossR    float64 // negative
	ExpectancyR float64 // mean R, the per-trade edge in risk units
	WinRateR    float64 // win rate among the planned trades
}

func computeHeadline(trades []analyticsTrade) headlineMetrics {
	var m headlineMetrics
	if len(trades) == 0 {
		return m
	}
	var sumWins, sumLosses float64 // sumLosses stored as positive magnitude
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
	}
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
	var sumR, sumWinR, sumLossR float64
	var winsR, lossesR int64
	for _, t := range trades {
		if t.NetPnl == nil || t.PlannedRisk == nil || *t.PlannedRisk <= 0 {
			continue
		}
		r := *t.NetPnl / *t.PlannedRisk
		m.RTrades++
		sumR += r
		switch {
		case r > 0:
			winsR++
			sumWinR += r
		case r < 0:
			lossesR++
			sumLossR += r
		}
	}
	if m.RTrades > 0 {
		m.AvgR = sumR / float64(m.RTrades)
		m.ExpectancyR = m.AvgR
		if winsR+lossesR > 0 {
			m.WinRateR = float64(winsR) / float64(winsR+lossesR)
		}
	}
	if winsR > 0 {
		m.AvgWinR = sumWinR / float64(winsR)
	}
	if lossesR > 0 {
		m.AvgLossR = sumLossR / float64(lossesR)
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
	DOW         string
	ActiveDates int64
	TotalTrades int64
	AvgPerDay   float64
	Net         float64
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
	DayWinPct          float64 // % of active days with net > 0
	BestDayPctOfProfit float64 // max(positive_day_net) / sum(positive_day_nets)
	TotalLots          float64 // sum(trade.qty)
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

// orLabel substitutes a label for an empty bucket key.
func orLabel(v, empty string) string {
	if v == "" {
		return empty
	}
	return v
}
