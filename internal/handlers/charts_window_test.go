package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// A chart loads the trade's whole trading day at the finest cached size,
// recommends the bar size the trade reads best at, and focuses on the
// stretch around the trade; the PNG keeps only that stretch.
func TestChartPanelLoadsTheDayAndFocusesTheTrade(t *testing.T) {
	fx := newPositionFixture(t)
	fx.h.MarketData = &marketdata.CachingSource{DB: fx.h.DB, Source: minuteSource{price: 7250}, Loc: time.UTC}
	now := time.Now().UTC()
	entry := now.Add(-3 * time.Hour)
	exit := entry.Add(8 * time.Minute)
	g := chartGroup{yahooSym: "ES=F", mapped: true, root: "ES", contract: "ESM6", direction: "long", firstEntry: entry, lastExit: exit}
	panel, status := fx.h.buildChartPanel(context.Background(), g, true, now)
	if status != "ok" {
		t.Fatalf("status = %s (%s)", status, panel.Note)
	}
	dayStart := time.Date(entry.Year(), entry.Month(), entry.Day(), 0, 0, 0, 0, time.UTC)
	if panel.From != dayStart.Unix() {
		t.Errorf("from = %v, want the day's start %v", time.Unix(panel.From, 0).UTC(), dayStart)
	}
	if panel.BarTimeframe != "1m" || panel.Timeframe != "1m" {
		t.Errorf("bar size = %s stored, %s recommended; want 1m for an eight-minute trade", panel.BarTimeframe, panel.Timeframe)
	}
	if panel.FocusFrom >= entry.Unix() || panel.FocusTo <= exit.Unix() || panel.FocusFrom < panel.From {
		t.Errorf("focus %v..%v does not wrap the trade inside the day", time.Unix(panel.FocusFrom, 0).UTC(), time.Unix(panel.FocusTo, 0).UTC())
	}
	if len(panel.Bars) < 600 || int64(len(panel.Bars)) > (now.Unix()-dayStart.Unix())/60+1 {
		t.Errorf("bars = %d, want the day so far at one minute", len(panel.Bars))
	}
	focused := focusPanel(panel)
	if len(focused.Bars) >= len(panel.Bars) || len(focused.Bars) < 30 {
		t.Errorf("png bars = %d of %d, want the stretch around the trade", len(focused.Bars), len(panel.Bars))
	}
	// A swing: the recommended size coarsens, the day window spans both days.
	g2 := chartGroup{yahooSym: "ES=F", mapped: true, root: "ES", contract: "ESM6", direction: "long", firstEntry: now.Add(-30 * time.Hour), lastExit: now.Add(-2 * time.Hour)}
	p2, status := fx.h.buildChartPanel(context.Background(), g2, true, now)
	if status != "ok" || p2.Timeframe == "1m" || p2.From > now.Add(-30*time.Hour).Unix() {
		t.Errorf("swing panel: status %s, size %s, from %v", status, p2.Timeframe, time.Unix(p2.From, 0).UTC())
	}
}
