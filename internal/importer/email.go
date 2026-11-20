package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
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
	EmailSkippedAddress     = "skipped_address_mismatch"  // strict addresses: sent to one user's fill address, account owned by another
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

	candidates, err := q.ListAccountsByRefAnyUser(ctx, models.ListAccountsByRefAnyUserParams{
		ImportSource: string(FormatMotiveWave),
		Ref:          f.AccountRef,
	})
	if err != nil {
		return "", fmt.Errorf("resolve account %s: %w", f.AccountRef, err)
	}
	// Prefer exact account_ref matches over email_ref alias matches, so an
	// alias that happens to collide with another account's real ref can
	// never steal its fills. Ambiguity within the winning tier cannot be
	// attributed and is skipped.
	var owners []models.Account
	for _, a := range candidates {
		if a.AccountRef == f.AccountRef {
			owners = append(owners, a)
		}
	}
	if len(owners) == 0 {
		owners = candidates
	}
	switch {
	case len(owners) == 0:
		created, ok, err := s.createAccountFromEmail(ctx, q, f)
		if err != nil {
			return "", fmt.Errorf("create account %s: %w", f.AccountRef, err)
		}
		if !ok {
			s.logger().Warn("mw email ingest: unknown account ref and no user to own it, fill stays archive-only until a CSV import or a manual account creates it",
				"ref", f.AccountRef, "order_id", f.OrderID, "to", f.Recipient)
			return EmailSkippedUnknownAcct, nil
		}
		owners = []models.Account{created}
	case len(owners) > 1:
		s.logger().Warn("mw email ingest: account ref owned by multiple users, cannot attribute",
			"ref", f.AccountRef, "owners", len(owners))
		return EmailSkippedAmbiguous, nil
	}
	acct := owners[0]
	// The address only decides NEW accounts; a ref someone owns stays theirs
	// whoever the mail was addressed to, so a mis-set MotiveWave can never
	// move fills between traders. The mismatch is noted for the Accounts
	// card, and with strict addresses on the fill is refused instead.
	if f.Recipient != "" {
		if users, err := q.ListUsers(ctx); err == nil {
			if to := UserForFillAddress(users, f.Recipient); to != 0 && to != acct.UserID {
				if s.MWStrictAddresses {
					s.logger().Warn("mw email ingest: fill addressed to one user's fill address for an account another user owns; strict addresses on, not booked",
						"ref", f.AccountRef, "order_id", f.OrderID, "addressed_user_id", to, "owner_user_id", acct.UserID)
					return EmailSkippedAddress, nil
				}
				s.noteAddressMismatch(f, to, acct.UserID)
			}
		}
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
	// Reaching this commit means the fill was inserted or updated (every
	// skip path returns earlier), so the bump is naturally changes-only.
	// Attributed to the resolved account OWNER, same as the builder above.
	s.bumpVersion(acct.UserID)
	return action, nil
}

// createAccountFromEmail books a fill mail's unknown account ref as a new
// account, the way a CSV import creates one, so the fills land without an
// export. The mail names no journal user, so the owner is the active user
// whose e-mail address is the message's recipient, else the only active
// user; with neither there is no owner and the fill stays archive-only
// (ok=false). The row is created with needs_setup so the trader confirms
// it on /accounts, which lists such accounts first; the import source is
// the CSV's so the account's first export matches the same row.
func (s *ImportService) createAccountFromEmail(ctx context.Context, q *models.Queries, f Fill) (models.Account, bool, error) {
	users, err := q.ListUsers(ctx)
	if err != nil {
		return models.Account{}, false, fmt.Errorf("list users: %w", err)
	}
	owner := ownerForRecipient(users, f.Recipient)
	if owner == 0 {
		return models.Account{}, false, nil
	}
	p := EmailAccountParams(owner, f.AccountRef, f.Source)
	created, err := q.CreateAccount(ctx, p)
	if err != nil {
		return models.Account{}, false, err
	}
	if err := SeedOpeningBalance(ctx, q, s.Timezone, created.ID, f.ExecTime); err != nil {
		return models.Account{}, false, fmt.Errorf("opening balance: %w", err)
	}
	s.logger().Info("mw email ingest: created an account from a fill mail; confirm it on /accounts",
		"ref", f.AccountRef, "user_id", owner, "firm", p.Firm, "to", f.Recipient)
	return created, true, nil
}

// EmailAccountParams is the account a MotiveWave fill mail creates for an
// unknown ref: named after the ref, firm guessed from the ref and the mail's
// Broker label, type Prop Eval, flagged needs_setup so the trader confirms it
// on /accounts, and the CSV's import source so the first export matches the
// same row. Shared by the mail path and the "Create as mine" button on the
// admin card, so both make the identical account.
func EmailAccountParams(userID int64, ref, source string) models.CreateAccountParams {
	defaultType := "prop_eval"
	return models.CreateAccountParams{
		UserID:           userID,
		Name:             ref,
		Firm:             strPtrOrNil(GuessFirm(FormatMotiveWave, ref, source)),
		ImportSource:     string(FormatMotiveWave),
		AccountRef:       ref,
		Type:             &defaultType,
		Currency:         "USD",
		NeedsSetup:       1,
		CommissionSource: "rate",
	}
}

// ownerForRecipient picks the user a fill mail belongs to: the active user
// whose e-mail address is the recipient, else the only active user, else 0.
func ownerForRecipient(users []models.User, recipient string) int64 {
	addr := strings.TrimSpace(recipient)
	if a, err := mail.ParseAddress(addr); err == nil {
		addr = a.Address
	}
	var active []models.User
	for _, u := range users {
		if u.IsActive == 1 {
			active = append(active, u)
		}
	}
	if addr != "" {
		// The generated fill address is the deliberate signal; the user's own
		// e-mail is the older convention and still honoured.
		for _, u := range active {
			if u.MwFillAddress != nil && strings.EqualFold(strings.TrimSpace(*u.MwFillAddress), addr) {
				return u.ID
			}
		}
		for _, u := range active {
			if u.Email != nil && strings.EqualFold(strings.TrimSpace(*u.Email), addr) {
				return u.ID
			}
		}
	}
	if len(active) == 1 {
		return active[0].ID
	}
	return 0
}

// EmailAddressMismatch is a fill mail that arrived at one user's fill
// address for an account another user owns, and was booked on that account
// (strict addresses off). Kept in memory since the process started, one per
// account ref, for the Accounts card: it is a note about the current
// wiring, and the next fill raises it again after a restart.
type EmailAddressMismatch struct {
	AccountRef      string
	AddressedUserID int64 // whose fill address the mail went to
	OwnerUserID     int64 // who owns the account the fill was booked on
	Count           int
	LastSeen        time.Time
}

func (s *ImportService) noteAddressMismatch(f Fill, addressed, owner int64) {
	s.mismatchMu.Lock()
	defer s.mismatchMu.Unlock()
	if s.mismatches == nil {
		s.mismatches = map[string]*EmailAddressMismatch{}
	}
	m := s.mismatches[f.AccountRef]
	if m == nil {
		m = &EmailAddressMismatch{AccountRef: f.AccountRef}
		s.mismatches[f.AccountRef] = m
	}
	m.AddressedUserID, m.OwnerUserID = addressed, owner
	m.Count++
	if f.ExecTime.After(m.LastSeen) {
		m.LastSeen = f.ExecTime
	}
	s.logger().Warn("mw email ingest: fill addressed to one user's fill address for an account another user owns; the account's owner keeps it",
		"ref", f.AccountRef, "order_id", f.OrderID, "addressed_user_id", addressed, "owner_user_id", owner)
}

// EmailAddressMismatches lists the mismatches noted since start, by ref.
func (s *ImportService) EmailAddressMismatches() []EmailAddressMismatch {
	s.mismatchMu.Lock()
	defer s.mismatchMu.Unlock()
	out := make([]EmailAddressMismatch, 0, len(s.mismatches))
	for _, m := range s.mismatches {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountRef < out[j].AccountRef })
	return out
}

// UserForFillAddress is the active user whose generated fill address is the
// recipient, or 0: the strict form of ownerForRecipient, used to notice a
// fill addressed to one trader for an account another trader owns.
func UserForFillAddress(users []models.User, recipient string) int64 {
	addr := strings.TrimSpace(recipient)
	if a, err := mail.ParseAddress(addr); err == nil {
		addr = a.Address
	}
	if addr == "" {
		return 0
	}
	for _, u := range users {
		if u.IsActive == 1 && u.MwFillAddress != nil && strings.EqualFold(strings.TrimSpace(*u.MwFillAddress), addr) {
			return u.ID
		}
	}
	return 0
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
