package journal

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Editing a checklist keeps the ids of unchanged rules (so ticks survive),
// a save stores only ticks that belong to the entry's strategy, and the
// entry's checklist reports the score and the misses.
func TestChecklistItemsAndTicks(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	q := models.New(conn)
	ctx := context.Background()
	s := &Service{DB: conn, Q: q, TZ: func() *time.Location { return time.UTC }}
	st, err := q.CreateStrategy(ctx, models.CreateStrategyParams{UserID: uid, Name: "ORB", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := q.CreateStrategy(ctx, models.CreateStrategyParams{UserID: uid, Name: "VWAP", Body: "y"})
	if err := s.SetChecklistItems(ctx, uid, st.ID, ParseChecklistText("- Entry at the level\n2. Stop before entry\n\n Size from the stop ")); err != nil {
		t.Fatal(err)
	}
	items, _ := q.ListChecklistItems(ctx, st.ID)
	if len(items) != 3 || items[0].Text != "Entry at the level" || items[2].Text != "Size from the stop" {
		t.Fatalf("items = %+v", items)
	}
	_ = s.SetChecklistItems(ctx, uid, other.ID, []string{"Above VWAP"})
	otherItems, _ := q.ListChecklistItems(ctx, other.ID)

	// A trade entry with the strategy and two ticks (one foreign id ignored).
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ?, 'a', 'motivewave_csv', 'A')`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES (10, 1, 'ESZ6', 'ES', 'long', 1, 100, 101, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:05:00 +0000 UTC', 'closed', 50)`); err != nil {
		t.Fatal(err)
	}
	sid := st.ID
	if _, err := s.SaveTrade(ctx, uid, 10, Fields{Body: "followed most of it", StrategyID: &sid, Checks: []int64{items[0].ID, items[2].ID, otherItems[0].ID, 999}}); err != nil {
		t.Fatal(err)
	}
	e, _ := q.GetJournalEntryByTradeID(ctx, ptr(int64(10)))
	cl, err := s.EntryChecklist(ctx, uid, e.ID)
	if err != nil || cl == nil || cl.Done != 2 || cl.Total != 3 || cl.Complete() || strings.Join(cl.Missed(), ",") != "Stop before entry" {
		t.Fatalf("checklist = %+v err=%v", cl, err)
	}
	// Editing the list: a reworded rule gets a new id, the others keep theirs and their ticks.
	if err := s.SetChecklistItems(ctx, uid, st.ID, []string{"Size from the stop", "Entry at the level", "Stop BEFORE the entry"}); err != nil {
		t.Fatal(err)
	}
	cl, _ = s.EntryChecklist(ctx, uid, e.ID)
	if cl.Done != 2 || cl.Total != 3 || cl.Items[0].Text != "Size from the stop" || !cl.Items[0].Checked || cl.Items[2].Checked {
		t.Errorf("after edit: %+v", cl)
	}
	// Switching the strategy drops ticks that do not belong to it.
	oid := other.ID
	if _, err := s.SaveTrade(ctx, uid, 10, Fields{Body: "switched", StrategyID: &oid, Checks: []int64{items[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if cl, _ = s.EntryChecklist(ctx, uid, e.ID); cl == nil || cl.Done != 0 || cl.Total != 1 {
		t.Errorf("after switching strategy: %+v", cl)
	}
	// A foreign user cannot edit the checklist.
	if err := s.SetChecklistItems(ctx, uid+1, st.ID, []string{"x"}); err != ErrNotFound {
		t.Errorf("foreign edit = %v, want ErrNotFound", err)
	}
}

func ptr[T any](v T) *T { return &v }
