package importer

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The purge keeps exactly what the dashboard, settings and audit trail need
// and removes the rest, looping over batches until nothing is left.
func TestSyncLogRetentionPurge(t *testing.T) {
	conn := dbtest.Open(t, "bootstrap")
	q := models.New(conn)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx, models.CreateUserParams{Username: "alice"})
	acct := func(name string) int64 {
		a, err := q.CreateAccount(ctx, models.CreateAccountParams{UserID: u.ID, Name: name, ImportSource: "topstepx_csv", AccountRef: name, Currency: "USD", CommissionSource: "rate"})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	a1, a2 := acct("A-1"), acct("A-2")
	row := func(acctID int64, typ, status string, execs int64, ageDays int) int64 {
		var acctPtr *int64
		if acctID != 0 {
			acctPtr = &acctID
		}
		r, err := q.CreateSyncLog(ctx, models.CreateSyncLogParams{UserID: &u.ID, AccountID: acctPtr, SyncType: typ, Status: status, ExecsAdded: execs})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sync_log SET synced_at = datetime('now', ?) WHERE id = ?", fmt.Sprintf("-%d days", ageDays), r.ID); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	// Age rows are named by their expected fate.
	goneNoop1 := row(a1, "topstepx_api", "success", 0, 10)
	goneNoop2 := row(a1, "topstepx_api", "success", 0, 5)
	goneNoop3 := row(a1, "topstepx_api", "success", 0, 3)
	keptAdds := row(a1, "topstepx_api", "success", 2, 30)     // added fills: kept 90d
	keptErr := row(a1, "topstepx_api", "error", 0, 30)        // errored: kept 90d
	goneOldAdds := row(a1, "topstepx_api", "success", 5, 200) // beyond the 90d event cutoff
	keptRecent := row(a1, "topstepx_api", "success", 0, 1)    // inside the 2d window (and newest for a1 + type)
	keptNewestA2 := row(a2, "topstepx_api", "success", 0, 40) // old no-op, but the newest row for account A-2
	keptCSV := row(0, "motivewave_csv", "success", 0, 400)    // CSV imports are never purged
	goneIBKR := row(a1, "ibkr_api", "success", 0, 41)
	keptIBKR := row(a1, "ibkr_api", "success", 0, 40) // old no-op, but the newest ibkr row (highest id)

	r := &SyncLogRetention{Queries: q, NoOpDays: 2, Batch: 2} // tiny batch to exercise the loop
	n, err := r.PurgeOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("purged %d rows, want 5", n)
	}
	exists := func(id int64) bool {
		var x int64
		err := conn.QueryRowContext(ctx, "SELECT id FROM sync_log WHERE id = ?", id).Scan(&x)
		return err != sql.ErrNoRows
	}
	for name, id := range map[string]int64{"noop1": goneNoop1, "noop2": goneNoop2, "noop3": goneNoop3, "oldAdds": goneOldAdds, "ibkrOld": goneIBKR} {
		if exists(id) {
			t.Errorf("%s (id %d) should have been purged", name, id)
		}
	}
	for name, id := range map[string]int64{"adds": keptAdds, "err": keptErr, "recent": keptRecent, "newestA2": keptNewestA2, "csv": keptCSV, "ibkrNewest": keptIBKR} {
		if !exists(id) {
			t.Errorf("%s (id %d) must be kept", name, id)
		}
	}
	// Idempotent: a second pass finds nothing.
	if n, _ := r.PurgeOnce(ctx); n != 0 {
		t.Errorf("second pass purged %d", n)
	}
	// Disabled: nothing happens even with purgeable rows present.
	row(a1, "topstepx_api", "success", 0, 30)
	if n, err := (&SyncLogRetention{Queries: q}).PurgeOnce(ctx); n != 0 || err != nil {
		t.Errorf("disabled purge = %d, %v", n, err)
	}
	var nilR *SyncLogRetention
	if n, err := nilR.PurgeOnce(ctx); n != 0 || err != nil {
		t.Errorf("nil receiver = %d, %v", n, err)
	}
}
