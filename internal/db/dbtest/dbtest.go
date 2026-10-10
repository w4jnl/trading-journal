// Package dbtest provides the shared migrated-SQLite bootstrap for tests.
// It exists so each package's tests don't re-implement the same
// t.TempDir() -> db.Open -> db.RunMigrations sequence (importer, positions,
// and handlers all did before this was extracted); a change to the migration
// bootstrap now lands in one place.
package dbtest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/w4jnl/trading-journal/internal/db"
)

// Open creates a throwaway SQLite database under t.TempDir(), applies every
// embedded migration with the given bootstrap username, and registers the
// connection for cleanup. Fails the test on any error.
func Open(t *testing.T, bootstrapUsername string) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.RunMigrations(conn, db.MigrationVars{BootstrapUsername: bootstrapUsername}); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return conn
}

// OpenWithReader is Open plus a read-only pool on the same file
// (db.OpenReader), for tests that must prove a read path never writes.
func OpenWithReader(t *testing.T, bootstrapUsername string) (writer, reader *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	writer, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := db.RunMigrations(writer, db.MigrationVars{BootstrapUsername: bootstrapUsername}); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	reader, err = db.OpenReader(path)
	if err != nil {
		t.Fatalf("db.OpenReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return writer, reader
}

// BootstrapUserID returns the id of the bootstrap user the migrations created
// for the given username. Fails the test if the row is missing.
func BootstrapUserID(t *testing.T, conn *sql.DB, username string) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(`SELECT id FROM users WHERE username = ?`, username).Scan(&id); err != nil {
		t.Fatalf("load bootstrap user %q: %v", username, err)
	}
	return id
}
