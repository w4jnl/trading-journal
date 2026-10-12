package handlers

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/coach"
	"github.com/w4jnl/trading-journal/internal/journal"
)

// The weekly edge digest: one ISO week read the way a Sunday review reads
// it. Counted per decision (a copy set once, money summed), set against
// the week before, with what worked, what cost, the coach's findings
// tallied across the days, how the day plans held, and a short list of
// things to keep and to fix. Everything is derived from the analytics
// summary, the coach and the plans; nothing is stored.

type digestKPI struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Value     *float64 `json:"value"`
	Prev      *float64 `json:"previous"`
	Display   string   `json:"-"`
	PrevShown string   `json:"-"`
	Delta     string   `json:"-"`
	DeltaSign string   `json:"-"` // pnl-pos | pnl-neg | ""
}

type digestDecision struct {
	TradeID   int64   `json:"tradeId"` // first member: opens the trade (or its set from the sets view)
	Day       string  `json:"day"`
	Symbol    string  `json:"symbolRoot"`
	Direction string  `json:"direction"`
	Accounts  int64   `json:"accounts"`
	Net       float64 `json:"net"`
	Strategy  string  `json:"strategy,omitempty"`
}

type digestFinding struct {
	Key      string       `json:"key"`
	Title    string       `json:"title"`
	Severity string       `json:"severity"`
	Count    int          `json:"count"` // times it fired
	Days     int          `json:"days"`  // days it fired on
	Label    *coach.Label `json:"label,omitempty"`
}

type digestRule struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Held  int    `json:"held"`
	Total int    `json:"total"`
}

type digestPlan struct {
	TradingDays int          `json:"tradingDays"`
	Planned     int          `json:"planned"`     // trading days with a plan
	WrittenLate int          `json:"writtenLate"` // of those, written after the first trade
	Rules       []digestRule `json:"rules"`
}

type weeklyDigest struct {
	Week        string           `json:"week"` // 2026-W41
	From        string           `json:"from"`
	To          string           `json:"to"`
	TradingDays int              `json:"tradingDays"`
	KPIs        []digestKPI      `json:"kpis"`
	This        analyticsSummary `json:"this"` // unit = sets: counts per decision
	Prev        analyticsSummary `json:"previous"`
	Best        *digestDecision  `json:"bestDecision"`
	Worst       *digestDecision  `json:"worstDecision"`
	Coach       []digestFinding  `json:"coach"`
	Plan        digestPlan       `json:"plan"`
	Keep        []string         `json:"keep"`
	Fix         []string         `json:"fix"`
}

var digestRuleNames = map[string]string{"bias": "bias", "max-trades": "decision cap", "stop-time": "stop time", "max-loss": "max loss"}

// digestWeek resolves ?week=YYYY-Www; empty means the week of the last
// trading day (this week when nothing closed before today).
func (h *Handlers) digestWeek(ctx context.Context, arg string) (time.Time, string, error) {
	tz := h.localTZ()
	arg = strings.TrimSpace(arg)
	if arg == "" {
		d, err := h.lastTradingDay(ctx, tz)
		if err != nil {
			return time.Time{}, "", err
		}
		if d.IsZero() {
			d = todayLocal(tz)
		}
		y, w := d.ISOWeek()
		arg = fmt.Sprintf("%d-W%02d", y, w)
	}
	monday, y, w, err := parseISOWeek(arg, tz)
	if err != nil {
		return time.Time{}, "", journal.BadInput("bad week; expected YYYY-Www, e.g. 2026-W41")
	}
	return monday, fmt.Sprintf("%d-W%02d", y, w), nil
}

// buildDigest assembles the digest of the ISO week starting monday.
func (h *Handlers) buildDigest(ctx context.Context, monday time.Time, weekID string) (*weeklyDigest, error) {
	tz := h.localTZ()
	sunday := monday.AddDate(0, 0, 6)
	this, err := h.analyticsSummaryFor(ctx, accountFilter{}, apiDateRange{From: monday, To: sunday}, "sets")
	if err != nil {
		return nil, err
	}
	prev, err := h.analyticsSummaryFor(ctx, accountFilter{}, apiDateRange{From: monday.AddDate(0, 0, -7), To: monday.AddDate(0, 0, -1)}, "sets")
	if err != nil {
		return nil, err
	}
	d := &weeklyDigest{Week: weekID, From: monday.Format("2006-01-02"), To: sunday.Format("2006-01-02"), This: this, Prev: prev,
		Coach: []digestFinding{}, Keep: []string{}, Fix: []string{}}

	// Best and worst decision; the days that traded.
	from, to := monday.UTC(), sunday.AddDate(0, 0, 1).UTC()
	trades, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, &from, &to)
	if err != nil {
		return nil, err
	}
	units := collapseSets(trades)
	days := map[string]bool{}
	for _, t := range units {
		days[t.ExitTime.In(tz).Format("2006-01-02")] = true
		if t.NetPnl == nil {
			continue
		}
		dec := &digestDecision{TradeID: t.ID, Day: t.ExitTime.In(tz).Format("2006-01-02"), Symbol: t.SymbolRoot, Direction: t.Direction, Accounts: t.SetSize, Net: *t.NetPnl, Strategy: t.Strategy}
		if d.Best == nil || dec.Net > d.Best.Net {
			d.Best = dec
		}
		if d.Worst == nil || dec.Net < d.Worst.Net {
			d.Worst = dec
		}
	}
	if d.Best != nil && d.Best.Net <= 0 {
		d.Best = nil
	}
	if d.Worst != nil && d.Worst.Net >= 0 {
		d.Worst = nil
	}
	d.TradingDays = len(days)
	d.Plan = digestPlan{TradingDays: len(days), Rules: []digestRule{}}

	// The coach per trading day: findings tallied, the plans' rules counted.
	tally := map[string]*digestFinding{}
	var order []string
	rules := map[string]*digestRule{}
	var ruleOrder []string
	dayKeys := make([]string, 0, len(days))
	for k := range days {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		day, _ := time.ParseInLocation("2006-01-02", k, tz)
		rep := h.coachReport(ctx, day)
		if rep == nil {
			continue
		}
		seen := map[string]bool{}
		for _, f := range rep.Findings {
			t, ok := tally[f.Key]
			if !ok {
				t = &digestFinding{Key: f.Key, Title: f.Title, Severity: f.Severity, Label: f.Label}
				tally[f.Key] = t
				order = append(order, f.Key)
			}
			t.Count++
			if !seen[f.Key] {
				seen[f.Key] = true
				t.Days++
			}
		}
		if p := rep.Plan; p != nil {
			d.Plan.Planned++
			if p.WrittenLate {
				d.Plan.WrittenLate++
			}
			for _, c := range p.Checks {
				r, ok := rules[c.Key]
				if !ok {
					r = &digestRule{Key: c.Key, Name: digestRuleNames[c.Key]}
					rules[c.Key] = r
					ruleOrder = append(ruleOrder, c.Key)
				}
				r.Total++
				if c.Held {
					r.Held++
				}
			}
		}
	}
	for _, k := range order {
		d.Coach = append(d.Coach, *tally[k])
	}
	sort.SliceStable(d.Coach, func(i, j int) bool {
		if d.Coach[i].Days != d.Coach[j].Days {
			return d.Coach[i].Days > d.Coach[j].Days
		}
		return d.Coach[i].Count > d.Coach[j].Count
	})
	for _, k := range ruleOrder {
		d.Plan.Rules = append(d.Plan.Rules, *rules[k])
	}

	d.KPIs = digestKPIs(this, prev)
	d.Keep, d.Fix = digestNotes(d)
	return d, nil
}

func fptr(v float64) *float64 { return &v }

// digestKPIs: the headline numbers of the week against the week before.
func digestKPIs(this, prev analyticsSummary) []digestKPI {
	type spec struct {
		key, label, unit string
		get              func(s analyticsSummary) *float64
		higherIsBetter   bool
	}
	specs := []spec{
		{"net", "Net P&L", "money", func(s analyticsSummary) *float64 { return fptr(s.Totals.Net) }, true},
		{"decisions", "Decisions", "count", func(s analyticsSummary) *float64 { return fptr(float64(s.Totals.Trades)) }, false},
		{"winRate", "Win rate", "pct", func(s analyticsSummary) *float64 {
			if s.Totals.Wins+s.Totals.Losses == 0 {
				return nil
			}
			return fptr(s.Totals.WinRate)
		}, true},
		{"profitFactor", "Profit factor", "ratio", func(s analyticsSummary) *float64 { return s.Totals.ProfitFactor }, true},
		{"avgR", "Avg R", "r", func(s analyticsSummary) *float64 {
			if s.R.PlannedTrades == 0 {
				return nil
			}
			return fptr(s.R.AvgR)
		}, true},
		{"exitKept", "Exit kept", "pct", func(s analyticsSummary) *float64 {
			if s.Excursion.Trades == 0 {
				return nil
			}
			return fptr(s.Excursion.ExitEfficiency)
		}, true},
	}
	format := func(v *float64, unit string) string {
		if v == nil {
			return "—"
		}
		switch unit {
		case "money":
			return fmt.Sprintf("%+.2f", *v)
		case "count":
			return fmt.Sprintf("%.0f", *v)
		case "pct":
			return fmt.Sprintf("%.0f%%", *v*100)
		case "r":
			return fmt.Sprintf("%+.2fR", *v)
		}
		if math.IsInf(*v, 1) {
			return "∞"
		}
		return fmt.Sprintf("%.2f", *v)
	}
	out := make([]digestKPI, 0, len(specs))
	noPrev := prev.Totals.Trades == 0
	for _, sp := range specs {
		k := digestKPI{Key: sp.key, Label: sp.label, Value: sp.get(this)}
		if !noPrev {
			k.Prev = sp.get(prev)
		}
		k.Display = format(k.Value, sp.unit)
		k.PrevShown = format(k.Prev, sp.unit)
		if k.Value != nil && k.Prev != nil && !math.IsInf(*k.Value, 0) && !math.IsInf(*k.Prev, 0) {
			delta := *k.Value - *k.Prev
			switch sp.unit {
			case "money":
				k.Delta = fmt.Sprintf("%+.2f", delta)
			case "count":
				k.Delta = fmt.Sprintf("%+.0f", delta)
			case "pct":
				k.Delta = fmt.Sprintf("%+.0f pts", delta*100)
			case "r":
				k.Delta = fmt.Sprintf("%+.2fR", delta)
			default:
				k.Delta = fmt.Sprintf("%+.2f", delta)
			}
			if sp.key != "decisions" && delta != 0 {
				if (delta > 0) == sp.higherIsBetter {
					k.DeltaSign = "pnl-pos"
				} else {
					k.DeltaSign = "pnl-neg"
				}
			}
		}
		out = append(out, k)
	}
	return out
}

// digestNotes: what to keep and what to fix, one sentence each, from the
// numbers (nothing is said that the numbers do not show).
func digestNotes(d *weeklyDigest) (keep, fix []string) {
	keep, fix = []string{}, []string{}
	if d.This.Totals.Trades == 0 {
		return keep, fix
	}
	bucket := func(name string) []bucketOut { return d.This.Breakdowns[name] }
	// Keep: the strategy that carried the week, the best hour, better exits.
	var best *bucketOut
	for _, b := range bucket("byStrategy") {
		b := b
		if b.Key != "(no strategy)" && b.Net > 0 && (best == nil || b.Net > best.Net) {
			best = &b
		}
	}
	if best != nil {
		keep = append(keep, fmt.Sprintf("%s carried the week: %+.2f over %d decision(s).", best.Key, best.Net, best.Trades))
	}
	var hour *bucketOut
	for _, b := range bucket("byHour") {
		b := b
		if b.Net > 0 && (hour == nil || b.Net > hour.Net) {
			hour = &b
		}
	}
	if hour != nil {
		keep = append(keep, fmt.Sprintf("Entries in the %s hour made %+.2f over %d decision(s).", hour.Key, hour.Net, hour.Trades))
	}
	if d.This.Excursion.Trades > 0 && d.Prev.Excursion.Trades > 0 && d.This.Excursion.ExitEfficiency > d.Prev.Excursion.ExitEfficiency+0.05 {
		keep = append(keep, fmt.Sprintf("Exits kept %.0f%% of the move, up from %.0f%%.", d.This.Excursion.ExitEfficiency*100, d.Prev.Excursion.ExitEfficiency*100))
	}
	// Fix: the costliest mistake, the finding that kept coming back, the
	// plan rule that held least (or the missing plans).
	var worst *bucketOut
	for _, b := range bucket("byMistake") {
		b := b
		if b.Key != "(none logged)" && b.Net < 0 && (worst == nil || b.Net < worst.Net) {
			worst = &b
		}
	}
	if worst != nil {
		fix = append(fix, fmt.Sprintf("%s cost %.2f over %d decision(s).", worst.Key, worst.Net, worst.Trades))
	}
	for _, f := range d.Coach {
		if f.Severity == "warn" && f.Days >= 2 {
			fix = append(fix, fmt.Sprintf("%s came up on %d of %d trading days.", f.Title, f.Days, d.TradingDays))
			break
		}
	}
	switch {
	case d.Plan.Planned == 0:
		fix = append(fix, fmt.Sprintf("No day plan on any of the %d trading days.", d.TradingDays))
	case d.Plan.Planned < d.TradingDays:
		fix = append(fix, fmt.Sprintf("No day plan on %d of %d trading days.", d.TradingDays-d.Plan.Planned, d.TradingDays))
	}
	var weak *digestRule
	for i := range d.Plan.Rules {
		r := &d.Plan.Rules[i]
		if r.Held < r.Total && (weak == nil || float64(r.Held)/float64(r.Total) < float64(weak.Held)/float64(weak.Total)) {
			weak = r
		}
	}
	if weak != nil {
		fix = append(fix, fmt.Sprintf("The plan's %s held on %d of %d planned days.", weak.Name, weak.Held, weak.Total))
	}
	return keep, fix
}

// DigestPage renders GET /digest?week=YYYY-Www.
func (h *Handlers) DigestPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	monday, weekID, err := h.digestWeek(ctx, r.URL.Query().Get("week"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d, err := h.buildDigest(ctx, monday, weekID)
	if err != nil {
		h.serverError(w, fmt.Errorf("digest: %w", err))
		return
	}
	tz := h.localTZ()
	prevY, prevW := monday.AddDate(0, 0, -7).ISOWeek()
	next := monday.AddDate(0, 0, 7)
	nextY, nextW := next.ISOWeek()
	_, wk := monday.ISOWeek()
	h.renderPage(w, r, "digest.html", map[string]any{
		"NavActive":  "digest",
		"BackHref":   validBackHref(r),
		"D":          d,
		"WeekNum":    wk,
		"RangeLabel": fmt.Sprintf("%s – %s", monday.Format("Mon 2 Jan"), monday.AddDate(0, 0, 6).Format("Mon 2 Jan 2006")),
		"PrevWeek":   fmt.Sprintf("%d-W%02d", prevY, prevW),
		"NextWeek":   fmt.Sprintf("%d-W%02d", nextY, nextW),
		"HasNext":    !next.After(todayLocal(tz)),
		// Raw: the hrefs carry a literal "?back=", so html/template
		// query-escapes it (see the back-link rule in CLAUDE.md).
		"Self": "/digest?week=" + weekID,
	})
}

// DigestMarkdown serves GET /digest.md?week= as a download (to mail or post).
func (h *Handlers) DigestMarkdown(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	monday, weekID, err := h.digestWeek(ctx, r.URL.Query().Get("week"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d, err := h.buildDigest(ctx, monday, weekID)
	if err != nil {
		h.serverError(w, fmt.Errorf("digest: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="digest-`+weekID+`.md"`)
	_, _ = w.Write([]byte(digestMarkdown(d)))
}

func digestMarkdown(d *weeklyDigest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Weekly digest · %s (%s to %s)\n\n", d.Week, d.From, d.To)
	if d.This.Totals.Trades == 0 {
		b.WriteString("_No closed trades this week._\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d decision(s) over %d trading day(s), counted per decision (a copy set once).\n\n", d.This.Totals.Trades, d.TradingDays)
	b.WriteString("| | This week | Previous | Change |\n|---|---:|---:|---:|\n")
	for _, k := range d.KPIs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", k.Label, k.Display, k.PrevShown, k.Delta)
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	}
	list("Keep", d.Keep)
	list("Fix", d.Fix)
	if d.Best != nil || d.Worst != nil {
		b.WriteString("\n## Best and worst decision\n\n")
		if d.Best != nil {
			fmt.Fprintf(&b, "- Best: %s %s %s on %d account(s), %+.2f\n", d.Best.Day, d.Best.Symbol, d.Best.Direction, d.Best.Accounts, d.Best.Net)
		}
		if d.Worst != nil {
			fmt.Fprintf(&b, "- Worst: %s %s %s on %d account(s), %+.2f\n", d.Worst.Day, d.Worst.Symbol, d.Worst.Direction, d.Worst.Accounts, d.Worst.Net)
		}
	}
	if len(d.Coach) > 0 {
		b.WriteString("\n## Coach findings\n\n| Finding | Days | Times |\n|---|---:|---:|\n")
		for _, f := range d.Coach {
			fmt.Fprintf(&b, "| %s | %d | %d |\n", f.Title, f.Days, f.Count)
		}
	}
	fmt.Fprintf(&b, "\n## Day plans\n\nPlanned %d of %d trading days", d.Plan.Planned, d.Plan.TradingDays)
	if d.Plan.WrittenLate > 0 {
		fmt.Fprintf(&b, " (%d written after the first trade)", d.Plan.WrittenLate)
	}
	b.WriteString(".\n")
	for _, r := range d.Plan.Rules {
		fmt.Fprintf(&b, "- %s held on %d of %d days\n", r.Name, r.Held, r.Total)
	}
	return b.String()
}

// APIDigest: GET /api/v1/digest?week=YYYY-Www
func (h *Handlers) APIDigest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	monday, weekID, err := h.digestWeek(ctx, r.URL.Query().Get("week"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	d, err := h.buildDigest(ctx, monday, weekID)
	if err != nil {
		h.logger().Warn("api digest", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not build the digest")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handlers) mcpGetWeeklyDigest(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	monday, weekID, err := h.digestWeek(ctx, req.GetString("week", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	d, err := h.buildDigest(ctx, monday, weekID)
	if err != nil {
		return mcpgo.NewToolResultError("could not build the digest"), nil
	}
	return mcpJSON(d)
}

// digestForAI is the compact digest for a week entry's AI context.
func digestForAI(d *weeklyDigest) map[string]any {
	kpis := map[string]any{}
	for _, k := range d.KPIs {
		kpis[k.Key] = map[string]any{"value": k.Value, "previous": k.Prev}
	}
	return map[string]any{
		"week": d.Week, "tradingDays": d.TradingDays, "kpis": kpis,
		"keep": d.Keep, "fix": d.Fix, "coach": d.Coach, "plan": d.Plan,
		"bestDecision": d.Best, "worstDecision": d.Worst,
		"byStrategy": d.This.Breakdowns["byStrategy"], "byMistake": d.This.Breakdowns["byMistake"],
	}
}
