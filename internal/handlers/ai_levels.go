package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/gexview"
	"github.com/w4jnl/trading-journal/internal/levels"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ---- proposed stop and target ---------------------------------------------------
//
// "Suggest stop & target" on a trade or set sheet: the app lists candidate
// levels from what was known at the entry (internal/levels: structure and
// volatility in the bars before it, the trader's earlier winners, gamma
// levels then), the model picks a stop and a target among them with its
// reasons, and the replay on the bars after the entry shows what those levels
// would have done. Nothing changes until "Use as plan". Runs on h.AITasks;
// stored in ai_outputs (kind levels, ref trade:N or set:N).

const (
	aiKindLevels = "levels"
	// levelsHistoryMin is the fewest earlier decisions a history scope needs
	// before it counts.
	levelsHistoryMin = 8
	// The bars read: before the entry for the structure, after it for the
	// replay (cached bars only; nothing is fetched).
	levelsBarsBefore = 90 * time.Minute
	levelsBarsAfter  = 6 * time.Hour
)

// levelsData is a stored proposal.
type levelsData struct {
	Subject    string               `json:"subject"` // trade:N | set:N
	Direction  string               `json:"direction"`
	Entry      float64              `json:"entry"`
	EntryTime  int64                `json:"entryTime"`
	Tick       float64              `json:"tick"`
	Candidates []levels.Candidate   `json:"candidates"`
	History    *levels.History      `json:"history,omitempty"`
	Stop       float64              `json:"stop"`
	Target     float64              `json:"target"`
	Reasons    []string             `json:"reasons"`
	Outcome    *levels.Outcome      `json:"outcome,omitempty"`
	Actual     *float64             `json:"actualPoints,omitempty"` // what the trade's own exit made, in points
	Legs       []int64              `json:"legs"`
	BarsTf     marketdata.Timeframe `json:"barsTf,omitempty"`
}

// levelsSubject is the trade or set a proposal is for.
type levelsSubject struct {
	ref       string // trade:N | set:N
	direction string
	root      string
	entry     float64 // average for a set
	entryTime time.Time
	exit      *float64 // average exit, when closed
	legs      []models.GetTradeByIDRow
	strategy  string // the strategy of its note, "" when none
}

func (h *Handlers) levelsSubjectForTrade(ctx context.Context, id int64) (*levelsSubject, error) {
	t, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		return nil, err
	}
	s := &levelsSubject{ref: "trade:" + strconv.FormatInt(id, 10), direction: t.Direction, root: t.SymbolRoot,
		entry: t.EntryPrice, entryTime: t.EntryTime, exit: t.ExitPrice, legs: []models.GetTradeByIDRow{t}}
	s.strategy = h.tradeStrategyName(ctx, id)
	return s, nil
}

func (h *Handlers) levelsSubjectForSet(ctx context.Context, setID int64) (*levelsSubject, error) {
	if _, err := h.Queries.GetTradeSet(ctx, models.GetTradeSetParams{ID: setID, UserID: userID(ctx)}); err != nil {
		return nil, err
	}
	rows, err := h.loadSetRows(ctx, setID)
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("load set members: %w", err)
	}
	s := &levelsSubject{ref: "set:" + strconv.FormatInt(setID, 10), direction: rows[0].Direction, root: rows[0].SymbolRoot, entryTime: rows[0].EntryTime}
	var sumEntry, sumExit float64
	exits := 0
	for _, m := range rows {
		t, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: m.ID, UserID: userID(ctx)})
		if err != nil {
			return nil, err
		}
		s.legs = append(s.legs, t)
		sumEntry += t.EntryPrice
		if t.ExitPrice != nil {
			sumExit += *t.ExitPrice
			exits++
		}
		if t.EntryTime.Before(s.entryTime) {
			s.entryTime = t.EntryTime
		}
		if s.strategy == "" {
			s.strategy = h.tradeStrategyName(ctx, m.ID)
		}
	}
	s.entry = sumEntry / float64(len(s.legs))
	if exits == len(s.legs) {
		x := sumExit / float64(exits)
		s.exit = &x
	}
	return s, nil
}

// tradeStrategyName is the strategy on the trade's note or a group note it is
// in ("" when none).
func (h *Handlers) tradeStrategyName(ctx context.Context, tradeID int64) string {
	var name string
	_ = h.rdb().QueryRowContext(ctx, `SELECT s.name FROM journal_entries je JOIN strategies s ON s.id = je.strategy_id
		WHERE je.user_id = ? AND (je.trade_id = ? OR je.id IN (SELECT journal_entry_id FROM journal_entry_trades WHERE trade_id = ?))
		LIMIT 1`, userID(ctx), tradeID, tradeID).Scan(&name)
	return name
}

// levelsHistory is the trader's record before the entry: the decisions of
// the same strategy, else of the same instrument and direction, else of the
// same instrument, whichever first has levelsHistoryMin of them.
func (h *Handlers) levelsHistory(ctx context.Context, s *levelsSubject) *levels.History {
	before := s.entryTime
	all, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, nil, &before)
	if err != nil {
		return nil
	}
	key := copyset.Key(s.root)
	root := strings.TrimSuffix(key, "=F")
	scopes := []struct {
		label string
		keep  func(analyticsTrade) bool
	}{
		{"strategy " + s.strategy, func(t analyticsTrade) bool { return s.strategy != "" && t.Strategy == s.strategy }},
		{root + " " + s.direction, func(t analyticsTrade) bool { return copyset.Key(t.SymbolRoot) == key && t.Direction == s.direction }},
		{root, func(t analyticsTrade) bool { return copyset.Key(t.SymbolRoot) == key }},
	}
	for _, sc := range scopes {
		var pick []analyticsTrade
		for _, t := range all {
			if sc.keep(t) && t.EntryTime.Before(s.entryTime) {
				pick = append(pick, t)
			}
		}
		ev := h.measureDecisions(ctx, pick, h.localTZ())
		if ev.Decisions >= levelsHistoryMin && ev.WinnerHeat.P80 != nil {
			return &levels.History{Scope: sc.label, N: ev.Decisions, WinnerHeat: ev.WinnerHeat.P80, WinnerBest: ev.WinnerBest.P50, WinnerBest8: ev.WinnerBest.P80}
		}
	}
	return nil
}

// levelsGamma is the gamma map that was live at the entry, in futures
// prices (none for a market the engine does not cover, or a stale row).
func (h *Handlers) levelsGamma(ctx context.Context, s *levelsSubject) []levels.GammaLevel {
	market, ok := gexMarket(s.root)
	if h.GEX == nil || !ok {
		return nil
	}
	snap, _, err := h.GEX.CleanSnapshot(ctx, market, s.entryTime, gexCleanSteps, gexview.StaleAfter)
	if err != nil || snap == nil || s.entryTime.Sub(snap.AsOf) > gexview.StaleAfter {
		return nil
	}
	var out []levels.GammaLevel
	add := func(name string, p *float64) {
		if p != nil && *p > 0 {
			out = append(out, levels.GammaLevel{Name: name, Price: *p})
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
	return out
}

const aiLevelsPrompt = `You set the stop and the target of a futures trade at the moment of its entry, as a disciplined trader would. The user message is JSON: the direction, the entry price and time (the trader's timezone), the tick, and candidate levels the app computed from what was known at that moment only - price structure in the bars before the entry, their volatility, the trader's own record before this trade (how much heat their winners took, how far they ran) and the gamma levels then - each with its distance in points and what it is; plus the strategy's playbook when the trade has one.

Choose ONE stop and ONE target. Prefer a stop beyond nearby structure that also covers the heat most of the trader's winners took; a target at a level price is likely to reach (a swing, a gamma magnet or wall, the distance the trader's winners usually ran), aiming for a reward:risk of at least 1.5 when the levels support it - and say so when they do not. You may set a level between candidates if you say why. Follow the playbook's rules when there is one. Never use anything that happened after the entry.

Answer with ONLY a JSON object:
{"stop": <price>, "target": <price>, "reasons": ["...", "..."]}
- reasons: two to four short sentences naming the candidates the levels come from.`

// proposeLevels works out the subject's proposal and stores it.
func (h *Handlers) proposeLevels(ctx context.Context, s *levelsSubject) error {
	client, model, err := h.aiFor(ctx)
	if err != nil {
		return err
	}
	tz := h.localTZ()
	tick := 0.0
	if inst, err := h.Queries.GetInstrument(ctx, s.root); err == nil && inst.TickSize != nil {
		tick = *inst.TickSize
	}
	in := levels.Input{Direction: s.direction, Entry: s.entry, EntryTime: s.entryTime.Unix(), Tick: tick, Loc: tz,
		History: h.levelsHistory(ctx, s), Gamma: h.levelsGamma(ctx, s)}
	var after []marketdata.Bar
	var tf marketdata.Timeframe
	if sym, ok := marketdata.YahooSymbol(s.root); ok && h.MarketData != nil {
		bars, btf, _, err := h.MarketData.BarsForWindow(ctx, sym, s.entryTime.Add(-levelsBarsBefore), s.entryTime.Add(levelsBarsAfter))
		if err == nil {
			tf = btf
			width := int64(tfSeconds(btf))
			for _, b := range bars {
				if b.Time+width <= s.entryTime.Unix() { // a bar that closed before the entry
					in.Before = append(in.Before, b)
				} else {
					after = append(after, b)
				}
			}
		}
	}
	cands := levels.Candidates(in)
	if len(cands) == 0 {
		return errors.New("nothing to work from: no cached bars before the entry, no earlier trades to learn from and no gamma levels")
	}
	playbook := ""
	if s.strategy != "" {
		_ = h.rdb().QueryRowContext(ctx, `SELECT body FROM strategies WHERE user_id = ? AND name = ?`, userID(ctx), s.strategy).Scan(&playbook)
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"direction": s.direction, "entry": s.entry, "entryTime": s.entryTime.In(tz).Format("2006-01-02 15:04:05"),
		"timezone": tz.String(), "tick": tick, "candidates": cands, "history": in.History,
		"strategy": s.strategy, "playbook": aiTruncate(playbook, 1500),
	}, "", "  ")
	out, err := client.Generate(ctx, aiLevelsPrompt+aiLanguageLine(h.userAILanguage(ctx)), string(payload))
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}
	var res struct {
		Stop    float64  `json:"stop"`
		Target  float64  `json:"target"`
		Reasons []string `json:"reasons"`
	}
	if err := ai.DecodeJSON(out, &res); err != nil {
		return errors.New("the model did not answer in the requested format; try again or another model")
	}
	sign := 1.0
	if s.direction != "long" {
		sign = -1
	}
	res.Stop, res.Target = levels.Snap(res.Stop, tick), levels.Snap(res.Target, tick)
	if (s.entry-res.Stop)*sign <= 0 || (res.Target-s.entry)*sign <= 0 {
		return fmt.Errorf("the model put the stop (%g) or the target (%g) on the wrong side of the entry (%g); try again", res.Stop, res.Target, s.entry)
	}
	data := levelsData{Subject: s.ref, Direction: s.direction, Entry: s.entry, EntryTime: s.entryTime.Unix(), Tick: tick,
		Candidates: cands, History: in.History, Stop: res.Stop, Target: res.Target, Reasons: cleanRules(res.Reasons, 4), BarsTf: tf}
	for _, l := range s.legs {
		data.Legs = append(data.Legs, l.ID)
	}
	if o := levels.Replay(after, s.direction, s.entry, res.Stop, res.Target, s.entryTime.Unix()); o.Hit != "nodata" {
		data.Outcome = &o
	}
	if s.exit != nil {
		a := math.Round((*s.exit-s.entry)*sign*100) / 100
		data.Actual = &a
	}
	raw, _ := json.Marshal(data)
	return h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{
		UserID: userID(ctx), Kind: aiKindLevels, Ref: s.ref, Body: "", Data: string(raw), Model: model.Label(aiWordsFor("")),
	})
}

// levelsView is a stored proposal as a sheet shows it.
type levelsView struct {
	levelsData
	Model      string
	RiskPts    float64
	RewardPts  float64
	RewardRisk float64
	OutcomeAt  string // the deciding bar's clock time
	Stops      []levels.Candidate
	Targets    []levels.Candidate
}

// levelsState is a sheet's proposal, whether one is being worked out, and its
// failure (shown once).
func (h *Handlers) levelsState(ctx context.Context, ref string) (view *levelsView, pending bool, failure string) {
	uid := userID(ctx)
	key := aiTaskKey(uid, "levels", ref)
	pending = h.AITasks.Pending(key)
	failure = h.AITasks.TakeError(uid, key)
	out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindLevels, Ref: ref})
	if err != nil {
		return nil, pending, failure
	}
	var d levelsData
	if json.Unmarshal([]byte(out.Data), &d) != nil {
		return nil, pending, failure
	}
	v := &levelsView{levelsData: d, Model: out.Model}
	v.RiskPts, v.RewardPts = math.Abs(d.Entry-d.Stop), math.Abs(d.Target-d.Entry)
	if v.RiskPts > 0 {
		v.RewardRisk = math.Round(v.RewardPts/v.RiskPts*10) / 10
	}
	if d.Outcome != nil && d.Outcome.At > 0 {
		v.OutcomeAt = time.Unix(d.Outcome.At, 0).In(h.localTZ()).Format("15:04")
	}
	for _, c := range d.Candidates {
		if c.Kind == "stop" {
			v.Stops = append(v.Stops, c)
		} else {
			v.Targets = append(v.Targets, c)
		}
	}
	return v, pending, failure
}

// ---- routes ----------------------------------------------------------------------

// levelsGuard: 404 without AI, 409 without a provider. False = stop.
func (h *Handlers) levelsGuard(w http.ResponseWriter, r *http.Request) bool {
	if h.AITasks == nil {
		http.NotFound(w, r)
		return false
	}
	return !h.guardNoAIProvider(w, r)
}

// TradeLevelsStart: POST /trades/{id}/levels (htmx) - work out the proposal
// in the background; the sheet shows it being worked out.
func (h *Handlers) TradeLevelsStart(w http.ResponseWriter, r *http.Request) {
	if !h.levelsGuard(w, r) {
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s, err := h.levelsSubjectForTrade(ctx, id)
	if err != nil {
		http.Error(w, "trade not found", http.StatusNotFound)
		return
	}
	h.startTask(ctx, aiTaskKey(userID(ctx), "levels", s.ref), func(ctx context.Context) error { return h.proposeLevels(ctx, s) })
	h.renderTradeDetail(w, r, "")
}

// SetLevelsStart: POST /trades/sets/{id}/levels (htmx) - the same for a copy
// set, from its average entry.
func (h *Handlers) SetLevelsStart(w http.ResponseWriter, r *http.Request) {
	if !h.levelsGuard(w, r) {
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s, err := h.levelsSubjectForSet(ctx, id)
	if err != nil {
		http.Error(w, "set not found", http.StatusNotFound)
		return
	}
	h.startTask(ctx, aiTaskKey(userID(ctx), "levels", s.ref), func(ctx context.Context) error { return h.proposeLevels(ctx, s) })
	h.renderSetDetail(w, r, "")
}

// applyLevels makes a stored proposal the plan of every leg: the stop (each
// leg's risk from its own size and contract) and the target.
func (h *Handlers) applyLevels(ctx context.Context, ref string) error {
	out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: userID(ctx), Kind: aiKindLevels, Ref: ref})
	if err != nil {
		return errors.New("that proposal is gone")
	}
	var d levelsData
	if err := json.Unmarshal([]byte(out.Data), &d); err != nil {
		return err
	}
	for _, id := range d.Legs {
		t, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)})
		if err != nil {
			continue // the leg is gone (a rebuild); the rest still apply
		}
		stop, target := d.Stop, d.Target
		var risk *float64
		if inst, err := h.Queries.GetInstrument(ctx, t.SymbolRoot); err == nil {
			risk = positions.PlannedRisk(t.EntryPrice, stop, t.Qty, inst)
		}
		if _, err := h.Queries.SetTradePlan(ctx, models.SetTradePlanParams{PlannedStop: &stop, PlannedRisk: risk, ID: id, UserID: userID(ctx)}); err != nil {
			return fmt.Errorf("set plan: %w", err)
		}
		if _, err := h.Queries.SetTradeTarget(ctx, models.SetTradeTargetParams{PlannedTarget: &target, ID: id, UserID: userID(ctx)}); err != nil {
			return fmt.Errorf("set target: %w", err)
		}
	}
	_ = h.Queries.DeleteAIOutput(ctx, models.DeleteAIOutputParams{UserID: userID(ctx), Kind: aiKindLevels, Ref: ref})
	h.LiveData.Bump(userID(ctx))
	return nil
}

// TradeLevelsApply: POST /trades/{id}/levels/apply (htmx).
func (h *Handlers) TradeLevelsApply(w http.ResponseWriter, r *http.Request) {
	if err := h.applyLevels(r.Context(), "trade:"+r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("HX-Trigger", `{"toast":{"message":"Plan set from the proposed levels","kind":"success"}}`)
	h.renderTradeDetail(w, r, "plan")
}

// SetLevelsApply: POST /trades/sets/{id}/levels/apply (htmx).
func (h *Handlers) SetLevelsApply(w http.ResponseWriter, r *http.Request) {
	if err := h.applyLevels(r.Context(), "set:"+r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("HX-Trigger", `{"toast":{"message":"Plan set on every leg from the proposed levels","kind":"success"}}`)
	h.renderSetDetail(w, r, "")
}

// TradeLevelsDiscard / SetLevelsDiscard: POST .../levels/discard (htmx).
func (h *Handlers) TradeLevelsDiscard(w http.ResponseWriter, r *http.Request) {
	_ = h.Queries.DeleteAIOutput(r.Context(), models.DeleteAIOutputParams{UserID: userID(r.Context()), Kind: aiKindLevels, Ref: "trade:" + r.PathValue("id")})
	h.renderTradeDetail(w, r, "")
}

func (h *Handlers) SetLevelsDiscard(w http.ResponseWriter, r *http.Request) {
	_ = h.Queries.DeleteAIOutput(r.Context(), models.DeleteAIOutputParams{UserID: userID(r.Context()), Kind: aiKindLevels, Ref: "set:" + r.PathValue("id")})
	h.renderSetDetail(w, r, "")
}

// tfSeconds is a bar size in seconds ("1m" = 60, "1h" = 3600; 60 when it
// does not parse).
func tfSeconds(tf marketdata.Timeframe) int {
	t := string(tf)
	if len(t) < 2 {
		return 60
	}
	n, err := strconv.Atoi(t[:len(t)-1])
	if err != nil || n <= 0 {
		return 60
	}
	switch t[len(t)-1] {
	case 'm':
		return n * 60
	case 'h':
		return n * 3600
	case 'd':
		return n * 86400
	}
	return 60
}
