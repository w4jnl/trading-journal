package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// While an AI review writes an entry, the page saves and deletes of that
// entry are refused; other entries, and appends, carry on.
func TestLockedEntryRefusesPageEdits(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := itoa(uid)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status)
			VALUES (10, 1, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 15:00:00 +0000 UTC', 'closed')`,
		`INSERT INTO journal_entries (id, user_id, trade_id, body, entry_date, scope) VALUES (3, ` + u + `, 10, 'trade', '2026-10-09 00:00:00+00:00', 'trade')`,
		`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (4, ` + u + `, 'group', '2026-10-09 00:00:00+00:00', 'group')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	locked := map[int64]bool{}
	s := &Service{DB: conn, Q: models.New(conn), TZ: func() *time.Location { return time.UTC },
		Locked: func(id int64) bool { return locked[id] }}
	ctx := context.Background()
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	// The day and week notes come from the page saves, so the lookups find them.
	if _, err := s.SaveDay(ctx, uid, day, Fields{Body: "day"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveWeek(ctx, uid, monday, Fields{Body: "week"}); err != nil {
		t.Fatal(err)
	}
	dayE, _ := s.Q.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: &uid, EntryDate: day})
	weekE, _ := s.Q.GetWeekJournalEntry(ctx, models.GetWeekJournalEntryParams{UserID: &uid, EntryDate: monday})
	for _, id := range []int64{dayE.ID, weekE.ID, 3, 4} {
		locked[id] = true
	}

	for name, call := range map[string]func() error{
		"save day":     func() error { _, err := s.SaveDay(ctx, uid, day, Fields{Body: "x"}); return err },
		"empty day":    func() error { _, err := s.SaveDay(ctx, uid, day, Fields{}); return err },
		"delete day":   func() error { _, err := s.DeleteDay(ctx, uid, day); return err },
		"save week":    func() error { _, err := s.SaveWeek(ctx, uid, monday, Fields{Body: "x"}); return err },
		"delete week":  func() error { _, err := s.DeleteWeek(ctx, uid, monday); return err },
		"save trade":   func() error { _, err := s.SaveTrade(ctx, uid, 10, Fields{Body: "x"}); return err },
		"save group":   func() error { _, err := s.SaveGroup(ctx, uid, 4, Fields{Body: "x"}); return err },
		"delete group": func() error { return s.Delete(ctx, uid, 4) },
	} {
		if err := call(); !errors.Is(err, ErrReviewPending) {
			t.Errorf("%s: err = %v, want ErrReviewPending", name, err)
		}
	}
	// The review's own append still goes through.
	e, _ := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: 3, UserID: &uid})
	if err := s.AppendNote(ctx, uid, e, "review"); err != nil {
		t.Errorf("append to a locked entry: %v", err)
	}
	// Unlocked, the same save works.
	delete(locked, dayE.ID)
	if out, err := s.SaveDay(ctx, uid, day, Fields{Body: "edited"}); err != nil || out != OutcomeSaved {
		t.Errorf("unlocked save = %q, %v", out, err)
	}
}
