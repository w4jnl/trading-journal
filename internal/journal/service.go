// Package journal is the one place journal data is written. The HTML
// handlers, the JSON API, the MCP tools, the AI reviewer and the GEX
// annotator all create, save, append, tag, assign and link entries through
// Service; none of them touches journal_entries or journal_entry_trades on
// its own. Two rules every method keeps: the owning user is an explicit
// parameter, never implied by the query, and every committed write reports
// through Changed (the live-data bump) so open pages learn about it.
package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Service holds the writer pool and the two optional callbacks.
type Service struct {
	DB *sql.DB // writer pool: the transactions and the hand-written writes
	Q  *models.Queries
	TZ func() *time.Location // trading timezone (dates, note stamps)
	// Changed runs after every committed write with the owning user; nil
	// means nobody listens (tests).
	Changed func(ctx context.Context, userID int64)
	// NewAnnotator builds the per-pass gamma-context annotator PrepareDay
	// calls for every entry it created or reused (the GEX annotator, which
	// disables itself for the rest of a pass after a fetch failure); nil
	// skips annotation.
	NewAnnotator func() Annotator
	// Locked reports an entry the editors must not change right now: an AI
	// review of it is running and will append to it. The page saves and
	// deletes refuse it with ErrReviewPending; appends (the API, MCP and the
	// review itself) are unaffected. nil = nothing is ever locked.
	Locked func(entryID int64) bool
}

// locked reports whether the editors must leave entryID alone for now.
func (s *Service) locked(entryID int64) bool { return s.Locked != nil && s.Locked(entryID) }

// Annotator adds context to a prepared entry from its representative trade.
type Annotator interface {
	Annotate(ctx context.Context, entryID int64, t PrepTrade)
}

// Markers inside a body that say who reviewed the entry, and the placeholder
// PrepareDay seeds so a later note can tell "not yet analyzed" from real text.
const (
	// MarkerLocal tags analyses written by the in-app AI; MarkerMCP those
	// written by an external AI through the MCP server. Separate markers let
	// each analyze an entry independently - one's review never makes the
	// other skip it.
	MarkerLocal     = "tj-ai:v1"
	MarkerMCP       = "tj-ai-mcp:v1"
	PlaceholderBody = "---\n\n_AI review_"
)

// Outcome is what a save did, in the words the pages flash.
type Outcome string

const (
	OutcomeSaved   Outcome = "saved"   // created or updated
	OutcomeDeleted Outcome = "deleted" // an empty body removed the entry
	OutcomeEmpty   Outcome = "empty"   // nothing to save and nothing to delete
)

var (
	// ErrNotFound: the entry (or trade) is not the caller's, or does not exist.
	ErrNotFound = errors.New("journal: not found")
	// ErrReviewPending: a page save or delete of an entry an AI review is
	// still writing (Service.Locked).
	ErrReviewPending = errors.New("an AI review of this note is in progress; it can be edited again when the review finishes")
	// ErrNoOwnedTrades: none of the given trade ids belong to the caller.
	ErrNoOwnedTrades = errors.New("journal: none of the trades are yours")
)

// ValidationError is bad input (an HTTP 400 for the API, a tool error for
// MCP), as opposed to an internal failure.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

// BadInput builds a ValidationError.
func BadInput(format string, a ...any) error { return ValidationError{fmt.Sprintf(format, a...)} }

// CreateInput is the create-entry payload shared by the REST POST /journal
// handler (JSON-decoded) and the MCP create_journal_entry tool. Which
// scope-specific field is required depends on Scope: day->Date,
// week->Week, trade->TradeID, group->TradeIDs.
type CreateInput struct {
	Scope      string  `json:"scope"`
	Body       string  `json:"body"`
	Tag        *string `json:"tag"`
	Mood       *int64  `json:"mood"` // day scope only
	StrategyID *int64  `json:"strategyId"`
	Date       string  `json:"date"`     // day:  YYYY-MM-DD
	Week       string  `json:"week"`     // week: YYYY-Www
	TradeID    *int64  `json:"tradeId"`  // trade
	TradeIDs   []int64 `json:"tradeIds"` // group
	// Review labels from the fixed vocabulary (labels.go); any scope.
	Mistakes []string `json:"mistakes"`
	Emotions []string `json:"emotions"`
}

// Conflict reports that a singleton (day/week/trade) entry already exists.
// It is returned instead of an error so callers can surface the existing id.
type Conflict struct {
	EntryID int64
	Message string
}

func (s *Service) tz() *time.Location {
	if s.TZ != nil {
		if l := s.TZ(); l != nil {
			return l
		}
	}
	return time.UTC
}

func (s *Service) changed(ctx context.Context, userID int64) {
	if s.Changed != nil {
		s.Changed(ctx, userID)
	}
}

// ParseTag validates an optional tag. nil/empty -> nil (no tag); a value
// outside the whitelist -> ok=false.
func ParseTag(t *string) (*string, bool) {
	if t == nil {
		return nil, true
	}
	v := strings.TrimSpace(*t)
	if v == "" {
		return nil, true
	}
	switch v {
	case "good", "neutral", "bad":
		return &v, true
	}
	return nil, false
}

// ParseMood validates an optional 1-5 mood. nil -> nil; out of range -> ok=false.
func ParseMood(m *int64) (*int64, bool) {
	if m == nil {
		return nil, true
	}
	if *m < 1 || *m > 5 {
		return nil, false
	}
	v := *m
	return &v, true
}

// ValidateStrategyID confirms a non-null strategy id belongs to the user and
// is not archived. nil/0 -> nil (no strategy); unknown, foreign or archived
// -> ok=false.
func (s *Service) ValidateStrategyID(ctx context.Context, userID int64, id *int64) (*int64, bool) {
	if id == nil || *id == 0 {
		return nil, true
	}
	st, err := s.Q.GetStrategy(ctx, models.GetStrategyParams{ID: *id, UserID: userID})
	if err != nil || st.ArchivedAt != nil {
		return nil, false
	}
	v := *id
	return &v, true
}

// FormatAppendedNote builds the dated markdown block appended to a journal
// body: a thematic break, an italic dated heading in the trading timezone,
// then the note. Never overwrites existing content.
func FormatAppendedNote(now time.Time, loc *time.Location, note string) string {
	return formatAppendedNoteAs(now, loc, "Note added", note)
}

// formatAppendedNoteAs is FormatAppendedNote with the stamp's words given
// (the AI review writes them in the review language).
func formatAppendedNoteAs(now time.Time, loc *time.Location, added, note string) string {
	stamp := now.In(loc).Format("2006-01-02 15:04 MST")
	return "\n\n---\n\n_" + added + " " + stamp + "_\n\n" + strings.TrimRight(note, " \t\n") + "\n"
}

// AppendNote adds note to an entry the user owns. While the entry is still
// the PrepareDay placeholder (created but not yet analyzed) the note
// REPLACES it - keeping the leading "---" divider, dropping the "pending"
// line - so an analyzed entry never carries a stale placeholder above the
// real review. Otherwise the note is appended as a dated, divider-separated
// block. The REST, MCP, AI-review and GEX-annotation paths all funnel here.
func (s *Service) AppendNote(ctx context.Context, userID int64, entry models.JournalEntry, note string) error {
	return s.AppendNoteAs(ctx, userID, entry, note, "Note added")
}

// AppendNoteAs is AppendNote with the appended block's stamp words given
// ("Note added" in English), for a note written in another language.
func (s *Service) AppendNoteAs(ctx context.Context, userID int64, entry models.JournalEntry, note, added string) error {
	note = strings.TrimSpace(note)
	if strings.TrimSpace(entry.Body) == strings.TrimSpace(PlaceholderBody) {
		seeded := "---\n\n" + strings.TrimRight(note, " \t\n") + "\n"
		res, err := s.DB.ExecContext(ctx,
			"UPDATE journal_entries SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?",
			seeded, entry.ID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		s.changed(ctx, userID)
		return nil
	}
	block := formatAppendedNoteAs(time.Now(), s.tz(), added, note)
	if err := s.Q.AppendJournalEntryBody(ctx, models.AppendJournalEntryBodyParams{
		Body: block, ID: entry.ID, UserID: &userID,
	}); err != nil {
		return err
	}
	s.changed(ctx, userID)
	return nil
}

// EntryHasMarker reports whether the user's entry carries marker in its body
// (false for a missing or foreign entry).
func (s *Service) EntryHasMarker(ctx context.Context, userID, entryID int64, marker string) bool {
	e, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID})
	if err != nil {
		return false
	}
	return strings.Contains(e.Body, marker)
}

// Create validates and creates one entry for any scope. It returns a
// *Conflict (not an error) when a day/week/trade singleton already exists, a
// ValidationError for bad input, and any other error is internal.
func (s *Service) Create(ctx context.Context, userID int64, in CreateInput) (models.JournalEntry, *Conflict, error) {
	none := models.JournalEntry{}
	tz := s.tz()
	tag, ok := ParseTag(in.Tag)
	if !ok {
		return none, nil, BadInput("tag must be one of: good, neutral, bad")
	}
	strategyID, ok := s.ValidateStrategyID(ctx, userID, in.StrategyID)
	if !ok {
		return none, nil, BadInput("unknown strategyId")
	}
	// Labels are validated before any write so a bad key fails cleanly.
	mistakes, err := normalizeLabels(KindMistake, in.Mistakes)
	if err != nil {
		return none, nil, err
	}
	emotions, err := normalizeLabels(KindEmotion, in.Emotions)
	if err != nil {
		return none, nil, err
	}
	labelled := func(e models.JournalEntry) (models.JournalEntry, *Conflict, error) {
		if err := s.writeLabels(ctx, e.ID, KindMistake, mistakes, false); err != nil {
			return none, nil, err
		}
		if err := s.writeLabels(ctx, e.ID, KindEmotion, emotions, false); err != nil {
			return none, nil, err
		}
		s.changed(ctx, userID)
		return e, nil, nil
	}
	body := strings.TrimSpace(in.Body)
	uid := &userID

	switch in.Scope {
	case "day":
		if body == "" {
			return none, nil, BadInput("body is required for a day entry")
		}
		if strings.TrimSpace(in.Date) == "" {
			return none, nil, BadInput("date is required for scope=day (YYYY-MM-DD)")
		}
		date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(in.Date), tz)
		if err != nil {
			return none, nil, BadInput("bad date; expected YYYY-MM-DD")
		}
		if date.After(todayIn(tz)) {
			return none, nil, BadInput("journal entries are not allowed on future dates")
		}
		if existing, err := s.Q.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: uid, EntryDate: date}); err == nil {
			return none, &Conflict{EntryID: existing.ID, Message: "a day entry already exists for " + date.Format("2006-01-02")}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return none, nil, fmt.Errorf("check existing day entry: %w", err)
		}
		mood, ok := ParseMood(in.Mood)
		if !ok {
			return none, nil, BadInput("mood must be an integer 1-5")
		}
		e, err := s.Q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: uid, EntryDate: date, Body: body, Mood: mood, Tag: tag, Scope: "day", StrategyID: strategyID,
		})
		if err != nil {
			return none, nil, fmt.Errorf("create entry: %w", err)
		}
		return labelled(e)

	case "week":
		if body == "" {
			return none, nil, BadInput("body is required for a week entry")
		}
		if strings.TrimSpace(in.Week) == "" {
			return none, nil, BadInput("week is required for scope=week (YYYY-Www, e.g. 2026-W23)")
		}
		monday, _, _, err := ParseISOWeek(strings.TrimSpace(in.Week), tz)
		if err != nil {
			return none, nil, BadInput("bad week; expected YYYY-Www (e.g. 2026-W23)")
		}
		if existing, err := s.Q.GetWeekJournalEntry(ctx, models.GetWeekJournalEntryParams{UserID: uid, EntryDate: monday}); err == nil {
			return none, &Conflict{EntryID: existing.ID, Message: "a week entry already exists for " + strings.TrimSpace(in.Week)}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return none, nil, fmt.Errorf("check existing week entry: %w", err)
		}
		e, err := s.Q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: uid, EntryDate: monday, Body: body, Tag: tag, Scope: "week", StrategyID: strategyID,
		})
		if err != nil {
			return none, nil, fmt.Errorf("create entry: %w", err)
		}
		return labelled(e)

	case "trade":
		if body == "" {
			return none, nil, BadInput("body is required for a trade entry")
		}
		if in.TradeID == nil || *in.TradeID <= 0 {
			return none, nil, BadInput("tradeId is required for scope=trade")
		}
		trade, err := s.Q.GetTradeByID(ctx, models.GetTradeByIDParams{ID: *in.TradeID, UserID: userID})
		if err != nil {
			// Foreign/unknown id is request input -> 400. Be specific so a client
			// self-corrects (re-list) instead of assuming a backend problem.
			return none, nil, BadInput("tradeId %d not found on your accounts; re-list with list_trades and pass an exact trade id (not an entry or account id)", *in.TradeID)
		}
		if existing, err := s.Q.GetJournalEntryByTradeID(ctx, in.TradeID); err == nil {
			return none, &Conflict{EntryID: existing.ID, Message: "a journal entry already exists for this trade"}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return none, nil, fmt.Errorf("check existing trade entry: %w", err)
		}
		accountID := trade.AccountID
		e, err := s.Q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: uid, TradeID: in.TradeID, AccountID: &accountID, EntryDate: tradeEntryDate(trade.EntryTime, trade.ExitTime),
			Body: body, Tag: tag, Scope: "trade", StrategyID: strategyID,
		})
		if err != nil {
			return none, nil, fmt.Errorf("create entry: %w", err)
		}
		return labelled(e)

	case "group":
		if len(in.TradeIDs) == 0 {
			return none, nil, BadInput("tradeIds is required for scope=group (at least one trade)")
		}
		e, err := s.createGroup(ctx, userID, in.TradeIDs, body, tag, strategyID)
		if errors.Is(err, ErrNoOwnedTrades) {
			return none, nil, BadInput("none of the given tradeIds were found on your accounts (got %v); they may be entry ids or from a different day - re-list with list_trades and pass exact trade ids", in.TradeIDs)
		}
		if err != nil {
			return none, nil, err
		}
		return labelled(e)

	default:
		return none, nil, BadInput("scope must be one of: day, week, trade, group")
	}
}

// CreateGroup creates an empty group entry over the caller's trades among
// ids (the /trades bulk action and the group editor). ErrNoOwnedTrades when
// none of the ids are the caller's.
func (s *Service) CreateGroup(ctx context.Context, userID int64, ids []int64) (models.JournalEntry, error) {
	return s.createGroup(ctx, userID, ids, "", nil, nil)
}

// createGroup keeps only the trade ids the caller owns (a guessed or foreign
// id can never be linked into the user's group), dates the entry at the
// latest exit among them, and creates entry + links in one transaction.
func (s *Service) createGroup(ctx context.Context, userID int64, ids []int64, body string, tag *string, strategyID *int64) (models.JournalEntry, error) {
	none := models.JournalEntry{}
	owned, err := s.ownedTrades(ctx, userID, ids)
	if err != nil {
		return none, err
	}
	if len(owned) == 0 {
		return none, ErrNoOwnedTrades
	}
	entryDate, err := s.MaxExitDate(ctx, userID, owned)
	if err != nil {
		return none, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return none, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	q := s.Q.WithTx(tx)
	e, err := q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: &userID, EntryDate: entryDate, Body: body, Tag: tag, Scope: "group", StrategyID: strategyID,
	})
	if err != nil {
		return none, fmt.Errorf("create entry: %w", err)
	}
	for _, id := range owned {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)", e.ID, id); err != nil {
			return none, fmt.Errorf("link trade %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return none, fmt.Errorf("commit: %w", err)
	}
	s.changed(ctx, userID)
	return e, nil
}

// LinkTrades adds the caller's trades among ids to the caller's group entry
// (idempotent: INSERT OR IGNORE on the composite key) and returns how many
// were linked. ErrNotFound when the entry is not the caller's group entry;
// ErrNoOwnedTrades when none of the ids are the caller's.
func (s *Service) LinkTrades(ctx context.Context, userID, entryID int64, ids []int64) (int, error) {
	var one int64
	if err := s.DB.QueryRowContext(ctx,
		"SELECT 1 FROM journal_entries WHERE id = ? AND user_id = ? AND scope = 'group'", entryID, userID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("load group entry: %w", err)
	}
	owned, err := s.ownedTrades(ctx, userID, ids)
	if err != nil {
		return 0, err
	}
	if len(owned) == 0 {
		return 0, ErrNoOwnedTrades
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	for _, id := range owned {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)", entryID, id); err != nil {
			return 0, fmt.Errorf("link trade %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	s.changed(ctx, userID)
	return len(owned), nil
}

// UnlinkTrade removes one trade from the caller's group entry. Ownership
// lives in the predicate: false when nothing matched (a foreign id must not
// confirm or mutate).
func (s *Service) UnlinkTrade(ctx context.Context, userID, entryID, tradeID int64) (bool, error) {
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM journal_entry_trades
		 WHERE journal_entry_id = ? AND trade_id = ?
		   AND journal_entry_id IN (SELECT id FROM journal_entries WHERE user_id = ? AND scope = 'group')`,
		entryID, tradeID, userID)
	if err != nil {
		return false, fmt.Errorf("unlink: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	s.changed(ctx, userID)
	return true, nil
}

// ownedTrades filters ids down to trades on the caller's accounts, keeping
// the order and dropping duplicates.
func (s *Service) ownedTrades(ctx context.Context, userID int64, ids []int64) ([]int64, error) {
	owned := make([]int64, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var one int64
		err := s.DB.QueryRowContext(ctx,
			"SELECT 1 FROM trades t JOIN accounts a ON a.id = t.account_id WHERE t.id = ? AND a.user_id = ?", id, userID).Scan(&one)
		switch {
		case err == nil:
			owned = append(owned, id)
		case errors.Is(err, sql.ErrNoRows):
		default:
			return nil, fmt.Errorf("check trade %d: %w", id, err)
		}
	}
	return owned, nil
}

// MaxExitDate returns the trading-tz date of the latest exit among the
// caller's trades in ids (today if none has exited yet) - a group entry's
// date. ORDER BY ... LIMIT 1 rather than MAX(): the aggregate loses the
// column type in modernc/sqlite and cannot scan into time.Time.
func (s *Service) MaxExitDate(ctx context.Context, userID int64, ids []int64) (time.Time, error) {
	tz := s.tz()
	ph := strings.Repeat("?,", len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	var maxExit time.Time
	err := s.DB.QueryRowContext(ctx,
		"SELECT t.exit_time FROM trades t JOIN accounts a ON a.id = t.account_id WHERE a.user_id = ? AND t.id IN ("+ph[:len(ph)-1]+") AND t.exit_time IS NOT NULL ORDER BY t.exit_time DESC LIMIT 1",
		args...).Scan(&maxExit)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, fmt.Errorf("resolve entry date: %w", err)
		}
		maxExit = time.Now().In(tz)
	}
	y, m, d := maxExit.In(tz).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tz), nil
}

// AssignStrategyIfUnset sets an entry's strategy ONLY when it has none, so
// an existing assignment is never changed (assigned=false then). Trade and
// group entries only: a strategy describes a single setup, a day/week spans
// mixed ones. ValidationError for an unknown entry/strategy or wrong scope.
func (s *Service) AssignStrategyIfUnset(ctx context.Context, userID, entryID, strategyID int64) (bool, error) {
	e, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, BadInput("entry %d not found", entryID)
		}
		return false, fmt.Errorf("load entry: %w", err)
	}
	if e.Scope != "trade" && e.Scope != "group" {
		return false, BadInput("strategy applies to trade or group entries, not %s", e.Scope)
	}
	st, err := s.Q.GetStrategy(ctx, models.GetStrategyParams{ID: strategyID, UserID: userID})
	if err != nil {
		return false, BadInput("unknown strategyId %d", strategyID)
	}
	if st.ArchivedAt != nil {
		return false, BadInput("strategy %d is archived (retired) and cannot be assigned", strategyID)
	}
	res, err := s.DB.ExecContext(ctx,
		"UPDATE journal_entries SET strategy_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND strategy_id IS NULL",
		strategyID, entryID, userID)
	if err != nil {
		return false, fmt.Errorf("assign strategy: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.changed(ctx, userID)
	}
	return n > 0, nil
}

// AssignTagIfUnset sets an entry's tag (good|neutral|bad) ONLY when it has
// none yet, so a tag the user set is never changed (assigned=false then).
func (s *Service) AssignTagIfUnset(ctx context.Context, userID, entryID int64, tag string) (bool, error) {
	t, ok := ParseTag(&tag)
	if !ok || t == nil {
		return false, BadInput("tag must be one of: good, neutral, bad")
	}
	if _, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, BadInput("entry %d not found", entryID)
		}
		return false, fmt.Errorf("load entry: %w", err)
	}
	res, err := s.DB.ExecContext(ctx,
		"UPDATE journal_entries SET tag = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND tag IS NULL",
		*t, entryID, userID)
	if err != nil {
		return false, fmt.Errorf("assign tag: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.changed(ctx, userID)
	}
	return n > 0, nil
}

// todayIn is midnight today in tz.
func todayIn(tz *time.Location) time.Time {
	now := time.Now().In(tz)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
}

// tradeEntryDate is the date a trade entry files under: the exit (or the
// entry while still open), so day-by-date queries find it with the trade.
func tradeEntryDate(entry time.Time, exit *time.Time) time.Time {
	if exit != nil {
		return *exit
	}
	return entry
}
