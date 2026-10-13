package handlers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- the intraday guardrail's web side -------------------------------------------
//
// internal/guardrail records the nudges; pages of a user who turned it on
// take them (POST /nudges/take, guardrail.js) and show each once - a toast,
// and a browser notification when the tab is in the background. The day
// page lists the day's nudges under the coach.

// nudgeStale: an undelivered nudge older than this is not shown any more
// (the moment has passed); it stays listed on its day.
const nudgeStale = 2 * time.Hour

// nudgeTakeMu makes a take atomic across tabs: each nudge reaches one page.
var nudgeTakeMu sync.Mutex

// guardrailOn reports whether the caller turned the guardrail on.
func (h *Handlers) guardrailOn(ctx context.Context) bool {
	u, err := h.rq().GetUserByID(ctx, userID(ctx))
	return err == nil && u.Guardrail == 1
}

// apiNudge is a nudge as the page gets it.
type apiNudge struct {
	ID       int64  `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Lesson   string `json:"lesson,omitempty"`
	Href     string `json:"href,omitempty"`
}

// NudgesTake: POST /nudges/take - the caller's new nudges, marked shown.
func (h *Handlers) NudgesTake(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)
	nudgeTakeMu.Lock()
	defer nudgeTakeMu.Unlock()
	rows, err := h.Queries.ListUndeliveredNudges(ctx, models.ListUndeliveredNudgesParams{
		UserID: uid, CreatedAt: time.Now().UTC().Add(-nudgeStale).Truncate(time.Second),
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not read the nudges")
		return
	}
	out := make([]apiNudge, 0, len(rows))
	for _, n := range rows {
		a := apiNudge{ID: n.ID, Severity: n.Severity, Title: n.Title, Detail: n.Detail, Href: n.Href}
		if n.Lesson != nil {
			a.Lesson = *n.Lesson
		}
		out = append(out, a)
	}
	if len(rows) > 0 {
		// Up to the newest listed: a stale one (not listed) goes too.
		if err := h.Queries.MarkNudgesDelivered(ctx, models.MarkNudgesDeliveredParams{UserID: uid, ID: rows[len(rows)-1].ID}); err != nil {
			h.logger().Warn("mark nudges delivered", "err", err)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"nudges": out})
}

// SettingsGuardrailSave: POST /settings/guardrail - turn the nudges on/off.
func (h *Handlers) SettingsGuardrailSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var on int64
	if r.PostFormValue("on") == "1" {
		on = 1
	}
	if err := h.Queries.SetUserGuardrail(ctx, models.SetUserGuardrailParams{Guardrail: on, ID: userID(ctx)}); err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?flash=guardrail#guardrail", http.StatusSeeOther)
}

// dayNudges are the nudges of a day for the day page.
func (h *Handlers) dayNudges(ctx context.Context, day string) []models.Nudge {
	rows, err := h.rq().ListNudgesForDay(ctx, models.ListNudgesForDayParams{UserID: userID(ctx), Day: day})
	if err != nil {
		h.logger().Warn("day nudges", "err", err)
		return nil
	}
	return rows
}
