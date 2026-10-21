package handlers

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// The bars a review reads. json.MarshalIndent prints an array element per
// line, so a bar as [time, open, high, low, close] cost seven lines; a bar
// is one string instead, "time open high low close", which the method's
// glossary explains. Prices print without trailing zeros.

// barRow is one bar as the model reads it.
func barRow(clock string, b marketdata.Bar) string {
	return clock + " " + fmtBarPrice(b.Open) + " " + fmtBarPrice(b.High) + " " + fmtBarPrice(b.Low) + " " + fmtBarPrice(b.Close)
}

func fmtBarPrice(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// barRows renders bars with the clock in the trader's timezone.
func barRows(bars []marketdata.Bar, layout string, tz *time.Location) []string {
	rows := make([]string, 0, len(bars))
	for _, b := range bars {
		rows = append(rows, barRow(time.Unix(b.Time, 0).In(tz).Format(layout), b))
	}
	return rows
}

// compactChartForAI turns the chart into what the reviewer reads: per panel
// the contract, the entry and exit averages and markers, and a window of
// price bars around the trade (aiBarWindow) instead of every cached bar.
func compactChartForAI(ce chartEnvelope, tz *time.Location) map[string]any {
	panels := make([]map[string]any, 0, len(ce.Panels))
	for i, p := range ce.Panels {
		if i == aiMaxChartPanels {
			break
		}
		panel := map[string]any{
			"contract":  p.Contract,
			"direction": p.Direction,
			"entryAvg":  p.EntryAvg,
			"exitAvg":   p.ExitAvg,
			"markers":   p.Markers,
		}
		if w := aiBarWindow(p, tz); w != nil {
			panel["bars"] = w
		}
		panels = append(panels, panel)
	}
	return map[string]any{"status": ce.Status, "panels": panels}
}

// The bar window: aiBarsBefore bars before the entry and aiBarsAfter after the
// exit, merged into wider bars when that is more than aiMaxBars; a chart
// beyond aiMaxChartPanels contracts (a busy day) is cut.
const (
	aiBarsBefore     = 30
	aiBarsAfter      = 15
	aiMaxBars        = 90
	aiMaxChartPanels = 3
)

// aiBarWindow is the stretch of a panel's bars around its trade, as
// {tf, entry, exit, rows: [[time, open, high, low, close], ...]} with times
// in the trader's timezone: what the model needs to judge the entry against
// the move and the exit against what followed. nil without bars.
func aiBarWindow(p chartPanel, tz *time.Location) map[string]any {
	if len(p.Bars) == 0 {
		return nil
	}
	var entryT, exitT int64
	for _, m := range p.Markers {
		if m.Role == "entry" && (entryT == 0 || m.T < entryT) {
			entryT = m.T
		}
		if m.Role == "exit" && m.T > exitT {
			exitT = m.T
		}
	}
	// barAt: the last bar opening at or before t.
	barAt := func(t int64) int {
		i := sort.Search(len(p.Bars), func(i int) bool { return p.Bars[i].Time > t }) - 1
		if i < 0 {
			return 0
		}
		return i
	}
	start, end := len(p.Bars)-aiMaxBars, len(p.Bars)
	if entryT > 0 {
		in, out := barAt(entryT), barAt(entryT)
		if exitT > 0 {
			out = barAt(exitT)
		}
		start, end = in-aiBarsBefore, out+aiBarsAfter+1
	}
	start, end = max(start, 0), min(end, len(p.Bars))
	bars := p.Bars[start:end]
	// Merge k consecutive bars when the window is too long.
	k := (len(bars) + aiMaxBars - 1) / aiMaxBars
	tf := p.BarTimeframe
	if k > 1 {
		merged := make([]marketdata.Bar, 0, aiMaxBars)
		for i := 0; i < len(bars); i += k {
			j := min(i+k, len(bars))
			b := marketdata.Bar{Time: bars[i].Time, Open: bars[i].Open, High: bars[i].High, Low: bars[i].Low, Close: bars[j-1].Close}
			for _, x := range bars[i:j] {
				b.High, b.Low = math.Max(b.High, x.High), math.Min(b.Low, x.Low)
			}
			merged = append(merged, b)
		}
		bars = merged
		tf = scaleTimeframe(p.BarTimeframe, k)
	}
	clock := func(t int64) string { return time.Unix(t, 0).In(tz).Format("15:04") }
	rows := make([]string, 0, len(bars))
	for _, b := range bars {
		rows = append(rows, barRow(clock(b.Time), b))
	}
	w := map[string]any{"tf": tf, "rows": rows}
	if entryT > 0 {
		w["entry"] = clock(entryT)
	}
	if exitT > 0 {
		w["exit"] = clock(exitT)
	}
	return w
}

// scaleTimeframe names k merged bars of tf ("1m" x5 = "5m"); tf itself when it
// does not parse.
func scaleTimeframe(tf string, k int) string {
	if len(tf) < 2 {
		return tf
	}
	n, err := strconv.Atoi(tf[:len(tf)-1])
	if err != nil {
		return tf
	}
	return strconv.Itoa(n*k) + tf[len(tf)-1:]
}
