package handlers

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
)

// TestBulkSave_NoPhantomEditOnRoundMinute reproduces the reconcile bug where
// saving without touching a round-minute fill still stamped edited_at: a
// datetime-local input drops ":00" seconds on submit, so the fill arrived as
// "T19:59" while the guard compared against the stored "T19:59:00" and
// rewrote the row. The guard now compares parsed instants, so a no-op save
// must leave edited_at NULL and report no changes.
func TestBulkSave_NoPhantomEditOnRoundMinute(t *testing.T) {
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")

	firm := "apex"
	acct, err := q.CreateAccount(context.Background(), models.CreateAccountParams{
		UserID: uid, Name: "recon", Firm: &firm, ImportSource: "motivewave_csv",
		AccountRef: "APEX-RECON", Currency: "USD", CommissionSource: "rate",
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// A fill stored at an exact round minute (:00 seconds).
	execTime := time.Date(2026, 7, 8, 19, 59, 0, 0, time.UTC)
	if _, err := q.InsertExecution(context.Background(), models.InsertExecutionParams{
		AccountID: acct.ID, BrokerExecID: "T1", Symbol: "MESU6", SymbolRoot: "MES",
		Side: "sell", Qty: 2, Price: 7534.5, ExecTime: execTime,
	}); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}
	exec, err := q.GetExecutionByBrokerID(context.Background(), models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "T1",
	})
	if err != nil {
		t.Fatalf("GetExecutionByBrokerID: %v", err)
	}

	h := &Handlers{
		DB: conn, Queries: q, Timezone: time.UTC,
		ImportSvc: &importer.ImportService{DB: conn, Queries: q, Timezone: time.UTC, UserID: uid},
	}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})

	// Form the browser would send: unchanged values, but exec_time WITHOUT the
	// ":00" seconds (datetime-local normalization).
	id := exec.ID
	form := url.Values{}
	form.Set("txn."+itoa64(id)+".symbol", "MESU6")
	form.Set("txn."+itoa64(id)+".symbol_root", "MES")
	form.Set("txn."+itoa64(id)+".side", "sell")
	form.Set("txn."+itoa64(id)+".qty", "2")
	form.Set("txn."+itoa64(id)+".price", "7534.5")
	form.Set("txn."+itoa64(id)+".exec_time", "2026-07-08T19:59") // <- no seconds

	req := httptest.NewRequest("POST", "/admin/transactions/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.AdminTransactionsBulkSave(rec, req)

	after, err := q.GetExecutionByBrokerID(context.Background(), models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "T1",
	})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.EditedAt != nil {
		t.Errorf("edited_at = %v, want nil (no-op save must not stamp a phantom edit)", after.EditedAt)
	}
	if loc := rec.Result().Header.Get("Location"); !strings.Contains(loc, "no+changes") {
		t.Errorf("redirect = %q, want a 'no changes' flash", loc)
	}

	// A genuine time edit (different minute) still writes.
	form.Set("txn."+itoa64(id)+".exec_time", "2026-07-08T20:15")
	req2 := httptest.NewRequest("POST", "/admin/transactions/save", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2 = req2.WithContext(ctx)
	h.AdminTransactionsBulkSave(httptest.NewRecorder(), req2)
	edited, _ := q.GetExecutionByBrokerID(context.Background(), models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "T1",
	})
	if edited.EditedAt == nil {
		t.Error("a real time change should have stamped edited_at")
	}
	if !edited.ExecTime.Equal(time.Date(2026, 7, 8, 20, 15, 0, 0, time.UTC)) {
		t.Errorf("exec_time = %v, want 20:15:00Z", edited.ExecTime)
	}
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}

// A save must send the user back to the same filtered view: the filter rides
// on the form action's query string and redirectTransactions copies it (plus
// the flash) onto the redirect.
func TestBulkSave_RedirectKeepsFilter(t *testing.T) {
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	firm := "apex"
	acct, err := q.CreateAccount(context.Background(), models.CreateAccountParams{
		UserID: uid, Name: "recon", Firm: &firm, ImportSource: "motivewave_csv",
		AccountRef: "APEX-RECON-2", Currency: "USD", CommissionSource: "rate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertExecution(context.Background(), models.InsertExecutionParams{
		AccountID: acct.ID, BrokerExecID: "T9", Symbol: "MESU6", SymbolRoot: "MES",
		Side: "buy", Qty: 1, Price: 7500, ExecTime: time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	exec, _ := q.GetExecutionByBrokerID(context.Background(), models.GetExecutionByBrokerIDParams{AccountID: acct.ID, BrokerExecID: "T9"})
	h := &Handlers{DB: conn, Queries: q, Timezone: time.UTC,
		ImportSvc: &importer.ImportService{DB: conn, Queries: q, Timezone: time.UTC, UserID: uid}}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})

	form := url.Values{}
	form.Set("txn."+itoa64(exec.ID)+".delete", "on")
	target := "/admin/transactions/save?account_id=" + itoa64(acct.ID) + "&from=2026-06-01&to=2026-06-30&deleted=1"
	req := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.AdminTransactionsBulkSave(rec, req)

	loc := rec.Result().Header.Get("Location")
	for _, want := range []string{"/admin/transactions?", "account_id=" + itoa64(acct.ID), "from=2026-06-01", "to=2026-06-30", "deleted=1", "flash=saved+1+change"} {
		if !strings.Contains(loc, want) {
			t.Errorf("redirect %q missing %q", loc, want)
		}
	}
	if after, _ := q.GetExecutionByBrokerID(context.Background(), models.GetExecutionByBrokerIDParams{AccountID: acct.ID, BrokerExecID: "T9"}); after.DeletedAt == nil {
		t.Errorf("fill was not soft-deleted")
	}
}
