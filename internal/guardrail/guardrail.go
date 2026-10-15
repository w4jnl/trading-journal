// Package guardrail watches the trading day while it runs and nudges the
// trader when it starts going the way their own reviews warned about. As
// fills arrive (the user's live-data version moves) it re-reads the day:
// the coach's warnings on the trades that just closed, and the plan's
// rules - the decision cap reached, an account near or at the max loss -
// and, on the clock, the plan's stop time. Each nudge is recorded once
// (nudges table) and quotes the trader's own open lesson about that
// mistake; pages pick it up and show it. Deterministic: no model in the
// loop, so a nudge lands seconds after the fill that caused it.
package guardrail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/coach"
	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
)

const (
	// DefaultEvery is how often the watched users are looked at.
	DefaultEvery = 20 * time.Second
	// DefaultFresh is how recent a trade (or the stop time) must be for a
	// nudge: an import of a finished session is not a moment to nudge.
	DefaultFresh = 20 * time.Minute
	// nearLoss is the share of the max loss that earns a warning first.
	nearLoss = 0.8
	// lessonDays is how far back an open lesson is quoted (as the ledger).
	lessonDays = 30
)

// Service is the guardrail worker.
type Service struct {
	DB    *sql.DB // writes the nudges
	RDB   *sql.DB // reads the day; DB when nil
	Live  *livedata.Notifier
	TZ    *time.Location
	Every time.Duration
	Fresh time.Duration
	Now   func() time.Time
	Log   *slog.Logger
	// LastClose is the newest cached price of a root (never a fetch), for
	// the "price through your stop" rule; nil leaves that rule off.
	LastClose func(ctx context.Context, root string) (price float64, at time.Time, ok bool)
	// OnNudges hears every nudge the moment it is recorded (the push to
	// the phone); nil when nothing listens.
	OnNudges func(ctx context.Context, uid int64, nudges []models.Nudge)

	mu   sync.Mutex
	seen map[int64]uint64 // the data version last read per user
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) fresh() time.Duration {
	if s.Fresh > 0 {
		return s.Fresh
	}
	return DefaultFresh
}

func (s *Service) rdb() *sql.DB {
	if s.RDB != nil {
		return s.RDB
	}
	return s.DB
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Run ticks until ctx ends.
func (s *Service) Run(ctx context.Context) {
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

// Tick looks at every watched user once: the day again when their data
// changed since the last look, the clock always.
func (s *Service) Tick(ctx context.Context) {
	ids, err := models.New(s.rdb()).ListGuardrailUserIDs(ctx)
	if err != nil {
		s.log().Warn("guardrail users", "err", err)
		return
	}
	for _, uid := range ids {
		v := s.Live.Version(uid)
		s.mu.Lock()
		if s.seen == nil {
			s.seen = map[int64]uint64{}
		}
		last, known := s.seen[uid]
		s.seen[uid] = v
		s.mu.Unlock()
		if _, err := s.Check(ctx, uid, !known || last != v); err != nil {
			s.log().Warn("guardrail check", "user", uid, "err", err)
		}
	}
}

// Nudge is one thing to say.
type Nudge struct {
	Key      string
	Ref      string
	Severity string
	Title    string
	Detail   string
	Mistake  string // the mistake label it is about, for the lesson
	Plan     bool   // a plan rule: quotes the top lesson when none is about it
	Href     string // where the nudge leads; the day page when empty
}

// dayTrade is a closed trade of the day.
type dayTrade struct {
	ID        int64
	AccountID int64
	Account   string
	Entry     time.Time
	Exit      time.Time
	Net       float64
}

// Check reads the user's day and records the nudges that are new; it
// returns them. dataChanged=false skips the trades and reads the clock only.
func (s *Service) Check(ctx context.Context, uid int64, dataChanged bool) ([]models.Nudge, error) {
	now := s.now()
	local := now.In(s.TZ)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.TZ)
	key := day.Format("2006-01-02")
	q := models.New(s.DB)
	plan, err := dayplan.Get(ctx, models.New(s.rdb()), uid, key)
	if err != nil {
		return nil, err
	}
	var cands []Nudge
	var trades []dayTrade
	if dataChanged {
		trades, err = s.dayTrades(ctx, uid, day)
		if err != nil {
			return nil, err
		}
		if len(trades) > 0 {
			rep, err := coach.Run(ctx, s.rdb(), uid, day, s.TZ)
			if err != nil {
				return nil, err
			}
			cands = append(cands, fromFindings(rep, trades, now, s.fresh())...)
			cands = append(cands, fromPlan(plan, rep.Decisions, trades, now, s.fresh())...)
		}
	}
	cands = append(cands, stopTime(plan, day, now, s.fresh())...)
	// What is open right now, on the clock: a stop still missing minutes
	// after the fill, open risk past what the plan leaves.
	legs, names, err := s.openLegs(ctx, uid)
	if err != nil {
		return nil, err
	}
	if len(legs) > 0 {
		if trades == nil {
			if trades, err = s.dayTrades(ctx, uid, day); err != nil {
				return nil, err
			}
		}
		cands = append(cands, fromOpenPositions(legs, names, plan, trades, now, s.fresh())...)
		cands = append(cands, s.throughStop(ctx, legs, now)...)
	}
	if len(cands) == 0 {
		return nil, nil
	}

	lessons := s.openLessons(ctx, uid, day)
	var out []models.Nudge
	for _, c := range cands {
		var lesson *string
		if l, ok := lessons.byKey[c.Mistake]; ok && c.Mistake != "" {
			lesson = &l
		} else if c.Plan && lessons.top != "" {
			lesson = &lessons.top
		}
		href := c.Href
		if href == "" {
			href = "/journal/" + key
		}
		n := models.Nudge{UserID: uid, Day: key, Key: c.Key, Ref: c.Ref, Severity: c.Severity, Title: c.Title, Detail: c.Detail,
			Lesson: lesson, Href: href}
		row, err := q.InsertNudge(ctx, models.InsertNudgeParams{UserID: n.UserID, Day: n.Day, Key: n.Key, Ref: n.Ref,
			Severity: n.Severity, Title: n.Title, Detail: n.Detail, Lesson: n.Lesson, Href: n.Href})
		if errors.Is(err, sql.ErrNoRows) {
			continue // said before
		}
		if err != nil {
			return out, fmt.Errorf("record nudge: %w", err)
		}
		out = append(out, row)
	}
	if len(out) > 0 {
		s.log().Debug("guardrail nudges", "user", uid, "n", len(out))
		s.Live.Bump(uid)
		s.mu.Lock() // our own bump is not new trading data
		if s.seen != nil {
			s.seen[uid] = s.Live.Version(uid)
		}
		s.mu.Unlock()
		if s.OnNudges != nil {
			s.OnNudges(ctx, uid, out)
		}
	}
	return out, nil
}

// dayTrades are the day's closed trades on the accounts the coach reads.
func (s *Service) dayTrades(ctx context.Context, uid int64, day time.Time) ([]dayTrade, error) {
	rows, err := s.rdb().QueryContext(ctx, `
		SELECT t.id, t.account_id, a.name, t.entry_time, t.exit_time, COALESCE(t.net_pnl, 0)
		FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ? AND a.active = 1 AND a.excluded_from_stats = 0
		  AND t.status = 'closed' AND t.exit_time >= ? AND t.exit_time < ?
		ORDER BY t.exit_time, t.id`, uid, day.UTC(), day.AddDate(0, 0, 1).UTC())
	if err != nil {
		return nil, fmt.Errorf("guardrail: day trades: %w", err)
	}
	defer rows.Close()
	var out []dayTrade
	for rows.Next() {
		var t dayTrade
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Account, &t.Entry, &t.Exit, &t.Net); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// isFresh: the moment lies within fresh before now (a minute of clock skew
// ahead allowed; anything later is not a live fill).
func isFresh(now, t time.Time, fresh time.Duration) bool {
	age := now.Sub(t)
	return age >= -time.Minute && age <= fresh
}

// fromFindings turns the coach's warnings into nudges, for the ones whose
// newest trade closed within fresh. The ref is that trade's entry time
// (trade ids change when a contract is rebuilt; entry times do not), so a
// warning that repeats on a new decision is said again.
func fromFindings(rep *coach.Report, trades []dayTrade, now time.Time, fresh time.Duration) []Nudge {
	byID := make(map[int64]dayTrade, len(trades))
	for _, t := range trades {
		byID[t.ID] = t
	}
	var out []Nudge
	for _, f := range rep.Findings {
		if f.Severity != "warn" {
			continue
		}
		var newest *dayTrade
		for _, id := range f.TradeIDs {
			if t, ok := byID[id]; ok && (newest == nil || t.Exit.After(newest.Exit)) {
				t := t
				newest = &t
			}
		}
		if newest == nil || !isFresh(now, newest.Exit, fresh) {
			continue
		}
		n := Nudge{Key: f.Key, Ref: newest.Entry.UTC().Format(time.RFC3339Nano), Severity: "warn", Title: f.Title, Detail: f.Detail}
		if f.Label != nil {
			n.Mistake = f.Label.Key
		}
		out = append(out, n)
	}
	return out
}

// fromPlan says when the decision cap is reached (not yet past it: the
// coach says that) and when an account nears or reaches the max loss.
func fromPlan(plan *dayplan.Plan, decisions int, trades []dayTrade, now time.Time, fresh time.Duration) []Nudge {
	if plan == nil || len(trades) == 0 {
		return nil
	}
	var out []Nudge
	last := trades[0].Exit
	for _, t := range trades {
		if t.Exit.After(last) {
			last = t.Exit
		}
	}
	if plan.MaxTrades != nil && int64(decisions) == *plan.MaxTrades && isFresh(now, last, fresh) {
		out = append(out, Nudge{Key: "cap-reached", Ref: strconv.Itoa(decisions), Severity: "warn", Plan: true, Mistake: "overtraded",
			Title:  "Decision cap reached",
			Detail: fmt.Sprintf("That was decision %d of %d: the plan says no new entries today.", decisions, decisions)})
	}
	if plan.MaxLoss != nil && *plan.MaxLoss > 0 {
		type acct struct {
			name string
			net  float64
			last time.Time
		}
		byAcct := map[int64]*acct{}
		var order []int64
		for _, t := range trades {
			a := byAcct[t.AccountID]
			if a == nil {
				a = &acct{name: t.Account}
				byAcct[t.AccountID] = a
				order = append(order, t.AccountID)
			}
			a.net += t.Net
			if t.Exit.After(a.last) {
				a.last = t.Exit
			}
		}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		limit := *plan.MaxLoss
		for _, id := range order {
			a := byAcct[id]
			if !isFresh(now, a.last, fresh) {
				continue
			}
			ref := strconv.FormatInt(id, 10)
			switch {
			case a.net <= -limit:
				out = append(out, Nudge{Key: "max-loss-hit", Ref: ref, Severity: "warn", Plan: true, Mistake: "against-plan",
					Title:  "Max loss reached on " + a.name,
					Detail: fmt.Sprintf("Down %.2f today against the plan's %.2f: the plan says stop for the day.", -a.net, limit)})
			case a.net <= -nearLoss*limit:
				out = append(out, Nudge{Key: "max-loss-near", Ref: ref, Severity: "warn", Plan: true, Mistake: "against-plan",
					Title:  "Near the max loss on " + a.name,
					Detail: fmt.Sprintf("Down %.2f of the %.2f the plan allows today.", -a.net, limit)})
			}
		}
	}
	return out
}

// stopTime says the plan's stop time has come, within fresh of it.
func stopTime(plan *dayplan.Plan, day, now time.Time, fresh time.Duration) []Nudge {
	if plan == nil {
		return nil
	}
	mins, ok := plan.StopMinutes()
	if !ok {
		return nil
	}
	at := day.Add(time.Duration(mins) * time.Minute)
	if now.Before(at) || now.Sub(at) > fresh {
		return nil
	}
	return []Nudge{{Key: "stop-time", Ref: plan.StopTime, Severity: "warn", Plan: true, Mistake: "against-plan",
		Title:  "Stop time",
		Detail: fmt.Sprintf("It is %s, the plan's stop time: no new entries today.", plan.StopTime)}}
}

// lessonIndex is the trader's open lessons: the newest per mistake and the
// most repeated one.
type lessonIndex struct {
	byKey map[string]string
	top   string
}

func (s *Service) openLessons(ctx context.Context, uid int64, day time.Time) lessonIndex {
	idx := lessonIndex{byKey: map[string]string{}}
	rows, err := models.New(s.rdb()).ListOpenLessons(ctx, models.ListOpenLessonsParams{UserID: uid, Day: day.AddDate(0, 0, -lessonDays).Format("2006-01-02")})
	if err != nil {
		s.log().Warn("guardrail lessons", "err", err)
		return idx
	}
	count := map[string]int{}
	best := 0
	for _, l := range rows { // newest first
		k := ""
		if l.MistakeKey != nil && journal.ValidLabel(journal.KindMistake, *l.MistakeKey) {
			k = *l.MistakeKey
			if _, ok := idx.byKey[k]; !ok {
				idx.byKey[k] = l.Text
			}
		}
		count[k]++
		if k != "" && count[k] > best {
			best, idx.top = count[k], idx.byKey[k]
		}
		if idx.top == "" {
			idx.top = l.Text
		}
	}
	return idx
}
