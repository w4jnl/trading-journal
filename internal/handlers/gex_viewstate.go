package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Sticky per-user view state for the two /gex tabs. The stored value is a
// CANONICAL relative URL rebuilt from validated fields - never raw client
// input - and bare entry links (/gex, /gex/exposure: no market segment, no
// query) 303 to it, so opening the page from any browser or device restores
// the last market/filters/view. Explicit paths and any query params always
// win and, via the client's state reports, become the new stored state.
// ?at= (historical browsing) is never reported, so it cannot clobber the
// saved live view.

const (
	gexPrefOverview = "gex_overview"
	gexPrefExposure = "gex_exposure"
)

// gexCanonicalViewURL rebuilds the stored URL from validated parts. The
// market segment is always explicit (that is what makes the bare-entry
// redirect loop-free). A view key only makes sense without an expiry
// filter - a filter forces the blend slice. The Overview stores expiry +
// view but never bucket: its bin control is client-side re-binning,
// persisted in localStorage, not a server param.
func gexCanonicalViewURL(page, market string, expiries []string, bucket int, view string) string {
	base := "/gex"
	if page == gexPrefExposure {
		base = "/gex/exposure"
	}
	base += "/" + market
	q := url.Values{}
	if len(expiries) > 0 {
		q.Set("expiry", strings.Join(expiries, ","))
	}
	if view != "" && len(expiries) == 0 {
		q.Set("view", view)
	}
	if page == gexPrefExposure && bucket > 0 {
		q.Set("bucket", strconv.Itoa(bucket))
	}
	if len(q) > 0 {
		base += "?" + q.Encode()
	}
	return base
}

// gexValidateStoredURL re-validates a stored pref value (defense in depth:
// the DB row could predate a format change). ok=false means "ignore it".
func gexValidateStoredURL(page, stored string) (string, bool) {
	u, err := url.Parse(stored)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "", false
	}
	base := "/gex"
	if page == gexPrefExposure {
		base = "/gex/exposure"
	}
	market := strings.TrimPrefix(u.Path, base+"/")
	if market != "ES" && market != "NQ" {
		return "", false
	}
	vals := u.Query()
	for k := range vals {
		if k != "expiry" && k != "bucket" && k != "view" {
			return "", false
		}
	}
	exps, ok := gexParseExpiries(vals.Get("expiry"))
	if !ok {
		return "", false
	}
	bucket, ok := gexParseBucket(vals.Get("bucket"))
	if !ok {
		return "", false
	}
	view, ok := gexParseView(vals.Get("view"))
	if !ok {
		return "", false
	}
	return gexCanonicalViewURL(page, market, exps, bucket, view), true
}

// gexStickyRedirect sends a bare entry request (no market segment, no
// query) to the user's stored view. Returns true when it redirected.
func (h *Handlers) gexStickyRedirect(w http.ResponseWriter, r *http.Request, page string) bool {
	if r.PathValue("market") != "" || r.URL.RawQuery != "" {
		return false
	}
	stored, err := h.Queries.GetViewPref(r.Context(), models.GetViewPrefParams{
		UserID: userID(r.Context()),
		Page:   page,
	})
	if err != nil || stored == "" {
		return false
	}
	target, ok := gexValidateStoredURL(page, stored)
	if !ok || target == r.URL.Path {
		return false
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
	return true
}

// GEXViewStateSave is POST /gex/viewstate: the pages report their current
// state (debounced, fire-and-forget) and it becomes the sticky view.
func (h *Handlers) GEXViewStateSave(w http.ResponseWriter, r *http.Request) {
	if h.GEX == nil {
		http.NotFound(w, r)
		return
	}
	page := r.FormValue("page")
	if page != gexPrefOverview && page != gexPrefExposure {
		http.Error(w, "bad page", http.StatusBadRequest)
		return
	}
	market := r.FormValue("market")
	if market != "ES" && market != "NQ" {
		http.Error(w, "bad market", http.StatusBadRequest)
		return
	}
	exps, ok := gexParseExpiries(r.FormValue("expiry"))
	if !ok {
		http.Error(w, "bad expiry", http.StatusBadRequest)
		return
	}
	bucket, ok := gexParseBucket(r.FormValue("bucket"))
	if !ok {
		http.Error(w, "bad bucket", http.StatusBadRequest)
		return
	}
	view, ok := gexParseView(r.FormValue("view"))
	if !ok {
		http.Error(w, "bad view", http.StatusBadRequest)
		return
	}
	err := h.Queries.UpsertViewPref(r.Context(), models.UpsertViewPrefParams{
		UserID:    userID(r.Context()),
		Page:      page,
		Value:     gexCanonicalViewURL(page, market, exps, bucket, view),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		h.logger().Warn("gex viewstate save failed", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
