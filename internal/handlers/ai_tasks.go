package handlers

import "strings"

// ---- AI tasks and their difficulty tiers ---------------------------------
//
// Every AI call names its task, and the task's tier picks the model and the
// effort it runs at: quick where the trader is waiting on it, standard for
// the volume path, deep where the output is read for weeks. A trader maps
// each tier to a model and an effort of their own provider (Settings > AI);
// a blank tier is the main model and effort, so with nothing set every task
// runs as before, and the same model may serve every tier at different
// efforts. Trade and group reviews share a tier on purpose: they run back to
// back inside a day run and share the cached prompt prefix, which a
// different model or effort would start over.

// aiTask is one kind of AI call.
type aiTask string

const (
	aiTaskReviewTrade       aiTask = "review_trade"
	aiTaskReviewGroup       aiTask = "review_group"
	aiTaskReviewDay         aiTask = "review_day"
	aiTaskReviewWeek        aiTask = "review_week"
	aiTaskPlanDraft         aiTask = "plan_draft"
	aiTaskPositionLevels    aiTask = "position_levels"
	aiTaskStrategyDraft     aiTask = "strategy_draft"
	aiTaskStrategyDiscovery aiTask = "strategy_discovery"
	aiTaskLab               aiTask = "lab"
	aiTaskGexExplain        aiTask = "gex_explain"
)

// aiTier is a difficulty tier a task runs at.
type aiTier string

const (
	aiTierQuick    aiTier = "quick"
	aiTierStandard aiTier = "standard"
	aiTierDeep     aiTier = "deep"
)

// aiTiers are the tiers in display order, lightest first.
var aiTiers = []aiTier{aiTierQuick, aiTierStandard, aiTierDeep}

// aiTierOf parses a tier name from a form.
func aiTierOf(s string) (aiTier, bool) {
	for _, t := range aiTiers {
		if string(t) == s {
			return t, true
		}
	}
	return "", false
}

// aiTaskSpec is a task's tier and how the settings page names it.
type aiTaskSpec struct {
	Task aiTask
	Tier aiTier
	Name string
}

// aiTaskSpecs classifies every task, in the order the settings page lists
// them inside a tier.
var aiTaskSpecs = []aiTaskSpec{
	{aiTaskPositionLevels, aiTierQuick, "the stop and target suggestion on an open position"},
	{aiTaskGexExplain, aiTierQuick, "the gamma map explanation"},
	{aiTaskReviewTrade, aiTierStandard, "trade reviews"},
	{aiTaskReviewGroup, aiTierStandard, "group reviews"},
	{aiTaskPlanDraft, aiTierStandard, "the day plan draft"},
	{aiTaskReviewDay, aiTierDeep, "day reviews"},
	{aiTaskReviewWeek, aiTierDeep, "week reviews"},
	{aiTaskStrategyDraft, aiTierDeep, "strategy drafts"},
	{aiTaskStrategyDiscovery, aiTierDeep, "finding strategies in the untagged trades"},
	{aiTaskLab, aiTierDeep, "the lab's explanation"},
}

// Tier is the tier t runs at; an unlisted task is standard.
func (t aiTask) Tier() aiTier {
	for _, s := range aiTaskSpecs {
		if s.Task == t {
			return s.Tier
		}
	}
	return aiTierStandard
}

// aiTaskForScope is the review task for a journal entry's scope.
func aiTaskForScope(scope string) aiTask {
	switch scope {
	case "day":
		return aiTaskReviewDay
	case "week":
		return aiTaskReviewWeek
	case "trade":
		return aiTaskReviewTrade
	default: // the group scopes
		return aiTaskReviewGroup
	}
}

// aiTierView is one tier as Settings > AI shows it: what it covers and the
// model and effort the trader mapped to it ("" = the main setting).
type aiTierView struct {
	Tier   aiTier
	Label  string
	Covers string
	Model  string
	Effort string
}

// aiTierViews lists the tiers with their tasks, filled from the stored rows.
func aiTierViews(rows map[aiTier][2]string) []aiTierView {
	labels := map[aiTier]string{aiTierQuick: "Quick", aiTierStandard: "Standard", aiTierDeep: "Deep"}
	out := make([]aiTierView, 0, len(aiTiers))
	for _, t := range aiTiers {
		var covers []string
		for _, s := range aiTaskSpecs {
			if s.Tier == t {
				covers = append(covers, s.Name)
			}
		}
		v := aiTierView{Tier: t, Label: labels[t], Covers: strings.Join(covers, ", ")}
		if r, ok := rows[t]; ok {
			v.Model, v.Effort = r[0], r[1]
		}
		out = append(out, v)
	}
	return out
}
