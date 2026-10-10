package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// idorFixture seeds two users with one account and one closed trade each and
// returns a Handlers wired to the migrated DB. Routes are mounted by the
// caller so each test states exactly which handler it probes.
type idorFixture struct {
	h            *Handlers
	q            *models.Queries
	alice, bob   *auth.User
	aAcct, bAcct int64
	aTrade       int64
	bTrade       int64
}

func newIDORFixture(t *testing.T) *idorFixture {
	t.Helper()
	conn, q := newAuthzTestDB(t)
	ctx := context.Background()
	mkUser := func(name string) *auth.User {
		u, err := q.CreateUser(ctx, models.CreateUserParams{Username: name})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return &auth.User{ID: u.ID, Username: name, DisplayName: name}
	}
	alice, bob := mkUser("alice"), mkUser("bob")
	mkAcct := func(u *auth.User, ref string) int64 {
		a, err := q.CreateAccount(ctx, models.CreateAccountParams{
			UserID: u.ID, Name: ref, ImportSource: "motivewave_csv",
			AccountRef: ref, Currency: "USD", CommissionSource: "rate",
		})
		if err != nil {
			t.Fatalf("create account %s: %v", ref, err)
		}
		return a.ID
	}
	aAcct, bAcct := mkAcct(alice, "APEX-A"), mkAcct(bob, "APEX-B")
	now := time.Now().UTC()
	exit := now.Add(time.Hour)
	mkTrade := func(acct int64) int64 {
		tr, err := q.CreateTrade(ctx, models.CreateTradeParams{
			AccountID: acct, Symbol: "MESU26", SymbolRoot: "MES", Direction: "long",
			Qty: 1, EntryPrice: 5000, ExitPrice: ptrF(5010), EntryTime: now, ExitTime: &exit,
			NetPnl: ptrF(50), Status: "closed",
		})
		if err != nil {
			t.Fatalf("create trade: %v", err)
		}
		return tr.ID
	}
	return &idorFixture{
		h:     &Handlers{DB: conn, Queries: q, Timezone: time.UTC, AttachmentsDir: t.TempDir()},
		q:     q,
		alice: alice, bob: bob, aAcct: aAcct, bAcct: bAcct,
		aTrade: mkTrade(aAcct), bTrade: mkTrade(bAcct),
	}
}

// post sends a form POST as user u through mux and returns the recorder.
func (f *idorFixture) post(t *testing.T, mux *http.ServeMux, u *auth.User, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(auth.WithUser(context.Background(), u))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func (f *idorFixture) tradeAccount(t *testing.T, tradeID int64) int64 {
	t.Helper()
	var acct int64
	if err := f.h.DB.QueryRow(`SELECT account_id FROM trades WHERE id = ?`, tradeID).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	return acct
}

// Reassign used to move ANY account's history into ANY account: the
// queries are keyed on account_id alone. Both sides must be the caller's.
func TestAccountsReassignIsUserScoped(t *testing.T) {
	f := newIDORFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts/{id}/reassign", f.h.AccountsReassign)

	// Foreign source: alice tries to pull bob's history into her account.
	rec := f.post(t, mux, f.alice, fmt.Sprintf("/accounts/%d/reassign", f.bAcct), url.Values{"to": {fmt.Sprint(f.aAcct)}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign source: code=%d, want 404", rec.Code)
	}
	if got := f.tradeAccount(t, f.bTrade); got != f.bAcct {
		t.Errorf("bob's trade moved to account %d", got)
	}
	// Foreign destination: alice tries to scatter her data into bob's account.
	rec = f.post(t, mux, f.alice, fmt.Sprintf("/accounts/%d/reassign", f.aAcct), url.Values{"to": {fmt.Sprint(f.bAcct)}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign destination: code=%d, want 404", rec.Code)
	}
	if got := f.tradeAccount(t, f.aTrade); got != f.aAcct {
		t.Errorf("alice's trade moved to account %d", got)
	}
	// Own accounts: the move works.
	a2, err := f.q.CreateAccount(context.Background(), models.CreateAccountParams{
		UserID: f.alice.ID, Name: "APEX-A2", ImportSource: "motivewave_csv", AccountRef: "APEX-A2", Currency: "USD", CommissionSource: "rate",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = f.post(t, mux, f.alice, fmt.Sprintf("/accounts/%d/reassign", f.aAcct), url.Values{"to": {fmt.Sprint(a2.ID)}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "moved") {
		t.Errorf("own reassign: code=%d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
	if got := f.tradeAccount(t, f.aTrade); got != a2.ID {
		t.Errorf("alice's trade not moved: account %d", got)
	}
}

func TestBalanceAnchorIsUserScoped(t *testing.T) {
	f := newIDORFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts/{id}/balances", f.h.AccountsAddBalance)
	count := func() int {
		var n int
		if err := f.h.DB.QueryRow(`SELECT count(*) FROM balance_anchors`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rec := f.post(t, mux, f.alice, fmt.Sprintf("/accounts/%d/balances", f.bAcct), url.Values{"balance": {"50000"}, "anchor_date": {"2026-10-01"}})
	if rec.Code != http.StatusNotFound || count() != 0 {
		t.Errorf("foreign account: code=%d anchors=%d, want 404 and none", rec.Code, count())
	}
	rec = f.post(t, mux, f.alice, fmt.Sprintf("/accounts/%d/balances", f.aAcct), url.Values{"balance": {"50000"}, "anchor_date": {"2026-10-01"}})
	if rec.Code != http.StatusSeeOther || count() != 1 {
		t.Errorf("own account: code=%d anchors=%d, want 303 and one", rec.Code, count())
	}
}

func TestGroupUnlinkIsUserScoped(t *testing.T) {
	f := newIDORFixture(t)
	ctx := context.Background()
	entry, err := f.q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: &f.bob.ID, EntryDate: time.Now().UTC(), Body: "bob's group", Scope: "group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.DB.Exec(`INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)`, entry.ID, f.bTrade); err != nil {
		t.Fatal(err)
	}
	links := func() int {
		var n int
		if err := f.h.DB.QueryRow(`SELECT count(*) FROM journal_entry_trades WHERE journal_entry_id = ?`, entry.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /journal/group/{id}/unlink", f.h.GroupJournalUnlink)
	rec := f.post(t, mux, f.alice, fmt.Sprintf("/journal/group/%d/unlink", entry.ID), url.Values{"trade_id": {fmt.Sprint(f.bTrade)}})
	if rec.Code != http.StatusNotFound || links() != 1 {
		t.Errorf("foreign unlink: code=%d links=%d, want 404 and link intact", rec.Code, links())
	}
	rec = f.post(t, mux, f.bob, fmt.Sprintf("/journal/group/%d/unlink", entry.ID), url.Values{"trade_id": {fmt.Sprint(f.bTrade)}})
	if rec.Code != http.StatusSeeOther || links() != 0 {
		t.Errorf("own unlink: code=%d links=%d, want 303 and link removed", rec.Code, links())
	}
}

// Attachments are served to their uploader only. Before user_id existed the
// serve query accepted any row with entry_id = 0 - every upload, since the
// save handlers never fill entry_id in - for any signed-in user.
func TestAttachmentServeIsUserScoped(t *testing.T) {
	f := newIDORFixture(t)
	if err := os.WriteFile(filepath.Join(f.h.AttachmentsDir, "bobchart.png"), []byte("\x89PNG-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := f.h.DB.Exec(`INSERT INTO journal_attachments (user_id, entry_id, entry_scope, file_path, mime, byte_size) VALUES (?, 0, 'day', 'bobchart.png', 'image/png', 9)`, f.bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /journal/attachments/{id}", f.h.JournalAttachmentServe)
	get := func(u *auth.User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/journal/attachments/%d", id), nil).WithContext(auth.WithUser(context.Background(), u))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(f.alice); rec.Code != http.StatusNotFound {
		t.Errorf("alice read bob's attachment: code=%d", rec.Code)
	}
	if rec := get(f.bob); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PNG-bytes") {
		t.Errorf("bob cannot read own attachment: code=%d", rec.Code)
	}
}

// The migration backfills user_id from the body that references the image.
func TestAttachmentOwnerBackfillFromBody(t *testing.T) {
	conn := dbtest.Open(t, "bootstrap")
	// dbtest applied every migration, including the backfill, against an
	// empty table; re-run the backfill statements by hand on seeded rows to
	// prove the GLOB keys on the referencing body.
	for _, stmt := range []string{
		`INSERT INTO users (id, username) VALUES (41, 'carol'), (42, 'dave')`,
		`INSERT INTO journal_attachments (id, entry_id, entry_scope, file_path, mime, byte_size, user_id) VALUES
		   (1, 0, 'day', 'a1.png', 'image/png', 1, NULL),
		   (12, 0, 'day', 'a12.png', 'image/png', 1, NULL),
		   (7, 0, 'day', 'a7.png', 'image/png', 1, NULL)`,
		`INSERT INTO journal_entries (user_id, entry_date, body, scope) VALUES
		   (41, '2026-10-01', 'see ![](/journal/attachments/12) here', 'day'),
		   (42, '2026-10-01', 'mine ![](/journal/attachments/1)', 'day')`,
		`UPDATE journal_attachments SET user_id = (
		    SELECT je.user_id FROM journal_entries je
		    WHERE je.user_id IS NOT NULL AND je.body GLOB ('*/journal/attachments/' || journal_attachments.id || '[^0-9]*') LIMIT 1)
		  WHERE user_id IS NULL`,
		`UPDATE journal_attachments SET user_id = (SELECT id FROM users WHERE username = 'bootstrap') WHERE user_id IS NULL`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	owner := func(id int64) string {
		var u string
		if err := conn.QueryRow(`SELECT u.username FROM journal_attachments a JOIN users u ON u.id = a.user_id WHERE a.id = ?`, id).Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	if got := owner(12); got != "carol" {
		t.Errorf("attachment 12 owner = %s, want carol", got)
	}
	if got := owner(1); got != "dave" { // 1 must not match the /12 reference
		t.Errorf("attachment 1 owner = %s, want dave (digit boundary)", got)
	}
	if got := owner(7); got != "bootstrap" {
		t.Errorf("unreferenced attachment owner = %s, want bootstrap fallback", got)
	}
}
