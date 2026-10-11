package journal

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

func TestFormatAppendedNote(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 6, 6, 14, 32, 0, 0, time.UTC) // 15:32 CET
	got := FormatAppendedNote(now, loc, "did X")
	want := "\n\n---\n\n_Note added 2026-06-06 15:32 CET_\n\ndid X\n"
	if got != want {
		t.Errorf("FormatAppendedNote =\n%q\nwant\n%q", got, want)
	}
	// Trailing whitespace on the note is trimmed; the block always ends in \n.
	if !strings.HasSuffix(FormatAppendedNote(now, loc, "hi\n\n  "), "hi\n") {
		t.Error("trailing whitespace not trimmed to single newline")
	}
}

func TestParseTag(t *testing.T) {
	if got, ok := ParseTag(nil); !ok || got != nil {
		t.Errorf("nil tag = (%v, %v), want (nil, true)", got, ok)
	}
	empty := "  "
	if got, ok := ParseTag(&empty); !ok || got != nil {
		t.Errorf("blank tag = (%v, %v), want (nil, true)", got, ok)
	}
	for _, v := range []string{"good", "neutral", "bad"} {
		if got, ok := ParseTag(&v); !ok || got == nil || *got != v {
			t.Errorf("tag %q = (%v, %v), want valid", v, got, ok)
		}
	}
	bad := "great"
	if _, ok := ParseTag(&bad); ok {
		t.Error("tag \"great\" accepted, want rejected")
	}
}

func TestParseMood(t *testing.T) {
	if got, ok := ParseMood(nil); !ok || got != nil {
		t.Errorf("nil mood = (%v, %v), want (nil, true)", got, ok)
	}
	for _, v := range []int64{1, 3, 5} {
		if got, ok := ParseMood(&v); !ok || got == nil || *got != v {
			t.Errorf("mood %d rejected, want accepted", v)
		}
	}
	for _, v := range []int64{0, 6, -1} {
		if _, ok := ParseMood(&v); ok {
			t.Errorf("mood %d accepted, want rejected", v)
		}
	}
}

func TestClusterCopytrader(t *testing.T) {
	base := time.Date(2026, 5, 12, 17, 25, 43, 0, time.UTC)
	mk := func(id int64, root, dir string, acct int64, at time.Time) PrepTrade {
		return PrepTrade{ID: id, SymbolRoot: root, Direction: dir, AccountID: acct, EntryTime: at}
	}
	trs := []PrepTrade{
		// copytrader fan-out: MES short across 3 accounts within ~1s -> one group
		mk(1, "MES", "short", 10, base),
		mk(2, "MES", "short", 11, base.Add(500*time.Millisecond)),
		mk(3, "MES", "short", 12, base.Add(time.Second)),
		// solo on another symbol
		mk(4, "NQ", "long", 10, base.Add(time.Hour)),
		// same symbol+dir but 5s apart on 2 accounts -> NOT a cluster (two solos)
		mk(5, "MES", "short", 10, base.Add(10*time.Minute)),
		mk(6, "MES", "short", 11, base.Add(10*time.Minute+5*time.Second)),
		// two trades within the window but on ONE account -> not a group (solos)
		mk(7, "CL", "long", 20, base),
		mk(8, "CL", "long", 20, base.Add(time.Second)),
	}
	groups, solos := ClusterCopytrader(trs)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if len(groups[0]) != 3 || distinctAccounts(groups[0]) != 3 {
		t.Errorf("group size/accts = %d/%d, want 3/3 (ids %v)", len(groups[0]), distinctAccounts(groups[0]), tradeIDsOf(groups[0]))
	}
	if len(solos) != 5 { // 4 (NQ), 5, 6 (far-apart MES), 7, 8 (same-account CL)
		t.Errorf("solos = %d, want 5 (ids %v)", len(solos), tradeIDsOf(solos))
	}
}

func TestClusterCopytraderCrossInstrument(t *testing.T) {
	base := time.Date(2026, 5, 12, 17, 25, 43, 0, time.UTC)
	mk := func(id int64, root string, acct int64, at time.Time) PrepTrade {
		return PrepTrade{ID: id, SymbolRoot: root, Direction: "short", AccountID: acct, EntryTime: at}
	}
	// Master trades ES; followers get the micro MES at the same instant -> one
	// copytrader decision (ES and MES share the ES=F underlying).
	groups, solos := ClusterCopytrader([]PrepTrade{
		mk(1, "ES", 10, base),
		mk(2, "MES", 11, base.Add(200*time.Millisecond)),
		mk(3, "MES", 12, base.Add(400*time.Millisecond)),
	})
	if len(groups) != 1 || len(solos) != 0 {
		t.Fatalf("ES+MES: got %d groups / %d solos, want 1/0", len(groups), len(solos))
	}
	if n := distinctAccounts(groups[0]); n != 3 {
		t.Errorf("group accounts = %d, want 3", n)
	}
	if lbl := clusterLabel(groups[0]); !strings.Contains(lbl, "ES") || !strings.Contains(lbl, "MES") {
		t.Errorf("label %q should name both roots", lbl)
	}
	// Different underlyings at the same instant must NOT merge.
	g2, s2 := ClusterCopytrader([]PrepTrade{mk(1, "ES", 10, base), mk(2, "NQ", 11, base)})
	if len(g2) != 0 || len(s2) != 2 {
		t.Errorf("ES+NQ same instant: got %d groups / %d solos, want 0/2", len(g2), len(s2))
	}
}

// The page saves: outcomes by (existing entry, body), ownership on links and
// group creation, and the Changed callback after every committed write.
func TestServiceSavesAndLinks(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO users (id, username) VALUES (2, 'other')`,
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES
			(1, ` + itoa(uid) + `, 'mine', 'motivewave_csv', 'A-1'), (2, 2, 'theirs', 'motivewave_csv', 'A-2')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status)
			VALUES (10, 1, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 15:00:00 +0000 UTC', 'closed'),
			       (11, 1, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-09 14:00:01 +0000 UTC', '2026-10-09 15:00:00 +0000 UTC', 'closed'),
			       (20, 2, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 15:00:00 +0000 UTC', 'closed')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	var bumps int
	s := &Service{DB: conn, Q: models.New(conn), TZ: func() *time.Location { return time.UTC },
		Changed: func(context.Context, int64) { bumps++ }}
	ctx := context.Background()
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

	if out, err := s.SaveDay(ctx, uid, day, Fields{}); err != nil || out != OutcomeEmpty || bumps != 0 {
		t.Fatalf("empty save on no entry: out=%q err=%v bumps=%d", out, err, bumps)
	}
	if out, err := s.SaveDay(ctx, uid, day, Fields{Body: "note", Mistakes: []string{"chased"}, Emotions: []string{"anxious", "ANXIOUS"}}); err != nil || out != OutcomeSaved || bumps != 1 {
		t.Fatalf("create: out=%q err=%v bumps=%d", out, err, bumps)
	}
	if out, err := s.SaveDay(ctx, uid, day, Fields{Body: "note 2", Emotions: []string{"calm"}}); err != nil || out != OutcomeSaved || bumps != 2 {
		t.Fatalf("update: out=%q err=%v bumps=%d", out, err, bumps)
	}
	// The update replaced the label sets: mistakes cleared, emotion swapped.
	dayEntry, _ := s.Q.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: &uid, EntryDate: day})
	if labels, err := s.EntryLabels(ctx, uid, dayEntry.ID); err != nil || len(labels[KindMistake]) != 0 || len(labels[KindEmotion]) != 1 || labels[KindEmotion][0] != "calm" {
		t.Fatalf("labels after update = %v (%v), want only emotion calm", labels, err)
	}
	if _, err := s.SaveDay(ctx, uid, day, Fields{Body: "x", Mistakes: []string{"made-up"}}); err == nil {
		t.Fatal("unknown label accepted")
	}
	if err := s.SetLabels(ctx, uid, dayEntry.ID, KindMistake, []string{"fomo"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLabels(ctx, uid, dayEntry.ID, KindMistake, []string{"revenge"}, false); err != nil {
		t.Fatal(err)
	}
	if labels, _ := s.EntryLabels(ctx, uid, dayEntry.ID); len(labels[KindMistake]) != 2 {
		t.Fatalf("additive SetLabels = %v, want fomo + revenge", labels[KindMistake])
	}
	if err := s.SetLabels(ctx, 2, dayEntry.ID, KindMistake, []string{"fomo"}, true); err != ErrNotFound {
		t.Fatalf("another user's labels: err=%v, want ErrNotFound", err)
	}
	if out, err := s.SaveDay(ctx, uid, day, Fields{}); err != nil || out != OutcomeDeleted || bumps != 5 {
		t.Fatalf("empty save deletes: out=%q err=%v bumps=%d", out, err, bumps)
	}
	if ok, err := s.DeleteDay(ctx, uid, day); err != nil || ok {
		t.Fatalf("delete of a missing entry: ok=%v err=%v", ok, err)
	}

	// Group creation keeps only the caller's trades; a foreign-only set is refused.
	if _, err := s.CreateGroup(ctx, uid, []int64{20}); err != ErrNoOwnedTrades {
		t.Fatalf("group over a foreign trade: err=%v, want ErrNoOwnedTrades", err)
	}
	g, err := s.CreateGroup(ctx, uid, []int64{10, 20})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM journal_entry_trades WHERE journal_entry_id = ?`, g.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("group links = %d, want 1 (only the owned trade)", n)
	}
	if linked, err := s.LinkTrades(ctx, uid, g.ID, []int64{11, 20, 11}); err != nil || linked != 1 {
		t.Fatalf("link: linked=%d err=%v, want 1", linked, err)
	}
	if _, err := s.LinkTrades(ctx, 2, g.ID, []int64{20}); err != ErrNotFound {
		t.Fatalf("another user linking into my group: err=%v, want ErrNotFound", err)
	}
	if ok, err := s.UnlinkTrade(ctx, 2, g.ID, 10); err != nil || ok {
		t.Fatalf("another user unlinking: ok=%v err=%v", ok, err)
	}
	if ok, err := s.UnlinkTrade(ctx, uid, g.ID, 10); err != nil || !ok {
		t.Fatalf("unlink: ok=%v err=%v", ok, err)
	}
	if _, err := s.SaveTrade(ctx, uid, 20, Fields{Body: "x"}); err != ErrNotFound {
		t.Fatalf("trade entry on a foreign trade: err=%v, want ErrNotFound", err)
	}
	if out, err := s.SaveTrade(ctx, uid, 10, Fields{Body: "trade note"}); err != nil || out != OutcomeSaved {
		t.Fatalf("trade save: out=%q err=%v", out, err)
	}
	// Append seeds a placeholder, appends otherwise; both bump.
	e, _, err := s.Create(ctx, uid, CreateInput{Scope: "week", Week: "2026-W41", Body: PlaceholderBody})
	if err != nil {
		t.Fatal(err)
	}
	before := bumps
	if err := s.AppendNote(ctx, uid, e, "review"); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := conn.QueryRow(`SELECT body FROM journal_entries WHERE id = ?`, e.ID).Scan(&body); err != nil || strings.Contains(body, "_AI review_") || !strings.HasPrefix(body, "---") {
		t.Fatalf("seeded body = %q", body)
	}
	if bumps != before+1 {
		t.Fatalf("append did not bump")
	}
	if err := s.AppendNote(ctx, 2, e, "theirs"); err != ErrNotFound {
		t.Fatalf("foreign append on a placeholder: err=%v, want ErrNotFound", err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// Accounts hidden in Settings stay out of prepare-day: neither their trades
// nor the days only they traded on surface.
func TestHiddenAccountsStayOutOfPrepareDay(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref, excluded_from_stats) VALUES
			(1, ` + itoa(uid) + `, 'visible', 'motivewave_csv', 'A-1', 0), (2, ` + itoa(uid) + `, 'hidden', 'motivewave_csv', 'A-2', 1)`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-08 14:00:00 +0000 UTC', '2026-10-08 15:00:00 +0000 UTC', 'closed', 50),
			(20, 2, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-08 16:00:00 +0000 UTC', '2026-10-08 17:00:00 +0000 UTC', 'closed', 500),
			(21, 2, 'ESU6', 'ES', 'short', 1, 100, 99, '2026-10-09 16:00:00 +0000 UTC', '2026-10-09 17:00:00 +0000 UTC', 'closed', 500)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{DB: conn, Q: models.New(conn), TZ: func() *time.Location { return time.UTC }}
	ctx := context.Background()
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	trades, err := s.LoadDayTrades(ctx, uid, day)
	if err != nil || len(trades) != 1 || trades[0].ID != 10 {
		t.Errorf("LoadDayTrades = %+v (%v), want only trade 10", trades, err)
	}
	if last, err := s.LastTradingDay(ctx, uid); err != nil || !last.Equal(day) {
		t.Errorf("LastTradingDay = %v (%v), want %v (the hidden account's 9th does not count)", last, err, day)
	}
	res, err := s.PrepareDay(ctx, uid, "2026-10-08")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].TradeIDs[0] != 10 {
		t.Errorf("PrepareDay entries = %+v, want one for trade 10", res.Entries)
	}
}
