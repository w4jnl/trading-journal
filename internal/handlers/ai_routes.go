package handlers

import (
	"context"
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/auth"
)

// ---- web routes for server-side AI analysis (all under /ai/, session-authed) ----
//
// Every route is a no-op (404) when AIJobs is nil, i.e. ai.enabled is false.

// aiJobRunning reports whether the calling user has a day-analysis job in progress.
func (h *Handlers) aiJobRunning(ctx context.Context) bool {
	return h.AIJobs != nil && h.AIJobs.Running(userID(ctx))
}

// guardJournalingDuringAI writes a 409 and returns true when the user's AI day job
// is running, so journal edits (and ad-hoc single analyses) wait for it to finish or
// be cancelled - the day job writes those same entries.
func (h *Handlers) guardJournalingDuringAI(w http.ResponseWriter, r *http.Request) bool {
	if h.aiJobRunning(r.Context()) {
		http.Error(w, "AI analysis is running — cancel it or wait before editing the journal.", http.StatusConflict)
		return true
	}
	return false
}

// renderAIStatus renders the HTMX status fragment for the user's day job. datePath is
// the {date} segment used in the polling URLs ("last" or YYYY-MM-DD).
func (h *Handlers) renderAIStatus(w http.ResponseWriter, _ *http.Request, uid int64, datePath string) {
	h.renderPartial(w, "_ai_status.html", "ai_status", map[string]any{
		"Status":   h.AIJobs.Status(uid),
		"DatePath": datePath,
	})
}

// AIDayStart: POST /ai/day/{date} - start (or resume) the day-analysis job. date may
// be "last" for the last recorded day; ?mode=all re-analyzes already-marked entries.
func (h *Handlers) AIDayStart(w http.ResponseWriter, r *http.Request) {
	if h.AIJobs == nil {
		http.NotFound(w, r)
		return
	}
	user := auth.MustUser(r.Context())
	datePath := r.PathValue("date")
	date := datePath
	if date == "last" {
		date = "" // runDayAnalysis -> prepareDayEntries defaults to the last trading day
	}
	force := r.URL.Query().Get("mode") == "all"
	h.AIJobs.Start(user.ID, datePath, func(ctx context.Context, progress func(done, total int, current string)) error {
		return h.runDayAnalysis(auth.WithUser(ctx, user), date, force, progress)
	})
	h.renderAIStatus(w, r, user.ID, datePath)
}

// AIDayStatus: GET /ai/day/{date}/status - the HTMX status fragment (self-polls while running).
func (h *Handlers) AIDayStatus(w http.ResponseWriter, r *http.Request) {
	if h.AIJobs == nil {
		http.NotFound(w, r)
		return
	}
	h.renderAIStatus(w, r, auth.MustUser(r.Context()).ID, r.PathValue("date"))
}

// AIDayCancel: POST /ai/day/{date}/cancel.
func (h *Handlers) AIDayCancel(w http.ResponseWriter, r *http.Request) {
	if h.AIJobs == nil {
		http.NotFound(w, r)
		return
	}
	user := auth.MustUser(r.Context())
	h.AIJobs.Cancel(user.ID)
	h.renderAIStatus(w, r, user.ID, r.PathValue("date"))
}

// AIAnalyzeEntry: POST /ai/entry/{id} - analyze one entry (appends the review and fills an
// unset tag/strategy that fits), then reload the page. A reload (not an in-place swap) is
// deliberate: the newly-assigned strategy renders in the strategy card, which lives OUTSIDE
// the entry view, so swapping only the view would hide it - and the Analyze button then
// hides itself (the entry is now marked). Matches how Save already behaves on these pages.
func (h *Handlers) AIAnalyzeEntry(w http.ResponseWriter, r *http.Request) {
	if h.AIJobs == nil {
		http.NotFound(w, r)
		return
	}
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := h.analyzeEntry(r.Context(), id, false); err != nil {
		http.Error(w, "AI analysis failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// AIAnalyzeTrade: POST /ai/trade/{id} - ensure the trade's journal entry exists, then
// analyze it. analyzeEntry fills an unset tag/strategy from the verdict when one clearly
// fits (never overwriting yours). Re-renders the trade sheet in place (like Save) so the
// analysis appears inline and the sheet stays open.
func (h *Handlers) AIAnalyzeTrade(w http.ResponseWriter, r *http.Request) {
	if h.AIJobs == nil {
		http.NotFound(w, r)
		return
	}
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	tradeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tz := h.localTZ()
	entry, conflict, err := h.createJournalEntry(r.Context(), apiCreateInput{Scope: "trade", TradeID: &tradeID, Body: aiPlaceholderBody}, tz)
	var entryID int64
	switch {
	case conflict != nil:
		entryID = conflict.EntryID
	case err == nil:
		entryID = entry.ID
	default:
		http.Error(w, "could not prepare entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := h.analyzeEntry(r.Context(), entryID, false); err != nil {
		http.Error(w, "AI analysis failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	h.renderTradeDetail(w, r, "analyzed")
}
