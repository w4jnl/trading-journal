// Package coach runs deterministic checks over a trading day: rule-based
// observations about behaviour (a re-entry right after a loss, a size-up
// after a loss, a stop that was not honoured, a winner cut short, a loser
// held, more decisions than usual, entries past the usual hour) with the
// numbers behind each one and, where a review label fits, the label it
// suggests. They feed the day page, the prepare-day result the AI reviewer
// reads, the API and MCP. Nothing here judges; the trader does.
package coach

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Trade is what the checks need from a closed trade.
type Trade struct {
	ID          int64
	AccountID   int64
	Account     string
	SymbolRoot  string
	Direction   string
	Qty         float64
	EntryPrice  float64
	ExitPrice   *float64
	EntryTime   time.Time
	ExitTime    time.Time
	NetPnl      *float64
	GrossPnl    *float64
	PlannedRisk *float64
	MaePnl      *float64
	MfePoints   *float64
}

// Label is the review label a finding suggests (internal/journal vocabulary).
type Label struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Finding is one observation.
type Finding struct {
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Detail   string  `json:"detail"`
	Severity string  `json:"severity"` // warn | info
	TradeIDs []int64 `json:"tradeIds"`
	Label    *Label  `json:"label,omitempty"`
}

// Baseline is the trader's own recent norm the day is read against.
type Baseline struct {
	Days                int     `json:"days"`                // trading days behind the medians
	MedianDecisions     float64 `json:"medianDecisions"`     // per day
	UsualLastEntry      string  `json:"usualLastEntry"`      // HH:MM, trading timezone; the 90th percentile of entry times
	MedianWinnerHoldSec float64 `json:"medianWinnerHoldSec"` // how long a winner is usually on
}

// Report is the day's checks.
type Report struct {
	Date      string       `json:"date"`
	Trades    int          `json:"trades"`
	Decisions int          `json:"decisions"`
	Baseline  Baseline     `json:"baseline"`
	Findings  []Finding    `json:"findings"`
	Plan      *PlanOutcome `json:"plan,omitempty"` // the day's pre-market plan against the day; nil without one
}

// Warnings counts the findings that read as mistakes rather than notes.
func (r *Report) Warnings() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == "warn" {
			n++
		}
	}
	return n
}

const (
	baselineWindowDays = 45 // calendar days of history read for the norms
	minBaselineDays    = 5  // trading days needed before the norm-based checks speak
	revengeWindow      = 60 * time.Second
	heldLoserFactor    = 3.0
	overtradeFactor    = 1.5
	overtradeFloor     = 10
	lateSessionSlack   = 30 * time.Minute
	leftOnTableKept    = 0.25
)

// Run loads the day's trades and the history behind the norms, and checks
// the day. day is a trading-timezone midnight; a trade belongs to the day
// of its exit. Hidden and retired accounts are left out, as in analytics.
func Run(ctx context.Context, db *sql.DB, userID int64, day time.Time, tz *time.Location) (*Report, error) {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, tz)
	from := day.AddDate(0, 0, -baselineWindowDays).UTC()
	to := day.AddDate(0, 0, 1).UTC()
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.account_id, a.name, t.symbol_root, t.direction, t.qty, t.entry_price, t.exit_price,
		       t.entry_time, t.exit_time, t.net_pnl, t.gross_pnl, t.planned_risk, t.mae_pnl, t.mfe_points
		FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ? AND a.active = 1 AND a.excluded_from_stats = 0
		  AND t.status = 'closed' AND t.exit_time >= ? AND t.exit_time < ?
		ORDER BY t.entry_time, t.id`, userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("coach: load trades: %w", err)
	}
	defer rows.Close()
	var all []Trade
	for rows.Next() {
		var t Trade
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Account, &t.SymbolRoot, &t.Direction, &t.Qty, &t.EntryPrice, &t.ExitPrice,
			&t.EntryTime, &t.ExitTime, &t.NetPnl, &t.GrossPnl, &t.PlannedRisk, &t.MaePnl, &t.MfePoints); err != nil {
			return nil, fmt.Errorf("coach: scan: %w", err)
		}
		all = append(all, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var today, history []Trade
	for _, t := range all {
		if t.ExitTime.In(tz).Before(day) {
			history = append(history, t)
		} else {
			today = append(today, t)
		}
	}
	plan, err := dayplan.Get(ctx, models.New(db), userID, day.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("coach: %w", err)
	}
	return Analyze(day, tz, today, history, plan), nil
}

// decision is a copy set or a lone trade: the unit the checks reason in.
type decision struct {
	Trades []Trade
	Entry  time.Time
	Exit   time.Time
	Net    float64
	Qty    float64
	Key    string
}

func decisions(trs []Trade) []decision {
	cs := make([]copyset.Trade, len(trs))
	for i, t := range trs {
		cs[i] = copyset.Trade{ID: t.ID, AccountID: t.AccountID, SymbolRoot: t.SymbolRoot, Direction: t.Direction, EntryTime: t.EntryTime}
	}
	sets, solos := copyset.Cluster(cs)
	groups := make([][]int, 0, len(sets)+len(solos))
	groups = append(groups, sets...)
	for _, k := range solos {
		groups = append(groups, []int{k})
	}
	out := make([]decision, 0, len(groups))
	for _, g := range groups {
		d := decision{Key: copyset.Key(trs[g[0]].SymbolRoot)}
		for i, k := range g {
			t := trs[k]
			d.Trades = append(d.Trades, t)
			if i == 0 || t.EntryTime.Before(d.Entry) {
				d.Entry = t.EntryTime
			}
			if i == 0 || t.ExitTime.After(d.Exit) {
				d.Exit = t.ExitTime
			}
			if t.NetPnl != nil {
				d.Net += *t.NetPnl
			}
			d.Qty += t.Qty
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Entry.Before(out[j].Entry) })
	return out
}

func ids(d decision) []int64 {
	out := make([]int64, 0, len(d.Trades))
	for _, t := range d.Trades {
		out = append(out, t.ID)
	}
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func mistake(key string) *Label {
	return &Label{Kind: journal.KindMistake, Key: key, Name: journal.LabelName(journal.KindMistake, key)}
}

// Analyze checks today's trades against the history (closed trades of the
// preceding weeks) and the day's plan (nil without one). Pure: Run feeds
// it from the database.
func Analyze(day time.Time, tz *time.Location, today, history []Trade, plan *dayplan.Plan) *Report {
	r := &Report{Date: day.Format("2006-01-02"), Trades: len(today)}
	ds := decisions(today)
	r.Decisions = len(ds)
	if plan != nil {
		out, findings := planOutcome(*plan, ds, today, tz)
		r.Plan = out
		defer func() { r.Findings = append(findings, r.Findings...) }() // the plan's own rules lead
	}

	// Norms from the history: decisions per trading day, the usual last
	// entry (90th percentile of entry times) and how long winners are on.
	perDay := map[string]int{}
	var entryMinutes, winnerHolds []float64
	for _, d := range decisions(history) {
		perDay[d.Exit.In(tz).Format("2006-01-02")]++
		lt := d.Entry.In(tz)
		entryMinutes = append(entryMinutes, float64(lt.Hour()*60+lt.Minute()))
		if d.Net > 0 {
			winnerHolds = append(winnerHolds, d.Exit.Sub(d.Entry).Seconds())
		}
	}
	counts := make([]float64, 0, len(perDay))
	for _, n := range perDay {
		counts = append(counts, float64(n))
	}
	r.Baseline = Baseline{Days: len(perDay), MedianDecisions: median(counts), MedianWinnerHoldSec: median(winnerHolds)}
	usualLast := percentile(entryMinutes, 0.9)
	if len(entryMinutes) > 0 {
		r.Baseline.UsualLastEntry = fmt.Sprintf("%02d:%02d", int(usualLast)/60, int(usualLast)%60)
	}
	if len(today) == 0 {
		return r
	}

	// Revenge entry: a decision opened within a minute of a losing exit on
	// the same underlying.
	for i, d := range ds {
		for j := i - 1; j >= 0; j-- {
			p := ds[j]
			if p.Key != d.Key || p.Net >= 0 {
				continue
			}
			if gap := d.Entry.Sub(p.Exit); gap >= 0 && gap <= revengeWindow {
				r.Findings = append(r.Findings, Finding{
					Key: "revenge-entry", Severity: "warn", Title: "Re-entry right after a loss",
					Detail:   fmt.Sprintf("%s %s opened %ds after a %.2f loss on the same market.", d.Trades[0].SymbolRoot, d.Trades[0].Direction, int(gap.Seconds()), p.Net),
					TradeIDs: ids(d), Label: mistake("revenge"),
				})
				break
			}
		}
	}

	// Size up after a loss: per account, a bigger position right after a
	// losing one. Copies report once, with every account's trade.
	byAccount := map[int64][]Trade{}
	for _, t := range today {
		byAccount[t.AccountID] = append(byAccount[t.AccountID], t)
	}
	sizeUp := map[int64]*Finding{} // keyed by the decision's first trade id
	var sizeUpOrder []int64
	for _, trs := range byAccount {
		sort.Slice(trs, func(i, j int) bool { return trs[i].EntryTime.Before(trs[j].EntryTime) })
		for i := 1; i < len(trs); i++ {
			prev, cur := trs[i-1], trs[i]
			if prev.NetPnl == nil || *prev.NetPnl >= 0 || cur.Qty <= prev.Qty {
				continue
			}
			dk := decisionOf(ds, cur.ID)
			f, ok := sizeUp[dk]
			if !ok {
				f = &Finding{
					Key: "size-up-after-loss", Severity: "warn", Title: "Size up after a loss",
					Detail: fmt.Sprintf("%s: %g contracts after a losing %g-contract trade on %s.", cur.SymbolRoot, cur.Qty, prev.Qty, cur.Account),
					Label:  mistake("oversized"),
				}
				sizeUp[dk] = f
				sizeUpOrder = append(sizeUpOrder, dk)
			}
			f.TradeIDs = append(f.TradeIDs, cur.ID)
		}
	}
	for _, k := range sizeUpOrder {
		f := sizeUp[k]
		if n := len(f.TradeIDs); n > 1 {
			f.Detail += fmt.Sprintf(" Same on %d accounts.", n)
		}
		r.Findings = append(r.Findings, *f)
	}

	// Past the stop: measured heat beyond the planned risk.
	for _, t := range today {
		if t.PlannedRisk != nil && *t.PlannedRisk > 0 && t.MaePnl != nil && *t.MaePnl > *t.PlannedRisk {
			r.Findings = append(r.Findings, Finding{
				Key: "past-stop", Severity: "warn", Title: "Price went past the planned stop",
				Detail:   fmt.Sprintf("%s on %s: %.2f of heat against a planned risk of %.2f.", t.SymbolRoot, t.Account, *t.MaePnl, *t.PlannedRisk),
				TradeIDs: []int64{t.ID}, Label: mistake("moved-stop"),
			})
		}
	}

	// Left on the table: a winner that kept under a quarter of its best move.
	for _, d := range ds {
		if d.Net <= 0 {
			continue
		}
		var realized, best float64
		measured := false
		for _, t := range d.Trades {
			if t.MfePoints == nil || t.ExitPrice == nil || *t.MfePoints <= 0 {
				continue
			}
			measured = true
			rp := *t.ExitPrice - t.EntryPrice
			if t.Direction == "short" {
				rp = -rp
			}
			realized += rp * t.Qty
			best += *t.MfePoints * t.Qty
		}
		if !measured || best <= 0 {
			continue
		}
		if kept := realized / best; kept < leftOnTableKept {
			r.Findings = append(r.Findings, Finding{
				Key: "left-on-table", Severity: "info", Title: "Winner cut short",
				Detail:   fmt.Sprintf("%s %s kept %.0f%% of the best move it saw.", d.Trades[0].SymbolRoot, d.Trades[0].Direction, kept*100),
				TradeIDs: ids(d), Label: mistake("early-exit"),
			})
		}
	}

	// The norm-based checks need enough history to mean anything.
	if r.Baseline.Days < minBaselineDays {
		return r
	}

	// Held a loser: a losing decision on for several times a usual winner.
	if hold := r.Baseline.MedianWinnerHoldSec; hold > 0 {
		for _, d := range ds {
			if d.Net >= 0 {
				continue
			}
			if held := d.Exit.Sub(d.Entry).Seconds(); held >= heldLoserFactor*hold {
				r.Findings = append(r.Findings, Finding{
					Key: "held-loser", Severity: "warn", Title: "Held a loser",
					Detail:   fmt.Sprintf("%s %s was on for %s and lost %.2f; your winners are usually out in %s.", d.Trades[0].SymbolRoot, d.Trades[0].Direction, durationText(held), d.Net, durationText(hold)),
					TradeIDs: ids(d), Label: mistake("held-loser"),
				})
			}
		}
	}

	// Overtraded: decisions well above the usual day (the plan's own cap,
	// when it has one, says it better).
	if m := r.Baseline.MedianDecisions; m > 0 && (plan == nil || plan.MaxTrades == nil) {
		limit := math.Max(overtradeFloor, overtradeFactor*m)
		if float64(len(ds)) > limit {
			var all []int64
			for _, d := range ds {
				all = append(all, ids(d)...)
			}
			r.Findings = append(r.Findings, Finding{
				Key: "overtraded", Severity: "warn", Title: "More decisions than usual",
				Detail:   fmt.Sprintf("%d decisions today against a usual %.0f per day.", len(ds), m),
				TradeIDs: all, Label: mistake("overtraded"),
			})
		}
	}

	// Late session: entries past the usual last entry (unless the plan set
	// its own stop time).
	if r.Baseline.UsualLastEntry != "" && (plan == nil || plan.StopTime == "") {
		cutoff := usualLast + lateSessionSlack.Minutes()
		var late []int64
		var first time.Time
		for _, d := range ds {
			lt := d.Entry.In(tz)
			if float64(lt.Hour()*60+lt.Minute()) > cutoff {
				if first.IsZero() {
					first = lt
				}
				late = append(late, ids(d)...)
			}
		}
		if len(late) > 0 {
			r.Findings = append(r.Findings, Finding{
				Key: "late-session", Severity: "info", Title: "Trading past the usual hour",
				Detail:   fmt.Sprintf("%d trade(s) entered after %s; your entries usually stop by %s.", len(late), first.Format("15:04"), r.Baseline.UsualLastEntry),
				TradeIDs: late,
			})
		}
	}
	return r
}

// decisionOf finds the decision holding a trade (its first trade's id).
func decisionOf(ds []decision, tradeID int64) int64 {
	for _, d := range ds {
		for _, t := range d.Trades {
			if t.ID == tradeID {
				return d.Trades[0].ID
			}
		}
	}
	return tradeID
}

func durationText(sec float64) string {
	switch {
	case sec < 90:
		return fmt.Sprintf("%.0fs", sec)
	case sec < 5400:
		return fmt.Sprintf("%.0f min", sec/60)
	default:
		return fmt.Sprintf("%.1f h", sec/3600)
	}
}
