package levelmap

import (
	"errors"
	"fmt"
	"time"
)

// Feed is what a MotiveWave chart posts (docs/api-handoff-002-levels-feed.md):
// the session map as the chart knows it. Blocks absent from the post are nil.
type Feed struct {
	Contract string     `json:"contract"`
	AsOf     time.Time  `json:"asOf"`
	Mark     float64    `json:"mark"`
	Session  *FeedSess  `json:"session,omitempty"`
	VWAP     *FeedVWAP  `json:"vwap,omitempty"`
	ATR      *FeedATR   `json:"atr,omitempty"`
	Study    *FeedStudy `json:"study,omitempty"`
}

type FeedSess struct {
	Pdh           *float64 `json:"pdh,omitempty"`
	Pdl           *float64 `json:"pdl,omitempty"`
	Pdc           *float64 `json:"pdc,omitempty"`
	Onh           *float64 `json:"onh,omitempty"`
	Onl           *float64 `json:"onl,omitempty"`
	Poc           *float64 `json:"poc,omitempty"`
	Vah           *float64 `json:"vah,omitempty"`
	Val           *float64 `json:"val,omitempty"`
	ProfileApprox bool     `json:"profileApprox,omitempty"`
	OR            *FeedOR  `json:"or,omitempty"`
}

type FeedOR struct {
	Minutes int     `json:"minutes"`
	High    float64 `json:"high"`
	Low     float64 `json:"low"`
	Final   bool    `json:"final"`
}

type FeedVWAP struct {
	Value float64 `json:"value"`
	SD1   float64 `json:"sd1,omitempty"`
	SD2   float64 `json:"sd2,omitempty"`
}

type FeedATR struct {
	Daily14    float64   `json:"daily14"`
	Method     string    `json:"method,omitempty"` // smma | sma
	Anchor     float64   `json:"anchor,omitempty"`
	AnchoredAt string    `json:"anchoredAt,omitempty"` // eth | rth
	Mults      []float64 `json:"mults,omitempty"`
}

type FeedStudy struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	BarSize string `json:"barSize,omitempty"`
}

// Validate checks a post against the contract: the required fields, the
// clock, and that every price present is a positive number.
func (f *Feed) Validate(now time.Time) error {
	if f == nil {
		return errors.New("empty body")
	}
	if f.Contract == "" {
		return errors.New("contract is required")
	}
	if f.AsOf.IsZero() {
		return errors.New("asOf is required (RFC 3339)")
	}
	if f.AsOf.After(now.Add(5 * time.Minute)) {
		return errors.New("asOf is in the future")
	}
	if now.Sub(f.AsOf) > 3*24*time.Hour {
		return errors.New("asOf is older than three days")
	}
	if !positive(f.Mark) {
		return errors.New("mark must be a positive price")
	}
	check := func(name string, p *float64) error {
		if p != nil && !positive(*p) {
			return fmt.Errorf("session.%s must be a positive price", name)
		}
		return nil
	}
	if s := f.Session; s != nil {
		for name, p := range map[string]*float64{"pdh": s.Pdh, "pdl": s.Pdl, "pdc": s.Pdc, "onh": s.Onh, "onl": s.Onl, "poc": s.Poc, "vah": s.Vah, "val": s.Val} {
			if err := check(name, p); err != nil {
				return err
			}
		}
		if o := s.OR; o != nil {
			if !positive(o.High) || !positive(o.Low) || o.Low > o.High {
				return errors.New("session.or needs a positive high and low, low at or under high")
			}
			switch o.Minutes {
			case 5, 15, 30:
			default:
				return errors.New("session.or.minutes must be 5, 15 or 30")
			}
		}
	}
	if v := f.VWAP; v != nil {
		if !positive(v.Value) || v.SD1 < 0 || v.SD2 < 0 {
			return errors.New("vwap needs a positive value and non-negative deviations")
		}
	}
	if a := f.ATR; a != nil {
		if !positive(a.Daily14) {
			return errors.New("atr.daily14 must be positive")
		}
		switch a.Method {
		case "", "smma", "sma":
		default:
			return errors.New("atr.method must be smma or sma")
		}
		switch a.AnchoredAt {
		case "", "eth", "rth":
		default:
			return errors.New("atr.anchoredAt must be eth or rth")
		}
		if a.Anchor != 0 && !positive(a.Anchor) {
			return errors.New("atr.anchor must be a positive price")
		}
		for _, k := range a.Mults {
			if k <= 0 || k > 5 {
				return errors.New("atr.mults must be between 0 and 5")
			}
		}
	}
	return nil
}

func positive(v float64) bool { return v > 0 && v < 1e9 }

// Fields counts what a post carries, so two posts with the same asOf (two
// charts of one market) can be ranked: the fuller one shows.
func (f *Feed) Fields() int {
	if f == nil {
		return 0
	}
	n := 0
	if f.Mark > 0 {
		n++
	}
	if s := f.Session; s != nil {
		for _, p := range []*float64{s.Pdh, s.Pdl, s.Pdc, s.Onh, s.Onl, s.Poc, s.Vah, s.Val} {
			if p != nil {
				n++
			}
		}
		if s.OR != nil {
			n += 2
		}
	}
	if v := f.VWAP; v != nil {
		n++
		if v.SD1 > 0 {
			n++
		}
		if v.SD2 > 0 {
			n++
		}
	}
	if a := f.ATR; a != nil {
		n++
		if a.Anchor > 0 {
			n++
		}
	}
	return n
}

// overrideFromFeed drops the bar-derived session levels the chart knows
// better and returns the chart's own: the replacements, the profile, the
// VWAP bands.
func overrideFromFeed(levels []Level, f *Feed) (kept, added []Level) {
	drop := map[string]bool{}
	add := func(l Level) { added = append(added, l) }
	put := func(kind, name string, p *float64, tf string) {
		if p == nil {
			return
		}
		drop[kind] = true
		add(Level{Kind: kind, Name: name, Price: *p, TF: tf, Source: SourceChart})
	}
	if s := f.Session; s != nil {
		put("pdh", "prior day high", s.Pdh, "1d")
		put("pdl", "prior day low", s.Pdl, "1d")
		put("pdc", "prior day close", s.Pdc, "1d")
		put("onh", "overnight high", s.Onh, "1m")
		put("onl", "overnight low", s.Onl, "1m")
		approx := ""
		if s.ProfileApprox {
			approx = " (approx.)"
		}
		put("poc", "prior day POC"+approx, s.Poc, "1d")
		put("vah", "prior day value area high"+approx, s.Vah, "1d")
		put("val", "prior day value area low"+approx, s.Val, "1d")
		if o := s.OR; o != nil {
			forming := ""
			if !o.Final {
				forming = ", forming"
			}
			h, l := o.High, o.Low
			put("orh", fmt.Sprintf("opening range high (%dm%s)", o.Minutes, forming), &h, "1m")
			put("orl", fmt.Sprintf("opening range low (%dm%s)", o.Minutes, forming), &l, "1m")
		}
	}
	if v := f.VWAP; v != nil {
		val := v.Value
		put("vwap", "session VWAP", &val, "1m")
		if v.SD1 > 0 {
			add(Level{Kind: "vwap_band", Name: "VWAP +1 sd", Price: v.Value + v.SD1, TF: "1m", Source: SourceChart})
			add(Level{Kind: "vwap_band", Name: "VWAP -1 sd", Price: v.Value - v.SD1, TF: "1m", Source: SourceChart})
		}
		if v.SD2 > 0 {
			add(Level{Kind: "vwap_band", Name: "VWAP +2 sd", Price: v.Value + v.SD2, TF: "1m", Source: SourceChart})
			add(Level{Kind: "vwap_band", Name: "VWAP -2 sd", Price: v.Value - v.SD2, TF: "1m", Source: SourceChart})
		}
	}
	for _, l := range levels {
		if drop[l.Kind] && l.Source == SourceBars {
			continue
		}
		kept = append(kept, l)
	}
	return kept, added
}
