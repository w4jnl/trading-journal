package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- server-side AI analysis -------------------------------------------------
//
// The inward counterpart to the MCP server: instead of an external AI calling our
// tools, the app gathers the same context (trades + chart + strategy playbooks via
// the existing journalDetailJSON / prepareDayEntries helpers) and asks the configured
// AI endpoint for the analysis text, then writes it with the same append-only +
// tj-ai marker semantics. No model tool-calling required, so any local model works.

const aiSystemBase = `You are a trading-journal analyst reviewing the user's own trades. You receive JSON context for one journal entry: its trade(s) (symbol, direction, qty, entry/exit price and time, P&L), a day summary, a compact chart (entry/exit averages + markers), and the assigned strategy playbook if any.

Write a concise, specific review in Markdown, grounded in the numbers - not generic advice. Use the SAME scannable structure every time, WHETHER OR NOT a strategy is provided: a short bullet list, one bullet per section, each led by a bold label. Never collapse the review into a single paragraph. Do NOT use top-level headings (the review is inserted under an existing heading) - use bold labels and optional nested bullets only.

For a single trade, cover these sections (omit one only if there is genuinely nothing to say):
- **Execution:** entry vs the move (early / late / chasing?), exit (left a runner? cut a winner? stop vs adverse excursion?).
- **Risk & sizing:** size vs the rest of the day; was the loss/gain in a normal range?
- **Strategy:** if a playbook is provided, did the trade follow its setup / entry / exit / risk rules? Call out deviations explicitly. If NO strategy is tagged, name the closest match from the available strategies (use the trade and the trader's notes as evidence) or say plainly that none of them fits - then give the rest of the review in full.
- **Verdict & lesson:** an honest good / neutral / bad read and one concrete, actionable takeaway.

For a group or a whole day, keep the same bold-labelled bullet structure but focus on what repeated, copytrader consistency across accounts, the net result, and one concrete thing to fix.

Interpret the numbers; do not just restate them. Never invent prices or times not present in the context. Output only the Markdown review - no preamble such as "Here is the analysis".`

// aiSystemPrompt returns the analyst system prompt. It always asks for a trailing
// machine-readable verdict line; analyzeEntry uses it to fill in an UNSET tag/strategy
// (never overwriting your choice) and only when one clearly fits - the model returns
// "none" otherwise, so nothing is forced. language (e.g. "Czech"), when non-empty, makes
// the model write the review prose in that language; empty = the model default (English).
func aiSystemPrompt(language string) string {
	verdict := "\n\n" + `End with EXACTLY one final line - a machine-readable verdict (it is removed before saving):
<!-- tj-ai:verdict tag=<good|neutral|bad|none> strategy=<id|none> -->
- tag: your honest read - good / neutral / bad. Use none ONLY when a good/neutral/bad verdict genuinely does not apply.
- strategy: identify the SINGLE best-fitting entry from "availableStrategies" and give its id or exact name. Weigh the trade (structure, direction, timing) AND the trader's own notes - a note like "3bar" points to a "Three-Bar Reversal". The trader has NOT pre-tagged one, so choosing the fit is YOUR job: do not answer none just because nothing was tagged or you are not fully certain. Use none only when nothing in the list is plausibly related (and always for a whole day or a group of mixed setups). Use only an id or name from that list; never invent one.`
	if language == "" {
		return aiSystemBase + verdict
	}
	// Local models tend to default to English when the language directive is buried at the
	// end of a long English prompt, so LEAD and CLOSE with it. The verdict line is parsed,
	// so it is the explicit exception that stays in English.
	lead := "WRITE YOUR ENTIRE REVIEW IN " + language + ". Every sentence, every bold section label, and every bullet must be in " + language + " - even though these instructions are written in English. The ONLY part that stays in English is the final machine-readable verdict line (the literal <!-- tj-ai:verdict ... --> comment and its keywords).\n\n"
	tail := "\n\nReminder: everything above the verdict line - all prose and labels - must be written in " + language + ", not English."
	return lead + aiSystemBase + verdict + tail
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
//	<!-- tj-ai:verdict tag=good strategy=12 -->
//	<!-- tj-ai:verdict tag=good strategy=ORB VWAP -->
//
// tag is a clean enum; strategy may be an id, a name (possibly with spaces), or none.
var (
	aiVerdictRe    = regexp.MustCompile(`(?i)<!--\s*tj-ai:verdict\s+([^>]*?)\s*-->`)
	verdictTagRe   = regexp.MustCompile(`(?i)\btag\s*=\s*"?(good|neutral|bad)\b`)
	verdictTagKVRe = regexp.MustCompile(`(?i)\btag\s*=\s*"?(?:good|neutral|bad|none)"?`)
)

type aiVerdict struct {
	Tag      string
	Strategy string // raw token: an id, a name, or "none"/"" - resolve with resolveStrategyID
}

func parseAIVerdict(s string) aiVerdict {
	var v aiVerdict
	m := aiVerdictRe.FindStringSubmatch(s)
	if m == nil {
		return v
	}
	inner := strings.TrimSpace(m[1])
	if tm := verdictTagRe.FindStringSubmatch(inner); tm != nil {
		v.Tag = strings.ToLower(tm[1])
	}
	// Strip the tag k=v so it can't bleed into a strategy NAME, then take everything after
	// "strategy=" as the raw value (an id, or a name which may contain spaces).
	rest := verdictTagKVRe.ReplaceAllString(inner, " ")
	if i := strings.Index(strings.ToLower(rest), "strategy="); i >= 0 {
		v.Strategy = strings.Trim(strings.TrimSpace(rest[i+len("strategy="):]), `"'`)
	}
	return v
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

// aiEntryContext builds the user-prompt JSON: the entry's trades + day summary +
// compact chart, the assigned strategy playbook, and the candidate strategies the model
// may pick from (offered for every entry so any analysis can suggest one).
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
	out := map[string]any{"entry": detail}

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

// analyzeEntry generates analysis for one entry and appends it (+ the tj-ai marker) to
// the body. It also fills in an UNSET tag (any scope) and an UNSET strategy (trade/group
// scope only - a day/week spans mixed setups) from the model's verdict, but only when one
// clearly fits; the model returns "none" otherwise, so nothing is forced and a tag/strategy
// you set is never overwritten. force re-analyzes even if already marked.
func (h *Handlers) analyzeEntry(ctx context.Context, entryID int64, force bool) (aiAnalyzeResult, error) {
	res := aiAnalyzeResult{EntryID: entryID}
	if h.AI == nil {
		return res, fmt.Errorf("ai analysis is disabled")
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
	out, err := h.AI.Generate(ctx, aiSystemPrompt(lang), userPrompt)
	if err != nil {
		return res, fmt.Errorf("ai generate: %w", err)
	}

	// Fill an unset tag/strategy from the verdict. The assign helpers enforce the rules:
	// only-if-unset (WHERE ... IS NULL, so a value you set is never overwritten) and, for
	// strategy, trade/group scope only (a day/week spans mixed setups - assignStrategyIfUnset
	// rejects it, which we ignore here). only-if-fitting comes from the model returning
	// "none" when nothing clearly applies. The strategy token is resolved by id OR name
	// (resolveStrategyID) - models often echo the name rather than the numeric id.
	v := parseAIVerdict(out)
	body := stripAIVerdict(out)
	if v.Tag != "" {
		if ok, err := h.assignTagIfUnset(ctx, entryID, v.Tag); err == nil && ok {
			res.Tag = v.Tag
		}
	}
	var resolvedSID int64
	if v.Strategy != "" {
		strats, _ := h.Queries.ListActiveStrategies(ctx, userID(ctx))
		if resolvedSID = resolveStrategyID(v.Strategy, strats); resolvedSID > 0 {
			if ok, err := h.assignStrategyIfUnset(ctx, entryID, resolvedSID); err == nil && ok {
				res.StrategyID = resolvedSID
			}
		}
	}
	// Visible with -d. Reveals whether the model returned a tag/strategy at all, whether the
	// strategy token resolved to an id, and whether each was actually applied (vs already set).
	slog.Debug("ai analysis verdict",
		"entry", entryID, "scope", e.Scope,
		"tag", v.Tag, "tagAssigned", res.Tag != "",
		"strategyToken", v.Strategy, "strategyResolved", resolvedSID, "strategyAssigned", res.StrategyID)
	note := "### AI review — " + time.Now().In(tz).Format("2006-01-02") + "\n\n" +
		strings.TrimSpace(body) + "\n\n<!-- " + aiMarkerLocal + " -->"
	if err := h.appendOrSeedJournalNote(ctx, e, note); err != nil {
		return res, fmt.Errorf("append analysis: %w", err)
	}
	return res, nil
}

// runDayAnalysis is the day-job body: cluster + create the day's entries
// (prepareDayEntries), analyze each, then add a day-level synthesis. Reports progress and
// stops promptly when ctx is cancelled. force re-analyzes already-marked entries too;
// otherwise marked entries are skipped (which also makes a re-run resume where a cancelled
// run left off).
func (h *Handlers) runDayAnalysis(ctx context.Context, dateStr string, force bool, progress func(done, total int, current string)) error {
	if h.AI == nil {
		return fmt.Errorf("ai analysis is disabled")
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
		if _, err := h.analyzeEntry(ctx, pe.EntryID, force); err != nil {
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
			if _, err := h.analyzeEntry(ctx, dayID, force); err != nil {
				return err
			}
		}
	}
	done++
	progress(done, total, "")
	return nil
}
