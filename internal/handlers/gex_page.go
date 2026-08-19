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

// gexChart is the profile chart's data blob. Denominated like the ladder:
// futures when the basis is warm, index when cold - Denom names which, so
// the vertical and horizontal price axes always read as one.
type gexChart struct {
	Denom   string    `json:"denom"`
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
	// Engine v0.5.0 additions (update 002), all absent on older snapshots:
	// today's expiring book's walls and flip, the ranked gamma magnets, and
	// the prior session's close +/- its expected move (static all day).
	Flip0   *float64    `json:"flip0"`
	CW0     *float64    `json:"cw0"`
	PW0     *float64    `json:"pw0"`
	GW0     *float64    `json:"gw0"`
	CW0Gex  string      `json:"cw0Gex"`
	PW0Gex  string      `json:"pw0Gex"`
	GW0Gex  string      `json:"gw0Gex"`
	Magnets []gexMagnet `json:"magnets"`
	EM1High *float64    `json:"em1High"`
	EM1Low  *float64    `json:"em1Low"`
	// Engine v0.6.0 additions (update 003): the front-week (<=5 DTE) flip,
	// today's book re-weighted by session volume, and the near magnets
	// (ranked within +/-1% of spot).
	// IntraState mirrors the engine's regime.intraday_state verbatim for the
	// ladder's now-pill (the INTRA column state is published, never derived).
	IntraState string      `json:"intraState"`
	FlipI      *float64    `json:"flipI"`
	CWV        *float64    `json:"cwv"`
	PWV        *float64    `json:"pwv"`
	GWV        *float64    `json:"gwv"`
	CWVGex     string      `json:"cwvGex"`
	PWVGex     string      `json:"pwvGex"`
	GWVGex     string      `json:"gwvGex"`
	Near       []gexMagnet `json:"near"`
}

// gexMagnet is one ranked gamma magnet (gamma_1..gamma_3) in the ladder's
// denomination.
type gexMagnet struct {
	V   float64 `json:"v"`
	N   int     `json:"n"` // rank, 1 = largest
	Gex string  `json:"gex"`
}

// gexView is the pre-formatted view model for one snapshot, shared verbatim
// by the HTML render and the GEXData JSON endpoint so the scrub JS can
// repaint the exact same fields the server rendered.
type gexView struct {
	AsOfET      string `json:"asOfET"`
	Stale       bool   `json:"stale"` // as_of lags the viewed moment (prior-session close)
	Regime      string `json:"regime"`
	RegimeClass string `json:"regimeClass"`
	// RegimeIntra carries the front-week reading ONLY when it disagrees with
	// the whole-book state ("intraday negative") - agreement renders nothing,
	// so the tile stays quiet in the common case (engine update 004).
	RegimeIntra      string         `json:"regimeIntra"`
	RegimeIntraClass string         `json:"regimeIntraClass"`
	Pctl             string         `json:"pctl"`
	NetGex           string         `json:"netGex"`
	NetGexSub        string         `json:"netGexSub"` // 1h/1d change line; "" pre-v0.5.0
	SpotIndex        string         `json:"spotIndex"`
	SpotFutures      string         `json:"spotFutures"`
	ConvLine         string         `json:"convLine"`
	FlipFutures      string         `json:"flipFutures"`
	FlipIndex        string         `json:"flipIndex"`
	FlipSub          string         `json:"flipSub"`
	EM               string         `json:"em"`
	Levels           []gexPageLevel `json:"levels"`
	Chart            gexChart       `json:"chart"`
	Ladder           gexLadder      `json:"ladder"`
	HasProfile       bool           `json:"hasProfile"`
	// AsOfParam identifies the snapshot for the AI-read endpoint, as
	// RFC3339Nano. It must keep SUB-SECOND precision: as_of lands a fraction
	// of a second past its grid mark (19:00:02.501055Z), so a value truncated
	// to whole seconds is EARLIER than the snapshot and at-or-before resolves
	// it to the previous one - which pinned reads to the wrong moment.
	//
	// The retained read is carried twice on purpose: Analysis is the markdown
	// the page template renders through the shared partial, AnalysisHTML the
	// sanitized HTML the scrub path swaps in (no markdown pass in JS). Only
	// the latter is serialized, so the JSON never ships both.
	AsOfParam    string        `json:"asOfParam"`
	Analysis     string        `json:"-"`
	AnalysisHTML template.HTML `json:"analysisHTML"`
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
			// The picker has minute resolution, so "14:55" denotes that whole
			// minute - query its END. Snapshots are stamped 1-3 s AFTER their
			// grid minute (engine contract), so asking at :00 would resolve
			// at-or-before to the PREVIOUS slot, and the picker would
			// disagree with the scrub slider sitting on the same minute.
			at, latest = t.Add(59*time.Second), false
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
		h.gexAttachAnalysis(r.Context(), &view, market, s.AsOf)
		data["View"] = view
		data["SessionClosed"] = latest && view.Stale
		data["AIEnabled"] = h.AI != nil
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
		v := gexBuildView(s, at)
		h.gexAttachAnalysis(r.Context(), &v, market, s.AsOf)
		writeJSON(w, http.StatusOK, v)
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
		NetGex: "—",
		EM:     "—",
	}
	switch s.Regime.State {
	case "positive":
		v.RegimeClass = "pnl-pos"
	case "negative":
		v.RegimeClass = "pnl-neg"
	}
	if is := s.Regime.IntradayState; is != "" && is != s.Regime.State {
		v.RegimeIntra = "intraday " + is
		switch is {
		case "positive":
			v.RegimeIntraClass = "pnl-pos"
		case "negative":
			v.RegimeIntraClass = "pnl-neg"
		}
	}
	if total, ok := s.Regime.Total(); ok {
		v.NetGex = gexFmtB(total)
	}
	// Serve-time history: how the book moved into this moment (under ?at=
	// these are relative to that historical moment, so scrubbing stays
	// reproducible). Absent on markets/engines that do not publish them.
	var chg []string
	if d := s.NetGexChange1H; d != nil {
		chg = append(chg, "1h "+gexFmtB(*d))
	}
	if d := s.NetGexChange1D; d != nil {
		chg = append(chg, "1d "+gexFmtB(*d))
	}
	v.NetGexSub = strings.Join(chg, " · ")
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

	v.Levels = make([]gexPageLevel, 0, len(s.Levels)+3)
	// The flips lead the ladder: each is a price level like the walls, and
	// listing it here is what pins its denomination - both columns present,
	// same level-minus-spot convention as every other row. (The engine
	// publishes no GEX for a flip; it is the zero crossing by definition.)
	flipRow := func(fl gex.Flip, bucket string) {
		if fl.Index == nil && fl.Futures == nil {
			return
		}
		row := gexPageLevel{Label: "Gamma flip", Bucket: bucket, Index: "—", Futures: "—", Gex: "—", Dist: "—"}
		if i := fl.Index; i != nil {
			row.Index = fmt.Sprintf("%.2f", *i)
		}
		if f := fl.Futures; f != nil {
			row.Futures = fmt.Sprintf("%.2f", *f)
			if spotFut != nil {
				row.Dist = fmt.Sprintf("%+.2f pts", *f-*spotFut)
			}
		}
		v.Levels = append(v.Levels, row)
	}
	// Whole book, then the front-week (<=5 DTE) book, then today only - the
	// same nesting order the buckets narrow in.
	flipRow(s.Flip, "—")
	flipRow(s.FlipIntraday, "intraday")
	flipRow(s.Flip0DTE, "0dte")
	for _, l := range s.Levels {
		row := gexPageLevel{
			Label:   gexLevelName(l.Type),
			Bucket:  l.Bucket,
			Index:   fmt.Sprintf("%.2f", l.Index),
			Futures: "—",
			Gex:     gexFmtB(l.Gex),
			Dist:    "—",
		}
		if strings.HasPrefix(l.Type, "em_1d_") {
			// The prior-day EM band carries gex 0.0 by contract - it is a
			// static price band, not a gamma concentration; "+0.0B" would
			// read as a measured zero.
			row.Gex = "—"
		}
		if l.Futures != nil {
			row.Futures = fmt.Sprintf("%.2f", *l.Futures)
			if spotFut != nil {
				row.Dist = fmt.Sprintf("%+.2f pts", *l.Futures-*spotFut)
			}
		}
		v.Levels = append(v.Levels, row)
	}

	v.Chart = gexBuildChart(s, spotFut)
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

	pick := func(typ, bucket string) (*float64, string) {
		l := gexLevelIn(s, typ, bucket)
		if l == nil {
			return nil, ""
		}
		if useFut {
			return l.Futures, gexFmtB(l.Gex)
		}
		idx := l.Index
		return &idx, gexFmtB(l.Gex)
	}
	lad.CW, lad.CWGex = pick("call_wall", "all")
	lad.PW, lad.PWGex = pick("put_wall", "all")
	lad.CW0, lad.CW0Gex = pick("call_wall", "0dte")
	lad.PW0, lad.PW0Gex = pick("put_wall", "0dte")
	lad.GW0, lad.GW0Gex = pick("gamma_wall", "0dte")
	lad.CWV, lad.CWVGex = pick("call_wall", "0dte_vol")
	lad.PWV, lad.PWVGex = pick("put_wall", "0dte_vol")
	lad.GWV, lad.GWVGex = pick("gamma_wall", "0dte_vol")
	for i, typ := range []string{"gamma_1", "gamma_2", "gamma_3"} {
		if v, note := pick(typ, "all"); v != nil {
			lad.Magnets = append(lad.Magnets, gexMagnet{V: *v, N: i + 1, Gex: note})
		}
	}
	for i, typ := range []string{"gamma_near_1", "gamma_near_2", "gamma_near_3"} {
		if v, note := pick(typ, "all"); v != nil {
			lad.Near = append(lad.Near, gexMagnet{V: *v, N: i + 1, Gex: note})
		}
	}
	// The prior-day band carries gex 0.0 by contract - the note would only
	// say "+0", so it is dropped.
	lad.EM1High, _ = pick("em_1d_max", "all")
	lad.EM1Low, _ = pick("em_1d_min", "all")

	lad.IntraState = s.Regime.IntradayState
	if useFut {
		lad.Flip = s.Flip.Futures
		lad.Flip0 = s.Flip0DTE.Futures
		lad.FlipI = s.FlipIntraday.Futures
	} else {
		lad.Flip = s.Flip.Index
		lad.Flip0 = s.Flip0DTE.Index
		lad.FlipI = s.FlipIntraday.Index
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
	case "gamma_wall":
		return "Gamma wall"
	case "gamma_1", "gamma_2", "gamma_3":
		return "Gamma magnet " + typ[len(typ)-1:]
	case "gamma_near_1", "gamma_near_2", "gamma_near_3":
		return "Near magnet " + typ[len(typ)-1:]
	case "em_1d_min":
		return "Prior EM low"
	case "em_1d_max":
		return "Prior EM high"
	default:
		return strings.ReplaceAll(typ, "_", " ")
	}
}

// gexBuildChart assembles the profile chart's data blob in the LADDER's
// denomination, so the two price axes on the page never disagree: futures
// when the basis is warm, index otherwise. Spot, flip and walls use the
// engine's published futures values (contract: never re-derive a futures
// price the engine publishes); the profile's strike axis has no published
// futures form, so it is scaled by the snapshot's own stored conversion
// ratio - the factor the engine applied to those very levels, which keeps
// each wall line exactly on its bar. The same scaling is the fallback for a
// level whose futures field is unexpectedly null while the ratio is warm.
func gexBuildChart(s *gex.Snapshot, spotFut *float64) gexChart {
	ch := gexChart{
		Denom:   "index",
		Strikes: s.Profile.StrikesIndex,
		Gex:     s.Profile.NetGex,
		Spot:    s.SpotIndex,
		Flip:    s.Flip.Index,
	}
	cw, pw := gexWall(s, "call_wall"), gexWall(s, "put_wall")
	if cw != nil {
		ch.CW = &cw.Index
	}
	if pw != nil {
		ch.PW = &pw.Index
	}
	fc := s.FuturesConversion
	if spotFut == nil || fc == nil || fc.Ratio <= 0 {
		return ch
	}

	ch.Denom = "futures"
	ch.Spot = *spotFut
	strikes := make([]float64, len(s.Profile.StrikesIndex))
	for i, v := range s.Profile.StrikesIndex {
		strikes[i] = v * fc.Ratio
	}
	ch.Strikes = strikes
	scaled := func(published *float64, index float64) *float64 {
		if published != nil {
			return published
		}
		f := index * fc.Ratio
		return &f
	}
	if s.Flip.Index != nil {
		ch.Flip = scaled(s.Flip.Futures, *s.Flip.Index)
	} else {
		ch.Flip = s.Flip.Futures
	}
	if cw != nil {
		ch.CW = scaled(cw.Futures, cw.Index)
	}
	if pw != nil {
		ch.PW = scaled(pw.Futures, pw.Index)
	}
	return ch
}
