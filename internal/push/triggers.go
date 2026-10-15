package push

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/guardrail"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/markethours"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/push/apns"
)

// ---- What gets pushed, and when ---------------------------------------------
//
// Three triggers. A nudge the guardrail records goes out at once as a
// time-sensitive alert (NudgeCreated, hooked on guardrail.Service.OnNudges).
// A change of the user's data (the live-data version moved: a fill, a
// plan, a note) is looked at on the next tick: a trade that closed in the
// last minutes becomes a quiet alert, and every change becomes one silent
// sync push carrying the badge and the unseen nudge ids, so the app's
// delivered notifications and badge stay right. Once a day, before the
// open, a phone that opted in hears that no plan is written. The Session
// activity (live.go) rides the same tick.

const (
	// DefaultEvery is how often users with phones are looked at.
	DefaultEvery = 5 * time.Second
	// tradeFresh bounds the closed-trade alert: an import of a finished
	// session must not announce every trade in it.
	tradeFresh = 20 * time.Minute
	// badgeWindow bounds the badge to nudges of the last two hours.
	badgeWindow = 2 * time.Hour
	// planMissingAt is the minute of the Chicago day the missing-plan
	// reminder goes out (08:15, fifteen minutes before the open) and
	// planMissingUntil the last minute it still makes sense.
	planMissingAt    = 8*60 + 15
	planMissingUntil = 8*60 + 30
)

// Categories the app registers; one per shape of answer.
const (
	CategoryNudge    = "TJ_NUDGE"
	CategoryTrade    = "TJ_TRADE"
	CategoryPosition = "TJ_POSITION"
)

// tickState is the per-user memory of the triggers.
type tickState struct {
	version     uint64
	pushedTrade map[string]time.Time // closed trades announced, by their identity
	planMissing string               // the day the reminder went out
}

// Run ticks until ctx ends; nothing happens without an APNs client.
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	every := s.Every
	if every <= 0 {
		every = DefaultEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick looks at every user with a phone once.
func (s *Service) Tick(ctx context.Context) {
	if s == nil || s.Live == nil {
		return
	}
	users, err := s.Q.ListUsersWithDevices(ctx)
	if err != nil {
		s.log().Warn("push: users", "err", err)
		return
	}
	for _, uid := range users {
		st := s.state(uid)
		v := s.Live.Version(uid)
		s.mu.Lock()
		first := st.pushedTrade == nil
		changed := !first && st.version != v
		st.version = v
		s.mu.Unlock()
		if first {
			s.snapshot(ctx, uid, st)
		} else if changed {
			s.DataChanged(ctx, uid)
		}
		s.planMissing(ctx, uid, st)
		s.liveTick(ctx, uid, changed)
	}
}

func (s *Service) state(uid int64) *tickState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[int64]*tickState{}
	}
	st := s.states[uid]
	if st == nil {
		st = &tickState{}
		s.states[uid] = st
	}
	return st
}

// closedTrade is a trade that closed, as the alert says it.
type closedTrade struct {
	ID         int64
	SymbolRoot string
	Direction  string
	Qty        float64
	Net        float64
	ExitTime   time.Time
	Account    string
	AccountID  int64
	Symbol     string
}

func (t closedTrade) key() string {
	return fmt.Sprintf("%d|%s|%s|%g|%d", t.AccountID, t.Symbol, t.Direction, t.Qty, t.ExitTime.Unix())
}

// recentClosed are the user's trades closed in the last tradeFresh.
func (s *Service) recentClosed(ctx context.Context, uid int64) ([]closedTrade, error) {
	since := s.now().Add(-tradeFresh).UTC()
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.id, t.symbol_root, t.symbol, t.direction, t.qty, COALESCE(t.net_pnl, 0), t.exit_time, a.id, a.name
		FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ? AND t.status = 'closed' AND t.exit_time >= ?
		ORDER BY t.exit_time, t.id`, uid, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []closedTrade
	for rows.Next() {
		var t closedTrade
		if err := rows.Scan(&t.ID, &t.SymbolRoot, &t.Symbol, &t.Direction, &t.Qty, &t.Net, &t.ExitTime, &t.AccountID, &t.Account); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// snapshot remembers the recent trades as already known, so a server
// start announces nothing.
func (s *Service) snapshot(ctx context.Context, uid int64, st *tickState) {
	recent, err := s.recentClosed(ctx, uid)
	if err != nil {
		s.log().Warn("push: snapshot", "user", uid, "err", err)
		recent = nil
	}
	s.mu.Lock()
	st.pushedTrade = map[string]time.Time{}
	for _, t := range recent {
		st.pushedTrade[t.key()] = t.ExitTime
	}
	s.mu.Unlock()
}

// DataChanged announces the trades that closed since the last look and
// sends every phone a silent sync.
func (s *Service) DataChanged(ctx context.Context, uid int64) {
	if !s.Enabled() {
		return
	}
	st := s.state(uid)
	targets, err := s.targets(ctx, uid)
	if err != nil || len(targets) == 0 {
		return
	}
	recent, err := s.recentClosed(ctx, uid)
	if err != nil {
		s.log().Warn("push: closed trades", "user", uid, "err", err)
	}
	now := s.now()
	var fresh []closedTrade
	s.mu.Lock()
	if st.pushedTrade == nil {
		st.pushedTrade = map[string]time.Time{}
	}
	for k, at := range st.pushedTrade {
		if now.Sub(at) > 2*tradeFresh {
			delete(st.pushedTrade, k)
		}
	}
	for _, t := range recent {
		if _, done := st.pushedTrade[t.key()]; !done {
			st.pushedTrade[t.key()] = t.ExitTime
			fresh = append(fresh, t)
		}
	}
	s.mu.Unlock()
	if len(fresh) > 3 {
		fresh = fresh[len(fresh)-3:]
	}
	badge := s.badge(ctx, uid)
	for _, t := range fresh {
		for _, tg := range targets {
			if why := tg.prefs.Filtered(KindTradeClosed); why != "" {
				s.log().Debug("push skipped", "what", "trade", "device", tg.row.Name, "why", why)
				continue
			}
			if tg.prefs != nil && tg.prefs.QuietOutsideRth && !markethours.InRTHWindow(now, s.chicago(), 8*60+30, 15*60) {
				s.log().Debug("push skipped", "what", "trade", "device", tg.row.Name, "why", "outside RTH")
				continue
			}
			if s.tradeOnActivity(ctx, tg, t, badge) {
				continue
			}
			_ = s.send(ctx, tg, s.tradeNotification(tg, t, badge), "trade")
		}
	}
	s.sync(ctx, uid, targets, badge)
}

// tradeNotification is the closed-trade alert.
func (s *Service) tradeNotification(tg target, t closedTrade, badge int) apns.Notification {
	sign := "+"
	if t.Net < 0 {
		sign = "-"
	}
	body := fmt.Sprintf("%s%.2f · %s", sign, abs(t.Net), t.ExitTime.In(s.tz()).Format("15:04"))
	return apns.Notification{
		Title: t.SymbolRoot + " " + t.Direction, Subtitle: t.Account, Body: body,
		Category: CategoryTrade, ThreadID: "trades", CollapseID: "trade:" + t.key(),
		Badge: &badge, Sound: tg.prefs.Sound(KindTradeClosed), TimeSensitive: tg.prefs.TimeSensitive(KindTradeClosed), Mutable: true,
		Data: map[string]any{"kind": "trade", "tradeId": t.ID, "route": "trade/" + fmt.Sprint(t.ID), "symbolRoot": t.SymbolRoot, "direction": t.Direction, "netPnl": t.Net, "accountId": t.AccountID},
	}
}

// NudgeCreated pushes the guardrail's new nudges to every phone that wants
// them, at once. Wired on guardrail.Service.OnNudges.
func (s *Service) NudgeCreated(ctx context.Context, uid int64, nudges []models.Nudge) {
	if !s.Enabled() || len(nudges) == 0 {
		return
	}
	targets, err := s.targets(ctx, uid)
	if err != nil || len(targets) == 0 {
		return
	}
	badge := s.badge(ctx, uid)
	for _, n := range nudges {
		for _, tg := range targets {
			if why := tg.prefs.Filtered(KindNudge); why != "" {
				s.log().Debug("push skipped", "what", "nudge", "device", tg.row.Name, "why", why)
				continue
			}
			_ = s.send(ctx, tg, s.nudgeNotification(tg, n, badge), "nudge")
		}
	}
	// The guardrail bumped the version for its own rows; the tick will
	// not see new trades in it, but the sync keeps the badge right.
	s.sync(ctx, uid, targets, badge)
}

// nudgeNotification is the alert for one nudge: the title is the thing,
// the subtitle where it is, the body what it wants (with the lesson).
func (s *Service) nudgeNotification(tg target, n models.Nudge, badge int) apns.Notification {
	route := guardrail.RouteFor(n.Href)
	subtitle := s.dayLabel(n.Day)
	category := CategoryNudge
	if strings.HasPrefix(route, "position/") {
		if parts := strings.SplitN(route, "/", 3); len(parts) == 3 {
			subtitle = parts[2] + " · open"
		}
		category = CategoryPosition
	}
	body := n.Detail
	if n.Lesson != nil && *n.Lesson != "" {
		body += " — Your lesson: " + *n.Lesson
	}
	data := map[string]any{"kind": "nudge", "id": n.ID, "key": n.Key, "route": route, "day": n.Day}
	if n.Lesson != nil {
		data["lesson"] = *n.Lesson
	}
	return apns.Notification{
		Title: n.Title, Subtitle: subtitle, Body: body,
		Category: category, ThreadID: "nudges", CollapseID: fmt.Sprintf("nudge:%d", n.ID),
		Badge: &badge, Sound: tg.prefs.Sound(KindNudge), TimeSensitive: tg.prefs.TimeSensitive(KindNudge), Mutable: true,
		Data: data,
	}
}

// sync sends every phone the silent push that carries the whole waiting
// state: the badge and the unseen nudge ids, plus the data version.
func (s *Service) sync(ctx context.Context, uid int64, targets []target, badge int) {
	ids := s.unseenIDs(ctx, uid)
	if ids == nil {
		ids = []int64{}
	}
	data := map[string]any{"kind": "sync", "v": s.Live.Version(uid), "badge": badge, "unseen": ids}
	for _, tg := range targets {
		_ = s.send(ctx, tg, apns.Notification{Background: true, Data: data}, "sync")
	}
}

// badge is today's unseen nudges from the last two hours.
func (s *Service) badge(ctx context.Context, uid int64) int {
	n, err := s.Q.CountUnseenNudges(ctx, models.CountUnseenNudgesParams{UserID: uid, Day: s.today(), CreatedAt: s.now().UTC().Add(-badgeWindow)})
	if err != nil {
		return 0
	}
	return int(n)
}

// unseenIDs are the nudges the badge counts.
func (s *Service) unseenIDs(ctx context.Context, uid int64) []int64 {
	rows, err := s.Q.ListNudgesForDayDesc(ctx, models.ListNudgesForDayDescParams{UserID: uid, Day: s.today()})
	if err != nil {
		return nil
	}
	since := s.now().UTC().Add(-badgeWindow)
	var ids []int64
	for _, n := range rows {
		if n.SeenAt == nil && !n.CreatedAt.Before(since) {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// planMissing reminds a phone that opted in, once per trading day
// between 08:15 and 08:30 Chicago, when the guardrail is on and no plan is
// written for today.
func (s *Service) planMissing(ctx context.Context, uid int64, st *tickState) {
	now := s.now().In(s.chicago())
	mins := now.Hour()*60 + now.Minute()
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday || mins < planMissingAt || mins >= planMissingUntil {
		return
	}
	day := s.today()
	s.mu.Lock()
	done := st.planMissing == day
	s.mu.Unlock()
	if done {
		return
	}
	targets, err := s.targets(ctx, uid)
	if err != nil {
		return
	}
	var wanting []target
	for _, tg := range targets {
		if tg.prefs.Allows(KindPlanMissing) {
			wanting = append(wanting, tg)
		}
	}
	s.mu.Lock()
	st.planMissing = day
	s.mu.Unlock()
	if len(wanting) == 0 {
		return
	}
	u, err := s.Q.GetUserByID(ctx, uid)
	if err != nil || u.Guardrail == 0 {
		return
	}
	if p, err := dayplan.Get(ctx, s.Q, uid, day); err != nil || p != nil {
		return
	}
	badge := s.badge(ctx, uid)
	for _, tg := range wanting {
		_ = s.send(ctx, tg, apns.Notification{
			Title: "No plan for today", Subtitle: s.dayLabel(day), Body: "The open is in fifteen minutes and no plan is written: bias, levels, max trades, max loss.",
			Category: CategoryNudge, ThreadID: "nudges", CollapseID: "plan-missing:" + day,
			Badge: &badge, Sound: tg.prefs.Sound(KindPlanMissing), TimeSensitive: tg.prefs.TimeSensitive(KindPlanMissing),
			Data: map[string]any{"kind": "nudge", "key": "plan-missing", "route": "plan", "day": day},
		}, "plan-missing")
	}
}

func (s *Service) tz() *time.Location {
	if s != nil && s.TZ != nil {
		return s.TZ
	}
	return time.UTC
}

func (s *Service) chicago() *time.Location {
	if s != nil && s.Chicago != nil {
		return s.Chicago
	}
	if loc, err := time.LoadLocation("America/Chicago"); err == nil {
		return loc
	}
	return s.tz()
}

// today is the trading day's key.
func (s *Service) today() string { return s.now().In(s.tz()).Format("2006-01-02") }

// dayLabel is a day as the subtitle says it: "Wed 15 Oct".
func (s *Service) dayLabel(day string) string {
	if t, err := time.ParseInLocation("2006-01-02", day, s.tz()); err == nil {
		return t.Format("Mon 2 Jan")
	}
	return day
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

var _ = sync.Mutex{}
var _ *livedata.Notifier
