package handlers

import "net/http"

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
