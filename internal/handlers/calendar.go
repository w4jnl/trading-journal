package handlers

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// dayCell is what the template renders for each day in the grid.
type dayCell struct {
	Date       time.Time
	InMonth    bool // false for spillover from prev/next month
	InFuture   bool // true when the date is past today — journal entry disabled
	HasData    bool
	Trades     int64
	Wins       int64
	Losses     int64
	NetPnl     float64
	GrossPnl   float64
	Commission float64
	HasJournal bool   // day-level journal entry exists
	JournalTag string // good|bad|neutral|""
	// SpillPnl is true for an out-of-month day whose week IS part of the month
	// view - the template renders its P&L muted so the week total is explained.
	SpillPnl bool
}

// weekTotal aggregates a single row of the calendar grid. Counts every day in
// the row including spillover from neighbouring months — this matches the
// reference design where week summaries don't try to clip at month boundaries.
type weekTotal struct {
	ISOWeek    int    // ISO 8601 week of year (1..53)
	WeekID     string // "2026-W19" — the URL slug for /journal/week/{week}
	MondayStr  string // "2026-05-04" — used as ?from on the /trades link
	SundayStr  string // "2026-05-10" — used as ?to (Sunday = Monday + 6)
	HasData    bool
	Trades     int64
	NetPnl     float64
	HasJournal bool   // a week-level journal entry exists for this ISO week
	JournalTag string // good|bad|neutral|""
	InFuture   bool   // Monday is after today — no journal/link affordance
	// InMonth is true when at least one of the week's days falls in the viewed
	// month. A trailing all-spillover week (InMonth=false) shows no summary.
	InMonth bool
}

// CalendarPage renders /calendar — a monthly grid colored by net P&L.
// Query params: month=YYYY-MM (default current), account_id (default all).
func (h *Handlers) CalendarPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isFragment := liveFragment(r)
	h.TopstepxSync.MaybeEnqueueUser(userID(ctx)) // throttled background API sync; nil-safe
	h.IbkrSync.MaybeEnqueueUser(userID(ctx))     // throttled IBKR gateway poll; nil-safe
	tz := h.localTZ()

	monthStart, err := parseMonth(r.URL.Query().Get("month"), tz)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	monthEnd := monthStart.AddDate(0, 1, 0)

	// The grid is a Sunday-start 6-week block that always covers the month plus
	// spillover days from the neighbouring months. Daily stats are fetched for
	// the WHOLE grid (not just the month) so a week-summary total includes the
	// spillover days that share its ISO week with in-month days (e.g. Jun 29-30
	// count toward the week that also holds Jul 1-3). The spillover day CELLS
	// stay greyed - only the week totals use their data.
	gridStart := monthStart
	for gridStart.Weekday() != time.Sunday {
		gridStart = gridStart.AddDate(0, 0, -1)
	}
	gridEnd := gridStart.AddDate(0, 0, 42) // 6 weeks, exclusive upper bound

	// Bridge legacy ?account_id=N → ?filter=acc:N so older calendar bookmarks
	// keep working through the multi-select transition.
	if legacy := strings.TrimSpace(r.URL.Query().Get("account_id")); legacy != "" {
		q := r.URL.Query()
		q.Add("filter", "acc:"+legacy)
		newR := *r
		newR.URL = &url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
		r = &newR
	}
	accountFilt, err := h.resolveAccountFilter(r)
	if err != nil {
		h.serverError(w, fmt.Errorf("resolve filter: %w", err))
		return
	}
	accountIDSet := map[int64]bool{}
	for _, id := range accountFilt.AccountIDs {
		accountIDSet[id] = true
	}
	retired, err := h.retiredAccountSet(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("retired set: %w", err))
		return
	}
	// Stats-excluded accounts drop out of the DEFAULT view; an explicit account
	// filter overrides (the user asked for those specific accounts).
	excluded, err := h.statsExcludedAccountSet(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("excluded set: %w", err))
		return
	}

	rows, err := h.rq().ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{
		UserID:     userID(ctx),
		StatDate:   gridStart,
		StatDate_2: gridEnd,
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("list daily_stats: %w", err))
		return
	}

	// Day-level journal entries for the visible range so we can flag cells.
	journals, err := h.rq().ListDayJournalsInRange(ctx, models.ListDayJournalsInRangeParams{
		UserID:      userIDPtr(ctx),
		EntryDate:   monthStart,
		EntryDate_2: monthEnd,
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("list day journals: %w", err))
		return
	}
	journalsByDate := map[string]string{} // key: "YYYY-MM-DD" → tag (may be "")
	for _, j := range journals {
		y, m, d := j.EntryDate.In(tz).Date()
		key := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
		tag := ""
		if j.Tag != nil {
			tag = *j.Tag
		}
		journalsByDate[key] = tag
	}

	// Week-level journal entries are keyed by Monday's date. Pull a window
	// that comfortably covers the visible grid (a Sunday-start month always
	// has Mondays in [monthStart-6 .. monthEnd+6]).
	weekJournalStart := monthStart.AddDate(0, 0, -7)
	weekJournalEnd := monthEnd.AddDate(0, 0, 7)
	weekJournals, err := h.rq().ListWeekJournalsInRange(ctx, models.ListWeekJournalsInRangeParams{
		UserID:      userIDPtr(ctx),
		EntryDate:   weekJournalStart,
		EntryDate_2: weekJournalEnd,
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("list week journals: %w", err))
		return
	}
	weekJournalsByMonday := map[string]string{} // key: Monday "YYYY-MM-DD" → tag
	for _, j := range weekJournals {
		y, m, d := j.EntryDate.In(tz).Date()
		key := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
		tag := ""
		if j.Tag != nil {
			tag = *j.Tag
		}
		weekJournalsByMonday[key] = tag
	}

	// Aggregate per local-day key.
	byDate := map[string]*dayCell{}
	for _, row := range rows {
		if retired[row.AccountID] {
			continue
		}
		if accountFilt.Active {
			if !accountIDSet[row.AccountID] {
				continue
			}
		} else if excluded[row.AccountID] {
			continue
		}
		y, m, d := row.StatDate.In(tz).Date()
		key := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
		c, ok := byDate[key]
		if !ok {
			c = &dayCell{Date: time.Date(y, m, d, 0, 0, 0, 0, tz)}
			byDate[key] = c
		}
		c.HasData = true
		c.Trades += row.TradesCount
		c.Wins += row.Wins
		c.Losses += row.Losses
		c.NetPnl += row.NetPnl
		c.GrossPnl += row.GrossPnl
		c.Commission += row.Commission
	}

	// gridStart / gridEnd were computed above (they also bound the stats fetch).
	now := time.Now().In(tz)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	weeks := make([][]dayCell, 6)
	for wk := 0; wk < 6; wk++ {
		row := make([]dayCell, 7)
		for d := 0; d < 7; d++ {
			day := gridStart.AddDate(0, 0, wk*7+d)
			key := day.Format("2006-01-02")
			cell := dayCell{
				Date:     day,
				InMonth:  day.Month() == monthStart.Month(),
				InFuture: day.After(today),
			}
			if c, ok := byDate[key]; ok {
				cell.HasData = c.HasData
				cell.Trades = c.Trades
				cell.Wins = c.Wins
				cell.Losses = c.Losses
				cell.NetPnl = c.NetPnl
				cell.GrossPnl = c.GrossPnl
				cell.Commission = c.Commission
			}
			if tag, ok := journalsByDate[key]; ok {
				cell.HasJournal = true
				cell.JournalTag = tag
			}
			row[d] = cell
		}

		// The 7th column (index 6, Saturday) is repurposed as the week-summary
		// slot, so a Saturday's own P&L would be hidden yet still counted in the
		// week total - making the total not add up to the visible Sun-Fri cells.
		// The market is closed Saturday; a Saturday-dated trade is a Friday
		// US-session trade whose exit crossed local midnight (common in +UTC
		// zones like Europe/Amsterdam). Fold it into Friday so it's shown and
		// the week total reconciles. Journal flags stay on Saturday's own date.
		if sat := &row[6]; sat.HasData {
			fri := &row[5]
			fri.HasData = true
			fri.Trades += sat.Trades
			fri.Wins += sat.Wins
			fri.Losses += sat.Losses
			fri.NetPnl += sat.NetPnl
			fri.GrossPnl += sat.GrossPnl
			fri.Commission += sat.Commission
			sat.HasData = false
			sat.Trades, sat.Wins, sat.Losses = 0, 0, 0
			sat.NetPnl, sat.GrossPnl, sat.Commission = 0, 0, 0
		}
		weeks[wk] = row
	}

	// Month totals.
	var monthNet, monthGross, monthComm float64
	var monthTrades, winDays, lossDays int64
	weeklyTotals := make([]weekTotal, len(weeks))
	for wi := range weeks {
		week := weeks[wi]
		// ISO week is anchored to Monday — week[1] is Monday of this row, the
		// stable representative for the ISO week regardless of which Sunday
		// or Saturday the grid happens to start on.
		monday := week[1].Date
		isoYear, isoWeek := monday.ISOWeek()
		wt := weekTotal{
			ISOWeek:   isoWeek,
			WeekID:    fmt.Sprintf("%d-W%02d", isoYear, isoWeek),
			MondayStr: monday.Format("2006-01-02"),
			SundayStr: monday.AddDate(0, 0, 6).Format("2006-01-02"),
			InFuture:  monday.After(today),
		}
		// A week is part of THIS month's view iff at least one of its days is
		// in-month. The trailing all-spillover row (a fixed 6-week grid always
		// renders 6 rows) has none - it shows no summary.
		for _, c := range week {
			if c.InMonth {
				wt.InMonth = true
				break
			}
		}
		if tag, ok := weekJournalsByMonday[monday.Format("2006-01-02")]; ok {
			wt.HasJournal = true
			wt.JournalTag = tag
		}
		for di := range week {
			c := &weeks[wi][di]
			// Week aggregates: every day in the row, including spillover.
			if c.HasData {
				wt.HasData = true
				wt.Trades += c.Trades
				wt.NetPnl += c.NetPnl
			}
			// A spillover day that belongs to an in-month week (e.g. Jul 1-3 in
			// the week that also holds Jun 28-30) shows its P&L muted, so the
			// user can see what feeds the week total. Spillover days of an
			// all-spillover week stay blank.
			if !c.InMonth && wt.InMonth && c.HasData {
				c.SpillPnl = true
			}
			// Month aggregates: only in-month days.
			if !c.InMonth || !c.HasData {
				continue
			}
			monthNet += c.NetPnl
			monthGross += c.GrossPnl
			monthComm += c.Commission
			monthTrades += c.Trades
			switch {
			case c.NetPnl > 0:
				winDays++
			case c.NetPnl < 0:
				lossDays++
			}
		}
		weeklyTotals[wi] = wt
	}

	accounts, err := h.rq().ListAccounts(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}

	// CalendarBack is the relative URL ("/calendar?month=…[&filter=…]")
	// that cross-page deep links from cells embed as `?back=…`. html/template
	// URL-escapes the value when interpolated into an href, so we pass it raw.
	calBackParams := url.Values{}
	calBackParams.Set("month", monthStart.Format("2006-01"))
	for _, v := range accountFilt.RawValues {
		calBackParams.Add("filter", v)
	}
	calendarBack := "/calendar?" + calBackParams.Encode()

	// FilterQS is the already-encoded filter-only query string that nav links
	// concatenate onto their own URLs to preserve the active selection across
	// month navigation. template.URL marks it pre-escaped so html/template
	// won't double-encode.
	filterOnly := url.Values{}
	for _, v := range accountFilt.RawValues {
		filterOnly.Add("filter", v)
	}
	filterQS := template.URL(filterOnly.Encode())

	data := map[string]any{
		"NavActive":        "calendar",
		"BackHref":         validBackHref(r),
		"MonthLabel":       monthStart.Format("January 2006"),
		"MonthQS":          monthStart.Format("2006-01"),
		"PrevMonth":        monthStart.AddDate(0, -1, 0).Format("2006-01"),
		"NextMonth":        monthStart.AddDate(0, 1, 0).Format("2006-01"),
		"Weeks":            weeks,
		"WeeklyTotals":     weeklyTotals,
		"MonthNet":         monthNet,
		"MonthGross":       monthGross,
		"MonthComm":        monthComm,
		"Trades":           monthTrades,
		"WinDays":          winDays,
		"LossDays":         lossDays,
		"Accounts":         accountsForFilter(accounts, false),
		"FilterSelections": accountFilt.Selected,
		"FilterCount":      accountFilt.SelectedCount,
		"FilterQS":         filterQS,
		"CalendarBack":     calendarBack,
	}
	if isFragment {
		h.renderPageFragment(w, r, "calendar.html", "cal_fragment", data)
		return
	}
	h.renderPage(w, r, "calendar.html", data)
}

func parseMonth(s string, tz *time.Location) (time.Time, error) {
	if s == "" {
		now := time.Now().In(tz)
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, tz), nil
	}
	t, err := time.ParseInLocation("2006-01", s, tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("month must be YYYY-MM")
	}
	return t, nil
}
