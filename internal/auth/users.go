package auth

import (
	"context"
	"database/sql"
	"fmt"
)

// PurgeUser deletes the given user and every row that belongs to them:
// trades, executions, journal entries, balance anchors, daily stats,
// sessions, custom journal templates, and accounts. sync_log rows with
// user_id = this user get their user_id set to NULL (the column is
// ON DELETE SET NULL) so audit history is preserved as anonymous events.
//
// The wipe runs in a single transaction. accounts has NO ACTION FKs from
// trades / executions / journal_entries / daily_stats, so we explicitly
// delete the leaf rows first before the accounts -> users cascade fires.
//
// Returns the number of accounts and trades that were removed so the
// caller can show a useful confirmation.
type PurgeStats struct {
	Accounts int64
	Trades   int64
}

func PurgeUser(ctx context.Context, db *sql.DB, userID int64) (PurgeStats, error) {
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
	return stats, nil
}
