package handlers

import (
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/models"
)

// syncBannerMaxPolls bounds the banner's client-side polling: ~10 polls at
// 3s = ~30s. Beyond that the poller stops unless a sync is still pending
// (then it keeps going up to the hard cap) - covers a slow gateway without
// polling forever on an idle page.
const (
	syncBannerMaxPolls = 10
	syncBannerHardCap  = 40
)

// syncBannerData is the render model for _sync_banner.html.
type syncBannerData struct {
	State string // "poll" | "banner" | "done"
	Base  int64  // sync_log id baseline captured at page load
	N     int    // poll counter
	Fills int64  // new fills since baseline (for the banner text)
}

// SyncBanner (GET /sync/banner) drives the live-sync refresh banner. On the
// first call it captures a baseline (newest sync_log id) and starts polling;
// once a live sync ingests fills past that baseline it returns a "N new
// fills - Refresh" prompt; if nothing arrives within the poll window (and no
// sync is pending) it returns an inert div that stops polling.
func (h *Handlers) SyncBanner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)

	base, err := strconv.ParseInt(r.URL.Query().Get("base"), 10, 64)
	if err != nil {
		// First call: baseline = newest sync_log id right now.
		uidPtr := uid
		base, _ = h.Queries.MaxSyncLogIDForUser(ctx, &uidPtr)
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))

	uidPtr := uid
	fills, err := h.Queries.SumFillsSyncedSinceID(ctx, models.SumFillsSyncedSinceIDParams{
		UserID: &uidPtr, ID: base,
	})
	if err != nil {
		h.logger().Warn("sync banner: sum fills", "err", err)
		h.renderPartial(w, "_sync_banner.html", "sync_banner", syncBannerData{State: "done"})
		return
	}

	switch {
	case fills > 0:
		h.renderPartial(w, "_sync_banner.html", "sync_banner", syncBannerData{State: "banner", Fills: fills})
	case h.syncPending(uid) && n < syncBannerHardCap, n < syncBannerMaxPolls:
		h.renderPartial(w, "_sync_banner.html", "sync_banner", syncBannerData{State: "poll", Base: base, N: n + 1})
	default:
		h.renderPartial(w, "_sync_banner.html", "sync_banner", syncBannerData{State: "done"})
	}
}

// syncPending reports whether either live-sync service has a sync queued or
// running for the user (both receivers are nil-safe when a feature is off).
func (h *Handlers) syncPending(userID int64) bool {
	return h.TopstepxSync.PendingForUser(userID) || h.IbkrSync.PendingForUser(userID)
}
