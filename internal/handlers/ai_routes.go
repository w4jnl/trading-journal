package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/auth"
)

// ---- web routes for server-side AI analysis (all under /ai/, session-authed) ----
//
// Every route is a no-op (404) when AIJobs is nil (no server default and no
// per-user keys). A user without a provider gets 409 with a pointer to Settings.

// aiJobRunning reports whether the calling user has a day-analysis job in progress.
func (h *Handlers) aiJobRunning(ctx context.Context) bool {
	return h.AIJobs != nil && h.AIJobs.Running(userID(ctx))
}

// guardNoAIProvider writes a 409 and returns true when the calling user has no
// provider (no own key, no server default); the message toasts in the page.
func (h *Handlers) guardNoAIProvider(w http.ResponseWriter, r *http.Request) bool {
	if !h.aiAvailable(r.Context()) {
		http.Error(w, errNoAIProvider.Error(), http.StatusConflict)
		return true
	}
	return false
}

// guardJournalingDuringAI writes a 409 and returns true when the user's AI day job
// is running, so journal edits (and ad-hoc single analyses) wait for it to finish or
// be cancelled - the day job writes those same entries.
func (h *Handlers) guardJournalingDuringAI(w http.ResponseWriter, r *http.Request) bool {
	if h.aiJobRunning(r.Context()) {
		http.Error(w, aiBusyMessage, http.StatusConflict)
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
	if h.guardNoAIProvider(w, r) {
		return
	}
	user := auth.MustUser(r.Context())
	datePath := r.PathValue("date")
	date := datePath
	if date == "last" {
		date = "" // runDayAnalysis -> prepareDayEntries defaults to the last trading day
	}
	force := r.URL.Query().Get("mode") == "all"
	// Resolved here for the status line; every entry resolves its own client
	// again, so a key changed mid-run applies to the entries after it.
	_, model, err := h.aiFor(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// The status line is app chrome, so English like the rest of the UI.
	h.AIJobs.Start(user.ID, datePath, model.Label(aiWordsFor("")), func(ctx context.Context, progress func(done, total int, current string)) error {
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

// The per-note Analyze buttons. Each is always offered (given a provider): on
// a note with no entry yet it creates one first, and on a note already
// reviewed it writes a fresh review below the earlier one (force) - an explicit
// click is never a no-op. Only the day job skips reviewed entries, so a
// cancelled run resumes where it stopped.

// aiButtonGuard is the common preamble of the button routes: 404 when AI is off
// on this server, 409 without a provider or while the day job writes, and the
// write deadline lifted for the wait on the model. False = stop.
func (h *Handlers) aiButtonGuard(w http.ResponseWriter, r *http.Request) bool {
	clearWriteDeadline(w) // waits on the AI model, possibly longer than the server WriteTimeout
	if h.AIJobs == nil {
		http.NotFound(w, r)
		return false
	}
	return !h.guardNoAIProvider(w, r) && !h.guardJournalingDuringAI(w, r)
}

// aiEnsureEntry returns the id of the entry in, creating it with the review
// placeholder (which the first review replaces) when it does not exist yet.
func (h *Handlers) aiEnsureEntry(ctx context.Context, in apiCreateInput) (int64, error) {
	in.Body = aiPlaceholderBody
	e, conflict, err := h.createJournalEntry(ctx, in)
	switch {
	case conflict != nil:
		return conflict.EntryID, nil
	case err != nil:
		return 0, err
	}
	return e.ID, nil
}

// aiReviewAndReload analyzes entryID and has htmx reload the page. A reload
// (not an in-place swap) is deliberate: a newly-assigned strategy renders in
// the strategy card and the labels in the editor, both outside the note view.
// Matches how Save already behaves on these pages.
func (h *Handlers) aiReviewAndReload(w http.ResponseWriter, r *http.Request, entryID int64) {
	if _, err := h.analyzeEntry(r.Context(), entryID, true); err != nil {
		http.Error(w, "AI analysis failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// aiEnsureError answers a failed aiEnsureEntry: the service's validation
// errors (a bad date or week, a trade that is not the caller's) are the
// caller's fault.
func (h *Handlers) aiEnsureError(w http.ResponseWriter, err error) {
	var ve apiValidationError
	if errors.As(err, &ve) {
		http.Error(w, ve.Msg, http.StatusBadRequest)
		return
	}
	http.Error(w, "could not prepare entry: "+err.Error(), http.StatusInternalServerError)
}

// AIAnalyzeEntry: POST /ai/entry/{id} - review an existing entry (any scope;
// the group note page uses it), then reload the page.
func (h *Handlers) AIAnalyzeEntry(w http.ResponseWriter, r *http.Request) {
	if !h.aiButtonGuard(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	h.aiReviewAndReload(w, r, id)
}

// AIAnalyzeDayNote: POST /ai/note/day/{date} - review the day's note (the day
// synthesis; "AI: analyze this day" also reviews every trade), creating the
// note when the day has none yet.
func (h *Handlers) AIAnalyzeDayNote(w http.ResponseWriter, r *http.Request) {
	if !h.aiButtonGuard(w, r) {
		return
	}
	id, err := h.aiEnsureEntry(r.Context(), apiCreateInput{Scope: "day", Date: r.PathValue("date")})
	if err != nil {
		h.aiEnsureError(w, err)
		return
	}
	h.aiReviewAndReload(w, r, id)
}

// AIAnalyzeWeekNote: POST /ai/note/week/{week} - review the week's note
// (YYYY-Www), creating it when the week has none yet.
func (h *Handlers) AIAnalyzeWeekNote(w http.ResponseWriter, r *http.Request) {
	if !h.aiButtonGuard(w, r) {
		return
	}
	id, err := h.aiEnsureEntry(r.Context(), apiCreateInput{Scope: "week", Week: r.PathValue("week")})
	if err != nil {
		h.aiEnsureError(w, err)
		return
	}
	h.aiReviewAndReload(w, r, id)
}

// AIAnalyzeTrade: POST /ai/trade/{id} - review the trade's note, creating it
// when the trade has none. analyzeEntry fills an unset tag, strategy and labels
// from the verdict when they clearly fit (never overwriting yours). From the
// trade sheet it re-renders the sheet in place (like Save) so the review
// appears inline; ?page=1 (the trade journal page) reloads the page instead.
func (h *Handlers) AIAnalyzeTrade(w http.ResponseWriter, r *http.Request) {
	if !h.aiButtonGuard(w, r) {
		return
	}
	tradeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	entryID, err := h.aiEnsureEntry(r.Context(), apiCreateInput{Scope: "trade", TradeID: &tradeID})
	if err != nil {
		h.aiEnsureError(w, err)
		return
	}
	if r.URL.Query().Get("page") == "1" {
		h.aiReviewAndReload(w, r, entryID)
		return
	}
	if _, err := h.analyzeEntry(r.Context(), entryID, true); err != nil {
		http.Error(w, "AI analysis failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	h.renderTradeDetail(w, r, "analyzed")
}

// AIAnalyzeSet: POST /ai/set/{id} - review a copy set as one decision: its
// group note (created when no note covers the set's trades yet) gets the group
// review, and the set sheet re-renders in place showing it.
func (h *Handlers) AIAnalyzeSet(w http.ResponseWriter, r *http.Request) {
	if !h.aiButtonGuard(w, r) {
		return
	}
	setID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	entryID, err := h.ensureSetGroup(r.Context(), setID, aiPlaceholderBody)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "set not found", http.StatusNotFound)
		return
	case err != nil:
		h.aiEnsureError(w, err)
		return
	}
	if _, err := h.analyzeEntry(r.Context(), entryID, true); err != nil {
		http.Error(w, "AI analysis failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	h.renderSetDetail(w, r, "analyzed")
}
