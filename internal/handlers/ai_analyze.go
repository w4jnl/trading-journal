package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ---- server-side AI analysis -------------------------------------------------
//
// The inward counterpart to the MCP server: instead of an external AI calling our
// tools, the app gathers the same context (trades + chart + strategy playbooks via
// the existing journalDetailJSON / prepareDayEntries helpers) and asks the configured
// AI endpoint for the analysis text, then writes it with the same append-only +
// tj-ai marker semantics. No model tool-calling required, so any local model works.

// The analyst prompt is assembled per entry scope: a shared preamble (role,
// grounding, format), a scope part saying what this item is and which
// sections to write, and the machine-readable verdict line that scope may
// fill. A trade and a week are different questions, and a prompt that serves
// both reads every item as a trade.

const aiPromptCommon = `You are a trading-journal analyst reviewing the user's own futures trades. The user message is JSON context for ONE journal entry; the part below says what kind of item it is and what to write.

Ground every point in the context: interpret the numbers rather than restating them, and never invent prices, times or facts that are not there. Money is USD; times are in the trader's timezone (context.timezone). entry.notes are the trader's own words; entry.labels the mistakes and emotions they already marked; coachChecks, when present, are rule-based observations computed from the trades (timing, size, heat against the planned stop, share of the best move kept, the trader's own norms) - treat them as objective evidence, cite the relevant ones with their numbers, weigh them against the notes, and do not present them as your own discovery.

Format: Markdown, a short bullet list with one bullet per section, each led by a bold label (nested bullets allowed). No headings (the review is inserted under an existing one), no preamble such as "Here is the analysis", no closing pleasantries. Omit a section only when there is genuinely nothing to say. Be specific and brief: one concrete, actionable lesson beats a list of generic advice. Use the SAME structure every time, whether or not a strategy is tagged.`

// aiPromptScope says what the entry is and which sections to write.
var aiPromptScope = map[string]string{
	"trade": `THIS ITEM IS ONE TRADE: a single decision on one account. Review that decision.

Each entry.trades item carries the prices and times, holdMinutes, the fills (scale-ins and scale-outs), the trader's plan (plannedStop, plannedRisk, and r = net / planned risk; null without a plan) and the measured price path while open (mae / mfe = worst heat / best open profit in USD, maePoints / mfePoints, exitEfficiency = share of the best move the exit kept; null when not measured).

Sections:
- **Setup & entry:** what the trade was and the entry against the move (early, late, chasing?).
- **Risk:** the planned stop and risk (say plainly when there was none), the heat taken against it (mae against plannedRisk), the size (coachChecks flag a size-up).
- **Exit:** what the exit kept of the best move (exitEfficiency, mfe): a runner left, a winner cut short, a loser held.
- **Strategy:** the assigned playbook's rules followed or broken (entry.checklist shows the ticked and missed rules); with no strategy tagged, name the closest one from availableStrategies or say that none fits.
- **Mindset:** only from the trader's notes, labels or coachChecks; leave it out when there is no evidence.
- **Verdict & lesson:** good / neutral / bad on the process, not just the P&L, and one concrete takeaway.`,

	"group": `THIS ITEM IS A GROUP: one decision copied across several accounts (a copy set), or a few trades the trader grouped as one idea. Review it as one decision first, then what differed between its legs.

Each entry.trades item carries the prices and times, holdMinutes, the fills, the plan (plannedStop, plannedRisk, r) and the measured price path (mae / mfe in USD, maePoints / mfePoints, exitEfficiency); null fields were not planned or not measured. entry.summary totals the group.

Sections:
- **Setup & entry:** what the decision was and the entry against the move.
- **Risk:** the planned stop and risk per leg and in total (say plainly when there was none), the heat taken against it.
- **Exit:** what the exits kept of the best move, a runner left, a winner cut, a loser held.
- **Copy consistency:** fills, sizes and exits across accounts - slippage, or a leg managed differently; leave it out for a single account.
- **Strategy:** the playbook's rules followed or broken (entry.checklist); with none tagged, the closest one from availableStrategies or that none fits.
- **Mindset:** only from the notes, labels or coachChecks.
- **Verdict & lesson:** good / neutral / bad on the process, and one concrete takeaway.`,

	"day": `THIS ITEM IS A WHOLE TRADING DAY. Write the day's synthesis, not a trade-by-trade recap: trades have their own reviews.

entry.trades are the day's trades (copies of one decision across accounts share a time and price), entry.summary the day's totals, entry.mood the trader's 1-5 self-rating. dayPlan, when present, is the trader's pre-market plan with one check per rule (what the plan said, what the day did, held or not).

Sections:
- **Result & shape:** the net result and how it was made or lost over the session: the sequence, the best and the worst decision.
- **Plan:** judge the day against the trader's plan first - each rule held or broken and what it cost; say so when the plan was written or edited after the first trade. Leave it out without a plan.
- **Patterns:** what repeated across the day (coachChecks and the trades): re-entries, size changes, timing, overtrading.
- **Discipline & mindset:** from the mood, the notes and the coach checks; no guessing.
- **Verdict & focus:** good / neutral / bad on the day's process, and the one thing to do differently next session.`,

	"week": `THIS ITEM IS A TRADING WEEK. Build the review on weeklyDigest: its numbers are per decision and set against the week before, its keep and fix lists are derived from the numbers, its coach and plan tallies count what recurred across the days.

Sections:
- **Result:** the week against the week before, and what drove the change.
- **Keep:** what worked, with the numbers that show it.
- **Fix:** what cost the most, with the numbers.
- **Recurring:** coach findings and plan rules that repeated across the days.
- **Focus for next week:** one or two concrete rules or habits.`,
}

// aiVerdictDecision is the verdict line for a trade or group: the tag, the
// strategy and the review labels the model may fill when unset.
const aiVerdictDecision = `End with EXACTLY one final line - a machine-readable verdict (it is removed before saving):
<!-- tj-ai:verdict tag=<good|neutral|bad|none> strategy=<id|none> mistakes=<keys|none> emotions=<keys|none> -->
- tag: your honest read of the process - good / neutral / bad. Use none ONLY when no such read applies.
- strategy: identify the SINGLE best-fitting entry from "availableStrategies" and give its id or exact name. Weigh the trade (structure, direction, timing) AND the trader's own notes - a note like "3bar" points to a "Three-Bar Reversal". When the trader has not tagged one, choosing the fit is YOUR job: do not answer none just because nothing was tagged or you are not fully certain. Use none only when nothing in the list is plausibly related. Use only an id or name from that list; never invent one.
- mistakes: comma-separated keys from labelVocabulary.mistakes for the mistakes the evidence shows (the numbers, coachChecks or the notes), at most 3; none when the trade was clean or the evidence is thin.
- emotions: comma-separated keys from labelVocabulary.emotions ONLY when the trader's notes express that state or coachChecks show it unmistakably (a revenge re-entry right after a loss); never infer an emotion from the P&L alone; at most 2; otherwise none.
Use only keys from labelVocabulary. Labels the trader already set are kept either way.`

// aiVerdictTagOnly is the verdict line for a day or a week: a day spans mixed
// setups, so it takes an overall tag but no strategy or labels.
const aiVerdictTagOnly = `End with EXACTLY one final line - a machine-readable verdict (it is removed before saving):
<!-- tj-ai:verdict tag=<good|neutral|bad|none> -->
- tag: your honest read of the process over the whole period - good / neutral / bad. Use none ONLY when no such read applies.`

// aiSystemPrompt returns the analyst system prompt for an entry scope (trade,
// group, day, week; anything else reads as a trade). language (e.g. "Czech"),
// when non-empty, makes the model write the review prose in that language;
// empty = the model default (English).
func aiSystemPrompt(scope, language string) string {
	part, ok := aiPromptScope[scope]
	verdict := aiVerdictDecision
	switch {
	case !ok:
		part = aiPromptScope["trade"]
	case scope == "day" || scope == "week":
		verdict = aiVerdictTagOnly
	}
	body := aiPromptCommon + "\n\n" + part + "\n\n" + verdict
	if language == "" {
		return body
	}
	// Local models tend to default to English when the language directive is buried at the
	// end of a long English prompt, so LEAD and CLOSE with it. The verdict line is parsed,
	// so it is the explicit exception that stays in English.
	lead := "WRITE YOUR ENTIRE REVIEW IN " + language + ". Every sentence, every bold section label, and every bullet must be in " + language + " - even though these instructions are written in English. The section labels in the brief below (Setup & entry, Risk, Exit, ...) are English placeholders: write each of them in " + language + " too. The ONLY part that stays in English is the final machine-readable verdict line (the literal <!-- tj-ai:verdict ... --> comment, its keywords and the label keys).\n\n"
	tail := "\n\nReminder: everything above the verdict line - all prose and labels - must be written in " + language + ", not English."
	return lead + body + tail
}

// userAILanguage returns the calling user's configured AI-review language (e.g. "Czech"),
// or "" when unset or unavailable - in which case the model uses its default (English).
func (h *Handlers) userAILanguage(ctx context.Context) string {
	u, err := h.Queries.GetUserByID(ctx, userID(ctx))
	if err != nil {
		return ""
	}
	return u.AiLanguage
}

// aiVerdictRe matches the trailing machine-readable verdict line, e.g.
//
//	<!-- tj-ai:verdict tag=good strategy=12 mistakes=chased,late-exit emotions=none -->
//	<!-- tj-ai:verdict tag=good strategy=ORB VWAP -->
//
// tag is a clean enum; strategy may be an id, a name (possibly with spaces), or
// none; mistakes / emotions are comma- or space-separated label keys, or none.
var (
	aiVerdictRe    = regexp.MustCompile(`(?i)<!--\s*tj-ai:verdict\s+([^>]*?)\s*-->`)
	verdictFieldRe = regexp.MustCompile(`(?i)\b(tag|strategy|mistakes|emotions)\s*=`)
)

type aiVerdict struct {
	Tag      string
	Strategy string   // raw token: an id, a name, or "none"/"" - resolve with resolveStrategyID
	Mistakes []string // raw keys, lower-cased; checked against the vocabulary on assignment
	Emotions []string
}

// parseAIVerdict reads the verdict's fields in any order. A value runs to the
// next known field, so a strategy NAME may contain spaces.
func parseAIVerdict(s string) aiVerdict {
	var v aiVerdict
	m := aiVerdictRe.FindStringSubmatch(s)
	if m == nil {
		return v
	}
	inner := m[1]
	locs := verdictFieldRe.FindAllStringSubmatchIndex(inner, -1)
	for i, loc := range locs {
		end := len(inner)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		key := strings.ToLower(inner[loc[2]:loc[3]])
		val := strings.Trim(strings.TrimSpace(inner[loc[1]:end]), `"'`)
		switch key {
		case "tag":
			switch t := strings.ToLower(val); t {
			case "good", "neutral", "bad":
				v.Tag = t
			}
		case "strategy":
			v.Strategy = val
		case "mistakes":
			v.Mistakes = verdictKeys(val)
		case "emotions":
			v.Emotions = verdictKeys(val)
		}
	}
	return v
}

// verdictKeys splits a label list ("chased, late-exit"), dropping "none".
func verdictKeys(val string) []string {
	var out []string
	for _, k := range strings.FieldsFunc(strings.ToLower(val), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if k = strings.Trim(k, `"'`); k != "" && k != "none" {
			out = append(out, k)
		}
	}
	return out
}

// resolveStrategyID maps a verdict strategy token (an id, a name, or "none"/"") to a
// strategy id the caller owns, or 0 when nothing matches. Names are accepted (case-
// insensitive) because models often echo the name from availableStrategies rather than the
// numeric id - a name-only verdict used to silently leave the strategy unset.
func resolveStrategyID(raw string, strats []models.Strategy) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return 0
	}
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		for _, s := range strats {
			if s.ID == id {
				return id
			}
		}
		return 0 // numeric but not one of the caller's strategies
	}
	for _, s := range strats {
		if strings.EqualFold(strings.TrimSpace(s.Name), raw) {
			return s.ID
		}
	}
	return 0
}

func stripAIVerdict(s string) string {
	return strings.TrimRight(aiVerdictRe.ReplaceAllString(s, ""), " \t\n")
}

// aiAnalyzeResult reports what one entry analysis did (for the single-entry route).
type aiAnalyzeResult struct {
	EntryID    int64
	Skipped    bool // already had the marker
	Tag        string
	StrategyID int64
	Mistakes   []string // label keys the review filled
	Emotions   []string
}

func aiTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// compactChartForAI strips the heavy per-bar arrays (token budget) but keeps the
// entry/exit averages + markers + bar count so the model still gets price context.
func compactChartForAI(ce chartEnvelope) map[string]any {
	panels := make([]map[string]any, 0, len(ce.Panels))
	for _, p := range ce.Panels {
		panels = append(panels, map[string]any{
			"contract":     p.Contract,
			"direction":    p.Direction,
			"entryAvg":     p.EntryAvg,
			"exitAvg":      p.ExitAvg,
			"barTimeframe": p.BarTimeframe,
			"bars":         len(p.Bars),
			"markers":      p.Markers,
		})
	}
	return map[string]any{"status": ce.Status, "panels": panels}
}

// aiTrade is one trade as the reviewer sees it: prices and times in the
// trader's timezone, the plan and the measured price path (nulls = not
// planned or not measured) and, for a trade or group review, its fills.
type aiTrade struct {
	ID             int64    `json:"id"`
	Account        string   `json:"account"`
	Symbol         string   `json:"symbol"`
	Direction      string   `json:"direction"`
	Qty            float64  `json:"qty"`
	EntryTime      string   `json:"entryTime"`
	ExitTime       string   `json:"exitTime,omitempty"`
	HoldMinutes    *float64 `json:"holdMinutes"`
	EntryPrice     float64  `json:"entryPrice"`
	ExitPrice      *float64 `json:"exitPrice"`
	NetPnl         *float64 `json:"netPnl"`
	Commission     *float64 `json:"commission"`
	PlannedStop    *float64 `json:"plannedStop"`
	PlannedRisk    *float64 `json:"plannedRisk"`
	R              *float64 `json:"r"`
	Mae            *float64 `json:"mae"`
	Mfe            *float64 `json:"mfe"`
	MaePoints      *float64 `json:"maePoints"`
	MfePoints      *float64 `json:"mfePoints"`
	ExitEfficiency *float64 `json:"exitEfficiency"`
	Fills          []aiFill `json:"fills,omitempty"`
}

type aiFill struct {
	Time  string  `json:"time"`
	Side  string  `json:"side"`
	Role  string  `json:"role"`
	Qty   float64 `json:"qty"`
	Price float64 `json:"price"`
}

// aiMaxFills caps the fills listed per trade: a scalper's ladder can run long,
// and the first few dozen tell the story.
const aiMaxFills = 40

// aiTrades loads the reviewer's view of the entry's trades. withFills adds
// each trade's fills (trade and group reviews; a day keeps to the trades).
func (h *Handlers) aiTrades(ctx context.Context, trades []linkedTrade, tz *time.Location, withFills bool) ([]aiTrade, error) {
	const layout = "2006-01-02 15:04:05"
	out := make([]aiTrade, 0, len(trades))
	for _, lt := range trades {
		t, err := h.rq().GetTradeByID(ctx, models.GetTradeByIDParams{ID: lt.ID, UserID: userID(ctx)})
		if err != nil {
			return nil, fmt.Errorf("load trade %d: %w", lt.ID, err)
		}
		a := aiTrade{
			ID: t.ID, Account: t.AccountName, Symbol: t.Symbol, Direction: t.Direction, Qty: t.Qty,
			EntryTime: t.EntryTime.In(tz).Format(layout), EntryPrice: t.EntryPrice, ExitPrice: t.ExitPrice,
			NetPnl: t.NetPnl, Commission: t.Commission,
			PlannedStop: t.PlannedStop, PlannedRisk: t.PlannedRisk, R: positions.RMultiple(t.NetPnl, t.PlannedRisk),
			Mae: t.MaePnl, Mfe: t.MfePnl, MaePoints: t.MaePoints, MfePoints: t.MfePoints,
			ExitEfficiency: apiExitEfficiency(t.Direction, t.EntryPrice, t.ExitPrice, t.MfePoints),
		}
		if t.ExitTime != nil {
			a.ExitTime = t.ExitTime.In(tz).Format(layout)
			mins := math.Round(t.ExitTime.Sub(t.EntryTime).Minutes()*10) / 10
			a.HoldMinutes = &mins
		}
		if withFills {
			execs, err := h.rq().ListExecutionsForTrade(ctx, t.ID)
			if err != nil {
				return nil, fmt.Errorf("load fills of trade %d: %w", t.ID, err)
			}
			for i, e := range execs {
				if i == aiMaxFills {
					break
				}
				a.Fills = append(a.Fills, aiFill{Time: e.ExecTime.In(tz).Format(layout), Side: e.Side, Role: e.Role, Qty: e.QtyUsed, Price: e.Price})
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// aiLabelVocabulary lists the review labels the verdict may name.
func aiLabelVocabulary() map[string]any {
	kind := func(k string) []map[string]string {
		var out []map[string]string
		for _, l := range journal.LabelOptions(k) {
			out = append(out, map[string]string{"key": l.Key, "name": l.Name})
		}
		return out
	}
	return map[string]any{"mistakes": kind(journal.KindMistake), "emotions": kind(journal.KindEmotion)}
}

// aiEntryContext builds the user-prompt JSON for one entry, shaped by its
// scope: the entry (notes, labels, checklist, summary, compact chart) with
// its trades in the reviewer's view (fills for a trade or group; none for a
// week, which reads the digest instead), the coach checks, the day plan or
// the weekly digest, the assigned playbook, the strategies the model may pick
// from and, for a trade or group, the label vocabulary.
func (h *Handlers) aiEntryContext(ctx context.Context, e models.JournalEntry, tz *time.Location) (string, error) {
	detail, err := h.journalDetailJSON(ctx, e, tz)
	if err != nil {
		return "", err
	}
	delete(detail, "attachments")
	delete(detail, "chartPng")
	delete(detail, "createdAt")
	delete(detail, "updatedAt")
	if ce, ok := detail["chart"].(chartEnvelope); ok {
		detail["chart"] = compactChartForAI(ce)
	}
	decision := e.Scope == "trade" || e.Scope == "group"
	linked, err := h.apiTradesForEntry(ctx, e, tz)
	if err != nil {
		return "", err
	}
	if e.Scope == "week" {
		delete(detail, "trades") // the digest carries the week, per decision
	} else {
		rich, err := h.aiTrades(ctx, linked, tz, decision)
		if err != nil {
			return "", err
		}
		detail["trades"] = rich
	}
	out := map[string]any{"entry": detail, "timezone": tz.String()}
	if decision {
		out["labelVocabulary"] = aiLabelVocabulary()
	}

	// The day's coach checks, narrowed to this entry's trades (all of them
	// for a day entry): objective, rule-based evidence for the review.
	if e.Scope != "week" {
		if rep := h.coachReport(ctx, e.EntryDate.In(tz)); rep != nil {
			var ids []int64
			if e.Scope != "day" {
				ids = []int64{}
				for _, t := range linked {
					ids = append(ids, t.ID)
				}
			}
			if narrowed := coachFor(rep, ids); narrowed != nil && len(narrowed.Findings) > 0 {
				out["coachChecks"] = narrowed.Findings
			}
			if e.Scope == "day" && rep.Plan != nil {
				out["dayPlan"] = rep.Plan
			}
		}
	}

	if e.Scope == "week" {
		monday := e.EntryDate.In(tz)
		y, w := monday.ISOWeek()
		if d, err := h.buildDigest(ctx, monday, fmt.Sprintf("%d-W%02d", y, w)); err == nil {
			out["weeklyDigest"] = digestForAI(d)
		} else {
			h.logger().Warn("ai week digest", "err", err)
		}
	}

	if e.StrategyID != nil {
		if s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: *e.StrategyID, UserID: userID(ctx)}); err == nil {
			out["assignedStrategy"] = map[string]any{"id": s.ID, "name": s.Name, "playbook": s.Body}
		}
	}
	if strats, err := h.Queries.ListActiveStrategies(ctx, userID(ctx)); err == nil && len(strats) > 0 {
		list := make([]map[string]any, 0, len(strats))
		for _, s := range strats {
			list = append(list, map[string]any{"id": s.ID, "name": s.Name, "playbook": aiTruncate(s.Body, 600)})
		}
		out["availableStrategies"] = list
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// aiMaxMistakes / aiMaxEmotions cap the labels one review may fill: a few
// that the evidence shows, not a sweep of the vocabulary.
const (
	aiMaxMistakes = 3
	aiMaxEmotions = 2
)

// analyzeEntry generates analysis for one entry and appends it (+ the tj-ai marker) to
// the body, under a heading that names the model that wrote it. It also fills what the
// trader left unset when the verdict names something that fits: the tag (any scope), and
// for a trade or group the strategy and the mistake and emotion labels. A value the trader
// set is never overwritten, and the note ends with a line saying what was filled. force
// re-analyzes even if already marked.
func (h *Handlers) analyzeEntry(ctx context.Context, entryID int64, force bool) (aiAnalyzeResult, error) {
	res := aiAnalyzeResult{EntryID: entryID}
	client, model, err := h.aiFor(ctx)
	if err != nil {
		return res, err
	}
	if !force && h.entryHasMarker(ctx, entryID, aiMarkerLocal) {
		res.Skipped = true
		return res, nil
	}
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: userIDPtr(ctx)})
	if err != nil {
		return res, fmt.Errorf("load entry %d: %w", entryID, err)
	}
	tz := h.localTZ()
	userPrompt, err := h.aiEntryContext(ctx, e, tz)
	if err != nil {
		return res, err
	}
	// Restate the language directive at the top of the USER turn too - some local models
	// follow the user message more closely than the system prompt for output language.
	lang := h.userAILanguage(ctx)
	if lang != "" {
		userPrompt = "Reply in " + lang + " (write the whole review in " + lang + "; keep only the verdict line in English).\n\n" + userPrompt
	}
	out, err := client.Generate(ctx, aiSystemPrompt(e.Scope, lang), userPrompt)
	if err != nil {
		return res, fmt.Errorf("ai generate: %w", err)
	}

	// Fill what is unset from the verdict. The assign helpers enforce the rules: only-if-
	// unset (a value you set is never overwritten) and, for strategy and labels, trade/group
	// scope only (a day/week spans mixed setups). only-if-fitting comes from the model
	// answering "none" when nothing clearly applies. The strategy token is resolved by id OR
	// name (resolveStrategyID) - models often echo the name rather than the numeric id.
	v := parseAIVerdict(out)
	body := stripAIVerdict(out)
	if v.Tag != "" {
		if ok, err := h.assignTagIfUnset(ctx, entryID, v.Tag); err == nil && ok {
			res.Tag = v.Tag
		}
	}
	var resolvedSID int64
	var strategyName string
	if v.Strategy != "" {
		strats, _ := h.Queries.ListActiveStrategies(ctx, userID(ctx))
		if resolvedSID = resolveStrategyID(v.Strategy, strats); resolvedSID > 0 {
			if ok, err := h.assignStrategyIfUnset(ctx, entryID, resolvedSID); err == nil && ok {
				res.StrategyID = resolvedSID
				for _, st := range strats {
					if st.ID == resolvedSID {
						strategyName = st.Name
					}
				}
			}
		}
	}
	if e.Scope == "trade" || e.Scope == "group" {
		svc := h.journalSvc()
		if keys, err := svc.AssignLabelsIfUnset(ctx, userID(ctx), entryID, journal.KindMistake, firstN(v.Mistakes, aiMaxMistakes)); err == nil {
			res.Mistakes = keys
		}
		if keys, err := svc.AssignLabelsIfUnset(ctx, userID(ctx), entryID, journal.KindEmotion, firstN(v.Emotions, aiMaxEmotions)); err == nil {
			res.Emotions = keys
		}
	}
	// Visible with -d. Reveals what the model proposed and what was actually applied
	// (vs already set by the trader).
	slog.Debug("ai analysis verdict",
		"entry", entryID, "scope", e.Scope, "model", model,
		"tag", v.Tag, "tagAssigned", res.Tag != "",
		"strategyToken", v.Strategy, "strategyResolved", resolvedSID, "strategyAssigned", res.StrategyID,
		"mistakes", v.Mistakes, "mistakesAssigned", res.Mistakes,
		"emotions", v.Emotions, "emotionsAssigned", res.Emotions)
	words := aiWordsFor(lang)
	note := "### " + words.Review + " — " + time.Now().In(tz).Format("2006-01-02") + "\n" +
		"*" + mdEscapeInline(model.Label(words)) + "*\n\n" +
		strings.TrimSpace(body) + "\n\n"
	if filled := aiFilledLine(res, strategyName, words); filled != "" {
		note += filled + "\n\n"
	}
	note += "<!-- " + aiMarkerLocal + " -->"
	if err := h.journalSvc().AppendNoteAs(ctx, userID(ctx), e, note, words.NoteAdded); err != nil {
		return res, fmt.Errorf("append analysis: %w", err)
	}
	return res, nil
}

// aiFilledLine says, at the foot of a review and in its language, which unset
// fields the review filled in, so a tag or label the trader did not choose is
// never a mystery. Tag values and label names read as on the app's pills. ""
// when it filled nothing.
func aiFilledLine(res aiAnalyzeResult, strategyName string, w aiWords) string {
	var parts []string
	if res.Tag != "" {
		parts = append(parts, w.Tag+" **"+res.Tag+"**")
	}
	if res.StrategyID > 0 && strategyName != "" {
		parts = append(parts, w.Strategy+" **"+mdEscapeInline(strategyName)+"**")
	}
	names := func(kind string, keys []string) string {
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, journal.LabelName(kind, k))
		}
		return strings.Join(out, ", ")
	}
	if len(res.Mistakes) > 0 {
		parts = append(parts, w.Mistakes+" **"+names(journal.KindMistake, res.Mistakes)+"**")
	}
	if len(res.Emotions) > 0 {
		parts = append(parts, w.Emotions+" **"+names(journal.KindEmotion, res.Emotions)+"**")
	}
	if len(parts) == 0 {
		return ""
	}
	return "*" + w.Filled + ": " + strings.Join(parts, " · ") + ".*"
}

// mdEscapeInline backslash-escapes the Markdown punctuation that could
// restyle a model or strategy name dropped into a line of prose.
func mdEscapeInline(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_[]<>#|", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// firstN returns at most n leading items of keys.
func firstN(keys []string, n int) []string {
	if len(keys) > n {
		return keys[:n]
	}
	return keys
}

// runDayAnalysis is the day-job body: cluster + create the day's entries
// (prepareDayEntries), analyze each, then add a day-level synthesis. Reports progress and
// stops promptly when ctx is cancelled. force re-analyzes already-marked entries too;
// otherwise marked entries are skipped (which also makes a re-run resume where a cancelled
// run left off).
func (h *Handlers) runDayAnalysis(ctx context.Context, dateStr string, force bool, progress func(done, total int, current string)) error {
	// Fail before preparing any entry when the user has no usable provider.
	if _, _, err := h.aiFor(ctx); err != nil {
		return err
	}
	prep, err := h.prepareDayEntries(ctx, dateStr)
	if err != nil {
		return err
	}
	resolvedDate, entries := prep.Date, prep.Entries

	total := len(entries) + 1 // + the day synthesis
	done := 0
	progress(done, total, "")

	for _, pe := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progress(done, total, pe.Label)
		if err := h.aiTrackedReview(ctx, pe.EntryID, force); err != nil {
			return err
		}
		done++
		progress(done, total, "")
	}

	// Day-level synthesis: ensure a day-scope entry, then analyze it. A day spans mixed
	// setups, so it may pick up an overall tag but never an auto-strategy (see analyzeEntry).
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if resolvedDate != "" {
		progress(done, total, "day summary")
		var dayID int64
		day, conflict, err := h.createJournalEntry(ctx, apiCreateInput{Scope: "day", Date: resolvedDate, Body: aiPlaceholderBody})
		switch {
		case conflict != nil:
			dayID = conflict.EntryID
		case err == nil:
			dayID = day.ID
		}
		if dayID != 0 {
			if err := h.aiTrackedReview(ctx, dayID, force); err != nil {
				return err
			}
		}
	}
	done++
	progress(done, total, "")
	return nil
}

// aiTrackedReview runs one of the day job's reviews under the review tracker,
// so its note shows as under review (spinner, locked editor) like one started
// from a button. An entry a button review is already writing is skipped: it
// gets that review.
func (h *Handlers) aiTrackedReview(ctx context.Context, entryID int64, force bool) error {
	done, ok := h.AIReviews.Track(userID(ctx), entryID, h.entryTradeIDs(ctx, entryID))
	if !ok {
		if h.AIReviews != nil {
			return nil // a button review of this entry is running
		}
		_, err := h.analyzeEntry(ctx, entryID, force) // no tracker (tests)
		return err
	}
	_, err := h.analyzeEntry(ctx, entryID, force)
	done(err)
	return err
}
