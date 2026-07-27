package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// Email ingest actions, reported back to the SMTP layer for logging. The SMTP
// reply is 250 regardless - these only describe what happened to the data.
const (
	EmailInserted           = "inserted"                  // new provisional execution created
	EmailUpdated            = "updated"                   // provisional snapshot replaced (cumulative partial-fill email)
	EmailUnchanged          = "unchanged"                 // duplicate content for an order we already hold
	EmailSkippedAuthorative = "skipped_authoritative"     // a CSV/API row already covers this order
	EmailSkippedTombstoned  = "skipped_tombstoned"        // user deleted the provisional row; email must not resurrect it
	EmailSkippedLiveSync    = "skipped_live_sync"         // account is API-synced (IBKR); email is capture-only for it
	EmailSkippedInline      = "skipped_inline"            // trades-CSV account; executions are synthetic there
	EmailSkippedMatched     = "skipped_matched"           // an order-id-less manual/old-CSV row already records this fill
	EmailSkippedStale       = "skipped_stale"             // older cumulative snapshot than the row we hold (archive replay)
	EmailSkippedUnknownAcct = "skipped_unknown_account"   // no user owns this account ref yet; first CSV import creates it
	EmailSkippedAmbiguous   = "skipped_ambiguous_account" // more than one user owns the ref; cannot attribute
)

// IngestEmailFill upserts one PROVISIONAL execution from a MotiveWave
// order-fill notification email.
//
// MW emails one message per fill event, but each message reports the order's
// CUMULATIVE filled quantity and running average price (verified against real
// partial fills: a 3-lot filling 1+2 produces "Quantity: 1" then
// "Quantity: 3" under the same Order ID). So the unit of storage is the
// ORDER, keyed (account_id, order_id): the first email inserts a row, later
// emails for the same order replace it in place. That mirrors the MW
// Executions-CSV shape for Rithmic-connection accounts (one aggregated row
// per order), which is what makes the CSV supersede in ingest() an exact
// 1:1 replacement.
//
// Accounts with live_sync_since set (IBKR gateway polling) are skipped at or
// after the cutover - their fills arrive via the API in a different exec-id
// namespace, and ingesting the email copy would double-count. The email is
// still archived by the capture layer regardless.
//
// User attribution: an email carries no user identity, only the broker
// account ref, so the fill is booked under whichever user ALREADY OWNS the
// (motivewave_csv, ref) account - resolved across all users, since a real
// broker ref belongs to exactly one person. Unknown refs are deliberately
// NOT auto-created (unlike the CSV path, email has no owner signal): the
// fill stays archive-only until the first authoritative CSV import creates
// the account under the right user - and that CSV delivers these very fills
// anyway, so nothing is lost. A ref owned by two users cannot be attributed
// and is skipped with a warning.
//
// f.OrderID is required. f.BrokerExecID should be the synthetic
// "MWEMAIL-<order id>" so it can never collide with a CSV ticket.
func (s *ImportService) IngestEmailFill(ctx context.Context, f Fill) (string, error) {
	if f.OrderID == "" {
		return "", errors.New("email fill has no order id")
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	owners, err := q.ListAccountsByRefAnyUser(ctx, models.ListAccountsByRefAnyUserParams{
		ImportSource: string(FormatMotiveWave),
		AccountRef:   f.AccountRef,
	})
	if err != nil {
		return "", fmt.Errorf("resolve account %s: %w", f.AccountRef, err)
	}
	switch {
	case len(owners) == 0:
		s.logger().Warn("mw email ingest: unknown account ref, fill stays archive-only until its first CSV import",
			"ref", f.AccountRef, "order_id", f.OrderID)
		return EmailSkippedUnknownAcct, nil
	case len(owners) > 1:
		s.logger().Warn("mw email ingest: account ref owned by multiple users, cannot attribute",
			"ref", f.AccountRef, "owners", len(owners))
		return EmailSkippedAmbiguous, nil
	}
	acct := owners[0]
	if acct.CommissionSource == "inline" {
		return EmailSkippedInline, tx.Commit()
	}
	if cut := acct.LiveSyncSince; cut != nil && !f.ExecTime.Before(*cut) {
		return EmailSkippedLiveSync, tx.Commit()
	}

	if _, err := upsertInstrumentStub(ctx, tx, q, f); err != nil {
		return "", fmt.Errorf("instrument %s: %w", f.SymbolRoot, err)
	}

	oid := f.OrderID
	action := ""
	rebuildSymbol := f.Symbol
	existing, err := q.GetExecutionByOrderID(ctx, models.GetExecutionByOrderIDParams{
		AccountID: acct.ID,
		OrderID:   &oid,
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No row knows this order id. Before inserting, guard against fills
		// already recorded WITHOUT one - hand-entered rows and old-format CSV
		// imports - by matching (symbol, side, qty, price, time). Scoped to
		// order-id-less rows on purpose: anything with an order id dedups
		// exactly above, and a same-second same-price fill from a DIFFERENT
		// order would carry its own id and must not be swallowed here.
		matched, err := manualRowMatches(ctx, q, acct.ID, f)
		if err != nil {
			return "", fmt.Errorf("manual-row guard: %w", err)
		}
		if matched {
			return EmailSkippedMatched, tx.Commit()
		}
		n, err := q.InsertExecution(ctx, models.InsertExecutionParams{
			AccountID:    acct.ID,
			BrokerExecID: f.BrokerExecID,
			OrderID:      &oid,
			ImportMethod: "mw_email",
			Symbol:       f.Symbol,
			SymbolRoot:   f.SymbolRoot,
			Side:         f.Side,
			Qty:          f.Qty,
			Price:        f.Price,
			ExecTime:     f.ExecTime,
			RawData:      strPtrOrNil(f.RawData),
		})
		if err != nil {
			return "", fmt.Errorf("insert: %w", err)
		}
		if n == 0 {
			// broker_exec_id collision without an order_id match should be
			// impossible (MWEMAIL- prefix); treat it as already-covered.
			return EmailUnchanged, tx.Commit()
		}
		action = EmailInserted
	case err != nil:
		return "", fmt.Errorf("lookup order %s: %w", f.OrderID, err)
	case existing.ImportMethod != "mw_email":
		return EmailSkippedAuthorative, tx.Commit()
	case existing.DeletedAt != nil:
		return EmailSkippedTombstoned, tx.Commit()
	case existing.Side == f.Side && existing.Qty == f.Qty && existing.Price == f.Price &&
		existing.ExecTime.Equal(f.ExecTime):
		return EmailUnchanged, tx.Commit()
	case f.ExecTime.Before(existing.ExecTime),
		f.ExecTime.Equal(existing.ExecTime) && f.Qty < existing.Qty:
		// An OLDER cumulative snapshot than the row we hold - the archive
		// replay walks oldest-first over messages the live path already
		// applied. Rewinding and re-advancing would churn rows and FIFO
		// rebuilds on every boot; snapshots only ever move forward.
		return EmailSkippedStale, tx.Commit()
	default:
		if err := q.UpdateProvisionalExecution(ctx, models.UpdateProvisionalExecutionParams{
			Side:     f.Side,
			Qty:      f.Qty,
			Price:    f.Price,
			ExecTime: f.ExecTime,
			RawData:  strPtrOrNil(f.RawData),
			ID:       existing.ID,
		}); err != nil {
			return "", fmt.Errorf("update provisional %d: %w", existing.ID, err)
		}
		// The symbol is pinned by the first email for the order; rebuild that
		// FIFO queue even if the incoming message spells the contract oddly.
		rebuildSymbol = existing.Symbol
		action = EmailUpdated
	}

	tz := s.Timezone
	if tz == nil {
		tz = time.UTC
	}
	// The builder is scoped to the resolved OWNER, not the bootstrap user -
	// this is what books the trade under the right person in multi-user
	// deployments.
	builder := &positions.Builder{Queries: q, Timezone: tz, UserID: acct.UserID}
	if _, err := builder.Rebuild(ctx, []positions.Key{{AccountID: acct.ID, Symbol: rebuildSymbol}}); err != nil {
		return "", fmt.Errorf("rebuild %d/%s: %w", acct.ID, rebuildSymbol, err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	s.enqueuePrefetch(ctx, acct.ID)
	return action, nil
}

// manualRowMatchWindow absorbs the precision mismatch between MW's email time
// (whole seconds, reconstructed from the Date header) and manually entered or
// old-CSV times (typed to the second, or millisecond-precise).
const manualRowMatchWindow = 2 * time.Second

// manualRowMatches reports whether a non-email execution WITHOUT an order id
// already records this fill on the account: same contract, side, qty, price,
// and an exec time within manualRowMatchWindow. Times are compared in Go
// rather than SQL to sidestep stored-datetime string-format comparisons.
func manualRowMatches(ctx context.Context, q *models.Queries, accountID int64, f Fill) (bool, error) {
	rows, err := q.ListUnorderedExecsForAccountSymbol(ctx, models.ListUnorderedExecsForAccountSymbolParams{
		AccountID: accountID,
		Symbol:    f.Symbol,
	})
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Side != f.Side || r.Qty != f.Qty || r.Price != f.Price {
			continue
		}
		d := r.ExecTime.Sub(f.ExecTime)
		if d < 0 {
			d = -d
		}
		if d <= manualRowMatchWindow {
			return true, nil
		}
	}
	return false, nil
}
