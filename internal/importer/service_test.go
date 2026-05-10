package importer

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaro/trading-journal/internal/db"
	"github.com/jaro/trading-journal/internal/models"
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

	svc := &ImportService{
		DB:           conn,
		Queries:      models.New(conn),
		ProcessedDir: processed,
		FailedDir:    failed,
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
