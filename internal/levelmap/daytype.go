package levelmap

import (
	"fmt"
	"math"
	"strings"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// DayType is what the prior day and the overnight say about today, in facts
// defined by arithmetic.
type DayType struct {
	PriorDay  string            `json:"priorDay,omitempty"`
	Range     string            `json:"range,omitempty"` // inside | outside | nr7 | wide | normal
	RangePts  float64           `json:"rangePts,omitempty"`
	RangeATR  float64           `json:"rangeAtr,omitempty"` // the prior day's range as a share of the ATR
	Close     string            `json:"close,omitempty"`    // strong | weak | mid
	ClosePos  float64           `json:"closePos,omitempty"` // where it closed in its range, 0 to 1
	Relation  string            `json:"relation,omitempty"` // of the prior day's range to the day before: inside | overlap-higher | overlap-lower | outside | above | below
	Gap       *Gap              `json:"gap,omitempty"`
	Trend     map[string]string `json:"trend,omitempty"` // per timeframe: up | down | flat
	Overnight *Overnight        `json:"overnight,omitempty"`
	Bias      string            `json:"bias,omitempty"` // long | short | neutral
	Reasons   []string          `json:"reasons,omitempty"`
	Sentence  string            `json:"sentence,omitempty"`
}

// Gap is the open against the prior close: the RTH open once it exists,
// the last price before.
type Gap struct {
	Dir      string  `json:"dir"` // up | down
	Points   float64 `json:"points"`
	ATRShare float64 `json:"atrShare,omitempty"`
	Filled   bool    `json:"filled"`
	Basis    string  `json:"basis"` // rthOpen | last
}

// Overnight is the current trading day's Globex so far against the prior day.
type Overnight struct {
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	RangeATR float64 `json:"rangeAtr,omitempty"`
	AbovePDH bool    `json:"abovePdh"`
	BelowPDL bool    `json:"belowPdl"`
}

// ClassifyDay reads the facts off the completed days, the session so far,
// the ATR and the last price.
func ClassifyDay(prior []DayBar, sc *marketdata.SessionContext, atr, last float64, rthOpen *float64, tick float64) DayType {
	var dt DayType
	n := len(prior)
	if n == 0 {
		return dt
	}
	p := prior[n-1]
	dt.PriorDay = p.Day
	rng := p.High - p.Low
	dt.RangePts = rng
	if atr > 0 {
		dt.RangeATR = math.Round(rng/atr*100) / 100
	}
	// Range.
	dt.Range = "normal"
	if n >= 2 {
		q := prior[n-2]
		switch {
		case p.High < q.High && p.Low > q.Low:
			dt.Range = "inside"
		case p.High > q.High && p.Low < q.Low:
			dt.Range = "outside"
		}
		switch {
		case p.High <= q.Low:
			dt.Relation = "below"
		case p.Low >= q.High:
			dt.Relation = "above"
		case p.High < q.High && p.Low > q.Low:
			dt.Relation = "inside"
		case p.High > q.High && p.Low < q.Low:
			dt.Relation = "outside"
		case p.High > q.High:
			dt.Relation = "overlap-higher"
		default:
			dt.Relation = "overlap-lower"
		}
	}
	if n >= 7 && dt.Range == "normal" {
		nr := true
		for _, d := range prior[n-7 : n-1] {
			if d.High-d.Low <= rng {
				nr = false
				break
			}
		}
		if nr {
			dt.Range = "nr7"
		}
	}
	if atr > 0 && rng > 1.3*atr && dt.Range == "normal" {
		dt.Range = "wide"
	}
	// Close.
	if rng > 0 {
		pos := (p.Close - p.Low) / rng
		dt.ClosePos = math.Round(pos*100) / 100
		switch {
		case pos >= 0.75:
			dt.Close = "strong"
		case pos <= 0.25:
			dt.Close = "weak"
		default:
			dt.Close = "mid"
		}
	}
	// The gap: from the RTH open once it exists, else from the last price.
	ref, basis := 0.0, ""
	if rthOpen != nil && *rthOpen > 0 {
		ref, basis = *rthOpen, "rthOpen"
	} else if last > 0 {
		ref, basis = last, "last"
	}
	if ref > 0 {
		g := ref - p.Close
		if math.Abs(g) > tick {
			gap := &Gap{Dir: "up", Points: math.Round(math.Abs(g)*100) / 100, Basis: basis}
			if g < 0 {
				gap.Dir = "down"
			}
			if atr > 0 {
				gap.ATRShare = math.Round(math.Abs(g)/atr*100) / 100
			}
			if sc != nil && sc.RTH != nil && sc.RTH.Low <= p.Close && sc.RTH.High >= p.Close {
				gap.Filled = true
			}
			dt.Gap = gap
		}
	}
	// Trend on the daily: the last three swings on each side.
	dt.Trend = map[string]string{}
	dt.Trend["1d"] = trendOf(dailySwings(prior))
	// The overnight.
	if sc != nil && sc.Overnight != nil {
		o := &Overnight{High: sc.Overnight.High, Low: sc.Overnight.Low, AbovePDH: sc.Overnight.High > p.High, BelowPDL: sc.Overnight.Low < p.Low}
		if atr > 0 {
			o.RangeATR = math.Round((o.High-o.Low)/atr*100) / 100
		}
		dt.Overnight = o
	}
	dt.Bias, dt.Reasons = biasOf(dt)
	dt.Sentence = sentenceOf(dt)
	return dt
}

// trendOf reads higher highs and higher lows (or the reverse) off the
// swings; anything else is flat.
func trendOf(swings []Level) string {
	var highs, lows []float64
	for _, s := range swings {
		if strings.Contains(s.Name, "high") {
			highs = append(highs, s.Price)
		} else {
			lows = append(lows, s.Price)
		}
	}
	if len(highs) < 2 || len(lows) < 2 {
		return "flat"
	}
	hh := highs[len(highs)-1] > highs[len(highs)-2]
	hl := lows[len(lows)-1] > lows[len(lows)-2]
	switch {
	case hh && hl:
		return "up"
	case !hh && !hl:
		return "down"
	}
	return "flat"
}

// biasOf is one lean from the facts, with the facts named. It is a
// suggestion the trader overrides.
func biasOf(dt DayType) (string, []string) {
	score := 0
	var why []string
	switch dt.Close {
	case "strong":
		score++
		why = append(why, "closed strong")
	case "weak":
		score--
		why = append(why, "closed weak")
	}
	if dt.Trend["1d"] == "up" {
		score++
		why = append(why, "daily trend up")
	} else if dt.Trend["1d"] == "down" {
		score--
		why = append(why, "daily trend down")
	}
	if o := dt.Overnight; o != nil {
		if o.AbovePDH {
			score++
			why = append(why, "overnight above the prior high")
		}
		if o.BelowPDL {
			score--
			why = append(why, "overnight below the prior low")
		}
	}
	if g := dt.Gap; g != nil && g.ATRShare >= 0.5 && !g.Filled {
		why = append(why, fmt.Sprintf("gap %s of %.1f ATR open", g.Dir, g.ATRShare))
	}
	switch dt.Range {
	case "nr7":
		why = append(why, "NR7: expansion likely")
	case "inside":
		why = append(why, "inside day: break of the range decides")
	case "wide":
		why = append(why, "wide-range day: expect the range to narrow")
	}
	switch {
	case score >= 2:
		return "long", why
	case score <= -2:
		return "short", why
	}
	return "neutral", why
}

func sentenceOf(dt DayType) string {
	if dt.PriorDay == "" {
		return ""
	}
	parts := []string{}
	switch dt.Range {
	case "nr7":
		parts = append(parts, "NR7")
	case "inside":
		parts = append(parts, "inside day")
	case "outside":
		parts = append(parts, "outside day")
	case "wide":
		parts = append(parts, "wide-range day")
	}
	if dt.Close != "" && dt.Close != "mid" {
		parts = append(parts, "closing "+dt.Close)
	}
	head := "Prior day"
	if len(parts) > 0 {
		head = "Prior day: " + strings.Join(parts, ", ")
	}
	tail := []string{}
	if o := dt.Overnight; o != nil {
		switch {
		case o.AbovePDH:
			tail = append(tail, "overnight above the prior high")
		case o.BelowPDL:
			tail = append(tail, "overnight below the prior low")
		default:
			tail = append(tail, "overnight inside the prior range")
		}
	}
	if g := dt.Gap; g != nil && g.ATRShare >= 0.25 {
		tail = append(tail, fmt.Sprintf("gap %s %.1f ATR", g.Dir, g.ATRShare))
	}
	s := head
	if len(tail) > 0 {
		s += "; " + strings.Join(tail, ", ")
	}
	lean := map[string]string{"long": "lean long", "short": "lean short", "neutral": "no lean"}[dt.Bias]
	return s + ". " + strings.ToUpper(lean[:1]) + lean[1:] + "."
}
