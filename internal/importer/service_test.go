package importer

import (
	"context"
	"database/sql"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/models"
)

func setupTestService(t *testing.T) (*ImportService, *sql.DB, string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if err := db.RunMigrations(conn); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	processed := filepath.Join(tmp, "processed")
	failed := filepath.Join(tmp, "failed")

	loc, _ := time.LoadLocation("America/Chicago")
	svc := &ImportService{
		DB:           conn,
		Queries:      models.New(conn),
		ProcessedDir: processed,
		FailedDir:    failed,
		Timezone:     loc,
	}
	return svc, conn, tmp, processed, failed
}

func copyFixtureTo(t *testing.T, fixturePath, destDir string) string {
	t.Helper()
	src, err := os.Open(fixturePath)
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	defer src.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(destDir, filepath.Base(fixturePath))
	out, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestImportFile_MotiveWaveFixture(t *testing.T) {
	svc, conn, tmp, processed, _ := setupTestService(t)
	src := copyFixtureTo(t, motivewaveFixture, filepath.Join(tmp, "inbox"))

	out, err := svc.ImportFile(context.Background(), src)
	if err != nil {
		t.Fatalf("ImportFile: %v (outcome %+v)", err, out)
	}

	if out.ErrorMessage != "" {
		t.Fatalf("import reported error: %s", out.ErrorMessage)
	}
	if out.Format != FormatMotiveWave {
		t.Errorf("format = %q, want motivewave_csv", out.Format)
	}
	if out.FillsParsed != 990 {
		t.Errorf("FillsParsed = %d, want 990", out.FillsParsed)
	}
	if out.ExecsInserted != 990 {
		t.Errorf("ExecsInserted = %d, want 990", out.ExecsInserted)
	}
	if out.AccountsCreated != 17 {
		t.Errorf("AccountsCreated = %d, want 17", out.AccountsCreated)
	}
	if out.InstrumentsStubbed != 0 {
		t.Errorf("InstrumentsStubbed = %d, want 0 (all roots are seeded after EP→ES canonicalization)", out.InstrumentsStubbed)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source file should be gone, stat err = %v", err)
	}
	if out.DestinationPath == "" || filepath.Dir(out.DestinationPath) != processed {
		t.Errorf("DestinationPath = %q, want a file in %q", out.DestinationPath, processed)
	}

	assertExecCount(t, conn, 990)
	assertSyncLogCount(t, conn, 1, "success")

	// AMP account 120042 should have firm=amp from Source-based detection.
	var firm sql.NullString
	if err := conn.QueryRow(`SELECT firm FROM accounts WHERE account_ref = '120042'`).Scan(&firm); err != nil {
		t.Fatalf("query AMP account: %v", err)
	}
	if !firm.Valid || firm.String != "amp" {
		t.Errorf("AMP account firm = %v, want 'amp'", firm)
	}

	// The position builder must have produced at least some closed trades.
	if out.TradesCreated == 0 {
		t.Error("TradesCreated = 0, expected the builder to materialize closed trades from real fills")
	}

	// Spot-check the first APEX-100001-28 ES trade. From the raw CSV the first
	// two fills for that account/symbol are sell 1 @ 7253 then buy 1 @ 7252.25
	// — a short held for ~23 minutes that closed +0.75 points.
	apexFirst := firstTradeForAccount(t, conn, "APEX-100001-28", "ESM6")
	if apexFirst.Direction != "short" {
		t.Errorf("APEX-100001-28 ES first trade direction = %q, want short", apexFirst.Direction)
	}
	if apexFirst.EntryPrice != 7253 {
		t.Errorf("entry_price = %v, want 7253", apexFirst.EntryPrice)
	}
	if apexFirst.ExitPrice == nil || *apexFirst.ExitPrice != 7252.25 {
		t.Errorf("exit_price = %v, want 7252.25", apexFirst.ExitPrice)
	}
	wantGross := 0.75 * 50.0 // 0.75 ES points × $50 multiplier × 1 contract
	if apexFirst.GrossPnl == nil || !approxEq(*apexFirst.GrossPnl, wantGross) {
		t.Errorf("gross_pnl = %v, want %v", apexFirst.GrossPnl, wantGross)
	}
	// Apex ES is $1.99 per side; round-turn for 1 contract is $3.98.
	wantComm := 1.99 * 2
	if apexFirst.Commission == nil || !approxEq(*apexFirst.Commission, wantComm) {
		t.Errorf("commission = %v, want %v (apex ES round-turn)", apexFirst.Commission, wantComm)
	}
	if apexFirst.NetPnl == nil || !approxEq(*apexFirst.NetPnl, wantGross-wantComm) {
		t.Errorf("net_pnl = %v, want %v", apexFirst.NetPnl, wantGross-wantComm)
	}
}

func TestImportFile_TopstepXFixture(t *testing.T) {
	svc, conn, tmp, processed, _ := setupTestService(t)
	src := copyFixtureTo(t, topstepxFixture, filepath.Join(tmp, "inbox"))

	out, err := svc.ImportFile(context.Background(), src)
	if err != nil {
		t.Fatalf("ImportFile: %v (outcome %+v)", err, out)
	}

	if out.ErrorMessage != "" {
		t.Fatalf("import reported error: %s", out.ErrorMessage)
	}
	if out.Format != FormatTopstepX {
		t.Errorf("format = %q, want topstepx_csv", out.Format)
	}
	if out.FillsParsed != 49 {
		t.Errorf("FillsParsed = %d, want 49", out.FillsParsed)
	}
	if out.ExecsInserted != 49 {
		t.Errorf("ExecsInserted = %d, want 49", out.ExecsInserted)
	}
	if out.AccountsCreated != 1 {
		t.Errorf("AccountsCreated = %d, want 1", out.AccountsCreated)
	}
	if out.InstrumentsStubbed != 0 {
		t.Errorf("InstrumentsStubbed = %d, want 0", out.InstrumentsStubbed)
	}
	if filepath.Dir(out.DestinationPath) != processed {
		t.Errorf("DestinationPath = %q, want a file in %q", out.DestinationPath, processed)
	}

	assertExecCount(t, conn, 49)
	assertSyncLogCount(t, conn, 1, "success")

	var firm sql.NullString
	if err := conn.QueryRow(`SELECT firm FROM accounts WHERE import_source = 'topstepx_csv' LIMIT 1`).Scan(&firm); err != nil {
		t.Fatalf("query topstepx account: %v", err)
	}
	if !firm.Valid || firm.String != "topstep" {
		t.Errorf("TopstepX account firm = %v, want 'topstep'", firm)
	}

	if out.TradesCreated == 0 {
		t.Error("TradesCreated = 0, expected the builder to materialize closed trades")
	}
}

func TestImportFile_Idempotent(t *testing.T) {
	svc, conn, tmp, _, _ := setupTestService(t)

	src1 := copyFixtureTo(t, motivewaveFixture, filepath.Join(tmp, "inbox1"))
	out1, err := svc.ImportFile(context.Background(), src1)
	if err != nil || out1.ErrorMessage != "" {
		t.Fatalf("first import: %v %s", err, out1.ErrorMessage)
	}

	src2 := copyFixtureTo(t, motivewaveFixture, filepath.Join(tmp, "inbox2"))
	out2, err := svc.ImportFile(context.Background(), src2)
	if err != nil || out2.ErrorMessage != "" {
		t.Fatalf("second import: %v %s", err, out2.ErrorMessage)
	}

	if out2.ExecsInserted != 0 {
		t.Errorf("second import ExecsInserted = %d, want 0 (all duplicates)", out2.ExecsInserted)
	}
	if out2.AccountsCreated != 0 {
		t.Errorf("second import AccountsCreated = %d, want 0", out2.AccountsCreated)
	}
	assertExecCount(t, conn, 990)
}

// topstepTradesFixture is the trades-CSV sample used by the cross-format
// collision tests. The orders-CSV sample (topstepxFixture) carries
// AccountName=EXPRESS-V2-100000-42718837 throughout.
const topstepTradesFixture = "../../exportData/topstepTrades.csv"

// TestImportFile_OrdersThenTradesRejects verifies the per-account import
// lock fires when an orders-imported account then receives a trades CSV.
// The orders fixture creates exactly one TopStep account, so the
// trades-CSV import (with no filename hint) resolves to that same account
// via resolveTopstepAccount's "exactly one" branch.
func TestImportFile_OrdersThenTradesRejects(t *testing.T) {
	svc, _, tmp, _, failed := setupTestService(t)

	// First: orders CSV, succeeds, claims the account as 'orders'.
	orders := copyFixtureTo(t, topstepxFixture, filepath.Join(tmp, "inbox-orders"))
	out1, err := svc.ImportFile(context.Background(), orders)
	if err != nil || out1.ErrorMessage != "" {
		t.Fatalf("orders import: err=%v outcome=%+v", err, out1)
	}

	// Then: trades CSV, must be rejected.
	trades := copyFixtureTo(t, topstepTradesFixture, filepath.Join(tmp, "inbox-trades"))
	out2, err := svc.ImportFile(context.Background(), trades)
	if err != nil {
		t.Fatalf("trades import returned hard error: %v", err)
	}
	if out2.ErrorMessage == "" {
		t.Fatalf("expected ErrorMessage, got empty (outcome %+v)", out2)
	}
	if !contains(out2.ErrorMessage, "already orders-imported") {
		t.Errorf("ErrorMessage = %q, want it to mention 'already orders-imported'", out2.ErrorMessage)
	}
	if filepath.Dir(out2.DestinationPath) != failed {
		t.Errorf("DestinationPath = %q, want a file in %q", out2.DestinationPath, failed)
	}
}

// TestImportFile_TradesThenOrdersRejects is the symmetric case. We hand-
// craft a small orders CSV whose AccountName matches the trades-imported
// account so resolveTopstepAccount returns the existing inline account
// (otherwise the orders CSV would create a brand-new account and there'd
// be no collision to test).
func TestImportFile_TradesThenOrdersRejects(t *testing.T) {
	svc, conn, tmp, _, failed := setupTestService(t)

	// First: trades CSV. With no other TopStep accounts, resolveTopstepAccount
	// creates TOPSTEP-DEFAULT and flips its commission_source to 'inline'.
	trades := copyFixtureTo(t, topstepTradesFixture, filepath.Join(tmp, "inbox-trades"))
	out1, err := svc.ImportFile(context.Background(), trades)
	if err != nil || out1.ErrorMessage != "" {
		t.Fatalf("trades import: err=%v outcome=%+v", err, out1)
	}

	// Recover the account_ref the trades importer settled on.
	var accountRef string
	if err := conn.QueryRow(`SELECT account_ref FROM accounts WHERE import_source = 'topstepx_csv' LIMIT 1`).Scan(&accountRef); err != nil {
		t.Fatalf("query topstep account_ref: %v", err)
	}

	// Then: a tiny orders CSV that targets that exact account by name. The
	// AccountName column is what findOrCreateAccount keys off (after the
	// resolveTopstepAccount path is bypassed for FormatTopstepX - orders
	// imports go through findOrCreateAccount directly per AccountRef).
	ordersCSV := []byte("Id,AccountName,ContractName,Status,Type,Size,Side,CreatedAt,TradeDay,FilledAt,CancelledAt,TriggeredAt,StopPrice,LimitPrice,ExecutePrice,TriggeredPrice,PositionDisposition,CreationDisposition,RejectionReason,ExchangeOrderId,PlatformOrderId\n" +
		"99999," + accountRef + ",MESM6,Filled,Limit,1,Ask,05/15/2026 10:00:00 +00:00,05/15/2026 00:00:00 +00:00,05/15/2026 10:00:00 +00:00,,,,,7300.0,,Opening,Trader,,,\n")
	ordersPath := filepath.Join(tmp, "inbox-orders", "tiny-orders.csv")
	if err := os.MkdirAll(filepath.Dir(ordersPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ordersPath, ordersCSV, 0o644); err != nil {
		t.Fatal(err)
	}

	out2, err := svc.ImportFile(context.Background(), ordersPath)
	if err != nil {
		t.Fatalf("orders import returned hard error: %v", err)
	}
	if out2.ErrorMessage == "" {
		t.Fatalf("expected ErrorMessage, got empty (outcome %+v)", out2)
	}
	if !contains(out2.ErrorMessage, "already trades-imported") {
		t.Errorf("ErrorMessage = %q, want it to mention 'already trades-imported'", out2.ErrorMessage)
	}
	if filepath.Dir(out2.DestinationPath) != failed {
		t.Errorf("DestinationPath = %q, want a file in %q", out2.DestinationPath, failed)
	}
}

// TestImportFile_FreshAccountAcceptsEither - regression - guards against
// the claim check rejecting brand-new accounts.
func TestImportFile_FreshAccountAcceptsEither(t *testing.T) {
	svc, _, tmp, processed, _ := setupTestService(t)

	// Orders CSV onto an empty DB should succeed.
	src := copyFixtureTo(t, topstepxFixture, filepath.Join(tmp, "inbox"))
	out, err := svc.ImportFile(context.Background(), src)
	if err != nil || out.ErrorMessage != "" {
		t.Fatalf("orders into fresh DB failed: err=%v outcome=%+v", err, out)
	}
	if filepath.Dir(out.DestinationPath) != processed {
		t.Errorf("DestinationPath = %q, want a file in %q", out.DestinationPath, processed)
	}
}

// TestImportFile_MotiveWaveNotBlockedByStaleInlineFlag - regression. A
// MotiveWave (APEX) account can end up with commission_source='inline'
// via the migration backfill after a user reassigned TopstepX trades
// into it (see migration 20260515120000). The TopStep claim-lock must
// not fire for the subsequent MotiveWave import, and the FIFO builder
// must still run for that account so new fills produce trades.
func TestImportFile_MotiveWaveNotBlockedByStaleInlineFlag(t *testing.T) {
	svc, conn, tmp, processed, _ := setupTestService(t)

	// Simulate the migration-leak state: a MotiveWave account with the
	// stale 'inline' flag. We use one of the account_refs that appears in
	// the MotiveWave fixture so the importer resolves to this exact row
	// instead of creating a fresh one.
	if _, err := conn.Exec(`
		INSERT INTO accounts (name, firm, import_source, account_ref, type, currency, needs_setup, commission_source)
		VALUES ('APEX-100001-28', 'apex', 'motivewave_csv', 'APEX-100001-28', 'prop_eval', 'USD', 0, 'inline')
	`); err != nil {
		t.Fatalf("seed inline APEX account: %v", err)
	}

	src := copyFixtureTo(t, motivewaveFixture, filepath.Join(tmp, "inbox"))
	out, err := svc.ImportFile(context.Background(), src)
	if err != nil {
		t.Fatalf("ImportFile: %v (outcome %+v)", err, out)
	}
	if out.ErrorMessage != "" {
		t.Fatalf("import rejected with %q, want success (stale inline flag must not block MotiveWave)", out.ErrorMessage)
	}
	if filepath.Dir(out.DestinationPath) != processed {
		t.Errorf("DestinationPath = %q, want a file in %q", out.DestinationPath, processed)
	}

	// FIFO must have run for the inline-flagged account too - otherwise
	// the import silently produces zero trades for it.
	var apexTrades int
	if err := conn.QueryRow(`
		SELECT COUNT(*) FROM trades t
		JOIN accounts a ON a.id = t.account_id
		WHERE a.account_ref = 'APEX-100001-28'
	`).Scan(&apexTrades); err != nil {
		t.Fatalf("count APEX trades: %v", err)
	}
	if apexTrades == 0 {
		t.Error("APEX-100001-28 has 0 trades after import, expected FIFO to run despite the stale inline flag")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestImportFile_BadFormatGoesToFailed(t *testing.T) {
	svc, _, tmp, _, failed := setupTestService(t)

	bad := filepath.Join(tmp, "inbox", "garbage.csv")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("Foo,Bar,Baz\n1,2,3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := svc.ImportFile(context.Background(), bad)
	if err != nil {
		t.Fatalf("ImportFile returned hard error: %v", err)
	}
	if out.ErrorMessage == "" {
		t.Errorf("expected ErrorMessage, got empty")
	}
	if filepath.Dir(out.DestinationPath) != failed {
		t.Errorf("DestinationPath = %q, want a file in %q", out.DestinationPath, failed)
	}
	sidecar := out.DestinationPath + ".error.txt"
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("sidecar missing: %v", err)
	}
}

func firstTradeForAccount(t *testing.T, conn *sql.DB, accountRef, symbol string) models.Trade {
	t.Helper()
	row := conn.QueryRow(`
		SELECT t.id, t.account_id, t.symbol, t.symbol_root, t.direction, t.qty,
		       t.entry_price, t.exit_price, t.entry_time, t.exit_time,
		       t.gross_pnl, t.commission, t.net_pnl, t.status, t.tags, t.setup, t.created_at
		FROM trades t
		JOIN accounts a ON a.id = t.account_id
		WHERE a.account_ref = ? AND t.symbol = ?
		ORDER BY t.entry_time, t.id
		LIMIT 1
	`, accountRef, symbol)
	var tr models.Trade
	if err := row.Scan(
		&tr.ID, &tr.AccountID, &tr.Symbol, &tr.SymbolRoot, &tr.Direction, &tr.Qty,
		&tr.EntryPrice, &tr.ExitPrice, &tr.EntryTime, &tr.ExitTime,
		&tr.GrossPnl, &tr.Commission, &tr.NetPnl, &tr.Status, &tr.Tags, &tr.Setup, &tr.CreatedAt,
	); err != nil {
		t.Fatalf("first trade %s/%s: %v", accountRef, symbol, err)
	}
	return tr
}

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

func assertExecCount(t *testing.T, conn *sql.DB, want int) {
	t.Helper()
	var got int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&got); err != nil {
		t.Fatalf("count execs: %v", err)
	}
	if got != want {
		t.Errorf("executions row count = %d, want %d", got, want)
	}
}

func assertSyncLogCount(t *testing.T, conn *sql.DB, want int, status string) {
	t.Helper()
	var got int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sync_log WHERE status = ?`, status).Scan(&got); err != nil {
		t.Fatalf("count sync_log: %v", err)
	}
	if got != want {
		t.Errorf("sync_log[status=%q] count = %d, want %d", status, got, want)
	}
}
