package journal

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

func suggestFixture(t *testing.T) (*Service, int64) {
	t.Helper()
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := itoa(uid)
	for _, stmt := range []string{
		`INSERT INTO users (id, username) VALUES (2, 'other')`,
		`INSERT INTO strategies (id, user_id, name, body) VALUES (5, ` + u + `, 'ORB', 'opening range')`,
		`INSERT INTO strategies (id, user_id, name, body, archived_at) VALUES (6, ` + u + `, 'Old', 'x', CURRENT_TIMESTAMP)`,
		`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (1, ` + u + `, 'n', '2026-10-09 00:00:00+00:00', 'group')`,
		`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (2, ` + u + `, 'n', '2026-10-09 00:00:00+00:00', 'day')`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label) VALUES (1, 'mistake', 'fomo')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{DB: conn, Q: models.New(conn), TZ: func() *time.Location { return time.UTC }}, uid
}

// A review proposes; nothing is written until the trader accepts.
func TestSuggestAcceptDismiss(t *testing.T) {
	s, uid := suggestFixture(t)
	ctx := context.Background()

	open, err := s.Suggest(ctx, uid, 1, "bad", 5, []string{"chased", "fomo", "made-up"}, []string{"rushed"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Suggestion{{SuggestTag, "bad"}, {SuggestStrategy, "5"}, {KindMistake, "chased"}, {KindEmotion, "rushed"}}
	if !reflect.DeepEqual(open, want) {
		t.Fatalf("open = %v, want %v (fomo is set already, made-up unknown)", open, want)
	}
	e, _ := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: 1, UserID: &uid})
	if e.Tag != nil || e.StrategyID != nil {
		t.Fatal("a suggestion wrote the field")
	}
	if l, _ := s.EntryLabels(ctx, uid, 1); len(l[KindMistake]) != 1 {
		t.Fatalf("a suggestion wrote a label: %v", l)
	}

	// Accept the tag and a label: they become the trader's.
	if err := s.AcceptSuggestion(ctx, uid, 1, SuggestTag, "bad"); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptSuggestion(ctx, uid, 1, KindMistake, "chased"); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: 1, UserID: &uid})
	if e.Tag == nil || *e.Tag != "bad" || e.TagSource == nil || *e.TagSource != SourceTrader {
		t.Errorf("accepted tag = %v / %v", e.Tag, e.TagSource)
	}
	if src := s.EntryLabelSources(ctx, uid, 1); src["mistake:chased"] != SourceTrader {
		t.Errorf("accepted label source = %v", src)
	}
	// Dismiss the strategy: gone now and not proposed again.
	if err := s.DismissSuggestion(ctx, uid, 1, SuggestStrategy, "5"); err != nil {
		t.Fatal(err)
	}
	left, _ := s.Suggestions(ctx, uid, 1)
	if !reflect.DeepEqual(left, []Suggestion{{KindEmotion, "rushed"}}) {
		t.Errorf("left = %v", left)
	}
	again, _ := s.Suggest(ctx, uid, 1, "good", 5, nil, []string{"rushed"})
	if !reflect.DeepEqual(again, []Suggestion{{KindEmotion, "rushed"}}) {
		t.Errorf("re-review proposed %v (tag is set, strategy dismissed)", again)
	}
	// The trader sets the emotion in the editor: the suggestion stops showing.
	if err := s.writeLabels(ctx, 1, KindEmotion, []string{"rushed"}, false, SourceTrader); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.Suggestions(ctx, uid, 1); len(left) != 0 {
		t.Errorf("stale suggestion still listed: %v", left)
	}

	// No strategy on a day entry, no archived strategy, nothing for others.
	if open, _ := s.Suggest(ctx, uid, 2, "", 5, nil, nil); len(open) != 0 {
		t.Errorf("day entry got %v", open)
	}
	if open, _ := s.Suggest(ctx, uid, 1, "", 6, nil, nil); len(open) != 0 {
		t.Errorf("archived strategy proposed: %v", open)
	}
	if _, err := s.Suggest(ctx, 2, 1, "good", 0, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user: %v", err)
	}
	if err := s.AcceptSuggestion(ctx, 2, 1, KindEmotion, "rushed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user accepted: %v", err)
	}
	// Accepting during a review is refused.
	s.Suggest(ctx, uid, 2, "good", 0, nil, nil)
	s.Locked = func(id int64) bool { return id == 2 }
	if err := s.AcceptSuggestion(ctx, uid, 2, SuggestTag, "good"); !errors.Is(err, ErrReviewPending) {
		t.Errorf("accept under review: %v", err)
	}
}

// Who set what: page saves are the trader's, the API's writes are 'api'.
func TestSourcesRecordWhoSetWhat(t *testing.T) {
	s, uid := suggestFixture(t)
	ctx := context.Background()
	if ok, err := s.AssignTagIfUnset(ctx, uid, 1, "good"); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := s.AssignStrategyIfUnset(ctx, uid, 1, 5); err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.SetLabels(ctx, uid, 1, KindEmotion, []string{"calm"}, false); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: 1, UserID: &uid})
	if e.TagSource == nil || *e.TagSource != SourceAPI || e.StrategySource == nil || *e.StrategySource != SourceAPI {
		t.Errorf("API writes not marked: %v %v", e.TagSource, e.StrategySource)
	}
	if src := s.EntryLabelSources(ctx, uid, 1); src["emotion:calm"] != SourceAPI || src["mistake:fomo"] != SourceTrader {
		t.Errorf("label sources = %v", src)
	}
	// Saving the note in the editor confirms what it holds.
	good := "good"
	sid := int64(5)
	if _, err := s.SaveGroup(ctx, uid, 1, Fields{Body: "n2", Tag: &good, StrategyID: &sid, Emotions: []string{"calm"}}); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: 1, UserID: &uid})
	if *e.TagSource != SourceTrader || *e.StrategySource != SourceTrader {
		t.Errorf("page save did not confirm: %v %v", *e.TagSource, *e.StrategySource)
	}
	if src := s.EntryLabelSources(ctx, uid, 1); src["emotion:calm"] != SourceTrader {
		t.Errorf("page save did not confirm the label: %v", src)
	}
	// An API create marks what it set.
	tag := "bad"
	ne, _, err := s.Create(ctx, uid, CreateInput{Scope: "day", Date: "2026-10-12", Body: "x", Tag: &tag, Mistakes: []string{"chased"}})
	if err != nil {
		t.Fatal(err)
	}
	ne, _ = s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: ne.ID, UserID: &uid})
	if ne.TagSource == nil || *ne.TagSource != SourceAPI || s.EntryLabelSources(ctx, uid, ne.ID)["mistake:chased"] != SourceAPI {
		t.Errorf("API create not marked: %v", ne.TagSource)
	}
}
