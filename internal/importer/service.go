package importer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ImportService ingests CSV exports into the database.
//
// One ImportFile call:
//  1. Reads the file fully into memory.
//  2. Detects format from the header row.
//  3. Parses to []Fill.
//  4. In a single transaction: upserts the necessary accounts and instrument
//     stubs, inserts executions with INSERT-OR-IGNORE.
//  5. Writes a sync_log row.
//  6. Moves the source file out of inbox/: success → ProcessedDir, failure →
//     FailedDir (with a .error.txt sidecar). The move always happens so files
//     never silently vanish.
type ImportService struct {
	DB           *sql.DB
	Queries      *models.Queries
	ProcessedDir string
	FailedDir    string
	Timezone     *time.Location // used by the position builder for daily_stats date bucketing
	Logger       *slog.Logger

	// UserID owns any account auto-created during import and is recorded
	// on sync_log rows. Phase 1b: all imports are attributed to a single
	// user (the stub authenticator's bootstrap user). Phase 1c will thread
	// per-request / per-inbox-subdir ownership through Import* methods.
	UserID int64
}

// Outcome summarizes what an ImportFile call did.
type Outcome struct {
	SourcePath         string // original path that was passed in (no longer exists after move)
	Format             Format // detected format; FormatUnknown means dispatch failed
	FillsParsed        int    // rows successfully parsed (for TopstepX, only Filled rows)
	ExecsInserted      int    // affected-row count from INSERT
	AccountsCreated    int    // new accounts auto-created during this import
	InstrumentsStubbed int    // unknown symbol roots auto-created with needs_setup=1
	TradesCreated      int    // closed trades materialized by the position builder
	SyncLogID          int64  // id of the sync_log row written
	DestinationPath    string // post-move location of the file
	ErrorMessage       string // empty on success
}

// ImportFile reads, parses, ingests, and relocates a single CSV file.
// Account discovery for trade-shaped formats happens from the filename.
func (s *ImportService) ImportFile(ctx context.Context, path string) (Outcome, error) {
	return s.importFile(ctx, path, 0)
}

// ImportFileToAccount is the explicit-account variant used by the /import
// UI's account picker — caller supplies the target account_id, bypassing
// filename-based resolution. Used only for trade-shaped formats; for
// fill-shaped formats (MotiveWave, TopstepX orders) the account ID is
// embedded per-row and this parameter is ignored.
func (s *ImportService) ImportFileToAccount(ctx context.Context, path string, accountID int64) (Outcome, error) {
	return s.importFile(ctx, path, accountID)
}

// importFile is the shared body. forceAccountID=0 means "use the filename-
// based resolver"; >0 means "ingest into this exact account".
func (s *ImportService) importFile(ctx context.Context, path string, forceAccountID int64) (Outcome, error) {
	out := Outcome{SourcePath: path}

	content, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("read %s: %w", path, err)
	}

	out.Format = DetectFormat(content)
	if out.Format == FormatUnknown {
		out.ErrorMessage = "unrecognized CSV header (neither MotiveWave nor TopstepX orders/trades)"
		return s.finalizeFailure(ctx, path, out)
	}

	// Branch by shape: fill-based formats go through FIFO; trade-based formats
	// write trades directly because the source already aggregated them.
	switch out.Format {
	case FormatMotiveWave, FormatTopstepX:
		fills, err := dispatchParse(out.Format, bytes.NewReader(content))
		if err != nil {
			out.ErrorMessage = fmt.Sprintf("parse: %v", err)
			return s.finalizeFailure(ctx, path, out)
		}
		out.FillsParsed = len(fills)
		if err := s.ingest(ctx, fills, out.Format, &out); err != nil {
			out.ErrorMessage = fmt.Sprintf("ingest: %v", err)
			return s.finalizeFailure(ctx, path, out)
		}
	case FormatTopstepXTrades:
		records, err := ParseTopstepXTrades(bytes.NewReader(content))
		if err != nil {
			out.ErrorMessage = fmt.Sprintf("parse: %v", err)
			return s.finalizeFailure(ctx, path, out)
		}
		out.FillsParsed = len(records)
		if err := s.ingestTrades(ctx, records, path, &out, forceAccountID); err != nil {
			out.ErrorMessage = fmt.Sprintf("ingest: %v", err)
			return s.finalizeFailure(ctx, path, out)
		}
	}

	return s.finalizeSuccess(ctx, path, out)
}

func dispatchParse(format Format, r io.Reader) ([]Fill, error) {
	switch format {
	case FormatMotiveWave:
		return ParseMotiveWave(r)
	case FormatTopstepX:
		return ParseTopstepX(r)
	default:
		return nil, fmt.Errorf("no parser for format %q", format)
	}
}

// ensureClaimCompatible aborts the import if the destination account has
// already been claimed by the other TopStep CSV format. The two TopStep
// CSVs (orders + trades) share import_source = "topstepx_csv" and are
// dedup'd by separate id namespaces, so mixing them on one account would
// double-count. The lock keys off accounts.commission_source ('inline' =
// trades-CSV has run) and the existence of FIFO trades (broker_trade_id
// IS NULL = orders-CSV has run); see GetAccountImportClaim. The guard
// only applies to the TopStep orders/trades pair - MotiveWave accounts
// live in their own (import_source, account_ref) namespace and have no
// equivalent collision risk, so MotiveWave imports skip the check.
func ensureClaimCompatible(ctx context.Context, q *models.Queries, userID, accountID int64, accountName string, incoming Format) error {
	claim, err := q.GetAccountImportClaim(ctx, models.GetAccountImportClaimParams{
		ID:     accountID,
		UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("read import claim: %w", err)
	}
	switch {
	case incoming == FormatTopstepXTrades && claim == "orders":
		return fmt.Errorf("account %q is already orders-imported; delete it from /accounts and re-import to switch to trades-CSV", accountName)
	case incoming == FormatTopstepX && claim == "trades":
		return fmt.Errorf("account %q is already trades-imported; delete it from /accounts and re-import to switch to orders-CSV", accountName)
	}
	return nil
}

// DetectFormat inspects the first line to identify the CSV source. A leading
// UTF-8 BOM is tolerated (TopstepX exports include one).
//
// TopstepX ships two distinct shapes — the legacy orders export and the
// newer per-trade export. We tell them apart by the second header column:
//   Id,AccountName,…  → orders export
//   Id,ContractName,… → trades export
func DetectFormat(content []byte) Format {
	content = stripBOMBytes(content)
	end := bytes.IndexByte(content, '\n')
	if end < 0 {
		end = len(content)
	}
	header := strings.TrimSpace(string(content[:end]))
	if header == "" {
		return FormatUnknown
	}
	cols := strings.Split(header, ",")
	first := strings.TrimSpace(cols[0])
	second := ""
	if len(cols) > 1 {
		second = strings.TrimSpace(cols[1])
	}
	switch first {
	case "Account":
		return FormatMotiveWave
	case "Id":
		switch second {
		case "AccountName":
			return FormatTopstepX
		case "ContractName":
			return FormatTopstepXTrades
		}
	}
	return FormatUnknown
}

// ingest runs all DB writes for one import inside a single transaction.
// After inserting executions, it invokes the position builder to rebuild
// trades and daily_stats for any (account, symbol) pair that got new fills.
func (s *ImportService) ingest(ctx context.Context, fills []Fill, format Format, out *Outcome) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	accounts := map[string]int64{}
	inlineAccounts := map[int64]bool{} // commission_source = 'inline' -> skip FIFO rebuild
	instruments := map[string]bool{}
	rebuildKeys := map[positions.Key]bool{}

	for _, f := range fills {
		if _, seen := instruments[f.SymbolRoot]; !seen {
			created, err := upsertInstrumentStub(ctx, tx, q, f)
			if err != nil {
				return fmt.Errorf("instrument %s: %w", f.SymbolRoot, err)
			}
			if created {
				out.InstrumentsStubbed++
			}
			instruments[f.SymbolRoot] = true
		}

		accountID, ok := accounts[f.AccountRef]
		if !ok {
			acct, created, err := s.findOrCreateAccount(ctx, q, format, f.AccountRef, f.Source)
			if err != nil {
				return fmt.Errorf("account %s: %w", f.AccountRef, err)
			}
			if created {
				out.AccountsCreated++
			}
			if err := ensureClaimCompatible(ctx, q, s.UserID, acct.ID, acct.Name, format); err != nil {
				return err
			}
			accounts[f.AccountRef] = acct.ID
			// commission_source='inline' is only meaningful on TopStep
			// accounts (set by the trades-CSV path to preserve per-trade
			// commissions). On any other account it's a stale-state
			// artifact - e.g., the migration backfill flipped the account
			// after trades were reassigned in from a TopStep account. Don't
			// let that artifact suppress FIFO rebuilds for non-TopStep
			// accounts, or new fills would land with zero trades built.
			if acct.CommissionSource == "inline" && acct.ImportSource == string(FormatTopstepX) {
				inlineAccounts[acct.ID] = true
			}
			accountID = acct.ID
		}

		var raw *string
		if f.RawData != "" {
			r := f.RawData
			raw = &r
		}
		n, err := q.InsertExecution(ctx, models.InsertExecutionParams{
			AccountID:    accountID,
			BrokerExecID: f.BrokerExecID,
			Symbol:       f.Symbol,
			SymbolRoot:   f.SymbolRoot,
			Side:         f.Side,
			Qty:          f.Qty,
			Price:        f.Price,
			ExecTime:     f.ExecTime,
			RawData:      raw,
		})
		if err != nil {
			return fmt.Errorf("insert execution %s/%s: %w", f.AccountRef, f.BrokerExecID, err)
		}
		out.ExecsInserted += int(n)
		if n > 0 {
			rebuildKeys[positions.Key{AccountID: accountID, Symbol: f.Symbol}] = true
		}
	}

	if len(rebuildKeys) > 0 {
		tz := s.Timezone
		if tz == nil {
			tz = time.UTC
		}
		builder := &positions.Builder{Queries: q, Timezone: tz, UserID: s.UserID}
		keys := make([]positions.Key, 0, len(rebuildKeys))
		for k := range rebuildKeys {
			// Inline accounts (TopstepX trades CSV) are the source of truth for
			// their trades; running FIFO over the synthetic execs would replace
			// the CSV-provided commissions with rate-table values.
			if inlineAccounts[k.AccountID] {
				continue
			}
			keys = append(keys, k)
		}
		if len(keys) > 0 {
			res, err := builder.Rebuild(ctx, keys)
			if err != nil {
				return fmt.Errorf("position builder: %w", err)
			}
			out.TradesCreated = res.TradesCreated
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ingestTrades writes pre-aggregated trade rows (TopstepX trades CSV) to
// `trades` directly, with two synthetic executions per trade for audit-trail
// continuity. The FIFO position builder is intentionally skipped — the source
// file already has the entry/exit price, gross P&L, and commission.
//
// Account resolution: TopstepX trades CSV files don't carry an account
// identifier, so we read the filename. Pattern: strip "topstep_" /
// "topstepx_" prefix and "_trades" suffix, then look up by the remainder.
// Fall back to the single existing TopStep account if there is exactly one,
// else create TOPSTEP-DEFAULT (needs_setup=1).
func (s *ImportService) ingestTrades(ctx context.Context, records []TradeRecord, srcPath string, out *Outcome, forceAccountID int64) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	var accountID int64
	if forceAccountID > 0 {
		// Explicit assignment from the UI picker — caller has validated the
		// account exists, so just use it.
		accountID = forceAccountID
	} else {
		var err error
		var created bool
		accountID, created, err = s.resolveTopstepAccount(ctx, q, srcPath)
		if err != nil {
			return fmt.Errorf("resolve account: %w", err)
		}
		if created {
			out.AccountsCreated++
		}
	}
	// Reject if the account is already orders-imported. Done before
	// SetAccountCommissionSource so a rejected import doesn't mutate state.
	acct, err := q.GetAccountByID(ctx, models.GetAccountByIDParams{
		ID:     accountID,
		UserID: s.UserID,
	})
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	if err := ensureClaimCompatible(ctx, q, s.UserID, accountID, acct.Name, FormatTopstepXTrades); err != nil {
		return err
	}
	// Trades CSV is the only source that carries per-trade commissions inline,
	// so the destination account must opt out of rate-based recompute. Idempotent.
	if err := q.SetAccountCommissionSource(ctx, models.SetAccountCommissionSourceParams{
		CommissionSource: "inline",
		ID:               accountID,
		UserID:           s.UserID,
	}); err != nil {
		return fmt.Errorf("mark account inline: %w", err)
	}

	instruments := map[string]bool{}
	for _, rec := range records {
		// Instrument stub upsert — only once per root.
		if _, seen := instruments[rec.SymbolRoot]; !seen {
			f := Fill{SymbolRoot: rec.SymbolRoot, Exchange: ""}
			stubCreated, err := upsertInstrumentStub(ctx, tx, q, f)
			if err != nil {
				return fmt.Errorf("instrument %s: %w", rec.SymbolRoot, err)
			}
			if stubCreated {
				out.InstrumentsStubbed++
			}
			instruments[rec.SymbolRoot] = true
		}

		// Skip if this trade is already imported.
		if _, err := q.GetTradeByBrokerID(ctx, models.GetTradeByBrokerIDParams{
			AccountID:     accountID,
			BrokerTradeID: ptrStr(rec.BrokerTradeID),
		}); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("dedup trade %s: %w", rec.BrokerTradeID, err)
		}

		entrySide, exitSide := sidesForDirection(rec.Direction)

		// Entry execution.
		entryExecID, err := upsertExecutionReturningID(ctx, q, models.InsertExecutionParams{
			AccountID:    accountID,
			BrokerExecID: rec.BrokerTradeID + "-entry",
			Symbol:       rec.Symbol,
			SymbolRoot:   rec.SymbolRoot,
			Side:         entrySide,
			Qty:          rec.Qty,
			Price:        rec.EntryPrice,
			ExecTime:     rec.EntryTime,
			RawData:      strPtrOrNil(rec.RawData),
		})
		if err != nil {
			return fmt.Errorf("entry exec %s: %w", rec.BrokerTradeID, err)
		}

		// Exit execution.
		exitExecID, err := upsertExecutionReturningID(ctx, q, models.InsertExecutionParams{
			AccountID:    accountID,
			BrokerExecID: rec.BrokerTradeID + "-exit",
			Symbol:       rec.Symbol,
			SymbolRoot:   rec.SymbolRoot,
			Side:         exitSide,
			Qty:          rec.Qty,
			Price:        rec.ExitPrice,
			ExecTime:     rec.ExitTime,
			RawData:      nil, // RawData already on the entry side
		})
		if err != nil {
			return fmt.Errorf("exit exec %s: %w", rec.BrokerTradeID, err)
		}

		// Trade row.
		exitPriceVal := rec.ExitPrice
		exitTimeVal := rec.ExitTime
		gross := rec.GrossPnL
		commission := rec.Commission
		net := rec.NetPnL
		trade, err := q.CreateImportedTrade(ctx, models.CreateImportedTradeParams{
			AccountID:     accountID,
			Symbol:        rec.Symbol,
			SymbolRoot:    rec.SymbolRoot,
			Direction:     rec.Direction,
			Qty:           rec.Qty,
			EntryPrice:    rec.EntryPrice,
			ExitPrice:     &exitPriceVal,
			EntryTime:     rec.EntryTime,
			ExitTime:      &exitTimeVal,
			GrossPnl:      &gross,
			Commission:    &commission,
			NetPnl:        &net,
			BrokerTradeID: ptrStr(rec.BrokerTradeID),
		})
		if err != nil {
			return fmt.Errorf("create trade %s: %w", rec.BrokerTradeID, err)
		}
		out.TradesCreated++
		out.ExecsInserted += 2 // approximate; some may have been pre-existing on retry

		// Junction rows.
		if err := q.LinkTradeExecution(ctx, models.LinkTradeExecutionParams{
			TradeID: trade.ID, ExecutionID: entryExecID, Role: "entry", QtyUsed: rec.Qty,
		}); err != nil {
			return fmt.Errorf("link entry: %w", err)
		}
		if err := q.LinkTradeExecution(ctx, models.LinkTradeExecutionParams{
			TradeID: trade.ID, ExecutionID: exitExecID, Role: "exit", QtyUsed: rec.Qty,
		}); err != nil {
			return fmt.Errorf("link exit: %w", err)
		}
	}

	// daily_stats has to refresh too so /calendar and /analytics pick up the
	// new trades. We call the position builder's rebuild path scoped to this
	// account — it rewrites trades for any (account, symbol) pair the user
	// has, INCLUDING those we just inserted. But that would clobber the
	// pre-aggregated trades. Instead, just refresh daily_stats from the
	// authoritative trades table — no FIFO rebuild for trade-imported
	// accounts.
	if err := RefreshDailyStatsForAccount(ctx, q, accountID, s.Timezone); err != nil {
		return fmt.Errorf("refresh daily_stats: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// sidesForDirection turns a trade direction into (entry side, exit side).
// A long trade is entered with a buy and exited with a sell; short, the
// other way around.
func sidesForDirection(direction string) (entry, exit string) {
	if direction == "short" {
		return "sell", "buy"
	}
	return "buy", "sell"
}

// upsertExecutionReturningID inserts the execution (idempotent via INSERT OR
// IGNORE) and returns the row's id by following up with a lookup. Two-step
// because the existing InsertExecution query returns affected rowcount, not
// LastInsertId.
func upsertExecutionReturningID(ctx context.Context, q *models.Queries, p models.InsertExecutionParams) (int64, error) {
	if _, err := q.InsertExecution(ctx, p); err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	row, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID:    p.AccountID,
		BrokerExecID: p.BrokerExecID,
	})
	if err != nil {
		return 0, fmt.Errorf("lookup after insert: %w", err)
	}
	return row.ID, nil
}

// resolveTopstepAccount picks (or creates) the TopStep account a trades CSV
// belongs to. Order of preference:
//   1. Filename is a recognizable account ref → use it (create the account
//      on first sight). Recognized shapes:
//         - starts with "topstep_" / "topstepx_" (user-prefix convention)
//         - or starts with "EXPRESS-" (TopstepX's own ref shape)
//      In both cases the `_trades` suffix is also stripped.
//   2. No recognizable ref. If exactly one TopStep account exists → use it.
//   3. Else create TOPSTEP-DEFAULT with needs_setup=1.
//
// All TopstepX-sourced accounts share `import_source = "topstepx_csv"` so the
// trades export and the legacy orders export converge on the same row when
// they're for the same broker account.
func (s *ImportService) resolveTopstepAccount(ctx context.Context, q *models.Queries, srcPath string) (int64, bool, error) {
	ref := ExtractTopstepAccountRef(srcPath)

	// Pull the candidate set: any TopStep-import-sourced account, plus any
	// account whose firm is explicitly topstep (covers user-renamed cases).
	all, err := q.ListAccounts(ctx, s.UserID)
	if err != nil {
		return 0, false, err
	}
	var topsteppy []models.Account
	for _, a := range all {
		if a.ImportSource == string(FormatTopstepX) ||
			(a.Firm != nil && *a.Firm == "topstep") {
			topsteppy = append(topsteppy, a)
		}
	}

	firm := "topstep"
	defaultType := "prop_eval"

	// 1. Filename ref — find existing or create with that ref.
	if ref != "" {
		for _, a := range topsteppy {
			if a.AccountRef == ref {
				return a.ID, false, nil
			}
		}
		created, err := q.CreateAccount(ctx, models.CreateAccountParams{
			UserID:           s.UserID,
			Name:             ref,
			Firm:             &firm,
			ImportSource:     string(FormatTopstepX),
			AccountRef:       ref,
			Type:             &defaultType,
			Currency:         "USD",
			NeedsSetup:       1,
			CommissionSource: "inline",
		})
		if err != nil {
			return 0, false, err
		}
		return created.ID, true, nil
	}

	// 2. No ref. If exactly one TopStep account exists, use it.
	if len(topsteppy) == 1 {
		return topsteppy[0].ID, false, nil
	}

	// 3. Else find-or-create TOPSTEP-DEFAULT — a stable bucket for files the
	// user didn't tag with an account ref. (If we always tried CREATE, the
	// unique constraint on (import_source, account_ref) would fail on the
	// second generic drop.)
	for _, a := range topsteppy {
		if a.AccountRef == "TOPSTEP-DEFAULT" {
			return a.ID, false, nil
		}
	}
	created, err := q.CreateAccount(ctx, models.CreateAccountParams{
		UserID:           s.UserID,
		Name:             "TOPSTEP-DEFAULT",
		Firm:             &firm,
		ImportSource:     string(FormatTopstepX),
		AccountRef:       "TOPSTEP-DEFAULT",
		Type:             &defaultType,
		Currency:         "USD",
		NeedsSetup:       1,
		CommissionSource: "inline",
	})
	if err != nil {
		return 0, false, err
	}
	return created.ID, true, nil
}

// ExtractTopstepAccountRef recovers a TopstepX account_ref from a filename
// when the user explicitly signaled one. Two recognized conventions:
//
//   1. Prefix: "topstep_<ref>[_trades].csv" or "topstepx_<ref>[_trades].csv"
//      → ref is everything between the prefix and the optional suffix.
//   2. Native shape: "EXPRESS-<rest>[_trades].csv" — TopstepX's own account
//      naming convention as seen in their exports.
//
// Anything else returns "" — we don't try to guess so a generic filename
// like `topstepTrades.csv` doesn't accidentally invent a phantom account.
func ExtractTopstepAccountRef(srcPath string) string {
	base := filepath.Base(srcPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))

	// Prefix convention.
	for _, prefix := range []string{"topstepx_", "topstep_"} {
		if strings.HasPrefix(base, prefix) {
			ref := strings.TrimPrefix(base, prefix)
			ref = strings.TrimSuffix(ref, "_trades")
			ref = strings.TrimSuffix(ref, "-trades")
			if ref != "" && isRefSafe(ref) {
				return ref
			}
			return ""
		}
	}

	// Native EXPRESS- shape.
	if strings.HasPrefix(base, "EXPRESS-") {
		ref := strings.TrimSuffix(base, "_trades")
		ref = strings.TrimSuffix(ref, "-trades")
		if isRefSafe(ref) {
			return ref
		}
	}
	return ""
}

// isRefSafe rejects anything that isn't pure alphanumeric/dash/underscore —
// guards against weird filename characters making it into the DB as an
// account_ref.
func isRefSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || (r >= '0' && r <= '9') ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return true
}

// ptrStr returns a *string pointer, even for the empty string. (strPtrOrNil
// returns nil for empty; for broker_trade_id we want to persist whatever
// value we were given — an empty string would be a real bug we'd want to
// catch in the unique index, not silently turn into NULL.)
func ptrStr(s string) *string { return &s }

// RefreshDailyStatsForAccount rebuilds daily_stats for an account from its
// trades table. Standalone so trade-imported accounts can refresh stats
// without invoking the FIFO position builder, and so other packages
// (e.g. handlers) can refresh after a reassign.
func RefreshDailyStatsForAccount(ctx context.Context, q *models.Queries, accountID int64, tz *time.Location) error {
	if tz == nil {
		tz = time.UTC
	}
	if err := q.DeleteDailyStatsForAccount(ctx, accountID); err != nil {
		return err
	}
	trades, err := q.ListTradesByDate(ctx, accountID)
	if err != nil {
		return err
	}
	type bucket struct {
		count, wins, losses int64
		gross, net, comm    float64
		lw, ll              float64
		hasW, hasL          bool
	}
	groups := map[time.Time]*bucket{}
	var dates []time.Time
	for _, t := range trades {
		if t.ExitTime == nil {
			continue
		}
		y, m, d := t.ExitTime.In(tz).Date()
		key := time.Date(y, m, d, 0, 0, 0, 0, tz)
		b, ok := groups[key]
		if !ok {
			b = &bucket{}
			groups[key] = b
			dates = append(dates, key)
		}
		b.count++
		if t.GrossPnl != nil {
			b.gross += *t.GrossPnl
		}
		if t.Commission != nil {
			b.comm += *t.Commission
		}
		if t.NetPnl == nil {
			continue
		}
		b.net += *t.NetPnl
		switch {
		case *t.NetPnl > 0:
			b.wins++
			if !b.hasW || *t.NetPnl > b.lw {
				b.lw = *t.NetPnl
				b.hasW = true
			}
		case *t.NetPnl < 0:
			b.losses++
			if !b.hasL || *t.NetPnl < b.ll {
				b.ll = *t.NetPnl
				b.hasL = true
			}
		}
	}
	for _, d := range dates {
		b := groups[d]
		var lw, ll *float64
		if b.hasW {
			lw = &b.lw
		}
		if b.hasL {
			ll = &b.ll
		}
		if err := q.CreateDailyStat(ctx, models.CreateDailyStatParams{
			AccountID:   accountID,
			StatDate:    d,
			TradesCount: b.count,
			Wins:        b.wins,
			Losses:      b.losses,
			GrossPnl:    b.gross,
			NetPnl:      b.net,
			Commission:  b.comm,
			LargestWin:  lw,
			LargestLoss: ll,
		}); err != nil {
			return err
		}
	}
	return nil
}

// upsertInstrumentStub inserts a stub row for the instrument if missing. The
// returned bool reports whether a new row was created (we detect this with a
// pre-check since INSERT OR IGNORE doesn't return that information cleanly
// through database/sql).
func upsertInstrumentStub(ctx context.Context, tx *sql.Tx, q *models.Queries, f Fill) (bool, error) {
	if _, err := q.GetInstrument(ctx, f.SymbolRoot); err == nil {
		return false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("lookup: %w", err)
	}
	exchange := strPtrOrNil(f.Exchange)
	if err := q.UpsertInstrumentStub(ctx, models.UpsertInstrumentStubParams{
		SymbolRoot: f.SymbolRoot,
		Exchange:   exchange,
	}); err != nil {
		return false, fmt.Errorf("upsert: %w", err)
	}
	return true, nil
}

func (s *ImportService) findOrCreateAccount(ctx context.Context, q *models.Queries, format Format, ref, source string) (models.Account, bool, error) {
	existing, err := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID:       s.UserID,
		ImportSource: string(format),
		AccountRef:   ref,
	})
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return models.Account{}, false, fmt.Errorf("lookup: %w", err)
	}

	firm := GuessFirm(format, ref, source)
	// Default new accounts to Prop · Eval - most common starting state for a
	// fresh prop-firm account. User can promote to funded/live or change
	// entirely via /accounts. needs_setup clears when firm + type are both
	// set (firm guess can still fail for unrecognized brokers).
	defaultType := "prop_eval"
	needsSetup := int64(0)
	if firm == "" {
		needsSetup = 1
	}
	created, err := q.CreateAccount(ctx, models.CreateAccountParams{
		UserID:           s.UserID,
		Name:             ref,
		Firm:             strPtrOrNil(firm),
		ImportSource:     string(format),
		AccountRef:       ref,
		Type:             &defaultType,
		Currency:         "USD",
		NeedsSetup:       needsSetup,
		CommissionSource: "rate",
	})
	if err != nil {
		return models.Account{}, false, fmt.Errorf("create: %w", err)
	}
	return created, true, nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *ImportService) finalizeSuccess(ctx context.Context, srcPath string, out Outcome) (Outcome, error) {
	dest, err := s.moveToProcessed(srcPath)
	if err != nil {
		s.logger().Warn("import succeeded but move-to-processed failed",
			"source", srcPath, "err", err)
	}
	out.DestinationPath = dest

	id, err := s.writeSyncLog(ctx, out, "success", out.errorOrSummary())
	if err != nil {
		s.logger().Error("write sync_log failed", "err", err)
	}
	out.SyncLogID = id

	s.logger().Info("import succeeded",
		"source", srcPath,
		"format", string(out.Format),
		"fills_parsed", out.FillsParsed,
		"execs_inserted", out.ExecsInserted,
		"accounts_created", out.AccountsCreated,
		"instruments_stubbed", out.InstrumentsStubbed,
		"destination", dest,
	)
	return out, nil
}

func (s *ImportService) finalizeFailure(ctx context.Context, srcPath string, out Outcome) (Outcome, error) {
	dest, err := s.moveToFailed(srcPath, out.ErrorMessage)
	if err != nil {
		s.logger().Warn("import failed and move-to-failed also failed",
			"source", srcPath, "err", err)
	}
	out.DestinationPath = dest

	id, err := s.writeSyncLog(ctx, out, "error", out.ErrorMessage)
	if err != nil {
		s.logger().Error("write sync_log failed", "err", err)
	}
	out.SyncLogID = id

	s.logger().Warn("import failed",
		"source", srcPath,
		"format", string(out.Format),
		"err", out.ErrorMessage,
		"destination", dest,
	)
	return out, nil
}

// moveToProcessed moves srcPath into ProcessedDir with a timestamp prefix to
// avoid name collisions.
func (s *ImportService) moveToProcessed(srcPath string) (string, error) {
	if s.ProcessedDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(s.ProcessedDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(s.ProcessedDir, timestampPrefixed(filepath.Base(srcPath)))
	if err := moveFile(srcPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// moveToFailed moves srcPath into FailedDir with a timestamp prefix and writes
// a sibling .error.txt file containing the import error message.
func (s *ImportService) moveToFailed(srcPath, errMsg string) (string, error) {
	if s.FailedDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(s.FailedDir, 0o755); err != nil {
		return "", err
	}
	base := timestampPrefixed(filepath.Base(srcPath))
	dest := filepath.Join(s.FailedDir, base)
	if err := moveFile(srcPath, dest); err != nil {
		return "", err
	}
	sidecar := dest + ".error.txt"
	if err := os.WriteFile(sidecar, []byte(errMsg+"\n"), 0o644); err != nil {
		return dest, fmt.Errorf("write sidecar: %w", err)
	}
	return dest, nil
}

// moveFile renames src → dst, falling back to copy + remove when src and dst
// are on different filesystems. macOS in particular places os.MkdirTemp("", …)
// on /private/var which is a different volume from project directories under
// /Volumes/data, so a naïve os.Rename fails with EXDEV.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	// Cross-device: copy + remove.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func isCrossDevice(err error) bool {
	// syscall.EXDEV on Unix; LinkError wraps it. errors.Is unwraps cleanly.
	return errors.Is(err, syscall.EXDEV)
}

func timestampPrefixed(base string) string {
	return time.Now().UTC().Format("2006-01-02T15-04-05Z") + "_" + base
}

func (s *ImportService) writeSyncLog(ctx context.Context, out Outcome, status, message string) (int64, error) {
	source := out.SourcePath
	row, err := s.Queries.CreateSyncLog(ctx, models.CreateSyncLogParams{
		UserID:      &s.UserID,
		AccountID:   nil,
		SyncType:    string(out.Format),
		Status:      status,
		SourceFile:  &source,
		TradesAdded: int64(out.TradesCreated),
		ExecsAdded:  int64(out.ExecsInserted),
		Message:     strPtrOrNil(message),
	})
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (s *ImportService) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// RebuildTradesForAccount re-runs the position builder for every (account_id,
// symbol) pair the account has executions for. Called from the accounts UI
// when the user changes firm - commissions need to refresh on existing trades.
//
// Inline-commission accounts (TopstepX trades CSV) skip the FIFO rebuild so
// the per-trade Fees+Commissions from the CSV are preserved; daily_stats is
// still refreshed because firm or other tags may have changed.
func (s *ImportService) RebuildTradesForAccount(ctx context.Context, accountID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	acct, err := q.GetAccountByID(ctx, models.GetAccountByIDParams{
		ID:     accountID,
		UserID: s.UserID,
	})
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	if acct.CommissionSource == "inline" {
		if err := RefreshDailyStatsForAccount(ctx, q, accountID, s.Timezone); err != nil {
			return fmt.Errorf("refresh daily_stats: %w", err)
		}
		return tx.Commit()
	}

	symbols, err := q.ListDistinctSymbolsForAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list symbols: %w", err)
	}
	if len(symbols) == 0 {
		return nil
	}

	keys := make([]positions.Key, 0, len(symbols))
	for _, sym := range symbols {
		keys = append(keys, positions.Key{AccountID: accountID, Symbol: sym})
	}
	if err := s.runRebuild(ctx, q, keys); err != nil {
		return err
	}
	return tx.Commit()
}

// RebuildTradesForSymbol re-runs the builder for every (account_id, symbol)
// pair that involves this symbol_root. Called from the /commissions UI when
// the user (re)categorizes an instrument's asset_class/size_class.
func (s *ImportService) RebuildTradesForSymbol(ctx context.Context, symbolRoot string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	rows, err := q.ListAccountSymbolKeysForRoot(ctx, models.ListAccountSymbolKeysForRootParams{
		SymbolRoot: symbolRoot,
		UserID:     s.UserID,
	})
	if err != nil {
		return fmt.Errorf("list keys for root %s: %w", symbolRoot, err)
	}
	if len(rows) == 0 {
		return nil
	}
	keys := make([]positions.Key, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, positions.Key{AccountID: r.AccountID, Symbol: r.Symbol})
	}
	if err := s.runRebuild(ctx, q, keys); err != nil {
		return err
	}
	return tx.Commit()
}

// RebuildTradesForCommissionCell re-runs the builder for every (account,
// symbol) pair where the account.firm matches and the instrument falls in the
// given (asset_class, size_class). Called from /commissions when a rate is
// added, edited, or deleted so the affected trades pick up the new rate.
func (s *ImportService) RebuildTradesForCommissionCell(ctx context.Context, firm, assetClass, sizeClass string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	rows, err := q.ListAccountSymbolKeysForCell(ctx, models.ListAccountSymbolKeysForCellParams{
		Firm:       &firm,
		AssetClass: &assetClass,
		SizeClass:  &sizeClass,
		UserID:     s.UserID,
	})
	if err != nil {
		return fmt.Errorf("list keys for cell (%s,%s,%s): %w", firm, assetClass, sizeClass, err)
	}
	if len(rows) == 0 {
		return nil
	}
	keys := make([]positions.Key, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, positions.Key{AccountID: r.AccountID, Symbol: r.Symbol})
	}
	if err := s.runRebuild(ctx, q, keys); err != nil {
		return err
	}
	return tx.Commit()
}

// runRebuild is the shared "build a positions.Builder with the service
// timezone, replay these keys" body used by all three Rebuild* helpers.
func (s *ImportService) runRebuild(ctx context.Context, q *models.Queries, keys []positions.Key) error {
	tz := s.Timezone
	if tz == nil {
		tz = time.UTC
	}
	builder := &positions.Builder{Queries: q, Timezone: tz, UserID: s.UserID}
	if _, err := builder.Rebuild(ctx, keys); err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	return nil
}

func (o Outcome) errorOrSummary() string {
	if o.ErrorMessage != "" {
		return o.ErrorMessage
	}
	return fmt.Sprintf("parsed=%d inserted=%d new_accounts=%d new_instruments=%d",
		o.FillsParsed, o.ExecsInserted, o.AccountsCreated, o.InstrumentsStubbed)
}
