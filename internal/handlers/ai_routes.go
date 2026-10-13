package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
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
//
// The review itself runs in the background (h.AIReviews): the click returns
// at once with the note shown as under review, and the review carries on when
// the trader closes the sheet or leaves the page. While it runs the note
// shows a spinner wherever it is listed and its editor is locked; the page or
// sheet polls GET /ai/review/{entry} and redraws when the review lands.

// aiButtonGuard is the common preamble of the button routes: 404 when AI is
// off on this server, 409 without a provider or while the day job writes.
// False = stop.
func (h *Handlers) aiButtonGuard(w http.ResponseWriter, r *http.Request) bool {
	if h.AIJobs == nil || h.AIReviews == nil {
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

// aiStartReview starts the background review of the caller's entryID (a no-op
// when one is already running). The review gets the caller's identity and the
// app's lifetime, not the request's.
func (h *Handlers) aiStartReview(ctx context.Context, entryID int64) {
	user := auth.MustUser(ctx)
	h.AIReviews.Start(user.ID, entryID, h.entryTradeIDs(ctx, entryID), func(bg context.Context) error {
		_, err := h.analyzeEntry(auth.WithUser(bg, user), entryID, true)
		if err != nil {
			h.logger().Warn("ai review failed", "entry", entryID, "err", err)
		}
		return err
	})
}

// entryTradeIDs lists the trades a trade or group entry covers (none for a
// day or week), so the trades list can mark them while it is under review.
func (h *Handlers) entryTradeIDs(ctx context.Context, entryID int64) []int64 {
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: userIDPtr(ctx)})
	if err != nil {
		return nil
	}
	switch e.Scope {
	case "trade":
		if e.TradeID != nil {
			return []int64{*e.TradeID}
		}
	case "group":
		rows, err := h.rdb().QueryContext(ctx, `SELECT trade_id FROM journal_entry_trades WHERE journal_entry_id = ?`, entryID)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		return ids
	}
	return nil
}

// aiStartAndReload starts the review of entryID and reloads the page, which
// then shows the note under review and polls for the result.
func (h *Handlers) aiStartAndReload(w http.ResponseWriter, r *http.Request, entryID int64) {
	h.aiStartReview(r.Context(), entryID)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// AIAnalyzeEntry: POST /ai/entry/{id} - review an existing entry (any scope;
// the group note page uses it).
func (h *Handlers) AIAnalyzeEntry(w http.ResponseWriter, r *http.Request) {
	if !h.aiButtonGuard(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := h.Queries.GetJournalEntryByID(r.Context(), models.GetJournalEntryByIDParams{ID: id, UserID: userIDPtr(r.Context())}); err != nil {
		http.Error(w, "entry not found", http.StatusNotFound)
		return
	}
	h.aiStartAndReload(w, r, id)
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
	h.aiStartAndReload(w, r, id)
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
	h.aiStartAndReload(w, r, id)
}

// AIAnalyzeTrade: POST /ai/trade/{id} - review the trade's note, creating it
// when the trade has none. From the trade sheet it re-renders the sheet in
// place, now showing the note under review; ?page=1 (the trade journal page)
// reloads the page instead.
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
		h.aiStartAndReload(w, r, entryID)
		return
	}
	h.aiStartReview(r.Context(), entryID)
	h.renderTradeDetail(w, r, "")
}

// AIAnalyzeSet: POST /ai/set/{id} - review a copy set as one decision: its
// group note (created when no note covers the set's trades yet) gets the group
// review; the set sheet re-renders in place showing it under review.
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
	h.aiStartReview(r.Context(), entryID)
	h.renderSetDetail(w, r, "")
}

// AIReviewStatus: GET /ai/review/{id} - the poll of a page or sheet showing
// entry {id} under review. 204 while it runs (htmx swaps nothing). Once it
// has landed: then=trade&trade=N or then=set&set=N re-render that sheet in
// place (HX-Retarget), with the review or its error; anything else reloads
// the page, which shows them.
func (h *Handlers) AIReviewStatus(w http.ResponseWriter, r *http.Request) {
	if h.AIReviews == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := h.Queries.GetJournalEntryByID(r.Context(), models.GetJournalEntryByIDParams{ID: id, UserID: userIDPtr(r.Context())}); err != nil {
		http.Error(w, "entry not found", http.StatusNotFound)
		return
	}
	if h.AIReviews.EntryPending(id) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q := r.URL.Query()
	switch q.Get("then") {
	case "trade", "set":
		r.SetPathValue("id", q.Get(q.Get("then")))
		w.Header().Set("HX-Retarget", "#trade-sheet-content")
		w.Header().Set("HX-Reswap", "innerHTML")
		if q.Get("then") == "trade" {
			h.renderTradeDetail(w, r, "analyzed")
		} else {
			h.renderSetDetail(w, r, "analyzed")
		}
	default:
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	}
}
