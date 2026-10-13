package handlers

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/levels"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ---- the stop/target lab (/analytics/lab) ----------------------------------------
//
// Past decisions (one leg per copy set) replayed on the cached 1m bars after
// their entries under a grid of fixed stops and targets in points - the axes
// from the trader's own heat and best-seen distributions - with an optional
// time stop: expectancy per decision with a 90% bootstrap interval, win rate,
// and how many were decided inside one bar or never reached either level;
// beside the trader's own exits over the same decisions, and the MAE/MFE
// scatter of winners and losers (Sweeney). "Explain" has the model read the
// grid and recommend a rule with its caveats (h.AITasks, ai_outputs kind lab).

const (
	aiKindLab = "lab"
	// labDays is the default look-back; labMaxDecisions caps the replay
	// (the newest); labWindow is how long after an entry the replay looks.
	labDays         = 90
	labMaxDecisions = 1500
	labWindow       = 4 * time.Hour
	// labMinCell is the sample a cell needs to be called the best.
	labMinCell = 20
)

// labFilters is the lab's selection, read from the query.
type labFilters struct {
	Strategy  string
	Root      string // folded ("ES" covers MES)
	Direction string // "" | long | short
	Session   string // "" | a sessionBucket name
	From, To  string // YYYY-MM-DD in the trading timezone
	TimeStop  int    // minutes; 0 = none
	Stops     []float64
	Targets   []float64
}

func parseLabFilters(r *http.Request, tz *time.Location) labFilters {
	q := r.URL.Query()
	f := labFilters{Strategy: strings.TrimSpace(q.Get("strategy")), Root: strings.ToUpper(strings.TrimSpace(q.Get("root"))),
		Session: q.Get("session"), From: q.Get("from"), To: q.Get("to")}
	switch q.Get("direction") {
	case "long", "short":
		f.Direction = q.Get("direction")
	}
	if n, err := strconv.Atoi(q.Get("timestop")); err == nil && n > 0 && n <= 24*60 {
		f.TimeStop = n
	}
	parse := func(s string) []float64 {
		var out []float64
		for _, p := range strings.Split(s, ",") {
			if v, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err == nil && v >= 0 && v < 10000 && len(out) < 8 {
				out = append(out, v)
			}
		}
		sort.Float64s(out)
		return out
	}
	f.Stops, f.Targets = parse(q.Get("stops")), parse(q.Get("targets"))
	if _, ok := parseLocalDay(f.From, tz); !ok {
		f.From = time.Now().In(tz).AddDate(0, 0, -labDays).Format("2006-01-02")
	}
	if _, ok := parseLocalDay(f.To, tz); !ok {
		f.To = time.Now().In(tz).Format("2006-01-02")
	}
	return f
}

func (f labFilters) query() url.Values {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("strategy", f.Strategy)
	set("root", f.Root)
	set("direction", f.Direction)
	set("session", f.Session)
	set("from", f.From)
	set("to", f.To)
	if f.TimeStop > 0 {
		q.Set("timestop", strconv.Itoa(f.TimeStop))
	}
	join := func(xs []float64) string {
		parts := make([]string, len(xs))
		for i, x := range xs {
			parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
		}
		return strings.Join(parts, ",")
	}
	set("stops", join(f.Stops))
	set("targets", join(f.Targets))
	return q
}

// key names this selection in the task tracker and ai_outputs.
func (f labFilters) key() string {
	sum := sha1.Sum([]byte(f.query().Encode()))
	return hex.EncodeToString(sum[:8])
}

// labScatterPt is one decision in the MAE/MFE scatter (SVG coordinates).
type labScatterPt struct {
	X, Y float64
	Win  bool
}

// labResult is everything the page shows.
type labResult struct {
	Decisions int
	Skipped   int // decisions without cached bars after the entry
	Stops     []float64
	Targets   []float64
	Grid      [][]levels.Cell
	Baseline  levels.Baseline
	BestStop  float64
	BestTgt   float64
	BestFound bool
	PointUSD  float64 // money per point of one contract of the filtered root (0 when mixed)
	Scatter   []labScatterPt
	HeatMax   float64 // scatter axes, points
	BestMax   float64
	Roots     []string // the folded roots in the period, for the filter
}

// labDecisions loads the selection's decisions (one leg per set, newest
// first, capped) with the bars after each entry, plus each one's heat and
// best seen in points.
func (h *Handlers) labDecisions(ctx context.Context, f labFilters) ([]levels.LabDecision, []labScatterPt, []string, int, error) {
	tz := h.localTZ()
	from, _ := parseLocalDay(f.From, tz)
	to, _ := parseLocalDay(f.To, tz)
	fromU, toU := from.UTC(), to.AddDate(0, 0, 1).UTC()
	all, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, &fromU, &toU)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		chicago = tz
	}
	rootSet := map[string]bool{}
	seenSet := map[int64]bool{}
	var pick []analyticsTrade
	for _, t := range all {
		root := strings.TrimSuffix(copyset.Key(t.SymbolRoot), "=F")
		rootSet[root] = true
		if t.SetID != nil {
			if seenSet[*t.SetID] {
				continue
			}
			seenSet[*t.SetID] = true
		}
		if (f.Strategy != "" && t.Strategy != f.Strategy) || (f.Root != "" && root != f.Root) ||
			(f.Direction != "" && t.Direction != f.Direction) || (f.Session != "" && sessionBucket(t.EntryTime, chicago) != f.Session) {
			continue
		}
		pick = append(pick, t)
	}
	roots := make([]string, 0, len(rootSet))
	for r := range rootSet {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	sort.Slice(pick, func(i, j int) bool { return pick[i].EntryTime.After(pick[j].EntryTime) })
	if len(pick) > labMaxDecisions {
		pick = pick[:labMaxDecisions]
	}

	// One read of the cached bars per vendor symbol over the whole span.
	type span struct{ from, to time.Time }
	spans := map[string]*span{}
	for _, t := range pick {
		sym, ok := marketdata.YahooSymbol(t.SymbolRoot)
		if !ok {
			continue
		}
		sp := spans[sym]
		if sp == nil {
			sp = &span{from: t.EntryTime, to: t.EntryTime}
			spans[sym] = sp
		}
		if t.EntryTime.Before(sp.from) {
			sp.from = t.EntryTime
		}
		if t.EntryTime.After(sp.to) {
			sp.to = t.EntryTime
		}
	}
	barsBy := map[string][]marketdata.Bar{}
	if h.MarketData != nil {
		for sym, sp := range spans {
			if b, _, _, err := h.MarketData.BarsForWindow(ctx, sym, sp.from.Add(-time.Minute), sp.to.Add(labWindow)); err == nil {
				barsBy[sym] = b
			}
		}
	}
	pv := map[string]float64{}
	pointValue := func(root string) float64 {
		if v, ok := pv[root]; ok {
			return v
		}
		v := 0.0
		if inst, err := h.Queries.GetInstrument(ctx, root); err == nil {
			if p, ok := positions.PointValue(inst); ok {
				v = p
			}
		}
		pv[root] = v
		return v
	}
	var ds []levels.LabDecision
	var pts []labScatterPt
	skipped := 0
	for _, t := range pick {
		sym, _ := marketdata.YahooSymbol(t.SymbolRoot)
		bars := barsBy[sym]
		en := t.EntryTime.Unix()
		i := sort.Search(len(bars), func(i int) bool { return bars[i].Time > en-60 })
		j := sort.Search(len(bars), func(i int) bool { return bars[i].Time >= en+int64(labWindow/time.Second) })
		sign := 1.0
		if t.Direction != "long" {
			sign = -1
		}
		actual := 0.0
		if t.ExitPrice != nil {
			actual = math.Round((*t.ExitPrice-t.EntryPrice)*sign*100) / 100
		}
		after := bars[i:j]
		if len(after) < 2 {
			skipped++
			continue
		}
		ds = append(ds, levels.LabDecision{Direction: t.Direction, Entry: t.EntryPrice, EntryTime: en, After: after, Actual: actual})
		if v := pointValue(t.SymbolRoot); v > 0 && t.Qty > 0 && t.MaePnl != nil && t.MfePnl != nil {
			pts = append(pts, labScatterPt{X: *t.MaePnl / t.Qty / v, Y: *t.MfePnl / t.Qty / v, Win: t.NetPnl != nil && *t.NetPnl > 0})
		}
	}
	return ds, pts, roots, skipped, nil
}

// runLab builds the page's result for a selection.
func (h *Handlers) runLab(ctx context.Context, f labFilters) (*labResult, error) {
	ds, pts, roots, skipped, err := h.labDecisions(ctx, f)
	if err != nil {
		return nil, err
	}
	res := &labResult{Decisions: len(ds), Skipped: skipped, Roots: roots}
	if len(ds) == 0 {
		return res, nil
	}
	tick := 0.25
	if f.Root != "" {
		if inst, err := h.Queries.GetInstrument(ctx, f.Root); err == nil {
			if inst.TickSize != nil && *inst.TickSize > 0 {
				tick = *inst.TickSize
			}
			if p, ok := positions.PointValue(inst); ok {
				res.PointUSD = p
			}
		}
	}
	// The axes: from the winners' heat (stops) and best seen (targets).
	var heat, best []float64
	for _, p := range pts {
		if p.Win {
			heat, best = append(heat, p.X), append(best, p.Y)
		}
	}
	res.Stops, res.Targets = f.Stops, f.Targets
	if len(res.Stops) == 0 {
		res.Stops = levels.Spread(heat, 0.5, 0.95, 5, tick)
	}
	if len(res.Targets) == 0 {
		res.Targets = append(levels.Spread(best, 0.3, 0.9, 5, tick), 0)
	}
	if len(res.Stops) == 0 { // no measured excursions: a plain ladder
		res.Stops = []float64{2, 4, 6, 8}
	}
	if len(res.Targets) == 0 {
		res.Targets = []float64{2, 4, 8, 0}
	}
	res.Grid = levels.Grid(ds, res.Stops, res.Targets, f.TimeStop)
	res.Baseline = levels.Actuals(ds)
	bestAvg := math.Inf(-1)
	for _, row := range res.Grid {
		for _, c := range row {
			if c.N >= labMinCell && c.Avg > bestAvg {
				bestAvg, res.BestStop, res.BestTgt, res.BestFound = c.Avg, c.Stop, c.Target, true
			}
		}
	}
	// The scatter, scaled to its 95th percentiles.
	xs, ys := []float64{}, []float64{}
	for _, p := range pts {
		xs, ys = append(xs, p.X), append(ys, p.Y)
	}
	if len(xs) > 0 {
		res.HeatMax = math.Max(levels.Spread(xs, 0.95, 0.95, 1, 0.01)[0], 0.5)
		res.BestMax = math.Max(levels.Spread(ys, 0.95, 0.95, 1, 0.01)[0], 0.5)
		for _, p := range pts {
			if len(res.Scatter) == 2000 {
				break
			}
			res.Scatter = append(res.Scatter, labScatterPt{
				X: 40 + math.Min(p.X/res.HeatMax, 1)*500, Y: 290 - math.Min(p.Y/res.BestMax, 1)*270, Win: p.Win,
			})
		}
	}
	return res, nil
}

// LabPage: GET /analytics/lab.
func (h *Handlers) LabPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	f := parseLabFilters(r, tz)
	res, err := h.runLab(ctx, f)
	if err != nil {
		h.serverError(w, fmt.Errorf("lab: %w", err))
		return
	}
	strategies, _ := h.Queries.ListStrategies(ctx, userID(ctx))
	key := f.key()
	uid := userID(ctx)
	data := map[string]any{
		"NavActive": "analytics", "F": f, "Res": res, "Strategies": strategies,
		"Sessions": []string{"before the open", "first hour", "morning", "midday", "afternoon", "after the close"},
		"QS":       template.URL(f.query().Encode()), "Key": key,
		"Err":            r.URL.Query().Get("err"),
		"ExplainPending": h.AITasks.Pending(aiTaskKey(uid, "lab", key)),
		"ExplainError":   h.AITasks.TakeError(uid, aiTaskKey(uid, "lab", key)),
	}
	if out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindLab, Ref: key}); err == nil {
		data["Explain"] = template.HTML(renderMarkdown(out.Body))
		data["ExplainModel"], data["ExplainAt"] = out.Model, out.CreatedAt
	}
	h.renderPage(w, r, "lab.html", data)
}

const aiLabPrompt = `You read a stop/target lab for a futures day trader. The app replayed the trader's past decisions (one per copy set) on 1-minute bars after each entry under a grid of fixed stops and targets in points from the entry (target 0 = no target: the stop, the time stop or the end of a 4-hour window decides), with an optional time stop. Per cell: n decisions, the average result in points per decision with its 90% bootstrap interval, the win rate, how many a single bar decided both ways (counted as the stop) and how many reached neither level. The baseline is what the trader's own exits made over the same decisions. The user message is JSON with the selection, the grid and the baseline.

Write a short Markdown reading (bullets with bold labels, no headings):
- **Best rule:** the stop/target (and time stop) with the best expectancy that is not a lone outlier - prefer a cell whose neighbours also do well; say how it compares with the baseline in points per decision.
- **Confidence:** sample size, whether its interval overlaps the baseline's and its neighbours', and that the best cell of any grid is optimistic (picked after the fact).
- **What it changes:** in plain words, how trading that rule would differ from what the trader does now (stops wider or tighter, letting winners run or taking profits sooner).
- **Next step:** one concrete thing to try or check (for example, test it forward for N trades, or narrow the selection).
Never invent numbers that are not in the JSON.`

// LabExplain: POST /analytics/lab/explain?... - have the model read the grid
// of this selection, in the background.
func (h *Handlers) LabExplain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.AITasks == nil {
		http.NotFound(w, r)
		return
	}
	f := parseLabFilters(r, h.localTZ())
	back := "/analytics/lab?" + f.query().Encode()
	if !h.aiAvailable(ctx) {
		http.Redirect(w, r, back+"&err="+url.QueryEscape(errNoAIProvider.Error()), http.StatusSeeOther)
		return
	}
	h.startTask(ctx, aiTaskKey(userID(ctx), "lab", f.key()), func(ctx context.Context) error { return h.explainLab(ctx, f) })
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (h *Handlers) explainLab(ctx context.Context, f labFilters) error {
	client, model, err := h.aiFor(ctx)
	if err != nil {
		return err
	}
	res, err := h.runLab(ctx, f)
	if err != nil {
		return err
	}
	if res.Decisions == 0 {
		return errors.New("no decisions with cached bars in this selection")
	}
	payload, _ := jsonIndent(map[string]any{
		"selection": map[string]any{"strategy": f.Strategy, "instrument": f.Root, "direction": f.Direction, "session": f.Session,
			"from": f.From, "to": f.To, "timeStopMinutes": f.TimeStop},
		"decisions": res.Decisions, "stops": res.Stops, "targets": res.Targets, "grid": res.Grid, "baseline": res.Baseline,
		"usdPerPoint": res.PointUSD,
	})
	out, err := client.Generate(ctx, aiLabPrompt+aiLanguageLine(h.userAILanguage(ctx)), payload)
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}
	body := strings.TrimSpace(out)
	if body == "" {
		return errors.New("the model returned an empty reading")
	}
	return h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{
		UserID: userID(ctx), Kind: aiKindLab, Ref: f.key(), Body: body, Model: model.Label(aiWordsFor("")),
	})
}
