package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// The page editors' saves. Each resolves the singleton for its scope, then:
// no entry + empty body -> OutcomeEmpty; no entry + body -> create;
// entry + empty body -> delete; entry + body -> update. Mood is editable on
// day entries only and preserved elsewhere.

// SaveDay saves the user's entry for the trading-tz calendar day.
func (s *Service) SaveDay(ctx context.Context, userID int64, day time.Time, body string, mood *int64, tag *string, strategyID *int64) (Outcome, error) {
	uid := &userID
	existing, err := s.Q.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: uid, EntryDate: day})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if body == "" {
			return OutcomeEmpty, nil
		}
		if _, err := s.Q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: uid, EntryDate: day, Body: body, Mood: mood, Tag: tag, Scope: "day", StrategyID: strategyID,
		}); err != nil {
			return "", fmt.Errorf("create day entry: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("load day entry: %w", err)
	default:
		if body == "" {
			return s.deleteEntry(ctx, userID, existing.ID)
		}
		if err := s.Q.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
			UserID: uid, Body: body, Mood: mood, Tag: tag, StrategyID: strategyID, ID: existing.ID,
		}); err != nil {
			return "", fmt.Errorf("update day entry: %w", err)
		}
	}
	s.changed(ctx, userID)
	return OutcomeSaved, nil
}

// DeleteDay removes the user's day entry; false when there was none.
func (s *Service) DeleteDay(ctx context.Context, userID int64, day time.Time) (bool, error) {
	existing, err := s.Q.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: &userID, EntryDate: day})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load day entry: %w", err)
	}
	_, err = s.deleteEntry(ctx, userID, existing.ID)
	return err == nil, err
}

// SaveWeek saves the user's entry for the ISO week starting at monday.
func (s *Service) SaveWeek(ctx context.Context, userID int64, monday time.Time, body string, tag *string, strategyID *int64) (Outcome, error) {
	uid := &userID
	existing, err := s.Q.GetWeekJournalEntry(ctx, models.GetWeekJournalEntryParams{UserID: uid, EntryDate: monday})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if body == "" {
			return OutcomeEmpty, nil
		}
		if _, err := s.Q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: uid, EntryDate: monday, Body: body, Tag: tag, Scope: "week", StrategyID: strategyID,
		}); err != nil {
			return "", fmt.Errorf("create week entry: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("load week entry: %w", err)
	default:
		if body == "" {
			return s.deleteEntry(ctx, userID, existing.ID)
		}
		if err := s.Q.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
			UserID: uid, Body: body, Mood: existing.Mood, Tag: tag, StrategyID: strategyID, ID: existing.ID,
		}); err != nil {
			return "", fmt.Errorf("update week entry: %w", err)
		}
	}
	s.changed(ctx, userID)
	return OutcomeSaved, nil
}

// DeleteWeek removes the user's week entry; false when there was none.
func (s *Service) DeleteWeek(ctx context.Context, userID int64, monday time.Time) (bool, error) {
	existing, err := s.Q.GetWeekJournalEntry(ctx, models.GetWeekJournalEntryParams{UserID: &userID, EntryDate: monday})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load week entry: %w", err)
	}
	_, err = s.deleteEntry(ctx, userID, existing.ID)
	return err == nil, err
}

// SaveTrade saves the entry on one of the user's trades. ErrNotFound when
// the trade is not the caller's.
func (s *Service) SaveTrade(ctx context.Context, userID, tradeID int64, body string, tag *string, strategyID *int64) (Outcome, error) {
	uid := &userID
	trade, err := s.Q.GetTradeByID(ctx, models.GetTradeByIDParams{ID: tradeID, UserID: userID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("load trade: %w", err)
	}
	existing, err := s.Q.GetJournalEntryByTradeID(ctx, &tradeID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if body == "" {
			return OutcomeEmpty, nil
		}
		accountID := trade.AccountID
		if _, err := s.Q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			UserID: uid, TradeID: &tradeID, AccountID: &accountID, EntryDate: tradeEntryDate(trade.EntryTime, trade.ExitTime),
			Body: body, Tag: tag, Scope: "trade", StrategyID: strategyID,
		}); err != nil {
			return "", fmt.Errorf("create trade entry: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("load trade entry: %w", err)
	default:
		if body == "" {
			return s.deleteEntry(ctx, userID, existing.ID)
		}
		if err := s.Q.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
			UserID: uid, Body: body, Mood: existing.Mood, Tag: tag, StrategyID: strategyID, ID: existing.ID,
		}); err != nil {
			return "", fmt.Errorf("update trade entry: %w", err)
		}
	}
	s.changed(ctx, userID)
	return OutcomeSaved, nil
}

// SaveGroup saves the body of the user's group entry; an empty body deletes
// it (the junction rows go with ON DELETE CASCADE).
func (s *Service) SaveGroup(ctx context.Context, userID, entryID int64, body string, tag *string, strategyID *int64) (Outcome, error) {
	if body == "" {
		return s.deleteEntry(ctx, userID, entryID)
	}
	if err := s.Q.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
		UserID: &userID, Body: body, Mood: nil, Tag: tag, StrategyID: strategyID, ID: entryID,
	}); err != nil {
		return "", fmt.Errorf("update group entry: %w", err)
	}
	s.changed(ctx, userID)
	return OutcomeSaved, nil
}

// Delete removes the user's entry by id (no error when it was not theirs or
// already gone: the query's user predicate simply matches nothing).
func (s *Service) Delete(ctx context.Context, userID, entryID int64) error {
	_, err := s.deleteEntry(ctx, userID, entryID)
	return err
}

func (s *Service) deleteEntry(ctx context.Context, userID, entryID int64) (Outcome, error) {
	if err := s.Q.DeleteJournalEntry(ctx, models.DeleteJournalEntryParams{ID: entryID, UserID: &userID}); err != nil {
		return "", fmt.Errorf("delete entry: %w", err)
	}
	s.changed(ctx, userID)
	return OutcomeDeleted, nil
}
