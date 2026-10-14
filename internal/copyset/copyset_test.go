package copyset

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

func at(s int) time.Time { return time.Date(2026, 10, 9, 14, 0, s, 0, time.UTC) }

func TestCluster(t *testing.T) {
	trs := []Trade{
		{1, 1, "MES", "short", at(0), ""}, // set A: three accounts within 2s, micro + mini
		{2, 2, "MES", "short", at(1), ""},
		{3, 3, "ES", "short", at(2), ""},
		{4, 1, "MES", "short", at(5), ""}, // set B: 5s later is a new decision
		{5, 2, "MES", "short", at(6), ""},
		{6, 1, "MES", "long", at(5), ""}, // other direction: alone
		{7, 1, "NQ", "short", at(0), ""}, // other underlying, one account: alone
		{8, 1, "CL", "long", at(0), ""},  // same account twice is not a set
		{9, 1, "CL", "long", at(1), ""},
	}
	sets, solos := Cluster(trs)
	if len(sets) != 2 || len(solos) != 4 {
		t.Fatalf("sets=%v solos=%v", sets, solos)
	}
	ids := func(g []int) []int64 {
		var out []int64
		for _, k := range g {
			out = append(out, trs[k].ID)
		}
		return out
	}
	if a := ids(sets[0]); len(a) != 3 || a[0] != 3 && a[0] != 1 {
		t.Errorf("set A = %v", a)
	}
	if b := ids(sets[1]); len(b) != 2 || b[0] != 4 || b[1] != 5 {
		t.Errorf("set B = %v", b)
	}
	if Key("MES") != Key("ES") || Key("ES") != "ES=F" || Key("xyz") != "XYZ" {
		t.Errorf("keys: MES %q ES %q xyz %q", Key("MES"), Key("ES"), Key("xyz"))
	}
}

// Accounts in one declared copy group cluster within GroupWindow; the
// usual two seconds still rule between accounts outside a group or in
// different groups.
func TestCluster_CopyGroupWindow(t *testing.T) {
	trs := []Trade{
		{ID: 1, AccountID: 1, SymbolRoot: "ES", Direction: "long", EntryTime: at(0), CopyGroup: "main"},
		{ID: 2, AccountID: 2, SymbolRoot: "MES", Direction: "long", EntryTime: at(12), CopyGroup: "main"},  // IBKR leg, 12s late: same decision
		{ID: 3, AccountID: 3, SymbolRoot: "MES", Direction: "long", EntryTime: at(14)},                     // no group: 14s is a new decision
		{ID: 4, AccountID: 4, SymbolRoot: "MES", Direction: "long", EntryTime: at(13), CopyGroup: "other"}, // another group: not this decision
		{ID: 5, AccountID: 5, SymbolRoot: "ES", Direction: "long", EntryTime: at(45), CopyGroup: "main"},   // beyond GroupWindow: new
	}
	sets, solos := Cluster(trs)
	if len(sets) != 2 || len(solos) != 1 {
		t.Fatalf("sets=%v solos=%v", sets, solos)
	}
	if a := sets[0]; len(a) != 2 || trs[a[0]].ID != 1 || trs[a[1]].ID != 2 {
		t.Errorf("group set = %v", a)
	}
	// 3 and 4 open within 1s of each other: a plain two-second set.
	if b := sets[1]; len(b) != 2 || trs[b[0]].ID != 4 || trs[b[1]].ID != 3 {
		t.Errorf("plain set = %v", b)
	}
	if trs[solos[0]].ID != 5 {
		t.Errorf("solo = %v", solos)
	}
}

// Rebuild writes one set per cluster with its summary, is idempotent, and
// drops members with their trades.
func TestRebuild(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	q := models.New(conn)
	u := strconv.FormatInt(uid, 10)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'a', 'motivewave_csv', 'A'), (2, ` + u + `, 'b', 'motivewave_csv', 'B')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl) VALUES
			(10, 1, 'MESZ6', 'MES', 'short', 2, 7863.25, 7862.25, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:01:00 +0000 UTC', 'closed', 10, 2, 8),
			(11, 2, 'MESZ6', 'MES', 'short', 1, 7863.50, 7862.25, '2026-10-09 14:00:01 +0000 UTC', '2026-10-09 14:01:05 +0000 UTC', 'closed', 6.25, 1, 5.25),
			(12, 1, 'MESZ6', 'MES', 'long', 1, 7860, 7861, '2026-10-09 15:00:00 +0000 UTC', '2026-10-09 15:01:00 +0000 UTC', 'closed', 5, 1, 4)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	n, err := Rebuild(ctx, q, uid)
	if err != nil || n != 1 {
		t.Fatalf("Rebuild = %d, %v; want 1 set", n, err)
	}
	var sets []models.TradeSet
	rows, err := conn.Query(`SELECT id, symbol_root, direction, members, accounts, qty, entry_avg, entry_spread, net_pnl, exit_time FROM trade_sets WHERE user_id = ?`, uid)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s models.TradeSet
		if err := rows.Scan(&s.ID, &s.SymbolRoot, &s.Direction, &s.Members, &s.Accounts, &s.Qty, &s.EntryAvg, &s.EntrySpread, &s.NetPnl, &s.ExitTime); err != nil {
			t.Fatal(err)
		}
		sets = append(sets, s)
	}
	rows.Close()
	if len(sets) != 1 {
		t.Fatalf("sets = %+v", sets)
	}
	s := sets[0]
	if s.Members != 2 || s.Accounts != 2 || s.Qty != 3 || s.EntryAvg != 7863.375 || s.EntrySpread != 0.25 || s.NetPnl == nil || *s.NetPnl != 13.25 || s.ExitTime == nil || s.ExitTime.Second() != 5 {
		t.Errorf("set summary = %+v (net %v, exit %v)", s, s.NetPnl, s.ExitTime)
	}
	var members int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM trade_set_members WHERE set_id = ?`, s.ID).Scan(&members); err != nil || members != 2 {
		t.Errorf("members = %d, %v", members, err)
	}
	// Idempotent: a second pass leaves one set and two members.
	if n, err := Rebuild(ctx, q, uid); err != nil || n != 1 {
		t.Fatalf("second Rebuild = %d, %v", n, err)
	}
	var total, mem int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM trade_sets`).Scan(&total)
	_ = conn.QueryRow(`SELECT COUNT(*) FROM trade_set_members`).Scan(&mem)
	if total != 1 || mem != 2 {
		t.Errorf("after rerun: sets %d members %d", total, mem)
	}
	// Deleting a member trade drops its membership (cascade).
	if _, err := conn.Exec(`DELETE FROM trades WHERE id = 11`); err != nil {
		t.Fatal(err)
	}
	_ = conn.QueryRow(`SELECT COUNT(*) FROM trade_set_members`).Scan(&mem)
	if mem != 1 {
		t.Errorf("after trade delete: members %d, want 1", mem)
	}
	if n, err := RebuildForAccount(ctx, q, 1); err != nil || n != 0 {
		t.Errorf("RebuildForAccount after losing an account = %d, %v; want 0 sets", n, err)
	}
}
