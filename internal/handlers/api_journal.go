package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/chartpng"
	"github.com/w4jnl/trading-journal/internal/models"
)

// apiLoadEntry fetches an owned journal entry by id, writing the right JSON
// error (404 vs 500) and returning ok=false on failure.
func (h *Handlers) apiLoadEntry(w http.ResponseWriter, r *http.Request, id int64) (models.JournalEntry, bool) {
	e, err := h.Queries.GetJournalEntryByID(r.Context(), models.GetJournalEntryByIDParams{ID: id, UserID: userIDPtr(r.Context())})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "entry not found")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not load entry")
		}
		return models.JournalEntry{}, false
	}
	return e, true
}

// apiTradesForEntry dispatches the per-scope trade loaders for a JournalEntry by
// adapting it to the feedRow that tradesForEntry expects.
func (h *Handlers) apiTradesForEntry(ctx context.Context, e models.JournalEntry, tz *time.Location) ([]linkedTrade, error) {
	return h.tradesForEntry(ctx, feedRow{ID: e.ID, EntryDate: e.EntryDate, Scope: e.Scope, TradeID: e.TradeID}, tz)
}

// APIJournalList: GET /api/v1/journal  (filters via parseJournalFilters + paging)
func (h *Handlers) APIJournalList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	filters := parseJournalFilters(r, tz)
	limit := apiQueryInt(r, "limit", 50, 1, 200)
	offset := apiQueryInt(r, "offset", 0, 0, 1_000_000)

	rows, err := listJournalFeedFiltered(ctx, h.DB, filters, offset+limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not list journal")
		return
	}
	if offset < len(rows) {
		rows = rows[offset:]
	} else {
		rows = nil
	}

	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		trades, _ := h.tradesForEntry(ctx, row, tz)
		items = append(items, map[string]any{
			"id":       row.ID,
			"scope":    row.Scope,
			"date":     row.EntryDate.In(tz).Format("2006-01-02"),
			"tag":      row.Tag,
			"mood":     row.Mood,
			"strategy": row.StrategyName,
			"snippet":  apiSnippet(row.Body, 160),
			"summary":  apiSummaryFrom(summarizeTradeRows(trades)),
			"links": map[string]string{
				"self":     fmt.Sprintf("/api/v1/journal/%d", row.ID),
				"chartPng": fmt.Sprintf("/api/v1/journal/%d/chart.png", row.ID),
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"page":  map[string]int{"limit": limit, "offset": offset, "count": len(items)},
	})
}

// APIJournalDetail: GET /api/v1/journal/{id}
func (h *Handlers) APIJournalDetail(w http.ResponseWriter, r *http.Request) {
	tz := h.localTZ()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	e, ok := h.apiLoadEntry(w, r, id)
	if !ok {
		return
	}
	resp, err := h.journalDetailJSON(r, e, tz)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load trades")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// journalDetailJSON projects one entry into the detail response shape: notes
// (image refs rewritten), assigned strategy id+name only (never the body),
// trades overview + list, cached chart data, attachments, and a chartPng link.
// Shared by the detail endpoint and the create endpoint so a created entry comes
// back in exactly the shape a follow-up GET would return.
func (h *Handlers) journalDetailJSON(r *http.Request, e models.JournalEntry, tz *time.Location) (map[string]any, error) {
	ctx := r.Context()
	trades, err := h.apiTradesForEntry(ctx, e, tz)
	if err != nil {
		return nil, err
	}
	notes, ids := apiRewriteAttachmentRefs(e.Body)
	var strat *apiStrategyRef
	if s := h.loadAssignedStrategy(r, e.StrategyID); s != nil {
		strat = &apiStrategyRef{ID: s.ID, Name: s.Name} // short name only; never the body
	}
	resp := map[string]any{
		"id":          e.ID,
		"scope":       e.Scope,
		"date":        e.EntryDate.In(tz).Format("2006-01-02"),
		"tag":         e.Tag,
		"mood":        e.Mood,
		"notes":       notes,
		"strategy":    strat,
		"summary":     apiSummaryFrom(summarizeTradeRows(trades)),
		"trades":      apiTradesFrom(trades),
		"attachments": apiAttachmentsFrom(ids),
		"chartPng":    fmt.Sprintf("/api/v1/journal/%d/chart.png", e.ID),
		"createdAt":   e.CreatedAt,
		"updatedAt":   e.UpdatedAt,
	}
	if h.MarketData != nil {
		resp["chart"] = h.buildChartEnvelope(ctx, trades, false) // cached bars only; no network
	}
	return resp, nil
}

// APIJournalAppendNote: POST /api/v1/journal/{id}/notes  (append-only)
func (h *Handlers) APIJournalAppendNote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body (expected {\"note\": \"...\"})")
		return
	}
	note := strings.TrimSpace(body.Note)
	if note == "" {
		writeJSONError(w, http.StatusBadRequest, "note is empty")
		return
	}
	// Require an existing, owned entry - the API never creates entries.
	e, ok := h.apiLoadEntry(w, r, id)
	if !ok {
		return
	}
	block := formatAppendedNote(time.Now(), h.localTZ(), note)
	if err := h.Queries.AppendJournalEntryBody(ctx, models.AppendJournalEntryBodyParams{
		Body: block, ID: e.ID, UserID: userIDPtr(ctx),
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not append note")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": e.ID, "appended": true})
}

// formatAppendedNote builds the dated markdown block appended to a journal body:
// a thematic break, an italic dated heading in the trading timezone, then the
// note. Never overwrites existing content.
func formatAppendedNote(now time.Time, loc *time.Location, note string) string {
	stamp := now.In(loc).Format("2006-01-02 15:04 MST")
	return "\n\n---\n\n_Note added " + stamp + "_\n\n" + strings.TrimRight(note, " \t\n") + "\n"
}

// apiJournalCreateReq is the POST /api/v1/journal body. Which scope-specific
// field is required depends on `scope`: day->date, week->week, trade->tradeId,
// group->tradeIds.
type apiJournalCreateReq struct {
	Scope      string  `json:"scope"`
	Body       string  `json:"body"`
	Tag        *string `json:"tag"`
	Mood       *int64  `json:"mood"` // day scope only
	StrategyID *int64  `json:"strategyId"`
	Date       string  `json:"date"`     // day:  YYYY-MM-DD
	Week       string  `json:"week"`     // week: YYYY-Www
	TradeID    *int64  `json:"tradeId"`  // trade
	TradeIDs   []int64 `json:"tradeIds"` // group
}

// APIJournalCreate: POST /api/v1/journal - create one entry for any scope. Day,
// week, and trade entries are singletons: if one already exists for that date /
// ISO-week / trade, this returns 409 with the existing entryId (use the notes
// endpoint to append). Group entries have no uniqueness and always create.
func (h *Handlers) APIJournalCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	var req apiJournalCreateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tag, ok := apiParseTag(req.Tag)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "tag must be one of: good, neutral, bad")
		return
	}
	strategyID, ok := h.apiValidateStrategyID(ctx, req.StrategyID)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown strategyId")
		return
	}
	body := strings.TrimSpace(req.Body)

	switch req.Scope {
	case "day":
		h.apiCreateDay(w, r, req, body, tag, strategyID, tz)
	case "week":
		h.apiCreateWeek(w, r, req, body, tag, strategyID, tz)
	case "trade":
		h.apiCreateTrade(w, r, req, body, tag, strategyID, tz)
	case "group":
		h.apiCreateGroup(w, r, req, body, tag, strategyID, tz)
	default:
		writeJSONError(w, http.StatusBadRequest, "scope must be one of: day, week, trade, group")
	}
}

func (h *Handlers) apiCreateDay(w http.ResponseWriter, r *http.Request, req apiJournalCreateReq, body string, tag *string, strategyID *int64, tz *time.Location) {
	ctx := r.Context()
	if body == "" {
		writeJSONError(w, http.StatusBadRequest, "body is required for a day entry")
		return
	}
	if strings.TrimSpace(req.Date) == "" {
		writeJSONError(w, http.StatusBadRequest, "date is required for scope=day (YYYY-MM-DD)")
		return
	}
	date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(req.Date), tz)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad date; expected YYYY-MM-DD")
		return
	}
	now := time.Now().In(tz)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	if date.After(today) {
		writeJSONError(w, http.StatusBadRequest, "journal entries are not allowed on future dates")
		return
	}
	if existing, err := h.Queries.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: userIDPtr(ctx), EntryDate: date}); err == nil {
		h.apiConflict(w, "a day entry already exists for "+date.Format("2006-01-02"), existing.ID)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusInternalServerError, "could not check existing entry")
		return
	}
	mood, ok := apiParseMood(req.Mood)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "mood must be an integer 1-5")
		return
	}
	e, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: userIDPtr(ctx), TradeID: nil, AccountID: nil, EntryDate: date,
		Body: body, Mood: mood, Tag: tag, Scope: "day", StrategyID: strategyID,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create entry")
		return
	}
	h.apiRespondCreated(w, r, e, tz)
}

func (h *Handlers) apiCreateWeek(w http.ResponseWriter, r *http.Request, req apiJournalCreateReq, body string, tag *string, strategyID *int64, tz *time.Location) {
	ctx := r.Context()
	if body == "" {
		writeJSONError(w, http.StatusBadRequest, "body is required for a week entry")
		return
	}
	if strings.TrimSpace(req.Week) == "" {
		writeJSONError(w, http.StatusBadRequest, "week is required for scope=week (YYYY-Www, e.g. 2026-W23)")
		return
	}
	monday, _, _, err := parseISOWeek(strings.TrimSpace(req.Week), tz)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad week; expected YYYY-Www (e.g. 2026-W23)")
		return
	}
	if existing, err := h.Queries.GetWeekJournalEntry(ctx, models.GetWeekJournalEntryParams{UserID: userIDPtr(ctx), EntryDate: monday}); err == nil {
		h.apiConflict(w, "a week entry already exists for "+strings.TrimSpace(req.Week), existing.ID)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusInternalServerError, "could not check existing entry")
		return
	}
	e, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: userIDPtr(ctx), TradeID: nil, AccountID: nil, EntryDate: monday,
		Body: body, Mood: nil, Tag: tag, Scope: "week", StrategyID: strategyID,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create entry")
		return
	}
	h.apiRespondCreated(w, r, e, tz)
}

func (h *Handlers) apiCreateTrade(w http.ResponseWriter, r *http.Request, req apiJournalCreateReq, body string, tag *string, strategyID *int64, tz *time.Location) {
	ctx := r.Context()
	if body == "" {
		writeJSONError(w, http.StatusBadRequest, "body is required for a trade entry")
		return
	}
	if req.TradeID == nil || *req.TradeID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "tradeId is required for scope=trade")
		return
	}
	trade, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: *req.TradeID, UserID: userID(ctx)})
	if err != nil {
		// Foreign / unknown id - it's a request input, so 400 (not 404).
		writeJSONError(w, http.StatusBadRequest, "unknown tradeId")
		return
	}
	if existing, err := h.Queries.GetJournalEntryByTradeID(ctx, req.TradeID); err == nil {
		h.apiConflict(w, "a journal entry already exists for this trade", existing.ID)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusInternalServerError, "could not check existing entry")
		return
	}
	entryDate := trade.EntryTime
	if trade.ExitTime != nil {
		entryDate = *trade.ExitTime
	}
	accountID := trade.AccountID
	e, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: userIDPtr(ctx), TradeID: req.TradeID, AccountID: &accountID, EntryDate: entryDate,
		Body: body, Mood: nil, Tag: tag, Scope: "trade", StrategyID: strategyID,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create entry")
		return
	}
	h.apiRespondCreated(w, r, e, tz)
}

func (h *Handlers) apiCreateGroup(w http.ResponseWriter, r *http.Request, req apiJournalCreateReq, body string, tag *string, strategyID *int64, tz *time.Location) {
	ctx := r.Context()
	if len(req.TradeIDs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "tradeIds is required for scope=group (at least one trade)")
		return
	}
	// Keep only trade ids the caller actually owns, so a guessed/foreign id
	// can't be linked into the user's group entry (mirrors GroupJournalLink).
	owned := make([]int64, 0, len(req.TradeIDs))
	for _, id := range req.TradeIDs {
		var ok int64
		if err := h.DB.QueryRowContext(ctx,
			"SELECT 1 FROM trades t JOIN accounts a ON a.id = t.account_id WHERE t.id = ? AND a.user_id = ?",
			id, userID(ctx),
		).Scan(&ok); err == nil {
			owned = append(owned, id)
		}
	}
	if len(owned) == 0 {
		writeJSONError(w, http.StatusBadRequest, "no owned trades to link")
		return
	}

	// entry_date = max(exit_time) across the owned trades, in the trading tz.
	// ORDER BY ... LIMIT 1 keeps the scan on a typed column (same workaround as
	// GroupJournalCreate). Fall back to now() if none have an exit yet.
	placeholders := strings.Repeat("?,", len(owned))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(owned)+1)
	args = append(args, userID(ctx))
	for _, id := range owned {
		args = append(args, id)
	}
	var maxExit time.Time
	row := h.DB.QueryRowContext(ctx,
		"SELECT t.exit_time FROM trades t JOIN accounts a ON a.id = t.account_id WHERE a.user_id = ? AND t.id IN ("+placeholders+") AND t.exit_time IS NOT NULL ORDER BY t.exit_time DESC LIMIT 1",
		args...,
	)
	if err := row.Scan(&maxExit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			maxExit = time.Now().In(tz)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not resolve entry date")
			return
		}
	}
	y, m, d := maxExit.In(tz).Date()
	entryDate := time.Date(y, m, d, 0, 0, 0, 0, tz)

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not begin transaction")
		return
	}
	defer tx.Rollback()
	q := h.Queries.WithTx(tx)
	e, err := q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: userIDPtr(ctx), TradeID: nil, AccountID: nil, EntryDate: entryDate,
		Body: body, Mood: nil, Tag: tag, Scope: "group", StrategyID: strategyID,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create entry")
		return
	}
	for _, id := range owned {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)",
			e.ID, id,
		); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not link trade")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not commit")
		return
	}
	h.apiRespondCreated(w, r, e, tz)
}

// apiRespondCreated writes 201 + Location with the created entry rendered through
// the same projection a GET /journal/{id} would return.
func (h *Handlers) apiRespondCreated(w http.ResponseWriter, r *http.Request, e models.JournalEntry, tz *time.Location) {
	resp, err := h.journalDetailJSON(r, e, tz)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "entry created but could not render it")
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/journal/%d", e.ID))
	writeJSON(w, http.StatusCreated, resp)
}

// apiConflict writes a 409 with the id of the entry that already exists, so the
// caller can switch to POST /journal/{entryId}/notes (append-only).
func (h *Handlers) apiConflict(w http.ResponseWriter, msg string, entryID int64) {
	writeJSON(w, http.StatusConflict, map[string]any{"error": msg, "entryId": entryID})
}

// apiParseTag validates the optional tag. nil/empty -> nil (no tag); a value
// outside the whitelist -> ok=false.
func apiParseTag(t *string) (*string, bool) {
	if t == nil {
		return nil, true
	}
	s := strings.TrimSpace(*t)
	if s == "" {
		return nil, true
	}
	switch s {
	case "good", "neutral", "bad":
		return &s, true
	}
	return nil, false
}

// apiParseMood validates an optional 1-5 mood. nil -> nil; out of range -> ok=false.
func apiParseMood(m *int64) (*int64, bool) {
	if m == nil {
		return nil, true
	}
	if *m < 1 || *m > 5 {
		return nil, false
	}
	v := *m
	return &v, true
}

// apiValidateStrategyID confirms a non-null strategyId belongs to the caller.
// nil/0 -> nil (no strategy). Unknown/foreign -> ok=false (400), which is
// stricter than the UI's silent-drop but clearer for an agent.
func (h *Handlers) apiValidateStrategyID(ctx context.Context, id *int64) (*int64, bool) {
	if id == nil || *id == 0 {
		return nil, true
	}
	if _, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: *id, UserID: userID(ctx)}); err != nil {
		return nil, false
	}
	v := *id
	return &v, true
}

// APIJournalChartPNG: GET /api/v1/journal/{id}/chart.png[?contract=ES]
func (h *Handlers) APIJournalChartPNG(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	e, ok := h.apiLoadEntry(w, r, id)
	if !ok {
		return
	}
	if h.MarketData == nil {
		writeJSONError(w, http.StatusNotFound, "charting is disabled")
		return
	}
	trades, err := h.apiTradesForEntry(ctx, e, tz)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load trades")
		return
	}
	env := h.buildChartEnvelope(ctx, trades, false) // cached bars only; never hits the network
	h.writeChartPNG(w, r, env, tz)
}

// writeChartPNG renders one panel of a chart envelope as a PNG. ?contract= picks
// the panel (by contract code or symbol root) when the entry/trade spans several;
// otherwise the first panel with cached bars wins. Writes a 404 JSON error when
// nothing is cached. Shared by the journal and trade chart.png endpoints.
func (h *Handlers) writeChartPNG(w http.ResponseWriter, r *http.Request, env chartEnvelope, tz *time.Location) {
	want := strings.TrimSpace(r.URL.Query().Get("contract"))
	var chosen *chartPanel
	for i := range env.Panels {
		p := &env.Panels[i]
		if len(p.Bars) == 0 {
			continue
		}
		if want == "" || strings.EqualFold(p.Contract, want) || strings.EqualFold(p.SymbolRoot, want) {
			chosen = p
			break
		}
	}
	if chosen == nil {
		writeJSONError(w, http.StatusNotFound, "no cached chart data; open the chart in the UI first")
		return
	}
	out, err := chartpng.Render(panelToPNG(*chosen, tz), chartpng.Options{})
	if err != nil {
		h.serverError(w, fmt.Errorf("render chart png: %w", err))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

func panelToPNG(p chartPanel, tz *time.Location) chartpng.Panel {
	bars := make([]chartpng.OHLC, len(p.Bars))
	for i, b := range p.Bars {
		bars[i] = chartpng.OHLC{Time: b.Time, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume}
	}
	markers := make([]chartpng.Marker, len(p.Markers))
	for i, m := range p.Markers {
		markers[i] = chartpng.Marker{Time: m.T, Price: m.Price, Side: m.Side, Role: m.Role}
	}
	title := p.Contract
	if p.Timeframe != "" {
		title += "  " + p.Timeframe
	}
	return chartpng.Panel{Title: title, Bars: bars, Markers: markers, EntryAvg: p.EntryAvg, ExitAvg: p.ExitAvg, Loc: tz}
}

// APIAttachmentServe: GET /api/v1/attachments/{id} (user-scoped image bytes)
func (h *Handlers) APIAttachmentServe(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	h.serveAttachmentBytes(w, r, id, userID(r.Context()), true)
}

func apiQueryInt(r *http.Request, name string, def, lo, hi int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func apiSnippet(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "..."
}
