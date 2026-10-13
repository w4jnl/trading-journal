package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/excursion"
	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ---- strategy co-author ----------------------------------------------------------
//
// Two AI helpers in the Library, both run in the background (h.AITasks) and
// stored in ai_outputs until the trader applies or discards them:
//
//   - Draft from my trades: the playbook and a checkable checklist for one
//     strategy, written from the decisions tagged with it - the evidence the
//     app measured over them plus the trader's own notes - and, when a
//     playbook exists, where what was written differs from what was done.
//   - Find strategies: untagged decisions grouped by instrument, time of day
//     and holding time; the model names each group, judges it and drafts
//     rules, and the trader can turn one into a strategy.
//
// The numbers are always the app's (measureDecisions); the model only writes.

const (
	aiKindStrategyDraft = "strategy_draft"
	aiKindDiscovery     = "strategy_discovery"

	// aiDiscoveryDays is how far back "find strategies" looks; groups need at
	// least aiDiscoveryMin decisions, and the aiDiscoveryTop largest are kept.
	aiDiscoveryDays = 180
	aiDiscoveryMin  = 10
	aiDiscoveryTop  = 8
	// aiStrategyNotes caps the trader's notes passed along (newest first).
	aiStrategyNotes    = 12
	aiStrategyNoteRune = 400
)

// aiTaskKey is the tracker key of one user's task about parts.
func aiTaskKey(userID int64, parts ...string) string {
	return strconv.FormatInt(userID, 10) + ":" + strings.Join(parts, ":")
}

// pctPair is the median and 80th percentile of a distribution.
type pctPair struct {
	P50 *float64 `json:"p50"`
	P80 *float64 `json:"p80"`
}

func percentiles(xs []float64) pctPair {
	if len(xs) == 0 {
		return pctPair{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	at := func(p float64) *float64 {
		v := s[int(math.Round(p*float64(len(s)-1)))]
		v = math.Round(v*100) / 100
		return &v
	}
	return pctPair{P50: at(0.5), P80: at(0.8)}
}

// strategyEvidence is what the app measured over a set of decisions (a copy
// set counts once): shown beside a draft and handed to the model. Heat and
// best open profit are in points, per decision.
type strategyEvidence struct {
	From           string         `json:"from,omitempty"`
	To             string         `json:"to,omitempty"`
	Decisions      int            `json:"decisions"`
	Trades         int            `json:"trades"`
	Wins           int            `json:"wins"`
	Losses         int            `json:"losses"`
	WinRate        float64        `json:"winRate"`
	Net            float64        `json:"net"`
	AvgNet         float64        `json:"avgNetPerDecision"`
	HoldMinutes    pctPair        `json:"holdMinutes"`
	Long           int            `json:"long"`
	Short          int            `json:"short"`
	Symbols        map[string]int `json:"symbols"`
	EntryHours     map[string]int `json:"entryHours"`
	RCount         int            `json:"rCount"`
	R              pctPair        `json:"r"`
	ExitKept       pctPair        `json:"exitKept"`
	WinnerHeat     pctPair        `json:"winnerHeatPts"`
	LoserHeat      pctPair        `json:"loserHeatPts"`
	WinnerBest     pctPair        `json:"winnerBestPts"`
	LoserBest      pctPair        `json:"loserBestPts"`
	Mistakes       map[string]int `json:"mistakes,omitempty"`
	Emotions       map[string]int `json:"emotions,omitempty"`
	ChecklistRules int            `json:"checklistRules,omitempty"`
	// Decisions whose note ticked every rule, of those that had a note.
	ChecklistComplete int `json:"checklistComplete,omitempty"`
	ChecklistNoted    int `json:"checklistNoted,omitempty"`
}

// measureDecisions computes the evidence over raw (per-account) trades: money
// and counts per decision, heat and best seen in points from one leg of each
// decision (the legs of a copy set share the price path).
func (h *Handlers) measureDecisions(ctx context.Context, raw []analyticsTrade, tz *time.Location) strategyEvidence {
	ev := strategyEvidence{Symbols: map[string]int{}, EntryHours: map[string]int{}, Mistakes: map[string]int{}, Emotions: map[string]int{}, Trades: len(raw)}
	if len(raw) == 0 {
		return ev
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
	// One representative leg per decision for the price path.
	type path struct {
		win              bool
		heat, best, kept *float64
	}
	paths := map[int64]path{}
	seenSet := map[int64]bool{}
	for _, t := range raw {
		if t.SetID != nil {
			if seenSet[*t.SetID] {
				continue
			}
			seenSet[*t.SetID] = true
		}
		v := pointValue(t.SymbolRoot)
		if v <= 0 || t.Qty <= 0 || t.MaePnl == nil || t.MfePnl == nil {
			continue
		}
		heat, best := *t.MaePnl/t.Qty/v, *t.MfePnl/t.Qty/v
		p := path{win: t.NetPnl != nil && *t.NetPnl > 0, heat: &heat, best: &best}
		if t.ExitPrice != nil {
			p.kept = excursion.Efficiency(t.Direction, t.EntryPrice, *t.ExitPrice, best)
		}
		paths[t.ID] = p
	}
	var winHeat, loseHeat, winBest, loseBest, kept, holds, rs []float64
	for _, p := range paths {
		if p.win {
			winHeat, winBest = append(winHeat, *p.heat), append(winBest, *p.best)
		} else {
			loseHeat, loseBest = append(loseHeat, *p.heat), append(loseBest, *p.best)
		}
		if p.kept != nil {
			kept = append(kept, *p.kept)
		}
	}
	decisions := collapseSets(raw)
	ev.Decisions = len(decisions)
	first, last := decisions[0].EntryTime, decisions[0].EntryTime
	for _, d := range decisions {
		if d.EntryTime.Before(first) {
			first = d.EntryTime
		}
		if d.EntryTime.After(last) {
			last = d.EntryTime
		}
		net := 0.0
		if d.NetPnl != nil {
			net = *d.NetPnl
		}
		ev.Net += net
		if net > 0 {
			ev.Wins++
		} else if net < 0 {
			ev.Losses++
		}
		if d.Direction == "long" {
			ev.Long++
		} else {
			ev.Short++
		}
		ev.Symbols[d.SymbolRoot]++
		ev.EntryHours[d.EntryTime.In(tz).Format("15")+":00"]++
		holds = append(holds, d.ExitTime.Sub(d.EntryTime).Minutes())
		if r := positions.RMultiple(d.NetPnl, d.PlannedRisk); r != nil {
			rs = append(rs, *r)
		}
		for _, m := range d.Mistakes {
			ev.Mistakes[journal.LabelName(journal.KindMistake, m)]++
		}
		for _, e := range d.Emotions {
			ev.Emotions[journal.LabelName(journal.KindEmotion, e)]++
		}
		if d.ChecklistTotal > 0 {
			ev.ChecklistRules = int(d.ChecklistTotal)
			ev.ChecklistNoted++
			if d.ChecklistDone >= d.ChecklistTotal {
				ev.ChecklistComplete++
			}
		}
	}
	ev.From, ev.To = first.In(tz).Format("2006-01-02"), last.In(tz).Format("2006-01-02")
	ev.Net = math.Round(ev.Net*100) / 100
	ev.AvgNet = math.Round(ev.Net/float64(ev.Decisions)*100) / 100
	ev.WinRate = math.Round(float64(ev.Wins)/float64(ev.Decisions)*1000) / 1000
	ev.HoldMinutes = percentiles(holds)
	ev.RCount, ev.R = len(rs), percentiles(rs)
	ev.ExitKept = percentiles(kept)
	ev.WinnerHeat, ev.LoserHeat = percentiles(winHeat), percentiles(loseHeat)
	ev.WinnerBest, ev.LoserBest = percentiles(winBest), percentiles(loseBest)
	return ev
}

// traderWords is the trader's own writing in a note: the appended blocks the
// AI or the gamma annotator wrote are dropped, the rest flattened to text.
func traderWords(body string, n int) string {
	var keep []string
	for _, seg := range strings.Split(body, "\n---\n") {
		if strings.Contains(seg, "<!-- "+aiMarkerLocal) || strings.Contains(seg, "<!-- "+aiMarkerMCP) ||
			strings.Contains(seg, "<!-- "+gexMarker) || strings.TrimSpace(seg) == strings.TrimSpace(aiPlaceholderBody) {
			continue
		}
		keep = append(keep, seg)
	}
	return previewText(strings.Join(keep, "\n"), n)
}

// strategyNotes returns the trader's own words on the notes tagged with the
// strategy, newest first.
func (h *Handlers) strategyNotes(ctx context.Context, strategyID int64) []string {
	rows, err := h.rdb().QueryContext(ctx, `SELECT body FROM journal_entries
		WHERE user_id = ? AND strategy_id = ? ORDER BY entry_date DESC, id DESC LIMIT 60`, userID(ctx), strategyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() && len(out) < aiStrategyNotes {
		var body string
		if rows.Scan(&body) == nil {
			if w := traderWords(body, aiStrategyNoteRune); w != "" {
				out = append(out, w)
			}
		}
	}
	return out
}

// aiLanguageLine asks for prose in the review language (JSON keys stay).
func aiLanguageLine(lang string) string {
	if lang == "" {
		return ""
	}
	return "\n\nWrite every text value in " + lang + "; keep the JSON keys in English."
}

const aiStrategyDraftPrompt = `You help a futures day trader write down a strategy they already trade. The user message is JSON: the strategy's name, its current playbook and checklist (either may be empty), the evidence the app measured over the trader's decisions tagged with it, and the trader's own notes on those trades. In the evidence a copy set (one decision on several accounts) counts once; money is USD; heat (worst open loss) and best (best open profit) are in points per decision, as the median (p50) and 80th percentile (p80), for winners and losers apart; exitKept is the share of the best move the exit kept; entryHours are in the trader's timezone; r is net over the planned risk where one was planned.

Write the playbook as the trader actually trades it. Ground every rule in the evidence or the notes; never invent a rule nothing supports, and say plainly where the evidence is thin (few decisions, no planned risk, no notes).

Answer with ONLY a JSON object:
{"playbook": "<markdown>", "checklist": ["<rule>", ...], "statedVsActual": "<markdown, or empty>"}
- playbook: markdown with the sections ## Setup, ## Entry, ## Stop, ## Targets and management, ## When, ## When not to trade it, ## Evidence. Concrete: stops and targets in points from the entry (use the heat and best-seen percentiles), times in the trader's timezone. Evidence is 3-5 bullets with the key numbers.
- checklist: 4 to 8 rules, each one a yes/no check on a single trade ("Stop placed before the entry, at most 4 pts away"), never vague advice.
- statedVsActual: only when a current playbook exists - where what it says differs from what the trader did, with the numbers. Otherwise an empty string.`

const aiDiscoveryPrompt = `You help a futures day trader find the strategies hidden in the trades they never tagged with one. The user message is JSON: groups of untagged decisions (a copy set counts once) by instrument, time of day on the exchange clock (Chicago) and holding time, each with the evidence the app measured (as for a strategy: money in USD, heat and best open profit in points as p50/p80 for winners and losers, exitKept, entryHours in the trader's timezone) and any notes the trader wrote on them.

For each group: name the setup the trader most likely runs there (short, e.g. "First-hour MES scalp"), describe it in two or three sentences from the evidence, judge whether it is worth formalizing - keep (it pays), fix (it could, with the rule that would change it) or drop (it costs) - with the reason in numbers, and draft 3 to 6 rules, each a yes/no check on a single trade.

Answer with ONLY a JSON object:
{"groups": [{"id": "<the group id>", "name": "...", "summary": "...", "verdict": "keep|fix|drop", "reason": "...", "rules": ["...", ...]}]}`

// strategyDraftData is the part of a stored draft the app acts on.
type strategyDraftData struct {
	Checklist      []string         `json:"checklist"`
	StatedVsActual string           `json:"statedVsActual"`
	Evidence       strategyEvidence `json:"evidence"`
}

// draftStrategy writes the co-author's draft for the caller's strategy into
// ai_outputs.
func (h *Handlers) draftStrategy(ctx context.Context, strategyID int64) error {
	client, model, err := h.aiFor(ctx)
	if err != nil {
		return err
	}
	st, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: strategyID, UserID: userID(ctx)})
	if err != nil {
		return fmt.Errorf("load strategy: %w", err)
	}
	tz := h.localTZ()
	all, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, nil, nil)
	if err != nil {
		return fmt.Errorf("load trades: %w", err)
	}
	var mine []analyticsTrade
	for _, t := range all {
		if t.Strategy == st.Name {
			mine = append(mine, t)
		}
	}
	if len(mine) == 0 {
		return errors.New("no trade is tagged with this strategy yet: tag a few of its trades, then draft")
	}
	ev := h.measureDecisions(ctx, mine, tz)
	checklist := []string{}
	if lists, err := h.journalSvc().ChecklistsForUser(ctx, userID(ctx)); err == nil {
		for _, it := range lists[strategyID] {
			checklist = append(checklist, it.Text)
		}
	}
	in, _ := json.MarshalIndent(map[string]any{
		"strategy": st.Name, "currentPlaybook": st.Body, "currentChecklist": checklist,
		"evidence": ev, "notes": h.strategyNotes(ctx, strategyID), "timezone": tz.String(),
	}, "", "  ")
	lang := h.userAILanguage(ctx)
	out, err := client.Generate(ctx, aiStrategyDraftPrompt+aiLanguageLine(lang), string(in))
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}
	var res struct {
		Playbook       string   `json:"playbook"`
		Checklist      []string `json:"checklist"`
		StatedVsActual string   `json:"statedVsActual"`
	}
	if err := ai.DecodeJSON(out, &res); err != nil || strings.TrimSpace(res.Playbook) == "" {
		return errors.New("the model did not answer in the draft format; try again or another model")
	}
	data, _ := json.Marshal(strategyDraftData{Checklist: cleanRules(res.Checklist, 8), StatedVsActual: res.StatedVsActual, Evidence: ev})
	return h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{
		UserID: userID(ctx), Kind: aiKindStrategyDraft, Ref: strconv.FormatInt(strategyID, 10),
		Body: strings.TrimSpace(res.Playbook), Data: string(data), Model: model.Label(aiWordsFor("")),
	})
}

// cleanRules trims, drops empties and caps a model's rule list.
func cleanRules(in []string, max int) []string {
	out := []string{}
	for _, r := range in {
		if r = strings.TrimSpace(r); r != "" && len(out) < max {
			out = append(out, r)
		}
	}
	return out
}

// discoveryGroup is one group of untagged decisions with what the model made
// of it.
type discoveryGroup struct {
	ID       string           `json:"id"`
	Symbol   string           `json:"symbol"`
	Session  string           `json:"session"`
	Hold     string           `json:"hold"`
	Evidence strategyEvidence `json:"evidence"`
	Name     string           `json:"name"`
	Summary  string           `json:"summary"`
	Verdict  string           `json:"verdict"`
	Reason   string           `json:"reason"`
	Rules    []string         `json:"rules"`
	Created  bool             `json:"created,omitempty"`
}

// sessionBucket names the part of the CME day an entry falls in (Chicago
// wall clock, where equity futures RTH is 08:30-15:00).
func sessionBucket(t time.Time, chicago *time.Location) string {
	ct := t.In(chicago)
	m := ct.Hour()*60 + ct.Minute()
	switch {
	case m < 8*60+30:
		return "before the open"
	case m < 9*60+30:
		return "first hour"
	case m < 11*60:
		return "morning"
	case m < 13*60:
		return "midday"
	case m < 15*60:
		return "afternoon"
	default:
		return "after the close"
	}
}

func holdBucket(d time.Duration) string {
	switch {
	case d < 5*time.Minute:
		return "scalp (under 5 min)"
	case d < 30*time.Minute:
		return "short hold (5-30 min)"
	default:
		return "long hold (30 min or more)"
	}
}

// discoverGroups groups the caller's untagged decisions of the last
// aiDiscoveryDays by instrument (micros with their mini), session part and
// holding time, keeping the largest groups of at least aiDiscoveryMin.
func (h *Handlers) discoverGroups(ctx context.Context) ([]discoveryGroup, error) {
	tz := h.localTZ()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		chicago = tz
	}
	from := time.Now().AddDate(0, 0, -aiDiscoveryDays)
	all, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, &from, nil)
	if err != nil {
		return nil, fmt.Errorf("load trades: %w", err)
	}
	// Group whole decisions: a set's legs go where its first leg goes.
	byKey := map[string][]analyticsTrade{}
	setKey := map[int64]string{}
	for _, t := range all {
		if t.Strategy != "" {
			continue
		}
		key := ""
		if t.SetID != nil {
			key = setKey[*t.SetID]
		}
		if key == "" {
			// copyset.Key folds a micro into its mini as the vendor symbol
			// ("ES=F"); the group shows the root.
			key = strings.TrimSuffix(copyset.Key(t.SymbolRoot), "=F") + "|" + sessionBucket(t.EntryTime, chicago) + "|" + holdBucket(t.ExitTime.Sub(t.EntryTime))
			if t.SetID != nil {
				setKey[*t.SetID] = key
			}
		}
		byKey[key] = append(byKey[key], t)
	}
	var groups []discoveryGroup
	for key, trades := range byKey {
		ev := h.measureDecisions(ctx, trades, tz)
		if ev.Decisions < aiDiscoveryMin {
			continue
		}
		parts := strings.SplitN(key, "|", 3)
		groups = append(groups, discoveryGroup{Symbol: parts[0], Session: parts[1], Hold: parts[2], Evidence: ev})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Evidence.Decisions > groups[j].Evidence.Decisions })
	if len(groups) > aiDiscoveryTop {
		groups = groups[:aiDiscoveryTop]
	}
	for i := range groups {
		groups[i].ID = "g" + strconv.Itoa(i+1)
	}
	return groups, nil
}

// discoverStrategies groups the untagged decisions, has the model read the
// groups and stores the result.
func (h *Handlers) discoverStrategies(ctx context.Context) error {
	client, model, err := h.aiFor(ctx)
	if err != nil {
		return err
	}
	groups, err := h.discoverGroups(ctx)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return fmt.Errorf("not enough untagged decisions to group: a group needs %d or more of the last %d days", aiDiscoveryMin, aiDiscoveryDays)
	}
	in := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		in = append(in, map[string]any{"id": g.ID, "symbol": g.Symbol, "session": g.Session, "hold": g.Hold, "evidence": g.Evidence})
	}
	payload, _ := json.MarshalIndent(map[string]any{"groups": in, "timezone": h.localTZ().String()}, "", "  ")
	out, err := client.Generate(ctx, aiDiscoveryPrompt+aiLanguageLine(h.userAILanguage(ctx)), string(payload))
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}
	var res struct {
		Groups []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Summary string   `json:"summary"`
			Verdict string   `json:"verdict"`
			Reason  string   `json:"reason"`
			Rules   []string `json:"rules"`
		} `json:"groups"`
	}
	if err := ai.DecodeJSON(out, &res); err != nil {
		return errors.New("the model did not answer in the requested format; try again or another model")
	}
	named := 0
	for _, r := range res.Groups {
		for i := range groups {
			if groups[i].ID == r.ID {
				groups[i].Name, groups[i].Summary, groups[i].Reason = strings.TrimSpace(r.Name), strings.TrimSpace(r.Summary), strings.TrimSpace(r.Reason)
				switch v := strings.ToLower(strings.TrimSpace(r.Verdict)); v {
				case "keep", "fix", "drop":
					groups[i].Verdict = v
				}
				groups[i].Rules = cleanRules(r.Rules, 6)
				named++
			}
		}
	}
	if named == 0 {
		return errors.New("the model did not describe any of the groups; try again or another model")
	}
	data, _ := json.Marshal(groups)
	return h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{
		UserID: userID(ctx), Kind: aiKindDiscovery, Ref: "all", Body: "", Data: string(data), Model: model.Label(aiWordsFor("")),
	})
}

// ---- routes ---------------------------------------------------------------------

// aiTaskGuard is the preamble of the Library's AI routes: 404 when AI is off,
// back to the Library with the reason without a provider. False = stop.
func (h *Handlers) aiTaskGuard(w http.ResponseWriter, r *http.Request) bool {
	if h.AITasks == nil {
		http.NotFound(w, r)
		return false
	}
	if !h.aiAvailable(r.Context()) {
		redirectLibrary(w, r, "strategies", "", errNoAIProvider.Error())
		return false
	}
	return true
}

// startTask runs work as the caller in the background under key.
func (h *Handlers) startTask(ctx context.Context, key string, work func(ctx context.Context) error) {
	user := auth.MustUser(ctx)
	h.AITasks.Start(user.ID, key, func(bg context.Context) error {
		err := work(auth.WithUser(bg, user))
		if err != nil {
			h.logger().Warn("ai task failed", "key", key, "err", err)
		}
		return err
	})
}

// StrategyDraftStart: POST /library/strategies/{id}/draft - draft the
// strategy's playbook from its trades, in the background.
func (h *Handlers) StrategyDraftStart(w http.ResponseWriter, r *http.Request) {
	if !h.aiTaskGuard(w, r) {
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: userID(ctx)}); err != nil {
		http.NotFound(w, r)
		return
	}
	h.startTask(ctx, aiTaskKey(userID(ctx), "strategy", r.PathValue("id")), func(ctx context.Context) error {
		return h.draftStrategy(ctx, id)
	})
	redirectLibrary(w, r, "strategies", "drafting", "")
}

// StrategyDraftApply: POST /library/strategies/{id}/draft/apply - make the
// draft the strategy's playbook and checklist (the name stays).
func (h *Handlers) StrategyDraftApply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	st, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: userID(ctx), Kind: aiKindStrategyDraft, Ref: r.PathValue("id")})
	if err != nil {
		redirectLibrary(w, r, "strategies", "", "that draft is gone")
		return
	}
	var data strategyDraftData
	_ = json.Unmarshal([]byte(out.Data), &data)
	if err := h.Queries.UpdateStrategy(ctx, models.UpdateStrategyParams{Name: st.Name, Body: out.Body, ID: id, UserID: userID(ctx)}); err != nil {
		redirectLibrary(w, r, "strategies", "", "could not update the strategy: "+err.Error())
		return
	}
	if len(data.Checklist) > 0 {
		if err := h.journalSvc().SetChecklistItems(ctx, userID(ctx), id, data.Checklist); err != nil {
			redirectLibrary(w, r, "strategies", "", "playbook saved, but the checklist could not be: "+err.Error())
			return
		}
	}
	_ = h.Queries.DeleteAIOutput(ctx, models.DeleteAIOutputParams{UserID: userID(ctx), Kind: aiKindStrategyDraft, Ref: r.PathValue("id")})
	redirectLibrary(w, r, "strategies", "draft-applied", "")
}

// StrategyDraftDiscard: POST /library/strategies/{id}/draft/discard.
func (h *Handlers) StrategyDraftDiscard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = h.Queries.DeleteAIOutput(ctx, models.DeleteAIOutputParams{UserID: userID(ctx), Kind: aiKindStrategyDraft, Ref: r.PathValue("id")})
	redirectLibrary(w, r, "strategies", "", "")
}

// StrategyDiscoverStart: POST /library/strategies/discover - find strategies
// in the untagged trades, in the background.
func (h *Handlers) StrategyDiscoverStart(w http.ResponseWriter, r *http.Request) {
	if !h.aiTaskGuard(w, r) {
		return
	}
	ctx := r.Context()
	h.startTask(ctx, aiTaskKey(userID(ctx), "discovery"), h.discoverStrategies)
	redirectLibrary(w, r, "strategies", "discovering", "")
}

// StrategyDiscoverCreate: POST /library/strategies/discover/create (form: id)
// - create a strategy from a found group: its name, a playbook from the
// summary and evidence, its rules as the checklist.
func (h *Handlers) StrategyDiscoverCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: userID(ctx), Kind: aiKindDiscovery, Ref: "all"})
	if err != nil {
		redirectLibrary(w, r, "strategies", "", "those results are gone")
		return
	}
	var groups []discoveryGroup
	_ = json.Unmarshal([]byte(out.Data), &groups)
	gid := r.PostFormValue("id")
	for i := range groups {
		g := &groups[i]
		if g.ID != gid || g.Name == "" {
			continue
		}
		st, err := h.Queries.CreateStrategy(ctx, models.CreateStrategyParams{UserID: userID(ctx), Name: g.Name, Body: discoveryPlaybook(*g)})
		if err != nil {
			redirectLibrary(w, r, "strategies", "", "could not create the strategy (name already used?): "+err.Error())
			return
		}
		if err := h.journalSvc().SetChecklistItems(ctx, userID(ctx), st.ID, g.Rules); err != nil {
			redirectLibrary(w, r, "strategies", "", "strategy created, but its checklist could not be saved: "+err.Error())
			return
		}
		g.Created = true
		data, _ := json.Marshal(groups)
		_ = h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{UserID: userID(ctx), Kind: aiKindDiscovery, Ref: "all", Body: out.Body, Data: string(data), Model: out.Model})
		redirectLibrary(w, r, "strategies", "strategy-found", "")
		return
	}
	redirectLibrary(w, r, "strategies", "", "no such group")
}

// StrategyDiscoverDiscard: POST /library/strategies/discover/discard.
func (h *Handlers) StrategyDiscoverDiscard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = h.Queries.DeleteAIOutput(ctx, models.DeleteAIOutputParams{UserID: userID(ctx), Kind: aiKindDiscovery, Ref: "all"})
	redirectLibrary(w, r, "strategies", "", "")
}

// discoveryPlaybook is the starting playbook of a strategy made from a found
// group: what it is, the verdict, and the evidence it came from.
func discoveryPlaybook(g discoveryGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Setup\n\n%s\n\n", g.Summary)
	fmt.Fprintf(&b, "## Where it came from\n\n%s, %s, %s - found in %d untagged decisions (%s to %s).", g.Symbol, g.Session, g.Hold, g.Evidence.Decisions, g.Evidence.From, g.Evidence.To)
	if g.Verdict != "" {
		fmt.Fprintf(&b, " Verdict: **%s** - %s", g.Verdict, g.Reason)
	}
	b.WriteString("\n\n## Evidence\n\n")
	b.WriteString(evidenceLine(g.Evidence))
	b.WriteString("\n")
	return b.String()
}

// evidenceLine is the evidence as one readable line.
func evidenceLine(ev strategyEvidence) string {
	parts := []string{
		fmt.Sprintf("%d decisions", ev.Decisions),
		fmt.Sprintf("win %.0f%%", ev.WinRate*100),
		fmt.Sprintf("net %+.2f (%+.2f each)", ev.Net, ev.AvgNet),
	}
	if ev.HoldMinutes.P50 != nil {
		parts = append(parts, fmt.Sprintf("median hold %.0f min", *ev.HoldMinutes.P50))
	}
	if ev.WinnerHeat.P80 != nil {
		parts = append(parts, fmt.Sprintf("winners' heat p80 %.2f pts", *ev.WinnerHeat.P80))
	}
	if ev.WinnerBest.P50 != nil {
		parts = append(parts, fmt.Sprintf("winners' best p50 %.2f pts", *ev.WinnerBest.P50))
	}
	if ev.ExitKept.P50 != nil {
		parts = append(parts, fmt.Sprintf("exit kept %.0f%% of the best move", *ev.ExitKept.P50*100))
	}
	if ev.RCount > 0 && ev.R.P50 != nil {
		parts = append(parts, fmt.Sprintf("median R %.2f (%d planned)", *ev.R.P50, ev.RCount))
	}
	return strings.Join(parts, " · ")
}

// AITaskStatus: GET /ai/tasks/status?k=... (htmx) - the poll of a view
// showing a background AI task: 204 while it runs, then a reload.
func (h *Handlers) AITaskStatus(w http.ResponseWriter, r *http.Request) {
	k := r.URL.Query().Get("k")
	if k == "" || h.AITasks.Pending(aiTaskKey(userID(r.Context()), k)) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// ---- the Library's view of them ---------------------------------------------------

// strategyDraftView is a stored draft as the Library shows it.
type strategyDraftView struct {
	Body           template.HTML
	Checklist      []string
	StatedVsActual template.HTML
	Evidence       string
	Model          string
	CreatedAt      time.Time
}

// discoveryView is a found group with its evidence as a line.
type discoveryView struct {
	discoveryGroup
	Line string
}

// strategyAIView is the Library's AI state: per strategy a draft, whether one
// is being written and its failure; the found groups likewise.
type strategyAIView struct {
	Drafts           map[int64]strategyDraftView
	Pending          map[int64]bool
	Errors           map[int64]string
	Discovery        []discoveryView
	DiscoveryModel   string
	DiscoveryAt      time.Time
	DiscoveryPending bool
	DiscoveryError   string
}

func (h *Handlers) strategyAI(ctx context.Context, strategies []models.Strategy) strategyAIView {
	v := strategyAIView{Drafts: map[int64]strategyDraftView{}, Pending: map[int64]bool{}, Errors: map[int64]string{}}
	uid := userID(ctx)
	for _, st := range strategies {
		ref := strconv.FormatInt(st.ID, 10)
		key := aiTaskKey(uid, "strategy", ref)
		v.Pending[st.ID] = h.AITasks.Pending(key)
		if msg := h.AITasks.TakeError(uid, key); msg != "" {
			v.Errors[st.ID] = msg
		}
		out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindStrategyDraft, Ref: ref})
		if err != nil {
			continue
		}
		var data strategyDraftData
		_ = json.Unmarshal([]byte(out.Data), &data)
		d := strategyDraftView{
			Body: template.HTML(renderMarkdown(out.Body)), Checklist: data.Checklist,
			Evidence: evidenceLine(data.Evidence), Model: out.Model, CreatedAt: out.CreatedAt,
		}
		if strings.TrimSpace(data.StatedVsActual) != "" {
			d.StatedVsActual = template.HTML(renderMarkdown(data.StatedVsActual))
		}
		v.Drafts[st.ID] = d
	}
	key := aiTaskKey(uid, "discovery")
	v.DiscoveryPending = h.AITasks.Pending(key)
	v.DiscoveryError = h.AITasks.TakeError(uid, key)
	if out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindDiscovery, Ref: "all"}); err == nil {
		var groups []discoveryGroup
		_ = json.Unmarshal([]byte(out.Data), &groups)
		for _, g := range groups {
			v.Discovery = append(v.Discovery, discoveryView{discoveryGroup: g, Line: evidenceLine(g.Evidence)})
		}
		v.DiscoveryModel, v.DiscoveryAt = out.Model, out.CreatedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		h.logger().Warn("load discovery", "err", err)
	}
	return v
}

// jsonIndent marshals v for a model's user turn.
func jsonIndent(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return string(b), err
}
