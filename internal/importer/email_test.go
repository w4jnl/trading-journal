package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

func emailFill(orderID string, qty, price float64, at time.Time) Fill {
	return Fill{
		AccountRef:   "LFE050-EXAMPLE1-TEST018",
		BrokerExecID: "MWEMAIL-" + orderID,
		OrderID:      orderID,
		Symbol:       "MESU6",
		SymbolRoot:   "MES",
		Source:       "Rithmic WS",
		Side:         "sell",
		Qty:          qty,
		Price:        price,
		ExecTime:     at,
	}
}

// TestIngestEmailFill_Lifecycle walks the full provisional flow against the
// verified real-world semantics: MW emails CUMULATIVE order snapshots, the
// CSV import supersedes them via (account, order id), and a re-import stays
// idempotent.
func TestIngestEmailFill_Lifecycle(t *testing.T) {
	svc, conn, tmp, _, _ := setupTestService(t)
	ctx := context.Background()
	q := models.New(conn)

	t0 := time.Date(2026, 7, 27, 7, 19, 31, 0, time.UTC)

	// 1. First email: partial fill snapshot (1 of an eventual 3).
	action, err := svc.IngestEmailFill(ctx, svc.UserID, emailFill("172416726", 1, 7516.25, t0))
	if err != nil || action != EmailInserted {
		t.Fatalf("first email = %q err %v, want inserted", action, err)
	}
	acct, err := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: svc.UserID, ImportSource: "motivewave_csv", AccountRef: "LFE050-EXAMPLE1-TEST018",
	})
	if err != nil {
		t.Fatalf("auto-created account: %v", err)
	}
	if acct.Firm == nil || *acct.Firm != "lucid" {
		t.Errorf("firm guess = %v, want lucid", acct.Firm)
	}

	// 2. Second email, same order: cumulative snapshot (3 @ same price, later
	// time) must REPLACE the row, not add a second one.
	t1 := t0.Add(2 * time.Second)
	action, err = svc.IngestEmailFill(ctx, svc.UserID, emailFill("172416726", 3, 7516.25, t1))
	if err != nil || action != EmailUpdated {
		t.Fatalf("cumulative email = %q err %v, want updated", action, err)
	}
	row, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "MWEMAIL-172416726",
	})
	if err != nil || row.Qty != 3 || row.ImportMethod != "mw_email" {
		t.Fatalf("provisional row = qty %v method %q err %v, want qty 3 mw_email", row.Qty, row.ImportMethod, err)
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM executions WHERE account_id=?`, acct.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("execution count after cumulative update = %d err %v, want 1", count, err)
	}

	// 3. Exact replay of the last email: no-op.
	action, err = svc.IngestEmailFill(ctx, svc.UserID, emailFill("172416726", 3, 7516.25, t1))
	if err != nil || action != EmailUnchanged {
		t.Fatalf("replay = %q err %v, want unchanged", action, err)
	}

	// 3b. Replay of the EARLIER partial snapshot (archive sweep walks
	// oldest-first): stale, must not rewind the row.
	action, err = svc.IngestEmailFill(ctx, svc.UserID, emailFill("172416726", 1, 7516.25, t0))
	if err != nil || action != EmailSkippedStale {
		t.Fatalf("stale replay = %q err %v, want skipped_stale", action, err)
	}
	row, err = q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "MWEMAIL-172416726",
	})
	if err != nil || row.Qty != 3 {
		t.Fatalf("row after stale replay = qty %v err %v, want 3", row.Qty, err)
	}

	// 4. CSV import (Executions-view shape with Order ID column) supersedes
	// the provisional row: it disappears, the authoritative ticket row lands.
	header := `Account,"Order ID",Underlying,Symbol,Action,Price,Time,Quantity,"Fill Id",Exchange,Ticket,Source,Connection` + "\n"
	csvRow := `LFE050-EXAMPLE1-TEST018,172416726,MES,MESU6,SLD,7516.25,"07/27/2026 09:19:33.000+0200",3,1035029,CME,1035029,"Rithmic WS",Lucid` + "\n"
	csvPath := filepath.Join(tmp, "mw.csv")
	if err := os.WriteFile(csvPath, []byte(header+csvRow), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := svc.ImportFile(ctx, csvPath)
	if err != nil {
		t.Fatalf("csv import: %v", err)
	}
	if out.ExecsInserted != 1 || out.Superseded != 1 {
		t.Fatalf("csv import = inserted %d superseded %d, want 1/1", out.ExecsInserted, out.Superseded)
	}
	if _, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "MWEMAIL-172416726",
	}); err == nil {
		t.Error("provisional row survived the CSV supersede")
	}
	auth, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "1035029",
	})
	if err != nil || auth.ImportMethod != "motivewave_csv" || auth.OrderID == nil || *auth.OrderID != "172416726" {
		t.Fatalf("authoritative row = %+v err %v", auth, err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM executions WHERE account_id=?`, acct.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("execution count after supersede = %d err %v, want 1 (no double-count)", count, err)
	}

	// 5. A LATE email for the superseded order must not resurrect anything.
	action, err = svc.IngestEmailFill(ctx, svc.UserID, emailFill("172416726", 3, 7516.25, t1))
	if err != nil || action != EmailSkippedAuthorative {
		t.Fatalf("late email = %q err %v, want skipped_authoritative", action, err)
	}

	// 6. End-of-day full re-import of the same CSV: idempotent, nothing new.
	csvPath2 := filepath.Join(tmp, "mw-reimport.csv")
	if err := os.WriteFile(csvPath2, []byte(header+csvRow), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = svc.ImportFile(ctx, csvPath2)
	if err != nil || out.ExecsInserted != 0 || out.Superseded != 0 {
		t.Fatalf("re-import = inserted %d superseded %d err %v, want 0/0", out.ExecsInserted, out.Superseded, err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM executions WHERE account_id=?`, acct.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("execution count after re-import = %d err %v, want 1", count, err)
	}
}

// TestIngestEmailFill_LiveSyncSkip: accounts polled via a broker API (IBKR)
// receive their fills through the API path; the email copy must be dropped at
// or after the cutover so nothing double-counts.
func TestIngestEmailFill_LiveSyncSkip(t *testing.T) {
	svc, conn, _, _, _ := setupTestService(t)
	ctx := context.Background()
	q := models.New(conn)

	// Seed the account via a first email (pre-cutover), then set the cutover.
	f := emailFill("500001", 1, 7000, time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))
	f.AccountRef = "U10000001"
	f.Source = "Interactive Brokers"
	if action, err := svc.IngestEmailFill(ctx, svc.UserID, f); err != nil || action != EmailInserted {
		t.Fatalf("seed email = %q err %v", action, err)
	}
	acct, err := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: svc.UserID, ImportSource: "motivewave_csv", AccountRef: "U10000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	cutover := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	if err := q.SetAccountLiveSyncSince(ctx, models.SetAccountLiveSyncSinceParams{
		LiveSyncSince: &cutover, ID: acct.ID, UserID: svc.UserID,
	}); err != nil {
		t.Fatal(err)
	}

	g := emailFill("500002", 1, 7001, time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC))
	g.AccountRef = "U10000001"
	g.Source = "Interactive Brokers"
	action, err := svc.IngestEmailFill(ctx, svc.UserID, g)
	if err != nil || action != EmailSkippedLiveSync {
		t.Fatalf("post-cutover email = %q err %v, want skipped_live_sync", action, err)
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM executions WHERE account_id=?`, acct.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("execution count = %d err %v, want 1 (only the pre-cutover seed)", count, err)
	}
}

// TestIngestEmailFill_ManualRowGuard: a fill already recorded WITHOUT an
// order id - hand-entered in the reconcile editor or imported from an
// old-format CSV - must block the email insert (the order-id join cannot see
// it). A near-miss on any field must NOT block.
func TestIngestEmailFill_ManualRowGuard(t *testing.T) {
	svc, conn, _, _, _ := setupTestService(t)
	ctx := context.Background()
	q := models.New(conn)

	// Seed the account, then hand-enter a fill the way the reconcile editor
	// does (manual=1, no order id).
	at := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	if action, err := svc.IngestEmailFill(ctx, svc.UserID, emailFill("700001", 1, 7500, at)); err != nil || action != EmailInserted {
		t.Fatalf("seed = %q err %v", action, err)
	}
	acct, err := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: svc.UserID, ImportSource: "motivewave_csv", AccountRef: "LFE050-EXAMPLE1-TEST018",
	})
	if err != nil {
		t.Fatal(err)
	}
	manualAt := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	if _, err := q.InsertManualExecution(ctx, models.InsertManualExecutionParams{
		AccountID: acct.ID, BrokerExecID: "MANUAL-1-guard", Symbol: "MESU6", SymbolRoot: "MES",
		Side: "sell", Qty: 2, Price: 7510.25, ExecTime: manualAt,
	}); err != nil {
		t.Fatal(err)
	}

	// Email for the same fill (time off by 1s - the email clock is derived
	// from the Date header): must be skipped, nothing inserted.
	f := emailFill("700002", 2, 7510.25, manualAt.Add(time.Second))
	action, err := svc.IngestEmailFill(ctx, svc.UserID, f)
	if err != nil || action != EmailSkippedMatched {
		t.Fatalf("duplicate of manual row = %q err %v, want skipped_matched", action, err)
	}
	if _, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "MWEMAIL-700002",
	}); err == nil {
		t.Error("provisional row inserted despite matching manual row")
	}

	// A different price at the same moment is a different fill: goes in.
	g := emailFill("700003", 2, 7511.00, manualAt)
	if action, err := svc.IngestEmailFill(ctx, svc.UserID, g); err != nil || action != EmailInserted {
		t.Fatalf("distinct fill = %q err %v, want inserted", action, err)
	}
	// Same fields but outside the +-2s window: also a different fill.
	h := emailFill("700004", 2, 7510.25, manualAt.Add(10*time.Second))
	if action, err := svc.IngestEmailFill(ctx, svc.UserID, h); err != nil || action != EmailInserted {
		t.Fatalf("out-of-window fill = %q err %v, want inserted", action, err)
	}
}

// TestIngestEmailFill_TombstoneRespected: a user-deleted provisional row must
// not be resurrected by an email replay for the same order.
func TestIngestEmailFill_TombstoneRespected(t *testing.T) {
	svc, conn, _, _, _ := setupTestService(t)
	ctx := context.Background()
	q := models.New(conn)

	at := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	if action, err := svc.IngestEmailFill(ctx, svc.UserID, emailFill("600001", 2, 7500, at)); err != nil || action != EmailInserted {
		t.Fatalf("insert = %q err %v", action, err)
	}
	acct, err := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: svc.UserID, ImportSource: "motivewave_csv", AccountRef: "LFE050-EXAMPLE1-TEST018",
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "MWEMAIL-600001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SoftDeleteExecution(ctx, models.SoftDeleteExecutionParams{ID: row.ID, UserID: svc.UserID}); err != nil {
		t.Fatal(err)
	}

	action, err := svc.IngestEmailFill(ctx, svc.UserID, emailFill("600001", 3, 7500, at.Add(time.Second)))
	if err != nil || action != EmailSkippedTombstoned {
		t.Fatalf("replay on tombstone = %q err %v, want skipped_tombstoned", action, err)
	}
}
