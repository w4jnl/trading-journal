package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
)

// Archives hold config.yaml and with it every secret, so they must not be
// world-readable; pre-migration archives have their own small retention.
func TestArchiveModeAndPreMigrationRetention(t *testing.T) {
	dir := t.TempDir()
	conn := dbtest.Open(t, "boot")
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("server:\n  port: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := Options{Dir: dir, DBPath: "unused", ConfigPath: cfg, DB: conn, Loc: time.UTC}
	if err := RunOnce(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		if m := info.Mode().Perm(); m != 0o600 {
			t.Errorf("%s mode = %o, want 0600", e.Name(), m)
		}
	}
	for _, v := range []string{"20260101000000", "20260201000000", "20260301000000", "20260401000000"} {
		if err := RunPreMigration(context.Background(), o, v); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ = os.ReadDir(dir)
	var pre []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pre-migrate-") {
			pre = append(pre, e.Name())
		}
	}
	if len(pre) != keepPreMigrate {
		t.Fatalf("pre-migration archives = %v, want the newest %d", pre, keepPreMigrate)
	}
	for _, n := range pre {
		if strings.Contains(n, "20260101000000") {
			t.Errorf("oldest pre-migration archive survived pruning: %v", pre)
		}
	}
	// The daily archive is untouched by the pre-migration pruning.
	if a := listArchives(t, dir); len(a) != 1 {
		t.Errorf("daily archives = %v", a)
	}
}

// The restore path: the snapshot inside the archive opens, passes
// integrity_check and needs no migration on a same-version binary.
func TestRestoreSnapshotIsHealthy(t *testing.T) {
	dir := t.TempDir()
	conn := dbtest.Open(t, "boot")
	if _, err := conn.Exec(`INSERT INTO users (username) VALUES ('restored-user')`); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("server:\n  port: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := Options{Dir: dir, DBPath: "unused", ConfigPath: cfg, DB: conn, Loc: time.UTC}
	if err := RunOnce(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, listArchives(t, dir)[0])

	// Extract db/trading.db the way the README tells the operator to.
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	restored := filepath.Join(t.TempDir(), "trading.db")
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "db/trading.db" {
			out, err := os.Create(restored)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				t.Fatal(err)
			}
			out.Close()
			found = true
		}
	}
	if !found {
		t.Fatal("archive has no db/trading.db")
	}
	rconn, err := db.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer rconn.Close()
	var ic string
	if err := rconn.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
		t.Fatalf("integrity_check = %q, %v", ic, err)
	}
	var n int
	if err := rconn.QueryRow(`SELECT count(*) FROM users WHERE username = 'restored-user'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("restored data missing: n=%d err=%v", n, err)
	}
	pending, err := db.PendingMigrations(rconn)
	if err != nil || len(pending) != 0 {
		t.Errorf("same-version restore should need no migrations: %v %v", pending, err)
	}
	if err := db.RunMigrations(rconn, db.MigrationVars{BootstrapUsername: "boot"}); err != nil {
		t.Errorf("RunMigrations on the restored snapshot: %v", err)
	}
}
