// Exposure tab: the per-strike profile blob, bucket ladders and expiry chips.
package gexview

import (
	"encoding/json"
	"fmt"
	"github.com/w4jnl/trading-journal/internal/gex"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"
)

// ExpoProfile is one selectable per-strike profile in the page blob.
// CallOI/PutOI are contract counts (engine v0.7.0+, else nil): when present
// the right-hand columns render OI and the GEX split moves to the tooltip.
type ExpoProfile struct {
	Strikes []float64 `json:"strikes"`
	Net     []float64 `json:"net"`
	Call    []float64 `json:"call"`
	Put     []float64 `json:"put"`
	CallOI  []float64 `json:"callOi,omitempty"`
	PutOI   []float64 `json:"putOi,omitempty"`
}

// ExpoLevel is one overlay line. Kind drives color/dash in JS; Family
// gates it behind the profile filter chips (a 0DTE wall only makes sense on
// the 0DTE view).
type ExpoLevel struct {
	Label  string  `json:"label"`
	V      float64 `json:"v"`
	Kind   string  `json:"kind"`   // flip | flipI | flip0 | cw | pw | gw
	Family string  `json:"family"` // all | 0dte | 0dte_vol
	Gex    string  `json:"gex"`    // formatted; "" for flips
}

// ExpoExpiry is one informational chip in the expiry strip.
type ExpoExpiry struct {
	Date  string `json:"date"`
	Label string `json:"label"`
	DTE   int    `json:"dte"`
	Net   string `json:"net"`
	Pct   string `json:"pct"`
	Neg   bool   `json:"neg"`
}

// Expo is the page's inline JSON blob (plus template-rendered chrome).
// The /gex/exposure/data endpoint returns the same shape (minus levels and
// regime - the client keeps the page-load ones).
type Expo struct {
	Denom  string  `json:"denom"`
	AsOfET string  `json:"asOfET"`
	OIDate string  `json:"oiDate"`
	Spot   float64 `json:"spot"`
	// EM is half of today's expected-move band in the levels' denomination
	// (nil when the snapshot has none). The client lands on spot +/- 3 EM.
	EM          *float64               `json:"em"`
	BucketWidth float64                `json:"bucketWidth"`
	Profiles    map[string]ExpoProfile `json:"profiles"`
	Levels      []ExpoLevel            `json:"levels"`
	Expiries    []ExpoExpiry           `json:"expiries"`
	Regime      string                 `json:"regime"`
	NetTotal    string                 `json:"netTotal"`
	Stale       bool                   `json:"stale"`
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
	// market's ladder (CanonLadder: ES 5-10-25-50, NQ 10-20-50-100),
	// each width flagged servable or not (finer than the stored grid, or
	// not an integer multiple of it). Unservable widths render disabled
	// rather than hidden, so the control reads the same every day - an ES
	// row stored at 10 before engine v0.8.0 grays out 5 and 25; today's
	// 5-point grid enables everything.
	BucketIndex float64         `json:"bucketIndex"`
	BucketBase  int             `json:"bucketBase"`
	BucketOpts  []ExpoBucketOpt `json:"bucketOpts"`
	// Data-endpoint extras (omitempty keeps live blobs byte-stable):
	// LevelsNote flags a degraded moment-addressed response (snapshot fetch
	// failed, so no level lines); Degraded names the narrowing params a
	// fallback=1 request dropped ("bucket", "expiry", or both).
	LevelsNote string `json:"levelsNote,omitempty"`
	Degraded   string `json:"degraded,omitempty"`
	// Quality is the moment's trust verdict + provenance (engine update
	// 009), from the profile payload (same row as the snapshot, so the same
	// verdict) with the snapshot as fallback; the strip above the chart.
	Quality Quality `json:"quality"`
	// ProvLine mirrors the overview footer's provenance suffix (update 010).
	ProvLine string `json:"provLine,omitempty"`
}

// ExpoBucketOpt is one width in the bucket control.
type ExpoBucketOpt struct {
	W  int  `json:"w"`
	Ok bool `json:"ok"`
}

// BucketMax is the engine's ?bucket= ceiling (index points).
const BucketMax = 100

// CanonLadder is a market's width ladder (index points): the stored grid
// and its 2x / 5x / 10x. The grid follows the index leg's listed strike
// spacing near the money - SPX strikes sit 5 points apart, NDX 10 - so the
// ladders differ per market and stay fixed per market, which keeps the
// control reading the same on every row. An unknown market derives its
// ladder from the row's own stored width.
func CanonLadder(market string, base int) []int {
	switch strings.ToUpper(market) {
	case "ES", "SPX":
		return []int{5, 10, 25, 50}
	case "NQ", "NDX":
		return []int{10, 20, 50, 100}
	}
	if base > 0 {
		return []int{base, 2 * base, 5 * base, 10 * base}
	}
	return []int{5, 10, 25, 50}
}

// BucketOpts builds the market's width ladder for a stored base width: a
// width is servable when it IS the base or a coarser integer multiple of it
// within the engine's ceiling (the ?bucket= contract) - so an ES row stored
// at 10 before engine v0.8.0 greys out 5 and 25 while today's 5-point rows
// enable everything. A base outside the ladder (unexpected) is prepended so
// the native grid is always offered.
func BucketOpts(market string, base int) []ExpoBucketOpt {
	if base <= 0 {
		return nil
	}
	canon := CanonLadder(market, base)
	opts := make([]ExpoBucketOpt, 0, len(canon)+1)
	seen := false
	for _, w := range canon {
		if w == base {
			seen = true
		}
		opts = append(opts, ExpoBucketOpt{W: w, Ok: w == base || (w > base && w%base == 0 && w <= BucketMax)})
	}
	if !seen {
		opts = append([]ExpoBucketOpt{{W: base, Ok: true}}, opts...)
	}
	return opts
}

// ExpoPageData is the Exposure tab's JSON data island (#gex-expo-data),
// read by /static/js/gex-exposure.js. See PageData.
type ExpoPageData struct {
	Market  string          `json:"market"`
	Grid    []int64         `json:"grid"`
	GridPos int             `json:"gridPos"`
	Expo    json.RawMessage `json:"expo"` // the Expo blob; null without a profile
}

// BuildExpo assembles the exposure blob. s may be nil (snapshot down):
// the result is then index-denominated with no levels.
func BuildExpo(pd *gex.ProfileDetail, s *gex.Snapshot, now time.Time, tz *time.Location) Expo {
	et := Eastern()
	e := Expo{
		Denom:       "index",
		AsOfET:      pd.AsOf.In(et).Format("Mon Jan 2, 15:04:05 ET"),
		OIDate:      pd.OIDate,
		Spot:        pd.SpotIndex,
		BucketWidth: pd.BucketWidth,
		Profiles:    map[string]ExpoProfile{},
		Levels:      []ExpoLevel{},
		Expiries:    []ExpoExpiry{},
		Stale:       now.Sub(pd.AsOf) > StaleAfter,
	}

	e.SupportsExpiry = pd.SupportsExpiry
	e.Today = now.In(tz).Format("2006-01-02")
	dq, prov := pd.DataQuality, pd.Provenance
	if dq == nil && prov == nil && s != nil {
		dq, prov = s.DataQuality, s.Provenance
	}
	e.Quality = BuildQuality(dq, prov)
	e.ProvLine = ProvLine(prov, dq)
	e.IndexName = "index"
	if len(pd.Sources) > 0 && pd.Sources[0] != "" {
		e.IndexName = pd.Sources[0]
	}
	e.BucketIndex = pd.BucketWidth
	e.BucketBase = int(math.Round(pd.BucketWidth))
	e.BucketOpts = BucketOpts(pd.Market, e.BucketBase)

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
		spotFut = SpotFutures(s)
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
		p := ExpoProfile{
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
		e.NetTotal = FmtB(total)
	}

	// Levels: flatten the ladder builder's output (published futures values,
	// same as everywhere else on /gex). Family gates each line behind the
	// profile chips; the 0DTE flip is DUPLICATED under 0dte_vol so the
	// client filter stays a dumb equality check.
	if s != nil {
		lad := BuildLadder(s, spotFut)
		e.Regime = lad.Regime
		if lad.EMHigh != nil && lad.EMLow != nil && *lad.EMHigh > *lad.EMLow {
			em := (*lad.EMHigh - *lad.EMLow) / 2
			e.EM = &em
		}
		add := func(label string, v *float64, kind, family, gexNote string) {
			if v != nil {
				e.Levels = append(e.Levels, ExpoLevel{Label: label, V: *v, Kind: kind, Family: family, Gex: gexNote})
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

	e.Expiries = BuildExpiryChips(pd.ExpiryDecay, now, tz)
	return e
}

// BuildExpiryChips builds the expiry strip from expiry_decay, sorted by
// date. Pct is the share of total ABSOLUTE decay - matching the reference
// UI's per-expiry weight readout. now anchors the DTE math (the viewed
// moment under ?at=, so historical strips read as that day did).
func BuildExpiryChips(decay map[string]float64, now time.Time, tz *time.Location) []ExpoExpiry {
	dates := make([]string, 0, len(decay))
	var absSum float64
	for d, v := range decay {
		dates = append(dates, d)
		absSum += math.Abs(v)
	}
	sort.Strings(dates)
	todayMidnight := time.Date(now.In(tz).Year(), now.In(tz).Month(), now.In(tz).Day(), 0, 0, 0, 0, tz)
	out := make([]ExpoExpiry, 0, len(dates))
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
		out = append(out, ExpoExpiry{
			Date:  d,
			Label: t.Format("Jan 2"),
			DTE:   int(math.Round(t.Sub(todayMidnight).Hours() / 24)),
			Net:   FmtB(v),
			Pct:   pct,
			Neg:   v < 0,
		})
	}
	return out
}

func (p ExpoPageData) IslandJSON() template.JS {
	if p.Expo == nil {
		p.Expo = json.RawMessage("null")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "null"
	}
	return template.JS(b)
}
