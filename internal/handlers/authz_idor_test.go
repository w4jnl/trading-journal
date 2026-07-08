package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// newAuthzTestDB opens a migrated, throwaway SQLite DB for authorization tests.
// The bootstrap migration seeds instruments (incl. MES) and a bootstrap user;
// the tests add their own users on top.
func newAuthzTestDB(t *testing.T) (*sql.DB, *models.Queries) {
	t.Helper()
	conn := dbtest.Open(t, "bootstrap")
	return conn, models.New(conn)
}

func ptrF(v float64) *float64 { return &v }

// TestTradeAndJournalAreUserScoped is an IDOR probe: the queries every
// trade-detail and journal handler funnels through (GetTradeByID,
// GetJournalEntryByID) must refuse to return one user's resource to another,
// even when the attacker knows the exact row id. The scoping lives in the SQL
// (WHERE ... a.user_id = ? / user_id = ?), so a regression here silently opens
// cross-tenant reads.
func TestTradeAndJournalAreUserScoped(t *testing.T) {
	_, q := newAuthzTestDB(t)
	ctx := context.Background()

	alice, err := q.CreateUser(ctx, models.CreateUserParams{Username: "alice"})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := q.CreateUser(ctx, models.CreateUserParams{Username: "bob"})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	acct, err := q.CreateAccount(ctx, models.CreateAccountParams{
		UserID: alice.ID, Name: "A-1", ImportSource: "motivewave_csv",
		AccountRef: "APEX-A-1", Currency: "USD", CommissionSource: "rate",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	now := time.Now().UTC()
	exit := now.Add(time.Hour)
	trade, err := q.CreateTrade(ctx, models.CreateTradeParams{
		AccountID: acct.ID, Symbol: "MESU26", SymbolRoot: "MES", Direction: "long",
		Qty: 1, EntryPrice: 5000, ExitPrice: ptrF(5010), EntryTime: now, ExitTime: &exit,
		NetPnl: ptrF(50), Status: "closed",
	})
	if err != nil {
		t.Fatalf("create trade: %v", err)
	}

	entry, err := q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: &alice.ID, EntryDate: now, Body: "alice private notes", Scope: "day",
	})
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}

	// Owner reads succeed.
	if _, err := q.GetTradeByID(ctx, models.GetTradeByIDParams{ID: trade.ID, UserID: alice.ID}); err != nil {
		t.Fatalf("owner cannot read own trade: %v", err)
	}
	if _, err := q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entry.ID, UserID: &alice.ID}); err != nil {
		t.Fatalf("owner cannot read own journal: %v", err)
	}

	// Cross-user reads must return no rows (the handler maps this to 404).
	if _, err := q.GetTradeByID(ctx, models.GetTradeByIDParams{ID: trade.ID, UserID: bob.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("bob read alice's trade (err=%v) - IDOR", err)
	}
	if _, err := q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entry.ID, UserID: &bob.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("bob read alice's journal (err=%v) - IDOR", err)
	}
}

// TestAPIKeyMiddlewareValidThenRevoked drives the real APIKeyAuth.Middleware
// against a live DB: a valid key resolves to its owner and reaches the handler;
// once revoked, the same key is rejected with 401 and never dispatches.
func TestAPIKeyMiddlewareValidThenRevoked(t *testing.T) {
	_, q := newAuthzTestDB(t)
	ctx := context.Background()

	user, err := q.CreateUser(ctx, models.CreateUserParams{Username: "keyholder"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	full, prefix, sha, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := q.CreateAPIKey(ctx, models.CreateAPIKeyParams{
		UserID: user.ID, Name: "test", KeyPrefix: prefix, KeySha256: sha,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	a := &auth.APIKeyAuth{Queries: q}
	drive := func() (int, string) {
		var gotUser string
		h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u := auth.UserFrom(r.Context()); u != nil {
				gotUser = u.Username
			}
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/trades", nil)
		req.Header.Set("Authorization", "Bearer "+full)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, gotUser
	}

	if code, who := drive(); code != http.StatusOK || who != "keyholder" {
		t.Fatalf("valid key: status=%d user=%q, want 200 / keyholder", code, who)
	}

	if err := q.RevokeAPIKey(ctx, models.RevokeAPIKeyParams{ID: key.ID, UserID: user.ID}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code, who := drive(); code != http.StatusUnauthorized || who != "" {
		t.Errorf("revoked key: status=%d user=%q, want 401 / empty", code, who)
	}
}
