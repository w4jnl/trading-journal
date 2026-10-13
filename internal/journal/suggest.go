package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Suggestions: what an in-app AI review proposes for a note instead of
// writing it. The trader accepts one (it becomes the field, as their own) or
// dismisses it (kept dismissed, so a later review does not propose it again).

// Suggestion kinds: the tag, the strategy, and the two label kinds.
const (
	SuggestTag      = "tag"
	SuggestStrategy = "strategy"
)

// Suggestion is one open proposal: Value is the tag word, the strategy id or
// the label key.
type Suggestion struct {
	Kind  string
	Value string
}

// Suggest records an AI review's proposals for the user's entry and returns
// the ones open after it - this review's proposals that still apply and were
// not dismissed before. A tag is proposed only while the entry has none, a
// strategy only on a trade or group entry without one (and only an active
// strategy of the user's), labels only from the vocabulary and not already on
// the entry. Anything else is dropped.
func (s *Service) Suggest(ctx context.Context, userID, entryID int64, tag string, strategyID int64, mistakes, emotions []string) ([]Suggestion, error) {
	e, err := s.ownEntry(ctx, userID, entryID)
	if err != nil {
		return nil, err
	}
	have, err := s.labelSet(ctx, entryID)
	if err != nil {
		return nil, err
	}
	var want []Suggestion
	if t, ok := ParseTag(&tag); ok && t != nil && e.Tag == nil {
		want = append(want, Suggestion{SuggestTag, *t})
	}
	if strategyID > 0 && e.StrategyID == nil && (e.Scope == "trade" || e.Scope == "group") {
		if st, err := s.Q.GetStrategy(ctx, models.GetStrategyParams{ID: strategyID, UserID: userID}); err == nil && st.ArchivedAt == nil {
			want = append(want, Suggestion{SuggestStrategy, strconv.FormatInt(strategyID, 10)})
		}
	}
	for _, kl := range []struct {
		kind string
		keys []string
	}{{KindMistake, mistakes}, {KindEmotion, emotions}} {
		for _, k := range kl.keys {
			k = strings.TrimSpace(strings.ToLower(k))
			if hasKey(kl.kind, k) && !have[kl.kind+":"+k] {
				want = append(want, Suggestion{kl.kind, k})
			}
		}
	}
	var open []Suggestion
	for _, w := range want {
		if w.Kind == SuggestTag || w.Kind == SuggestStrategy {
			if err := s.Q.DropOtherOpenSuggestions(ctx, models.DropOtherOpenSuggestionsParams{JournalEntryID: entryID, Kind: w.Kind, Value: w.Value}); err != nil {
				return nil, fmt.Errorf("replace suggestion: %w", err)
			}
		}
		if err := s.Q.UpsertSuggestion(ctx, models.UpsertSuggestionParams{JournalEntryID: entryID, Kind: w.Kind, Value: w.Value}); err != nil {
			return nil, fmt.Errorf("record suggestion: %w", err)
		}
		row, err := s.Q.GetSuggestion(ctx, models.GetSuggestionParams{JournalEntryID: entryID, Kind: w.Kind, Value: w.Value})
		if err == nil && row.Status == "open" {
			open = append(open, w)
		}
	}
	if len(open) > 0 {
		s.changed(ctx, userID)
	}
	return open, nil
}

// Suggestions lists the user's entry's open suggestions that still apply: a
// tag or strategy only while the field is empty, a label not yet on the entry
// (the trader may have set it in the editor meanwhile).
func (s *Service) Suggestions(ctx context.Context, userID, entryID int64) ([]Suggestion, error) {
	if entryID == 0 {
		return nil, nil
	}
	e, err := s.ownEntry(ctx, userID, entryID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Q.ListOpenSuggestions(ctx, entryID)
	if err != nil {
		return nil, fmt.Errorf("list suggestions: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	have, err := s.labelSet(ctx, entryID)
	if err != nil {
		return nil, err
	}
	var out []Suggestion
	for _, r := range rows {
		switch r.Kind {
		case SuggestTag:
			if e.Tag != nil {
				continue
			}
		case SuggestStrategy:
			if e.StrategyID != nil {
				continue
			}
		default:
			if have[r.Kind+":"+r.Value] {
				continue
			}
		}
		out = append(out, Suggestion{r.Kind, r.Value})
	}
	return out, nil
}

// AcceptSuggestion makes an open suggestion the trader's own value (source
// trader) and removes it. A tag or strategy set meanwhile is not overwritten.
// ErrNotFound when the entry is not the user's or holds no such suggestion;
// ErrReviewPending while a review is writing the entry.
func (s *Service) AcceptSuggestion(ctx context.Context, userID, entryID int64, kind, value string) error {
	if _, err := s.ownEntry(ctx, userID, entryID); err != nil {
		return err
	}
	if s.locked(entryID) {
		return ErrReviewPending
	}
	row, err := s.Q.GetSuggestion(ctx, models.GetSuggestionParams{JournalEntryID: entryID, Kind: kind, Value: value})
	if err != nil || row.Status != "open" {
		return ErrNotFound
	}
	switch kind {
	case SuggestTag:
		if _, err := s.DB.ExecContext(ctx,
			"UPDATE journal_entries SET tag = ?, tag_source = 'trader', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND tag IS NULL",
			value, entryID, userID); err != nil {
			return fmt.Errorf("accept tag: %w", err)
		}
	case SuggestStrategy:
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return ErrNotFound
		}
		if st, err := s.Q.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: userID}); err != nil || st.ArchivedAt != nil {
			return BadInput("strategy %d is no longer available", id)
		}
		if _, err := s.DB.ExecContext(ctx,
			"UPDATE journal_entries SET strategy_id = ?, strategy_source = 'trader', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND strategy_id IS NULL",
			id, entryID, userID); err != nil {
			return fmt.Errorf("accept strategy: %w", err)
		}
	case KindMistake, KindEmotion:
		if !hasKey(kind, value) {
			return ErrNotFound
		}
		if err := s.writeLabels(ctx, entryID, kind, []string{value}, false, SourceTrader); err != nil {
			return err
		}
	default:
		return ErrNotFound
	}
	if err := s.Q.DeleteSuggestion(ctx, models.DeleteSuggestionParams{JournalEntryID: entryID, Kind: kind, Value: value}); err != nil {
		return fmt.Errorf("drop suggestion: %w", err)
	}
	s.changed(ctx, userID)
	return nil
}

// DismissSuggestion sets an open suggestion aside for good. ErrNotFound when
// the entry is not the user's.
func (s *Service) DismissSuggestion(ctx context.Context, userID, entryID int64, kind, value string) error {
	if _, err := s.ownEntry(ctx, userID, entryID); err != nil {
		return err
	}
	if err := s.Q.DismissSuggestion(ctx, models.DismissSuggestionParams{JournalEntryID: entryID, Kind: kind, Value: value}); err != nil {
		return fmt.Errorf("dismiss suggestion: %w", err)
	}
	s.changed(ctx, userID)
	return nil
}

// ownEntry loads the user's entry; ErrNotFound when it is not theirs.
func (s *Service) ownEntry(ctx context.Context, userID, entryID int64) (models.JournalEntry, error) {
	e, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID})
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, fmt.Errorf("load entry: %w", err)
	}
	return e, nil
}

// labelSet is the entry's labels as "kind:key".
func (s *Service) labelSet(ctx context.Context, entryID int64) (map[string]bool, error) {
	rows, err := s.Q.ListEntryLabels(ctx, entryID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Kind+":"+r.Label] = true
	}
	return have, nil
}
