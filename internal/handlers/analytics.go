package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// analyticsTrade is the minimal projection of a closed trade we need for
// computing the analytics page. Pulling from raw SQL keeps the handler in
// charge of the type so we can avoid a full models.Trade allocation per row.
type analyticsTrade struct {
	AccountID  int64
	Symbol     string
	SymbolRoot string
	Direction  string
	GrossPnl   *float64
	NetPnl     *float64
	Commission *float64
	EntryTime  time.Time
	ExitTime   time.Time
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
	var accountID *int64
	if v := q.Get("account_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			accountID = &id
		}
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

	trades, err := h.fetchAnalyticsTrades(ctx, accountID, from, to)
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

	accounts, err := h.Queries.ListAccounts(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}

	var accountIDForTmpl int64
	if accountID != nil {
		accountIDForTmpl = *accountID
	}
	formFrom, formTo := "", ""
	if from != nil {
		formFrom = from.Format("2006-01-02")
	}
	if to != nil {
		formTo = to.AddDate(0, 0, -1).Format("2006-01-02")
	}

	h.renderPage(w, "analytics.html", map[string]any{
		"Headline":    headline,
		"BySymbol":    bySymbol,
		"ByDirection": byDirection,
		"ByDOW":       byDOW,
		"ByHour":      byHour,
		"Accounts":    accounts,
		"AccountID":   accountIDForTmpl,
		"From":        formFrom,
		"To":          formTo,
		"TradeCount":  int64(len(trades)),
	})
}

// fetchAnalyticsTrades pulls the slice with optional account / date filters
// applied at the SQL level (cheaper than filtering 500+ trades in Go).
func (h *Handlers) fetchAnalyticsTrades(ctx context.Context, accountID *int64, from, to *time.Time) ([]analyticsTrade, error) {
	clauses := []string{"status='closed'", "exit_time IS NOT NULL"}
	var args []any
	if accountID != nil {
		clauses = append(clauses, "account_id = ?")
		args = append(args, *accountID)
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
	query := `SELECT account_id, symbol, symbol_root, direction, gross_pnl, net_pnl, commission, entry_time, exit_time
	          FROM trades WHERE ` + where + ` ORDER BY exit_time`
	rows, err := h.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []analyticsTrade
	for rows.Next() {
		var t analyticsTrade
		var exit sql.NullTime // exit_time IS NOT NULL filter above, but be defensive
		if err := rows.Scan(&t.AccountID, &t.Symbol, &t.SymbolRoot, &t.Direction,
			&t.GrossPnl, &t.NetPnl, &t.Commission, &t.EntryTime, &exit); err != nil {
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
	Trades       int64
	Wins         int64
	Losses       int64
	WinRate      float64
	NetPnl       float64
	GrossPnl     float64
	Commission   float64
	AvgWin       float64
	AvgLoss      float64 // negative number
	LargestWin   float64
	LargestLoss  float64 // negative number
	ProfitFactor float64
	Expectancy   float64
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
			if first || *t.NetPnl > m.LargestWin {
				m.LargestWin = *t.NetPnl
			}
		case *t.NetPnl < 0:
			m.Losses++
			sumLosses += -*t.NetPnl
			if first || *t.NetPnl < m.LargestLoss {
				m.LargestLoss = *t.NetPnl
			}
		}
		first = false
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
