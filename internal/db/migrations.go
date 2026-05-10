package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies any pending migrations from the embedded migrations/ dir.
// Files use dbmate format (-- migrate:up / -- migrate:down). Only the up section
// is executed at runtime; rollbacks are a manual / dev-tooling concern.
func RunMigrations(conn *sql.DB) error {
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(files)

	applied, err := loadAppliedVersions(conn)
	if err != nil {
		return err
	}

	for _, path := range files {
		version := versionFromFilename(path)
		if version == "" {
			return fmt.Errorf("migration %s: cannot extract version from filename", path)
		}
		if applied[version] {
			continue
		}

		raw, err := migrationsFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		upSQL, err := extractUp(string(raw))
		if err != nil {
			return fmt.Errorf("migration %s: %w", path, err)
		}

		if err := applyOne(conn, version, upSQL); err != nil {
			return fmt.Errorf("apply %s: %w", path, err)
		}
		slog.Info("migration applied", "version", version, "file", path)
	}
	return nil
}

func loadAppliedVersions(conn *sql.DB) (map[string]bool, error) {
	rows, err := conn.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func applyOne(conn *sql.DB, version, upSQL string) error {
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(upSQL); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// versionFromFilename returns the leading numeric prefix of a migration filename.
// "20260510120000_create_accounts.sql" → "20260510120000".
func versionFromFilename(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.Index(base, "_"); i >= 0 {
		return base[:i]
	}
	return ""
}

// extractUp returns the SQL between `-- migrate:up` and `-- migrate:down` markers.
// If the file has no markers, the whole content is treated as the up section.
func extractUp(content string) (string, error) {
	const upMarker = "-- migrate:up"
	const downMarker = "-- migrate:down"

	upIdx := strings.Index(content, upMarker)
	if upIdx < 0 {
		return strings.TrimSpace(content), nil
	}
	body := content[upIdx+len(upMarker):]
	if i := strings.Index(body, downMarker); i >= 0 {
		body = body[:i]
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("empty up section")
	}
	return body, nil
}
