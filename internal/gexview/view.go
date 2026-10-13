// Package gexview builds the Gamma (GEX) view models - the Overview tab's
// view, ladder and charts, the Exposure tab's profile blob, the data-quality
// strip - from the engine's snapshots and profiles. Pure functions over
// internal/gex types: no HTTP, no database, no request state, so the
// handlers only parse parameters, fetch, build and render.
package gexview

import (
	"encoding/json"
	"fmt"
	"github.com/w4jnl/trading-journal/internal/gex"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HistoryStart is the engine's first stored session (nothing exists at or
// before any earlier moment, per the P4 handoff).
var HistoryStart = time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

// GridSessions caps the scrub slider's window. 60 sessions x 75 slots is
// a ~50KB inline array; the "as of" picker covers anything older.
const GridSessions = 60

// PageLevel is one row of the levels table, pre-formatted.
type PageLevel struct {
	Label   string `json:"label"`
	Bucket  string `json:"bucket"`
	Index   string `json:"index"`
	Futures string `json:"futures"`
	Gex     string `json:"gex"`
	Dist    string `json:"dist"` // signed points from spot (futures terms)
}

// Chart is the profile chart's data blob. Denominated like the ladder:
// futures when the basis is warm, index when cold - Denom names which, so
// the vertical and horizontal price axes always read as one.
type Chart struct {
	Denom   string    `json:"denom"`
	Strikes []float64 `json:"strikes"`
	Gex     []float64 `json:"gex"`
	Spot    float64   `json:"spot"`
	Flip    *float64  `json:"flip"`
	// FlipI / Flip0 are the front-week and 0DTE flips (same denomination
	// rules as Flip), drawn as reference lines on both profile charts.
	FlipI *float64 `json:"flipI"`
	Flip0 *float64 `json:"flip0"`
	CW    *float64 `json:"cw"`
	PW    *float64 `json:"pw"`
	// NativeW is the snapshot profile's own strike spacing in INDEX
	// points, Ratio the futures conversion behind Strikes (0 when index-
	// denominated). Together they let the client re-bin the profile into
	// the canonical coarser widths (net GEX sums exactly) - the snapshot
	// endpoint has no ?bucket=, so aggregation is client-side.
	NativeW float64 `json:"nativeW"`
	Ratio   float64 `json:"ratio"`
	// BinLadder is the market's bucket-width ladder for the bin control
	// (CanonLadder) - the client greys out widths the native grid
	// cannot serve, exactly like the Exposure tab's control.
	BinLadder []int `json:"binLadder"`
}

// Ladder is the numeric model behind the vertical regime-ladder panel:
// the price structure around spot, in ONE denomination so the panel can be
// read at a glance. Futures when the basis is warm (that is the price you
// fill at), index otherwise; Denom names which, and the panel labels it.
// Nil levels are simply not drawn.
type Ladder struct {
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
	Flip0   *float64 `json:"flip0"`
	CW0     *float64 `json:"cw0"`
	PW0     *float64 `json:"pw0"`
	GW0     *float64 `json:"gw0"`
	CW0Gex  string   `json:"cw0Gex"`
	PW0Gex  string   `json:"pw0Gex"`
	GW0Gex  string   `json:"gw0Gex"`
	Magnets []Magnet `json:"magnets"`
	EM1High *float64 `json:"em1High"`
	EM1Low  *float64 `json:"em1Low"`
	// Engine v0.9.0 additions (update 007): the session-open-anchored
	// intraday band (opening spot +/- opening EM x 1.25, static after the
	// open) - the band that lines up with MenthorQ's intra 1D Min/Max.
	EMIHigh *float64 `json:"emiHigh"`
	EMILow  *float64 `json:"emiLow"`
	// Engine v0.6.0 additions (update 003): the front-week (<=5 DTE) flip,
	// today's book re-weighted by session volume, and the near magnets
	// (ranked within +/-1% of spot).
	// IntraState mirrors the engine's regime.intraday_state verbatim for the
	// ladder's now-pill (the INTRA column state is published, never derived).
	IntraState string   `json:"intraState"`
	FlipI      *float64 `json:"flipI"`
	CWV        *float64 `json:"cwv"`
	PWV        *float64 `json:"pwv"`
	GWV        *float64 `json:"gwv"`
	CWVGex     string   `json:"cwvGex"`
	PWVGex     string   `json:"pwvGex"`
	GWVGex     string   `json:"gwvGex"`
	Near       []Magnet `json:"near"`
}

// Magnet is one ranked gamma magnet (gamma_1..gamma_3) in the ladder's
// denomination.
type Magnet struct {
	V   float64 `json:"v"`
	N   int     `json:"n"` // rank, 1 = largest
	Gex string  `json:"gex"`
}

// ChartProfile is one selectable per-strike slice of the profile chart
// (from the engine's /v1/profile views, keyed like the Exposure tab:
// all/0dte/0dte_vol). Strikes are denominated like Chart.Strikes - the
// chart's own conversion ratio, so the wall/flip lines stay exactly on the
// bars - and NativeW is the served bucket width in INDEX points, feeding
// the client-side re-binning exactly like Chart.NativeW.
type ChartProfile struct {
	Strikes []float64 `json:"strikes"`
	Gex     []float64 `json:"gex"`
	NativeW float64   `json:"nativeW"`
}

// BuildChartProfiles maps the profile-detail views onto chart slices in
// ch's denomination. Filtered responses carry only the blend view, so the
// map then has just "all" - which is exactly the Exposure tab's rule (a
// filter forces the blend slice). nil when nothing well-formed is served.
func BuildChartProfiles(pd *gex.ProfileDetail, ch Chart) map[string]ChartProfile {
	if pd == nil {
		return nil
	}
	ratio := 1.0
	if ch.Ratio > 0 {
		ratio = ch.Ratio
	}
	out := make(map[string]ChartProfile, 3)
	add := func(key, engineKey string) {
		ps, ok := pd.Profiles[engineKey]
		if !ok {
			return
		}
		n := len(ps.StrikesIndex)
		if n < 2 || len(ps.NetGex) != n {
			return // malformed or degenerate: skip rather than chart nonsense
		}
		strikes := ps.StrikesIndex
		if ratio != 1.0 {
			strikes = make([]float64, n)
			for i, v := range ps.StrikesIndex {
				strikes[i] = v * ratio
			}
		}
		out[key] = ChartProfile{Strikes: strikes, Gex: ps.NetGex, NativeW: pd.BucketWidth}
	}
	add("all", "blend")
	add("0dte", "0dte")
	add("0dte_vol", "0dte_vol")
	if len(out) == 0 {
		return nil
	}
	return out
}

// DTERow is one band of the GEX-by-DTE tile.
type DTERow struct {
	Label string `json:"label"`
	Net   string `json:"net"`
	Delta string `json:"delta"` // vs ~1h earlier; "" when unavailable
	Neg   bool   `json:"neg"`
}

// BuildDTE buckets expiry_decay into 0-5 / 6-20 / >20 calendar-DTE
// bands (trading-TZ midnights, same date math as the Exposure chips).
// prev is the ~1h-earlier profile for the delta column; nil drops it.
// Bands with no expiries render "-" rather than a measured zero.
func BuildDTE(pd *gex.ProfileDetail, prev *gex.ProfileDetail, now time.Time, tz *time.Location) []DTERow {
	if pd == nil || len(pd.ExpiryDecay) == 0 {
		return nil
	}
	sum := func(decay map[string]float64) ([3]float64, [3]int) {
		var tot [3]float64
		var n [3]int
		midnight := time.Date(now.In(tz).Year(), now.In(tz).Month(), now.In(tz).Day(), 0, 0, 0, 0, tz)
		for d, v := range decay {
			t, err := time.ParseInLocation("2006-01-02", d, tz)
			if err != nil {
				continue
			}
			dte := int(math.Round(t.Sub(midnight).Hours() / 24))
			i := 0
			switch {
			case dte > 20:
				i = 2
			case dte > 5:
				i = 1
			}
			tot[i] += v
			n[i]++
		}
		return tot, n
	}
	tot, n := sum(pd.ExpiryDecay)
	var ptot [3]float64
	var pn [3]int
	if prev != nil {
		ptot, pn = sum(prev.ExpiryDecay)
	}
	labels := [3]string{"0-5d", "6-20d", ">20d"}
	rows := make([]DTERow, 0, 3)
	for i := range labels {
		row := DTERow{Label: labels[i], Net: "—"}
		if n[i] > 0 {
			row.Net = FmtB(tot[i])
			row.Neg = tot[i] < 0
			if prev != nil && pn[i] > 0 {
				row.Delta = FmtB(tot[i] - ptot[i]) // column header carries the "1h"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// View is the pre-formatted view model for one snapshot, shared verbatim
// by the HTML render and the GEXData JSON endpoint so the scrub JS can
// repaint the exact same fields the server rendered.
type View struct {
	AsOfET      string `json:"asOfET"`
	Stale       bool   `json:"stale"` // as_of lags the viewed moment (prior-session close)
	Regime      string `json:"regime"`
	RegimeClass string `json:"regimeClass"`
	// RegimeIntra carries the front-week reading ONLY when it disagrees with
	// the whole-book state ("intraday negative") - agreement renders nothing,
	// so the tile stays quiet in the common case (engine update 004).
	RegimeIntra      string `json:"regimeIntra"`
	RegimeIntraClass string `json:"regimeIntraClass"`
	// RegimeFlipped is the "flipped 15:10 ET" badge (engine v0.10.0
	// flipped_at); "" when the session has no flip yet.
	RegimeFlipped string `json:"regimeFlipped"`
	Pctl          string `json:"pctl"`
	NetGex        string `json:"netGex"`
	NetGexSub     string `json:"netGexSub"` // 1h/1d change line; "" pre-v0.5.0
	// NetDexLine pairs customer-book delta notional with its 1h change
	// ("DEX +978.6B · 1h +51.8B"); NOT dealer-signed (update 008).
	NetDexLine string `json:"netDexLine"`
	// Put/call session volume (blend markets): ratio tinted by lean, the
	// leg totals, and the 0DTE-only ratio when served.
	PCRatio      string `json:"pcRatio"`
	PCRatioClass string `json:"pcRatioClass"`
	PCSub        string `json:"pcSub"`
	PC0DTE       string `json:"pc0dte"`
	// PCLegs is the per-leg ratio line ("SPX 1.16 · SPY 1.14"; engine
	// v0.11.0 pc_volume_legs) so index and ETF contract counts stop
	// mixing in the blended ratio above it. "" when the engine has none.
	PCLegs string `json:"pcLegs"`
	// Quality is the row's trust verdict + provenance as the strip under
	// the banners renders it (update 009); Show=false hides the strip.
	Quality Quality `json:"quality"`
	// Skew tenors, engine-defined 25-delta risk reversal / ATM IV.
	SkewDTE0 string `json:"skewDte0"`
	SkewM1   string `json:"skewM1"`
	// SkewRR is the unnormalized risk reversal line ("RR 0DTE -1.9 · 1M
	// +3.4 vol pts"; v0.12.0), "" when neither tenor is served.
	SkewRR string `json:"skewRR"`
	// ProvLine is the footer's provenance suffix (update 010): a repaired
	// row's reason and the straddle clock-health figure; "" when neither.
	ProvLine    string `json:"provLine"`
	SpotIndex   string `json:"spotIndex"`
	SpotFutures string `json:"spotFutures"`
	ConvLine    string `json:"convLine"`
	FlipFutures string `json:"flipFutures"`
	FlipIndex   string `json:"flipIndex"`
	FlipSub     string `json:"flipSub"`
	EM          string `json:"em"`
	// EM presentation (intraday-panel phase 1): percent of spot, and the
	// session band position - min/spot/max against the 1Di band (engine
	// v0.9.0), falling back to spot +/- EM (which centers the marker by
	// construction). EMPosPct -1 = no band to draw.
	EMPct string `json:"emPct"`
	// EMLabel names what EM is (update 010): "next session" when the engine
	// publishes expected_move_next_session - the true one-day move - else
	// "1d" for older rows, where the only figure is the front expiry's
	// straddle. With both present, EMPct also carries "to close X pts".
	EMLabel  string `json:"emLabel"`
	EMBandLo string `json:"emBandLo"`
	EMBandHi string `json:"emBandHi"`
	EMPosPct int    `json:"emPosPct"`
	// DTE is the GEX-by-DTE breakdown (0-5 / 6-20 / >20 calendar days),
	// bucketed journal-side from /v1/profile expiry_decay; empty when the
	// profile fetch failed or the engine predates v0.7.0.
	DTE        []DTERow    `json:"dte"`
	Levels     []PageLevel `json:"levels"`
	Chart      Chart       `json:"chart"`
	Ladder     Ladder      `json:"ladder"`
	HasProfile bool        `json:"hasProfile"`
	// The profile card's filter surface (all /v1/profile-sourced, so all
	// best-effort): the expiry chip strip, whether the engine serves
	// per-expiry filtering, the selectable chart slices (all/0dte/
	// 0dte_vol; just "all" under a filter), and - when a filter cannot be
	// served for this moment - why the CHART alone is empty. The filter
	// never touches tiles/ladder/levels: those are whole-book snapshot
	// artifacts the engine cannot filter.
	SupportsExpiry   bool                    `json:"supportsExpiry"`
	Expiries         []ExpoExpiry            `json:"expiries,omitempty"`
	ChartProfiles    map[string]ChartProfile `json:"chartProfiles,omitempty"`
	ChartUnavailable string                  `json:"chartUnavailable,omitempty"`
	// AsOfParam identifies the snapshot for the AI-read endpoint, as
	// RFC3339Nano. It must keep SUB-SECOND precision: as_of lands a fraction
	// of a second past its grid mark (19:00:02.501055Z), so a value truncated
	// to whole seconds is EARLIER than the snapshot and at-or-before resolves
	// it to the previous one - which pinned reads to the wrong moment.
	//
	// The retained read is carried twice on purpose: Analysis is the markdown
	// the page template renders through the shared partial, AnalysisHTML the
	// sanitized HTML the scrub path swaps in (no markdown pass in JS), with the
	// model line already appended. Only the latter is serialized, so the JSON
	// never ships both.
	AsOfParam     string        `json:"asOfParam"`
	Analysis      string        `json:"-"`
	AnalysisModel string        `json:"-"` // the model that wrote the read
	AnalysisHTML  template.HTML `json:"analysisHTML"`
}

// PageData is the Overview tab's JSON data island (#gex-page-data), read
// by /static/js/gex-overview.js: the page ships no inline script, so its
// per-request facts travel as data instead.
type PageData struct {
	Market  string          `json:"market"`
	TZ      string          `json:"tz"`      // trading timezone (IANA), for the scrub label
	Latest  bool            `json:"latest"`  // live view: schedule the 5-minute refresh
	Grid    []int64         `json:"grid"`    // scrub slots (unix seconds), nil when too short
	GridPos int             `json:"gridPos"` // slot at or before the viewed moment
	View    json.RawMessage `json:"view"`    // the View blob; null without a snapshot
}

// Grid marshals the scrub slider's session grid plus the initial slider
// position: the last slot at or before the viewed moment (-1 = older than
// the window; the slider then starts parked without a seeded view). ok is
// false when the grid is too short to scrub. Shared by the Overview and
// Exposure page handlers - one grid, one scrub behavior.
func Grid(now, at time.Time) (grid []int64, pos int, ok bool) {
	grid = SessionGrid(now)
	if len(grid) < 2 {
		return nil, 0, false
	}
	pos = -1
	for i, ts := range grid {
		if ts <= at.Unix() {
			pos = i
		}
	}
	return grid, pos, true
}

// SessionGrid returns the scrub slider's time slots: 09:50-16:00 ET in
// five-minute steps (the engine's snapshot grid) for the most recent
// GridSessions weekdays, ascending, bounded by the engine history start
// and now. Holidays are left in - their slots resolve to the prior session's
// close via at-or-before, which the stale flag surfaces honestly.
func SessionGrid(now time.Time) []int64 {
	et := Eastern()
	// HistoryStart is a plain DATE; take its components rather than
	// converting the instant (which would slide it into the prior ET day).
	sy, sm, sd := HistoryStart.Date()
	startDay := time.Date(sy, sm, sd, 0, 0, 0, 0, et)

	n := now.In(et)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, et)
	var days []time.Time
	for len(days) < GridSessions && !day.Before(startDay) {
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

// BuildView turns a snapshot into the pre-formatted view model. ref is
// the moment being viewed (now for the live view); Stale flags an as_of
// lagging it by more than the grid step allows - the prior-session-close
// case. Every engine-nullable value renders "-" instead of erroring
// (client.go documents which ones those are).
func BuildView(s *gex.Snapshot, ref time.Time) View {
	et := Eastern()
	v := View{
		AsOfET:   s.AsOf.In(et).Format("Mon Jan 2, 15:04:05 ET"),
		Stale:    ref.Sub(s.AsOf) > StaleAfter,
		Regime:   s.Regime.State,
		Pctl:     "—",
		NetGex:   "—",
		EM:       "—",
		EMBandLo: "—",
		EMBandHi: "—",
		EMPosPct: -1,
		PCRatio:  "—",
		SkewDTE0: "—",
		SkewM1:   "—",
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
		if f := s.Regime.IntradayFlippedAt; f != nil {
			v.RegimeIntra += " · since " + f.In(et).Format("15:04 ET")
		}
	}
	if f := s.Regime.FlippedAt; f != nil {
		v.RegimeFlipped = "flipped " + f.In(et).Format("15:04 ET")
	}
	if total, ok := s.Regime.Total(); ok {
		v.NetGex = FmtB(total)
	}
	// Serve-time history: how the book moved into this moment (under ?at=
	// these are relative to that historical moment, so scrubbing stays
	// reproducible). Absent on markets/engines that do not publish them.
	var chg []string
	if d := s.NetGexChange1H; d != nil {
		chg = append(chg, "1h "+FmtB(*d))
	}
	if d := s.NetGexChange1D; d != nil {
		chg = append(chg, "1d "+FmtB(*d))
	}
	v.NetGexSub = strings.Join(chg, " · ")
	if d := s.NetDex; d != nil {
		v.NetDexLine = "DEX " + FmtB(*d)
		if c := s.NetDexChange1H; c != nil {
			v.NetDexLine += " · 1h " + FmtB(*c)
		}
	}
	if pc := s.PCVolume; pc != nil && pc.Call > 0 {
		v.PCRatio = fmt.Sprintf("%.2f", pc.Ratio)
		switch {
		case pc.Ratio >= 1.05:
			v.PCRatioClass = "pnl-neg" // put-heavy
		case pc.Ratio <= 0.95:
			v.PCRatioClass = "pnl-pos"
		}
		v.PCSub = "puts " + FmtCount(pc.Put) + " · calls " + FmtCount(pc.Call)
	}
	if pc := s.PCVolume0DTE; pc != nil && pc.Call > 0 {
		v.PC0DTE = fmt.Sprintf("0DTE %.2f", pc.Ratio)
	}
	v.PCLegs = PCLegsLine(s.PCVolumeLegs)
	v.Quality = BuildQuality(s.DataQuality, s.Provenance)
	if sk := s.Skew; sk != nil {
		if p := sk.DTE0Pct; p != nil {
			v.SkewDTE0 = fmt.Sprintf("%+.1f%%", *p)
		}
		if p := sk.M1Pct; p != nil {
			v.SkewM1 = fmt.Sprintf("%+.1f%%", *p)
		}
		v.SkewRR = SkewRRLine(sk)
	}
	v.ProvLine = ProvLine(s.Provenance, s.DataQuality)
	if p := s.Regime.Percentile20D; p != nil {
		v.Pctl = strconv.FormatFloat(*p, 'f', 0, 64)
	}
	// The tile headlines the one-day move. Since engine v0.12.0 that is
	// expected_move_next_session; expected_move_1d is the front expiry's
	// straddle, which intraday is only the move left to this session's
	// close, so it demotes to the sub line. Older rows have just the 1d
	// figure and keep the old label.
	v.EMLabel = "1d"
	emHead := s.ExpectedMove1D
	if n := s.ExpectedMoveNextSession; n != nil {
		emHead, v.EMLabel = n, "next session"
	}
	if emHead != nil {
		v.EM = fmt.Sprintf("%.1f pts", *emHead)
		var sub []string
		if s.SpotIndex > 0 {
			sub = append(sub, fmt.Sprintf("%.2f%% of spot", *emHead/s.SpotIndex*100))
		}
		if s.ExpectedMoveNextSession != nil && s.ExpectedMove1D != nil {
			sub = append(sub, fmt.Sprintf("to close %.1f pts", *s.ExpectedMove1D))
		}
		v.EMPct = strings.Join(sub, " · ")
	}

	v.SpotIndex = fmt.Sprintf("%.2f", s.SpotIndex)
	v.SpotFutures = "—"
	spotFut := SpotFutures(s)
	if spotFut != nil {
		v.SpotFutures = fmt.Sprintf("%.2f", *spotFut)
	}

	// Session band position: min/spot/max against the 1Di band (engine
	// v0.9.0; the session-open anchored range), falling back to spot +/- EM
	// - which centers the marker by construction, honest but static.
	{
		useFut := spotFut != nil
		spotV := s.SpotIndex
		if useFut {
			spotV = *spotFut
		}
		band := func(typ string) *float64 {
			l := LevelIn(s, typ, "all")
			if l == nil {
				return nil
			}
			if useFut {
				return l.Futures
			}
			idx := l.Index
			return &idx
		}
		lo, hi := band("em_intra_min"), band("em_intra_max")
		if (lo == nil || hi == nil) && s.ExpectedMove1D != nil {
			d := *s.ExpectedMove1D // index points by contract
			if useFut && s.FuturesConversion != nil && s.FuturesConversion.Ratio > 0 {
				d *= s.FuturesConversion.Ratio
			}
			l, h := spotV-d, spotV+d
			lo, hi = &l, &h
		}
		if lo != nil && hi != nil && *hi > *lo {
			v.EMBandLo = fmt.Sprintf("%.2f", *lo)
			v.EMBandHi = fmt.Sprintf("%.2f", *hi)
			v.EMPosPct = int(math.Round(math.Max(0, math.Min(100, (spotV-*lo)/(*hi-*lo)*100))))
		}
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

	v.Levels = make([]PageLevel, 0, len(s.Levels)+3)
	// The flips lead the ladder: each is a price level like the walls, and
	// listing it here is what pins its denomination - both columns present,
	// same level-minus-spot convention as every other row. (The engine
	// publishes no GEX for a flip; it is the zero crossing by definition.)
	flipRow := func(fl gex.Flip, bucket string) {
		if fl.Index == nil && fl.Futures == nil {
			return
		}
		row := PageLevel{Label: "Gamma flip", Bucket: bucket, Index: "—", Futures: "—", Gex: "—", Dist: "—"}
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
		row := PageLevel{
			Label:   LevelName(l.Type),
			Bucket:  l.Bucket,
			Index:   fmt.Sprintf("%.2f", l.Index),
			Futures: "—",
			Gex:     FmtB(l.Gex),
			Dist:    "—",
		}
		if l.Bucket == "0dte_vol" {
			row.Gex += " flow" // decision #33: session-flow gex, not OI-comparable
		}
		if strings.HasPrefix(l.Type, "em_") {
			// The EM bands (prior-day and intraday) carry gex 0.0 by
			// contract - they are static price bands, not gamma
			// concentrations; "+0.0B" would read as a measured zero.
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

	v.Chart = BuildChart(s, spotFut)
	v.Ladder = BuildLadder(s, spotFut)
	v.HasProfile = len(s.Profile.StrikesIndex) > 1 &&
		len(s.Profile.StrikesIndex) == len(s.Profile.NetGex)
	return v
}

// SkewRRLine renders the risk reversal in vol points per tenor. The
// engine serves IV units (0.034 = 3.4 vol points), hence the x100.
func SkewRRLine(sk *gex.Skew) string {
	if sk == nil {
		return ""
	}
	var parts []string
	if p := sk.DTE0RRPts; p != nil {
		parts = append(parts, fmt.Sprintf("0DTE %+.1f", *p*100))
	}
	if p := sk.M1RRPts; p != nil {
		parts = append(parts, fmt.Sprintf("1M %+.1f", *p*100))
	}
	if len(parts) == 0 {
		return ""
	}
	return "RR " + strings.Join(parts, " · ") + " vol pts"
}

// PCLegsLine renders the per-leg put/call ratios, legs in name order
// (which puts the index leg first for SPX/SPY and NDX/QQQ). Legs without
// call volume are skipped rather than shown as a division by zero.
func PCLegsLine(legs map[string]gex.PCVolume) string {
	if len(legs) == 0 {
		return ""
	}
	names := make([]string, 0, len(legs))
	for n := range legs {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if pc := legs[n]; pc.Call > 0 {
			parts = append(parts, fmt.Sprintf("%s %.2f", n, pc.Ratio))
		}
	}
	return strings.Join(parts, " · ")
}

// SpotFutures resolves spot in futures terms. The engine publishes it
// directly from v0.3.0 (contract: "consumers must never re-derive a futures
// price themselves - spot included"); the ratio fallback exists only so an
// older engine still renders, and disappears with it. Nil means the basis is
// cold or the market is index-only, in which case the page shows index values
// and says so rather than inventing a conversion.
func SpotFutures(s *gex.Snapshot) *float64 {
	if s.SpotFutures != nil {
		return s.SpotFutures
	}
	if fc := s.FuturesConversion; fc != nil {
		f := s.SpotIndex * fc.Ratio
		return &f
	}
	return nil
}

// BuildLadder assembles the regime-ladder panel's levels in a single
// denomination. spotFut is nil when the futures basis is cold, in which case
// the whole ladder falls back to index terms rather than dropping out.
//
// The expected-move band is spot +/- expected_move_1d. That figure is
// index-denominated at source (it comes from the SPX 0DTE ATM straddle), so
// the futures band scales it by the same conversion ratio the engine applies
// to its own levels.
func BuildLadder(s *gex.Snapshot, spotFut *float64) Ladder {
	useFut := spotFut != nil
	lad := Ladder{Denom: "index", Spot: s.SpotIndex, Regime: s.Regime.State}
	if useFut {
		lad.Denom, lad.Spot = "futures", *spotFut
	}

	pick := func(typ, bucket string) (*float64, string) {
		l := LevelIn(s, typ, bucket)
		if l == nil {
			return nil, ""
		}
		note := FmtB(l.Gex)
		if bucket == "0dte_vol" {
			// Decision #33: vol-bucket magnitudes are session-FLOW gex
			// (volume counts opens AND closes; late-day values run 20-50x
			// the OI walls). The suffix keeps the two scales from ever
			// reading as comparable.
			note += " flow"
		}
		if useFut {
			return l.Futures, note
		}
		idx := l.Index
		return &idx, note
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
			lad.Magnets = append(lad.Magnets, Magnet{V: *v, N: i + 1, Gex: note})
		}
	}
	for i, typ := range []string{"gamma_near_1", "gamma_near_2", "gamma_near_3"} {
		if v, note := pick(typ, "all"); v != nil {
			lad.Near = append(lad.Near, Magnet{V: *v, N: i + 1, Gex: note})
		}
	}
	// The EM bands carry gex 0.0 by contract - the note would only say
	// "+0", so it is dropped.
	lad.EM1High, _ = pick("em_1d_max", "all")
	lad.EM1Low, _ = pick("em_1d_min", "all")
	lad.EMIHigh, _ = pick("em_intra_max", "all")
	lad.EMILow, _ = pick("em_intra_min", "all")

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

// FmtB renders a raw gamma-dollar value at billions scale ("+74.5B").
// FmtCount formats a contract count (1714233 -> "1.71M").
func FmtCount(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0fK", v/1e3)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

func FmtB(v float64) string {
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

func LevelName(typ string) string {
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
	case "em_intra_min":
		return "Intraday EM low"
	case "em_intra_max":
		return "Intraday EM high"
	default:
		return strings.ReplaceAll(typ, "_", " ")
	}
}

// BuildChart assembles the profile chart's data blob in the LADDER's
// denomination, so the two price axes on the page never disagree: futures
// when the basis is warm, index otherwise. Spot, flip and walls use the
// engine's published futures values (contract: never re-derive a futures
// price the engine publishes); the profile's strike axis has no published
// futures form, so it is scaled by the snapshot's own stored conversion
// ratio - the factor the engine applied to those very levels, which keeps
// each wall line exactly on its bar. The same scaling is the fallback for a
// level whose futures field is unexpectedly null while the ratio is warm.
func BuildChart(s *gex.Snapshot, spotFut *float64) Chart {
	ch := Chart{
		Denom:   "index",
		Strikes: s.Profile.StrikesIndex,
		Gex:     s.Profile.NetGex,
		Spot:    s.SpotIndex,
		Flip:    s.Flip.Index,
		FlipI:   s.FlipIntraday.Index,
		Flip0:   s.Flip0DTE.Index,
	}
	// Native strike spacing: the smallest gap between adjacent strikes
	// (the profile can have holes - empty buckets are never stored - so
	// the minimum is the grid, not the average).
	for i := 1; i < len(s.Profile.StrikesIndex); i++ {
		if d := s.Profile.StrikesIndex[i] - s.Profile.StrikesIndex[i-1]; d > 0 && (ch.NativeW == 0 || d < ch.NativeW) {
			ch.NativeW = d
		}
	}
	ch.BinLadder = CanonLadder(s.Market, int(math.Round(ch.NativeW)))
	cw, pw := Wall(s, "call_wall"), Wall(s, "put_wall")
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
	ch.Ratio = fc.Ratio
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
	flipFut := func(fl gex.Flip) *float64 {
		if fl.Index != nil {
			return scaled(fl.Futures, *fl.Index)
		}
		return fl.Futures
	}
	ch.Flip = flipFut(s.Flip)
	ch.FlipI = flipFut(s.FlipIntraday)
	ch.Flip0 = flipFut(s.Flip0DTE)
	if cw != nil {
		ch.CW = scaled(cw.Futures, cw.Index)
	}
	if pw != nil {
		ch.PW = scaled(pw.Futures, pw.Index)
	}
	return ch
}

// IslandJSON renders the island. json.Marshal escapes < > & as \u00XX, so a
// "</script" sequence can never appear inside the element.
func (p PageData) IslandJSON() template.JS {
	if p.View == nil {
		p.View = json.RawMessage("null")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "null"
	}
	return template.JS(b)
}
