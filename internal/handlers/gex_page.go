package handlers

import (
	"context"
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

// The Market > Gamma (GEX) page: current (or as-of a chosen moment) gamma
// state for ES/NQ from the gex-engine - regime, flip, walls, expected move,
// and the per-strike net-GEX profile chart. This is the same data the review
// annotations condense; the page is the browsable view of it. Feature-gated
// on Handlers.GEX like every optional service: nil hides the nav item
// (gexEnabled func) and 404s the routes.
//
// Two ways to move through time: a scrub slider over the recent session grid
// (fed by the GEXData JSON endpoint so dragging updates tiles + chart in
// place, no reloads) and the "as of" picker for anything older.

// gexPageMoment is the "as of" input format (a datetime-local control,
// interpreted in the journal's trading timezone).
const gexPageMoment = "2006-01-02T15:04"

// gexParseAtParam parses the shared ?at= moment param in either wire form:
// unix seconds (what the scrub JS writes - client code cannot format a
// trading-TZ datetime-local string) or the picker's datetime-local format in
// the trading TZ. Unix values are used as-is (the scrub already bakes its
// +120s slot buffer into what it sends); picker values get +59s because the
// control has minute resolution, so "14:55" denotes that whole minute -
// query its END, or at-or-before would resolve onto the PREVIOUS slot
// (snapshots are stamped 1-3s after their grid minute). Zero time = live;
// ok=false only on malformed non-empty input.
func gexParseAtParam(raw string, tz *time.Location) (at time.Time, canonical string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, "", true
	}
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if sec <= 0 {
			return time.Time{}, "", false
		}
		return time.Unix(sec, 0), raw, true
	}
	t, err := time.ParseInLocation(gexPageMoment, raw, tz)
	if err != nil {
		return time.Time{}, "", false
	}
	return t.Add(59 * time.Second), raw, true
}

// gexFetchProfiles fetches the profile (and its ~1h-earlier sibling for
// the DTE tile's delta column), returning the DTE rows AND the unfiltered
// profile itself - the same fetch feeds the chart's profile slices and the
// expiry chip strip for free. Best effort: any error just shrinks the
// result (nil = empty states) - the page must never fail because
// /v1/profile did.
func (h *Handlers) gexFetchProfiles(ctx context.Context, market string, at, ref time.Time, tz *time.Location) ([]gexview.DTERow, *gex.ProfileDetail) {
	q := gex.ProfileQuery{}
	if !at.IsZero() {
		q.At = at
	}
	pd, err := h.GEX.ProfileDetail(ctx, market, q)
	if err != nil {
		return nil, nil
	}
	prev, err := h.GEX.ProfileDetail(ctx, market, gex.ProfileQuery{At: ref.Add(-time.Hour)})
	if err != nil {
		prev = nil
	}
	return gexview.BuildDTE(pd, prev, ref, tz), pd
}

// gexAttachProfileSurface fills a view's /v1/profile-sourced surface: the
// DTE tile, the expiry chip strip (always the UNFILTERED book's stats, DTE
// anchored at ref), the capability flag, and the selectable chart profile
// slices - narrowed to exps when a filter is on. The filter affects ONLY
// ChartProfiles: tiles, ladder and levels are whole-book snapshot
// artifacts the engine cannot filter. Best effort throughout; a filter the
// engine cannot serve for this moment empty-states the chart alone via
// ChartUnavailable.
func (h *Handlers) gexAttachProfileSurface(ctx context.Context, v *gexview.View, market string, at, ref time.Time, exps []string, tz *time.Location) {
	dte, pd := h.gexFetchProfiles(ctx, market, at, ref, tz)
	v.DTE = dte
	if pd == nil {
		return
	}
	v.SupportsExpiry = pd.SupportsExpiry
	v.Expiries = gexview.BuildExpiryChips(pd.ExpiryDecay, ref, tz)
	if len(exps) == 0 || !pd.SupportsExpiry {
		// A filter against a pre-v0.7.0 engine is not silently dropped
		// data: the chips render locked there, so nothing offered it.
		v.ChartProfiles = gexview.BuildChartProfiles(pd, v.Chart)
		return
	}
	q := gex.ProfileQuery{Expiries: exps}
	if !at.IsZero() {
		q.At = at
	}
	fpd, err := h.GEX.ProfileDetail(ctx, market, q)
	switch {
	case err == nil:
		v.ChartProfiles = gexview.BuildChartProfiles(fpd, v.Chart)
	case errors.Is(err, gex.ErrNoExpiryData):
		v.ChartUnavailable = "per-expiry breakdown unavailable for this moment (snapshot predates engine v0.7.0 or is past retention)"
	case errors.Is(err, gex.ErrExpiryUnsupported):
		v.ChartUnavailable = "the engine predates v0.7.0 — expiry filtering needs an engine upgrade/restart"
	default:
		h.logger().Warn("gex page: filtered profile fetch failed", "market", market, "err", err)
		v.ChartUnavailable = "filtered profile fetch failed"
	}
}

// gexMarketParam resolves the {market} path value ("" defaults to ES).
func gexMarketParam(r *http.Request) (string, bool) {
	market := strings.ToUpper(strings.TrimSpace(r.PathValue("market")))
	if market == "" {
		market = "ES"
	}
	return market, market == "ES" || market == "NQ"
}

// GEXPage renders /gex and /gex/{market}. Engine trouble never 5xxes the
// page: a missing snapshot renders an empty state, transport errors render a
// flash, and the shell (market toggle, slider, as-of picker) stays usable.
func (h *Handlers) GEXPage(w http.ResponseWriter, r *http.Request) {
	if h.GEX == nil {
		http.NotFound(w, r)
		return
	}
	if h.gexStickyRedirect(w, r, gexPrefOverview) {
		return
	}
	market, ok := gexMarketParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	tz := h.localTZ()
	now := time.Now()
	at := now
	latest := true
	atParam := ""
	// Page handlers stay lenient: a malformed ?at= renders live (URLs are
	// human-shareable); the data endpoints 400 instead.
	if t, p, ok := gexParseAtParam(r.URL.Query().Get("at"), tz); ok && p != "" {
		at, latest, atParam = t, false, p
	}

	// Filter params (lenient like at: malformed values are dropped, the
	// page renders unfiltered). The client validates its own restore too.
	exps, ok := gexExpoExpiriesParam(r)
	if !ok {
		exps = nil
	}
	viewKey, ok := gexParseView(r.URL.Query().Get("view"))
	if !ok {
		viewKey = ""
	}

	data := map[string]any{
		"NavActive": "gex",
		"BackHref":  validBackHref(r),
		"GexTab":    "overview",
		"Market":    market,
		"AtParam":   atParam,
		"AtValue":   at.In(tz).Format(gexPageMoment), // picker prefill
		"Latest":    latest,
	}
	// Market-switch links carry the moment + filter along, like the
	// Exposure tab (the client keeps them in sync via replaceState).
	qs := url.Values{}
	if atParam != "" {
		qs.Set("at", atParam)
	}
	if len(exps) > 0 {
		qs.Set("expiry", strings.Join(exps, ","))
	}
	if viewKey != "" && len(exps) == 0 {
		qs.Set("view", viewKey)
	}
	if len(qs) > 0 {
		data["QuerySuffix"] = "?" + qs.Encode()
	}

	page := gexview.PageData{Market: market, TZ: h.localTZ().String(), Latest: latest}
	if grid, pos, ok := gexview.Grid(now, at); ok {
		page.Grid, page.GridPos = grid, pos
		data["HasGrid"] = true // the scrub bar markup
	}

	s, err := h.GEX.Snapshot(r.Context(), market, at)
	switch {
	case errors.Is(err, gex.ErrNoSnapshot):
		data["Empty"] = true
		data["EmptyWhen"] = at.In(gexview.Eastern()).Format("Jan 2 2006 15:04 ET")
	case err != nil:
		h.logger().Warn("gex page: snapshot fetch failed", "market", market, "err", err)
		data["EngineError"] = err.Error()
		data["EngineNotes"] = h.gexHealthNotes(r.Context())
	default:
		ref := at
		if latest {
			ref = now
		}
		view := gexview.BuildView(s, ref)
		atOrZero := at
		if latest {
			atOrZero = time.Time{}
		}
		h.gexAttachProfileSurface(r.Context(), &view, market, atOrZero, ref, exps, h.localTZ())
		h.gexAttachAnalysis(r.Context(), &view, market, s.AsOf)
		data["View"] = view
		data["SessionClosed"] = latest && view.Stale
		if latest && view.Stale {
			// A stale live row is usually just the closed options session
			// - but during a feed outage it is the engine falling behind,
			// and its /health says so (v0.11.0 reasons). Ask, so the
			// banner does not call an outage "session closed".
			data["EngineNotes"] = h.gexHealthNotes(r.Context())
		}
		data["AIEnabled"] = h.AI != nil
		if b, err := json.Marshal(view); err == nil {
			page.View = b
		}
	}
	data["PageJSON"] = page.IslandJSON()
	h.renderPage(w, r, "gex.html", data)
}

// GEXData serves one snapshot's view model as JSON for the scrub slider.
// at is unix seconds (a grid slot plus the JS-side fetch buffer). A missing
// snapshot is a normal state ({"empty":true}), not an error; only transport
// trouble is a non-200.
func (h *Handlers) GEXData(w http.ResponseWriter, r *http.Request) {
	if h.GEX == nil {
		http.NotFound(w, r)
		return
	}
	market, ok := gexMarketParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sec, err := strconv.ParseInt(r.URL.Query().Get("at"), 10, 64)
	if err != nil || sec <= 0 {
		writeJSONError(w, http.StatusBadRequest, "at must be unix seconds")
		return
	}
	at := time.Unix(sec, 0)
	// Malformed expiry is a 400 like the Exposure data endpoint - never a
	// silent whole book presented under a filtered UI. A filter the engine
	// cannot serve for this MOMENT is different: a 200 whose chart alone is
	// empty (ChartUnavailable) - scrub slots must always paint.
	exps, ok := gexExpoExpiriesParam(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid expiry date; expected YYYY-MM-DD (comma-separated for a set)")
		return
	}

	s, err := h.GEX.Snapshot(r.Context(), market, at)
	switch {
	case errors.Is(err, gex.ErrNoSnapshot):
		writeJSON(w, http.StatusOK, map[string]any{"empty": true})
	case err != nil:
		h.logger().Warn("gex data: snapshot fetch failed", "market", market, "err", err)
		writeJSONError(w, http.StatusBadGateway, "gex-engine unreachable")
	default:
		v := gexview.BuildView(s, at)
		h.gexAttachProfileSurface(r.Context(), &v, market, at, at, exps, h.localTZ())
		h.gexAttachAnalysis(r.Context(), &v, market, s.AsOf)
		writeJSON(w, http.StatusOK, v)
	}
}
