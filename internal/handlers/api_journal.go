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
	rows, err := listJournalFeedFiltered(ctx, h.rdb(), filters, offset+limit)
	if err != nil {
		return nil, err
	}
	if offset < len(rows) {
		rows = rows[offset:]
	} else {
		rows = nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	labels, err := loadEntryLabels(ctx, h.rdb(), ids)
	if err != nil {
		return nil, err
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
			"labels":   labelsOut(labels[row.ID]),
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
	labels, err := h.journalSvc().EntryLabels(ctx, userID(ctx), e.ID)
	if err != nil {
		return nil, err
	}
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
		"labels":      labelsOut(labels),
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
	if h.guardJournalingDuringAIJSON(w, r) {
		return
	}
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

// APIJournalCreate: POST /api/v1/journal - create one entry for any scope. Day,
// week, and trade entries are singletons: if one already exists this returns 409
// with the existing entryId (use the notes endpoint to append). Group entries
// have no uniqueness and always create.
func (h *Handlers) APIJournalCreate(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAIJSON(w, r) {
		return
	}
	tz := h.localTZ()
	var in apiCreateInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	e, conflict, err := h.createJournalEntry(r.Context(), in)
	if conflict != nil {
		h.apiConflict(w, conflict.Message, conflict.EntryID)
		return
	}
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not create entry")
		}
		return
	}
	h.apiRespondCreated(w, r, e, tz)
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
	h.serveAttachmentBytes(w, r, id, userID(r.Context()))
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

// APIPrepareDay: POST /api/v1/journal/prepare-day  {date?: "YYYY-MM-DD"}
func (h *Handlers) APIPrepareDay(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAIJSON(w, r) {
		return
	}
	var body struct {
		Date string `json:"date"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	}
	resp, err := h.prepareDayEntries(r.Context(), body.Date)
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not prepare day")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// APIJournalAssignStrategy: POST /api/v1/journal/{id}/strategy  {strategyId}
// Assigns a strategy only if the entry has none yet (never overwrites).
func (h *Handlers) APIJournalAssignStrategy(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAIJSON(w, r) {
		return
	}
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
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not assign strategy")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "assigned": assigned})
}

// APIJournalAssignTag: POST /api/v1/journal/{id}/tag  {tag}
// Sets the tag only if the entry has none yet (never overwrites).
func (h *Handlers) APIJournalAssignTag(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAIJSON(w, r) {
		return
	}
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
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not assign tag")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "assigned": assigned})
}
