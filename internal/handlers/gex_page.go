package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
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

// gexHistoryStart is the engine's first stored session (nothing exists at or
// before any earlier moment, per the P4 handoff).
var gexHistoryStart = time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

// gexGridSessions caps the scrub slider's window. 60 sessions x 75 slots is
// a ~50KB inline array; the "as of" picker covers anything older.
const gexGridSessions = 60

// gexPageLevel is one row of the levels table, pre-formatted.
type gexPageLevel struct {
	Label   string `json:"label"`
	Bucket  string `json:"bucket"`
	Index   string `json:"index"`
	Futures string `json:"futures"`
	Gex     string `json:"gex"`
	Dist    string `json:"dist"` // signed points from spot (futures terms)
}

// gexChart is the profile chart's data blob (index-denominated).
type gexChart struct {
	Strikes []float64 `json:"strikes"`
	Gex     []float64 `json:"gex"`
	Spot    float64   `json:"spot"`
	Flip    *float64  `json:"flip"`
	CW      *float64  `json:"cw"`
	PW      *float64  `json:"pw"`
}

// gexLadder is the numeric model behind the vertical regime-ladder panel:
// the price structure around spot, in ONE denomination so the panel can be
// read at a glance. Futures when the basis is warm (that is the price you
// fill at), index otherwise; Denom names which, and the panel labels it.
// Nil levels are simply not drawn.
type gexLadder struct {
	Denom  string   `json:"denom"`
	Spot   float64  `json:"spot"`
	Flip   *float64 `json:"flip"`
	CW     *float64 `json:"cw"`
	PW     *float64 `json:"pw"`
	EMHigh *float64 `json:"emHigh"`
	EMLow  *float64 `json:"emLow"`
	CWGex  string   `json:"cwGex"`
	PWGex  string   `json:"pwGex"`
	Regime string   `json:"regime"`
}

// gexView is the pre-formatted view model for one snapshot, shared verbatim
// by the HTML render and the GEXData JSON endpoint so the scrub JS can
// repaint the exact same fields the server rendered.
type gexView struct {
	AsOfET      string         `json:"asOfET"`
	Stale       bool           `json:"stale"` // as_of lags the viewed moment (prior-session close)
	Regime      string         `json:"regime"`
	RegimeClass string         `json:"regimeClass"`
	Pctl        string         `json:"pctl"`
	NetGex      string         `json:"netGex"`
	SpotIndex   string         `json:"spotIndex"`
	SpotFutures string         `json:"spotFutures"`
	ConvLine    string         `json:"convLine"`
	FlipFutures string         `json:"flipFutures"`
	FlipIndex   string         `json:"flipIndex"`
	FlipSub     string         `json:"flipSub"`
	EM          string         `json:"em"`
	Levels      []gexPageLevel `json:"levels"`
	Chart       gexChart       `json:"chart"`
	Ladder      gexLadder      `json:"ladder"`
	HasProfile  bool           `json:"hasProfile"`
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
	market, ok := gexMarketParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	tz := h.localTZ()
	now := time.Now()
	at := now
	latest := true
	atParam := strings.TrimSpace(r.URL.Query().Get("at"))
	if atParam != "" {
		if t, err := time.ParseInLocation(gexPageMoment, atParam, tz); err == nil {
			at, latest = t, false
		} else {
			atParam = ""
		}
	}

	data := map[string]any{
		"NavActive": "gex",
		"BackHref":  validBackHref(r),
		"Market":    market,
		"AtParam":   atParam,
		"AtValue":   at.In(tz).Format(gexPageMoment), // picker prefill
		"Latest":    latest,
	}

	// Scrub-slider grid: recent session slots, oldest first, plus the initial
	// position (the last slot at or before the viewed moment; -1 = older than
	// the window, slider then starts parked without a seeded view).
	grid := gexSessionGrid(now)
	if len(grid) > 1 {
		pos := -1
		for i, ts := range grid {
			if ts <= at.Unix() {
				pos = i
			}
		}
		if b, err := json.Marshal(grid); err == nil {
			data["GridJSON"] = template.JS(b)
			data["GridPos"] = pos
		}
	}

	s, err := h.GEX.Snapshot(r.Context(), market, at)
	switch {
	case errors.Is(err, gex.ErrNoSnapshot):
		data["Empty"] = true
		data["EmptyWhen"] = at.In(gexEastern()).Format("Jan 2 2006 15:04 ET")
	case err != nil:
		h.logger().Warn("gex page: snapshot fetch failed", "market", market, "err", err)
		data["EngineError"] = err.Error()
	default:
		ref := at
		if latest {
			ref = now
		}
		view := gexBuildView(s, ref)
		data["View"] = view
		data["SessionClosed"] = latest && view.Stale
		if b, err := json.Marshal(view); err == nil {
			data["ViewJSON"] = template.JS(b)
		}
	}
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

	s, err := h.GEX.Snapshot(r.Context(), market, at)
	switch {
	case errors.Is(err, gex.ErrNoSnapshot):
		writeJSON(w, http.StatusOK, map[string]any{"empty": true})
	case err != nil:
		h.logger().Warn("gex data: snapshot fetch failed", "market", market, "err", err)
		writeJSONError(w, http.StatusBadGateway, "gex-engine unreachable")
	default:
		writeJSON(w, http.StatusOK, gexBuildView(s, at))
	}
}

// gexSessionGrid returns the scrub slider's time slots: 09:50-16:00 ET in
// five-minute steps (the engine's snapshot grid) for the most recent
// gexGridSessions weekdays, ascending, bounded by the engine history start
// and now. Holidays are left in - their slots resolve to the prior session's
// close via at-or-before, which the stale flag surfaces honestly.
func gexSessionGrid(now time.Time) []int64 {
	et := gexEastern()
	// gexHistoryStart is a plain DATE; take its components rather than
	// converting the instant (which would slide it into the prior ET day).
	sy, sm, sd := gexHistoryStart.Date()
	startDay := time.Date(sy, sm, sd, 0, 0, 0, 0, et)

	n := now.In(et)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, et)
	var days []time.Time
	for len(days) < gexGridSessions && !day.Before(startDay) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days = append(days, day)
		}
		day = day.AddDate(0, 0, -1)
	}

	var out []int64
	for i := len(days) - 1; i >= 0; i-- {
		for m := 9*60 + 50; m <= 16*60; m += 5 {
			t := days[i].Add(time.Duration(m) * time.Minute)
			if t.After(now) {
				break
			}
			out = append(out, t.Unix())
		}
	}
	return out
}

// gexBuildView turns a snapshot into the pre-formatted view model. ref is
// the moment being viewed (now for the live view); Stale flags an as_of
// lagging it by more than the grid step allows - the prior-session-close
// case. Every engine-nullable value renders "-" instead of erroring
// (client.go documents which ones those are).
func gexBuildView(s *gex.Snapshot, ref time.Time) gexView {
	et := gexEastern()
	v := gexView{
		AsOfET: s.AsOf.In(et).Format("Mon Jan 2, 15:04:05 ET"),
		Stale:  ref.Sub(s.AsOf) > gexStaleAfter,
		Regime: s.Regime.State,
		Pctl:   "—",
		NetGex: gexFmtB(s.Regime.NetGexAtSpot),
		EM:     "—",
	}
	switch s.Regime.State {
	case "positive":
		v.RegimeClass = "pnl-pos"
	case "negative":
		v.RegimeClass = "pnl-neg"
	}
	if p := s.Regime.Percentile20D; p != nil {
		v.Pctl = strconv.FormatFloat(*p, 'f', 0, 64)
	}
	if e := s.ExpectedMove1D; e != nil {
		v.EM = fmt.Sprintf("%.1f pts", *e)
	}

	v.SpotIndex = fmt.Sprintf("%.2f", s.SpotIndex)
	v.SpotFutures = "—"
	spotFut := gexSpotFutures(s)
	if spotFut != nil {
		v.SpotFutures = fmt.Sprintf("%.2f", *spotFut)
	}
	if fc := s.FuturesConversion; fc != nil {
		v.ConvLine = fmt.Sprintf("futures conversion %.4f · %s · paired %s",
			fc.Ratio, fc.Source, fc.PairedAt.In(et).Format("15:04 ET"))
	} else {
		v.ConvLine = "futures conversion unavailable (cold basis) — showing index values only"
	}

	// The flip is published in BOTH denominations. The tile leads with
	// futures (journal fill prices are futures prices, so that is the number
	// you trade against) and carries the index value the profile chart plots
	// - one unlabeled flip number next to an index-denominated spot tile
	// reads as a unit bug even when the math is right.
	v.FlipFutures, v.FlipIndex = "—", "—"
	if i := s.Flip.Index; i != nil {
		v.FlipIndex = fmt.Sprintf("%.2f", *i)
	}
	if f := s.Flip.Futures; f != nil {
		v.FlipFutures = fmt.Sprintf("%.2f", *f)
		if spotFut != nil {
			d := *spotFut - *f
			side := "above"
			if d < 0 {
				side = "below"
			}
			v.FlipSub = fmt.Sprintf("spot %+.2f pts (%s flip)", d, side)
		}
	}

	v.Levels = make([]gexPageLevel, 0, len(s.Levels)+1)
	// The flip leads the ladder: it is a price level like the walls, and
	// listing it here is what pins its denomination - both columns present,
	// same level-minus-spot convention as every other row. (The engine
	// publishes no GEX for the flip; it is the zero crossing by definition.)
	if s.Flip.Index != nil || s.Flip.Futures != nil {
		row := gexPageLevel{Label: "Gamma flip", Bucket: "—", Index: "—", Futures: "—", Gex: "—", Dist: "—"}
		if i := s.Flip.Index; i != nil {
			row.Index = fmt.Sprintf("%.2f", *i)
		}
		if f := s.Flip.Futures; f != nil {
			row.Futures = fmt.Sprintf("%.2f", *f)
			if spotFut != nil {
				row.Dist = fmt.Sprintf("%+.2f pts", *f-*spotFut)
			}
		}
		v.Levels = append(v.Levels, row)
	}
	for _, l := range s.Levels {
		row := gexPageLevel{
			Label:   gexLevelName(l.Type),
			Bucket:  l.Bucket,
			Index:   fmt.Sprintf("%.2f", l.Index),
			Futures: "—",
			Gex:     gexFmtB(l.Gex),
			Dist:    "—",
		}
		if l.Futures != nil {
			row.Futures = fmt.Sprintf("%.2f", *l.Futures)
			if spotFut != nil {
				row.Dist = fmt.Sprintf("%+.2f pts", *l.Futures-*spotFut)
			}
		}
		v.Levels = append(v.Levels, row)
	}

	v.Chart = gexChart{
		Strikes: s.Profile.StrikesIndex,
		Gex:     s.Profile.NetGex,
		Spot:    s.SpotIndex,
		Flip:    s.Flip.Index,
		CW:      gexWallIndex(s, "call_wall"),
		PW:      gexWallIndex(s, "put_wall"),
	}
	v.Ladder = gexBuildLadder(s, spotFut)
	v.HasProfile = len(s.Profile.StrikesIndex) > 1 &&
		len(s.Profile.StrikesIndex) == len(s.Profile.NetGex)
	return v
}

// gexSpotFutures resolves spot in futures terms. The engine publishes it
// directly from v0.3.0 (contract: "consumers must never re-derive a futures
// price themselves - spot included"); the ratio fallback exists only so an
// older engine still renders, and disappears with it. Nil means the basis is
// cold or the market is index-only, in which case the page shows index values
// and says so rather than inventing a conversion.
func gexSpotFutures(s *gex.Snapshot) *float64 {
	if s.SpotFutures != nil {
		return s.SpotFutures
	}
	if fc := s.FuturesConversion; fc != nil {
		f := s.SpotIndex * fc.Ratio
		return &f
	}
	return nil
}

// gexBuildLadder assembles the regime-ladder panel's levels in a single
// denomination. spotFut is nil when the futures basis is cold, in which case
// the whole ladder falls back to index terms rather than dropping out.
//
// The expected-move band is spot +/- expected_move_1d. That figure is
// index-denominated at source (it comes from the SPX 0DTE ATM straddle), so
// the futures band scales it by the same conversion ratio the engine applies
// to its own levels.
func gexBuildLadder(s *gex.Snapshot, spotFut *float64) gexLadder {
	useFut := spotFut != nil
	lad := gexLadder{Denom: "index", Spot: s.SpotIndex, Regime: s.Regime.State}
	if useFut {
		lad.Denom, lad.Spot = "futures", *spotFut
	}

	pickWall := func(typ string) (*float64, string) {
		l := gexWall(s, typ)
		if l == nil {
			return nil, ""
		}
		if useFut {
			return l.Futures, gexFmtB(l.Gex)
		}
		idx := l.Index
		return &idx, gexFmtB(l.Gex)
	}
	lad.CW, lad.CWGex = pickWall("call_wall")
	lad.PW, lad.PWGex = pickWall("put_wall")

	if useFut {
		lad.Flip = s.Flip.Futures
	} else {
		lad.Flip = s.Flip.Index
	}

	if e := s.ExpectedMove1D; e != nil {
		// expected_move_1d is a DISTANCE in index points and is not
		// basis-converted at source (engine contract §4). Placing it around a
		// futures spot therefore scales the distance by the same ratio - the
		// engine's own ladder does this too (its EM rows carry both columns).
		d := *e
		if useFut && s.FuturesConversion != nil {
			d *= s.FuturesConversion.Ratio
		}
		hi, lo := lad.Spot+d, lad.Spot-d
		lad.EMHigh, lad.EMLow = &hi, &lo
	}
	return lad
}

// gexFmtB renders a raw gamma-dollar value at billions scale ("+74.5B").
func gexFmtB(v float64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1e9, abs == 0:
		return fmt.Sprintf("%+.1fB", v/1e9)
	case abs >= 1e6:
		return fmt.Sprintf("%+.0fM", v/1e6)
	default:
		return fmt.Sprintf("%+.0f", v)
	}
}

func gexLevelName(typ string) string {
	switch typ {
	case "call_wall":
		return "Call wall"
	case "put_wall":
		return "Put wall"
	default:
		return strings.ReplaceAll(typ, "_", " ")
	}
}

// gexWallIndex is the chart-side wall position (index-denominated; walls
// always carry an index value even when the futures conversion is cold).
func gexWallIndex(s *gex.Snapshot, typ string) *float64 {
	if l := gexWall(s, typ); l != nil {
		return &l.Index
	}
	return nil
}
