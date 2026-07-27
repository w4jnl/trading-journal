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
	EmailInserted           = "inserted"              // new provisional execution created
	EmailUpdated            = "updated"               // provisional snapshot replaced (cumulative partial-fill email)
	EmailUnchanged          = "unchanged"             // duplicate content for an order we already hold
	EmailSkippedAuthorative = "skipped_authoritative" // a CSV/API row already covers this order
	EmailSkippedTombstoned  = "skipped_tombstoned"    // user deleted the provisional row; email must not resurrect it
	EmailSkippedLiveSync    = "skipped_live_sync"     // account is API-synced (IBKR); email is capture-only for it
	EmailSkippedInline      = "skipped_inline"        // trades-CSV account; executions are synthetic there
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
// f.OrderID is required. f.BrokerExecID should be the synthetic
// "MWEMAIL-<order id>" so it can never collide with a CSV ticket.
func (s *ImportService) IngestEmailFill(ctx context.Context, userID int64, f Fill) (string, error) {
	if f.OrderID == "" {
		return "", errors.New("email fill has no order id")
	}
	if userID != 0 {
		ctx = withUserScope(ctx, userID, "")
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	acct, created, err := s.findOrCreateAccount(ctx, q, FormatMotiveWave, f.AccountRef, f.Source)
	if err != nil {
		return "", fmt.Errorf("account %s: %w", f.AccountRef, err)
	}
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
	builder := &positions.Builder{Queries: q, Timezone: tz, UserID: s.effectiveUserID(ctx)}
	if _, err := builder.Rebuild(ctx, []positions.Key{{AccountID: acct.ID, Symbol: rebuildSymbol}}); err != nil {
		return "", fmt.Errorf("rebuild %d/%s: %w", acct.ID, rebuildSymbol, err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	if created {
		s.logger().Info("mw email ingest auto-created account", "ref", f.AccountRef)
	}
	s.enqueuePrefetch(ctx, acct.ID)
	return action, nil
}
