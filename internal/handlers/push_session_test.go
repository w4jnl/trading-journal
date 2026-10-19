package handlers

import (
	"encoding/json"
	"testing"

	"github.com/w4jnl/trading-journal/internal/push"
)

// The activity's state follows the day: the open position and its fills
// first, the closed trade after; and three positions fit the 4 KB limit.
func TestSessionState(t *testing.T) {
	fx := newPositionFixture(t)
	st, err := fx.h.SessionState(fx.ctx, fx.uid)
	if err != nil {
		t.Fatal(err)
	}
	if st.OpenCount != 1 || st.OpenSets != 1 || st.Trades != 0 || st.Decisions != 0 || len(st.Positions) != 1 || st.Positions[0].Symbol != "ESM6" || st.Positions[0].Qty != 3 || len(st.Positions[0].Set) != 0 || st.LastFill == 0 || !st.Stale {
		t.Errorf("open state = %+v", st)
	}
	closeFixturePosition(t, fx, 7261)
	st, err = fx.h.SessionState(fx.ctx, fx.uid)
	if err != nil {
		t.Fatal(err)
	}
	if st.OpenCount != 0 || st.OpenSets != 0 || st.Trades != 1 || st.Decisions != 1 || st.SetWins != 1 || st.SetLosses != 0 || st.Realized <= 0 || st.LastFill == 0 {
		t.Errorf("closed state = %+v", st)
	}
	// A copied decision: the set is every leg's account id, sorted, the same on each; openSets counts it once.
	legs := []apiPosition{{AccountID: 9, Symbol: "MESZ6", SetLegs: []int64{7}}, {AccountID: 7, Symbol: "MESZ6", SetLegs: []int64{9}}, {AccountID: 7, Symbol: "MNQZ6", SetLegs: []int64{}}}
	if a, b := sessionSet(legs[0]), sessionSet(legs[1]); len(a) != 2 || a[0] != 7 || a[1] != 9 || b[0] != 7 || b[1] != 9 || sessionSet(legs[2]) != nil {
		t.Errorf("sets = %v %v %v", a, b, sessionSet(legs[2]))
	}
	if n := openSets(legs); n != 2 {
		t.Errorf("openSets = %d, want 2 (the MES set and the lone MNQ)", n)
	}
	big := push.SessionState{Realized: -12345.67, Unrealized: 8901.23, Trades: 12, Wins: 7, Losses: 5, MaxTrades: 10, LossUsed: 2345.67, MaxLoss: 3000, OpenCount: 3, Nudges: 2, Alert: "MNQ short -1234.50",
		Positions: []push.SessionPosition{{AccountID: 1234567, Symbol: "MNQZ6", Direction: "short", Qty: 12, AvgEntry: 24010.25, Last: 24018.5, Unrealized: -1234.5, TicksToStop: -12.5, Beyond: true},
			{AccountID: 1234567, Symbol: "MESZ6", Direction: "long", Qty: 10, AvgEntry: 7766.25, Last: 7767.25, Unrealized: 123.75, TicksToStop: 49},
			{AccountID: 1234567, Symbol: "MCLZ6", Direction: "long", Qty: 10, AvgEntry: 80.12, Last: 80.5, Unrealized: 380, TicksToStop: 30, Set: []int64{1234567, 2345678, 3456789}}}}
	b, _ := json.Marshal(big)
	if len(b) > 4096 {
		t.Errorf("content state is %d bytes, over the 4 KB limit", len(b))
	}
}
