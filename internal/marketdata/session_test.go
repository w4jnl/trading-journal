package marketdata

import (
	"math"
	"testing"
	"time"
)

// sessionBars builds one-minute bars from 17:00 Chicago of the day before
// to asOf, flat at base except for the shaped moments: the prior RTH
// pushes to base+10 and closes at base+4, the overnight dips to base-6,
// the opening range reaches base+3 / base-2, and a 15m swing high stands
// at base+8 an hour into today's RTH.
func sessionBars(t *testing.T, chicago *time.Location, day time.Time, base float64) []Bar {
	t.Helper()
	start := time.Date(day.Year(), day.Month(), day.Day()-1, 17, 0, 0, 0, chicago).AddDate(0, 0, -1) // two trading days
	var out []Bar
	for ts := start; ts.Before(time.Date(day.Year(), day.Month(), day.Day(), 10, 30, 0, 0, chicago)); ts = ts.Add(time.Minute) {
		m := ts.Hour()*60 + ts.Minute()
		key := tradeDayKey(ts, chicago)
		prior := key == day.AddDate(0, 0, -1).Format("2006-01-02")
		today := key == day.Format("2006-01-02")
		b := Bar{Time: ts.Unix(), Open: base, High: base + 0.5, Low: base - 0.5, Close: base, Volume: 10}
		switch {
		case prior && m >= rthStartMin && m < rthEndMin:
			if m == rthStartMin+120 {
				b.High = base + 10
			}
			if m == rthEndMin-1 {
				b.Close = base + 4
			}
		case today && m >= sessionTurn || today && m < rthStartMin:
			if m == 2*60 { // 02:00 overnight low
				b.Low = base - 6
			}
		case today && m >= rthStartMin && m < rthStartMin+openingMins:
			b.High, b.Low = base+3, base-2
		case today && m == rthStartMin+60:
			b.High = base + 8 // a 15m swing high (bars around it stay at base+0.5)
		}
		out = append(out, b)
	}
	return out
}

func TestComputeSession(t *testing.T) {
	chicago, _ := time.LoadLocation("America/Chicago")
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, chicago) // a Wednesday
	bars := sessionBars(t, chicago, day, 7000)
	asOf := time.Date(2026, 10, 14, 10, 30, 0, 0, chicago)
	c := ComputeSession(bars, asOf, chicago)
	if c.Bars1m == 0 || c.TradeDay != "2026-10-14" || c.Last != 7000 {
		t.Fatalf("context = %+v", c)
	}
	if c.PriorRTH == nil || c.PriorRTH.High != 7010 || c.PriorRTH.Close == nil || *c.PriorRTH.Close != 7004 || c.PriorRTH.Day != "2026-10-13" {
		t.Errorf("prior RTH = %+v", c.PriorRTH)
	}
	if c.Overnight == nil || c.Overnight.Low != 6994 || c.Overnight.High != 7000.5 {
		t.Errorf("overnight = %+v", c.Overnight)
	}
	if c.OpeningRange == nil || c.OpeningRange.High != 7003 || c.OpeningRange.Low != 6998 {
		t.Errorf("opening range = %+v", c.OpeningRange)
	}
	if c.RTH == nil || c.RTH.High != 7008 || c.RTH.Open == nil || *c.RTH.Open != 7000 {
		t.Errorf("rth = %+v", c.RTH)
	}
	if c.Session == nil || c.Session.Low != 6994 || c.Session.High != 7008 {
		t.Errorf("session = %+v", c.Session)
	}
	if c.VWAP == nil || math.Abs(*c.VWAP-7000) > 0.5 {
		t.Errorf("vwap = %v", c.VWAP)
	}
	if c.ATR["5m"] <= 0 || c.ATR["15m"] <= 0 {
		t.Errorf("atr = %v", c.ATR)
	}
	var swingHigh bool
	for _, s := range c.Swings {
		if s.TF == "15m" && s.Kind == "high" && s.Price == 7008 {
			swingHigh = true
		}
	}
	if !swingHigh {
		t.Errorf("the 15m swing high at 7008 is missing: %+v", c.Swings)
	}
	levels := c.Levels()
	if len(levels) == 0 || math.Abs(levels[0].Distance) > math.Abs(levels[len(levels)-1].Distance) {
		t.Errorf("levels not nearest first: %+v", levels)
	}

	// As of an earlier moment, only what was known then: before RTH, no
	// opening range and no RTH; the overnight is the session.
	early := ComputeSession(bars, time.Date(2026, 10, 14, 8, 0, 0, 0, chicago), chicago)
	if early.RTH != nil || early.OpeningRange != nil || early.Overnight == nil || early.Overnight.Low != 6994 {
		t.Errorf("early context = %+v", early)
	}
	if ComputeSession(nil, asOf, chicago).Bars1m != 0 {
		t.Errorf("empty input must yield an empty context")
	}
}
