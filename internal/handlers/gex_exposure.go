package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
)

// The Market > Gamma (GEX) > Exposure tab: a MenthorQ-style per-strike net
// GEX ladder - horizontal bars on a vertical price axis, call/put GEX split
// columns, an expiry strip, and the structural levels overlaid. LIVE-LATEST
// ONLY in v1: the engine's /v1/profile ignores ?at= (documented in the
// engine-ask doc), so this tab has no scrub and simply self-reloads.
//
// Denomination follows the rest of /gex: futures-first when the basis is
// warm. /v1/profile carries no conversion, so the SNAPSHOT is fetched
// alongside it as the denomination + levels source; when the snapshot is
// unavailable the tab degrades to index strikes with no level lines rather
// than dying.

// gexExpoProfile is one selectable per-strike profile in the page blob.
type gexExpoProfile struct {
	Strikes []float64 `json:"strikes"`
	Net     []float64 `json:"net"`
	Call    []float64 `json:"call"`
	Put     []float64 `json:"put"`
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
}

// GEXExposurePage renders /gex/exposure and /gex/exposure/{market}.
func (h *Handlers) GEXExposurePage(w http.ResponseWriter, r *http.Request) {
	if h.GEX == nil {
		http.NotFound(w, r)
		return
	}
	market, ok := gexMarketParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	data := map[string]any{
		"NavActive": "gex",
		"BackHref":  validBackHref(r),
		"Market":    market,
		"GexTab":    "exposure",
	}

	pd, err := h.GEX.ProfileDetail(ctx, market)
	switch {
	case errors.Is(err, gex.ErrNoProfile):
		data["NoProfile"] = true
	case err != nil:
		h.logger().Warn("gex exposure: profile fetch failed", "market", market, "err", err)
		data["EngineError"] = err.Error()
	default:
		// The snapshot supplies denomination + levels; its failure only
		// degrades the view (index strikes, no level lines).
		s, serr := h.GEX.Snapshot(ctx, market, time.Now())
		if serr != nil {
			h.logger().Warn("gex exposure: snapshot fetch failed", "market", market, "err", serr)
			s = nil
			data["LevelsNote"] = "levels unavailable — snapshot fetch failed; showing index strikes"
		}
		expo := gexBuildExpo(pd, s, time.Now(), h.localTZ())
		data["Expo"] = expo
		if b, err := json.Marshal(expo); err == nil {
			data["ExpoJSON"] = template.JS(b)
		}
	}
	h.renderPage(w, r, "gex_exposure.html", data)
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

	// Denomination: identical futures-first rule as the overview charts.
	ratio := 1.0
	var spotFut *float64
	if s != nil {
		spotFut = gexSpotFutures(s)
		if spotFut != nil && s.FuturesConversion != nil && s.FuturesConversion.Ratio > 0 {
			e.Denom = "futures"
			e.Spot = *spotFut
			ratio = s.FuturesConversion.Ratio
			e.BucketWidth = pd.BucketWidth * ratio
		}
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
		e.Profiles[key] = gexExpoProfile{
			Strikes: scale(ps.StrikesIndex),
			Net:     ps.NetGex,
			Call:    ps.CallGex,
			Put:     ps.PutGex,
		}
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
		add("Flip 0DTE", lad.Flip0, "flip0", "0dte", "")
		add("CW 0DTE", lad.CW0, "cw", "0dte", lad.CW0Gex)
		add("PW 0DTE", lad.PW0, "pw", "0dte", lad.PW0Gex)
		add("GW 0DTE", lad.GW0, "gw", "0dte", lad.GW0Gex)
		add("Flip 0DTE", lad.Flip0, "flip0", "0dte_vol", "")
		add("CW vol", lad.CWV, "cw", "0dte_vol", lad.CWVGex)
		add("PW vol", lad.PWV, "pw", "0dte_vol", lad.PWVGex)
		add("GW vol", lad.GWV, "gw", "0dte_vol", lad.GWVGex)
	}

	// Expiry strip: informational chips from expiry_decay, sorted by date.
	// Pct is the share of total ABSOLUTE decay - matching the reference
	// UI's per-expiry weight readout.
	dates := make([]string, 0, len(pd.ExpiryDecay))
	var absSum float64
	for d, v := range pd.ExpiryDecay {
		dates = append(dates, d)
		absSum += math.Abs(v)
	}
	sort.Strings(dates)
	todayMidnight := time.Date(now.In(tz).Year(), now.In(tz).Month(), now.In(tz).Day(), 0, 0, 0, 0, tz)
	for _, d := range dates {
		v := pd.ExpiryDecay[d]
		t, err := time.ParseInLocation("2006-01-02", d, tz)
		if err != nil {
			continue
		}
		pct := "0%"
		if absSum > 0 {
			pct = fmt.Sprintf("%.0f%%", math.Abs(v)/absSum*100)
		}
		e.Expiries = append(e.Expiries, gexExpoExpiry{
			Date:  d,
			Label: t.Format("Jan 2"),
			DTE:   int(math.Round(t.Sub(todayMidnight).Hours() / 24)),
			Net:   gexFmtB(v),
			Pct:   pct,
			Neg:   v < 0,
		})
	}
	return e
}
