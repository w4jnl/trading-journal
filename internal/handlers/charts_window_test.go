package handlers

import (
	"context"
	"strings"
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
	// The day so far at one minute: as many bars as minutes since the
	// day's start, give or take the partial minute at each end (the test
	// runs at any hour, so the count is relative, not fixed).
	if minutes := (now.Unix() - dayStart.Unix()) / 60; int64(len(panel.Bars)) < minutes-2 || int64(len(panel.Bars)) > minutes+1 {
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

// secondSource serves one-second bars (an exchange feed stand-in).
type secondSource struct{ price float64 }

func (s secondSource) FetchExchangeBars(_ context.Context, _ string, unitSeconds int, from, to time.Time) ([]marketdata.Bar, error) {
	var out []marketdata.Bar
	for t := from.Unix(); t < to.Unix(); t += int64(unitSeconds) {
		out = append(out, marketdata.Bar{Time: t, Open: s.price, High: s.price + 0.5, Low: s.price - 0.5, Close: s.price, Volume: 1})
	}
	return out, nil
}

// A short trade with a one-second window gets the fine bars on its panel,
// the exchange source on the day, and the image at five seconds.
func TestChartPanelCarriesFineBars(t *testing.T) {
	fx := newPositionFixture(t)
	cache := &marketdata.CachingSource{DB: fx.h.DB, Source: minuteSource{price: 7250}, Exchange: secondSource{price: 7250}, Loc: time.UTC}
	fx.h.MarketData = cache
	now := time.Now().UTC()
	entry := now.Add(-2 * time.Hour)
	exit := entry.Add(50 * time.Second)
	if err := cache.FetchFine(context.Background(), "ES=F", entry.Add(-marketdata.FinePad), exit.Add(marketdata.FinePad)); err != nil {
		t.Fatal(err)
	}
	g := chartGroup{yahooSym: "ES=F", mapped: true, root: "ES", contract: "ESM6", direction: "long", firstEntry: entry, lastExit: exit}
	panel, status := fx.h.buildChartPanel(context.Background(), g, true, now)
	if status != "ok" {
		t.Fatalf("status = %s", status)
	}
	if len(panel.FineBars) < 280 || panel.FineFrom > entry.Unix() || panel.FineTo < exit.Unix() {
		t.Errorf("fine bars = %d over %v..%v", len(panel.FineBars), time.Unix(panel.FineFrom, 0).UTC(), time.Unix(panel.FineTo, 0).UTC())
	}
	if panel.Source != marketdata.SourceExchange || !strings.Contains(panel.Note, "Exchange data") || strings.Contains(panel.Note, "Continuous front-month") {
		t.Errorf("source = %q note = %q", panel.Source, panel.Note)
	}
	img := focusPanel(panel)
	if img.Timeframe != "5s" || len(img.Bars) < 50 || len(img.Bars) >= len(panel.FineBars) {
		t.Errorf("image = %d bars at %s", len(img.Bars), img.Timeframe)
	}
}
