package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPendingAndDowngradeGuard(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	pending, err := PendingMigrations(conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		t.Fatal("fresh database reports no pending migrations")
	}
	for i := 1; i < len(pending); i++ {
		if pending[i] < pending[i-1] {
			t.Fatalf("pending not sorted: %v", pending)
		}
	}
	if err := RunMigrations(conn, MigrationVars{BootstrapUsername: "boot"}); err != nil {
		t.Fatal(err)
	}
	if pending, _ = PendingMigrations(conn); len(pending) != 0 {
		t.Errorf("after migrating, pending = %v", pending)
	}
	// A version this build does not ship = a newer build already ran here.
	if _, err := conn.Exec(`INSERT INTO schema_migrations (version) VALUES ('99991231235959')`); err != nil {
		t.Fatal(err)
	}
	err = RunMigrations(conn, MigrationVars{BootstrapUsername: "boot"})
	if err == nil || !strings.Contains(err.Error(), "ahead of this binary") || !strings.Contains(err.Error(), "99991231235959") {
		t.Errorf("downgrade guard: err = %v", err)
	}
}
