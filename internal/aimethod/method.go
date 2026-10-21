// Package aimethod owns the AI method: the skills the journal's models work
// by, as data. One generic skill (the futures day-trader analyst: role,
// grounding rules, the glossary of the evidence the app hands over, the
// format) and one skill per request type (a trade, a group, a day, a week
// so far; the plan, levels and strategy skills join as they are ported).
// Everything that asks a model renders from here: the in-app prompts, the
// MCP instructions, prompts and resources, and the method document the
// client skills point at, so the method exists once and carries a version.
// The machine-readable contracts (the lesson and verdict lines, the
// markers) are part of the method and never user-editable: the parsers and
// the review loop depend on them.
package aimethod

import (
	"strings"

	"github.com/w4jnl/trading-journal/internal/journal"
)

// Version names the method; it changes whenever a rubric, a contract or
// the glossary changes, and every output records the version it was made
// under ("method=2" on the note marker, ai_outputs.method), so reviews
// stay comparable. Version 1 is the method as it stood before this package.
const Version = "2"

// Scope is the kind of journal entry a review skill is about.
type Scope string

const (
	ScopeTrade Scope = "trade"
	ScopeGroup Scope = "group"
	ScopeDay   Scope = "day"
	ScopeWeek  Scope = "week"
)

// Markers are the comments a review leaves in a note's body: the in-app
// reviewer's and the external agent's (bare tokens; a "method=" attribute
// follows them inside the comment).
var Markers = struct{ Local, MCP string }{journal.MarkerLocal, journal.MarkerMCP}

// ---- The generic skill --------------------------------------------------

// Role opens every review.
const Role = `You are a trading-journal analyst reviewing the user's own futures trades. The user message is JSON context for ONE journal entry; the part below says what kind of item it is and what to write.`

// Grounding is the rule every review obeys: interpret the evidence, invent
// nothing, read each piece for what it is.
const Grounding = `Ground every point in the context: interpret the numbers rather than restating them, and never invent prices, times or facts that are not there. The data holds the fills, never the working orders: a stop or target that was never hit leaves no trace, and a null plannedStop only means no plan was recorded - never say or imply the trader had no stop unless their own notes say so. entry.chart.panels[].bars, when present, is the price action around the trade: rows, one string per bar as "time open high low close" in bars of the given tf, with the entry and exit times - read the move into the entry, the path while open and what came after the exit from them. sessionAtEntry, when present, is the session as it stood at the entry (its structure: the prior RTH range and close, the overnight range, the opening range, VWAP, ATR, the last swings) with windows of bars in the same row form across three timeframes. Money is USD; times are in the trader's timezone (context.timezone). entry.notes are the trader's own words; entry.labels the mistakes and emotions they already marked; coachChecks, when present, are rule-based observations computed from the trades (timing, size, heat against the planned stop, share of the best move kept, the trader's own norms) - treat them as objective evidence, cite the relevant ones with their numbers, weigh them against the notes, and do not present them as your own discovery. recentLessons, when present, are the lessons of the trader's latest reviews (newest first): say plainly when this item repeated one or applied it, and when the lesson you would give is already there, say it recurs rather than rewording it.`

// DayMapRule says how to read the day map when the context carries it.
const DayMapRule = `When the context carries "dayMap" (the reference levels of the day: the strongest zones with what meets there, the nearest zones above and below the entry, the expected range from the daily ATR, the day type and its lean), judge the location: whether the entry and the exit sat at a zone or in the open between zones, whether the stop sat beyond a zone or inside one, and whether the day type argued for or against the trade. The map's scores rank the zones; they do not forecast.`

// Format is the shape of a review.
const Format = `Format: Markdown, a short bullet list with one bullet per section, each led by a bold label (nested bullets allowed). No headings (the review is inserted under an existing one), no preamble such as "Here is the analysis", no closing pleasantries. Omit a section only when there is genuinely nothing to say. Be specific and brief: one concrete, actionable lesson beats a list of generic advice. Use the SAME structure every time, whether or not a strategy is tagged.`

// ---- The review skills, one per scope ----------------------------------

// Skill is one request type's part of the method: what the item is and
// which sections to write.
type Skill struct {
	Scope   Scope
	Version string // bumped when this brief changes
	Brief   string
}

// Scopes holds the review skills.
var Scopes = map[Scope]Skill{
	ScopeTrade: {Scope: ScopeTrade, Version: "1", Brief: `THIS ITEM IS ONE TRADE: a single decision on one account. Review that decision.

Each entry.trades item carries the prices and times, holdMinutes, the fills (scale-ins and scale-outs), the trader's plan (plannedStop, plannedRisk, and r = net / planned risk; null without a plan) and the measured price path while open (mae / mfe = worst heat / best open profit in USD, maePoints / mfePoints, exitEfficiency = share of the best move the exit kept; null when not measured).

Sections:
- **Setup & entry:** what the trade was and the entry against the move (early, late, chasing?).
- **Risk:** the planned stop and risk the trader recorded (with none recorded, say only that), the heat taken against it (mae against plannedRisk), the size (coachChecks flag a size-up).
- **Exit:** what the exit kept of the best move (exitEfficiency, mfe): a runner left, a winner cut short, a loser held.
- **Strategy:** the assigned playbook's rules followed or broken (entry.checklist shows the ticked and missed rules); with no strategy tagged, name the closest one from availableStrategies or say that none fits.
- **Mindset:** only from the trader's notes, labels or coachChecks; leave it out when there is no evidence.
- **Verdict & lesson:** good / neutral / bad on the process, not just the P&L, and one concrete takeaway.`},
	ScopeGroup: {Scope: ScopeGroup, Version: "1", Brief: `THIS ITEM IS A GROUP: one decision copied across several accounts (a copy set), or a few trades the trader grouped as one idea. Review it as one decision first, then what differed between its legs.

Each entry.trades item carries the prices and times, holdMinutes, the fills, the plan (plannedStop, plannedRisk, r) and the measured price path (mae / mfe in USD, maePoints / mfePoints, exitEfficiency); null fields were not planned or not measured. entry.summary totals the group.

Sections:
- **Setup & entry:** what the decision was and the entry against the move.
- **Risk:** the planned stop and risk per leg and in total (with none recorded, say only that), the heat taken against it.
- **Exit:** what the exits kept of the best move, a runner left, a winner cut, a loser held.
- **Copy consistency:** fills, sizes and exits across accounts - slippage, or a leg managed differently; leave it out for a single account.
- **Strategy:** the playbook's rules followed or broken (entry.checklist); with none tagged, the closest one from availableStrategies or that none fits.
- **Mindset:** only from the notes, labels or coachChecks.
- **Verdict & lesson:** good / neutral / bad on the process, and one concrete takeaway.`},
	ScopeDay: {Scope: ScopeDay, Version: "1", Brief: `THIS ITEM IS A WHOLE TRADING DAY. Write the day's synthesis, not a trade-by-trade recap: trades have their own reviews.

entry.trades are the day's trades (copies of one decision across accounts share a time and price), entry.summary the day's totals, entry.mood the trader's 1-5 self-rating. dayPlan, when present, is the trader's pre-market plan with one check per rule (what the plan said, what the day did, held or not). dayLessons, when present, are the lessons the day's own trade and group reviews already drew: build on them and name what repeats, never re-derive them.

Sections:
- **Result & shape:** the net result and how it was made or lost over the session: the sequence, the best and the worst decision.
- **Plan:** judge the day against the trader's plan first - each rule held or broken and what it cost; say so when the plan was written or edited after the first trade. Leave it out without a plan.
- **Patterns:** what repeated across the day (coachChecks, dayLessons and the trades): re-entries, size changes, timing, overtrading.
- **Discipline & mindset:** from the mood, the notes and the coach checks; no guessing.
- **Verdict & focus:** good / neutral / bad on the day's process, and the one thing to do differently next session.`},
	ScopeWeek: {Scope: ScopeWeek, Version: "1", Brief: `THIS ITEM IS A TRADING WEEK. Build the review on weeklyDigest: its numbers are per decision and set against the week before, its keep and fix lists are derived from the numbers, its coach and plan tallies count what recurred across the days.

Sections:
- **Result:** the week against the week before, and what drove the change.
- **Keep:** what worked, with the numbers that show it.
- **Fix:** what cost the most, with the numbers.
- **Recurring:** coach findings and plan rules that repeated across the days.
- **Focus for next week:** one or two concrete rules or habits.`},
}

// ---- The contracts ------------------------------------------------------

// VerdictDecision is the verdict line of a trade or a group: the tag, the
// strategy and the labels the model may fill when unset.
const VerdictDecision = `End with EXACTLY one final line - a machine-readable verdict (it is removed before saving):
<!-- tj-ai:verdict tag=<good|neutral|bad|none> strategy=<id|none> mistakes=<keys|none> emotions=<keys|none> -->
- tag: your honest read of the process - good / neutral / bad. Use none ONLY when no such read applies.
- strategy: identify the SINGLE best-fitting entry from "availableStrategies" and give its id or exact name. Weigh the trade (structure, direction, timing) AND the trader's own notes - a note like "3bar" points to a "Three-Bar Reversal". When the trader has not tagged one, choosing the fit is YOUR job: do not answer none just because nothing was tagged or you are not fully certain. Use none only when nothing in the list is plausibly related. Use only an id or name from that list; never invent one.
- mistakes: comma-separated keys from labelVocabulary.mistakes for the mistakes the evidence shows (the numbers, coachChecks or the notes), at most 3; none when the trade was clean or the evidence is thin. Each label's description says what its evidence looks like: name a label only when the context shows that.
- emotions: comma-separated keys from labelVocabulary.emotions ONLY when the trader's notes express that state or coachChecks show it unmistakably (a revenge re-entry right after a loss); never infer an emotion from the P&L alone; at most 2; otherwise none.
Use only keys from labelVocabulary. Labels the trader already set are kept either way.`

// VerdictTagOnly is the verdict line of a day or a week: an overall tag,
// no strategy or labels (a day spans mixed setups).
const VerdictTagOnly = `End with EXACTLY one final line - a machine-readable verdict (it is removed before saving):
<!-- tj-ai:verdict tag=<good|neutral|bad|none> -->
- tag: your honest read of the process over the whole period - good / neutral / bad. Use none ONLY when no such read applies.`

// LessonLine asks for the one lesson line the lessons ledger keeps, naming
// the mistake keys a review may use.
func LessonLine() string {
	return `Just before the verdict line, add EXACTLY one line naming the single most useful lesson (it is removed before saving and kept in the trader's lessons ledger):
<!-- tj-ai:lesson key=<` + strings.Join(ObservableMistakeKeys(), "|") + `|none> | <the lesson: one sentence, an instruction the trader can follow next time> -->
- key: the mistake the lesson is about, from that list, or none.`
}

// ObservableMistakeKeys are the mistake keys a review may name.
func ObservableMistakeKeys() []string {
	var keys []string
	for _, l := range journal.ObservableOptions(journal.KindMistake) {
		keys = append(keys, l.Key)
	}
	return keys
}

// LabelVocabulary is the vocabulary the model may fill labels from, as the
// context carries it: key, name and the description of its evidence.
func LabelVocabulary() map[string]any {
	kind := func(k string) []map[string]string {
		var out []map[string]string
		for _, l := range journal.ObservableOptions(k) {
			out = append(out, map[string]string{"key": l.Key, "name": l.Name, "description": l.Desc})
		}
		return out
	}
	return map[string]any{"mistakes": kind(journal.KindMistake), "emotions": kind(journal.KindEmotion)}
}

// ---- The glossary -------------------------------------------------------

// Glossary describes each piece of evidence the app can hand a model, once,
// so every prompt that names a key says the same thing about it.
var Glossary = map[string]string{
	"sessionAtEntry.windows":   "rows are [time, open, high, low, close], newest last; 15m covers the last sessions, 5m the last hours, 1m the last minutes",
	"sessionAtEntry.structure": "the session as a trader reads it as of the entry: the prior RTH range and close, the overnight range, the opening range, VWAP, ATR per timeframe and the last swing highs and lows",
	"dayMap":                   "the reference levels of the day: the strongest zones with what meets there, the nearest zones above and below the price, the expected range from the daily ATR around the session's anchor, and the day type with its lean; the scores rank the zones, they do not forecast",
	"coachChecks":              "rule-based observations computed from the trades (timing, size, heat against the planned stop, share of the best move kept, the trader's own norms): objective evidence to cite with its numbers, never the model's own discovery",
	"recentLessons":            "the lessons of the trader's latest reviews, newest first",
	"dayPlan":                  "the trader's pre-market plan with one check per rule: what the plan said, what the day did, held or not",
	"weeklyDigest":             "the week per decision, set against the week before, with keep and fix lists derived from the numbers and tallies of what recurred",
	"labelVocabulary":          "the mistakes and emotions a review may name, each with the evidence its description asks for",
}
