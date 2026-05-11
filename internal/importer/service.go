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

	"github.com/jaro/trading-journal/internal/models"
	"github.com/jaro/trading-journal/internal/positions"
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
// Errors during parse/DB operations are captured: the file is moved to
// FailedDir with a sidecar .error.txt, and a sync_log row records the failure.
// The returned error is non-nil only for failures that prevented any of that
// recovery from happening (e.g. the source file vanished mid-processing).
func (s *ImportService) ImportFile(ctx context.Context, path string) (Outcome, error) {
	out := Outcome{SourcePath: path}

	content, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("read %s: %w", path, err)
	}

	out.Format = DetectFormat(content)
	if out.Format == FormatUnknown {
		out.ErrorMessage = "unrecognized CSV header (neither MotiveWave nor TopstepX)"
		return s.finalizeFailure(ctx, path, out)
	}

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

// DetectFormat inspects the first line to identify the CSV source. A leading
// UTF-8 BOM is tolerated (TopstepX exports include one).
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
	switch strings.TrimSpace(cols[0]) {
	case "Account":
		return FormatMotiveWave
	case "Id":
		return FormatTopstepX
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
			id, created, err := s.findOrCreateAccount(ctx, q, format, f.AccountRef, f.Source)
			if err != nil {
				return fmt.Errorf("account %s: %w", f.AccountRef, err)
			}
			if created {
				out.AccountsCreated++
			}
			accounts[f.AccountRef] = id
			accountID = id
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
		builder := &positions.Builder{Queries: q, Timezone: tz}
		keys := make([]positions.Key, 0, len(rebuildKeys))
		for k := range rebuildKeys {
			keys = append(keys, k)
		}
		res, err := builder.Rebuild(ctx, keys)
		if err != nil {
			return fmt.Errorf("position builder: %w", err)
		}
		out.TradesCreated = res.TradesCreated
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
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

func (s *ImportService) findOrCreateAccount(ctx context.Context, q *models.Queries, format Format, ref, source string) (int64, bool, error) {
	existing, err := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		ImportSource: string(format),
		AccountRef:   ref,
	})
	if err == nil {
		return existing.ID, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("lookup: %w", err)
	}

	firm := GuessFirm(format, ref, source)
	// Default new accounts to Prop · Eval — most common starting state for a
	// fresh prop-firm account. User can promote to funded/live or change
	// entirely via /accounts. needs_setup clears when firm + type are both
	// set (firm guess can still fail for unrecognized brokers).
	defaultType := "prop_eval"
	needsSetup := int64(0)
	if firm == "" {
		needsSetup = 1
	}
	created, err := q.CreateAccount(ctx, models.CreateAccountParams{
		Name:         ref,
		Firm:         strPtrOrNil(firm),
		ImportSource: string(format),
		AccountRef:   ref,
		Type:         &defaultType,
		Currency:     "USD",
		NeedsSetup:   needsSetup,
	})
	if err != nil {
		return 0, false, fmt.Errorf("create: %w", err)
	}
	return created.ID, true, nil
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
// when the user changes firm — commissions need to refresh on existing trades.
func (s *ImportService) RebuildTradesForAccount(ctx context.Context, accountID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := s.Queries.WithTx(tx)

	symbols, err := q.ListDistinctSymbolsForAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list symbols: %w", err)
	}
	if len(symbols) == 0 {
		return nil
	}

	tz := s.Timezone
	if tz == nil {
		tz = time.UTC
	}
	builder := &positions.Builder{Queries: q, Timezone: tz}
	keys := make([]positions.Key, 0, len(symbols))
	for _, sym := range symbols {
		keys = append(keys, positions.Key{AccountID: accountID, Symbol: sym})
	}
	if _, err := builder.Rebuild(ctx, keys); err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	return tx.Commit()
}

func (o Outcome) errorOrSummary() string {
	if o.ErrorMessage != "" {
		return o.ErrorMessage
	}
	return fmt.Sprintf("parsed=%d inserted=%d new_accounts=%d new_instruments=%d",
		o.FillsParsed, o.ExecsInserted, o.AccountsCreated, o.InstrumentsStubbed)
}
