package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/w4jnl/trading-journal/internal/journal"
)

// JournalCoachLabel: POST /journal/coach/{date} (form: kind, key) - the
// coach card's "+ <label>" button. It puts the finding's label on the day
// note as the trader's own, creating the note when the day has none, and
// sends the browser back to the card, where the button now reads as the
// label's pill and the note at the top carries it. Before, the button opened
// the editor with the chip pre-checked and left the save to the trader; on a
// note under review the editor is hidden, so the click looked like nothing.
// A note under review still refuses (409): the card shows that button
// disabled, so this answers a stale page.
func (h *Handlers) JournalCoachLabel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	day := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	kind, key := r.PostFormValue("kind"), r.PostFormValue("key")
	if !journal.ValidLabel(kind, key) {
		http.Error(w, "unknown label", http.StatusBadRequest)
		return
	}
	entryID, err := h.aiEnsureEntry(ctx, apiCreateInput{Scope: "day", Date: day})
	if err != nil {
		h.aiEnsureError(w, err)
		return
	}
	err = h.journalSvc().AddLabel(ctx, userID(ctx), entryID, kind, key)
	var ve apiValidationError
	switch {
	case errors.Is(err, journal.ErrReviewPending):
		http.Error(w, "the note is under AI review; add the label when it is done", http.StatusConflict)
		return
	case errors.Is(err, journal.ErrNotFound):
		http.Error(w, "entry not found", http.StatusNotFound)
		return
	case errors.As(err, &ve):
		http.Error(w, ve.Msg, http.StatusBadRequest)
		return
	case err != nil:
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/journal/"+day+"#coach", http.StatusSeeOther)
}
