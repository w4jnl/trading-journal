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

// MigrationVars provides values substituted into migration SQL bodies before
// exec. Currently only ${BOOTSTRAP_USERNAME} (the admin user assigned
// ownership of pre-multiuser data) is supported. The value is validated as
// SQL-safe (alphanumeric + a few separators) so the substitution can't be
// used to inject statements via a malicious config.
type MigrationVars struct {
	BootstrapUsername string
}

func (v MigrationVars) apply(sql string) (string, error) {
	if !strings.Contains(sql, "${BOOTSTRAP_USERNAME}") {
		return sql, nil
	}
	if v.BootstrapUsername == "" {
		return "", fmt.Errorf("migration references ${BOOTSTRAP_USERNAME} but no bootstrap username configured")
	}
	for _, r := range v.BootstrapUsername {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
		if !ok {
			return "", fmt.Errorf("bootstrap username %q contains unsafe character %q (allowed: A-Za-z0-9 _-.)", v.BootstrapUsername, r)
		}
	}
	return strings.ReplaceAll(sql, "${BOOTSTRAP_USERNAME}", v.BootstrapUsername), nil
}

// RunMigrations applies any pending migrations from the embedded migrations/ dir.
// Files use dbmate format (-- migrate:up / -- migrate:down). Only the up section
// is executed at runtime; rollbacks are a manual / dev-tooling concern.
func RunMigrations(conn *sql.DB, vars MigrationVars) error {
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
		upSQL, err = vars.apply(upSQL)
		if err != nil {
			return fmt.Errorf("migration %s: %w", path, err)
		}

		if check, ok := preChecks[version]; ok {
			if err := check(conn); err != nil {
				return fmt.Errorf("pre-check %s: %w", path, err)
			}
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

// preChecks runs before applying a specific migration. The check returns an
// error if the migration would fail in a way that's better caught with a
// readable message than as a raw SQL constraint violation.
var preChecks = map[string]func(*sql.DB) error{
	// Before rebuilding commission_rates around (asset_class, firm, size_class):
	// flag any existing rate whose symbol's instrument lacks the new categorization
	// fields. Those rows would be silently dropped by the INSERT … WHERE filter
	// otherwise; surfacing them explicitly lets the user fix the data first.
	"20260512100200": func(conn *sql.DB) error {
		rows, err := conn.Query(`
			SELECT c.firm, c.symbol_root
			FROM commission_rates c
			LEFT JOIN instruments i ON c.symbol_root = i.symbol_root
			WHERE i.symbol_root IS NULL OR i.asset_class IS NULL OR i.size_class IS NULL
			ORDER BY c.firm, c.symbol_root
		`)
		if err != nil {
			return fmt.Errorf("query orphan rates: %w", err)
		}
		defer rows.Close()
		var orphans []string
		for rows.Next() {
			var firm, sym string
			if err := rows.Scan(&firm, &sym); err != nil {
				return err
			}
			orphans = append(orphans, fmt.Sprintf("(%s, %s)", firm, sym))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(orphans) > 0 {
			return fmt.Errorf(
				"commission_rates has %d row(s) whose instrument is uncategorized: %s. "+
					"Either categorize those instruments (UPDATE instruments SET asset_class=…, size_class=…) "+
					"or delete the orphan rate rows, then re-run migrations",
				len(orphans), strings.Join(orphans, ", "),
			)
		}
		return nil
	},
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
