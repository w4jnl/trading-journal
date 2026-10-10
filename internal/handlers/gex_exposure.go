package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/gexview"
)

// The Market > Gamma (GEX) > Exposure tab: a MenthorQ-style per-strike net
// GEX ladder - horizontal bars on a vertical price axis, call/put split
// columns (OI when the engine serves it, GEX split otherwise), an expiry
// strip, and the structural levels overlaid.
//
// Engine v0.7.0 (journal-update-005) made the expiry chips REAL filters:
// ?expiry= returns the blend/index/etf slice for that date, echoed in
// expiry_filter. Chip clicks fetch /gex/exposure/data/{market} and re-render
// in place; the selection round-trips through the page URL so reloads and
// deep links restore it. Against an older engine (which silently ignores
// the param) the chips stay informational - the expiry_filter key is the
// capability probe.
//
// Denomination follows the rest of /gex: futures-first when the basis is
// warm, preferring the profile payload's own futures fields (v0.7.0+) and
// falling back to the snapshot's. The snapshot is still fetched on page
// load for level lines + regime; when it is unavailable the tab degrades
// to no level lines rather than dying.

// gexParseExpiries validates a comma-separated set of ISO dates (the
// multi-select expiry filter). Returns the deduped set in input order;
// ok=false when any member is malformed (callers must not degrade a bad
// filter to the whole book). Shared by the page/data params and the
// viewstate endpoint.
func gexParseExpiries(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", p); err != nil {
			return nil, false
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, true
}

func gexExpoExpiriesParam(r *http.Request) ([]string, bool) {
	return gexParseExpiries(r.URL.Query().Get("expiry"))
}

// gexParseBucket validates a bucket width (index points; 0 = stored
// default). The engine owns the real constraints (multiple of stored width,
// <= 100); this only rejects non-numeric junk.
func gexParseBucket(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > 1000 {
		return 0, false
	}
	return n, true
}

func gexExpoBucketParam(r *http.Request) (int, bool) {
	return gexParseBucket(r.URL.Query().Get("bucket"))
}

// gexParseView validates a profile-view key ("" = the default ALL view).
func gexParseView(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "", "all":
		return "", true
	case "0dte":
		return "0dte", true
	case "0dte_vol":
		return "0dte_vol", true
	}
	return "", false
}

// GEXExposurePage renders /gex/exposure and /gex/exposure/{market}. It
// always fetches the UNFILTERED book (the chip strip and profile buttons
// need it); a valid ?expiry= is handed to the client, which re-runs the
// chip-select fetch on init so deep links and self-reloads restore it.
func (h *Handlers) GEXExposurePage(w http.ResponseWriter, r *http.Request) {
	if h.GEX == nil {
		http.NotFound(w, r)
		return
	}
	if h.gexStickyRedirect(w, r, gexPrefExposure) {
		return
	}
	market, ok := gexMarketParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	tz := h.localTZ()
	now := time.Now()
	// Lenient like GEXPage: malformed ?at= renders live.
	var at time.Time
	atParam := ""
	if t, p, ok := gexParseAtParam(r.URL.Query().Get("at"), tz); ok && p != "" {
		at, atParam = t, p
	}
	// ref is the moment being VIEWED - chip DTE labels, the Today used for
	// the 0DTE level family, and Stale are all relative to it, so a
	// historical render reads as that day did, not as today does.
	ref := at
	if ref.IsZero() {
		ref = now
	}
	data := map[string]any{
		"NavActive": "gex",
		"BackHref":  validBackHref(r),
		"Market":    market,
		"GexTab":    "exposure",
		"AtParam":   atParam,
	}
	page := gexview.ExpoPageData{Market: market}
	if grid, pos, ok := gexview.Grid(now, ref); ok {
		page.Grid, page.GridPos = grid, pos
		data["HasGrid"] = true // the scrub bar markup
	}
	// Market-switch links carry the viewed moment + expiry filter along;
	// the client keeps the same params in sync via history.replaceState.
	qs := url.Values{}
	if atParam != "" {
		qs.Set("at", atParam)
	}
	if exps, ok := gexExpoExpiriesParam(r); ok && len(exps) > 0 {
		qs.Set("expiry", strings.Join(exps, ","))
	}
	if b, ok := gexExpoBucketParam(r); ok && b > 0 {
		qs.Set("bucket", strconv.Itoa(b))
	}
	if v, ok := gexParseView(r.URL.Query().Get("view")); ok && v != "" {
		qs.Set("view", v)
	}
	if len(qs) > 0 {
		data["QuerySuffix"] = "?" + qs.Encode()
	}

	pd, err := h.GEX.ProfileDetail(ctx, market, gex.ProfileQuery{At: at})
	switch {
	case errors.Is(err, gex.ErrNoProfile):
		data["NoProfile"] = true
	case errors.Is(err, gex.ErrNoSnapshot):
		data["EngineError"] = "no snapshot at or before the requested moment"
	case err != nil:
		h.logger().Warn("gex exposure: profile fetch failed", "market", market, "err", err)
		data["EngineError"] = err.Error()
	default:
		// The snapshot supplies levels + regime (and legacy denomination);
		// its failure only degrades the view, never kills it.
		snapAt := at
		if snapAt.IsZero() {
			snapAt = time.Now()
		}
		s, serr := h.GEX.Snapshot(ctx, market, snapAt)
		var levelsNote string
		if serr != nil {
			h.logger().Warn("gex exposure: snapshot fetch failed", "market", market, "err", serr)
			s = nil
			if pd.SpotFutures == nil {
				levelsNote = "levels unavailable — snapshot fetch failed; showing index strikes"
			} else {
				levelsNote = "levels unavailable — snapshot fetch failed"
			}
		}
		expo := gexview.BuildExpo(pd, s, ref, tz)
		// On the struct (not a template data key) so the inline blob carries
		// it too - the client's chrome sync re-renders it per scrub slot.
		expo.LevelsNote = levelsNote
		data["Expo"] = expo
		if b, err := json.Marshal(expo); err == nil {
			page.Expo = b
		}
	}
	data["PageJSON"] = page.IslandJSON()
	h.renderPage(w, r, "gex_exposure.html", data)
}

// GEXExposureData serves the chip-select fetches AND the scrub slots.
//
// Live filter fetches (no ?at=) keep today's contract: the gexview.Expo blob for
// one expiry slice (or the whole book), JSON, no snapshot call - the client
// keeps the page-load levels/regime and only swaps profiles + denomination.
//
// With ?at= (a scrub slot or historical deep link) the response is a full
// moment: the snapshot is fetched too (levels + regime, degrade-don't-die
// like the page), chip stats reflect that moment even under a filter, and
// with fallback=1 the narrowing params the engine cannot serve for that
// moment are dropped server-side (Degraded names what) instead of 4xxing -
// a scrub must always paint something.
func (h *Handlers) GEXExposureData(w http.ResponseWriter, r *http.Request) {
	if h.GEX == nil {
		http.NotFound(w, r)
		return
	}
	market, ok := gexMarketParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	tz := h.localTZ()

	w.Header().Set("Content-Type", "application/json")
	writeErr := func(status int, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	// Malformed params must NOT silently degrade - the exact silent-param
	// hazard ask-001 had the engine fix. This endpoint is programmatic, so
	// unlike the page handlers a bad ?at= is a 400, not a silent live view.
	at, _, ok := gexParseAtParam(r.URL.Query().Get("at"), tz)
	if !ok {
		writeErr(http.StatusBadRequest, "invalid at; expected unix seconds or YYYY-MM-DDTHH:MM")
		return
	}
	q := gex.ProfileQuery{At: at}
	exps, ok := gexExpoExpiriesParam(r)
	if !ok {
		writeErr(http.StatusBadRequest, "invalid expiry date; expected YYYY-MM-DD (comma-separated for a set)")
		return
	}
	q.Expiries = exps
	bucket, ok := gexExpoBucketParam(r)
	if !ok {
		writeErr(http.StatusBadRequest, "invalid bucket; expected a positive integer (index points)")
		return
	}
	q.Bucket = bucket

	pd, err := h.GEX.ProfileDetail(r.Context(), market, q)
	var degraded []string
	if r.URL.Query().Get("fallback") == "1" {
		// Scrub slots retry without the narrowing param the engine cannot
		// serve for this moment (a pre-v0.8.0 grid, a pre-v0.7.0 snapshot)
		// rather than 4xx. Each branch clears the param it retried without,
		// so the loop runs at most twice; the response names what was
		// dropped so the client can say so.
		for err != nil {
			if errors.Is(err, gex.ErrBadBucket) && q.Bucket > 0 {
				q.Bucket = 0
				degraded = append(degraded, "bucket")
			} else if (errors.Is(err, gex.ErrNoExpiryData) || errors.Is(err, gex.ErrExpiryUnsupported)) && len(q.Expiries) > 0 {
				q.Expiries = nil
				degraded = append(degraded, "expiry")
			} else {
				break
			}
			pd, err = h.GEX.ProfileDetail(r.Context(), market, q)
		}
	}
	switch {
	case errors.Is(err, gex.ErrNoExpiryData):
		writeErr(http.StatusNotFound, "per-expiry breakdown unavailable for this moment (snapshot predates engine v0.7.0 or is past retention)")
		return
	case errors.Is(err, gex.ErrNoSnapshot):
		writeErr(http.StatusNotFound, "no snapshot at or before the requested moment")
		return
	case errors.Is(err, gex.ErrExpiryUnsupported):
		writeErr(http.StatusUnprocessableEntity, "the engine predates v0.7.0 — expiry filtering needs an engine upgrade/restart")
		return
	case errors.Is(err, gex.ErrBadBucket):
		// Carries the engine's own detail (not a multiple / over 100 /
		// finer than this snapshot's stored grid / engine too old).
		writeErr(http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		h.logger().Warn("gex exposure data: profile fetch failed", "market", market, "err", err)
		writeErr(http.StatusBadGateway, "engine fetch failed")
		return
	}

	ref := time.Now()
	var s *gex.Snapshot
	var levelsNote string
	if !at.IsZero() {
		ref = at
		// A moment-addressed response repaints the client's levels + regime
		// chrome, so it needs the snapshot of THAT moment. Failure only
		// degrades to no level lines, mirroring the page handler.
		var serr error
		s, serr = h.GEX.Snapshot(r.Context(), market, at)
		if serr != nil {
			h.logger().Warn("gex exposure data: snapshot fetch failed", "market", market, "err", serr)
			s = nil
			levelsNote = "levels unavailable — snapshot fetch failed"
		}
	}
	expo := gexview.BuildExpo(pd, s, ref, tz)
	expo.LevelsNote = levelsNote
	expo.Degraded = strings.Join(degraded, ",")
	if !at.IsZero() && len(q.Expiries) > 0 {
		// Filtered responses carry no expiry_decay (engine contract), but a
		// scrubbed moment must still show that moment's chip stats: fetch
		// the unfiltered book for the strip. Best effort - on failure the
		// strip goes empty and the client's ghost chips cover the selection.
		if upd, uerr := h.GEX.ProfileDetail(r.Context(), market, gex.ProfileQuery{At: at}); uerr == nil {
			expo.Expiries = gexview.BuildExpiryChips(upd.ExpiryDecay, ref, tz)
		}
	}
	_ = json.NewEncoder(w).Encode(expo)
}
