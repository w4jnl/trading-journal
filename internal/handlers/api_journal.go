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
	trades, err := h.apiTradesForEntry(ctx, e, tz)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load trades")
		return
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
	writeJSON(w, http.StatusOK, resp)
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
