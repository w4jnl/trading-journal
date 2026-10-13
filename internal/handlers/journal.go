package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gomarkdown/markdown"
	mdHtml "github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"github.com/microcosm-cc/bluemonday"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// JournalFeed renders /journal — the chronological feed mixing day, trade,
// and week entries by date desc. Quick-add bar at the top lets the user jump
// to today / any date / any week. Filter section below it narrows the feed
// by date range, scope (day/trade/week), and tag.
//
// Clicking a feed item opens the right edit surface (modal for trade scope,
// page for day/week scopes).
func (h *Handlers) JournalFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.TopstepxSync.MaybeEnqueueUser(userID(ctx)) // throttled background API sync; nil-safe
	h.IbkrSync.MaybeEnqueueUser(userID(ctx))     // throttled IBKR gateway poll; nil-safe
	tz := h.localTZ()

	filters := parseJournalFilters(r, tz)

	rows, err := listJournalFeedFiltered(ctx, h.rdb(), filters, 100)
	if err != nil {
		h.serverError(w, fmt.Errorf("list feed: %w", err))
		return
	}

	// Collect the group-entry IDs for a single batch summary query — keeps
	// the feed render O(1 SQL trip) regardless of how many group entries
	// exist on the page.
	var groupIDs []int64
	for _, r := range rows {
		if r.Scope == "group" {
			groupIDs = append(groupIDs, r.ID)
		}
	}
	groupSummaries, err := loadGroupSummaries(ctx, h.rdb(), groupIDs)
	if err != nil {
		h.serverError(w, fmt.Errorf("load group summaries: %w", err))
		return
	}
	entryIDs := make([]int64, 0, len(rows))
	for _, r := range rows {
		entryIDs = append(entryIDs, r.ID)
	}
	entryLabels, err := loadEntryLabels(ctx, h.rdb(), entryIDs)
	if err != nil {
		h.serverError(w, fmt.Errorf("load entry labels: %w", err))
		return
	}
	suggestions, err := loadOpenSuggestions(ctx, h.rdb(), entryIDs)
	if err != nil {
		h.serverError(w, fmt.Errorf("load suggestions: %w", err))
		return
	}

	// The user's strategies: the filter options, the active-chip labels and
	// the names on suggested-strategy pills.
	strategies, err := h.Queries.ListStrategies(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list strategies: %w", err))
		return
	}
	stratName := map[int64]string{}
	stratOpts := make([]journalStrategyOption, 0, len(strategies))
	for _, s := range strategies {
		stratName[s.ID] = s.Name
		stratOpts = append(stratOpts, journalStrategyOption{ID: s.ID, Name: s.Name, Checked: filters.strategySet[s.ID]})
	}
	strategyName := func(id int64) (string, bool) {
		name, ok := stratName[id]
		return name, ok
	}

	type feedItem struct {
		ID        int64
		Scope     string // day | trade | week | group
		Date      time.Time
		DateLabel string
		MetaLabel string // "APEX-1 / MESM6 long · $42.50" for trade, "May 4–10" for week
		Tag       string
		Strategy  string // assigned strategy name, "" when none
		Body      string
		Mistakes  []string // review label keys (internal/journal vocabulary)
		Emotions  []string
		// The AI review's open proposals that still apply, shown dashed;
		// they are accepted or dismissed on the note itself.
		Suggestions []suggestionView
		EditURL     string // where to navigate when the row is clicked
		// Group-only — populated when Scope == "group", zero otherwise.
		GroupN      int64
		GroupNet    float64
		GroupWins   int64
		GroupLosses int64
		GroupHasPnl bool // true when at least one linked trade has net_pnl set
	}
	// Entries opened from the feed come back to this exact feed: the
	// filters and the view it was in.
	feedBack := "?back=" + url.QueryEscape(pageURL(r))
	items := make([]feedItem, 0, len(rows))
	for _, row := range rows {
		fi := feedItem{
			ID:       row.ID,
			Scope:    row.Scope,
			Date:     row.EntryDate.In(tz),
			Body:     row.Body,
			Mistakes: entryLabels[row.ID][journal.KindMistake],
			Emotions: entryLabels[row.ID][journal.KindEmotion],
		}
		if row.Tag != nil {
			fi.Tag = *row.Tag
		}
		if row.StrategyName != nil {
			fi.Strategy = *row.StrategyName
		}
		if open := suggestions[row.ID]; len(open) > 0 {
			have := map[string]bool{}
			for kind, keys := range entryLabels[row.ID] {
				for _, k := range keys {
					have[kind+":"+k] = true
				}
			}
			var still []journal.Suggestion
			for _, sg := range open {
				if sg.Applies(row.Tag != nil, row.StrategyName != nil, have) {
					still = append(still, sg)
				}
			}
			fi.Suggestions = suggestionViews(still, strategyName)
		}
		switch row.Scope {
		case "day":
			fi.DateLabel = fi.Date.Format("Mon, 2 Jan 2006")
			fi.EditURL = "/journal/" + fi.Date.Format("2006-01-02") + feedBack
		case "trade":
			fi.DateLabel = fi.Date.Format("Mon, 2 Jan 2006")
			meta := ""
			if row.AccountName != nil {
				meta = *row.AccountName
			}
			if row.TradeSymbol != nil {
				if meta != "" {
					meta += " · "
				}
				meta += *row.TradeSymbol
				if row.TradeDirection != nil {
					meta += " " + *row.TradeDirection
				}
			}
			if row.TradeNetPnl != nil {
				if meta != "" {
					meta += " · "
				}
				meta += fmt.Sprintf("%+.2f", *row.TradeNetPnl) // signed, no currency glyph - nothing else in the app shows one
			}
			fi.MetaLabel = meta
			if row.TradeID != nil {
				// Open the standalone trade-journal page so the feed → entry
				// experience is identical to day/week/group entries. The
				// centered modal on /trades is still available when the user
				// arrives at the trade list directly.
				fi.EditURL = fmt.Sprintf("/journal/trade/%d", *row.TradeID) + feedBack
			}
		case "week":
			y, w := fi.Date.ISOWeek()
			sunday := fi.Date.AddDate(0, 0, 6)
			fi.DateLabel = fmt.Sprintf("Week %d", w)
			fi.MetaLabel = fmt.Sprintf("%s – %s", fi.Date.Format("Jan 2"), sunday.Format("Jan 2"))
			fi.EditURL = fmt.Sprintf("/journal/week/%d-W%02d", y, w) + feedBack
		case "group":
			fi.DateLabel = fi.Date.Format("Mon, 2 Jan 2006")
			fi.EditURL = fmt.Sprintf("/journal/group/%d", row.ID) + feedBack
			if s, ok := groupSummaries[row.ID]; ok {
				fi.GroupN = s.N
				fi.GroupNet = s.Net
				fi.GroupWins = s.Wins
				fi.GroupLosses = s.Losses
				fi.GroupHasPnl = s.Wins+s.Losses > 0
				if s.LastExit.After(fi.Date) {
					// Prefer the latest exit's date as the displayed label
					// (entry_date may have been overridden by the user).
					fi.DateLabel = s.LastExit.In(tz).Format("Mon, 2 Jan 2006")
				}
			}
		}
		items = append(items, fi)
	}

	now := time.Now().In(tz)
	todayStr := now.Format("2006-01-02")
	y, wk := now.ISOWeek()
	thisWeekStr := fmt.Sprintf("%d-W%02d", y, wk)

	filterCount := len(filters.scopes) + len(filters.tags) + len(filters.strategies) + len(filters.mistakes) + len(filters.emotions)
	if filters.strategyNone {
		filterCount++
	}
	if filters.mistakeNone {
		filterCount++
	}
	if filters.fromStr != "" {
		filterCount++
	}
	if filters.toStr != "" {
		filterCount++
	}

	activeChips := buildJournalActiveChips(filters, stratName)
	patterns := journalLabelPatterns(filters, entryLabels)

	h.renderPage(w, r, "journal_feed.html", map[string]any{
		"NavActive":           "journal",
		"BackHref":            validBackHref(r),
		"Items":               items,
		"Today":               todayStr,
		"ThisWeek":            thisWeekStr,
		"FilterFrom":          filters.fromStr,
		"FilterTo":            filters.toStr,
		"ScopeSelected":       filters.scopeSet, // map[string]bool — used by checkboxes
		"TagSelected":         filters.tagSet,
		"StrategyOptions":     stratOpts,
		"StrategyNoneChecked": filters.strategyNone,
		"MistakeSelected":     filters.mistakeSet,
		"MistakeNoneChecked":  filters.mistakeNone,
		"EmotionSelected":     filters.emotionSet,
		"Patterns":            patterns,
		"FilterActive":        filters.active(),
		"FilterCount":         filterCount,
		"ActiveFilters":       activeChips,
		"ExportQS":            template.URL(exportQueryString(filters)), // ready-to-append (without leading '?')
		"FeedBack":            pageURL(r),
	})
}

// journalStrategyOption is one checkbox in the feed's Strategy filter row.
type journalStrategyOption struct {
	ID      int64
	Name    string
	Checked bool
}

// journalActiveChip is one removable chip on the collapsed filter bar. RemoveURL
// is the current feed URL with just this one filter value dropped.
type journalActiveChip struct {
	Label     string
	RemoveURL string
}

// buildJournalActiveChips renders one chip per active filter value, each with a
// RemoveURL that drops only that value (other active filters persist).
func buildJournalActiveChips(f journalFilters, stratName map[int64]string) []journalActiveChip {
	// Materialize the current params, then a helper clones-minus-one.
	base := url.Values{}
	if f.fromStr != "" {
		base.Set("from", f.fromStr)
	}
	if f.toStr != "" {
		base.Set("to", f.toStr)
	}
	for _, s := range f.scopes {
		base.Add("scope", s)
	}
	for _, t := range f.tags {
		base.Add("tag", t)
	}
	for _, id := range f.strategies {
		base.Add("strategy", strconv.FormatInt(id, 10))
	}
	if f.strategyNone {
		base.Add("strategy", "none")
	}
	f.labelValues(base)
	removeURL := func(key, val string) string {
		v := url.Values{}
		for k, vals := range base {
			for _, x := range vals {
				if k == key && x == val {
					continue
				}
				v.Add(k, x)
			}
		}
		if enc := v.Encode(); enc != "" {
			return "/journal?" + enc
		}
		return "/journal"
	}

	scopeLabels := map[string]string{"day": "Day", "trade": "Trade", "week": "Week", "group": "Group"}
	tagLabels := map[string]string{"good": "Good", "neutral": "Neutral", "bad": "Bad", "untagged": "Untagged"}

	var chips []journalActiveChip
	if f.fromStr != "" {
		chips = append(chips, journalActiveChip{"From " + f.fromStr, removeURL("from", f.fromStr)})
	}
	if f.toStr != "" {
		chips = append(chips, journalActiveChip{"To " + f.toStr, removeURL("to", f.toStr)})
	}
	for _, s := range f.scopes {
		chips = append(chips, journalActiveChip{scopeLabels[s], removeURL("scope", s)})
	}
	for _, t := range f.tags {
		chips = append(chips, journalActiveChip{tagLabels[t], removeURL("tag", t)})
	}
	for _, id := range f.strategies {
		name := stratName[id]
		if name == "" {
			continue // unknown / not the user's — don't surface a chip we can't label
		}
		chips = append(chips, journalActiveChip{name, removeURL("strategy", strconv.FormatInt(id, 10))})
	}
	if f.strategyNone {
		chips = append(chips, journalActiveChip{"Unassigned", removeURL("strategy", "none")})
	}
	for _, k := range f.mistakes {
		chips = append(chips, journalActiveChip{journal.LabelName(journal.KindMistake, k), removeURL("mistake", k)})
	}
	if f.mistakeNone {
		chips = append(chips, journalActiveChip{"No mistake logged", removeURL("mistake", "none")})
	}
	for _, k := range f.emotions {
		chips = append(chips, journalActiveChip{journal.LabelName(journal.KindEmotion, k), removeURL("emotion", k)})
	}
	return chips
}

// exportQueryString re-emits the active filters as a URL query string so the
// "Download .md" / "Print" buttons preserve the user's filter state. Empty
// when nothing is active — the buttons then export everything.
func exportQueryString(f journalFilters) string {
	v := url.Values{}
	if f.fromStr != "" {
		v.Set("from", f.fromStr)
	}
	if f.toStr != "" {
		v.Set("to", f.toStr)
	}
	for _, s := range f.scopes {
		v.Add("scope", s)
	}
	for _, t := range f.tags {
		v.Add("tag", t)
	}
	for _, id := range f.strategies {
		v.Add("strategy", strconv.FormatInt(id, 10))
	}
	if f.strategyNone {
		v.Add("strategy", "none")
	}
	f.labelValues(v)
	return v.Encode()
}

// journalFilters carries the parsed feed filters: date range + scope + tag +
// strategy.
type journalFilters struct {
	from, to       *time.Time
	fromStr, toStr string
	scopes         []string // "day" | "trade" | "week"
	scopeSet       map[string]bool
	// tags is the set the user picked. Special value "" stands for "untagged"
	// (tag column IS NULL). The DB tag values are good/neutral/bad.
	tags   []string
	tagSet map[string]bool
	// strategies are the selected strategy ids; strategyNone selects entries
	// with no strategy attached (strategy_id IS NULL).
	strategies   []int64
	strategyNone bool
	strategySet  map[int64]bool
	// Review labels (internal/journal vocabulary). Within a kind the picks
	// are OR-ed, across kinds AND-ed, like tag and strategy. mistakeNone
	// selects entries with no mistake logged at all.
	mistakes    []string
	emotions    []string
	mistakeSet  map[string]bool
	emotionSet  map[string]bool
	mistakeNone bool
}

func (f journalFilters) active() bool {
	return f.from != nil || f.to != nil || len(f.scopes) > 0 || len(f.tags) > 0 ||
		len(f.strategies) > 0 || f.strategyNone || len(f.mistakes) > 0 || len(f.emotions) > 0 || f.mistakeNone
}

// newJournalFilters is the empty filter set with its lookup maps ready.
func newJournalFilters() journalFilters {
	return journalFilters{
		scopeSet:    map[string]bool{},
		tagSet:      map[string]bool{},
		strategySet: map[int64]bool{},
		mistakeSet:  map[string]bool{},
		emotionSet:  map[string]bool{},
	}
}

// addLabelFilter records one label pick when it is in the vocabulary;
// "none" on mistakes means "nothing logged". Unknown keys are ignored.
func (f *journalFilters) addLabelFilter(kind, key string) {
	key = strings.ToLower(strings.TrimSpace(key))
	if kind == journal.KindMistake && key == "none" {
		f.mistakeNone = true
		return
	}
	if !journal.ValidLabel(kind, key) {
		return
	}
	if kind == journal.KindMistake && !f.mistakeSet[key] {
		f.mistakes = append(f.mistakes, key)
		f.mistakeSet[key] = true
	}
	if kind == journal.KindEmotion && !f.emotionSet[key] {
		f.emotions = append(f.emotions, key)
		f.emotionSet[key] = true
	}
}

// parseJournalFilters reads ?from=&to=&scope=&tag=&strategy=&mistake=&emotion=
// from the request.
func parseJournalFilters(r *http.Request, tz *time.Location) journalFilters {
	f := newJournalFilters()
	if v := strings.TrimSpace(r.URL.Query().Get("from")); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, tz); err == nil {
			f.from = &t
			f.fromStr = v
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("to")); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, tz); err == nil {
			t2 := t.AddDate(0, 0, 1) // inclusive upper bound
			f.to = &t2
			f.toStr = v
		}
	}
	for _, v := range r.URL.Query()["scope"] {
		switch v {
		case "day", "trade", "week", "group":
			if !f.scopeSet[v] {
				f.scopes = append(f.scopes, v)
				f.scopeSet[v] = true
			}
		}
	}
	for _, v := range r.URL.Query()["tag"] {
		switch v {
		case "good", "neutral", "bad", "untagged":
			if !f.tagSet[v] {
				f.tags = append(f.tags, v)
				f.tagSet[v] = true
			}
		}
	}
	for _, v := range r.URL.Query()["strategy"] {
		if v == "none" {
			f.strategyNone = true
			continue
		}
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 && !f.strategySet[id] {
			f.strategies = append(f.strategies, id)
			f.strategySet[id] = true
		}
	}
	for _, v := range r.URL.Query()["mistake"] {
		f.addLabelFilter(journal.KindMistake, v)
	}
	for _, v := range r.URL.Query()["emotion"] {
		f.addLabelFilter(journal.KindEmotion, v)
	}
	return f
}

// labelValues re-emits the label filters as query values (shared by the
// export query string and the active chips).
func (f journalFilters) labelValues(v url.Values) {
	for _, k := range f.mistakes {
		v.Add("mistake", k)
	}
	if f.mistakeNone {
		v.Add("mistake", "none")
	}
	for _, k := range f.emotions {
		v.Add("emotion", k)
	}
}

// feedRow is the projection ListJournalFeed used to return — we keep the
// same shape so the feedItem construction loop didn't have to change.
type feedRow struct {
	ID             int64
	TradeID        *int64
	AccountID      *int64
	EntryDate      time.Time
	Body           string
	Mood           *int64
	Tag            *string
	Scope          string
	TradeSymbol    *string
	TradeDirection *string
	TradeNetPnl    *float64
	AccountName    *string
	StrategyName   *string
}

// listJournalFeedFiltered runs a raw SQL query so the dynamic WHERE clause
// can include scope/tag IN (...) and the date range. Same join shape as the
// sqlc ListJournalFeed query, minus a couple of columns the feed doesn't need.
func listJournalFeedFiltered(ctx context.Context, db *sql.DB, f journalFilters, limit int) ([]feedRow, error) {
	w := whereUser(ownsJournalSQL, userID(ctx))
	if f.from != nil {
		w.and("je.entry_date >= ?", *f.from)
	}
	if f.to != nil {
		w.and("je.entry_date < ?", *f.to)
	}
	if len(f.scopes) > 0 {
		ph := strings.Repeat("?,", len(f.scopes))
		ph = ph[:len(ph)-1]
		args := make([]any, 0, len(f.scopes))
		for _, s := range f.scopes {
			args = append(args, s)
		}
		w.and("je.scope IN ("+ph+")", args...)
	}
	if len(f.tags) > 0 {
		// Build (tag IN (...) OR tag IS NULL) when "untagged" is one of the picks.
		var real []string
		untagged := false
		for _, t := range f.tags {
			if t == "untagged" {
				untagged = true
			} else {
				real = append(real, t)
			}
		}
		parts := []string{}
		var args []any
		if len(real) > 0 {
			ph := strings.Repeat("?,", len(real))
			ph = ph[:len(ph)-1]
			parts = append(parts, "je.tag IN ("+ph+")")
			for _, t := range real {
				args = append(args, t)
			}
		}
		if untagged {
			parts = append(parts, "je.tag IS NULL")
		}
		if len(parts) > 0 {
			w.and("("+strings.Join(parts, " OR ")+")", args...)
		}
	}
	if len(f.strategies) > 0 || f.strategyNone {
		var parts []string
		var args []any
		if len(f.strategies) > 0 {
			ph := strings.Repeat("?,", len(f.strategies))
			ph = ph[:len(ph)-1]
			parts = append(parts, "je.strategy_id IN ("+ph+")")
			for _, id := range f.strategies {
				args = append(args, id)
			}
		}
		if f.strategyNone {
			parts = append(parts, "je.strategy_id IS NULL")
		}
		w.and("("+strings.Join(parts, " OR ")+")", args...)
	}
	// Labels: an entry matches a kind when it carries any of the picked
	// keys (or, for "none", carries no mistake at all).
	labelClause := func(kind string, keys []string, none bool) {
		if len(keys) == 0 && !none {
			return
		}
		var parts []string
		var args []any
		if len(keys) > 0 {
			ph := strings.Repeat("?,", len(keys))
			ph = ph[:len(ph)-1]
			parts = append(parts, "EXISTS (SELECT 1 FROM journal_entry_labels l WHERE l.journal_entry_id = je.id AND l.kind = ? AND l.label IN ("+ph+"))")
			args = append(args, kind)
			for _, k := range keys {
				args = append(args, k)
			}
		}
		if none {
			parts = append(parts, "NOT EXISTS (SELECT 1 FROM journal_entry_labels l WHERE l.journal_entry_id = je.id AND l.kind = ?)")
			args = append(args, kind)
		}
		w.and("("+strings.Join(parts, " OR ")+")", args...)
	}
	labelClause(journal.KindMistake, f.mistakes, f.mistakeNone)
	labelClause(journal.KindEmotion, f.emotions, false)

	q := `SELECT je.id, je.trade_id, je.account_id, je.entry_date, je.body,
	             je.mood, je.tag, je.scope,
	             t.symbol, t.direction, t.net_pnl,
	             a.name, s.name
	      FROM journal_entries je
	      LEFT JOIN trades t      ON t.id = je.trade_id
	      LEFT JOIN accounts a    ON a.id = COALESCE(je.account_id, t.account_id)
	      LEFT JOIN strategies s  ON s.id = je.strategy_id
	      WHERE ` + w.sql() + `
	      ORDER BY je.entry_date DESC, je.id DESC
	      LIMIT ?`
	args := append(w.args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []feedRow
	for rows.Next() {
		var r feedRow
		if err := rows.Scan(
			&r.ID, &r.TradeID, &r.AccountID, &r.EntryDate, &r.Body,
			&r.Mood, &r.Tag, &r.Scope,
			&r.TradeSymbol, &r.TradeDirection, &r.TradeNetPnl,
			&r.AccountName, &r.StrategyName,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// JournalPage renders /journal/{date} — the day-level entry editor for a
// specific YYYY-MM-DD.
func (h *Handlers) JournalPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isFragment := liveFragment(r)

	dateStr := r.PathValue("date")
	if dateStr == "" {
		dateStr = time.Now().In(h.localTZ()).Format("2006-01-02")
	}
	date, err := time.ParseInLocation("2006-01-02", dateStr, h.localTZ())
	if err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}

	entry, err := h.Queries.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: userIDPtr(ctx), EntryDate: date})
	hasEntry := true
	if errors.Is(err, sql.ErrNoRows) {
		hasEntry = false
		entry = models.JournalEntry{}
	} else if err != nil {
		h.serverError(w, fmt.Errorf("get day entry: %w", err))
		return
	}

	recent, err := h.Queries.ListRecentJournalEntries(ctx, models.ListRecentJournalEntriesParams{UserID: userIDPtr(ctx), Limit: 30})
	if err != nil {
		h.serverError(w, fmt.Errorf("list recent: %w", err))
		return
	}

	prevDay := date.AddDate(0, 0, -1).Format("2006-01-02")
	nextDay := date.AddDate(0, 0, 1).Format("2006-01-02")

	var rendered template.HTML
	if hasEntry && entry.Body != "" {
		rendered = template.HTML(renderMarkdown(entry.Body))
	}

	mood := int64(0)
	if entry.Mood != nil {
		mood = *entry.Mood
	}
	tag := ""
	if entry.Tag != nil {
		tag = *entry.Tag
	}

	flash := r.URL.Query().Get("flash")

	// Cross-page links into this view set ?back=<relative-url>. Falls back to
	// the legacy ?from=calendar form for any in-flight tabs from the older
	// nav scheme.
	backHref := validBackHref(r)
	if backHref == "" && r.URL.Query().Get("from") == "calendar" {
		q := url.Values{}
		if v := r.URL.Query().Get("month"); v != "" {
			q.Set("month", v)
		}
		if v := r.URL.Query().Get("account_id"); v != "" {
			q.Set("account_id", v)
		}
		backHref = "/calendar"
		if encoded := q.Encode(); encoded != "" {
			backHref += "?" + encoded
		}
	}

	// Trades that closed on this day — surfaced in a "Trades · summary" block
	// so the day view matches the unified content shape of week/group entries.
	dayTrades, err := loadDayTrades(ctx, h.rdb(), date)
	if err != nil {
		h.serverError(w, fmt.Errorf("load day trades: %w", err))
		return
	}
	daySummary := summarizeTradeRows(dayTrades)

	strategies, err := h.Queries.ListStrategies(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list strategies: %w", err))
		return
	}
	mistakes, emotions := h.entryLabels(ctx, entry.ID)
	// A coach finding's "add label" link opens the editor with the chip
	// pre-checked (?edit=1&mistake=revenge); nothing is saved until Save.
	q := r.URL.Query()
	journalOpen := q.Get("edit") == "1"
	for _, k := range q["mistake"] {
		if journal.ValidLabel(journal.KindMistake, k) && !hasString(mistakes, k) {
			mistakes = append(mistakes, k)
			journalOpen = true
		}
	}
	for _, k := range q["emotion"] {
		if journal.ValidLabel(journal.KindEmotion, k) && !hasString(emotions, k) {
			emotions = append(emotions, k)
			journalOpen = true
		}
	}
	coachRep := h.coachReport(ctx, date)
	var planOutcome any
	if coachRep != nil && coachRep.Plan != nil {
		planOutcome = coachRep.Plan
	}

	data := map[string]any{
		"NavActive":        "journal",
		"BackHref":         backHref,
		"JournalOpen":      journalOpen,
		"Coach":            coachRep,
		"Date":             dateStr,
		"ChartEnabled":     h.chartShow(dayTrades),
		"ChartEndpoint":    "/charts/day/" + dateStr,
		"ChartCached":      h.chartCached(ctx, dayTrades),
		"DateNice":         date.Format("Mon, 2 Jan 2006"),
		"PrevDay":          prevDay,
		"NextDay":          nextDay,
		"Today":            time.Now().In(h.localTZ()).Format("2006-01-02"),
		"HasEntry":         hasEntry,
		"Entry":            entry,
		"AIPending":        h.AIReviews.EntryPending(entry.ID),
		"AIError":          h.aiReviewError(r, entry.ID),
		"Suggestions":      h.entrySuggestions(ctx, entry.ID),
		"LabelSrc":         h.labelSources(ctx, entry.ID),
		"TagSrc":           sourceOf(entry.TagSource),
		"Mood":             mood,
		"Tag":              tag,
		"Mistakes":         mistakes,
		"Emotions":         emotions,
		"Rendered":         rendered,
		"Recent":           recent,
		"Flash":            flash,
		"Trades":           dayTrades,
		"Summary":          daySummary,
		"Strategies":       strategies,
		"StrategyID":       strategyIDValue(entry.StrategyID),
		"AssignedStrategy": h.loadAssignedStrategy(r, entry.StrategyID),
	}
	// The plan card: the plan, its outcome, the open lessons and the draft.
	for k, v := range h.planCardData(ctx, dateStr, backHref, planOutcome) {
		data[k] = v
	}
	data["Nudges"] = h.dayNudges(ctx, dateStr)
	if msg := q.Get("plan_err"); msg != "" {
		data["PlanDraftError"] = msg
	}
	if isFragment {
		h.renderPageFragment(w, r, "journal.html", "journal_fragment", data)
		return
	}
	h.renderPage(w, r, "journal.html", data)
}

// parseMood reads the form value 1-5 for mood, returning nil for invalid/empty.
func parseMood(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m, err := strconv.ParseInt(s, 10, 64)
	if err != nil || m < 1 || m > 5 {
		return nil
	}
	return &m
}

// parseTag validates the quick-classify tag — good/bad/neutral or nil.
func parseTag(s string) *string {
	s = strings.TrimSpace(s)
	switch s {
	case "good", "bad", "neutral":
		return &s
	}
	return nil
}

func hasString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// JournalSave handles POST /journal/{date}: upserts a day-level entry for
// the date, then redirects back to GET /journal/{date} so a refresh doesn't
// resubmit the form. Carries through the `back` field if set, so the
// "Back to calendar" round-trip survives the save.
func (h *Handlers) JournalSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dateStr := r.PathValue("date")
	date, err := time.ParseInLocation("2006-01-02", dateStr, h.localTZ())
	if err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	// Reject future-dated journal entries — UI hides the link too, but defend
	// the API path against direct POSTs.
	now := time.Now().In(h.localTZ())
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, h.localTZ())
	if date.After(today) {
		http.Error(w, "journal entries are not allowed on future dates", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	redirectQuery := func(flash string) string {
		q := url.Values{}
		q.Set("flash", flash)
		// Round-trip the back URL if the form carried it. Also accepts legacy
		// from/month/account_id triple for in-flight tabs.
		if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
			q.Set("back", v)
		}
		for _, key := range []string{"from", "month", "account_id"} {
			if v := r.FormValue(key); v != "" {
				q.Set(key, v)
			}
		}
		return "/journal/" + dateStr + "?" + q.Encode()
	}

	body := strings.TrimSpace(r.FormValue("body"))
	mood := parseMood(r.FormValue("mood"))
	tag := parseTag(r.FormValue("tag"))
	strategyID := h.parseStrategyID(r)

	// Explicit delete signal from the editor's Delete button. We piggyback on
	// the save endpoint instead of a /journal/{date}/delete route because that
	// 3-segment path conflicts with /journal/week/{week} under Go 1.22 mux
	// pattern matching (neither pattern is uniformly more specific).
	if r.FormValue("_action") == "delete" {
		if _, err := h.journalSvc().DeleteDay(ctx, userID(ctx), date); err != nil {
			h.saveFailed(w, "delete day journal", err)
			return
		}
		http.Redirect(w, r, redirectQuery("deleted"), http.StatusSeeOther)
		return
	}
	mistakes, emotions := labelFields(r)
	out, err := h.journalSvc().SaveDay(ctx, userID(ctx), date, journal.Fields{Body: body, Mood: mood, Tag: tag, StrategyID: strategyID, Mistakes: mistakes, Emotions: emotions})
	if err != nil {
		h.saveFailed(w, "save day journal", err)
		return
	}
	http.Redirect(w, r, redirectQuery(string(out)), http.StatusSeeOther)
}

// JournalTradePage renders /journal/trade/{id} — the standalone trade-level
// entry editor. Mirrors the day/week/group view shape: header, tag picker,
// editor, then a Summary + Trades block (a single linked trade in this
// case). The form posts to the existing /trades/{id}/journal endpoint;
// when that endpoint sees no HX-Request header it redirects back here.
func (h *Handlers) JournalTradePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	trade, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var journalBody, journalTag, journalTagSrc string
	var hasJournal bool
	var journalEntryID int64
	var journalStrategyID *int64
	if entry, err := h.Queries.GetJournalEntryByTradeID(ctx, &id); err == nil {
		hasJournal = true
		journalBody = entry.Body
		journalEntryID = entry.ID
		journalStrategyID = entry.StrategyID
		if entry.Tag != nil {
			journalTag = *entry.Tag
		}
		journalTagSrc = sourceOf(entry.TagSource)
	} else if !errors.Is(err, sql.ErrNoRows) {
		h.serverError(w, fmt.Errorf("get trade journal: %w", err))
		return
	}

	trades, err := loadOneTrade(ctx, h.rdb(), id)
	if err != nil {
		h.serverError(w, fmt.Errorf("load trade for journal: %w", err))
		return
	}
	summary := summarizeTradeRows(trades)

	strategies, err := h.Queries.ListStrategies(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list strategies: %w", err))
		return
	}
	mistakes, emotions := h.entryLabels(ctx, journalEntryID)
	checklists, checked := h.checklistData(ctx, journalEntryID)

	h.renderPage(w, r, "journal_trade.html", map[string]any{
		"Checklists":       checklists,
		"Checked":          checked,
		"Checklist":        h.entryChecklist(ctx, journalEntryID),
		"NavActive":        "journal",
		"BackHref":         validBackHref(r),
		"Trade":            trade,
		"TradeID":          id,
		"ChartEnabled":     h.chartShow(trades),
		"ChartEndpoint":    fmt.Sprintf("/charts/trade/%d", id),
		"ChartCached":      h.chartCached(ctx, trades),
		"HasJournal":       hasJournal,
		"JournalBody":      journalBody,
		"JournalTag":       journalTag,
		"Mistakes":         mistakes,
		"Emotions":         emotions,
		"JournalEntryID":   journalEntryID,
		"AIPending":        h.AIReviews.EntryPending(journalEntryID),
		"AIError":          h.aiReviewError(r, journalEntryID),
		"Suggestions":      h.entrySuggestions(ctx, journalEntryID),
		"LabelSrc":         h.labelSources(ctx, journalEntryID),
		"TagSrc":           journalTagSrc,
		"GexRefresh":       h.GEX != nil && gexRefreshable(journalBody),
		"Summary":          summary,
		"Trades":           trades,
		"Flash":            r.URL.Query().Get("flash"),
		"Strategies":       strategies,
		"StrategyID":       strategyIDValue(journalStrategyID),
		"AssignedStrategy": h.loadAssignedStrategy(r, journalStrategyID),
	})
}

// WeekJournalPage renders /journal/week/{2026-W19} — the week-level entry
// editor. entry_date for a week-scoped row is the Monday of that ISO week.
func (h *Handlers) WeekJournalPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	weekID := r.PathValue("week")
	monday, isoYear, isoWeek, err := parseISOWeek(weekID, h.localTZ())
	if err != nil {
		http.Error(w, "bad week id: expected YYYY-Www (e.g. 2026-W19)", http.StatusBadRequest)
		return
	}

	entry, err := h.Queries.GetWeekJournalEntry(ctx, models.GetWeekJournalEntryParams{UserID: userIDPtr(ctx), EntryDate: monday})
	hasEntry := true
	if errors.Is(err, sql.ErrNoRows) {
		hasEntry = false
		entry = models.JournalEntry{}
	} else if err != nil {
		h.serverError(w, fmt.Errorf("get week entry: %w", err))
		return
	}

	var rendered template.HTML
	if hasEntry && entry.Body != "" {
		rendered = template.HTML(renderMarkdown(entry.Body))
	}
	tag := ""
	if entry.Tag != nil {
		tag = *entry.Tag
	}

	prev := monday.AddDate(0, 0, -7)
	next := monday.AddDate(0, 0, 7)
	py, pw := prev.ISOWeek()
	ny, nw := next.ISOWeek()
	sunday := monday.AddDate(0, 0, 6)

	weekTrades, err := loadWeekTrades(ctx, h.rdb(), monday)
	if err != nil {
		h.serverError(w, fmt.Errorf("load week trades: %w", err))
		return
	}
	weekSummary := summarizeTradeRows(weekTrades)

	strategies, err := h.Queries.ListStrategies(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list strategies: %w", err))
		return
	}
	mistakes, emotions := h.entryLabels(ctx, entry.ID)

	h.renderPage(w, r, "journal_week.html", map[string]any{
		"NavActive":        "journal",
		"BackHref":         validBackHref(r),
		"WeekID":           fmt.Sprintf("%d-W%02d", isoYear, isoWeek),
		"ChartEnabled":     h.chartShow(weekTrades),
		"ChartEndpoint":    fmt.Sprintf("/charts/week/%d-W%02d", isoYear, isoWeek),
		"ChartCached":      h.chartCached(ctx, weekTrades),
		"WeekLabel":        fmt.Sprintf("Week %d, %d", isoWeek, isoYear),
		"RangeLabel":       fmt.Sprintf("%s – %s", monday.Format("Mon, 2 Jan"), sunday.Format("Mon, 2 Jan 2006")),
		"PrevWeek":         fmt.Sprintf("%d-W%02d", py, pw),
		"NextWeek":         fmt.Sprintf("%d-W%02d", ny, nw),
		"HasEntry":         hasEntry,
		"Entry":            entry,
		"AIPending":        h.AIReviews.EntryPending(entry.ID),
		"AIError":          h.aiReviewError(r, entry.ID),
		"Suggestions":      h.entrySuggestions(ctx, entry.ID),
		"LabelSrc":         h.labelSources(ctx, entry.ID),
		"TagSrc":           sourceOf(entry.TagSource),
		"Tag":              tag,
		"Mistakes":         mistakes,
		"Emotions":         emotions,
		"Rendered":         rendered,
		"Flash":            r.URL.Query().Get("flash"),
		"Trades":           weekTrades,
		"Summary":          weekSummary,
		"Strategies":       strategies,
		"StrategyID":       strategyIDValue(entry.StrategyID),
		"AssignedStrategy": h.loadAssignedStrategy(r, entry.StrategyID),
	})
}

// WeekJournalSave handles POST /journal/week/{week_id}.
func (h *Handlers) WeekJournalSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	weekID := r.PathValue("week")
	monday, _, _, err := parseISOWeek(weekID, h.localTZ())
	if err != nil {
		http.Error(w, "bad week id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	redirectQuery := func(flash string) string {
		q := url.Values{}
		q.Set("flash", flash)
		if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
			q.Set("back", v)
		}
		return "/journal/week/" + weekID + "?" + q.Encode()
	}

	body := strings.TrimSpace(r.FormValue("body"))
	tag := parseTag(r.FormValue("tag"))
	strategyID := h.parseStrategyID(r)

	mistakes, emotions := labelFields(r)
	out, err := h.journalSvc().SaveWeek(ctx, userID(ctx), monday, journal.Fields{Body: body, Tag: tag, StrategyID: strategyID, Mistakes: mistakes, Emotions: emotions})
	if err != nil {
		h.saveFailed(w, "save week journal", err)
		return
	}
	http.Redirect(w, r, redirectQuery(string(out)), http.StatusSeeOther)
}

// WeekJournalDelete handles POST /journal/week/{week}/delete — explicit
// delete from the week-entry editor's Delete button.
func (h *Handlers) WeekJournalDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	weekID := r.PathValue("week")
	monday, _, _, err := parseISOWeek(weekID, h.localTZ())
	if err != nil {
		http.Error(w, "bad week id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if _, err := h.journalSvc().DeleteWeek(ctx, userID(ctx), monday); err != nil {
		h.saveFailed(w, "delete week journal", err)
		return
	}
	q := url.Values{}
	q.Set("flash", "deleted")
	target := "/journal"
	if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		target = v
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + q.Encode()
	} else {
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// JournalPreview handles POST /journal/preview — accepts a markdown body in
// form field `body` and returns the rendered HTML. Used by the editor's live
// preview pane (debounced) so the rendered output matches what `Save` will
// produce, byte-for-byte.
func (h *Handlers) JournalPreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := r.FormValue("body")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if strings.TrimSpace(body) == "" {
		_, _ = w.Write([]byte(`<p class="journal-editor__preview-empty">Nothing to preview yet.</p>`))
		return
	}
	_, _ = w.Write([]byte(renderMarkdown(body)))
}

// mdSanitizer strips dangerous HTML from rendered markdown. This app is
// multi-user (proxy auto-provisioning, admin act-as impersonation), so a
// journal/strategy body is untrusted input that another principal - notably an
// admin using act-as - may render in their own browser. gomarkdown passes raw
// inline HTML through and does not filter "javascript:" link URLs, and the
// result is emitted via template.HTML (bypassing html/template escaping), so
// without this pass a stored <script>/onerror=/javascript: payload would run in
// the viewer's session. UGCPolicy keeps everything a journal needs (images,
// links, tables, code, headings) while dropping scripts, event handlers, and
// unsafe URL schemes. A built Policy is immutable and safe for concurrent use.
var mdSanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// Open external links in a new tab with a safe rel; images embed uploaded
	// chart screenshots via relative /journal/attachments/ URLs (UGCPolicy
	// allows relative + http(s), but not data: URIs - an SVG/XSS vector).
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.RequireNoReferrerOnFullyQualifiedLinks(true)
	return p
}()

// renderMarkdown converts a markdown string into sanitized HTML. Uses gomarkdown
// with CommonMark extensions (tables, fenced code blocks) + AutoHeadingIDs, then
// runs the output through mdSanitizer (see its doc comment for the XSS rationale).
//
// Both the parser AND the renderer are created per call. With AutoHeadingIDs the renderer
// keeps a per-render heading-ID dedup map (EnsureUniqueHeadingID), so a shared
// *html.Renderer is NOT goroutine-safe: concurrent requests rendering markdown race on
// that map and crash the process with "fatal error: concurrent map writes".
func renderMarkdown(src string) string {
	p := parser.NewWithExtensions(parser.CommonExtensions | parser.AutoHeadingIDs)
	doc := p.Parse([]byte(src))
	// No HrefTargetBlank renderer flag: bluemonday strips the bare target
	// attribute anyway (UGCPolicy doesn't allowlist it) and then re-adds
	// target="_blank" + rel for fully-qualified links via the mdSanitizer
	// policy hooks - the flag here would just be a second, dead mechanism.
	// Net behavior: external links open in a new tab, same-origin links
	// navigate in place.
	renderer := mdHtml.NewRenderer(mdHtml.RendererOptions{
		Flags: mdHtml.CommonFlags,
	})
	unsafe := markdown.Render(doc, renderer)
	return string(mdSanitizer.SanitizeBytes(unsafe))
}

func (h *Handlers) localTZ() *time.Location {
	if h.Timezone != nil {
		return h.Timezone
	}
	return time.UTC
}

// exportTradeListCap is the maximum number of trades printed as a bullet
// list under a single journal entry in the .md and .pdf exports. Above
// this, the export shows only the summary count — keeps a 73-fill day
// from drowning the document.
const exportTradeListCap = 5

// tradesForEntry returns the trades relevant to a feed-row's entry, in the
// shared linkedTrade shape. Dispatches based on scope so every entry in the
// export can show the same "context-trades" block under its body:
//
//	day   → trades closed on that local-tz day
//	week  → trades closed within the ISO week starting at entry_date
//	trade → the single trade pointed to by trade_id
//	group → linked trades via journal_entry_trades
//
// Returns (nil, nil) when there's nothing to show — the caller suppresses
// the section entirely in that case.
func (h *Handlers) tradesForEntry(ctx context.Context, row feedRow, tz *time.Location) ([]linkedTrade, error) {
	entryDate := row.EntryDate.In(tz)
	day := time.Date(entryDate.Year(), entryDate.Month(), entryDate.Day(), 0, 0, 0, 0, tz)
	switch row.Scope {
	case "group":
		return loadLinkedTrades(ctx, h.rdb(), row.ID)
	case "day":
		return loadDayTrades(ctx, h.rdb(), day)
	case "week":
		return loadWeekTrades(ctx, h.rdb(), day)
	case "trade":
		if row.TradeID == nil {
			return nil, nil
		}
		return loadOneTrade(ctx, h.rdb(), *row.TradeID)
	}
	return nil, nil
}

// JournalExportMarkdown serves GET /journal/export.md — a text/markdown
// attachment built from the same filter parameters as the journal feed. The
// body of each entry is emitted verbatim (it's already markdown); the wrapper
// adds an H2 date heading, scope/tag metadata, and a horizontal rule between
// entries so the result reads as a single document.
func (h *Handlers) JournalExportMarkdown(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	filters := parseJournalFilters(r, tz)

	rows, err := listJournalFeedFiltered(ctx, h.rdb(), filters, 10000)
	if err != nil {
		h.serverError(w, fmt.Errorf("export feed: %w", err))
		return
	}

	var b strings.Builder
	now := time.Now().In(tz)
	b.WriteString("# Journal Export\n\n")
	b.WriteString("_Generated ")
	b.WriteString(now.Format("2006-01-02 15:04 MST"))
	if active := filterDescription(filters); active != "" {
		b.WriteString(". ")
		b.WriteString(active)
	}
	b.WriteString("._\n\n---\n\n")

	if len(rows) == 0 {
		b.WriteString("_No entries match the active filters._\n")
	}
	labels, err := loadEntryLabels(ctx, h.rdb(), feedRowIDs(rows))
	if err != nil {
		h.serverError(w, fmt.Errorf("export labels: %w", err))
		return
	}

	for _, row := range rows {
		entryDate := row.EntryDate.In(tz)
		// Load the trades that contextualize this entry — day / week /
		// trade / group all use the same shape now.
		trades, err := h.tradesForEntry(ctx, row, tz)
		if err != nil {
			h.serverError(w, fmt.Errorf("export trades for entry %d: %w", row.ID, err))
			return
		}
		summary := summarizeTradeRows(trades)

		switch row.Scope {
		case "week":
			_, wk := entryDate.ISOWeek()
			sunday := entryDate.AddDate(0, 0, 6)
			fmt.Fprintf(&b, "## Week %d · %s – %s — **week**\n\n",
				wk, entryDate.Format("Jan 2"), sunday.Format("Jan 2, 2006"))
		case "trade":
			fmt.Fprintf(&b, "## %s — **trade**\n\n", entryDate.Format("Mon, 2 Jan 2006"))
		case "group":
			fmt.Fprintf(&b, "## Group · %d trade%s · %s — **group**\n\n",
				summary.N, plural(summary.N), entryDate.Format("Mon, 2 Jan 2006"))
		default:
			fmt.Fprintf(&b, "## %s — **day**\n\n", entryDate.Format("Mon, 2 Jan 2006"))
		}

		if row.Tag != nil && *row.Tag != "" {
			fmt.Fprintf(&b, "- **Tag:** %s\n", *row.Tag)
		}
		if row.StrategyName != nil && *row.StrategyName != "" {
			fmt.Fprintf(&b, "- **Strategy:** %s\n", *row.StrategyName)
		}
		mistakes, emotions := labelNames(labels[row.ID])
		if len(mistakes) > 0 {
			fmt.Fprintf(&b, "- **Mistakes:** %s\n", strings.Join(mistakes, ", "))
		}
		if len(emotions) > 0 {
			fmt.Fprintf(&b, "- **Emotions:** %s\n", strings.Join(emotions, ", "))
		}
		if (row.Tag != nil && *row.Tag != "") || (row.StrategyName != nil && *row.StrategyName != "") || len(mistakes) > 0 || len(emotions) > 0 {
			b.WriteString("\n")
		}

		// Summary + per-trade bullet list — same for every scope so the
		// export is uniform regardless of how the user organized the entries.
		// Cap the bullet list at exportTradeListCap; the summary already
		// carries the full count, so a day with 73 fills doesn't drown the
		// document in single-fill lines.
		if summary.N > 0 {
			fmt.Fprintf(&b, "**Net** %s · **Gross** %s · **Comm** %s · **%dW/%dL** · %d trade%s\n\n",
				signedMoney(summary.Net), signedMoney(summary.Gross),
				fmt.Sprintf("%.2f", summary.Commission),
				summary.Wins, summary.Losses, summary.N, plural(summary.N))
			if len(trades) <= exportTradeListCap {
				for _, lt := range trades {
					net := "—"
					if lt.NetPnl != nil {
						net = signedMoney(*lt.NetPnl)
					}
					fmt.Fprintf(&b, "- %s · %s %s · qty %s · %s\n",
						lt.AccountName, lt.Symbol, lt.Direction, formatQty(lt.Qty), net)
				}
			} else {
				fmt.Fprintf(&b, "_%d trades not listed individually — see counts above._\n",
					len(trades))
			}
			b.WriteString("\n")
		}

		b.WriteString(strings.TrimRight(row.Body, "\n"))
		b.WriteString("\n\n---\n\n")
	}

	filename := exportFilename(filters, "md")
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write([]byte(b.String()))
}

// JournalExportPDF serves GET /journal/export.pdf — a server-generated
// PDF built from the same filter parameters as the feed. Owning the PDF
// rendering means the output is identical across browsers (Safari, Chrome,
// curl) — the long-running blank-PDF issue with browser Print → Save as
// PDF was caused by exactly that fragility.
func (h *Handlers) JournalExportPDF(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	filters := parseJournalFilters(r, tz)

	rows, err := listJournalFeedFiltered(ctx, h.rdb(), filters, 10000)
	if err != nil {
		h.serverError(w, fmt.Errorf("export feed: %w", err))
		return
	}

	labels, err := loadEntryLabels(ctx, h.rdb(), feedRowIDs(rows))
	if err != nil {
		h.serverError(w, fmt.Errorf("export labels (pdf): %w", err))
		return
	}
	entries := make([]pdfEntry, 0, len(rows))
	for _, row := range rows {
		entryDate := row.EntryDate.In(tz)
		// Pull related trades for every entry — day/week/trade/group all
		// share the same shape in the PDF now.
		trades, err := h.tradesForEntry(ctx, row, tz)
		if err != nil {
			h.serverError(w, fmt.Errorf("export trades (pdf) for entry %d: %w", row.ID, err))
			return
		}
		summary := summarizeTradeRows(trades)

		e := pdfEntry{
			Scope:   row.Scope,
			Body:    row.Body,
			Summary: summary,
			Trades:  trades,
		}
		switch row.Scope {
		case "week":
			_, wk := entryDate.ISOWeek()
			sunday := entryDate.AddDate(0, 0, 6)
			e.Title = fmt.Sprintf("Week %d · %s – %s",
				wk, entryDate.Format("Jan 2"), sunday.Format("Jan 2, 2006"))
		case "group":
			e.Title = fmt.Sprintf("Group · %d trade%s · %s",
				summary.N, plural(summary.N), entryDate.Format("Mon, 2 Jan 2006"))
		default:
			e.Title = entryDate.Format("Mon, 2 Jan 2006")
		}
		if row.AccountName != nil && row.Scope != "group" {
			e.Account = *row.AccountName
		}
		if row.Tag != nil {
			e.Tag = *row.Tag
		}
		if row.StrategyName != nil {
			e.Strategy = *row.StrategyName
		}
		e.Mistakes, e.Emotions = labelNames(labels[row.ID])
		entries = append(entries, e)
	}

	header := pdfHeader{
		Title:       "Journal Export",
		GeneratedAt: time.Now().In(tz).Format("2006-01-02 15:04 MST"),
		FilterDesc:  filterDescription(filters),
	}

	filename := exportFilename(filters, "pdf")
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if err := RenderJournalPDF(w, header, entries); err != nil {
		h.logger().Error("render pdf", "err", err)
	}
}

// filterDescription produces a short human-readable summary of the active
// filters, used in the export's header line. Empty when nothing is set.
func filterDescription(f journalFilters) string {
	var parts []string
	if f.fromStr != "" || f.toStr != "" {
		from, to := f.fromStr, f.toStr
		if from == "" {
			from = "…"
		}
		if to == "" {
			to = "…"
		}
		parts = append(parts, "range "+from+"–"+to)
	}
	if len(f.scopes) > 0 {
		parts = append(parts, "scope: "+strings.Join(f.scopes, ", "))
	}
	if len(f.tags) > 0 {
		parts = append(parts, "tag: "+strings.Join(f.tags, ", "))
	}
	if len(f.strategies) > 0 || f.strategyNone {
		n := len(f.strategies)
		if f.strategyNone {
			n++
		}
		parts = append(parts, fmt.Sprintf("strategy: %d selected", n))
	}
	if len(f.mistakes) > 0 || f.mistakeNone {
		ms := append([]string{}, f.mistakes...)
		if f.mistakeNone {
			ms = append(ms, "none")
		}
		parts = append(parts, "mistake: "+strings.Join(ms, ", "))
	}
	if len(f.emotions) > 0 {
		parts = append(parts, "emotion: "+strings.Join(f.emotions, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Filters — " + strings.Join(parts, "; ")
}

// exportFilename builds a stable filename for the download based on the
// filter date range. Falls back to today's date when no range is active.
func exportFilename(f journalFilters, ext string) string {
	if f.fromStr != "" && f.toStr != "" {
		return "journal-" + f.fromStr + "_to_" + f.toStr + "." + ext
	}
	if f.fromStr != "" {
		return "journal-from-" + f.fromStr + "." + ext
	}
	if f.toStr != "" {
		return "journal-until-" + f.toStr + "." + ext
	}
	return "journal-" + time.Now().Format("2006-01-02") + "." + ext
}
