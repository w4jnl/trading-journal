package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// PurgeUserOptions points at the on-disk roots so PurgeUser can sweep the
// user's filesystem footprint as part of the wipe. Any field may be empty
// to skip that particular cleanup.
type PurgeUserOptions struct {
	// Username of the user being purged; used as the subdir name under
	// the imports/* roots. Empty -> skip filesystem cleanup of inbox /
	// processed / failed.
	Username string
	// AttachmentsDir is the on-disk root for journal attachment files
	// (typically <database dir>/attachments). PurgeUser collects the
	// user's attachment paths before the DB wipe and best-effort
	// removes the files after the tx commits. Empty -> skip.
	AttachmentsDir string
	// InboxDir / ProcessedDir / FailedDir / UploadsDir are the four
	// imports/* roots. The first three are wiped at <root>/<username>/;
	// UploadsDir is wiped at <root>/<userID>/ (UI uploads are namespaced
	// by id, not name).
	InboxDir     string
	ProcessedDir string
	FailedDir    string
	UploadsDir   string
}

// PurgeUser deletes the given user and every row that belongs to them:
// trades, executions, journal entries, balance anchors, daily stats,
// sessions, custom journal templates, and accounts. sync_log rows with
// user_id = this user get their user_id set to NULL (the column is
// ON DELETE SET NULL) so audit history is preserved as anonymous events.
//
// When opts specifies on-disk roots, also removes:
//   - every attachment file referenced by the user's journal entries
//   - imports/inbox/<username>/, imports/processed/<username>/,
//     imports/failed/<username>/
//   - imports/uploads/<userID>/
//
// The wipe runs in a single transaction. accounts has NO ACTION FKs from
// trades / executions / journal_entries / daily_stats, so we explicitly
// delete the leaf rows first before the accounts -> users cascade fires.
//
// Returns counts so the caller can show a useful confirmation.
type PurgeStats struct {
	Accounts        int64
	Trades          int64
	Attachments     int64 // attachment FILES removed (DB rows are wiped too)
	DirsRemoved     int64
	FailedFiles     int64 // count of files we couldn't unlink; check logs
	FailedDirs      int64
}

func PurgeUser(ctx context.Context, db *sql.DB, userID int64, opts PurgeUserOptions) (PurgeStats, error) {
	var stats PurgeStats

	// Pre-count for the confirmation message.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM accounts WHERE user_id = ?`, userID).Scan(&stats.Accounts); err != nil {
		return stats, fmt.Errorf("count accounts: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trades WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
		userID).Scan(&stats.Trades); err != nil {
		return stats, fmt.Errorf("count trades: %w", err)
	}

	// Collect attachment file paths BEFORE the DB wipe so we can remove
	// the on-disk blobs after the tx commits. The rows themselves are
	// deleted inside the tx; if the file unlink fails it's logged but
	// doesn't fail the purge (we don't want to leave the user half
	// deleted in the DB because a disk write blipped).
	var attachmentPaths []string
	if opts.AttachmentsDir != "" {
		rows, err := db.QueryContext(ctx,
			`SELECT a.file_path FROM journal_attachments a
			 WHERE a.entry_id IN (SELECT id FROM journal_entries WHERE user_id = ?)`,
			userID)
		if err != nil {
			return stats, fmt.Errorf("list attachments: %w", err)
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return stats, fmt.Errorf("scan attachment: %w", err)
			}
			attachmentPaths = append(attachmentPaths, p)
		}
		rows.Close()
	}

	// Disable foreign_keys for the transaction. accounts has NO ACTION
	// FKs from trades / executions / journal_entries / daily_stats, and
	// trades has NO ACTION FKs from journal_entries.trade_id and
	// executions.trade_id - any explicit delete order would still trip
	// at least one of these. Same pattern the migration runner uses for
	// table rebuilds. foreign_key_check after the wipe catches any
	// referential leftover before COMMIT.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return stats, fmt.Errorf("disable foreign_keys: %w", err)
	}
	defer func() { _, _ = db.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`) }()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	steps := []string{
		// Per-trade dependents first.
		`DELETE FROM trade_executions
		   WHERE trade_id IN (SELECT id FROM trades WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?))`,
		`DELETE FROM journal_entry_trades
		   WHERE trade_id IN (SELECT id FROM trades WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?))`,
		// Journal-entry attachments by parent entry.
		`DELETE FROM journal_attachments
		   WHERE entry_id IN (SELECT id FROM journal_entries WHERE user_id = ?)`,
		// Group M:N rows owned by this user's group entries (catches
		// links to trades the user doesn't own but that were attached
		// to a group entry the user owns).
		`DELETE FROM journal_entry_trades
		   WHERE journal_entry_id IN (SELECT id FROM journal_entries WHERE user_id = ?)`,
		// Journal entries (any scope).
		`DELETE FROM journal_entries WHERE user_id = ?`,
		// Per-account leaf tables.
		`DELETE FROM trades       WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
		`DELETE FROM executions   WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
		`DELETE FROM daily_stats  WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
		`DELETE FROM balance_anchors WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
		// Accounts and per-user templates.
		`DELETE FROM accounts          WHERE user_id = ?`,
		`DELETE FROM journal_templates WHERE user_id = ?`,
		// sync_log.user_id is ON DELETE SET NULL in the schema, but
		// foreign_keys = OFF (set above) means the implicit trigger
		// doesn't fire. NULL the column explicitly here so audit
		// history survives anonymized.
		`UPDATE sync_log SET user_id = NULL WHERE user_id = ?`,
		// Sessions and finally the user.
		`DELETE FROM sessions WHERE user_id = ?`,
		`DELETE FROM users    WHERE id = ?`,
	}
	for i, stmt := range steps {
		if _, err := tx.ExecContext(ctx, stmt, userID); err != nil {
			return stats, fmt.Errorf("step %d: %w", i, err)
		}
	}

	// Confirm we didn't leave dangling references somewhere before
	// committing. With FKs off during the deletes, a buggy purge could
	// otherwise quietly produce orphans.
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return stats, fmt.Errorf("foreign_key_check: %w", err)
	}
	var violations []string
	for rows.Next() {
		var table, parent string
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			rows.Close()
			return stats, err
		}
		violations = append(violations, fmt.Sprintf("%s row %d -> %s", table, rowid.Int64, parent))
	}
	rows.Close()
	if len(violations) > 0 {
		return stats, fmt.Errorf("foreign_key_check found %d violation(s) after purge: %v", len(violations), violations)
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("commit: %w", err)
	}

	// DB wipe succeeded. Now best-effort sweep the filesystem footprint.
	// Failures here log but don't fail the call: the source of truth (the
	// DB) is consistent, and orphan files can be cleaned up later.
	stats.Attachments, stats.FailedFiles = removeAttachmentFiles(opts.AttachmentsDir, attachmentPaths)
	if opts.Username != "" {
		for _, root := range []string{opts.InboxDir, opts.ProcessedDir, opts.FailedDir} {
			if root == "" {
				continue
			}
			ok := removeDir(filepath.Join(root, opts.Username))
			if ok {
				stats.DirsRemoved++
			} else {
				stats.FailedDirs++
			}
		}
	}
	if opts.UploadsDir != "" {
		ok := removeDir(filepath.Join(opts.UploadsDir, strconv.FormatInt(userID, 10)))
		if ok {
			stats.DirsRemoved++
		} else {
			stats.FailedDirs++
		}
	}
	return stats, nil
}

// removeAttachmentFiles unlinks each path (relative to root) and returns
// (removed, failed). Files that don't exist are counted as removed -
// idempotent cleanup. A path that resolves outside root is refused so a
// poisoned DB row can't make us delete outside the attachments tree.
func removeAttachmentFiles(root string, paths []string) (removed, failed int64) {
	if root == "" {
		return 0, 0
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		slog.Warn("attachments root abs", "root", root, "err", err)
		return 0, int64(len(paths))
	}
	for _, p := range paths {
		full, err := filepath.Abs(filepath.Join(root, p))
		if err != nil {
			slog.Warn("attachment abs", "path", p, "err", err)
			failed++
			continue
		}
		// Containment check: never delete outside the attachments root.
		if full != rootAbs && !startsWithDir(full, rootAbs) {
			slog.Warn("attachment outside root; skipping", "path", full, "root", rootAbs)
			failed++
			continue
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove attachment", "path", full, "err", err)
			failed++
			continue
		}
		removed++
	}
	return removed, failed
}

// startsWithDir is the strict version of HasPrefix for paths: makes sure
// /foo/barbaz isn't accepted as "inside" /foo/bar.
func startsWithDir(p, dir string) bool {
	if p == dir {
		return true
	}
	return len(p) > len(dir) && p[:len(dir)] == dir && p[len(dir)] == filepath.Separator
}

// removeDir best-effort removes a directory tree. Missing dirs are treated
// as success. Returns false on any error.
func removeDir(dir string) bool {
	if dir == "" {
		return true
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}
		slog.Warn("stat user dir", "dir", dir, "err", err)
		return false
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("remove user dir", "dir", dir, "err", err)
		return false
	}
	return true
}
