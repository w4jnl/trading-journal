package push

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The Session activity starts by push on a phone that gave a start token
// once the day has a fill, updates on a change (urgent) and every five
// minutes otherwise, carries a closed trade as its alert with a badge
// push, and ends once flat and quiet after the close; a phone that
// turned the activity off gets none of it.
func TestSessionActivity(t *testing.T) {
	fx := newPushFixture(t, nil)
	state := &SessionState{Realized: 120.5, Trades: 2, Wins: 2, OpenCount: 1, Positions: []SessionPosition{{AccountID: 1, Symbol: "MESZ6", Direction: "long", Qty: 2, AvgEntry: 7000, Last: 7001, Unrealized: 10}},
		MaxTrades: 6, MaxLoss: 300, LossUsed: 0, LastFill: fx.now.Add(-time.Minute).Unix()}
	fx.s.State = func(context.Context, int64) (*SessionState, error) { return state, nil }
	tok := "0123456789abcdef0123456789abcdef"
	start := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := fx.s.SetActivities(fx.ctx, fx.uid, tok, &start, nil); err != nil {
		t.Fatal(err)
	}
	fx.s.Tick(fx.ctx) // first look: snapshot, and the activity starts
	got := fx.st.drain()
	if len(got) != 1 {
		t.Fatalf("pushes = %+v, want the start", got)
	}
	if got[0].pushType != "liveactivity" || got[0].path != "/3/device/"+start || got[0].aps["event"] != "start" || got[0].aps["attributes-type"] != sessionAttributesType ||
		got[0].aps["alert"].(map[string]any)["title"] != "Session" || got[0].aps["content-state"].(map[string]any)["realized"].(float64) != 120.5 || got[0].aps["stale-date"] == nil || got[0].priority != "10" {
		t.Errorf("start = %+v", got[0])
	}
	if attrs := got[0].aps["attributes"].(map[string]any); attrs["day"] != "2026-10-14" || attrs["rthStart"].(float64) == 0 {
		t.Errorf("attributes = %v", attrs)
	}
	// A second tick does not start it again; the phone reports the activity.
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 0 {
		t.Fatalf("started twice: %+v", got)
	}
	act := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := fx.s.SetActivities(fx.ctx, fx.uid, tok, nil, []Activity{{ID: "S1", Token: act, Kind: "session"}}); err != nil {
		t.Fatal(err)
	}
	// A change: an urgent update to the activity's token (plus the sync).
	state.Unrealized, state.Positions[0].Unrealized = 25, 25
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	got = fx.st.drain()
	var upd *sent
	for i := range got {
		if got[i].pushType == "liveactivity" {
			upd = &got[i]
		}
	}
	if upd == nil || upd.path != "/3/device/"+act || upd.aps["event"] != "update" || upd.priority != "10" || upd.aps["content-state"].(map[string]any)["unrealized"].(float64) != 25 {
		t.Fatalf("update = %+v (all %+v)", upd, got)
	}
	// Nothing changed and five minutes have not passed: silence. After
	// five minutes: a routine update at priority 5.
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 0 {
		t.Errorf("idle tick pushed: %+v", got)
	}
	fx.now = fx.now.Add(6 * time.Minute)
	fx.s.Tick(fx.ctx)
	got = fx.st.drain()
	if len(got) != 1 || got[0].aps["event"] != "update" || got[0].priority != "5" {
		t.Errorf("refresh = %+v", got)
	}
	// A closed trade rides the activity: an update with the alert and a
	// badge-only push, no TJ_TRADE notification.
	state.OpenCount, state.Positions, state.Trades, state.Realized = 0, nil, 3, 191.5
	fx.closeTrade(t, 9, 71, fx.now.Add(-30*time.Second))
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	got = fx.st.drain()
	kinds := map[string]int{}
	for _, g := range got {
		switch {
		case g.pushType == "liveactivity":
			al, ok := g.aps["alert"].(map[string]any)
			if !ok {
				continue // the routine update of the same tick
			}
			if al["title"] != "MES short" || al["body"] != "+71.00 · APEX-1" || g.aps["content-state"].(map[string]any)["alert"] != "MES short +71.00" {
				t.Errorf("activity alert = %+v", g)
			}
			kinds["activity"]++
		case g.pushType == "background":
			kinds["sync"]++
		case g.aps["alert"] == nil && g.aps["badge"] != nil:
			kinds["badge"]++
		default:
			kinds["notification"]++
		}
	}
	if kinds["activity"] < 1 || kinds["badge"] != 1 || kinds["sync"] != 1 || kinds["notification"] != 0 {
		t.Errorf("after the close: %v (%+v)", kinds, got)
	}
	// After the close, flat and quiet: the activity ends once.
	fx.now = time.Date(2026, 10, 14, 20, 20, 0, 0, time.UTC) // 15:20 Chicago
	state.LastFill = fx.now.Add(-15 * time.Minute).Unix()
	fx.s.Tick(fx.ctx)
	got = fx.st.drain()
	if len(got) != 1 || got[0].aps["event"] != "end" || got[0].aps["dismissal-date"] == nil {
		t.Errorf("end = %+v", got)
	}
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 0 {
		t.Errorf("ended twice: %+v", got)
	}
	if list, _ := fx.s.List(fx.ctx, fx.uid); len(list[0].Activities) != 0 {
		t.Errorf("the ended activity is still recorded: %+v", list[0].Activities)
	}
	// A phone that turned the activity off is left alone.
	if _, err := fx.s.SetPrefs(fx.ctx, fx.uid, tok, &Prefs{NoActivity: true}); err != nil {
		t.Fatal(err)
	}
	fx.now = time.Date(2026, 10, 15, 14, 30, 0, 0, time.UTC)
	state.OpenCount, state.LastFill = 1, fx.now.Unix()
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	for _, g := range fx.st.drain() {
		if g.pushType == "liveactivity" {
			t.Errorf("activity pushed to a phone that turned it off: %+v", g)
		}
	}
}

// The legs are ordered by the decision with the most at stake, kept
// together, and trimmed by whole sets: three decisions, twelve legs.
func TestSessionStateArrangesAndTrimsBySet(t *testing.T) {
	leg := func(a int64, s string, u float64, set ...int64) SessionPosition {
		return SessionPosition{AccountID: a, Symbol: s, Unrealized: u, Set: set}
	}
	st := &SessionState{Positions: []SessionPosition{
		leg(1, "MESZ6", 10, 1, 2, 3), leg(5, "MNQZ6", -25), leg(2, "MESZ6", 14, 1, 2, 3), leg(6, "MCLZ6", 4), leg(3, "MESZ6", 9, 1, 2, 3), leg(7, "ESZ6", 30),
	}}
	st.trim(time.Unix(100, 0))
	var got []string
	for _, p := range st.Positions {
		got = append(got, p.SetKey())
	}
	// The MES set (33 at stake) first, its largest leg leading; then ES 30, MNQ 25; MCL does not fit.
	want := []string{"1,2,3:MESZ6", "1,2,3:MESZ6", "1,2,3:MESZ6", "7:ESZ6", "5:MNQZ6"}
	if strings.Join(got, " ") != strings.Join(want, " ") || st.Positions[0].AccountID != 2 || st.Updated != 100 {
		t.Errorf("arranged = %v (lead %d), want %v", got, st.Positions[0].AccountID, want)
	}
	// A set that would cross the leg cap is kept whole up to it: 12 legs of one set, no more.
	wide := &SessionState{}
	for i := int64(1); i <= 14; i++ {
		wide.Positions = append(wide.Positions, leg(i, "MESZ6", float64(i), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14))
	}
	wide.Positions = append(wide.Positions, leg(20, "ESZ6", 1))
	wide.trim(time.Unix(1, 0))
	if len(wide.Positions) != liveMaxPositions || wide.Positions[0].AccountID != 14 {
		t.Errorf("wide: %d legs, lead %d", len(wide.Positions), wide.Positions[0].AccountID)
	}
	if b, _ := json.Marshal(wide); len(b) > 4096 {
		t.Errorf("twelve wide legs make %d bytes, over the 4 KB limit", len(b))
	}
	empty := &SessionState{}
	empty.trim(time.Unix(1, 0))
	if empty.Positions == nil || len(empty.Positions) != 0 {
		t.Errorf("empty positions must marshal as [], got %v", empty.Positions)
	}
}
