// admin_actas.go — POST /admin/act-as/{id} and POST /admin/act-as/clear.
// The mount in main.go runs the start endpoint behind RequireAdmin; the
// clear endpoint is public-ish (any user can drop their own act-as cookie,
// even after the admin who set it loses their admin flag).
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/auth"
)

// AdminActAsStart sets the act-as cookie for the target user. RequireAdmin
// has already verified the caller is admin. Refuses self-targeting (no-op)
// and inactive targets.
func (h *Handlers) AdminActAsStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.ActAs == nil {
		http.Error(w, "act-as disabled", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	me := auth.MustUser(ctx)
	if id == me.ID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	target, err := h.rq().GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			redirectAdminUsers(w, r, "", fmt.Sprintf("user %d not found", id))
			return
		}
		h.serverError(w, fmt.Errorf("lookup: %w", err))
		return
	}
	if target.IsActive == 0 {
		redirectAdminUsers(w, r, "", fmt.Sprintf("cannot act as %q: user is deactivated", target.Username))
		return
	}
	h.ActAs.SetCookie(w, target.ID)
	// Land on the dashboard so the admin immediately sees what the target
	// user would see; the banner reminds them to return.
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// AdminActAsClear removes the act-as cookie. Mounted without RequireAdmin
// so a user who's been "acted-as into" can always recover to their own
// view if the admin who set the cookie loses privilege mid-session.
func (h *Handlers) AdminActAsClear(w http.ResponseWriter, r *http.Request) {
	if h.ActAs == nil {
		http.Error(w, "act-as disabled", http.StatusServiceUnavailable)
		return
	}
	h.ActAs.ClearCookie(w)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
