package handlers

import (
	"fmt"
	"net/http"

	"github.com/w4jnl/trading-journal/internal/auth"
)

// DataVersion (GET /data/version) is the poll target for live_refresh.js:
// open pages compare the returned version against the one they were rendered
// with and refresh on ANY difference (versions are process-local and restart
// at zero, so "changed" must never be read as "increased"). Keyed on the
// EFFECTIVE user, so an admin acting-as follows the target user's stream.
// The read is an in-memory counter - no database work per poll.
func (h *Handlers) DataVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]uint64{"v": h.LiveData.Version(userID(r.Context()))})
}

// liveFragment reports whether this request asks for the page's live-refresh
// fragment bundle instead of the full page, and strips the flag from the URL
// so downstream link builders (pagination, filters) never propagate it.
func liveFragment(r *http.Request) bool {
	if r.URL.Query().Get("fragment") != "1" {
		return false
	}
	q := r.URL.Query()
	q.Del("fragment")
	r.URL.RawQuery = q.Encode()
	return true
}

// renderPageFragment executes one named template defined in a page file -
// the out-of-band swap bundle a live-refresh update applies. The bundle's
// regions carry hx-swap-oob (via data["OOB"]=true consumed by the shared
// region templates) and the response ends with the refreshed live_refresh
// marker so the client's version compare converges.
func (h *Handlers) renderPageFragment(w http.ResponseWriter, r *http.Request, page, name string, data map[string]any) {
	data["OOB"] = true
	if u := auth.UserFrom(r.Context()); u != nil {
		data["DataVersion"] = h.LiveData.Version(u.ID)
		// The bundle also OOB-swaps the sidebar open-position badge - the one
		// data-derived piece of chrome outside every page region.
		if _, exists := data["OpenPositionCount"]; !exists {
			if n, err := h.rq().CountOpenPositions(r.Context(), u.ID); err == nil {
				data["OpenPositionCount"] = n
			}
		}
	}
	t, err := h.parsedPage(page)
	if err != nil {
		h.serverError(w, fmt.Errorf("parse %s: %w", page, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		h.logger().Error("render fragment", "page", page, "name", name, "err", err)
	}
}
