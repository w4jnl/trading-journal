package push

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/markethours"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/push/apns"
)

// ---- The Session Live Activity ---------------------------------------------
//
// One activity per trading day on the phone's Lock Screen and in the
// Dynamic Island: the day's realised and unrealised P&L, what is open,
// how much of the plan is used, the nudges waiting. The server starts it
// by push-to-start on the first fill of the day (once per phone and day),
// updates it after every change and every five minutes while a position
// is open (prices move without fills), carries a closed trade as its
// alert instead of a notification unless the phone asked otherwise, and
// ends it once the day is flat and quiet after the close, or at the
// Globex break. Every update sets a stale date, so a phone that misses
// the end still greys the card. The state comes from the handlers (they
// price positions): Service.State.

// SessionState is the activity's content state; the keys are the Swift
// property names and every field is always present. It must stay small
// (ActivityKit allows 4 KB): the legs of at most three decisions, twelve
// legs in all. The counts come twice, per trade and per decision (a copy
// set counts once), so the phone shows whichever its trades list shows.
type SessionState struct {
	Realized   float64           `json:"realized"`
	Unrealized float64           `json:"unrealized"`
	Trades     int               `json:"trades"`
	Wins       int               `json:"wins"`
	Losses     int               `json:"losses"`
	Decisions  int               `json:"decisions"` // closed, a copy set once
	SetWins    int               `json:"setWins"`
	SetLosses  int               `json:"setLosses"`
	MaxTrades  int               `json:"maxTrades"` // 0: no cap
	LossUsed   float64           `json:"lossUsed"`
	MaxLoss    float64           `json:"maxLoss"` // 0: no budget
	Positions  []SessionPosition `json:"positions"`
	OpenCount  int               `json:"openCount"` // legs
	OpenSets   int               `json:"openSets"`  // decisions among them
	Nudges     int               `json:"nudges"`    // unseen
	LastFill   int64             `json:"lastFill"`
	Updated    int64             `json:"updated"`
	Stale      bool              `json:"stale"`
	Alert      string            `json:"alert"`
}

// SessionPosition is one open position in the activity: a leg. Set names
// the copied decision it belongs to as the sorted account ids of every
// leg (this one included; the same value on each), empty for a position
// on its own, so the phone can fold the legs into one row.
type SessionPosition struct {
	AccountID   int64   `json:"a"`
	Symbol      string  `json:"s"`
	Direction   string  `json:"d"`
	Qty         float64 `json:"q"`
	AvgEntry    float64 `json:"avg"`
	Last        float64 `json:"last"`
	Unrealized  float64 `json:"u"`
	TicksToStop float64 `json:"tts"`
	Beyond      bool    `json:"beyond"`
	Set         []int64 `json:"set"`
}

// SetKey names the decision a leg belongs to: its set and contract, or
// the leg itself when it is on its own.
func (p SessionPosition) SetKey() string {
	if len(p.Set) == 0 {
		return fmt.Sprintf("%d:%s", p.AccountID, p.Symbol)
	}
	var b strings.Builder
	for i, a := range p.Set {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(a, 10))
	}
	b.WriteByte(':')
	b.WriteString(p.Symbol)
	return b.String()
}

// SessionAttributes are the activity's static facts.
type SessionAttributes struct {
	Day        string `json:"day"`
	TZ         string `json:"tz"`
	RthStart   int64  `json:"rthStart"`
	RthEnd     int64  `json:"rthEnd"`
	ExchangeTZ string `json:"exchangeTz"` // the exchange's zone, so a face shows the hours in it and the phone's
}

const (
	sessionAttributesType = "SessionActivityAttributes"
	sessionKind           = "session"
	liveMaxSets           = 3  // decisions shown
	liveMaxPositions      = 12 // legs in all, for the 4 KB payload
	liveStaleAfter        = 10 * time.Minute
	liveRefresh           = 5 * time.Minute
	liveQuietAfter        = 10 * time.Minute
	liveDismissAfter      = 15 * time.Minute
)

// StateFunc gives the activity's state for a user; nil when there is
// nothing to show.
type StateFunc func(ctx context.Context, uid int64) (*SessionState, error)

// liveMemory is the per-user activity memory of the triggers.
type liveMemory struct {
	lastSent time.Time
	lastKey  string
	ended    string // the day an end was sent, so it is not sent again
}

// trim bounds the state for the payload and stamps it: the legs of the
// three decisions with the most at stake, whole sets only.
func (st *SessionState) trim(now time.Time) {
	st.Arrange()
	kept, sets := 0, 0
	last := ""
	for _, p := range st.Positions {
		if k := p.SetKey(); k != last {
			if sets == liveMaxSets {
				break
			}
			sets++
			last = k
		}
		if kept == liveMaxPositions {
			break
		}
		kept++
	}
	st.Positions = st.Positions[:kept]
	if st.Positions == nil {
		st.Positions = []SessionPosition{}
	}
	st.Updated = now.Unix()
}

// Arrange orders the legs by what is at stake: the decision with the
// largest unrealised exposure (summed over its legs) first, its legs
// together and the largest first, so the ones that fit are the ones that
// matter and the phone can fold a set from consecutive legs.
func (st *SessionState) Arrange() {
	exposure := map[string]float64{}
	for _, p := range st.Positions {
		exposure[p.SetKey()] += math.Abs(p.Unrealized)
	}
	sort.SliceStable(st.Positions, func(i, j int) bool {
		a, b := st.Positions[i], st.Positions[j]
		ka, kb := a.SetKey(), b.SetKey()
		if ka != kb {
			if exposure[ka] != exposure[kb] {
				return exposure[ka] > exposure[kb]
			}
			return ka < kb
		}
		return math.Abs(a.Unrealized) > math.Abs(b.Unrealized)
	})
}

// key is the state without the stamp, for change detection.
func (st SessionState) key() string {
	st.Updated = 0
	b, _ := json.Marshal(st)
	return string(b)
}

// sessionActive reports whether the day's activity should be running.
func (s *Service) sessionActive(st *SessionState, now time.Time) bool {
	if st == nil || !markethours.InGlobexHours(now, s.chicago()) {
		return false
	}
	return st.Trades > 0 || st.OpenCount > 0
}

// sessionEnds reports whether a running activity should end: flat and
// quiet after the close, or past the Globex break.
func (s *Service) sessionEnds(st *SessionState, now time.Time) bool {
	if st == nil || !markethours.InGlobexHours(now, s.chicago()) {
		return true
	}
	ct := now.In(s.chicago())
	mins := ct.Hour()*60 + ct.Minute()
	if st.OpenCount > 0 {
		return false
	}
	lastFill := time.Unix(st.LastFill, 0)
	quiet := now.Sub(lastFill) > liveQuietAfter
	return mins >= 15*60 && quiet
}

// sessionAttributes are today's facts.
func (s *Service) sessionAttributes(now time.Time) SessionAttributes {
	ct := now.In(s.chicago())
	start := time.Date(ct.Year(), ct.Month(), ct.Day(), 8, 30, 0, 0, s.chicago())
	end := time.Date(ct.Year(), ct.Month(), ct.Day(), 15, 0, 0, 0, s.chicago())
	return SessionAttributes{Day: s.today(), TZ: s.tz().String(), RthStart: start.Unix(), RthEnd: end.Unix(), ExchangeTZ: s.chicago().String()}
}

// liveTick is the activity's look at a user: start, update or end.
func (s *Service) liveTick(ctx context.Context, uid int64, changed bool) {
	if !s.Enabled() || s.State == nil {
		return
	}
	now := s.now()
	st := s.state(uid)
	s.mu.Lock()
	mem := st.live
	due := changed || now.Sub(mem.lastSent) >= liveRefresh
	s.mu.Unlock()
	if !due {
		return
	}
	targets, err := s.targets(ctx, uid)
	if err != nil || len(targets) == 0 {
		return
	}
	state, err := s.State(ctx, uid)
	if err != nil {
		s.log().Warn("push: session state", "user", uid, "err", err)
		return
	}
	if state != nil {
		state.trim(now)
	}
	active := s.sessionActive(state, now)
	ends := s.sessionEnds(state, now)
	key := ""
	if state != nil {
		key = state.key()
	}
	s.mu.Lock()
	same := key == mem.lastKey && now.Sub(mem.lastSent) < liveRefresh
	mem.lastSent, mem.lastKey = now, key
	st.live = mem
	s.mu.Unlock()
	for _, tg := range targets {
		if tg.prefs != nil && tg.prefs.NoActivity {
			continue
		}
		acts, _ := s.Q.ListDeviceActivities(ctx, tg.row.ID)
		var running []models.DeviceActivity
		for _, a := range acts {
			if a.Kind == sessionKind {
				running = append(running, a)
			}
		}
		switch {
		case len(running) > 0 && ends:
			for _, a := range running {
				_ = s.sendActivity(ctx, tg, a.Token, apns.Notification{Live: &apns.Live{Event: "end", ContentState: state, Dismissal: now.Add(liveDismissAfter)}}, "session end")
				_ = s.Q.DeleteDeviceActivityByToken(ctx, a.Token)
			}
		case len(running) > 0:
			if same && !changed {
				continue
			}
			for _, a := range running {
				_ = s.sendActivity(ctx, tg, a.Token, apns.Notification{Live: &apns.Live{Event: "update", ContentState: state, Stale: now.Add(liveStaleAfter), Urgent: changed}}, "session update")
			}
		case active && !ends && tg.row.ActivityStartToken != nil && *tg.row.ActivityStartToken != "":
			n, err := s.Q.InsertDeviceSession(ctx, models.InsertDeviceSessionParams{DeviceID: tg.row.ID, Day: s.today()})
			if err != nil || n == 0 {
				continue // started on this phone today already
			}
			_ = s.sendActivity(ctx, tg, *tg.row.ActivityStartToken, apns.Notification{
				Title: "Session", Body: s.sessionStartBody(state),
				Live: &apns.Live{Event: "start", ContentState: state, AttributesType: sessionAttributesType, Attributes: s.sessionAttributes(now), Stale: now.Add(liveStaleAfter), Urgent: true},
			}, "session start")
		}
	}
	_ = s.Q.DeleteOldDeviceSessions(ctx, now.In(s.tz()).AddDate(0, 0, -3).Format("2006-01-02"))
}

func (s *Service) sessionStartBody(st *SessionState) string {
	if st == nil {
		return "The session started."
	}
	if st.OpenCount > 0 {
		return fmt.Sprintf("%d open · %s realised", st.OpenCount, money(st.Realized))
	}
	return fmt.Sprintf("%d closed · %s", st.Trades, money(st.Realized))
}

func money(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-%.2f", -v)
	}
	return fmt.Sprintf("+%.2f", v)
}

// tradeOnActivity carries a closed trade as the phone's running Session
// activity's alert (one thing on the Lock Screen, not two) plus a
// badge-only push that keeps the badge; false when the phone has no
// running activity, or asked for notifications.
func (s *Service) tradeOnActivity(ctx context.Context, tg target, t closedTrade, badge int) bool {
	if !s.Enabled() || s.State == nil || (tg.prefs != nil && (tg.prefs.NoActivity || tg.prefs.FinishedAsNotification)) {
		return false
	}
	acts, err := s.Q.ListDeviceActivities(ctx, tg.row.ID)
	if err != nil {
		return false
	}
	var running []models.DeviceActivity
	for _, a := range acts {
		if a.Kind == sessionKind {
			running = append(running, a)
		}
	}
	if len(running) == 0 {
		return false
	}
	state, err := s.State(ctx, tg.row.UserID)
	if err != nil || state == nil {
		return false
	}
	now := s.now()
	state.trim(now)
	state.Alert = fmt.Sprintf("%s %s %s", t.SymbolRoot, t.Direction, money(t.Net))
	sent := false
	for _, a := range running {
		if err := s.sendActivity(ctx, tg, a.Token, apns.Notification{
			Title: t.SymbolRoot + " " + t.Direction, Body: money(t.Net) + " · " + t.Account, Sound: tg.prefs.Sound(KindTradeClosed),
			Live: &apns.Live{Event: "update", ContentState: state, Stale: now.Add(liveStaleAfter), Urgent: true},
		}, "trade on activity"); err == nil {
			sent = true
		}
	}
	if !sent {
		return false
	}
	_ = s.send(ctx, tg, apns.Notification{Badge: &badge}, "badge")
	return true
}
