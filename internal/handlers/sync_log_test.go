package handlers

import (
	"context"
	"testing"

	"github.com/w4jnl/trading-journal/internal/models"
)

// The "newest per type" and "newest per account" lookups (rewritten from
// quadratic correlated subqueries to grouped MAX(id)) must return exactly
// the latest row per group for the asking user, and never another user's.
func TestLastSyncLookupsAreNewestPerGroupAndUserScoped(t *testing.T) {
	_, q := newAuthzTestDB(t)
	ctx := context.Background()
	alice, _ := q.CreateUser(ctx, models.CreateUserParams{Username: "alice"})
	bob, _ := q.CreateUser(ctx, models.CreateUserParams{Username: "bob"})
	acct := func(uid int64, name string) int64 {
		a, err := q.CreateAccount(ctx, models.CreateAccountParams{UserID: uid, Name: name, ImportSource: "topstepx_csv", AccountRef: name, Currency: "USD", CommissionSource: "rate"})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	a1, a2, b1 := acct(alice.ID, "A-1"), acct(alice.ID, "A-2"), acct(bob.ID, "B-1")
	log := func(uid, acctID int64, typ, status string, execs int64) int64 {
		row, err := q.CreateSyncLog(ctx, models.CreateSyncLogParams{UserID: &uid, AccountID: &acctID, SyncType: typ, Status: status, ExecsAdded: execs})
		if err != nil {
			t.Fatal(err)
		}
		return row.ID
	}
	log(alice.ID, a1, "topstepx_api", "success", 0)
	aliceTSXa1 := log(alice.ID, a1, "topstepx_api", "success", 2)
	aliceTSXa2 := log(alice.ID, a2, "topstepx_api", "error", 0)
	log(alice.ID, a1, "ibkr_api", "success", 0)
	aliceIBKR := log(alice.ID, a1, "ibkr_api", "success", 1)
	log(bob.ID, b1, "topstepx_api", "success", 9) // newest topstepx row overall, but Bob's

	byType, err := q.ListLastSyncByType(ctx, &alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range byType {
		got[r.SyncType] = r.ID
	}
	if len(got) != 2 || got["topstepx_api"] != aliceTSXa2 || got["ibkr_api"] != aliceIBKR {
		t.Errorf("ListLastSyncByType = %v, want topstepx=%d ibkr=%d", got, aliceTSXa2, aliceIBKR)
	}

	perAcct, err := q.ListLastAPISyncPerAccount(ctx, models.ListLastAPISyncPerAccountParams{UserID: &alice.ID, SyncType: "topstepx_api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(perAcct) != 2 {
		t.Fatalf("per-account rows = %+v", perAcct)
	}
	seen := map[int64]int64{}
	for _, r := range perAcct {
		seen[*r.AccountID] = r.ExecsAdded
	}
	if seen[a1] != 2 || seen[a2] != 0 {
		t.Errorf("per-account newest = %v (want a1 -> execs 2 i.e. row %d, a2 -> error row %d)", seen, aliceTSXa1, aliceTSXa2)
	}
	if rows, _ := q.ListLastAPISyncPerAccount(ctx, models.ListLastAPISyncPerAccountParams{UserID: &bob.ID, SyncType: "ibkr_api"}); len(rows) != 0 {
		t.Errorf("bob has no ibkr rows, got %+v", rows)
	}
}
