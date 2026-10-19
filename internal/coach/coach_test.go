package coach

import (
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

// mk builds a closed trade; entry/exit are seconds into the day's 14:00 hour.
func mk(id, acct int64, root, dir string, qty float64, entrySec, exitSec int, net float64, day time.Time) Trade {
	return Trade{
		ID: id, AccountID: acct, Account: "acct" + string(rune('0'+acct)), SymbolRoot: root, Direction: dir, Qty: qty,
		EntryPrice: 100, ExitPrice: f(100 + net/50), EntryTime: day.Add(14*time.Hour + time.Duration(entrySec)*time.Second),
		ExitTime: day.Add(14*time.Hour + time.Duration(exitSec)*time.Second), NetPnl: f(net), GrossPnl: f(net),
	}
}

func keys(r *Report) []string {
	var out []string
	for _, x := range r.Findings {
		out = append(out, x.Key)
	}
	return out
}

func TestAnalyzeRules(t *testing.T) {
	tz := time.UTC
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, tz)
	// History: ten days, four decisions a day (two accounts copying, so eight
	// trades), entries at 14:00-14:30, winners on for ~40s.
	var history []Trade
	id := int64(1000)
	for d := 1; d <= 10; d++ {
		past := day.AddDate(0, 0, -d)
		for k := 0; k < 4; k++ {
			for acct := int64(1); acct <= 2; acct++ {
				history = append(history, mk(id, acct, "MES", "short", 1, k*600, k*600+40, 5, past))
				id++
			}
		}
	}
	// Today: a loss, then a re-entry 20s later (revenge), with 3 contracts
	// after 1 (size up); a loser held 10 minutes; a winner that kept little
	// of its move; a late entry at 18:00; and enough decisions to overtrade.
	var today []Trade
	today = append(today, mk(1, 1, "MES", "short", 1, 0, 30, -20, day), mk(2, 2, "MES", "short", 1, 1, 31, -20, day))
	today = append(today, mk(3, 1, "MES", "short", 3, 50, 90, 8, day), mk(4, 2, "MES", "short", 3, 51, 91, 8, day))
	today = append(today, mk(5, 1, "MES", "long", 1, 600, 1200, -30, day)) // held 10 min, losing
	w := mk(6, 1, "NQ", "long", 1, 2000, 2060, 10, day)                    // won 0.2 pts, best move 4 pts
	w.MfePoints = f(4)
	today = append(today, w)
	p := mk(7, 2, "NQ", "long", 1, 2100, 2150, -5, day) // past the stop: heat 150 on a 100 risk
	p.PlannedRisk, p.MaePnl = f(100), f(150)
	today = append(today, p)
	for i := 0; i < 8; i++ { // filler decisions to pass 1.5 x the median of 4
		today = append(today, mk(100+int64(i), 1, "CL", "long", 1, 2500+i*60, 2530+i*60, 1, day))
	}
	today = append(today, mk(200, 1, "MES", "long", 1, 4*3600, 4*3600+30, 2, day)) // 18:00 entry

	r := Analyze(day, tz, today, history, nil)
	// 14 decisions: the first set, the held loser and p lost; the second set, w, the 8 CL and the late MES won.
	if r.DecisionWins != 11 || r.DecisionLosses != 3 {
		t.Errorf("decision wins/losses = %d/%d, want 11/3", r.DecisionWins, r.DecisionLosses)
	}
	if r.Trades != len(today) || r.Decisions != 14 || r.Baseline.Days != 10 || r.Baseline.MedianDecisions != 4 || r.Baseline.UsualLastEntry != "14:30" || r.Baseline.MedianWinnerHoldSec != 40 {
		t.Fatalf("report shape: trades %d decisions %d baseline %+v", r.Trades, r.Decisions, r.Baseline)
	}
	got := strings.Join(keys(r), ",")
	for _, want := range []string{"revenge-entry", "size-up-after-loss", "past-stop", "left-on-table", "held-loser", "overtraded", "late-session"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	for _, x := range r.Findings {
		switch x.Key {
		case "revenge-entry":
			if len(x.TradeIDs) != 2 || x.Label == nil || x.Label.Key != "revenge" || !strings.Contains(x.Detail, "19s after a -40.00 loss") {
				t.Errorf("revenge: %+v", x)
			}
		case "size-up-after-loss":
			if len(x.TradeIDs) != 2 || !strings.Contains(x.Detail, "3 contracts after a losing 1-contract") || !strings.Contains(x.Detail, "Same on 2 accounts") || x.Label.Key != "oversized" {
				t.Errorf("size up: %+v", x)
			}
		case "past-stop":
			if len(x.TradeIDs) != 1 || x.TradeIDs[0] != 7 || x.Label.Key != "moved-stop" {
				t.Errorf("past stop: %+v", x)
			}
		case "left-on-table":
			if x.TradeIDs[0] != 6 || x.Severity != "info" || !strings.Contains(x.Detail, "kept 5%") {
				t.Errorf("left on table: %+v", x)
			}
		case "held-loser":
			if x.TradeIDs[0] != 5 || !strings.Contains(x.Detail, "10 min") || !strings.Contains(x.Detail, "40s") {
				t.Errorf("held loser: %+v", x)
			}
		case "overtraded":
			if !strings.Contains(x.Detail, "14 decisions today against a usual 4") {
				t.Errorf("overtraded: %+v", x)
			}
		case "late-session":
			if x.TradeIDs[0] != 200 || !strings.Contains(x.Detail, "after 18:00") || !strings.Contains(x.Detail, "stop by 14:30") || x.Label != nil {
				t.Errorf("late: %+v", x)
			}
		}
	}
	if r.Warnings() != 5 {
		t.Errorf("warnings = %d, want 5 (two info findings)", r.Warnings())
	}

	// Without enough history the norm-based checks stay quiet; the rest speak.
	thin := Analyze(day, tz, today, history[:8], nil)
	got = strings.Join(keys(thin), ",")
	for _, no := range []string{"held-loser", "overtraded", "late-session"} {
		if strings.Contains(got, no) {
			t.Errorf("%s should need %d baseline days, got %s", no, minBaselineDays, got)
		}
	}
	if !strings.Contains(got, "revenge-entry") || !strings.Contains(got, "past-stop") {
		t.Errorf("history-free checks missing: %s", got)
	}
	// An empty day reports the baseline and nothing else.
	if e := Analyze(day, tz, nil, history, nil); e.Trades != 0 || len(e.Findings) != 0 || e.Baseline.Days != 10 {
		t.Errorf("empty day: %+v", e)
	}
}
