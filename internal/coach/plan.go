package coach

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/dayplan"
)

// PlanCheck is one rule of the day's plan read against the day.
type PlanCheck struct {
	Key    string `json:"key"`    // max-trades | stop-time | max-loss | bias
	Rule   string `json:"rule"`   // what the plan said
	Actual string `json:"actual"` // what the day did
	Held   bool   `json:"held"`
}

// PlanOutcome is the day's plan with its checks. Checks stay empty until
// the day has trades; WrittenLate / EditedLate flag a plan written or
// changed after the first entry, which makes it a record, not a plan.
type PlanOutcome struct {
	Plan        dayplan.Plan `json:"plan"`
	Checks      []PlanCheck  `json:"checks"`
	Held        int          `json:"held"`
	WrittenLate bool         `json:"writtenLate"`
	EditedLate  bool         `json:"editedLate"`
}

// Broken counts the checks that did not hold.
func (o *PlanOutcome) Broken() int { return len(o.Checks) - o.Held }

// planOutcome reads the plan against the day's decisions and returns the
// outcome plus a finding per broken rule.
func planOutcome(p dayplan.Plan, ds []decision, today []Trade, tz *time.Location) (*PlanOutcome, []Finding) {
	out := &PlanOutcome{Plan: p, Checks: []PlanCheck{}}
	if len(ds) == 0 {
		return out, nil
	}
	first := ds[0].Entry
	out.WrittenLate = p.CreatedAt.After(first)
	out.EditedLate = !out.WrittenLate && p.UpdatedAt.After(first)
	var findings []Finding
	add := func(c PlanCheck) {
		out.Checks = append(out.Checks, c)
		if c.Held {
			out.Held++
		}
	}

	if p.Bias != "" && p.Bias != "neutral" {
		var against []decision
		var withNet, againstNet float64
		for _, d := range ds {
			dir := d.Trades[0].Direction
			if p.Bias == "flat" || dir != p.Bias {
				against = append(against, d)
				againstNet += d.Net
			} else {
				withNet += d.Net
			}
		}
		rule := dayplan.BiasName(p.Bias) + " bias"
		actual := fmt.Sprintf("%d with it (%+.2f), %d against (%+.2f)", len(ds)-len(against), withNet, len(against), againstNet)
		if p.Bias == "flat" {
			rule = "Stay out"
			actual = fmt.Sprintf("%d decisions taken (%+.2f)", len(ds), againstNet)
		}
		add(PlanCheck{Key: "bias", Rule: rule, Actual: actual, Held: len(against) == 0})
		if len(against) > 0 {
			detail := fmt.Sprintf("%d decision(s) against the planned %s bias, net %+.2f.", len(against), strings.ToLower(dayplan.BiasName(p.Bias)), againstNet)
			if p.Bias == "flat" {
				detail = fmt.Sprintf("The plan was to stay out; %d decision(s) taken, net %+.2f.", len(against), againstNet)
			}
			findings = append(findings, Finding{Key: "plan-bias", Severity: "warn", Title: "Traded against the plan's bias",
				Detail: detail, TradeIDs: idsOf(against), Label: mistake("against-plan")})
		}
	}

	if p.MaxTrades != nil {
		limit := int(*p.MaxTrades)
		add(PlanCheck{Key: "max-trades", Rule: fmt.Sprintf("At most %d decision(s)", limit), Actual: fmt.Sprintf("took %d", len(ds)), Held: len(ds) <= limit})
		if len(ds) > limit {
			extra := ds[limit:]
			findings = append(findings, Finding{Key: "plan-max-trades", Severity: "warn", Title: "More decisions than the plan allowed",
				Detail:   fmt.Sprintf("%d decisions against a plan of %d; decision %d opened at %s.", len(ds), limit, limit+1, extra[0].Entry.In(tz).Format("15:04")),
				TradeIDs: idsOf(extra), Label: mistake("overtraded")})
		}
	}

	if stop, ok := p.StopMinutes(); ok {
		var late []decision
		for _, d := range ds {
			lt := d.Entry.In(tz)
			if lt.Hour()*60+lt.Minute() > stop {
				late = append(late, d)
			}
		}
		last := ds[len(ds)-1].Entry.In(tz).Format("15:04")
		for _, d := range ds {
			if l := d.Entry.In(tz).Format("15:04"); l > last {
				last = l
			}
		}
		add(PlanCheck{Key: "stop-time", Rule: "Done by " + p.StopTime, Actual: "last entry " + last, Held: len(late) == 0})
		if len(late) > 0 {
			findings = append(findings, Finding{Key: "plan-stop-time", Severity: "warn", Title: "Traded past the plan's stop time",
				Detail:   fmt.Sprintf("%d decision(s) entered after the planned stop at %s, the first at %s.", len(late), p.StopTime, late[0].Entry.In(tz).Format("15:04")),
				TradeIDs: idsOf(late), Label: mistake("against-plan")})
		}
	}

	if p.MaxLoss != nil {
		limit := *p.MaxLoss
		// Running net per account, by exit time: the worst point any
		// account reached, and when one first crossed the limit.
		trs := append([]Trade(nil), today...)
		sort.SliceStable(trs, func(i, j int) bool { return trs[i].ExitTime.Before(trs[j].ExitTime) })
		cum := map[int64]float64{}
		worst, worstAcct := 0.0, ""
		var breach time.Time
		breachAcct := ""
		for _, t := range trs {
			if t.NetPnl == nil {
				continue
			}
			cum[t.AccountID] += *t.NetPnl
			if cum[t.AccountID] < worst {
				worst, worstAcct = cum[t.AccountID], t.Account
			}
			if breach.IsZero() && cum[t.AccountID] <= -limit {
				breach, breachAcct = t.ExitTime, t.Account
			}
		}
		rule := fmt.Sprintf("Stop at %.2f down per account", limit)
		switch {
		case breach.IsZero():
			actual := "never down"
			if worst < 0 {
				actual = fmt.Sprintf("worst account %.2f (%s)", worst, worstAcct)
			}
			add(PlanCheck{Key: "max-loss", Rule: rule, Actual: actual, Held: true})
		default:
			var after []decision
			for _, d := range ds {
				if d.Entry.After(breach) {
					after = append(after, d)
				}
			}
			at := breach.In(tz).Format("15:04")
			if len(after) == 0 {
				add(PlanCheck{Key: "max-loss", Rule: rule, Actual: fmt.Sprintf("hit at %s on %s, stopped there", at, breachAcct), Held: true})
			} else {
				add(PlanCheck{Key: "max-loss", Rule: rule, Actual: fmt.Sprintf("hit at %s on %s, %d decision(s) after", at, breachAcct, len(after)), Held: false})
				findings = append(findings, Finding{Key: "plan-max-loss", Severity: "warn", Title: "Kept trading past the plan's max loss",
					Detail:   fmt.Sprintf("%s was %.2f down at %s; %d decision(s) followed.", breachAcct, limit, at, len(after)),
					TradeIDs: idsOf(after), Label: mistake("against-plan")})
			}
		}
	}
	return out, findings
}

func idsOf(ds []decision) []int64 {
	var out []int64
	for _, d := range ds {
		out = append(out, ids(d)...)
	}
	return out
}
