package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Trade projections (camelCase; we never serialize tradeRow / sqlc model
// structs directly - they're snake_case) ----

type apiTradeRow struct {
	ID         int64      `json:"id"`
	AccountID  int64      `json:"accountId"`
	Account    string     `json:"account"`
	Firm       *string    `json:"firm"`
	Symbol     string     `json:"symbol"`
	SymbolRoot string     `json:"symbolRoot"`
	Direction  string     `json:"direction"`
	Qty        float64    `json:"qty"`
	EntryPrice float64    `json:"entryPrice"`
	ExitPrice  *float64   `json:"exitPrice"`
	EntryTime  time.Time  `json:"entryTime"`
	ExitTime   *time.Time `json:"exitTime"`
	GrossPnl   *float64   `json:"grossPnl"`
	Commission *float64   `json:"commission"`
	NetPnl     *float64   `json:"netPnl"`
	Status     string     `json:"status"`
	Retired    bool       `json:"retired"`
	HasJournal bool       `json:"hasJournal"`
	JournalTag string     `json:"journalTag,omitempty"`
	GroupCount int64      `json:"groupCount"`
}

type apiExecution struct {
	Time  time.Time `json:"time"`
	Side  string    `json:"side"` // buy|sell
	Role  string    `json:"role"` // entry|exit
	Qty   float64   `json:"qty"`  // qty of this fill attributed to the trade
	Price float64   `json:"price"`
}

type apiTradeDetail struct {
	ID             int64          `json:"id"`
	AccountID      int64          `json:"accountId"`
	Account        string         `json:"account"`
	Firm           *string        `json:"firm"`
	Symbol         string         `json:"symbol"`
	SymbolRoot     string         `json:"symbolRoot"`
	Direction      string         `json:"direction"`
	Qty            float64        `json:"qty"`
	EntryPrice     float64        `json:"entryPrice"`
	ExitPrice      *float64       `json:"exitPrice"`
	EntryTime      time.Time      `json:"entryTime"`
	ExitTime       *time.Time     `json:"exitTime"`
	GrossPnl       *float64       `json:"grossPnl"`
	Commission     *float64       `json:"commission"`
	NetPnl         *float64       `json:"netPnl"`
	Status         string         `json:"status"`
	BrokerTradeID  *string        `json:"brokerTradeId"`
	HasJournal     bool           `json:"hasJournal"`
	JournalEntryID *int64         `json:"journalEntryId"`
	JournalTag     string         `json:"journalTag,omitempty"`
	GroupCount     int64          `json:"groupCount"`
	Executions     []apiExecution `json:"executions"`
	Chart          *chartEnvelope `json:"chart,omitempty"`
	ChartPng       string         `json:"chartPng"`
}

func apiTradeRowFrom(t tradeRow) apiTradeRow {
	return apiTradeRow{
		ID: t.ID, AccountID: t.AccountID, Account: t.AccountName, Firm: t.AccountFirm,
		Symbol: t.Symbol, SymbolRoot: t.SymbolRoot, Direction: t.Direction, Qty: t.Qty,
		EntryPrice: t.EntryPrice, ExitPrice: t.ExitPrice, EntryTime: t.EntryTime, ExitTime: t.ExitTime,
		GrossPnl: t.GrossPnl, Commission: t.Commission, NetPnl: t.NetPnl, Status: t.Status,
		Retired: t.AccountRetired, HasJournal: t.HasJournal, JournalTag: t.JournalTag, GroupCount: t.GroupCount,
	}
}

// APITradesList: GET /api/v1/trades - filtered, paged (page size 50). Reuses the
// same filter parsing + dynamic-WHERE path as the HTML /trades page, so the API
// honors symbol_root / direction / outcome / from / to / account_id / page.
func (h *Handlers) APITradesList(w http.ResponseWriter, r *http.Request) {
	h.syncForAPIRead(r.Context(), apiWantsRefresh(r))
	ctx := r.Context()
	f, err := h.parseTradeFilters(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid filters")
		return
	}
	rows, err := listTradesFiltered(ctx, h.rdb(), f)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not list trades")
		return
	}
	total, err := countTradesFiltered(ctx, h.rdb(), f)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not count trades")
		return
	}
	items := make([]apiTradeRow, 0, len(rows))
	for _, t := range rows {
		items = append(items, apiTradeRowFrom(t))
	}
	pages := int((total + tradesPageSize - 1) / tradesPageSize)
	if pages < 1 {
		pages = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"page":  map[string]any{"page": f.Page, "pageSize": tradesPageSize, "pages": pages, "total": total},
	})
}

// APITradeDetail: GET /api/v1/trades/{id} - one trade with its fills, linked
// journal entry id (if any), group count, and cached chart data.
func (h *Handlers) APITradeDetail(w http.ResponseWriter, r *http.Request) {
	h.nudgeSyncFromAPI(r.Context())
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	d, err := h.tradeDetail(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "trade not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load trade")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// tradeDetail assembles the full trade projection (fills, linked journal entry,
// group count, cached chart). Shared by the REST detail endpoint and the MCP
// get_trade tool. Returns sql.ErrNoRows when the trade isn't owned/found.
func (h *Handlers) tradeDetail(ctx context.Context, id int64) (apiTradeDetail, error) {
	t, err := h.rq().GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		return apiTradeDetail{}, err
	}
	d := apiTradeDetail{
		ID: t.ID, AccountID: t.AccountID, Account: t.AccountName, Firm: t.AccountFirm,
		Symbol: t.Symbol, SymbolRoot: t.SymbolRoot, Direction: t.Direction, Qty: t.Qty,
		EntryPrice: t.EntryPrice, ExitPrice: t.ExitPrice, EntryTime: t.EntryTime, ExitTime: t.ExitTime,
		GrossPnl: t.GrossPnl, Commission: t.Commission, NetPnl: t.NetPnl, Status: t.Status,
		BrokerTradeID: t.BrokerTradeID,
		ChartPng:      fmt.Sprintf("/api/v1/trades/%d/chart.png", id),
	}
	if e, err := h.rq().GetJournalEntryByTradeID(ctx, &id); err == nil {
		d.HasJournal = true
		eid := e.ID
		d.JournalEntryID = &eid
		if e.Tag != nil {
			d.JournalTag = *e.Tag
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return apiTradeDetail{}, err
	}
	_ = h.rdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM journal_entry_trades WHERE trade_id = ?", id).Scan(&d.GroupCount)
	execs, err := h.rq().ListExecutionsForTrade(ctx, id)
	if err != nil {
		return apiTradeDetail{}, err
	}
	d.Executions = make([]apiExecution, 0, len(execs))
	for _, e := range execs {
		d.Executions = append(d.Executions, apiExecution{Time: e.ExecTime, Side: e.Side, Role: e.Role, Qty: e.QtyUsed, Price: e.Price})
	}
	if h.MarketData != nil {
		if trades, err := loadOneTrade(ctx, h.rdb(), id); err == nil {
			env := h.buildChartEnvelope(ctx, trades, false) // cached bars only; no network
			d.Chart = &env
		}
	}
	return d, nil
}

// APITradeChartPNG: GET /api/v1/trades/{id}/chart.png - rendered candlestick PNG
// of the trade's chart (cached data only).
func (h *Handlers) APITradeChartPNG(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := h.rq().GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)}); err != nil {
		writeJSONError(w, http.StatusNotFound, "trade not found")
		return
	}
	if h.MarketData == nil {
		writeJSONError(w, http.StatusNotFound, "charting is disabled")
		return
	}
	trades, err := loadOneTrade(ctx, h.rdb(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load trade")
		return
	}
	env := h.buildChartEnvelope(ctx, trades, false) // cached bars only; never hits the network
	h.writeChartPNG(w, r, env, tz)
}
