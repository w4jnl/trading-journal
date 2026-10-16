package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- AI from the phone -------------------------------------------------------
//
// Every AI call runs on the server with the user's own provider (aiFor), so
// the phone only starts work and reads results. GET /me says whether this
// user can (ai.ready); a start without a provider answers 409 with reason
// "ai-off", and 404 when the server runs no AI at all. Results ride on the
// objects the phone already reads: the review state on GET /journal/{id},
// the levels on a position, the draft on GET /plan. Each start and each
// finish bumps the live version (AIReviews.OnChange, AITasks.OnChange), so
// the phone's event stream refetches without polling. Starts need a device
// key, like every other trader action from the phone.

// apiAIState is /me's "ai": whether this user can run AI work.
func (h *Handlers) apiAIState(r *http.Request) map[string]any {
	return map[string]any{"ready": h.aiPossible() && h.aiAvailable(r.Context())}
}

// apiAIGuard is the preamble of every AI start: a device key, AI on this
// server (on says so), a provider for the caller. False = an error was written.
func (h *Handlers) apiAIGuard(w http.ResponseWriter, r *http.Request, on bool) bool {
	if !h.requireDeviceKey(w, r) {
		return false
	}
	if !on {
		writeJSONError(w, http.StatusNotFound, "AI is off on this server")
		return false
	}
	if !h.aiAvailable(r.Context()) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": errNoAIProvider.Error(), "reason": "ai-off"})
		return false
	}
	return true
}

// apiReviewStateCtx is the journal detail's "review": whether a review of
// the entry runs, and the failure of the last one, shown once.
func (h *Handlers) apiReviewStateCtx(ctx context.Context, entryID int64) map[string]any {
	out := map[string]any{"pending": false}
	if h.AIReviews == nil {
		return out
	}
	out["pending"] = h.AIReviews.EntryPending(entryID)
	if msg := h.AIReviews.TakeError(userID(ctx), entryID); msg != "" {
		out["error"] = msg
	}
	return out
}

// apiReviewStart is the body of POST /ai/review: the entry to review, by id
// or by what it is about (created with the review placeholder when it does
// not exist yet, as the web's buttons do).
type apiReviewStart struct {
	EntryID *int64 `json:"entryId"`
	Scope   string `json:"scope"` // day | week | trade | set, with the field below
	Date    string `json:"date"`
	Week    string `json:"week"`
	TradeID *int64 `json:"tradeId"`
	SetID   *int64 `json:"setId"`
}

// APIReviewStart: POST /api/v1/ai/review (device key) - review a note in the
// background: {"entryId"} for an existing note, or {"scope":"trade","tradeId"},
// {"scope":"set","setId"}, {"scope":"day","date"}, {"scope":"week","week"} for
// the note of that thing, created first when there is none. 202 with the
// entry id; 409 reason "review-pending" while one runs on it.
func (h *Handlers) APIReviewStart(w http.ResponseWriter, r *http.Request) {
	if !h.apiAIGuard(w, r, h.AIJobs != nil && h.AIReviews != nil) {
		return
	}
	ctx := r.Context()
	var in apiReviewStart
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var entryID int64
	var err error
	switch {
	case in.EntryID != nil:
		entryID = *in.EntryID
		if _, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: userIDPtr(ctx)}); err != nil {
			writeJSONError(w, http.StatusNotFound, "entry not found")
			return
		}
	case in.Scope == "trade" && in.TradeID != nil:
		entryID, err = h.aiEnsureEntry(ctx, apiCreateInput{Scope: "trade", TradeID: in.TradeID})
	case in.Scope == "set" && in.SetID != nil:
		entryID, err = h.ensureSetGroup(ctx, *in.SetID, aiPlaceholderBody)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "set not found")
			return
		}
	case in.Scope == "day" && in.Date != "":
		entryID, err = h.aiEnsureEntry(ctx, apiCreateInput{Scope: "day", Date: in.Date})
	case in.Scope == "week" && in.Week != "":
		entryID, err = h.aiEnsureEntry(ctx, apiCreateInput{Scope: "week", Week: in.Week})
	default:
		writeJSONError(w, http.StatusBadRequest, "name the note: entryId, or scope with tradeId, setId, date or week")
		return
	}
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "could not prepare the note")
		return
	}
	if h.AIReviews.EntryPending(entryID) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a review of this note is running", "reason": "review-pending", "entryId": entryID})
		return
	}
	h.aiStartReview(ctx, entryID)
	writeJSON(w, http.StatusAccepted, map[string]any{"entryId": entryID, "pending": true})
}

// APIDayRunStart: POST /api/v1/ai/day/{date} (device key) - start the day
// run, which reviews every note of the day and writes the day's synthesis;
// {date} may be "last". Body {"mode":"all"} re-reviews what was reviewed.
// 202 with the run's status; 409 reason "day-run" while one runs.
func (h *Handlers) APIDayRunStart(w http.ResponseWriter, r *http.Request) {
	if !h.apiAIGuard(w, r, h.AIJobs != nil) {
		return
	}
	ctx := r.Context()
	uid := userID(ctx)
	if h.AIJobs.Running(uid) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": aiDayRunMessage, "reason": "day-run", "status": h.AIJobs.Status(uid)})
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	datePath := r.PathValue("date")
	date := datePath
	if date == "last" {
		date = ""
	} else if _, err := time.Parse("2006-01-02", date); err != nil {
		writeJSONError(w, http.StatusBadRequest, "date must be YYYY-MM-DD or last")
		return
	}
	_, model, err := h.aiFor(ctx)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	user := auth.MustUser(ctx)
	force := in.Mode == "all"
	h.AIJobs.Start(uid, datePath, model.Label(aiWordsFor("")), func(bg context.Context, progress func(done, total int, current string)) error {
		return h.runDayAnalysis(auth.WithUser(bg, user), date, force, progress)
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"status": h.AIJobs.Status(uid)})
}

// APIDayRunStatus: GET /api/v1/ai/day - the caller's day run: state idle,
// running, done, error or cancelled, with progress.
func (h *Handlers) APIDayRunStatus(w http.ResponseWriter, r *http.Request) {
	if h.AIJobs == nil {
		writeJSONError(w, http.StatusNotFound, "AI is off on this server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": h.AIJobs.Status(userID(r.Context()))})
}

// APIDayRunCancel: DELETE /api/v1/ai/day (device key) - cancel the run; the
// notes reviewed so far keep their reviews.
func (h *Handlers) APIDayRunCancel(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	if h.AIJobs == nil {
		writeJSONError(w, http.StatusNotFound, "AI is off on this server")
		return
	}
	uid := userID(r.Context())
	h.AIJobs.Cancel(uid)
	writeJSON(w, http.StatusOK, map[string]any{"status": h.AIJobs.Status(uid)})
}

// apiPositionAI is a position's "ai": the stop and target the model proposed
// for it while open, and whether a proposal is being made.
type apiPositionAI struct {
	Pending  bool       `json:"pending"`
	Error    string     `json:"error,omitempty"`
	Levels   *aiLevels  `json:"levels,omitempty"`
	Model    string     `json:"model,omitempty"`
	At       *time.Time `json:"at,omitempty"`
	Outdated bool       `json:"outdated,omitempty"` // the size or entry changed since
	Auto     string     `json:"auto,omitempty"`     // what a new position gets without asking: off, suggest, plan
}

func (h *Handlers) apiPositionAIFrom(v positionAIView) *apiPositionAI {
	out := &apiPositionAI{Pending: v.Pending, Error: v.Error, Auto: v.Auto}
	if v.Has {
		levels := v.Levels
		at := v.At
		out.Levels, out.Model, out.At, out.Outdated = &levels, v.Model, &at, v.Outdated
	}
	return out
}

// apiOpenPosition finds the caller's open position; the status says why not.
func (h *Handlers) apiOpenPosition(r *http.Request) (*positionView, int, error) {
	ctx := r.Context()
	accountID, err := strconv.ParseInt(r.PathValue("account"), 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("bad account id")
	}
	symbol := r.PathValue("symbol")
	if _, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: accountID, UserID: userID(ctx)}); err != nil {
		return nil, http.StatusNotFound, errors.New("account not found")
	}
	rows, err := h.rq().ListOpenPositions(ctx, userID(ctx))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	views := h.openPositionRows(ctx, rows)
	for i := range views {
		if views[i].AccountID == accountID && views[i].Symbol == symbol && views[i].Qty != 0 && views[i].AvgEntry != nil {
			return &views[i], http.StatusOK, nil
		}
	}
	return nil, http.StatusConflict, errors.New("no open position on this account and contract")
}

// apiPositionItem is one position as GET /positions lists it, after a change.
func (h *Handlers) apiPositionItem(r *http.Request, accountID int64, symbol string) (apiPosition, bool) {
	items, err := h.openPositionsAPI(r.Context())
	if err != nil {
		return apiPosition{}, false
	}
	for _, it := range items {
		if it.AccountID == accountID && it.Symbol == symbol {
			return it, true
		}
	}
	return apiPosition{}, false
}

// APIPositionLevelsStart: POST /api/v1/positions/{account}/{symbol}/levels
// (device key) - ask the model for a stop and target on the open position,
// in the background; the position's "ai" shows pending, then the levels.
func (h *Handlers) APIPositionLevelsStart(w http.ResponseWriter, r *http.Request) {
	if !h.apiAIGuard(w, r, h.AITasks != nil) {
		return
	}
	pos, code, err := h.apiOpenPosition(r)
	if err != nil {
		writeJSONError(w, code, err.Error())
		return
	}
	ctx := r.Context()
	accountID, symbol := pos.AccountID, pos.Symbol
	h.startTask(ctx, aiTaskKey(userID(ctx), "levels", strconv.FormatInt(accountID, 10), symbol), func(ctx context.Context) error {
		return h.suggestPositionLevels(ctx, accountID, symbol)
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true})
}

// APIPositionLevelsApply: POST /api/v1/positions/{account}/{symbol}/levels/apply
// (device key) - write the proposed stop and first target into the plan
// (source 'ai', as the sheet's Apply does), on every leg of a copied
// decision. Answers with the position as GET /positions lists it.
func (h *Handlers) APIPositionLevelsApply(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	if h.AITasks == nil {
		writeJSONError(w, http.StatusNotFound, "AI is off on this server")
		return
	}
	pos, code, err := h.apiOpenPosition(r)
	if err != nil {
		writeJSONError(w, code, err.Error())
		return
	}
	ctx := r.Context()
	v := h.positionSuggestion(ctx, *pos)
	if !v.Has {
		writeJSONError(w, http.StatusConflict, "no suggestion to apply")
		return
	}
	if _, err := h.applyPositionSuggestion(ctx, *pos, v); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not write the plan")
		return
	}
	item, ok := h.apiPositionItem(r, pos.AccountID, pos.Symbol)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "position vanished")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// APIPositionLevelsDismiss: DELETE /api/v1/positions/{account}/{symbol}/levels
// (device key) - forget the proposal. 204.
func (h *Handlers) APIPositionLevelsDismiss(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	pos, code, err := h.apiOpenPosition(r)
	if err != nil {
		writeJSONError(w, code, err.Error())
		return
	}
	ctx := r.Context()
	if pos.Opened.ID != 0 {
		_ = h.Queries.DeleteAIOutput(ctx, models.DeleteAIOutputParams{UserID: userID(ctx), Kind: aiKindPositionLevels, Ref: aiLevelsRef(pos.AccountID, pos.Symbol, pos.Opened.ID)})
		h.LiveData.Bump(userID(ctx))
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiPlanDraft is GET /plan's "draft" for today or a later day: whether a
// draft is being written, the failure of the last attempt (shown once), and
// the draft itself until the plan is saved or the draft discarded.
type apiPlanDraft struct {
	Pending bool   `json:"pending"`
	Error   string `json:"error,omitempty"`
	*planDraftData
	Model     string     `json:"model,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// apiPlanDraftFor loads the draft state of day; nil for a past day or a
// server without AI tasks.
func (h *Handlers) apiPlanDraftFor(ctx context.Context, day string) *apiPlanDraft {
	if h.AITasks == nil || day < time.Now().In(h.localTZ()).Format("2006-01-02") {
		return nil
	}
	uid := userID(ctx)
	key := aiTaskKey(uid, "plan", day)
	out := &apiPlanDraft{Pending: h.AITasks.Pending(key), Error: h.AITasks.TakeError(uid, key)}
	if row, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindPlanDraft, Ref: day}); err == nil {
		var data planDraftData
		if json.Unmarshal([]byte(row.Data), &data) == nil {
			at := row.CreatedAt
			out.planDraftData, out.Model, out.CreatedAt = &data, row.Model, &at
		}
	}
	return out
}

// apiPlanDay reads and checks the {date} of a draft call: today or later.
func (h *Handlers) apiPlanDay(w http.ResponseWriter, r *http.Request) (string, bool) {
	day := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		writeJSONError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return "", false
	}
	if day < time.Now().In(h.localTZ()).Format("2006-01-02") {
		writeJSONError(w, http.StatusBadRequest, "a plan is drafted for today or a later day")
		return "", false
	}
	return day, true
}

// APIPlanDraftStart: POST /api/v1/plan/{date}/draft (device key) - draft the
// day's plan from the trader's own numbers and lessons, in the background;
// GET /plan shows it under "draft" to take over or discard. 202.
func (h *Handlers) APIPlanDraftStart(w http.ResponseWriter, r *http.Request) {
	if !h.apiAIGuard(w, r, h.AITasks != nil) {
		return
	}
	day, ok := h.apiPlanDay(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	h.startTask(ctx, aiTaskKey(userID(ctx), "plan", day), func(ctx context.Context) error { return h.draftPlan(ctx, day) })
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true})
}

// APIPlanDraftDiscard: DELETE /api/v1/plan/{date}/draft (device key). 204.
func (h *Handlers) APIPlanDraftDiscard(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	day, ok := h.apiPlanDay(w, r)
	if !ok {
		return
	}
	h.dropPlanDraft(r.Context(), day)
	w.WriteHeader(http.StatusNoContent)
}
