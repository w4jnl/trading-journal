package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open is the WRITER pool: one connection, so the importer, the sync
// workers and the HTTP handlers never race each other into SQLITE_BUSY and
// the per-connection PRAGMA toggles (foreign_keys off during a migration or
// a user purge) are guaranteed to apply to the connection that then begins
// the transaction. Every read on this pool queues behind every write and
// every other read - page renders belong on OpenReader.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is a single-writer engine; serialize all writes through one
	// connection so the importer and the HTTP handlers don't trip
	// SQLITE_BUSY despite the 5s busy_timeout. (This also serializes reads
	// on this pool - WAL concurrency only helps across connections, which
	// is what OpenReader adds.)
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return conn, nil
}

// OpenReader is a second, read-only pool on the same file for page renders
// and API reads. WAL lets any number of readers run alongside the writer,
// each seeing the last committed state; before this pool every SELECT
// waited for imports, syncs and each other on the one writer connection.
// query_only makes a misrouted write fail loudly ("attempt to write a
// readonly database") instead of racing the writer.
func OpenReader(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=query_only(1)", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite reader: %w", err)
	}
	conn.SetMaxOpenConns(4)
	conn.SetMaxIdleConns(4)
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping sqlite reader: %w", err)
	}
	return conn, nil
}
