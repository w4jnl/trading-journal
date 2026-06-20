package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/chartpng"
	"github.com/w4jnl/trading-journal/internal/marketdata"
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
	tz := h.localTZ()
	filters := parseJournalFilters(r, tz)
	limit := apiQueryInt(r, "limit", 50, 1, 200)
	offset := apiQueryInt(r, "offset", 0, 0, 1_000_000)
	items, err := h.journalListItems(r.Context(), filters, offset, limit, tz)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not list journal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"page":  map[string]int{"limit": limit, "offset": offset, "count": len(items)},
	})
}

// journalListItems builds the journal-list rows for the given filters + paging.
// Shared by the REST list endpoint and the MCP list_journal tool.
func (h *Handlers) journalListItems(ctx context.Context, filters journalFilters, offset, limit int, tz *time.Location) ([]map[string]any, error) {
	rows, err := listJournalFeedFiltered(ctx, h.DB, filters, offset+limit)
	if err != nil {
		return nil, err
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
	return items, nil
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
	resp, err := h.journalDetailJSON(r.Context(), e, tz)
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
func (h *Handlers) journalDetailJSON(ctx context.Context, e models.JournalEntry, tz *time.Location) (map[string]any, error) {
	trades, err := h.apiTradesForEntry(ctx, e, tz)
	if err != nil {
		return nil, err
	}
	notes, ids := apiRewriteAttachmentRefs(e.Body)
	var strat *apiStrategyRef
	if e.StrategyID != nil {
		if s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: *e.StrategyID, UserID: userID(ctx)}); err == nil {
			strat = &apiStrategyRef{ID: s.ID, Name: s.Name} // short name only; never the body
		}
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
	if err := h.appendOrSeedJournalNote(ctx, e, note); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not append note")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": e.ID, "appended": true})
}

// appendOrSeedJournalNote adds note to an entry's body. While the entry is still
// the prepare_day placeholder (created but not yet analyzed), the note REPLACES it -
// keeping the leading "---" divider but dropping the "pending" line - so an analyzed
// entry never carries a stale placeholder above the real review. Otherwise the note
// is appended as a dated, divider-separated block (the existing append-only
// behavior). Both the REST and MCP append paths funnel through here.
func (h *Handlers) appendOrSeedJournalNote(ctx context.Context, entry models.JournalEntry, note string) error {
	note = strings.TrimSpace(note)
	if strings.TrimSpace(entry.Body) == strings.TrimSpace(aiPlaceholderBody) {
		seeded := "---\n\n" + strings.TrimRight(note, " \t\n") + "\n"
		_, err := h.DB.ExecContext(ctx,
			"UPDATE journal_entries SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?",
			seeded, entry.ID, userIDPtr(ctx))
		return err
	}
	block := formatAppendedNote(time.Now(), h.localTZ(), note)
	return h.Queries.AppendJournalEntryBody(ctx, models.AppendJournalEntryBodyParams{
		Body: block, ID: entry.ID, UserID: userIDPtr(ctx),
	})
}

// formatAppendedNote builds the dated markdown block appended to a journal body:
// a thematic break, an italic dated heading in the trading timezone, then the
// note. Never overwrites existing content.
func formatAppendedNote(now time.Time, loc *time.Location, note string) string {
	stamp := now.In(loc).Format("2006-01-02 15:04 MST")
	return "\n\n---\n\n_Note added " + stamp + "_\n\n" + strings.TrimRight(note, " \t\n") + "\n"
}

// apiCreateInput is the create-entry payload, shared by the REST POST /journal
// handler (JSON-decoded) and the MCP create_journal_entry tool. Which
// scope-specific field is required depends on `scope`: day->date, week->week,
// trade->tradeId, group->tradeIds.
type apiCreateInput struct {
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

// createConflict reports that a singleton (day/week/trade) entry already exists.
// It is returned instead of an error so callers can surface the existing id.
type createConflict struct {
	EntryID int64
	Message string
}

// apiValidationError marks a bad-input failure (-> HTTP 400) so callers can
// distinguish it from an internal error (-> 500).
type apiValidationError struct{ msg string }

func (e apiValidationError) Error() string { return e.msg }

func badInput(format string, a ...any) error { return apiValidationError{fmt.Sprintf(format, a...)} }

// APIJournalCreate: POST /api/v1/journal - create one entry for any scope. Day,
// week, and trade entries are singletons: if one already exists this returns 409
// with the existing entryId (use the notes endpoint to append). Group entries
// have no uniqueness and always create.
func (h *Handlers) APIJournalCreate(w http.ResponseWriter, r *http.Request) {
	tz := h.localTZ()
	var in apiCreateInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	e, conflict, err := h.createJournalEntry(r.Context(), in, tz)
	if conflict != nil {
		h.apiConflict(w, conflict.Message, conflict.EntryID)
		return
	}
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not create entry")
		}
		return
	}
	h.apiRespondCreated(w, r, e, tz)
}

// createJournalEntry validates and creates one entry for any scope. Shared by
// the REST POST /journal handler and the MCP create_journal_entry tool. It
// returns a *createConflict (not an error) when a day/week/trade singleton
// already exists; an apiValidationError for bad input (-> 400); other errors are
// internal (-> 500).
func (h *Handlers) createJournalEntry(ctx context.Context, in apiCreateInput, tz *time.Location) (models.JournalEntry, *createConflict, error) {
	none := models.JournalEntry{}
	tag, ok := apiParseTag(in.Tag)
	if !ok {
		return none, nil, badInput("tag must be one of: good, neutral, bad")
	}
	strategyID, ok := h.apiValidateStrategyID(ctx, in.StrategyID)
	if !ok {
		return none, nil, badInput("unknown strategyId")
	}
	body := strings.TrimSpace(in.Body)

	switch in.Scope {
	case "day":
		if body == "" {
			return none, nil, badInput("body is required for a day entry")
		}
		if strings.TrimSpace(in.Date) == "" {
			return none, nil, badInput("date is required for scope=day (YYYY-MM-DD)")
		}
		date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(in.Date), tz)
		if err != nil {
			return none, nil, badInput("bad date; expected YYYY-MM-DD")
		}
		now := time.Now().In(tz)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
		if date.After(today) {
			return none, nil, badInput("journal entries are not allowed on future dates")
		}
		if existing, err := h.Queries.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: userIDPtr(ctx), EntryDate: date}); err == nil {
			return none, &createConflict{EntryID: existing.ID, Message: "a day entry already exists for " + date.Format("2006-01-02")}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return none, nil, fmt.Errorf("check existing day entry: %w", err)
		}
		mood, ok := apiParseMood(in.Mood)
		if !ok {
			return none, nil, badInput("mood must be an integer 1-5")
		}
		e, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: userIDPtr(ctx), EntryDate: date, Body: body, Mood: mood, Tag: tag, Scope: "day", StrategyID: strategyID,
		})
		if err != nil {
			return none, nil, fmt.Errorf("create entry: %w", err)
		}
		return e, nil, nil

	case "week":
		if body == "" {
			return none, nil, badInput("body is required for a week entry")
		}
		if strings.TrimSpace(in.Week) == "" {
			return none, nil, badInput("week is required for scope=week (YYYY-Www, e.g. 2026-W23)")
		}
		monday, _, _, err := parseISOWeek(strings.TrimSpace(in.Week), tz)
		if err != nil {
			return none, nil, badInput("bad week; expected YYYY-Www (e.g. 2026-W23)")
		}
		if existing, err := h.Queries.GetWeekJournalEntry(ctx, models.GetWeekJournalEntryParams{UserID: userIDPtr(ctx), EntryDate: monday}); err == nil {
			return none, &createConflict{EntryID: existing.ID, Message: "a week entry already exists for " + strings.TrimSpace(in.Week)}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return none, nil, fmt.Errorf("check existing week entry: %w", err)
		}
		e, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: userIDPtr(ctx), EntryDate: monday, Body: body, Tag: tag, Scope: "week", StrategyID: strategyID,
		})
		if err != nil {
			return none, nil, fmt.Errorf("create entry: %w", err)
		}
		return e, nil, nil

	case "trade":
		if body == "" {
			return none, nil, badInput("body is required for a trade entry")
		}
		if in.TradeID == nil || *in.TradeID <= 0 {
			return none, nil, badInput("tradeId is required for scope=trade")
		}
		trade, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: *in.TradeID, UserID: userID(ctx)})
		if err != nil {
			// Foreign/unknown id is request input -> 400. Be specific so a client
			// self-corrects (re-list) instead of assuming a backend problem.
			return none, nil, badInput("tradeId %d not found on your accounts; re-list with list_trades and pass an exact trade id (not an entry or account id)", *in.TradeID)
		}
		if existing, err := h.Queries.GetJournalEntryByTradeID(ctx, in.TradeID); err == nil {
			return none, &createConflict{EntryID: existing.ID, Message: "a journal entry already exists for this trade"}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return none, nil, fmt.Errorf("check existing trade entry: %w", err)
		}
		entryDate := trade.EntryTime
		if trade.ExitTime != nil {
			entryDate = *trade.ExitTime
		}
		accountID := trade.AccountID
		e, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: userIDPtr(ctx), TradeID: in.TradeID, AccountID: &accountID, EntryDate: entryDate,
			Body: body, Tag: tag, Scope: "trade", StrategyID: strategyID,
		})
		if err != nil {
			return none, nil, fmt.Errorf("create entry: %w", err)
		}
		return e, nil, nil

	case "group":
		if len(in.TradeIDs) == 0 {
			return none, nil, badInput("tradeIds is required for scope=group (at least one trade)")
		}
		// Keep only trade ids the caller owns, so a guessed/foreign id can't be
		// linked into the user's group entry (mirrors GroupJournalLink).
		owned := make([]int64, 0, len(in.TradeIDs))
		for _, id := range in.TradeIDs {
			var one int64
			if err := h.DB.QueryRowContext(ctx,
				"SELECT 1 FROM trades t JOIN accounts a ON a.id = t.account_id WHERE t.id = ? AND a.user_id = ?",
				id, userID(ctx),
			).Scan(&one); err == nil {
				owned = append(owned, id)
			}
		}
		if len(owned) == 0 {
			return none, nil, badInput("none of the given tradeIds were found on your accounts (got %v); they may be entry ids or from a different day - re-list with list_trades and pass exact trade ids", in.TradeIDs)
		}
		entryDate, err := h.maxExitDate(ctx, owned, tz)
		if err != nil {
			return none, nil, err
		}
		tx, err := h.DB.BeginTx(ctx, nil)
		if err != nil {
			return none, nil, fmt.Errorf("begin: %w", err)
		}
		defer tx.Rollback()
		q := h.Queries.WithTx(tx)
		e, err := q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: userIDPtr(ctx), EntryDate: entryDate, Body: body, Tag: tag, Scope: "group", StrategyID: strategyID,
		})
		if err != nil {
			return none, nil, fmt.Errorf("create entry: %w", err)
		}
		for _, id := range owned {
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)",
				e.ID, id,
			); err != nil {
				return none, nil, fmt.Errorf("link trade %d: %w", id, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return none, nil, fmt.Errorf("commit: %w", err)
		}
		return e, nil, nil

	default:
		return none, nil, badInput("scope must be one of: day, week, trade, group")
	}
}

// maxExitDate returns the trading-tz date of the latest exit among the given
// owned trade ids (now() if none have exited yet) - used as a group entry's date.
func (h *Handlers) maxExitDate(ctx context.Context, ids []int64, tz *time.Location) (time.Time, error) {
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID(ctx))
	for _, id := range ids {
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
			return time.Time{}, fmt.Errorf("resolve entry date: %w", err)
		}
	}
	y, m, d := maxExit.In(tz).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tz), nil
}

// apiRespondCreated writes 201 + Location with the created entry rendered through
// the same projection a GET /journal/{id} would return.
func (h *Handlers) apiRespondCreated(w http.ResponseWriter, r *http.Request, e models.JournalEntry, tz *time.Location) {
	resp, err := h.journalDetailJSON(r.Context(), e, tz)
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
	s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: *id, UserID: userID(ctx)})
	if err != nil || s.ArchivedAt != nil {
		return nil, false // unknown/foreign, or archived (retired strategies aren't assignable)
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

// errNoChartData means no panel in the envelope had cached bars to render.
var errNoChartData = errors.New("no cached chart data")

// chartEnvelopePNG renders one panel of an envelope as PNG bytes. contract picks
// the panel (by contract code or symbol root) when several exist; otherwise the
// first panel with cached bars wins. Returns errNoChartData when nothing is
// cached. Shared by the chart.png endpoints and the MCP get_chart_image tool.
func (h *Handlers) chartEnvelopePNG(env chartEnvelope, contract string, tz *time.Location) ([]byte, error) {
	want := strings.TrimSpace(contract)
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
		return nil, errNoChartData
	}
	return chartpng.Render(panelToPNG(*chosen, tz), chartpng.Options{})
}

// writeChartPNG renders one panel of a chart envelope and writes it as a PNG (or
// a 404 JSON error when nothing is cached). Shared by the journal + trade
// chart.png endpoints.
func (h *Handlers) writeChartPNG(w http.ResponseWriter, r *http.Request, env chartEnvelope, tz *time.Location) {
	out, err := h.chartEnvelopePNG(env, r.URL.Query().Get("contract"), tz)
	if errors.Is(err, errNoChartData) {
		writeJSONError(w, http.StatusNotFound, "no cached chart data; open the chart in the UI first")
		return
	}
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

// ---- prepare_day: server-side copytrader clustering + idempotent entry creation
//
// The analyst workflow used to make the model cluster a day's trades and pass raw
// trade-id arrays to create_journal_entry - which weaker models get wrong
// (mis-transcribed ids surface as "unknown tradeId" and look like a backend bug).
// prepareDayEntries does the clustering + creation in Go so the model only has to
// analyze the entries it gets back (by entryId), never raw trade ids.

const (
	// aiMarkerLocal tags analyses written by the in-app (local) AI; aiMarkerMCP tags those
	// written by an external AI through the MCP server / analyst skill. Separate markers let
	// each analyze an entry independently - one's review never makes the other skip it.
	aiMarkerLocal     = "tj-ai:v1"
	aiMarkerMCP       = "tj-ai-mcp:v1"
	aiPlaceholderBody = "---\n\n_AI review_"
)

// prepDayTrade is one closed trade on the target day plus what the clusterer needs.
type prepDayTrade struct {
	ID         int64
	Symbol     string
	SymbolRoot string
	Direction  string
	EntryTime  time.Time
	EntryPrice float64
	AccountID  int64
	Account    string
	NetPnl     *float64
}

// preparedEntry is one journal entry day-prep produced (created or reused).
type preparedEntry struct {
	EntryID         int64    `json:"entryId"`
	Scope           string   `json:"scope"`
	Created         bool     `json:"created"`
	AlreadyAnalyzed bool     `json:"alreadyAnalyzed"`
	Label           string   `json:"label"`
	SymbolRoot      string   `json:"symbolRoot,omitempty"`
	Direction       string   `json:"direction,omitempty"`
	Accounts        int      `json:"accounts,omitempty"`
	NetPnl          *float64 `json:"netPnl,omitempty"`
	TradeIDs        []int64  `json:"tradeIds,omitempty"`
}

// prepareDayEntries clusters the target day's trades (copytrader fan-out -> group,
// solo -> trade), creates the missing entries idempotently, and returns them.
// date "" resolves to the last trading day before today.
func (h *Handlers) prepareDayEntries(ctx context.Context, dateStr string, tz *time.Location) (map[string]any, error) {
	var day time.Time
	if s := strings.TrimSpace(dateStr); s != "" {
		d, err := time.ParseInLocation("2006-01-02", s, tz)
		if err != nil {
			return nil, badInput("bad date; expected YYYY-MM-DD")
		}
		day = d
	} else {
		d, err := h.lastTradingDay(ctx, tz)
		if err != nil {
			return nil, err
		}
		if d.IsZero() {
			return map[string]any{"date": "", "entries": []preparedEntry{}, "note": "no closed trades found on any prior day"}, nil
		}
		day = d
	}

	trades, err := h.loadDayTradesForPrep(ctx, day, tz)
	if err != nil {
		return nil, err
	}
	groups, solos := clusterCopytrader(trades)

	entries := make([]preparedEntry, 0, len(groups)+len(solos))
	for _, g := range groups {
		ids := tradeIDsOf(g)
		pe := preparedEntry{
			Scope: "group", Label: clusterLabel(g), SymbolRoot: g[0].SymbolRoot,
			Direction: g[0].Direction, Accounts: distinctAccounts(g), NetPnl: sumNet(g), TradeIDs: ids,
		}
		if eid, ok, err := h.findGroupForTrades(ctx, ids); err != nil {
			return nil, err
		} else if ok {
			pe.EntryID, pe.Created, pe.AlreadyAnalyzed = eid, false, h.entryHasMarker(ctx, eid, aiMarkerMCP)
		} else {
			e, _, err := h.createJournalEntry(ctx, apiCreateInput{Scope: "group", TradeIDs: ids, Body: aiPlaceholderBody}, tz)
			if err != nil {
				return nil, err
			}
			pe.EntryID, pe.Created = e.ID, true
		}
		entries = append(entries, pe)
	}
	for _, t := range solos {
		tid := t.ID
		pe := preparedEntry{
			Scope: "trade", Label: t.SymbolRoot + " " + t.Direction + " (" + t.Account + ")",
			SymbolRoot: t.SymbolRoot, Direction: t.Direction, NetPnl: t.NetPnl, TradeIDs: []int64{tid},
		}
		e, conflict, err := h.createJournalEntry(ctx, apiCreateInput{Scope: "trade", TradeID: &tid, Body: aiPlaceholderBody}, tz)
		if err != nil {
			return nil, err
		}
		if conflict != nil {
			pe.EntryID, pe.Created, pe.AlreadyAnalyzed = conflict.EntryID, false, h.entryHasMarker(ctx, conflict.EntryID, aiMarkerMCP)
		} else {
			pe.EntryID, pe.Created = e.ID, true
		}
		entries = append(entries, pe)
	}

	created := 0
	for _, e := range entries {
		if e.Created {
			created++
		}
	}
	slog.Debug("prepare_day", "date", day.Format("2006-01-02"), "trades", len(trades),
		"groups", len(groups), "solos", len(solos), "entries", len(entries), "created", created)

	return map[string]any{
		"date":    day.Format("2006-01-02"),
		"entries": entries,
		"note":    "For each entry with alreadyAnalyzed=false: get_chart_image(entryId=...) (+ get_strategy if one is assigned), then append_note(entryId, analysis ending in <!-- " + aiMarkerMCP + " -->). Then synthesize the day. Only ever pass the entryId values above - never raw trade ids.",
	}, nil
}

// lastTradingDay returns the trading-tz date of the most recent closed trade
// strictly before today (zero time if none).
func (h *Handlers) lastTradingDay(ctx context.Context, tz *time.Location) (time.Time, error) {
	now := time.Now().In(tz)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	var et time.Time
	err := h.DB.QueryRowContext(ctx,
		`SELECT t.exit_time FROM trades t JOIN accounts a ON a.id = t.account_id
		 WHERE a.user_id = ? AND a.active = 1 AND t.status = 'closed' AND t.exit_time IS NOT NULL AND t.exit_time < ?
		 ORDER BY t.exit_time DESC LIMIT 1`, userID(ctx), today).Scan(&et)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("resolve last trading day: %w", err)
	}
	y, m, d := et.In(tz).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tz), nil
}

func (h *Handlers) loadDayTradesForPrep(ctx context.Context, day time.Time, tz *time.Location) ([]prepDayTrade, error) {
	end := day.AddDate(0, 0, 1)
	rows, err := h.DB.QueryContext(ctx, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.entry_time, t.entry_price, t.account_id, a.name, t.net_pnl
		FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE t.status = 'closed' AND t.exit_time IS NOT NULL AND a.active = 1 AND a.user_id = ?
		  AND t.exit_time >= ? AND t.exit_time < ?
		ORDER BY t.entry_time ASC, t.id ASC`, userID(ctx), day, end)
	if err != nil {
		return nil, fmt.Errorf("load day trades: %w", err)
	}
	defer rows.Close()
	var out []prepDayTrade
	for rows.Next() {
		var t prepDayTrade
		if err := rows.Scan(&t.ID, &t.Symbol, &t.SymbolRoot, &t.Direction, &t.EntryTime, &t.EntryPrice, &t.AccountID, &t.Account, &t.NetPnl); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// clusterCopytrader groups trades that look like one decision fanned across
// accounts: same underlying + direction, entries within ~2s, on >=2 distinct
// accounts. Micros fold into the full-size series (see copyClusterKey), so a
// master's ES and a follower's MES at the same instant count as one cluster.
// Those clusters become group entries; everything else is a solo.
func clusterCopytrader(trs []prepDayTrade) (groups [][]prepDayTrade, solos []prepDayTrade) {
	const window = 2 * time.Second
	key := func(t prepDayTrade) string { return copyClusterKey(t.SymbolRoot) }
	sort.SliceStable(trs, func(i, j int) bool {
		ki, kj := key(trs[i]), key(trs[j])
		if ki != kj {
			return ki < kj
		}
		if trs[i].Direction != trs[j].Direction {
			return trs[i].Direction < trs[j].Direction
		}
		return trs[i].EntryTime.Before(trs[j].EntryTime)
	})
	for i := 0; i < len(trs); {
		j := i + 1
		for j < len(trs) &&
			key(trs[j]) == key(trs[i]) &&
			trs[j].Direction == trs[i].Direction &&
			trs[j].EntryTime.Sub(trs[i].EntryTime) <= window {
			j++
		}
		cluster := trs[i:j]
		if distinctAccounts(cluster) >= 2 {
			groups = append(groups, cluster)
		} else {
			solos = append(solos, cluster...)
		}
		i = j
	}
	return groups, solos
}

// copyClusterKey is the underlying-price key for copytrader grouping: micros fold
// into their full-size series (MES->ES=F, MNQ->NQ=F, ...) so a master's ES and a
// follower's MES at the same time are one decision. Unmapped roots key on
// themselves.
func copyClusterKey(symbolRoot string) string {
	if sym, ok := marketdata.YahooSymbol(symbolRoot); ok {
		return sym
	}
	return strings.ToUpper(strings.TrimSpace(symbolRoot))
}

func distinctAccounts(c []prepDayTrade) int {
	seen := map[int64]bool{}
	for _, t := range c {
		seen[t.AccountID] = true
	}
	return len(seen)
}

func tradeIDsOf(c []prepDayTrade) []int64 {
	ids := make([]int64, len(c))
	for i, t := range c {
		ids[i] = t.ID
	}
	return ids
}

func sumNet(c []prepDayTrade) *float64 {
	var sum float64
	present := false
	for _, t := range c {
		if t.NetPnl != nil {
			sum += *t.NetPnl
			present = true
		}
	}
	if !present {
		return nil
	}
	return &sum
}

func clusterLabel(c []prepDayTrade) string {
	return fmt.Sprintf("%s %s x%d accts", strings.Join(distinctRoots(c), "/"), c[0].Direction, distinctAccounts(c))
}

// distinctRoots returns the unique symbol roots in a cluster (sorted), so a
// cross-instrument copytrader group reads as e.g. "ES/MES".
func distinctRoots(c []prepDayTrade) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c {
		if !seen[t.SymbolRoot] {
			seen[t.SymbolRoot] = true
			out = append(out, t.SymbolRoot)
		}
	}
	sort.Strings(out)
	return out
}

// findGroupForTrades returns the id of an existing group entry (owned by the
// caller) that already includes any of the given trades, so day-prep reuses it
// rather than creating a duplicate group.
func (h *Handlers) findGroupForTrades(ctx context.Context, ids []int64) (int64, bool, error) {
	if len(ids) == 0 {
		return 0, false, nil
	}
	ph := strings.Repeat("?,", len(ids))
	ph = ph[:len(ph)-1]
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID(ctx))
	for _, id := range ids {
		args = append(args, id)
	}
	var eid int64
	err := h.DB.QueryRowContext(ctx,
		`SELECT jet.journal_entry_id FROM journal_entry_trades jet
		 JOIN journal_entries je ON je.id = jet.journal_entry_id
		 WHERE je.user_id = ? AND je.scope = 'group' AND jet.trade_id IN (`+ph+`) LIMIT 1`, args...).Scan(&eid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find group for trades: %w", err)
	}
	return eid, true, nil
}

func (h *Handlers) entryHasMarker(ctx context.Context, entryID int64, marker string) bool {
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: userIDPtr(ctx)})
	if err != nil {
		return false
	}
	return strings.Contains(e.Body, marker)
}

// APIPrepareDay: POST /api/v1/journal/prepare-day  {date?: "YYYY-MM-DD"}
func (h *Handlers) APIPrepareDay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Date string `json:"date"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	}
	resp, err := h.prepareDayEntries(r.Context(), body.Date, h.localTZ())
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not prepare day")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// assignStrategyIfUnset sets an entry's strategy ONLY when it currently has none,
// so an existing assignment is never changed (assigned=false then). It applies to
// trade and group entries only - a strategy describes a single setup, while a
// day/week entry spans mixed setups. Returns an apiValidationError for an unknown
// entry/strategy or a day/week scope. Used by the analyst (and the in-app AI) to
// fill in a fitting strategy on any entry without touching one the user set.
func (h *Handlers) assignStrategyIfUnset(ctx context.Context, entryID, strategyID int64) (bool, error) {
	uid := userIDPtr(ctx)
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: uid})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, badInput("entry %d not found", entryID)
		}
		return false, fmt.Errorf("load entry: %w", err)
	}
	// A strategy describes a single setup, so it only applies to a trade or a group (one
	// copytrader cluster). A day/week entry spans mixed setups - never attach one.
	if e.Scope != "trade" && e.Scope != "group" {
		return false, badInput("strategy applies to trade or group entries, not %s", e.Scope)
	}
	s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: strategyID, UserID: userID(ctx)})
	if err != nil {
		return false, badInput("unknown strategyId %d", strategyID)
	}
	if s.ArchivedAt != nil {
		return false, badInput("strategy %d is archived (retired) and cannot be assigned", strategyID)
	}
	res, err := h.DB.ExecContext(ctx,
		"UPDATE journal_entries SET strategy_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND strategy_id IS NULL",
		strategyID, entryID, *uid)
	if err != nil {
		return false, fmt.Errorf("assign strategy: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// APIJournalAssignStrategy: POST /api/v1/journal/{id}/strategy  {strategyId}
// Assigns a strategy only if the entry has none yet (never overwrites).
func (h *Handlers) APIJournalAssignStrategy(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		StrategyID int64 `json:"strategyId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.StrategyID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "strategyId is required")
		return
	}
	assigned, err := h.assignStrategyIfUnset(r.Context(), id, body.StrategyID)
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not assign strategy")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "assigned": assigned})
}

// assignTagIfUnset sets an entry's tag (good|neutral|bad) ONLY when it has none
// yet, so an existing tag is never changed (assigned=false then). Mirrors
// assignStrategyIfUnset; lets the analyst classify freshly-created entries
// without touching tags the user already set.
func (h *Handlers) assignTagIfUnset(ctx context.Context, entryID int64, tag string) (bool, error) {
	switch tag {
	case "good", "neutral", "bad":
	default:
		return false, badInput("tag must be one of: good, neutral, bad")
	}
	uid := userIDPtr(ctx)
	if _, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: uid}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, badInput("entry %d not found", entryID)
		}
		return false, fmt.Errorf("load entry: %w", err)
	}
	res, err := h.DB.ExecContext(ctx,
		"UPDATE journal_entries SET tag = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND tag IS NULL",
		tag, entryID, *uid)
	if err != nil {
		return false, fmt.Errorf("assign tag: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// APIJournalAssignTag: POST /api/v1/journal/{id}/tag  {tag}
// Sets the tag only if the entry has none yet (never overwrites).
func (h *Handlers) APIJournalAssignTag(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Tag == "" {
		writeJSONError(w, http.StatusBadRequest, "tag is required")
		return
	}
	assigned, err := h.assignTagIfUnset(r.Context(), id, body.Tag)
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not assign tag")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "assigned": assigned})
}
