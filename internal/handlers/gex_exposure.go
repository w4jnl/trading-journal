package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
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

// gexExpoProfile is one selectable per-strike profile in the page blob.
// CallOI/PutOI are contract counts (engine v0.7.0+, else nil): when present
// the right-hand columns render OI and the GEX split moves to the tooltip.
type gexExpoProfile struct {
	Strikes []float64 `json:"strikes"`
	Net     []float64 `json:"net"`
	Call    []float64 `json:"call"`
	Put     []float64 `json:"put"`
	CallOI  []float64 `json:"callOi,omitempty"`
	PutOI   []float64 `json:"putOi,omitempty"`
}

// gexExpoLevel is one overlay line. Kind drives color/dash in JS; Family
// gates it behind the profile filter chips (a 0DTE wall only makes sense on
// the 0DTE view).
type gexExpoLevel struct {
	Label  string  `json:"label"`
	V      float64 `json:"v"`
	Kind   string  `json:"kind"`   // flip | flipI | flip0 | cw | pw | gw
	Family string  `json:"family"` // all | 0dte | 0dte_vol
	Gex    string  `json:"gex"`    // formatted; "" for flips
}

// gexExpoExpiry is one informational chip in the expiry strip.
type gexExpoExpiry struct {
	Date  string `json:"date"`
	Label string `json:"label"`
	DTE   int    `json:"dte"`
	Net   string `json:"net"`
	Pct   string `json:"pct"`
	Neg   bool   `json:"neg"`
}

// gexExpo is the page's inline JSON blob (plus template-rendered chrome).
// The /gex/exposure/data endpoint returns the same shape (minus levels and
// regime - the client keeps the page-load ones).
type gexExpo struct {
	Denom       string                    `json:"denom"`
	AsOfET      string                    `json:"asOfET"`
	OIDate      string                    `json:"oiDate"`
	Spot        float64                   `json:"spot"`
	BucketWidth float64                   `json:"bucketWidth"`
	Profiles    map[string]gexExpoProfile `json:"profiles"`
	Levels      []gexExpoLevel            `json:"levels"`
	Expiries    []gexExpoExpiry           `json:"expiries"`
	Regime      string                    `json:"regime"`
	NetTotal    string                    `json:"netTotal"`
	Stale       bool                      `json:"stale"`
	// SupportsExpiry gates the chip strip's live mode; Today is the
	// trading-TZ date whose filter shows the 0DTE level family.
	SupportsExpiry bool   `json:"supportsExpiry"`
	Today          string `json:"today"`
	// Ratio is the futures/index conversion behind a futures-denominated
	// blob (0 when Denom is "index"). The client divides by it for the
	// index-strike display mode - the exact inverse of the scaling here,
	// so index strikes come back as the engine published them. IndexName
	// labels that mode ("SPX"/"NDX", from the payload's sources).
	Ratio     float64 `json:"ratio"`
	IndexName string  `json:"indexName"`
	// BucketIndex is the served grid in raw INDEX points (BucketWidth is
	// futures-scaled); BucketBase/BucketOpts drive the width control: the
	// canonical 5-10-25-50 ladder, each width flagged servable or not
	// (finer than the stored grid, or not an integer multiple of it).
	// Unservable widths render disabled rather than hidden, so the
	// control reads the same every day - today's pre-v0.8.0 snapshots
	// gray out 5 and 25; tomorrow's 5-point grid enables everything.
	BucketIndex float64            `json:"bucketIndex"`
	BucketBase  int                `json:"bucketBase"`
	BucketOpts  []gexExpoBucketOpt `json:"bucketOpts"`
	// Data-endpoint extras (omitempty keeps live blobs byte-stable):
	// LevelsNote flags a degraded moment-addressed response (snapshot fetch
	// failed, so no level lines); Degraded names the narrowing params a
	// fallback=1 request dropped ("bucket", "expiry", or both).
	LevelsNote string `json:"levelsNote,omitempty"`
	Degraded   string `json:"degraded,omitempty"`
}

// gexExpoBucketOpt is one width in the bucket control.
type gexExpoBucketOpt struct {
	W  int  `json:"w"`
	Ok bool `json:"ok"`
}

// gexBucketOpts builds the canonical 5-10-25-50 width ladder for a stored
// base width: a width is servable when it IS the base or a coarser integer
// multiple of it (the engine's ?bucket= contract). A base outside the
// ladder (unexpected) is prepended so the native grid is always offered.
func gexBucketOpts(base int) []gexExpoBucketOpt {
	if base <= 0 {
		return nil
	}
	canon := []int{5, 10, 25, 50}
	opts := make([]gexExpoBucketOpt, 0, len(canon)+1)
	seen := false
	for _, w := range canon {
		if w == base {
			seen = true
		}
		opts = append(opts, gexExpoBucketOpt{W: w, Ok: w == base || (w > base && w%base == 0)})
	}
	if !seen {
		opts = append([]gexExpoBucketOpt{{W: base, Ok: true}}, opts...)
	}
	return opts
}

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
	if js, pos, ok := gexGrid(now, ref); ok {
		data["GridJSON"] = js
		data["GridPos"] = pos
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
		expo := gexBuildExpo(pd, s, ref, tz)
		// On the struct (not a template data key) so the inline blob carries
		// it too - the client's chrome sync re-renders it per scrub slot.
		expo.LevelsNote = levelsNote
		data["Expo"] = expo
		if b, err := json.Marshal(expo); err == nil {
			data["ExpoJSON"] = template.JS(b)
		}
	}
	h.renderPage(w, r, "gex_exposure.html", data)
}

// GEXExposureData serves the chip-select fetches AND the scrub slots.
//
// Live filter fetches (no ?at=) keep today's contract: the gexExpo blob for
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
	expo := gexBuildExpo(pd, s, ref, tz)
	expo.LevelsNote = levelsNote
	expo.Degraded = strings.Join(degraded, ",")
	if !at.IsZero() && len(q.Expiries) > 0 {
		// Filtered responses carry no expiry_decay (engine contract), but a
		// scrubbed moment must still show that moment's chip stats: fetch
		// the unfiltered book for the strip. Best effort - on failure the
		// strip goes empty and the client's ghost chips cover the selection.
		if upd, uerr := h.GEX.ProfileDetail(r.Context(), market, gex.ProfileQuery{At: at}); uerr == nil {
			expo.Expiries = gexBuildExpiryChips(upd.ExpiryDecay, ref, tz)
		}
	}
	_ = json.NewEncoder(w).Encode(expo)
}

// gexBuildExpo assembles the exposure blob. s may be nil (snapshot down):
// the result is then index-denominated with no levels.
func gexBuildExpo(pd *gex.ProfileDetail, s *gex.Snapshot, now time.Time, tz *time.Location) gexExpo {
	et := gexEastern()
	e := gexExpo{
		Denom:       "index",
		AsOfET:      pd.AsOf.In(et).Format("Mon Jan 2, 15:04:05 ET"),
		OIDate:      pd.OIDate,
		Spot:        pd.SpotIndex,
		BucketWidth: pd.BucketWidth,
		Profiles:    map[string]gexExpoProfile{},
		Levels:      []gexExpoLevel{},
		Expiries:    []gexExpoExpiry{},
		Stale:       now.Sub(pd.AsOf) > gexStaleAfter,
	}

	e.SupportsExpiry = pd.SupportsExpiry
	e.Today = now.In(tz).Format("2006-01-02")
	e.IndexName = "index"
	if len(pd.Sources) > 0 && pd.Sources[0] != "" {
		e.IndexName = pd.Sources[0]
	}
	e.BucketIndex = pd.BucketWidth
	e.BucketBase = int(math.Round(pd.BucketWidth))
	e.BucketOpts = gexBucketOpts(e.BucketBase)

	// Denomination: identical futures-first rule as the overview charts.
	// The profile payload's own futures fields (engine v0.7.0+) win, so the
	// snapshot-less data endpoint is futures-correct; the snapshot is the
	// fallback for older engines.
	ratio := 1.0
	var spotFut *float64
	if pd.SpotFutures != nil && pd.FuturesConversion != nil && pd.FuturesConversion.Ratio > 0 {
		e.Denom = "futures"
		e.Spot = *pd.SpotFutures
		ratio = pd.FuturesConversion.Ratio
		e.BucketWidth = pd.BucketWidth * ratio
	}
	if s != nil {
		spotFut = gexSpotFutures(s)
		if e.Denom == "index" && spotFut != nil && s.FuturesConversion != nil && s.FuturesConversion.Ratio > 0 {
			e.Denom = "futures"
			e.Spot = *spotFut
			ratio = s.FuturesConversion.Ratio
			e.BucketWidth = pd.BucketWidth * ratio
		}
	}
	if e.Denom == "futures" {
		e.Ratio = ratio
	}

	scale := func(src []float64) []float64 {
		if ratio == 1.0 {
			return src
		}
		out := make([]float64, len(src))
		for i, v := range src {
			out[i] = v * ratio
		}
		return out
	}
	addProfile := func(key, engineKey string) {
		ps, ok := pd.Profiles[engineKey]
		if !ok {
			return
		}
		n := len(ps.StrikesIndex)
		if n == 0 || len(ps.NetGex) != n || len(ps.CallGex) != n || len(ps.PutGex) != n {
			return // malformed profile: skip rather than render nonsense
		}
		p := gexExpoProfile{
			Strikes: scale(ps.StrikesIndex),
			Net:     ps.NetGex,
			Call:    ps.CallGex,
			Put:     ps.PutGex,
		}
		// OI ships only when both arrays are well-formed (null on
		// pre-v0.7.0 snapshots; the client then keeps the GEX split).
		if len(ps.CallOI) == n && len(ps.PutOI) == n {
			p.CallOI, p.PutOI = ps.CallOI, ps.PutOI
		}
		e.Profiles[key] = p
	}
	addProfile("all", "blend")
	addProfile("0dte", "0dte")
	addProfile("0dte_vol", "0dte_vol")

	if all, ok := e.Profiles["all"]; ok {
		var total float64
		for _, v := range all.Net {
			total += v
		}
		e.NetTotal = gexFmtB(total)
	}

	// Levels: flatten the ladder builder's output (published futures values,
	// same as everywhere else on /gex). Family gates each line behind the
	// profile chips; the 0DTE flip is DUPLICATED under 0dte_vol so the
	// client filter stays a dumb equality check.
	if s != nil {
		lad := gexBuildLadder(s, spotFut)
		e.Regime = lad.Regime
		add := func(label string, v *float64, kind, family, gexNote string) {
			if v != nil {
				e.Levels = append(e.Levels, gexExpoLevel{Label: label, V: *v, Kind: kind, Family: family, Gex: gexNote})
			}
		}
		add("Flip", lad.Flip, "flip", "all", "")
		add("Flip intra", lad.FlipI, "flipI", "all", "")
		add("CW", lad.CW, "cw", "all", lad.CWGex)
		add("PW", lad.PW, "pw", "all", lad.PWGex)
		// Session-open intraday band (engine v0.9.0, update 007) - the
		// band that lines up with MenthorQ's intra 1D Min/Max.
		add("1Di hi", lad.EMIHigh, "em", "all", "")
		add("1Di lo", lad.EMILow, "em", "all", "")
		add("Flip 0DTE", lad.Flip0, "flip0", "0dte", "")
		add("CW 0DTE", lad.CW0, "cw", "0dte", lad.CW0Gex)
		add("PW 0DTE", lad.PW0, "pw", "0dte", lad.PW0Gex)
		add("GW 0DTE", lad.GW0, "gw", "0dte", lad.GW0Gex)
		add("Flip 0DTE", lad.Flip0, "flip0", "0dte_vol", "")
		add("CW vol", lad.CWV, "cw", "0dte_vol", lad.CWVGex)
		add("PW vol", lad.PWV, "pw", "0dte_vol", lad.PWVGex)
		add("GW vol", lad.GWV, "gw", "0dte_vol", lad.GWVGex)
	}

	e.Expiries = gexBuildExpiryChips(pd.ExpiryDecay, now, tz)
	return e
}

// gexBuildExpiryChips builds the expiry strip from expiry_decay, sorted by
// date. Pct is the share of total ABSOLUTE decay - matching the reference
// UI's per-expiry weight readout. now anchors the DTE math (the viewed
// moment under ?at=, so historical strips read as that day did).
func gexBuildExpiryChips(decay map[string]float64, now time.Time, tz *time.Location) []gexExpoExpiry {
	dates := make([]string, 0, len(decay))
	var absSum float64
	for d, v := range decay {
		dates = append(dates, d)
		absSum += math.Abs(v)
	}
	sort.Strings(dates)
	todayMidnight := time.Date(now.In(tz).Year(), now.In(tz).Month(), now.In(tz).Day(), 0, 0, 0, 0, tz)
	out := make([]gexExpoExpiry, 0, len(dates))
	for _, d := range dates {
		v := decay[d]
		t, err := time.ParseInLocation("2006-01-02", d, tz)
		if err != nil {
			continue
		}
		pct := "0%"
		if absSum > 0 {
			pct = fmt.Sprintf("%.0f%%", math.Abs(v)/absSum*100)
		}
		out = append(out, gexExpoExpiry{
			Date:  d,
			Label: t.Format("Jan 2"),
			DTE:   int(math.Round(t.Sub(todayMidnight).Hours() / 24)),
			Net:   gexFmtB(v),
			Pct:   pct,
			Neg:   v < 0,
		})
	}
	return out
}
