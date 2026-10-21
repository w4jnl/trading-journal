package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/w4jnl/trading-journal/internal/aimethod"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/coach"
	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/levelmap"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- the pre-market plan draft ----------------------------------------------------
//
// For today or a later day the trader can ask for a draft of the plan: the
// app gathers the evidence (their norms from the coach, what an account
// usually loses on a losing day, the last trading days' coach findings and
// plan checks, the open lessons, the gamma map known now) and the model
// drafts the decision cap, the stop time, the max loss, the levels and the
// focus notes from it - never the bias, which stays the trader's call. The
// draft (ai_outputs, kind plan_draft, ref = the day) prefills the plan form
// for the trader to change and save; saving the plan or discarding drops it.

const (
	aiKindPlanDraft = "plan_draft"

	planLossWindowDays   = 60             // calendar days of account-days behind the usual losing day
	planRecentDays       = 5              // trading days of coach findings handed along
	planRootsDays        = 30             // the instruments traded this recently get gamma levels
	planGammaMaxAge      = 24 * time.Hour // the gamma map may be the last session's
	planGammaBriefLevels = 3              // the gamma levels sent when gamma is not among the trader's focus
	planMaxLevelLines    = 12
)

// planLossDays is what an account loses on a losing day (USD, positive).
type planLossDays struct {
	Days  int      `json:"days"` // losing account-days behind the numbers
	P50   *float64 `json:"p50"`
	P80   *float64 `json:"p80"`
	Worst float64  `json:"worst"`
}

// planRecentDay is one of the last trading days as the draft reads it.
type planRecentDay struct {
	Day       string   `json:"day"`
	Net       float64  `json:"net"` // all accounts
	Decisions int      `json:"decisions"`
	Findings  []string `json:"coachFindings,omitempty"`
	Plan      []string `json:"planChecks,omitempty"`
}

// planGamma is a market's gamma map, in futures prices.
type planGamma struct {
	Market string       `json:"market"`
	AsOf   time.Time    `json:"asOf"`
	Levels []gammaLevel `json:"levels"`
}

// planEvidence is the user turn of a plan draft.
type planEvidence struct {
	Day         string           `json:"day"`
	Timezone    string           `json:"timezone"`
	Norms       *coach.Baseline  `json:"norms,omitempty"`
	LossDays    *planLossDays    `json:"accountLossDays,omitempty"`
	RecentDays  []planRecentDay  `json:"recentDays,omitempty"`
	OpenLessons []lessonGroup    `json:"openLessons,omitempty"`
	Gamma       []planGamma      `json:"gammaLevels,omitempty"`
	DayMap      []levelmap.Brief `json:"dayMap,omitempty"`
	CurrentPlan *dayplan.Plan    `json:"currentPlan,omitempty"`
}

// thin reports evidence too thin to draft from.
func (ev planEvidence) thin() bool {
	return ev.Norms == nil && ev.LossDays == nil && len(ev.RecentDays) == 0 && len(ev.OpenLessons) == 0 && len(ev.Gamma) == 0 && len(ev.DayMap) == 0
}

// planEvidenceFor gathers what a draft for day (local midnight) reads.
func (h *Handlers) planEvidenceFor(ctx context.Context, day time.Time) planEvidence {
	tz := h.localTZ()
	key := day.Format("2006-01-02")
	ev := planEvidence{Day: key, Timezone: tz.String(), CurrentPlan: h.dayPlan(ctx, key)}
	if rep := h.coachReport(ctx, day); rep != nil && rep.Baseline.Days > 0 {
		b := rep.Baseline
		ev.Norms = &b
	}

	// Account-days before the day: the losing ones, and the trading days.
	rows, err := h.rq().ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{
		UserID: userID(ctx), StatDate: day.AddDate(0, 0, -planLossWindowDays), StatDate_2: day,
	})
	if err != nil {
		h.logger().Warn("plan draft daily stats", "err", err)
	}
	var losses []float64
	worst := 0.0
	net := map[string]float64{}
	for _, r := range rows {
		y, m, d := r.StatDate.In(tz).Date()
		k := time.Date(y, m, d, 0, 0, 0, 0, tz).Format("2006-01-02")
		if k >= key {
			continue
		}
		net[k] += r.NetPnl
		if r.NetPnl < 0 {
			losses = append(losses, -r.NetPnl)
			worst = math.Max(worst, -r.NetPnl)
		}
	}
	if len(losses) > 0 {
		p := percentiles(losses)
		ev.LossDays = &planLossDays{Days: len(losses), P50: p.P50, P80: p.P80, Worst: math.Round(worst*100) / 100}
	}
	days := make([]string, 0, len(net))
	for k := range net {
		days = append(days, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	if len(days) > planRecentDays {
		days = days[:planRecentDays]
	}
	for _, k := range days {
		d, _ := time.ParseInLocation("2006-01-02", k, tz)
		rd := planRecentDay{Day: k, Net: math.Round(net[k]*100) / 100}
		if rep := h.coachReport(ctx, d); rep != nil {
			rd.Decisions = rep.Decisions
			for _, f := range rep.Findings {
				rd.Findings = append(rd.Findings, f.Title+": "+f.Detail)
			}
			if rep.Plan != nil {
				for _, c := range rep.Plan.Checks {
					state := "broken"
					if c.Held {
						state = "held"
					}
					rd.Plan = append(rd.Plan, c.Rule+" -> "+c.Actual+" ("+state+")")
				}
			}
		}
		ev.RecentDays = append(ev.RecentDays, rd)
	}

	ev.OpenLessons = h.openLessons(ctx, day.AddDate(0, 0, -openLessonDays).Format("2006-01-02"))
	if len(ev.OpenLessons) > 5 {
		ev.OpenLessons = ev.OpenLessons[:5]
	}
	ev.Gamma = h.planGamma(ctx, day)
	ev.DayMap = h.planLevels(ctx, day)
	return ev
}

// planGamma is the gamma map of each market the trader traded lately, as
// known now (or at the day's start when it lies in the past).
func (h *Handlers) planGamma(ctx context.Context, day time.Time) []planGamma {
	if h.GEX == nil {
		return nil
	}
	rows, err := h.rdb().QueryContext(ctx, `SELECT DISTINCT t.symbol_root FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ? AND t.entry_time >= ?`, userID(ctx), day.AddDate(0, 0, -planRootsDays).UTC())
	if err != nil {
		h.logger().Warn("plan draft roots", "err", err)
		return nil
	}
	defer rows.Close()
	seen := map[string]bool{}
	var markets []string
	for rows.Next() {
		var root string
		if rows.Scan(&root) != nil {
			continue
		}
		if m, ok := gexMarket(root); ok && !seen[m] {
			seen[m] = true
			markets = append(markets, m)
		}
	}
	sort.Strings(markets)
	at := time.Now()
	if day.AddDate(0, 0, 1).Before(at) {
		at = day
	}
	brief := h.userAIProfile(ctx).Plan().GammaBrief
	var out []planGamma
	for _, m := range markets {
		if lv, asOf := h.gammaLevels(ctx, m, at, planGammaMaxAge); len(lv) > 0 {
			if brief && len(lv) > planGammaBriefLevels {
				lv = lv[:planGammaBriefLevels] // a trader who weighs other evidence gets the walls, not the map
			}
			out = append(out, planGamma{Market: m, AsOf: asOf, Levels: lv})
		}
	}
	return out
}

// planLevels is the day map of each market the Levels page covers (traded
// lately, pinned, or fed by a chart), briefed for the model: the strongest
// zones, the expected range and the day type.
func (h *Handlers) planLevels(ctx context.Context, day time.Time) []levelmap.Brief {
	dayStr := day.Format("2006-01-02")
	var out []levelmap.Brief
	for _, market := range h.levelsMarkets(ctx, dayStr) {
		m, err := h.levelMap(ctx, market, dayStr, time.Now())
		if err != nil || m == nil || len(m.Zones) == 0 {
			continue
		}
		out = append(out, m.Brief(0, 8, 2))
	}
	return out
}

// gammaLevel is a level of the gamma map, in futures prices.
type gammaLevel struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

// gammaLevels is the market's gamma map known at a moment, from a snapshot
// at most maxAge old (nil without one), and the snapshot's time.
func (h *Handlers) gammaLevels(ctx context.Context, market string, at time.Time, maxAge time.Duration) ([]gammaLevel, time.Time) {
	if h.GEX == nil {
		return nil, time.Time{}
	}
	snap, _, err := h.GEX.CleanSnapshot(ctx, market, at, gexCleanSteps, maxAge)
	if err != nil || snap == nil || at.Sub(snap.AsOf) > maxAge {
		return nil, time.Time{}
	}
	var out []gammaLevel
	add := func(name string, p *float64) {
		if p != nil && *p > 0 {
			out = append(out, gammaLevel{Name: name, Price: *p})
		}
	}
	add("gamma flip", snap.Flip.Futures)
	add("intraday gamma flip", snap.FlipIntraday.Futures)
	names := map[string]string{"call_wall": "call wall", "put_wall": "put wall", "gamma_1": "gamma magnet 1", "gamma_2": "gamma magnet 2",
		"gamma_3": "gamma magnet 3", "em_1d_min": "expected-move low", "em_1d_max": "expected-move high"}
	for _, l := range snap.Levels {
		if name, ok := names[l.Type]; ok && (l.Bucket == "all" || l.Bucket == "") {
			add(name, l.Futures)
		}
	}
	return out, snap.AsOf
}

const aiPlanDraftPrompt = `You draft a futures day trader's pre-market plan for one session. The user message is JSON: the day; the trader's own norms (median decisions per day, the usual last entry time in the trader's timezone - a copy of one decision across accounts counts once); what one account usually loses on a losing day (USD: median, 80th percentile and worst over the last 60 days); the last trading days with their net result, the coach's findings (overtrading, late entries, re-entries after a loss, size-ups, stops run through) and how that day's plan held; the lessons still open from the trader's reviews (count = how often the same mistake came back); the gamma levels known now (futures prices, asOf = when computed); the day map per market (dayMap: the strongest zones with what meets there and their score, the nearest zones above and below the last price, the expected range from the daily ATR around the session's anchor, and the day type read off the prior day and the overnight with a lean and its reasons; a score ranks, it does not forecast); and the plan already written for the day, if any.

Draft the rules that would have prevented the recent damage while leaving room for the trader's normal day:
- maxTrades: the most decisions for the day - near the usual day, lower after overtrading or a run of losing days.
- stopTime: HH:MM in the trader's timezone, no new entries after it - from the usual last entry, earlier when late entries cost.
- maxLoss: USD per account at which the trader stops for the day - from the usual losing day, never beyond its 80th percentile.
- levels: the prices to watch, one per line as "<market> <price> <what it is>", from the day map's zones (the strongest and the nearest, naming what meets there) and the gamma levels; empty without either. Keep the list short: the four to eight prices that matter, nearest first.
- notes: two to four short lines, each an instruction for the day, built from the open lessons (the most repeated first) and the recent findings.
Use null for a field the evidence does not support. Never set a market bias: that is the trader's call. When the trader already wrote a plan, improve it rather than ignore it.

Answer with ONLY a JSON object:
{"maxTrades": <integer|null>, "stopTime": "<HH:MM>"|null, "maxLoss": <number|null>, "levels": "<lines>", "notes": "<lines>", "reasons": ["...", "..."]}
- reasons: one short sentence per field you set, with the number from the evidence behind it.`

// planDraftData is a stored draft.
type planDraftData struct {
	MaxTrades *int64   `json:"maxTrades,omitempty"`
	StopTime  string   `json:"stopTime,omitempty"`
	MaxLoss   *float64 `json:"maxLoss,omitempty"`
	Levels    string   `json:"levels,omitempty"`
	Notes     string   `json:"notes,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
}

// draftPlan writes the draft of the caller's plan for day into ai_outputs.
func (h *Handlers) draftPlan(ctx context.Context, day string) error {
	client, model, err := h.aiFor(ctx)
	if err != nil {
		return err
	}
	d, err := time.ParseInLocation("2006-01-02", day, h.localTZ())
	if err != nil {
		return err
	}
	ev := h.planEvidenceFor(ctx, d)
	if ev.thin() {
		return errors.New("there is no history to draft a plan from yet: trade a few days first")
	}
	in, err := jsonIndent(ev)
	if err != nil {
		return err
	}
	resp, err := ai.Do(ctx, client, ai.Cached(aiPlanDraftPrompt+aiProfileLine(h.userAIProfile(ctx))+aiLanguageLine(h.userAILanguage(ctx)), in))
	out := resp.Text
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}
	var res struct {
		MaxTrades any      `json:"maxTrades"`
		StopTime  any      `json:"stopTime"`
		MaxLoss   any      `json:"maxLoss"`
		Levels    any      `json:"levels"`
		Notes     any      `json:"notes"`
		Reasons   []string `json:"reasons"`
	}
	if err := ai.DecodeJSON(out, &res); err != nil {
		return errors.New("the model did not answer in the plan format; try again or another model")
	}
	draft := cleanPlanDraft(day, res.MaxTrades, res.StopTime, res.MaxLoss, res.Levels, res.Notes, res.Reasons)
	if draft.MaxTrades == nil && draft.StopTime == "" && draft.MaxLoss == nil && draft.Levels == "" && draft.Notes == "" {
		return errors.New("the model drafted nothing usable; try again or another model")
	}
	data, _ := json.Marshal(draft)
	return h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{
		UserID: userID(ctx), Kind: aiKindPlanDraft, Ref: day, Data: string(data), Model: model.Label(aiWordsFor("")), Method: aimethod.Version,
	})
}

// cleanPlanDraft validates a model's plan fields: a cap of 1-50 decisions,
// a well-formed stop time, a positive loss rounded to the dollar, bounded
// text. A field that does not validate is dropped, not the draft.
func cleanPlanDraft(day string, maxTrades, stopTime, maxLoss, lvls, notes any, reasons []string) planDraftData {
	var d planDraftData
	if n, ok := anyNumber(maxTrades); ok && n >= 1 && n <= 50 {
		v := int64(math.Round(n))
		d.MaxTrades = &v
	}
	if s, ok := stopTime.(string); ok {
		if t, err := time.Parse("15:04", strings.TrimSpace(s)); err == nil {
			d.StopTime = t.Format("15:04")
		}
	}
	if n, ok := anyNumber(maxLoss); ok && n > 0 && n < 1e6 {
		v := math.Round(n)
		d.MaxLoss = &v
	}
	d.Levels = strings.Join(cleanRules(anyLines(lvls), planMaxLevelLines), "\n")
	d.Notes = strings.Join(cleanRules(anyLines(notes), 6), "\n")
	d.Reasons = cleanRules(reasons, 6)
	p := dayplan.Plan{Day: day, MaxTrades: d.MaxTrades, StopTime: d.StopTime, MaxLoss: d.MaxLoss, Levels: d.Levels, Notes: d.Notes}
	if dayplan.Normalize(&p) != nil { // only the text bound is left to fail
		d.Levels, d.Notes = truncateRunes(d.Levels, 1000), truncateRunes(d.Notes, 1000)
	}
	return d
}

// anyNumber reads a JSON number or a numeric string.
func anyNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(x), "$")), 64)
		return f, err == nil
	}
	return 0, false
}

// anyLines reads text as lines: a string (split on newlines) or a list.
func anyLines(v any) []string {
	switch x := v.(type) {
	case string:
		return strings.Split(x, "\n")
	case []any:
		var out []string
		for _, it := range x {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- the plan card's view of it ------------------------------------------------------

// planDraftView is a stored draft as the plan card shows it.
type planDraftView struct {
	planDraftData
	Model     string
	CreatedAt time.Time
}

// planForm is what the plan form shows: the plan, with a draft laid over it
// (a drafted number replaces the plan's, drafted levels and notes go under
// the trader's own).
type planForm struct {
	Bias, MaxTrades, StopTime, MaxLoss, Levels, Notes string
}

func newPlanForm(p *dayplan.Plan, d *planDraftView) planForm {
	var f planForm
	if p != nil {
		f.Bias, f.StopTime, f.Levels, f.Notes = p.Bias, p.StopTime, p.Levels, p.Notes
		if p.MaxTrades != nil {
			f.MaxTrades = strconv.FormatInt(*p.MaxTrades, 10)
		}
		if p.MaxLoss != nil {
			f.MaxLoss = strconv.FormatFloat(*p.MaxLoss, 'g', -1, 64)
		}
	}
	if d == nil {
		return f
	}
	if d.MaxTrades != nil {
		f.MaxTrades = strconv.FormatInt(*d.MaxTrades, 10)
	}
	if d.StopTime != "" {
		f.StopTime = d.StopTime
	}
	if d.MaxLoss != nil {
		f.MaxLoss = strconv.FormatFloat(*d.MaxLoss, 'g', -1, 64)
	}
	f.Levels = appendNewLines(f.Levels, d.Levels)
	f.Notes = appendNewLines(f.Notes, d.Notes)
	return f
}

// appendNewLines adds the lines of more that base does not have yet.
func appendNewLines(base, more string) string {
	have := map[string]bool{}
	for _, l := range strings.Split(base, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	out := strings.TrimRight(base, "\n ")
	for _, l := range strings.Split(more, "\n") {
		if l = strings.TrimSpace(l); l == "" || have[l] {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += l
		have[l] = true
	}
	return out
}

// planCardData is what the day page's plan card needs.
func (h *Handlers) planCardData(ctx context.Context, day, backHref string, outcome any) map[string]any {
	uid := userID(ctx)
	today := time.Now().In(h.localTZ()).Format("2006-01-02")
	plan := h.dayPlan(ctx, day)
	ahead := day >= today
	data := map[string]any{
		"Date": day, "BackHref": backHref, "DayPlan": plan, "PlanOutcome": outcome,
		"PlanOpen": plan == nil && ahead, "PlanAhead": ahead,
	}
	var draft *planDraftView
	if ahead {
		data["PlanLessons"] = h.topOpenLessons(ctx, 3)
		key := aiTaskKey(uid, "plan", day)
		// Built here: hx-get is not a URL attribute to html/template, so a
		// back value would not be query-escaped in the template.
		data["PlanPoll"] = "/ai/tasks/status?k=plan:" + day + "&then=plan:" + day + "&back=" + url.QueryEscape(backHref)
		data["PlanDraftPending"] = h.AITasks.Pending(key)
		data["PlanDraftError"] = h.AITasks.TakeError(uid, key)
		if out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindPlanDraft, Ref: day}); err == nil {
			draft = &planDraftView{Model: out.Model, CreatedAt: out.CreatedAt}
			_ = json.Unmarshal([]byte(out.Data), &draft.planDraftData)
			data["PlanDraft"] = draft
			data["PlanOpen"] = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			h.logger().Warn("plan draft", "day", day, "err", err)
		}
	}
	data["PlanForm"] = newPlanForm(plan, draft)
	return data
}

// ---- routes -----------------------------------------------------------------------------

// planRoute reads the day of a draft request: today or later. "" = done (an
// error was written).
func (h *Handlers) planRoute(w http.ResponseWriter, r *http.Request) string {
	day := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return ""
	}
	if day < time.Now().In(h.localTZ()).Format("2006-01-02") {
		http.Error(w, "a plan is drafted for today or a later day", http.StatusBadRequest)
		return ""
	}
	return day
}

// planBack is the day page the plan routes return to.
func planBack(r *http.Request, day, errMsg string) string {
	q := url.Values{}
	if back := r.FormValue("back"); strings.HasPrefix(back, "/") && !strings.HasPrefix(back, "//") {
		q.Set("back", back)
	}
	if errMsg != "" {
		q.Set("plan_err", errMsg)
	}
	target := "/journal/" + day
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	return target + "#day-plan"
}

// DayPlanDraft: POST /journal/plan/{date}/draft - draft the day's plan in
// the background.
func (h *Handlers) DayPlanDraft(w http.ResponseWriter, r *http.Request) {
	if h.AITasks == nil {
		http.NotFound(w, r)
		return
	}
	day := h.planRoute(w, r)
	if day == "" {
		return
	}
	ctx := r.Context()
	if !h.aiAvailable(ctx) {
		http.Redirect(w, r, planBack(r, day, errNoAIProvider.Error()), http.StatusSeeOther)
		return
	}
	h.startTask(ctx, aiTaskKey(userID(ctx), "plan", day), func(ctx context.Context) error { return h.draftPlan(ctx, day) })
	http.Redirect(w, r, planBack(r, day, ""), http.StatusSeeOther)
}

// DayPlanDraftDiscard: POST /journal/plan/{date}/draft/discard.
func (h *Handlers) DayPlanDraftDiscard(w http.ResponseWriter, r *http.Request) {
	day := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	h.dropPlanDraft(ctx, day)
	http.Redirect(w, r, planBack(r, day, ""), http.StatusSeeOther)
}

// dropPlanDraft forgets the day's draft (saved or discarded).
func (h *Handlers) dropPlanDraft(ctx context.Context, day string) {
	if err := h.Queries.DeleteAIOutput(ctx, models.DeleteAIOutputParams{UserID: userID(ctx), Kind: aiKindPlanDraft, Ref: day}); err != nil {
		h.logger().Warn("drop plan draft", "day", day, "err", err)
	}
	h.LiveData.Bump(userID(ctx))
}
