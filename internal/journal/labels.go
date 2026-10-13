package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Review labels: a fixed vocabulary for what went wrong (mistakes) and how
// it felt (emotions), so analytics can group by them and the AI reviewer
// can classify consistently. Keys are stored; names are for display.

// Label kinds.
const (
	KindMistake = "mistake"
	KindEmotion = "emotion"
)

// Label is one vocabulary entry.
type Label struct {
	Key  string
	Name string
}

var labelVocabulary = map[string][]Label{
	KindMistake: {
		{"chased", "Chased the entry"},
		{"early-exit", "Exited too early"},
		{"late-exit", "Exited too late"},
		{"oversized", "Oversized"},
		{"no-stop", "No stop"},
		{"moved-stop", "Moved the stop"},
		{"held-loser", "Held a loser"},
		{"revenge", "Revenge trade"},
		{"fomo", "FOMO"},
		{"hesitated", "Hesitated"},
		{"overtraded", "Overtraded"},
		{"against-plan", "Against the plan"},
	},
	KindEmotion: {
		{"calm", "Calm"},
		{"focused", "Focused"},
		{"confident", "Confident"},
		{"anxious", "Anxious"},
		{"greedy", "Greedy"},
		{"frustrated", "Frustrated"},
		{"bored", "Bored"},
		{"tilted", "Tilted"},
		{"rushed", "Rushed"},
	},
}

// LabelKinds lists the kinds in display order.
var LabelKinds = []string{KindMistake, KindEmotion}

// LabelOptions returns the vocabulary of one kind (nil for an unknown kind).
func LabelOptions(kind string) []Label { return labelVocabulary[kind] }

// LabelKeys lists a kind's keys in vocabulary order (for tool descriptions).
func LabelKeys(kind string) []string {
	opts := LabelOptions(kind)
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Key)
	}
	return out
}

// LabelName returns the display name of a key, or the key itself.
func LabelName(kind, key string) string {
	for _, l := range labelVocabulary[kind] {
		if l.Key == key {
			return l.Name
		}
	}
	return key
}

// ValidLabel reports whether key belongs to kind's vocabulary.
func ValidLabel(kind, key string) bool { return LabelName(kind, key) != key || hasKey(kind, key) }

func hasKey(kind, key string) bool {
	for _, l := range labelVocabulary[kind] {
		if l.Key == key {
			return true
		}
	}
	return false
}

// Fields is what a page editor saves on an entry in one go.
type Fields struct {
	Body       string
	Mood       *int64 // day entries only; the other saves pass nil and keep the stored value
	Tag        *string
	StrategyID *int64
	Mistakes   []string // label keys (KindMistake)
	Emotions   []string // label keys (KindEmotion)
	Checks     []int64  // ticked checklist item ids of the entry's strategy (trade / group saves)
}

// normalizeLabels validates and dedupes keys for a kind; empty strings are
// dropped. Unknown keys are a ValidationError.
func normalizeLabels(kind string, keys []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" || seen[k] {
			continue
		}
		if !hasKey(kind, k) {
			return nil, BadInput("unknown %s label %q; one of: %s", kind, k, labelKeys(kind))
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func labelKeys(kind string) string {
	keys := make([]string, 0, len(labelVocabulary[kind]))
	for _, l := range labelVocabulary[kind] {
		keys = append(keys, l.Key)
	}
	return strings.Join(keys, ", ")
}

// SetLabels writes the labels of one kind on the user's entry. replace=true
// makes keys the whole set (the page editors); false adds to what is there
// (the API and MCP, which never remove what a trader set). ErrNotFound when
// the entry is not the caller's; a ValidationError for an unknown key.
func (s *Service) SetLabels(ctx context.Context, userID, entryID int64, kind string, keys []string, replace bool) error {
	if _, ok := labelVocabulary[kind]; !ok {
		return BadInput("unknown label kind %q", kind)
	}
	norm, err := normalizeLabels(kind, keys)
	if err != nil {
		return err
	}
	if _, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("load entry: %w", err)
	}
	src := SourceAPI // the API and MCP add; only the page editors replace
	if replace {
		src = SourceTrader
	}
	if err := s.writeLabels(ctx, entryID, kind, norm, replace, src); err != nil {
		return err
	}
	s.changed(ctx, userID)
	return nil
}

// Who set a value (journal_entry_labels.source, journal_entries.tag_source /
// strategy_source): the trader in a page editor (or by accepting an AI
// suggestion), or a client of the REST API or MCP - typically an AI tool -
// without the trader confirming it.
const (
	SourceTrader = "trader"
	SourceAPI    = "api"
)

// writeLabels is SetLabels without the ownership check, for the save paths
// that already hold the entry. Keys must be normalized. source says who set
// them; a label already on the entry keeps its row unless replace drops it.
func (s *Service) writeLabels(ctx context.Context, entryID int64, kind string, keys []string, replace bool, source string) error {
	if replace {
		if err := s.Q.DeleteEntryLabelsOfKind(ctx, models.DeleteEntryLabelsOfKindParams{JournalEntryID: entryID, Kind: kind}); err != nil {
			return fmt.Errorf("clear %s labels: %w", kind, err)
		}
	}
	for _, k := range keys {
		if err := s.Q.InsertEntryLabel(ctx, models.InsertEntryLabelParams{JournalEntryID: entryID, Kind: kind, Label: k, Source: source}); err != nil {
			return fmt.Errorf("label %s/%s: %w", kind, k, err)
		}
	}
	return nil
}

// applyFieldLabels validates both kinds up front (so a bad key fails before
// any write) and replaces the entry's labels with f's.
func (s *Service) applyFieldLabels(ctx context.Context, entryID int64, f Fields) error {
	m, err := normalizeLabels(KindMistake, f.Mistakes)
	if err != nil {
		return err
	}
	e, err := normalizeLabels(KindEmotion, f.Emotions)
	if err != nil {
		return err
	}
	if err := s.writeLabels(ctx, entryID, KindMistake, m, true, SourceTrader); err != nil {
		return err
	}
	return s.writeLabels(ctx, entryID, KindEmotion, e, true, SourceTrader)
}

// EntryLabelSources returns who set each of the user's entry's labels, keyed
// "kind:key" (empty when the entry is not theirs or has none).
func (s *Service) EntryLabelSources(ctx context.Context, userID, entryID int64) map[string]string {
	out := map[string]string{}
	if entryID == 0 {
		return out
	}
	if _, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID}); err != nil {
		return out
	}
	rows, err := s.Q.ListEntryLabels(ctx, entryID)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.Kind+":"+r.Label] = r.Source
	}
	return out
}

// EntryLabels returns the user's entry's labels by kind (empty map when the
// entry is not theirs or has none).
func (s *Service) EntryLabels(ctx context.Context, userID, entryID int64) (map[string][]string, error) {
	out := map[string][]string{}
	if entryID == 0 {
		return out, nil
	}
	if _, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("load entry: %w", err)
	}
	rows, err := s.Q.ListEntryLabels(ctx, entryID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	for _, r := range rows {
		out[r.Kind] = append(out[r.Kind], r.Label)
	}
	return out, nil
}
