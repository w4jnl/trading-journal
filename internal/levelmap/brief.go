package levelmap

import (
	"math"
	"sort"
	"strings"
)

// Brief is the map as a model reads it: the strongest zones with what
// meets there, the nearest zones around a price, the expected range and
// the day type. Small on purpose: a prompt holds a handful of markets.
type Brief struct {
	Market   string      `json:"market"`
	Day      string      `json:"day"`
	AsOf     string      `json:"asOf"`
	Source   string      `json:"source"` // what the map was built from: "bars", "bars+chart", "bars+chart+gex", ...
	Last     float64     `json:"last,omitempty"`
	ATR      float64     `json:"atrDaily,omitempty"`
	Expected *Expected   `json:"expectedRange,omitempty"`
	Anchor   string      `json:"atrAnchor,omitempty"`
	DayType  string      `json:"dayType,omitempty"`
	Lean     string      `json:"lean,omitempty"`
	Reasons  []string    `json:"leanReasons,omitempty"`
	Zones    []BriefZone `json:"strongestZones,omitempty"`
	Above    []BriefZone `json:"nearestAbove,omitempty"`
	Below    []BriefZone `json:"nearestBelow,omitempty"`
}

// BriefZone is one zone with its distance from the price the brief is
// taken around.
type BriefZone struct {
	Price    float64 `json:"price"`
	Low      float64 `json:"low,omitempty"`
	High     float64 `json:"high,omitempty"`
	Score    float64 `json:"score"`
	What     string  `json:"what"`
	Distance float64 `json:"distance,omitempty"` // points from the price, positive above
}

// Brief summarises the map around a price: the n strongest zones and the
// k nearest on each side. A zero price uses the map's last.
func (m *Map) Brief(around float64, n, k int) Brief {
	b := Brief{Market: m.Market, Day: m.Day, AsOf: m.AsOf.UTC().Format("2006-01-02T15:04:05Z"), Last: m.Last, ATR: math.Round(m.ATR*100) / 100, Expected: m.Expected, Anchor: m.Anchor}
	parts := []string{}
	if m.Sources.Bars {
		parts = append(parts, "bars")
	}
	if m.Sources.Chart {
		parts = append(parts, "chart")
	}
	if m.Sources.Gex {
		parts = append(parts, "gex")
	}
	b.Source = strings.Join(parts, "+")
	if dt := m.DayType; dt.PriorDay != "" {
		b.DayType, b.Lean, b.Reasons = dt.Sentence, dt.Bias, dt.Reasons
	}
	if around <= 0 {
		around = m.Last
	}
	zone := func(z Zone) BriefZone {
		names := make([]string, 0, len(z.Levels))
		for _, l := range z.Levels {
			if len(names) == 4 {
				break
			}
			names = append(names, l.Name)
		}
		out := BriefZone{Price: round2(z.Center), Score: z.Score, What: strings.Join(names, ", ")}
		if z.High-z.Low >= 0.005 {
			out.Low, out.High = round2(z.Low), round2(z.High)
		}
		if around > 0 {
			out.Distance = round2(z.Center - around)
		}
		return out
	}
	for i, z := range m.Zones {
		if i == n {
			break
		}
		b.Zones = append(b.Zones, zone(z))
	}
	if around > 0 && k > 0 {
		var above, below []Zone
		for _, z := range m.Zones {
			if z.Center > around {
				above = append(above, z)
			} else {
				below = append(below, z)
			}
		}
		sort.SliceStable(above, func(i, j int) bool { return above[i].Center < above[j].Center })
		sort.SliceStable(below, func(i, j int) bool { return below[i].Center > below[j].Center })
		for i, z := range above {
			if i == k {
				break
			}
			b.Above = append(b.Above, zone(z))
		}
		for i, z := range below {
			if i == k {
				break
			}
			b.Below = append(b.Below, zone(z))
		}
	}
	return b
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
