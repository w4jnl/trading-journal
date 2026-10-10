package handlers

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// Admin "Market data" page: review chart-cache coverage and backfill gaps.
// Bars are keyed by Yahoo symbol and shared across all users, so this is a
// system-wide view (admin-only). The window matches the import-prefetch horizon
// - older days have no intraday data at Yahoo, so backfilling them is pointless.
const mdWindowDays = 45

// mdCoverageRow is one Yahoo-symbol row in the coverage table.
type mdCoverageRow struct {
	Symbol      string // yahoo symbol, e.g. ES=F
	Roots       string // instrument roots mapping to it, e.g. "ES, MES"
	Needed      int
	Cached      int
	Missing     int
	MissingDays []string // capped sample of the missing day keys
}

// AdminMarketDataPage renders the coverage review across all accounts' recent
// trades.
func (h *Handlers) AdminMarketDataPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.MarketData == nil {
		h.renderPage(w, r, "admin_market_data.html", mergeData(map[string]any{
			"NavActive": "admin_market_data",
			"BackHref":  validBackHref(r),
			"Disabled":  true,
			"Flash":     r.URL.Query().Get("flash"),
		}, h.gexAdminData(r)))
		return
	}
	loc := h.localTZ()
	now := time.Now()
	cutoff := now.AddDate(0, 0, -mdWindowDays)
	cutoffDay := cutoff.In(loc).Format("2006-01-02")

	// Needed days per Yahoo symbol, derived from every account's trades (bars
	// are shared, so this is global). roots tracks which instrument roots map
	// onto each Yahoo symbol for display.
	rows, err := h.DB.QueryContext(ctx,
		`SELECT t.symbol_root, t.entry_time, t.exit_time
		   FROM trades t
		  WHERE t.exit_time IS NULL OR t.exit_time >= ?`, cutoff.UTC())
	if err != nil {
		h.serverError(w, fmt.Errorf("market-data trades: %w", err))
		return
	}
	needed := map[string]map[string]bool{}
	roots := map[string]map[string]bool{}
	for rows.Next() {
		var root string
		var entry time.Time
		var exit sql.NullTime
		if err := rows.Scan(&root, &entry, &exit); err != nil {
			rows.Close()
			h.serverError(w, err)
			return
		}
		sym, ok := marketdata.YahooSymbol(root)
		if !ok {
			continue
		}
		var exitPtr *time.Time
		if exit.Valid {
			e := exit.Time
			exitPtr = &e
		}
		for _, d := range marketdata.TradeDays(entry, exitPtr, loc, now) {
			dk := d.Format("2006-01-02")
			if dk < cutoffDay {
				continue // older than the horizon
			}
			if needed[sym] == nil {
				needed[sym] = map[string]bool{}
				roots[sym] = map[string]bool{}
			}
			needed[sym][dk] = true
			roots[sym][strings.ToUpper(root)] = true
		}
	}
	rows.Close()

	var list []mdCoverageRow
	var totNeeded, totCached, totMissing int
	for sym, days := range needed {
		covered, err := h.MarketData.CoveredDays(ctx, sym, cutoff)
		if err != nil {
			h.serverError(w, fmt.Errorf("market-data coverage %s: %w", sym, err))
			return
		}
		var missing []string
		cached := 0
		for dk := range days {
			if covered[dk] {
				cached++
			} else {
				missing = append(missing, dk)
			}
		}
		sort.Strings(missing)
		rootList := make([]string, 0, len(roots[sym]))
		for rt := range roots[sym] {
			rootList = append(rootList, rt)
		}
		sort.Strings(rootList)
		capped := missing
		if len(capped) > 30 {
			capped = capped[:30]
		}
		list = append(list, mdCoverageRow{
			Symbol:      sym,
			Roots:       strings.Join(rootList, ", "),
			Needed:      len(days),
			Cached:      cached,
			Missing:     len(missing),
			MissingDays: capped,
		})
		totNeeded += len(days)
		totCached += cached
		totMissing += len(missing)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Symbol < list[j].Symbol })

	h.renderPage(w, r, "admin_market_data.html", mergeData(map[string]any{
		"NavActive":    "admin_market_data",
		"BackHref":     validBackHref(r),
		"WindowDays":   mdWindowDays,
		"Rows":         list,
		"TotalNeeded":  totNeeded,
		"TotalCached":  totCached,
		"TotalMissing": totMissing,
		"PrefetchOn":   h.Prefetch != nil,
		"Flash":        r.URL.Query().Get("flash"),
	}, h.gexAdminData(r)))
}

// mergeData folds extra template keys into a page's data map.
func mergeData(data map[string]any, extra map[string]any) map[string]any {
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// AdminMarketDataBackfill enqueues every account for a background cache warm.
// The prefetch worker dedupes, skips already-cached days, respects the horizon,
// and throttles - so this only fetches the gaps.
func (h *Handlers) AdminMarketDataBackfill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.Prefetch == nil {
		http.Redirect(w, r, "/admin/market-data?flash=disabled", http.StatusSeeOther)
		return
	}
	rows, err := h.DB.QueryContext(ctx, `SELECT id FROM accounts`)
	if err != nil {
		h.serverError(w, fmt.Errorf("market-data accounts: %w", err))
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			h.serverError(w, err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		h.Prefetch.EnqueueAccount(ctx, id)
	}
	http.Redirect(w, r, "/admin/market-data?flash=backfill", http.StatusSeeOther)
}
