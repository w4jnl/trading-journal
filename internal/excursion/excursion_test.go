package excursion

import (
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
)

func bar(t time.Time, o, h, l, c float64) marketdata.Bar {
	return marketdata.Bar{Time: t.Unix(), Open: o, High: h, Low: l, Close: c}
}

func TestCompute(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	m := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	bars := []marketdata.Bar{
		bar(m(0), 100, 101, 99, 100), // before the entry: ignored
		bar(m(1), 100, 103, 98, 102), // entry at 14:01:30 sits inside this bar
		bar(m(2), 102, 105, 101, 104),
		bar(m(3), 104, 104, 97, 98), // exit at 14:03:10 sits inside this bar
		bar(m(4), 98, 110, 90, 100), // after the exit: ignored
	}
	entry, exit := m(1).Add(30*time.Second), m(3).Add(10*time.Second)

	long, ok := Compute("long", 100, entry, exit, bars, marketdata.TF1m)
	if !ok || long.Bars != 3 || long.MFEPoints != 5 || long.MAEPoints != 3 || long.Timeframe != marketdata.TF1m {
		t.Errorf("long = %+v ok=%v, want MFE 5 (high 105) MAE 3 (low 97) over 3 bars", long, ok)
	}
	short, ok := Compute("short", 100, entry, exit, bars, marketdata.TF1m)
	if !ok || short.MFEPoints != 3 || short.MAEPoints != 5 {
		t.Errorf("short = %+v, want MFE 3 MAE 5", short)
	}

	// A trade that never saw the other side clamps at zero.
	up, _ := Compute("long", 97, entry, exit, bars, marketdata.TF1m)
	if up.MAEPoints != 0 || up.MFEPoints != 8 {
		t.Errorf("entry at the low: %+v, want MAE 0 MFE 8", up)
	}

	// No overlapping bar, or an exit that is not after the entry: not ok.
	if _, ok := Compute("long", 100, m(10), m(12), bars, marketdata.TF1m); ok {
		t.Error("no bars in window should not be ok")
	}
	if _, ok := Compute("long", 100, entry, entry, bars, marketdata.TF1m); ok {
		t.Error("zero holding time should not be ok")
	}

	// Coarse bars must fit the holding time: a 2-minute hold on 5m bars is
	// mostly the bar's time without the trade; on 1m bars it always counts.
	five := []marketdata.Bar{bar(m(0), 100, 120, 80, 100)}
	if _, ok := Compute("long", 100, m(1), m(3), five, marketdata.TF5m); ok {
		t.Error("2-minute hold on 5m bars should be rejected")
	}
	if e, ok := Compute("long", 100, m(1), m(7), five, marketdata.TF5m); !ok || e.MFEPoints != 20 || e.MAEPoints != 20 {
		t.Errorf("6-minute hold on 5m bars = %+v ok=%v, want MFE 20 MAE 20", e, ok)
	}
	if _, ok := Compute("long", 100, m(1), m(1).Add(20*time.Second), bars, marketdata.TF1m); !ok {
		t.Error("20-second scalp on 1m bars should still be measured")
	}
}

func TestEfficiencyAndMoney(t *testing.T) {
	if e := Efficiency("long", 100, 104, 5); e == nil || *e != 0.8 {
		t.Errorf("long 4 of 5 = %v, want 0.8", e)
	}
	if e := Efficiency("short", 100, 101, 3); e == nil || *e != -1.0/3 {
		t.Errorf("short that lost 1 with 3 available = %v, want -0.333", e)
	}
	if Efficiency("long", 100, 101, 0) != nil {
		t.Error("no MFE -> nil efficiency")
	}
	f := func(v float64) *float64 { return &v }
	es := models.Instrument{TickSize: f(0.25), TickValue: f(12.5)}
	if m := Money(2, 3, es); m == nil || *m != 300 {
		t.Errorf("2 pts x 50/pt x 3 = %v, want 300", m)
	}
	if Money(2, 3, models.Instrument{}) != nil {
		t.Error("stub instrument -> nil money")
	}
}
