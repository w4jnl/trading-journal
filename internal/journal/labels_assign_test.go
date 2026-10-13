package journal

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The AI fills an empty label kind on a trade or group entry and nothing
// else: a kind the trader set, a day entry, or another user's entry stay
// untouched; unknown keys are dropped.
func TestAssignLabelsIfUnset(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO users (id, username) VALUES (2, 'other')`,
		`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (1, ` + itoa(uid) + `, 'n', '2026-10-09 00:00:00+00:00', 'group')`,
		`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (2, ` + itoa(uid) + `, 'n', '2026-10-09 00:00:00+00:00', 'day')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	var bumps int
	s := &Service{DB: conn, Q: models.New(conn), TZ: func() *time.Location { return time.UTC },
		Changed: func(context.Context, int64) { bumps++ }}
	ctx := context.Background()

	got, err := s.AssignLabelsIfUnset(ctx, uid, 1, KindMistake, []string{"late-exit", "made-up", "Chased", "chased"})
	if err != nil || !reflect.DeepEqual(got, []string{"chased", "late-exit"}) || bumps != 1 {
		t.Fatalf("first fill = %v, %v (bumps %d)", got, err, bumps)
	}
	// The kind is now set: a second suggestion is ignored.
	if got, err := s.AssignLabelsIfUnset(ctx, uid, 1, KindMistake, []string{"fomo"}); err != nil || got != nil {
		t.Errorf("set kind refilled: %v, %v", got, err)
	}
	// The other kind is still empty and fills independently.
	if got, _ := s.AssignLabelsIfUnset(ctx, uid, 1, KindEmotion, []string{"rushed"}); !reflect.DeepEqual(got, []string{"rushed"}) {
		t.Errorf("emotions = %v", got)
	}
	// Nothing valid, a day entry, another user: no writes.
	if got, _ := s.AssignLabelsIfUnset(ctx, uid, 2, KindMistake, []string{"fomo"}); got != nil {
		t.Errorf("day entry labelled: %v", got)
	}
	if _, err := s.AssignLabelsIfUnset(ctx, 2, 1, KindEmotion, []string{"calm"}); err != ErrNotFound {
		t.Errorf("other user: err = %v", err)
	}
	labels, _ := s.EntryLabels(ctx, uid, 1)
	if !reflect.DeepEqual(labels[KindMistake], []string{"chased", "late-exit"}) || !reflect.DeepEqual(labels[KindEmotion], []string{"rushed"}) {
		t.Errorf("labels = %v", labels)
	}
	if bumps != 2 {
		t.Errorf("bumps = %d, want 2 (one per write)", bumps)
	}
}
