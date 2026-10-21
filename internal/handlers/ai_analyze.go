package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/aimethod"
	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/levelmap"
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

// The analyst prompt is the method (internal/aimethod) rendered per entry
// scope: the generic skill (role, grounding, glossary, format), the scope's
// skill saying what this item is and which sections to write, the lesson
// line and the verdict line that scope may fill. A trade and a week are
// different questions, and a prompt that serves both reads every item as a
// trade.

// aiSystemPrompt returns the analyst system prompt for an entry scope (trade,
// group, day, week; anything else reads as a trade). language (e.g. "Czech"),
// when non-empty, makes the model write the review prose in that language;
// empty = the model default (English).
func aiSystemPrompt(scope, language string) string {
	return aimethod.SystemPrompt(aimethod.Scope(scope), language)
}

// aiReviewSystem is the system turn of a review: the method as one cached
// block, then the language lead when one is set.
func aiReviewSystem(language string) []ai.Block {
	blocks := []ai.Block{{Text: aimethod.ReviewMethod(), Cache: true}}
	if lead := aimethod.LanguageLead(language); lead != "" {
		blocks = append(blocks, ai.Block{Text: lead})
	}
	return blocks
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
	Mistakes   []string // label keys the review suggests (open suggestions)
	Emotions   []string
}

func aiTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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

// aiNotesRunes caps the trader's words a review reads.
const aiNotesRunes = 4000

// traderBody is the note's body without the blocks the AI or the gamma
// annotator appended, cut to n runes, and how many AI blocks were dropped.
func traderBody(body string, n int) (string, int) {
	var keep []string
	prior := 0
	for _, seg := range strings.Split(body, "\n---\n") {
		switch {
		case strings.Contains(seg, "<!-- "+aiMarkerLocal) || strings.Contains(seg, "<!-- "+aiMarkerMCP):
			prior++
			continue
		case strings.Contains(seg, "<!-- "+gexMarker) || strings.TrimSpace(seg) == strings.TrimSpace(aiPlaceholderBody):
			continue
		}
		keep = append(keep, seg)
	}
	out := strings.TrimSpace(strings.Join(keep, "\n---\n"))
	if r := []rune(out); len(r) > n {
		out = string(r[:n]) + " …"
	}
	return out, prior
}

// aiEntryContext builds the user-prompt JSON for one entry, shaped by its
// scope: the entry (notes, labels, checklist, summary, compact chart) with
// its trades in the reviewer's view (fills for a trade or group; none for a
// week, which reads the digest instead), the coach checks, the day plan or
// the weekly digest, the assigned playbook, the strategies the model may pick
// from and, for a trade or group, the label vocabulary.
func (h *Handlers) aiEntryContext(ctx context.Context, e models.JournalEntry, tz *time.Location) ([]ai.Block, *chartEnvelope, error) {
	detail, err := h.journalDetailJSON(ctx, e, tz)
	if err != nil {
		return nil, nil, err
	}
	delete(detail, "attachments")
	delete(detail, "chartPng")
	delete(detail, "createdAt")
	delete(detail, "updatedAt")
	// The trader's own words: the blocks the AI or the gamma annotator
	// appended are dropped (a review must not re-read its predecessors as
	// the trader's notes), their count kept.
	if body, ok := detail["notes"].(string); ok {
		words, prior := traderBody(body, aiNotesRunes)
		detail["notes"] = words
		if prior > 0 {
			detail["priorReviews"] = prior
		}
	}
	var chart *chartEnvelope
	if ce, ok := detail["chart"].(chartEnvelope); ok && e.Scope != "week" {
		chart = &ce
		detail["chart"] = compactChartForAI(ce, tz)
	} else {
		delete(detail, "chart") // a week reads the digest, not a week of bars
	}
	decision := e.Scope == "trade" || e.Scope == "group"
	linked, err := h.apiTradesForEntry(ctx, e, tz)
	if err != nil {
		return nil, nil, err
	}
	if e.Scope == "week" {
		delete(detail, "trades") // the digest carries the week, per decision
	}
	entryPrice := map[int64]float64{} // by trade id, for the map around the entry
	if e.Scope != "week" {
		rich, err := h.aiTrades(ctx, linked, tz, decision)
		if err != nil {
			return nil, nil, err
		}
		detail["trades"] = rich
		for _, t := range rich {
			entryPrice[t.ID] = t.EntryPrice
		}
	}
	out := map[string]any{"entry": detail, "timezone": tz.String()}
	if decision {
		out["labelVocabulary"] = aimethod.LabelVocabulary()
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

	if lessons := h.aiRecentLessons(ctx, e.ID); len(lessons) > 0 {
		out["recentLessons"] = lessons
	}
	// A day's review builds on what the day's own trade reviews drew.
	if e.Scope == "day" {
		if lessons := h.aiDayLessons(ctx, e.EntryDate.In(tz).Format("2006-01-02")); len(lessons) > 0 {
			out["dayLessons"] = lessons
		}
	}
	// A decision is judged with the session as it stood at its entry: the
	// structure and the bars known then, never what followed.
	if decision && len(linked) > 0 {
		first := linked[0]
		for _, t := range linked[1:] {
			if t.EntryTime.Before(first.EntryTime) {
				first = t
			}
		}
		if session := h.aiSession(ctx, first.SymbolRoot, first.EntryTime); session != nil {
			out["sessionAtEntry"] = session
		}
		// The day map of the decision's market, as it stood that day,
		// around the entry: whether the trade sat at a zone or in the open.
		if market, ok := levelmap.MarketOf(first.SymbolRoot); ok {
			if m, err := h.levelMap(ctx, market, levelmap.TradingDay(first.EntryTime, h.chicago()), time.Now()); err == nil && m != nil && len(m.Zones) > 0 {
				out["dayMap"] = m.Brief(entryPrice[first.ID], 6, 2)
			}
		}
	}
	// A day's review reads the map of every market traded that day.
	if e.Scope == "day" && len(linked) > 0 {
		seen := map[string]bool{}
		var maps []levelmap.Brief
		for _, t := range linked {
			market, ok := levelmap.MarketOf(t.SymbolRoot)
			if !ok || seen[market] {
				continue
			}
			seen[market] = true
			if m, err := h.levelMap(ctx, market, e.EntryDate.In(tz).Format("2006-01-02"), time.Now()); err == nil && m != nil && len(m.Zones) > 0 {
				maps = append(maps, m.Brief(0, 8, 0))
			}
		}
		if len(maps) > 0 {
			out["dayMap"] = maps
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
	// The user turn, stable first so a provider's prefix cache finds it: the
	// vocabulary and the playbooks as their own objects (cached), then the
	// scope line and the item.
	var blocks []ai.Block
	stable := func(keys ...string) {
		part := map[string]any{}
		for _, k := range keys {
			if v, ok := out[k]; ok {
				part[k] = v
				delete(out, k)
			}
		}
		if len(part) == 0 {
			return
		}
		b, err := json.MarshalIndent(part, "", "  ")
		if err == nil {
			blocks = append(blocks, ai.Block{Text: string(b), Cache: true})
		}
	}
	stable("labelVocabulary")
	stable("availableStrategies", "assignedStrategy")
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	blocks = append(blocks, ai.Block{Text: aimethod.ScopeLine(aimethod.Scope(e.Scope)) + "\n\n" + string(b)})
	return blocks, chart, nil
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
	blocks, chart, err := h.aiEntryContext(ctx, e, tz)
	if err != nil {
		return res, err
	}
	// The chart image, for a trade or group when the user sends it (vision
	// models; a model that refuses it is asked again without it).
	req := ai.Request{System: aiReviewSystem(h.userAILanguage(ctx)), User: blocks}
	last := &req.User[len(req.User)-1]
	if chart != nil && (e.Scope == "trade" || e.Scope == "group") && h.userSendsChart(ctx) {
		if b, err := h.chartEnvelopePNG(*chart, "", tz); err == nil {
			req.Images = [][]byte{b}
			last.Text = aiImageSentence + last.Text
		}
	}
	// Restate the language directive at the top of the USER turn too - some local models
	// follow the user message more closely than the system prompt for output language.
	lang := h.userAILanguage(ctx)
	if lang != "" {
		last.Text = "Reply in " + lang + " (write the whole review and the lesson in " + lang + "; keep only the keywords of the machine-readable lines in English).\n\n" + last.Text
		req.System = append(req.System, ai.Block{Text: aimethod.LanguageTail(lang)})
	}
	out, err := aiGenerate(ctx, client, req)
	if err != nil {
		return res, fmt.Errorf("ai generate: %w", err)
	}

	// Fill what is unset from the verdict. The assign helpers enforce the rules: only-if-
	// unset (a value you set is never overwritten) and, for strategy and labels, trade/group
	// scope only (a day/week spans mixed setups). only-if-fitting comes from the model
	// answering "none" when nothing clearly applies. The strategy token is resolved by id OR
	// name (resolveStrategyID) - models often echo the name rather than the numeric id.
	// The verdict's tag, strategy and labels become suggestions on the note,
	// never the fields themselves: the trader accepts or dismisses each
	// (journal.Suggest drops what does not apply - a tag or strategy already
	// set, a label already there, a strategy on a day). The strategy token is
	// resolved by id OR name (resolveStrategyID) - models often echo the name.
	v := parseAIVerdict(out)
	lesson := parseAILesson(out)
	body := stripAILesson(stripAIVerdict(out))
	var resolvedSID int64
	var strategyName string
	if v.Strategy != "" {
		strats, _ := h.Queries.ListActiveStrategies(ctx, userID(ctx))
		if resolvedSID = resolveStrategyID(v.Strategy, strats); resolvedSID > 0 {
			for _, st := range strats {
				if st.ID == resolvedSID {
					strategyName = st.Name
				}
			}
		}
	}
	var mistakes, emotions []string
	if e.Scope == "trade" || e.Scope == "group" {
		mistakes, emotions = firstN(v.Mistakes, aiMaxMistakes), firstN(v.Emotions, aiMaxEmotions)
	}
	open, err := h.journalSvc().Suggest(ctx, userID(ctx), entryID, v.Tag, resolvedSID, mistakes, emotions)
	if err != nil {
		h.logger().Warn("ai suggestions not recorded", "entry", entryID, "err", err)
	}
	for _, sg := range open {
		switch sg.Kind {
		case journal.SuggestTag:
			res.Tag = sg.Value
		case journal.SuggestStrategy:
			res.StrategyID = resolvedSID
		case journal.KindMistake:
			res.Mistakes = append(res.Mistakes, sg.Value)
		case journal.KindEmotion:
			res.Emotions = append(res.Emotions, sg.Value)
		}
	}
	// Visible with -d. Reveals what the model proposed and what is now open as a
	// suggestion (vs already set, or dismissed before).
	slog.Debug("ai analysis verdict",
		"entry", entryID, "scope", e.Scope, "model", model,
		"tag", v.Tag, "tagSuggested", res.Tag != "",
		"strategyToken", v.Strategy, "strategyResolved", resolvedSID, "strategySuggested", res.StrategyID,
		"mistakes", v.Mistakes, "mistakesSuggested", res.Mistakes,
		"emotions", v.Emotions, "emotionsSuggested", res.Emotions)
	words := aiWordsFor(lang)
	note := "### " + words.Review + " — " + time.Now().In(tz).Format("2006-01-02") + "\n" +
		"*" + mdEscapeInline(model.Label(words)) + "*\n\n" +
		strings.TrimSpace(body) + "\n\n"
	if suggested := aiSuggestedLine(res, strategyName, words); suggested != "" {
		note += suggested + "\n\n"
	}
	note += "<!-- " + aiMarkerLocal + " method=" + aimethod.Version + " -->"
	if err := h.journalSvc().AppendNoteAs(ctx, userID(ctx), e, note, words.NoteAdded); err != nil {
		return res, fmt.Errorf("append analysis: %w", err)
	}
	h.recordLesson(ctx, e, lesson)
	return res, nil
}

// aiSuggestedLine says, at the foot of a review and in its language, what the
// review suggests for the note (the trader accepts or dismisses each on the
// note). Tag values and label names read as on the app's pills. "" when it
// suggests nothing.
func aiSuggestedLine(res aiAnalyzeResult, strategyName string, w aiWords) string {
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
	return "*" + w.Suggested + ": " + strings.Join(parts, " · ") + ".*"
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
// (prepareDayEntries) and the day note, review each entry, then the day note
// as the synthesis. Reports progress and stops promptly when ctx is cancelled.
// force re-reviews already-reviewed entries too; otherwise they are skipped
// (which also makes a re-run resume where a cancelled run left off).
//
// The run locks only what it writes: every note it will review is held under
// review (h.AIReviews) from the start until its own review lands, so those
// notes show the spinner and refuse manual edits while the rest of the
// journal stays open. A note a button review is already writing is left to
// that review.
func (h *Handlers) runDayAnalysis(ctx context.Context, dateStr string, force bool, progress func(done, total int, current string)) error {
	// Fail before preparing any entry when the user has no usable provider.
	if _, _, err := h.aiFor(ctx); err != nil {
		return err
	}
	prep, err := h.prepareDayEntries(ctx, dateStr)
	if err != nil {
		return err
	}
	type item struct {
		id    int64
		label string
	}
	items := make([]item, 0, len(prep.Entries)+1)
	for _, pe := range prep.Entries {
		items = append(items, item{pe.EntryID, pe.Label})
	}
	// The day note goes last - it is the synthesis over the trades - but is
	// created now, so the run's whole set of notes is held from the start. A
	// day spans mixed setups: it may pick up an overall tag, never a strategy.
	if prep.Date != "" {
		if id, err := h.aiEnsureEntry(ctx, apiCreateInput{Scope: "day", Date: prep.Date}); err == nil {
			items = append(items, item{id, "day summary"})
		}
	}

	held := map[int64]func(error){}
	defer func() {
		for _, release := range held {
			release(nil) // cancelled or stopped by an error: let the rest go
		}
	}()
	for _, it := range items {
		if !force && h.entryHasMarker(ctx, it.id, aiMarkerLocal) {
			continue // reviewed already; the run skips it
		}
		if release, ok := h.AIReviews.Track(userID(ctx), it.id, h.entryTradeIDs(ctx, it.id)); ok {
			held[it.id] = release
		}
	}

	total, done := len(items), 0
	progress(done, total, "")
	for _, it := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progress(done, total, it.label)
		release, ok := held[it.id]
		switch {
		case ok:
			delete(held, it.id)
			_, err := h.analyzeEntry(ctx, it.id, force)
			release(err)
			if err != nil {
				return err
			}
		case h.AIReviews == nil: // no tracker (tests): review in place
			if _, err := h.analyzeEntry(ctx, it.id, force); err != nil {
				return err
			}
		}
		done++
		progress(done, total, "")
	}
	return nil
}

// aiRequest is a flat prompt as a request: a cached system block, the user
// text, and the PNG when there is one.
func aiRequest(system, user string, png []byte) ai.Request {
	req := ai.Request{System: []ai.Block{{Text: system, Cache: true}}, User: []ai.Block{{Text: user}}}
	if len(png) > 0 {
		req.Images = [][]byte{png}
	}
	return req
}

// aiImageSentence introduces the chart image in the user turn.
const aiImageSentence = "The attached image is this trade's chart (candles with the entry and exit marked).\n\n"

// aiGenerate makes one review call, with the chart image attached when there
// is one. A model that refuses the image (a 4xx other than a key or rate
// problem: a text-only model) gets the call again without it.
func aiGenerate(ctx context.Context, client ai.Client, req ai.Request) (string, error) {
	resp, err := ai.Do(ctx, client, req)
	if len(req.Images) > 0 {
		var se *ai.StatusError
		if err == nil || !errors.As(err, &se) || se.Code < 400 || se.Code >= 500 ||
			se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden || se.Code == http.StatusTooManyRequests {
			return resp.Text, err
		}
		slog.Warn("ai: the model refused the chart image; reviewing without it", "status", se.Code, "msg", se.Msg)
		req.Images = nil
		last := &req.User[len(req.User)-1]
		last.Text = strings.Replace(last.Text, aiImageSentence, "", 1)
		resp, err = ai.Do(ctx, client, req)
	}
	return resp.Text, err
}

// userSendsChart reports whether the calling user sends the chart image with
// trade reviews (Settings > AI).
func (h *Handlers) userSendsChart(ctx context.Context) bool {
	u, err := h.Queries.GetUserByID(ctx, userID(ctx))
	return err == nil && u.AiSendChart != 0
}
